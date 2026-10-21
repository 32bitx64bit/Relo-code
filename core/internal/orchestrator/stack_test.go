package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

// The reviewer and the tester of a worker's change used to start from a copy of the user's tree that did not
// contain it. They reported on code that was not there, and the Primary planned more probes to sort it out.
func TestDependentTasksSeeTheChangesTheyDependOn(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	r := newDispatchRuntime(`{"status":"completed","summary":"renamed the tab"}`)
	c.SetRuntime(r)
	s := c.CreateSession(dir, "rename", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	c.refreshSnapshot()

	work := c.Graph.Add(taskgraph.Task{Title: "Rename the tab", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "medium"})
	w := c.spawn(work, agent.Worker)
	if w.Worktree == dir {
		t.Fatal("the worker should have its own worktree")
	}
	if err := os.WriteFile(filepath.Join(dirOf(t, c, w), "a.txt"), []byte("one\nrenamed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Graph.Update(work.ID, func(x *taskgraph.Task) { x.AgentID = w.ID }); err != nil {
		t.Fatal(err)
	}
	if err := c.execWorker(context.Background(), work, w); err != nil {
		t.Fatal(err)
	}
	if got := c.Changes(); len(got) != 1 || got[0].Status != "proposed" || got[0].Path != "a.txt" {
		t.Fatalf("changes = %+v", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\n" {
		t.Fatalf("the worker's edit reached the user's folder: %q", got)
	}

	for _, role := range []agent.Role{agent.Reviewer, agent.Test, agent.Probe} {
		next := c.Graph.Add(taskgraph.Task{Title: "Check it: " + string(role), Role: string(role), Workspace: dir, Model: "mock/t1", Difficulty: "easy", Depends: []string{work.ID}})
		a := c.spawn(next, role)
		if a.Worktree == dir || a.Worktree == w.Worktree {
			t.Fatalf("%s: worktree = %s, want a copy of its own", role, a.Worktree)
		}
		if got, _ := agentFile(c, a, "a.txt"); got != "one\nrenamed\n" {
			t.Fatalf("%s: a.txt = %q, want it to contain the change it depends on", role, got)
		}
		var note, diff string
		for _, it := range c.Engine.SnapshotPool(a.ID) {
			switch it.Title {
			case "Workspace":
				note = it.Content
			case "Diff under review":
				diff = it.Content
			}
		}
		if !strings.Contains(note, `"Rename the tab"`) || !strings.Contains(note, "not accepted") {
			t.Fatalf("%s: workspace note = %q", role, note)
		}
		if role == agent.Reviewer && !strings.Contains(diff, "+renamed") {
			t.Fatalf("reviewer got no diff: %q", diff)
		}
		if role != agent.Reviewer && diff != "" {
			t.Fatalf("%s was handed a diff it does not need", role)
		}
		if files := c.changesFor(a, RawReport{}); len(files) != 0 {
			t.Fatalf("%s: the change it builds on was counted as its own: %+v", role, files)
		}
	}

	// A task with no dependency on it still starts from the user's tree, without the unaccepted change.
	solo := c.Graph.Add(taskgraph.Task{Title: "Unrelated", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "easy"})
	u := c.spawn(solo, agent.Worker)
	if got, _ := agentFile(c, u, "a.txt"); got != "one\n" {
		t.Fatalf("an unrelated worker saw the pending change: %q", got)
	}
	// And a probe with no dependency still reads the user's folder directly.
	loose := c.Graph.Add(taskgraph.Task{Title: "Look around", Role: string(agent.Probe), Workspace: dir, Model: "mock/t1", Difficulty: "easy"})
	if p := c.spawn(loose, agent.Probe); p.Worktree != dir {
		t.Fatalf("an independent probe got a worktree: %s", p.Worktree)
	}
}

// A change that was recorded on a stacked base must say what it needs, not just look like a standalone proposal.
func TestRecordedChangesCarryNeedsAndBaseRev(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.SetRuntime(newDispatchRuntime(`{"status":"completed","summary":"done"}`, `{"status":"completed","summary":"done"}`))
	s := c.CreateSession(dir, "stack", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	c.refreshSnapshot()

	work := c.Graph.Add(taskgraph.Task{Title: "First", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "medium"})
	w := c.spawn(work, agent.Worker)
	if err := os.WriteFile(filepath.Join(dirOf(t, c, w), "a.txt"), []byte("one\nfirst\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Graph.Update(work.ID, func(x *taskgraph.Task) { x.AgentID = w.ID }); err != nil {
		t.Fatal(err)
	}
	if err := c.execWorker(context.Background(), work, w); err != nil {
		t.Fatal(err)
	}
	first := c.Changes()
	if len(first) != 1 || first[0].Status != "proposed" {
		t.Fatalf("first change = %+v", first)
	}

	next := c.Graph.Add(taskgraph.Task{Title: "Second", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "easy", Depends: []string{work.ID}})
	b := c.spawn(next, agent.Worker)
	if err := os.WriteFile(filepath.Join(dirOf(t, c, b), "a.txt"), []byte("one\nfirst\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Graph.Update(next.ID, func(x *taskgraph.Task) { x.AgentID = b.ID }); err != nil {
		t.Fatal(err)
	}
	if err := c.execWorker(context.Background(), next, b); err != nil {
		t.Fatal(err)
	}
	var got Change
	for _, ch := range c.Changes() {
		if ch.AgentID == b.ID {
			got = ch
		}
	}
	if got.ID == "" {
		t.Fatal("the dependent agent recorded no change")
	}
	if len(got.Needs) != 1 || got.Needs[0] != first[0].ID {
		t.Fatalf("needs = %v, want [%s]", got.Needs, first[0].ID)
	}
	if len(got.Lineage) != 1 || got.Lineage[0].ID != first[0].ID || got.Lineage[0].PatchHash == "" {
		t.Fatalf("lineage = %+v, want the parent with its patch hash", got.Lineage)
	}
	if got.BaseRev == "" {
		t.Fatal("the change has no BaseRev")
	}
}

// A worker spawned after an accept must be cut from the accepted file, not from an older base (R1b).
func TestSpawnAfterAcceptSeesTheAcceptedFile(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	s := c.CreateSession(dir, "r1b", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	c.refreshSnapshot()

	early := c.spawn(c.Graph.Add(taskgraph.Task{Title: "Early", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "easy"}), agent.Worker)
	if !c.WS.Available(early.ID, early.Worktree) {
		t.Fatal("the early worker has no workspace")
	}
	// The user accepts a change into the workspace while agents are running.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\naccepted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	late := c.spawn(c.Graph.Add(taskgraph.Task{Title: "Late", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "easy"}), agent.Worker)
	if got, _ := agentFile(c, late, "a.txt"); got != "one\naccepted\n" {
		t.Fatalf("the late agent saw %q, want the accepted file", got)
	}
}

// A queued worker is re-cut onto a fresh base and re-stacks its recorded parent. A parent whose patch hash no longer
// matches refuses the recut and keeps the old base.
func TestRecutQueuedRestacksLineageOntoFreshBase(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.SetRuntime(newDispatchRuntime(`{"status":"completed","summary":"done"}`))
	s := c.CreateSession(dir, "recut", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	c.refreshSnapshot()

	work := c.Graph.Add(taskgraph.Task{Title: "First", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "medium"})
	w := c.spawn(work, agent.Worker)
	if err := os.WriteFile(filepath.Join(dirOf(t, c, w), "a.txt"), []byte("one\nfirst\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Graph.Update(work.ID, func(x *taskgraph.Task) { x.AgentID = w.ID }); err != nil {
		t.Fatal(err)
	}
	if err := c.execWorker(context.Background(), work, w); err != nil {
		t.Fatal(err)
	}
	parent := c.Changes()[0]

	next := c.Graph.Add(taskgraph.Task{Title: "Second", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "easy", Depends: []string{work.ID}})
	b := c.spawn(next, agent.Worker)
	lineage := c.lineageOf(b.ID)
	if len(lineage) != 1 || lineage[0].ID != parent.ID {
		t.Fatalf("lineage = %+v, want the parent", lineage)
	}

	// The user writes a file after the queued worker was cut; the recut must pick it up.
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("accepted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.setLineage(b.ID, []Dep{{ID: parent.ID, PatchHash: "stale"}})
	c.recutQueued(next, b)
	if _, err := agentFile(c, b, "c.txt"); err == nil {
		t.Fatal("a hash-mismatched parent must keep the old base")
	}

	c.setLineage(b.ID, lineage)
	c.recutQueued(next, b)
	if got, _ := agentFile(c, b, "c.txt"); got != "accepted\n" {
		t.Fatalf("recut worktree c.txt = %q, want the fresh base", got)
	}
	if got, _ := agentFile(c, b, "a.txt"); got != "one\nfirst\n" {
		t.Fatalf("recut worktree a.txt = %q, want the re-stacked parent", got)
	}
	if got := c.lineageOf(b.ID); len(got) != 1 || got[0].ID != parent.ID {
		t.Fatalf("lineage after recut = %+v, want the parent", got)
	}
}
