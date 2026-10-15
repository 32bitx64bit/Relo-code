package ctxeng

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMetricsAndInspectReturnCopies(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 8000)
	e.Ingest("a", []Item{{Class: ClassHistory, Content: "grep x"}})
	m := e.Metrics("a")
	m.Repeats["poisoned"] = 1
	ins := e.Inspect("a")
	ins.Metrics.Repeats["poisoned"] = 1
	if _, ok := e.Metrics("a").Repeats["poisoned"]; ok {
		t.Fatal("Metrics leaked internal Repeats map")
	}
	if _, ok := e.Inspect("a").Metrics.Repeats["poisoned"]; ok {
		t.Fatal("Inspect leaked internal Repeats map")
	}
}

func TestMetricsAndInspectSafeDuringIngest(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 8000)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 300; i++ {
			e.Ingest("a", []Item{{Class: ClassHistory, Content: fmt.Sprintf("grep %d", i)}})
		}
	}()
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m := e.Metrics("a")
				for k := range m.Repeats {
					_ = k
				}
				m.Repeats["local"] = 1
				ins := e.Inspect("a")
				for k := range ins.Metrics.Repeats {
					_ = k
				}
				ins.Metrics.Repeats["local"] = 1
			}
		}()
	}
	wg.Wait()
}

func TestForkDeepCopiesMetrics(t *testing.T) {
	e := New()
	e.Bind("p", "t", "worker", 8000)
	e.Ingest("p", []Item{{Class: ClassHistory, Content: "same"}})
	dst, _, err := e.Fork("p")
	if err != nil {
		t.Fatal(err)
	}
	before := e.Metrics(dst).Repeats["x"]
	e.Ingest("p", []Item{{Class: ClassHistory, Content: "x"}})
	if e.Metrics(dst).Repeats == nil || e.Metrics(dst).Repeats["x"] != before {
		t.Fatal("fork metrics share Repeats with parent")
	}
}

func TestRestoreGenerationDedupesItemsAndAvoidsNumberCollision(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 8000)
	e.Ingest("a", []Item{{ID: "x", Class: ClassTask, Source: SourceUser, Content: "goal"}})
	if _, err := e.Build(BuildRequest{AgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	e.RestoreGeneration(Generation{AgentID: "a", Number: 3, Items: []Item{
		{ID: "x", Class: ClassTask, Source: SourceUser, Content: "goal"},
		{ID: "y", Class: ClassFinding, Source: SourceWorker, Content: "restored"},
	}})
	count := map[string]int{}
	for _, it := range e.SnapshotPool("a") {
		count[it.ID]++
	}
	if count["x"] != 1 || count["y"] != 1 {
		t.Fatalf("pool not deduped: %v", count)
	}
	next, err := e.Build(BuildRequest{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Number != 4 {
		t.Fatalf("next generation number %d, want 4 (max restored 3 + 1)", next.Number)
	}
	if e.CurrentNumber("a") != 4 {
		t.Fatalf("CurrentNumber %d, want 4", e.CurrentNumber("a"))
	}
}

func TestNonBoundaryBuildAccountsOverflowAndAllowsRefresh(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 100)
	e.Ingest("a", []Item{{ID: "a1", Class: ClassCode, Source: SourceRepo, Content: strings.Repeat("a", 240)}})
	if _, err := e.Build(BuildRequest{AgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	// Non-boundary build appends new selected items after prior ones (cache-stable), so
	// prior + new can exceed budget. Overflow must be recorded and a refresh must be possible.
	e.Ingest("a", []Item{{ID: "b1", Class: ClassCode, Source: SourceRepo, Verified: true, Content: strings.Repeat("b", 240)}})
	g2, err := e.Build(BuildRequest{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Items) < 2 {
		t.Fatalf("append-only within generation violated: %d items", len(g2.Items))
	}
	if g2.OverflowTokens <= 0 || g2.Tokens <= g2.Budget {
		t.Fatalf("overflow unaccounted: tokens=%d budget=%d overflow=%d", g2.Tokens, g2.Budget, g2.OverflowTokens)
	}
	if _, _, ok := e.MaybeRefresh("a", 0.85, true); !ok {
		t.Fatal("over-budget generation did not trigger refresh")
	}
}

func TestPinnedFailingTestRendersVolatile(t *testing.T) {
	e := New()
	e.Bind("a", "t", "worker", 8000)
	e.Ingest("a", []Item{{ID: "f", Class: ClassFinding, Source: SourceTest, Title: "Test failure", Kind: KindTest, Pinned: true, Content: "TestRefreshRace failed"}})
	g, err := e.Build(BuildRequest{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(RenderStable(g), "TestRefreshRace failed") {
		t.Fatal("pinned failing test landed in cached static block")
	}
	if !strings.Contains(RenderVolatile(g), "TestRefreshRace failed") {
		t.Fatal("pinned failing test missing from volatile section")
	}
}

func TestRetrieveEmptyQueryKeepsRelevanceAndEmptyEntriesMatchNothing(t *testing.T) {
	items := []Item{{ID: "i", Content: "unrelated", Relevance: 0.7}}
	if got := Retrieve(items, Query{}); got[0].Relevance != 0.7 {
		t.Fatalf("empty query overwrote relevance: %v", got[0].Relevance)
	}
	withEmpty := []Item{{ID: "j", Content: "unrelated", Files: []string{"a.go"}}}
	if got := Retrieve(withEmpty, Query{Files: []string{""}}); got[0].Relevance != 0 {
		t.Fatalf("empty file entry matched everything: %v", got[0].Relevance)
	}
}

func TestCompressSectionNonASCIIOffsets(t *testing.T) {
	// U+0130 (İ) lowercases to a longer byte sequence; offsets must still index the original.
	it := Item{Class: ClassFinding, Title: "Bug", Kind: KindBug, Content: "İİİİ notes\nroot cause\nrace on refresh\nimplementation\nserialize by session"}
	out := compressText(it)
	if !strings.Contains(out, "Root cause:\nrace on refresh") {
		t.Fatalf("root cause misindexed:\n%s", out)
	}
	if !strings.Contains(out, "Implementation:\nserialize by session") {
		t.Fatalf("implementation misindexed:\n%s", out)
	}
}

func TestStableItemKeyDistinguishesKind(t *testing.T) {
	a := Item{Class: ClassFinding, Source: SourceWorker, Title: "T", Content: "same", Kind: KindBug}
	b := a
	b.Kind = KindTest
	if stableItemKey(a) == stableItemKey(b) {
		t.Fatal("items differing only by Kind share a key")
	}
}
