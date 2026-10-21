package ctxeng

import (
	"strings"
	"testing"
)

func TestBlobHashIsGitsObjectID(t *testing.T) {
	// `printf 'hello\n' | git hash-object --stdin`, and the empty blob every Git repository knows.
	if got := BlobHash([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("hello = %s", got)
	}
	if got := BlobHash(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty = %s", got)
	}
}

func ref(agent string, rev uint64, tree string) WorkspaceContextRef {
	return WorkspaceContextRef{AgentID: agent, BaseCommit: "base1", OverlayRevision: rev, EffectiveTreeOID: tree}
}

// A generation names the repository state it was built against; a refresh names the state the agent has by then, and
// the earlier generation keeps what it said.
func TestGenerationsCarryTheWorkspaceTheyWereBuiltAgainst(t *testing.T) {
	e := New()
	e.Bind("a1", "t1", "worker", 8000)
	e.Ingest("a1", []Item{{ID: "obj", Class: ClassTask, Source: SourceUser, Title: "Objective", Content: "Do it.", Durability: 1}})
	if _, ok := e.WorkspaceRef("a1"); ok {
		t.Fatal("an agent that was never bound has a workspace")
	}
	g0, _ := e.Build(BuildRequest{AgentID: "a1", Reason: "start"})
	if g0.Workspace != nil {
		t.Fatalf("an unbound agent's generation names a workspace: %+v", g0.Workspace)
	}

	e.BindWorkspace("a1", ref("a1", 3, "tree3"))
	g1, _ := e.Build(BuildRequest{AgentID: "a1", Reason: "start"})
	e.BindWorkspace("a1", ref("a1", 7, "tree7"))
	g2, err := e.Refresh("a1", "refresh")
	if err != nil {
		t.Fatal(err)
	}
	if g1.Workspace == nil || g1.Workspace.OverlayRevision != 3 || g1.Workspace.EffectiveTreeOID != "tree3" || g1.Workspace.BaseCommit != "base1" {
		t.Fatalf("g1 = %+v", g1.Workspace)
	}
	if g2.Workspace == nil || g2.Workspace.OverlayRevision != 7 || g2.Workspace.EffectiveTreeOID != "tree7" {
		t.Fatalf("g2 = %+v", g2.Workspace)
	}
	if again := e.Generation("a1", g1.Number); again.Workspace.OverlayRevision != 3 {
		t.Fatalf("an earlier generation moved to revision %d", again.Workspace.OverlayRevision)
	}
	// The binding belongs to the generation, not to the caller's copy.
	r, _ := e.WorkspaceRef("a1")
	r.OverlayRevision = 99
	if got, _ := e.WorkspaceRef("a1"); got.OverlayRevision != 7 {
		t.Fatal("WorkspaceRef handed out the engine's own state")
	}
	if ins := e.Inspect("a1"); ins.Workspace == nil || ins.Workspace.OverlayRevision != 7 {
		t.Fatalf("inspection = %+v", ins.Workspace)
	}
}

func lookupOf(files map[string]string) FileLookup {
	return func(path string) (string, bool, error) {
		body, ok := files[path]
		if !ok {
			return "", false, nil
		}
		return BlobHash([]byte(body)), true, nil
	}
}

func derived(agent string, rev uint64, files map[string]string) *Derivation {
	d := &Derivation{AgentID: agent, Revision: rev, Hashes: map[string]string{}}
	for p, body := range files {
		d.Hashes[p] = BlobHash([]byte(body))
	}
	return d
}

func TestStaleExcerptsAreDetectedFlaggedAndDroppedAtTheNextBoundary(t *testing.T) {
	e := New()
	e.Bind("a1", "t1", "worker", 20000)
	files := map[string]string{"a.go": "package a\n", "b.go": "package b\n"}
	e.BindWorkspace("a1", ref("a1", 1, "tree1"))
	e.Ingest("a1", []Item{
		{ID: "obj", Class: ClassTask, Source: SourceUser, Title: "Objective", Content: "Do it.", Durability: 1, Relevance: 1},
		{ID: "excerpt", Class: ClassCode, Source: SourceRepo, Title: "a.go", Content: "package a\n", Files: []string{"a.go"}, Durability: 1, Relevance: 1, From: derived("a1", 1, map[string]string{"a.go": files["a.go"]})},
		{ID: "other", Class: ClassCode, Source: SourceRepo, Title: "b.go", Content: "package b\n", Files: []string{"b.go"}, Durability: 1, Relevance: 1, From: derived("a1", 1, map[string]string{"b.go": files["b.go"]})},
		{ID: "finding", Class: ClassFinding, Source: SourceVerified, Kind: KindBug, Title: "a.go races", Content: "a.go races on x.", Verified: true, Files: []string{"a.go"}, Durability: 1, Relevance: 1, From: derived("a1", 1, map[string]string{"a.go": files["a.go"]})},
	})
	g1, _ := e.Build(BuildRequest{AgentID: "a1", Reason: "start"})
	before := Render(g1)
	if !strings.Contains(before, "package a") || !strings.Contains(before, "a.go races") {
		t.Fatalf("setup: %s", before)
	}

	// Nothing changed: everything is fresh and nothing asks for a refresh.
	if res := e.Revalidate("a1", lookupOf(files)); res.Checked != 3 || len(res.Stale) != 0 {
		t.Fatalf("fresh check = %+v", res)
	}
	if ok, why := e.Unhealthy("a1", 0.99); ok {
		t.Fatalf("a current context is unhealthy: %s", why)
	}

	// The agent edits a.go (revision 2).
	files["a.go"] = "package a // edited\n"
	res := e.Revalidate("a1", lookupOf(files))
	if res.Checked != 3 || len(res.Stale) != 2 {
		t.Fatalf("stale check = %+v", res)
	}
	if res.Stale[0].ID != "excerpt" || res.Stale[1].ID != "finding" || res.Stale[0].Paths[0] != "a.go" {
		t.Fatalf("stale = %+v", res.Stale)
	}
	// The generation that was built is untouched until a boundary, so the cached prompt prefix stays valid.
	if got := Render(e.Generation("a1", g1.Number)); got != before {
		t.Fatal("revalidation changed a generation that was already built")
	}
	g1b, err := e.Build(BuildRequest{AgentID: "a1", Reason: "next turn"})
	if err != nil || RenderStable(g1b) != RenderStable(g1) || strings.Contains(Render(g1b), "Stale - ") {
		t.Fatalf("a non-boundary build acted on stale items: %v", err)
	}
	ok, why := e.Unhealthy("a1", 0.99)
	if !ok || why != "stale file excerpt" {
		t.Fatalf("unhealthy = %v %q", ok, why)
	}

	// The refresh drops the stale code, keeps the untouched code, and keeps the finding with a flag.
	e.BindWorkspace("a1", ref("a1", 2, "tree2"))
	g2, err := e.Refresh("a1", why)
	if err != nil {
		t.Fatal(err)
	}
	text := Render(g2)
	if strings.Contains(text, "package a\n") {
		t.Fatalf("stale code survived the refresh:\n%s", text)
	}
	if !strings.Contains(text, "package b") {
		t.Fatalf("current code was lost:\n%s", text)
	}
	if !strings.Contains(text, "Stale - a.go changed after revision 1") || !strings.Contains(text, "a.go races on x.") {
		t.Fatalf("the finding was not kept with a flag:\n%s", text)
	}
	var droppedReason string
	for _, d := range g2.Dropped {
		if d.Item.ID == "excerpt" {
			droppedReason = d.Reason
		}
	}
	if droppedReason != "stale_file" {
		t.Fatalf("dropped = %+v", g2.Dropped)
	}

	// The file goes back to what the finding was cut from: it is current again.
	files["a.go"] = "package a\n"
	if res := e.Revalidate("a1", lookupOf(files)); len(res.Stale) != 0 {
		t.Fatalf("reverted file still stale: %+v", res)
	}
	g3, _ := e.Refresh("a1", "refresh")
	if strings.Contains(Render(g3), "Stale - ") {
		t.Fatalf("a current finding is still flagged:\n%s", Render(g3))
	}
}

func TestADeletedFileMakesItsExcerptStaleAndUnreadableFilesAreLeftAlone(t *testing.T) {
	e := New()
	e.Bind("a1", "t1", "worker", 20000)
	e.Ingest("a1", []Item{
		{ID: "x", Class: ClassCode, Source: SourceRepo, Content: "package x\n", Files: []string{"x.go"}, From: derived("a1", 1, map[string]string{"x.go": "package x\n"})},
		{ID: "gone", Class: ClassCode, Source: SourceRepo, Content: "(did not exist)", Files: []string{"new.go"}, From: &Derivation{AgentID: "a1", Revision: 1, Hashes: map[string]string{"new.go": ""}}},
	})
	files := map[string]string{"new.go": "package n\n"} // x.go was deleted, new.go now exists
	res := e.Revalidate("a1", lookupOf(files))
	if len(res.Stale) != 2 {
		t.Fatalf("deleted and newly created files must both read as changed: %+v", res)
	}
	broken := func(string) (string, bool, error) { return "", false, errUnreadable }
	if res := e.Revalidate("a1", broken); res.Unchecked != 2 || len(res.Stale) != 0 {
		t.Fatalf("a file that cannot be read is a failure of the check, not a change: %+v", res)
	}
}

type unreadable struct{}

func (unreadable) Error() string { return "unreadable" }

var errUnreadable error = unreadable{}

// Code cut from another agent's workspace is never reachable by accident; it comes in only when someone chose it.
func TestAnotherAgentsCodeIsNeverImportedImplicitly(t *testing.T) {
	e := New()
	e.Bind("a2", "t2", "worker", 20000)
	theirs := Item{ID: "theirs", Class: ClassCode, Source: SourceRepo, Title: "a.go (agent 1's edit)", Content: "package a // agent 1\n", Files: []string{"a.go"}, Durability: 1, Relevance: 1, From: derived("a1", 4, map[string]string{"a.go": "package a // agent 1\n"})}
	mine := Item{ID: "mine", Class: ClassCode, Source: SourceRepo, Title: "a.go", Content: "package a\n", Files: []string{"a.go"}, Durability: 1, Relevance: 1, From: derived("a2", 1, map[string]string{"a.go": "package a\n"})}

	e.Ingest("a2", []Item{theirs, mine})
	g, err := e.Build(BuildRequest{AgentID: "a2", Seed: []Item{{ID: "seeded", Class: ClassCode, Source: SourceRepo, Content: "package s\n", From: derived("a1", 4, map[string]string{"s.go": "package s\n"})}}, Reason: "start"})
	if err != nil {
		t.Fatal(err)
	}
	text := Render(g)
	if strings.Contains(text, "agent 1") || strings.Contains(text, "package s") {
		t.Fatalf("another agent's code reached this context:\n%s", text)
	}
	if !strings.Contains(text, "package a\n") {
		t.Fatalf("its own code is missing:\n%s", text)
	}
	if n := e.Metrics("a2").ForeignRejected; n != 2 {
		t.Fatalf("ForeignRejected = %d, want 2 (one ingested, one seeded)", n)
	}
	if _, err := e.Replace("a2", []Item{theirs}, "replace"); err != nil {
		t.Fatal(err)
	}
	if n := e.Metrics("a2").ForeignRejected; n != 3 {
		t.Fatalf("Replace let foreign code through: ForeignRejected = %d", n)
	}

	// Following a ChangeReference is the explicit way in.
	if n := e.IngestImported("a2", []Item{theirs}); n != 1 {
		t.Fatalf("imported = %d", n)
	}
	g2, err := e.Refresh("a2", "after import")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(Render(g2), "package a // agent 1") {
		t.Fatalf("an explicit import did not arrive:\n%s", Render(g2))
	}
	// Its hashes still describe agent 1's file, so against this agent's own files it reads as changed.
	if res := e.Revalidate("a2", lookupOf(map[string]string{"a.go": "package a\n"})); len(res.Stale) != 1 || res.Stale[0].ID != "theirs" {
		t.Fatalf("imported code was not checked against the files this agent has: %+v", res)
	}

	// A fork is a deliberate copy; what it carries is marked as brought along.
	forkID, fg, err := e.Fork("a2")
	if err != nil {
		t.Fatal(err)
	}
	if fg.Workspace != nil {
		t.Fatal("a fork inherited its source's workspace")
	}
	if !strings.Contains(Render(fg), "package a // agent 1") {
		t.Fatalf("a fork lost what it was copied from:\n%s", Render(fg))
	}
	// Only what the fork was handed is trusted; a fresh copy from agent 1 is still foreign to it.
	e.Ingest(forkID, []Item{theirs})
	if got, want := e.Metrics(forkID).ForeignRejected, e.Metrics("a2").ForeignRejected+1; got != want {
		t.Fatalf("fork ForeignRejected = %d, want %d", got, want)
	}
}
