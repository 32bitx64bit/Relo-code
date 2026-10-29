package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/taskgraph"
)

func TestQuestionLifecycleAndProvenance(t *testing.T) {
	c, _, s := briefFixture(t, `{"status":"blocked","summary":"Need direction","question":"Which approach?","options":[{"label":"Use A","recommended":true},{"label":"Use B"}]}`)
	events := &captured{}
	c.SetEmitter(events.emit)
	task := c.Graph.Add(taskgraph.Task{Title: "Widget stage 2", Role: "worker", Workspace: s.Workspace, Model: "mock/t1"})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Asking || len(got.BlockedOn.Options) != 2 || got.BlockedOn.Options[0].Label != "Use A" || !got.BlockedOn.Options[0].Recommended || c.settled() {
		t.Fatalf("question not parked: %+v", got)
	}
	if len(events.named("agent.failed")) != 0 || len(events.named("task.updated")) == 0 {
		t.Fatal("blocked worker emitted failure or omitted task update")
	}
	if kept := c.admitTasks([]TurnTask{{ID: "copy", Title: "Widget stage 2 retry", Role: "worker"}}); len(kept) != 0 {
		t.Fatalf("near-copy admitted: %+v", kept)
	}
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: task.ID, Answer: "Use A"}})
	got, _ = c.Graph.Get(task.ID)
	if !got.BlockedOn.Unresolved() {
		t.Fatal("uncited answer accepted")
	}
	c.SetDelegateAnswers(true)
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: task.ID, Answer: "Use A"}})
	got, _ = c.Graph.Get(task.ID)
	if got.Status != taskgraph.Queued || got.BlockedOn.AnsweredBy != taskgraph.AnsweredByDelegated || !strings.Contains(taskBrief(got), "Use A") {
		t.Fatalf("answer not resumed: %+v", got)
	}
}

func TestQuestionProtectedAndEvidence(t *testing.T) {
	c, _, _ := briefFixture(t)
	c.applyObjective(c.primaryID(t), "Use the verified blue contract for this implementation")
	c.SetDelegateAnswers(true)
	for _, question := range []string{"How should provider quota be handled?"} {
		task := c.Graph.Add(taskgraph.Task{Title: question, Status: taskgraph.Asking, BlockedOn: &taskgraph.BlockedOn{Question: question, BlockedAt: time.Now()}})
		c.applyResolvedBlocks([]ResolvedBlock{{TaskID: task.ID, Answer: "allow", Cite: "verified blue contract"}})
		got, _ := c.Graph.Get(task.ID)
		if !got.BlockedOn.Unresolved() {
			t.Fatalf("protected question answered by primary: %+v", got)
		}
		c.applyResolvedBlocks([]ResolvedBlock{{TaskID: task.ID, Answer: "allow", Cite: "allow"}}, "I allow this")
		got, _ = c.Graph.Get(task.ID)
		if got.BlockedOn.AnsweredBy != taskgraph.AnsweredByUser {
			t.Fatalf("user answer rejected: %+v", got)
		}
	}
	task := c.Graph.Add(taskgraph.Task{Title: "ordinary", Status: taskgraph.Asking, BlockedOn: &taskgraph.BlockedOn{Question: "Which contract?"}})
	c.SetDelegateAnswers(false)
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: task.ID, Answer: "blue", Cite: "verified blue contract"}})
	got, _ := c.Graph.Get(task.ID)
	if got.BlockedOn.AnsweredBy != taskgraph.AnsweredByPrimary {
		t.Fatalf("cited answer rejected: %+v", got)
	}
}

// A task parked on a legacy run-limit question, written by an older build, is an ordinary open question now: an
// answer re-queues it and the next dispatch launches it.
func TestLegacyRunLimitQuestionRequeuesWhenAnswered(t *testing.T) {
	c, r := capTestCoord(t)
	task := capTestFamilyTask(c, "legacy-run-limit", "Widget stage 2")
	c.Graph.Update(task.ID, func(x *taskgraph.Task) {
		x.Status, x.Attempt, x.Finished = taskgraph.Asking, 3, time.Now().UTC()
		x.BlockedOn = &taskgraph.BlockedOn{
			Question: "This work has already run 3 times (limit 3). Answer whether to allow more runs, or change the plan.",
			Options:  []taskgraph.Option{{Label: "Allow one more run"}, {Label: "Change the plan", Recommended: true}},
			Family:   taskgraph.FamilyKey(*x), BlockedAt: time.Now().UTC(),
		}
	})
	if err := c.AnswerQuestion(task.ID, "Allow one more run"); err != nil {
		t.Fatal(err)
	}
	answered, _ := c.Graph.Get(task.ID)
	if answered.Status != taskgraph.Queued || answered.BlockedOn.Unresolved() {
		t.Fatalf("answered legacy task is %s with block %+v, want queued and resolved", answered.Status, answered.BlockedOn)
	}
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
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Done {
		t.Fatalf("answered legacy task ended %s, want done", got.Status)
	}
}
