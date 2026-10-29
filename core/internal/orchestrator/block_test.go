package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
)

// blockCoord is a Coord with only the task graph, enough for admission, answers and the family.
func blockCoord(t *testing.T) (*Coord, taskgraph.Task) {
	t.Helper()
	c := &Coord{Graph: taskgraph.New()}
	first := c.Graph.Add(taskgraph.Task{Title: "Widget stage 2", Role: "worker", Paths: []string{"b.go", "a.go"}, Scope: "api"})
	return c, first
}

func TestAdmitRejectsBlockedFamilyUnderAnyIDOrTitle(t *testing.T) {
	c, first := blockCoord(t)
	c.Graph.Update(first.ID, func(x *taskgraph.Task) {
		x.Status = taskgraph.Failed
		x.BlockedOn = &taskgraph.BlockedOn{Question: "which contract?", Family: taskgraph.FamilyKey(*x), BlockedAt: time.Now().UTC()}
	})
	specs := []TurnTask{
		{ID: "new-id", Title: "Widget  stage 2 (retry)", Role: "worker", Paths: []string{"a.go", "b.go"}, Scope: "API"},
		{ID: "other", Title: "Widget stage 2", Role: "worker", Paths: []string{"a.go", "b.go"}, Scope: "api"},
		{ID: "elsewhere", Title: "Widget stage 3", Role: "worker", Paths: []string{"a.go", "b.go"}, Scope: "api"},
	}
	kept, _, _ := c.admitTaskAliases(specs)
	if len(kept) != 1 || kept[0].ID != "elsewhere" {
		t.Fatalf("kept %+v, want only the stage 3 spec", kept)
	}
}

func TestResolvedBlocksRecordAnswerOnce(t *testing.T) {
	c, first := blockCoord(t)
	c.Graph.Update(first.ID, func(x *taskgraph.Task) {
		x.BlockedOn = &taskgraph.BlockedOn{Question: "which contract?", Family: taskgraph.FamilyKey(*x), BlockedAt: time.Now().UTC()}
	})
	c.applyResolvedBlocks([]ResolvedBlock{
		{TaskID: "missing", Answer: "ignored"},
		{TaskID: first.ID, Answer: "use the new one", Cite: "use the new one"},
		{TaskID: first.ID, Answer: "later answer"},
	}, "use the new one")
	got, _ := c.Graph.Get(first.ID)
	if got.BlockedOn == nil || got.BlockedOn.Resolution != "use the new one" || got.BlockedOn.Unresolved() {
		t.Fatalf("blockedOn %+v, want first valid answer recorded and resolved", got.BlockedOn)
	}
	if open := c.openQuestions(); len(open) != 0 {
		t.Fatalf("open questions %d, want 0", len(open))
	}
}

// capTestCoord is a direct-mode (non-Git) workspace with the gated runtime and a single tier-1 slot, so same-family
// tasks run one at a time and a later launch can be caught waiting for a slot. It adds no tasks.
func capTestCoord(t *testing.T) (*Coord, *gatedRuntime) {
	t.Helper()
	return newQueueCoord(t, scheduler.Limits{MaxTier1: 1}, agent.Worker)
}

// capTestFamilyTask adds one worker task in the same respawn family: a distinct id and a reworded title, but the
// same role, paths and scope, so FamilyKey collapses them all to one family.
func capTestFamilyTask(c *Coord, id, title string) taskgraph.Task {
	return c.Graph.Add(taskgraph.Task{
		ID: id, Title: title, Role: string(agent.Worker),
		Workspace: c.Current().Workspace, Difficulty: routing.Medium, Model: "mock/t1",
		Paths: []string{"a.go", "b.go"}, Scope: "api",
	})
}

// A fourth launch of a family that already ran three times is not refused or parked: it spawns like any other.
func TestAFourthFamilyLaunchSpawnsWithoutAsking(t *testing.T) {
	c, r := capTestCoord(t)
	for _, prior := range []struct{ id, title string }{
		{"prior-1", "Widget stage 2"}, {"prior-2", "Widget stage 2 (retry)"}, {"prior-3", "widget STAGE 2!"},
	} {
		capTestFamilyTask(c, prior.id, prior.title)
		c.Graph.Update(prior.id, func(x *taskgraph.Task) { x.Status, x.Attempt, x.Finished = taskgraph.Done, 1, time.Now().UTC() })
	}
	capTestFamilyTask(c, "fourth", "Widget stage 2 per plan")
	started := make(chan int, 1)
	go func() { started <- c.dispatch(context.Background(), c.scheduler()) }()
	r.release(nextBegun(t, r))
	select {
	case n := <-started:
		if n != 1 {
			t.Fatalf("started %d workers, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	got, _ := c.Graph.Get("fourth")
	if got.Status != taskgraph.Done || got.BlockedOn != nil {
		t.Fatalf("fourth launch ended %s with block %+v, want done without a question", got.Status, got.BlockedOn)
	}
}

// Successful launches of one family all run; none is parked for a run limit.
func TestSuccessfulFamilyLaunchesAllRun(t *testing.T) {
	c, r := capTestCoord(t)
	ids := []string{"run-1", "run-2", "run-3", "run-4"}
	titles := []string{"Widget stage 2", "Widget stage 2 (retry)", "widget STAGE 2!", "Widget stage 2 per plan"}
	for i, id := range ids {
		capTestFamilyTask(c, id, titles[i])
	}
	started := make(chan int, 1)
	go func() { started <- c.dispatch(context.Background(), c.scheduler()) }()
	for range ids {
		title := nextBegun(t, r)
		r.release(title)
	}
	select {
	case n := <-started:
		if n != 4 {
			t.Fatalf("started %d workers, want 4", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	for _, id := range ids {
		got, _ := c.Graph.Get(id)
		if got.Status != taskgraph.Done {
			t.Errorf("%s ended %s, want done", id, got.Status)
		}
	}
}

// A worker that just failed leaves its task Queued for a retry with no Started or Finished. Past three attempts a
// sibling's success does not park it: the retry launches like any other.
func TestRetryAfterSiblingSuccessStillLaunches(t *testing.T) {
	c, r := capTestCoord(t)
	capTestFamilyTask(c, "retry-done", "Widget stage 2")
	capTestFamilyTask(c, "retry-next", "Widget stage 2 (retry)")
	c.Graph.Update("retry-next", func(x *taskgraph.Task) { x.Attempt = 3 })
	c.Graph.Update("retry-done", func(x *taskgraph.Task) { x.Status, x.Finished = taskgraph.Done, time.Now().UTC() })
	started := make(chan int, 1)
	go func() { started <- c.dispatch(context.Background(), c.scheduler()) }()
	r.release(nextBegun(t, r))
	select {
	case n := <-started:
		if n != 1 {
			t.Fatalf("started %d workers, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	got, _ := c.Graph.Get("retry-next")
	if got.Status != taskgraph.Done || got.BlockedOn != nil {
		t.Fatalf("retry ended %s with block %+v, want done", got.Status, got.BlockedOn)
	}
}
