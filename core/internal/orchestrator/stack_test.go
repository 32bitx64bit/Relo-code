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
	if err := os.WriteFile(filepath.Join(w.Worktree, "a.txt"), []byte("one\nrenamed\n"), 0o644); err != nil {
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
		if got, _ := os.ReadFile(filepath.Join(a.Worktree, "a.txt")); string(got) != "one\nrenamed\n" {
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
	if got, _ := os.ReadFile(filepath.Join(u.Worktree, "a.txt")); string(got) != "one\n" {
		t.Fatalf("an unrelated worker saw the pending change: %q", got)
	}
	// And a probe with no dependency still reads the user's folder directly.
	loose := c.Graph.Add(taskgraph.Task{Title: "Look around", Role: string(agent.Probe), Workspace: dir, Model: "mock/t1", Difficulty: "easy"})
	if p := c.spawn(loose, agent.Probe); p.Worktree != dir {
		t.Fatalf("an independent probe got a worktree: %s", p.Worktree)
	}
}
