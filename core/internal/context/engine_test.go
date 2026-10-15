package ctxeng

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestLargeHistoryCompressesDurableState(t *testing.T) {
	e := New()
	e.now = func() time.Time { return time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC) }
	e.Bind("a1", "t1", "worker", 2500)
	e.Ingest("a1", []Item{
		{ID: "obj", Class: ClassTask, Source: SourceUser, Title: "Objective", Content: "Add OAuth login for Google and GitHub.", Durability: 1, Relevance: 1},
		{ID: "con", Class: ClassTask, Source: SourceUser, Kind: KindConstraint, Title: "Constraint", Content: "Do not store secrets in SQLite.", Pinned: true, Durability: 1},
		{ID: "find", Class: ClassFinding, Source: SourceVerified, Kind: KindBug, Title: "Refresh race", Content: "Root cause:\ntwo concurrent refresh requests could replace the same token.\nImplementation:\nserialize by session ID.", Verified: true, Files: []string{"internal/auth/session.go"}, Durability: 0.95},
		{ID: "err", Class: ClassTask, Source: SourceTest, Kind: KindTest, Title: "Latest error", Content: "go test ./internal/auth failed: race detector on Token.Refresh", Durability: 0.9},
	})
	var hist []Item
	for i := 0; i < 40; i++ {
		hist = append(hist, Item{
			Class:   ClassHistory,
			Source:  SourceSummary,
			Content: strings.Repeat("old exploratory grep result and abandoned hypothesis about cookies "+strings.Repeat("x", 80), 3),
			Phase:   PhaseExplore,
		})
	}
	e.Ingest("a1", hist)
	e.SetPhase("a1", PhaseImplement)
	gen, err := e.Build(BuildRequest{AgentID: "a1", Reason: "compress large history"})
	if err != nil {
		t.Fatal(err)
	}
	if gen.Tokens >= 20000 {
		t.Fatalf("expected compressed context, got %d tokens", gen.Tokens)
	}
	text := Render(gen)
	for _, need := range []string{"OAuth", "secrets", "refresh", "race"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(need)) {
			t.Fatalf("missing durable %q in:\n%s", need, text)
		}
	}
	droppedHist := 0
	for _, d := range gen.Dropped {
		if d.Item.Class == ClassHistory {
			droppedHist++
		}
	}
	if droppedHist < 20 {
		t.Fatalf("expected history to be dropped, dropped %d", droppedHist)
	}
}

func TestObsoletePhaseRemoved(t *testing.T) {
	e := New()
	e.Bind("a1", "t1", "worker", 8000)
	e.Ingest("a1", []Item{
		{ID: "obj", Class: ClassTask, Source: SourceUser, Content: "Implement auth.", Durability: 1},
		{ID: "scan", Class: ClassEphemeral, Source: SourceRepo, Content: "grep output from exploring the whole monorepo", Phase: PhaseExplore},
		{ID: "find", Class: ClassFinding, Source: SourceWorker, Content: "Auth lives in internal/auth", Verified: true, Files: []string{"internal/auth"}, Durability: 0.9},
	})
	e.SetPhase("a1", PhaseImplement)
	gen, err := e.Refresh("a1", "phase change")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range gen.Items {
		if strings.Contains(it.Content, "grep output") {
			t.Fatal("obsolete exploration survived phase change")
		}
	}
	found := false
	for _, it := range gen.Items {
		if strings.Contains(it.Content, "internal/auth") || strings.Contains(it.Content, "Implement auth") {
			found = true
		}
	}
	if !found {
		t.Fatal("task state/findings lost across phase change")
	}
}

func TestGenerationContinuesSameAgent(t *testing.T) {
	e := New()
	e.Bind("agent17", "task9", "worker", 4000)
	e.Ingest("agent17", []Item{{Class: ClassTask, Source: SourceUser, Content: "Fix race", Durability: 1}})
	g1, _ := e.Build(BuildRequest{AgentID: "agent17"})
	g2, _ := e.Refresh("agent17", "stalled progress")
	if g1.Number != 1 || g2.Number != 2 {
		t.Fatalf("generations %d -> %d", g1.Number, g2.Number)
	}
	if g2.AgentID != "agent17" || g2.TaskID != "task9" {
		t.Fatalf("identity changed: %+v", g2)
	}
}

