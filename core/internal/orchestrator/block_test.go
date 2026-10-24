package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
)

// blockCoord is a Coord with only the task graph, enough for admission, answers and the family cap.
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
		{TaskID: first.ID, Answer: "use the new one"},
		{TaskID: first.ID, Answer: "later answer"},
	})
	got, _ := c.Graph.Get(first.ID)
	if got.BlockedOn == nil || got.BlockedOn.Resolution != "use the new one" || got.BlockedOn.Unresolved() {
		t.Fatalf("blockedOn %+v, want first valid answer recorded and resolved", got.BlockedOn)
	}
	if open := c.openQuestions(); len(open) != 0 {
		t.Fatalf("open questions %d, want 0", len(open))
	}
}

func TestFamilyAttemptsCountAcrossIDsAndCapBlocks(t *testing.T) {
	c, first := blockCoord(t)
	c.Graph.Update(first.ID, func(x *taskgraph.Task) { x.Attempts = 2 })
	second := c.Graph.Add(taskgraph.Task{Title: "widget   STAGE 2!", Role: "worker", Paths: []string{"a.go", "b.go"}, Scope: "API"})
	c.Graph.Update(second.ID, func(x *taskgraph.Task) { x.Attempts = 1 })
	other := c.Graph.Add(taskgraph.Task{Title: "Widget stage 2", Role: "test", Paths: []string{"a.go", "b.go"}, Scope: "api"})
	c.Graph.Update(other.ID, func(x *taskgraph.Task) { x.Attempts = 5 })

	got, _ := c.Graph.Get(second.ID)
	if n := c.familyAttempts(got); n != 3 {
		t.Fatalf("familyAttempts %d, want 3 (other role is a separate family)", n)
	}
	c.capBlock(got, 3)
	after, _ := c.Graph.Get(second.ID)
	if after.Status != taskgraph.Failed || after.BlockedOn == nil || !after.BlockedOn.Unresolved() || after.BlockedOn.Family != taskgraph.FamilyKey(got) {
		t.Fatalf("cap task %+v, want Failed with unresolved cap BlockedOn", after)
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

// The cap of three launches is reached through real dispatches, not by setting Attempts by hand. Four tasks that are
// one family - distinct ids, reworded titles - run one slot at a time. The first three launches run for real and are
// summed into the family budget; the fourth is refused before any agent starts for it, and the agent that had queued
// for it is retired with no model call. The reworded titles are HYPOTHETICAL fragments of one staged piece of work,
// reconstructed for the test.
func TestTheFamilyCapCountsThreeRealLaunchesAcrossIdsAndTitles(t *testing.T) {
	c, r := capTestCoord(t)
	ids := []string{"cap-1", "cap-2", "cap-3", "cap-4"}
	titles := []string{"Widget stage 2", "Widget  stage 2 (retry)", "widget STAGE 2!", "Widget stage 2 per plan"}
	var first taskgraph.Task
	for i, id := range ids {
		task := capTestFamilyTask(c, id, titles[i])
		if i == 0 {
			first = task
		} else if taskgraph.FamilyKey(task) != taskgraph.FamilyKey(first) {
			t.Fatalf("task %s (%q) is not in the family of %s", id, titles[i], first.ID)
		}
	}

	started := make(chan int, 1)
	go func() { started <- c.dispatch(context.Background(), c.scheduler()) }()

	// The first three launches run for real, one behind the single tier-1 slot. Each release frees the slot and the
	// dispatcher admits the next queued launch.
	begun := map[string]bool{}
	for i := 0; i < 3; i++ {
		title := nextBegun(t, r)
		begun[title] = true
		r.release(title)
	}
	select {
	case n := <-started:
		if n != 3 {
			t.Fatalf("dispatch started %d workers, want the three launches inside the cap", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish once the family reached its cap")
	}

	// The fourth launch never reached a model call.
	select {
	case title := <-r.begun:
		t.Fatalf("%q began after the family reached its cap of %d", title, maxFamilyAttempts)
	case <-time.After(100 * time.Millisecond):
	}
	for i := 0; i < 3; i++ {
		if !begun[titles[i]] {
			t.Errorf("launch %q never ran", titles[i])
		}
	}
	if begun[titles[3]] {
		t.Fatalf("the fourth launch %q ran", titles[3])
	}

	// The budget really is three real launches, summed across ids and titles.
	if n := c.familyAttempts(first); n != maxFamilyAttempts {
		t.Fatalf("familyAttempts = %d, want %d from three real launches", n, maxFamilyAttempts)
	}

	for i, id := range ids {
		got, _ := c.Graph.Get(id)
		if i < 3 {
			if got.Status != taskgraph.Done {
				t.Errorf("launch %d (%s) ended %s, want done", i+1, id, got.Status)
			}
			continue
		}
		if got.Status != taskgraph.Failed {
			t.Fatalf("the capped task is %s, want failed", got.Status)
		}
		if got.BlockedOn == nil || !got.BlockedOn.Unresolved() {
			t.Fatalf("capped task blockedOn = %+v, want an open question", got.BlockedOn)
		}
		if !strings.Contains(got.BlockedOn.Question, "limit 3") {
			t.Fatalf("the cap question does not state the limit: %q", got.BlockedOn.Question)
		}
		if got.BlockedOn.Family != taskgraph.FamilyKey(first) {
			t.Fatalf("capped family = %q, want %q", got.BlockedOn.Family, taskgraph.FamilyKey(first))
		}
	}

	// The agent that had queued for the fourth launch is retired, never run.
	retired := agentsOf(c, ids[3])
	if len(retired) != 1 {
		t.Fatalf("the capped task has %d agents, want the one queued agent", len(retired))
	}
	if retired[0].Status != agent.Cancelled {
		t.Errorf("the queued agent is %s, want cancelled", retired[0].Status)
	}
	if !strings.Contains(retired[0].Activity, "family run limit reached") {
		t.Errorf("the queued agent says %q, want the cap reason", retired[0].Activity)
	}
}