func TestPinnedSurvivesCompression(t *testing.T) {
	e := New()
	e.Bind("a1", "t1", "worker", 900)
	pin := Item{ID: "pin", Class: ClassTask, Source: SourceUser, Kind: KindConstraint, Content: strings.Repeat("Never rewrite migrations. ", 20), Pinned: true, Durability: 1}
	e.Ingest("a1", []Item{pin})
	for i := 0; i < 30; i++ {
		e.Ingest("a1", []Item{{Class: ClassCode, Source: SourceRepo, Content: strings.Repeat("func unused() {}", 50), Files: []string{strings.Repeat("pkg/file.go", 4)}}})
	}
	for i := 0; i < 5; i++ {
		if _, err := e.Refresh("a1", "compress"); err != nil {
			t.Fatal(err)
		}
	}
	ins := e.Inspect("a1")
	ok := false
	for _, it := range ins.Included {
		if it.Pinned || strings.Contains(it.Content, "Never rewrite migrations") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("pinned constraint lost: %+v", ins.Included)
	}
}

func TestUnverifiedHypothesisNotVerifiedMemory(t *testing.T) {
	e := New()
	f := e.PromoteFinding(Finding{
		AgentID:    "a1",
		TaskID:     "t1",
		Kind:       KindBug,
		Summary:    "Maybe the cache is stale",
		Confidence: 0.3,
		Verified:   false,
	})
	if f.Verified {
		t.Fatal("unverified finding marked verified")
	}
	got := e.SnapshotPool("a1")
	if len(got) == 0 {
		t.Fatal("expected finding in pool")
	}
	if got[0].Verified {
		t.Fatal("hypothesis silently became verified")
	}
	if got[0].Source != SourceHypothesis {
		t.Fatalf("source=%s", got[0].Source)
	}
	if _, err := e.VerifyFinding(f.ID); err != nil {
		t.Fatal(err)
	}
	v := e.Findings()[0]
	if !v.Verified {
		t.Fatal("explicit verify failed")
	}
}

func TestParentReceivesSummaryNotTranscript(t *testing.T) {
	e := New()
	e.Bind("worker", "t1", "worker", 16000)
	e.Ingest("worker", []Item{
		{Class: ClassHistory, Source: SourceSummary, Content: strings.Repeat("tool output line\n", 400)},
		{Class: ClassTask, Source: SourceUser, Content: "Fix session refresh.", Durability: 1},
		{Class: ClassFinding, Source: SourceVerified, Verified: true, Content: CompressReport("Authentication refresh race fixed in internal/auth/session.go.", []string{"internal/auth/session.go"}, []string{"go test ./internal/auth/... passed", "go test -race ./internal/auth/... passed"}, []string{"provider timeout untested"}, "two concurrent refresh requests could replace the same token.")},
	})
	e.Bind("primary", "t1", "primary", 8000)
	if _, err := e.Merge("primary", "worker"); err != nil {
		t.Fatal(err)
	}
	text := e.Render("primary")
	if strings.Count(text, "tool output line") > 5 {
		t.Fatal("raw transcript leaked to parent")
	}
	for _, need := range []string{"session.go", "Root cause", "Verification", "Open risk"} {
		if !strings.Contains(text, need) {
			t.Fatalf("parent summary missing %q:\n%s", need, text)
		}
	}
}

func TestBudgetDropsLowPriority(t *testing.T) {
	e := New()
	e.Bind("a1", "t1", "worker", 400)
	e.Ingest("a1", []Item{
		{Class: ClassTask, Source: SourceUser, Content: "Keep this objective.", Durability: 1, Relevance: 1},
		{Class: ClassMemory, Source: SourceRepo, Content: strings.Repeat("optional architecture footnote ", 80), Durability: 0.2},
	})
	gen, _ := e.Build(BuildRequest{AgentID: "a1"})
	if !strings.Contains(Render(gen), "Keep this objective") {
		t.Fatal("objective dropped")
	}
}

func TestForkKeepsIdentitySeparation(t *testing.T) {
	e := New()
	e.Bind("src", "t1", "worker", 2000)
	e.Ingest("src", []Item{{Class: ClassTask, Source: SourceUser, Content: "origin"}})
	_, _ = e.Build(BuildRequest{AgentID: "src"})
	dst, gen, err := e.Fork("src")
	if err != nil {
		t.Fatal(err)
	}
	if dst == "src" {
		t.Fatal("fork reused id")
	}
	if gen.AgentID != dst {
		t.Fatalf("fork gen agent %s", gen.AgentID)
	}
}

func TestRenderDeterministicAcrossInsertionOrder(t *testing.T) {
	items := []Item{
		{ID: "task", Class: ClassTask, Source: SourceUser, Title: "Objective", Content: "build"},
		{ID: "f2", Class: ClassFinding, Source: SourceWorker, Title: "B", Content: "second"},
		{ID: "f1", Class: ClassFinding, Source: SourceVerified, Title: "A", Content: "first", Verified: true},
		{ID: "c", Class: ClassCode, Source: SourceRepo, Title: "code", Content: "func f() {}"},
	}
	build := func(input []Item) (*Generation, error) {
		e := New()
		e.Bind("a", "t", "worker", 8000)
		e.now = func() time.Time { return time.Unix(1, 0).UTC() }
		e.Ingest("a", input)
		return e.Build(BuildRequest{AgentID: "a"})
	}
	g1, err := build(items)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := append([]Item{}, items...)
	rand.New(rand.NewSource(3)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	g2, err := build(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if RenderStable(g1) != RenderStable(g2) || RenderDynamic(g1) != RenderDynamic(g2) {
		t.Fatal("render differs for shuffled input")
	}
}

func TestBuildAppendsWithinGenerationAndRefreshKeepsStableBlock(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 8000)
	e.Ingest("a", []Item{{ID: "task", Class: ClassTask, Source: SourceUser, Content: "goal"}, {ID: "old", Class: ClassFinding, Source: SourceWorker, Content: "existing"}})
	g1, _ := e.Build(BuildRequest{AgentID: "a"})
	before := RenderDynamic(g1)
	e.Ingest("a", []Item{{ID: "new", Class: ClassFinding, Source: SourceWorker, Content: "added later"}})
	g2, _ := e.Build(BuildRequest{AgentID: "a"})
	if !strings.HasPrefix(RenderDynamic(g2), before) {
		t.Fatal("new item changed prior dynamic prefix")
	}
	refreshed, _ := e.Refresh("a", "test boundary")
	if RenderStable(g1) != RenderStable(refreshed) {
		t.Fatal("refresh changed stable context")
	}
}

func TestMandatoryOverflowReported(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 8)
	e.Ingest("a", []Item{{ID: "pin", Class: ClassTask, Source: SourceUser, Pinned: true, Content: strings.Repeat("mandatory ", 100)}})
	g, _ := e.Build(BuildRequest{AgentID: "a"})
	if g.OverflowTokens <= 0 {
		t.Fatalf("mandatory overflow unreported: %+v", g)
	}
}

func TestFullHistoryRetainsHistoryAndVerifyMatchesFindingIdentity(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 4000)
	e.SetStrategy("a", "full_history")
	e.Ingest("a", []Item{{ID: "hist", Class: ClassHistory, Content: "chronological event"}})
	g, _ := e.Build(BuildRequest{AgentID: "a"})
	found := false
	for _, it := range g.Items {
		if it.ID == "hist" {
			found = true
		}
	}
	if !found {
		t.Fatal("full_history dropped history")
	}
	f1 := e.PromoteFinding(Finding{ID: "one", AgentID: "a", Kind: KindBug, Summary: "same", Confidence: 0.8})
	f2 := e.PromoteFinding(Finding{ID: "two", AgentID: "a", Kind: KindBug, Summary: "same", Confidence: 0.8})
	_, _ = e.VerifyFinding(f1.ID)
	pool := e.SnapshotPool("a")
	verified1, verified2 := false, false
	for _, it := range pool {
		if it.FindingID == f1.ID {
			verified1 = it.Verified
		}
		if it.FindingID == f2.ID {
			verified2 = it.Verified
		}
	}
	if !verified1 || verified2 {
		t.Fatalf("verification crossed finding identities: %v %v (%+v)", verified1, verified2, f1)
	}
}

func TestRetrieveDoesNotCarryOldRelevance(t *testing.T) {
	items := []Item{{ID: "i", Content: "unrelated", Relevance: 0.9}}
	got := Retrieve(items, Query{Keywords: []string{"missing"}})
	if got[0].Relevance != 0 {
		t.Fatalf("stale relevance=%v", got[0].Relevance)
	}
}

func TestScopeFiltersClipsAndKeepsMandatoryItems(t *testing.T) {
	e := New()
	e.Bind("scoped", "task", "worker", 8000)
	long := strings.Repeat("package auth\nfunc Refresh() { /* implementation */ }\n", 80)
	e.Ingest("scoped", []Item{
		{ID: "keep", Class: ClassCode, Source: SourceRepo, Content: long, Files: []string{"internal/auth/session.go"}, Symbols: []string{"Refresh"}},
		{ID: "memory", Class: ClassMemory, Content: "project preference"},
		{ID: "other", Class: ClassHistory, Content: "old transcript"},
		{ID: "mandatory", Class: ClassTask, Source: SourceUser, Pinned: true, Content: strings.Repeat("mandatory constraint ", 30)},
		{ID: "dep", Class: ClassReport, Source: SourceParent, TaskID: "dep-1", Content: "dependency report"},
		{ID: "not-dep", Class: ClassReport, Source: SourceParent, TaskID: "dep-2", Content: "unrelated report"},
		{ID: "failure", Class: ClassTask, Source: SourceTest, Title: "Latest failure", Content: "volatile failure tail"},
	})
	scope := &Scope{MaxItemTokens: 40, Classes: []Class{ClassCode, ClassTask, ClassMemory, ClassReport}, DependencyReportsOnly: true}
	g, err := e.Build(BuildRequest{AgentID: "scoped", Scope: scope, DependencyTaskIDs: []string{"dep-1"}})
	if err != nil {
		t.Fatal(err)
	}
	included := map[string]Item{}
	for _, it := range g.Items {
		included[it.ID] = it
	}
	if _, ok := included["keep"]; !ok || !strings.Contains(included["keep"].Content, "[clipped ") {
		t.Fatalf("code item not clipped: %+v", included["keep"])
	}
	if included["keep"].Tokens > scope.MaxItemTokens {
		t.Fatalf("clipped item exceeds token cap: %d", included["keep"].Tokens)
	}
	if !strings.Contains(included["keep"].Content, "internal/auth/session.go") || !strings.Contains(included["keep"].Content, "Refresh") {
		t.Fatal("clip lost file path or symbol")
	}
	if _, ok := included["mandatory"]; !ok || !strings.Contains(included["mandatory"].Content, "mandatory constraint") {
		t.Fatal("mandatory item was clipped or dropped")
	}
	if _, ok := included["dep"]; !ok {
		t.Fatal("dependency report dropped")
	}
	if _, ok := included["failure"]; !ok {
		t.Fatal("volatile failure dropped despite allowed class")
	}
	if len(g.Dropped) != 3 {
		t.Fatalf("dropped items=%+v", g.Dropped)
	}
	for _, d := range g.Dropped {
		if d.Reason == "" {
			t.Fatalf("dropped item missing reason: %+v", d)
		}
		if d.Item.ID == "memory" && d.Reason != "scope_memory" {
			t.Fatalf("memory exclusion reason = %q", d.Reason)
		}
	}
	if !strings.Contains(RenderVolatile(g), "volatile failure tail") || Tokens(Render(g)) != g.Tokens {
		t.Fatalf("volatile tail/clip marker missing from token accounting: tokens=%d rendered=%d", g.Tokens, Tokens(Render(g)))
	}
	if g.OverflowTokens != maxInt(0, g.Tokens-g.Budget) {
		t.Fatalf("overflow=%d, tokens=%d budget=%d", g.OverflowTokens, g.Tokens, g.Budget)
	}
}

func TestScopeAppliesAtGenerationBoundaries(t *testing.T) {
	e := New()
	e.Bind("boundary", "task", "worker", 8000)
	e.Ingest("boundary", []Item{{ID: "old", Class: ClassFinding, Content: "existing finding"}})
	g1, _ := e.Build(BuildRequest{AgentID: "boundary", Scope: &Scope{Classes: []Class{ClassFinding}}})
	before := RenderDynamic(g1)
	e.Ingest("boundary", []Item{{ID: "new", Class: ClassCode, Content: "new code"}})
	g2, _ := e.Build(BuildRequest{AgentID: "boundary", Scope: &Scope{Classes: []Class{ClassCode}}})
	if !strings.HasPrefix(RenderDynamic(g2), before) {
		t.Fatal("scope changed existing prefix within generation")
	}
	g3, _ := e.Refresh("boundary", "new scope boundary")
	for _, it := range g3.Items {
		if it.Class != ClassFinding {
			t.Fatalf("scope was not applied at refresh boundary: %+v", it)
		}
	}
}

func TestEmptyScopeClassesMeansAllClasses(t *testing.T) {
	e := New()
	e.Bind("empty-scope", "task", "worker", 4000)
	e.Ingest("empty-scope", []Item{{ID: "code", Class: ClassCode, Content: "source"}})
	g, err := e.Build(BuildRequest{AgentID: "empty-scope", Scope: &Scope{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range g.Items {
		if it.ID == "code" {
			return
		}
	}
	t.Fatalf("nil Classes unexpectedly filtered all items: %+v", g)
}

// Callers seed the same facts every turn. Each copy is sent in every later call, so the pool keeps one.
func TestIngestSkipsAnItemThePoolAlreadyHolds(t *testing.T) {
	e := New()
	e.Bind("a", "t", "primary", 10000)
	note := Item{Class: ClassMemory, Title: "m", Content: "same"}
	e.Ingest("a", []Item{note})
	e.Ingest("a", []Item{note, {Class: ClassMemory, Title: "m", Content: "other"}})
	pinned := note
	pinned.Pinned = true
	e.Ingest("a", []Item{pinned})
	pool := e.SnapshotPool("a")
	if len(pool) != 2 {
		t.Fatalf("pool has %d items, want 2: %+v", len(pool), pool)
	}
	for _, it := range pool {
		if it.Content == "same" && !it.Pinned {
			t.Fatal("the later copy was pinned; the item that is kept must be too")
		}
	}
}

// Repeated history is how the loop detector sees an agent going in circles, so it is not merged.
func TestIngestStillCountsRepeatedHistory(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 10000)
	step := Item{Class: ClassHistory, Content: "ran the same command"}
	e.Ingest("a", []Item{step, step, step})
	if got := len(e.SnapshotPool("a")); got != 3 {
		t.Fatalf("pool has %d history items, want 3", got)
	}
	if e.Metrics("a").RepeatedActions == 0 {
		t.Fatal("repeats were not counted")
	}
}

// What was left out is a count. Quoting each item's first line sent a probe 3.7k tokens about memory it was not meant to see.
func TestExcludedItemsAreCountedNotQuoted(t *testing.T) {
	var dropped []Dropped
	for i := 0; i < 3; i++ {
		dropped = append(dropped, Dropped{Item: Item{Class: ClassMemory, Title: "T", Content: "FIRST-LINE-OF-A-MEMORY\nmore"}, Reason: "scope_memory"})
	}
	dropped = append(dropped, Dropped{Item: Item{Class: ClassFinding, Content: "f"}, Reason: "scope_class"})
	out := RenderVolatile(&Generation{Dropped: dropped})
	if strings.Contains(out, "FIRST-LINE-OF-A-MEMORY") {
		t.Fatalf("the excluded items are quoted:\n%s", out)
	}
	for _, want := range []string{"3 memory (scope_memory)", "1 finding (scope_class)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if got := RenderVolatile(&Generation{}); got != "" {
		t.Fatalf("nothing was left out but the render says %q", got)
	}
}
