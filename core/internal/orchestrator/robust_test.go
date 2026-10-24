package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/taskgraph"
)

// An empty list must reach the UI as [], never null: the extension treats null as "nothing to update" and keeps the
// previous session's rows on screen.
func TestSnapshotListsAreNeverNull(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	raw, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"agents", "tasks", "findings", "memory", "messages", "changes", "verification"} {
		if got := strings.TrimSpace(string(fields[key])); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
}

func TestAdmitTasksDropsRepeatsWithoutCappingPlanning(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	c.Graph.Add(taskgraph.Task{ID: "t1", Title: "Add login", Role: "worker", Status: taskgraph.Done})
	specs := []TurnTask{
		{ID: "a", Title: "add  LOGIN", Role: "worker"}, // same work as t1
		{ID: "b", Title: "Add login", Role: "reviewer"},
		{ID: "c", Title: "Write docs", Role: "worker"},
		{ID: "d", Title: "Write docs", Role: "worker"}, // repeated within the same turn
	}
	got := c.admitTasks(specs)
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("admitted = %+v", got)
	}
	// Well past the old 40-task limit, distinct tasks are all admitted and no notice is posted.
	var many []TurnTask
	for i := 0; i < 50; i++ {
		many = append(many, TurnTask{ID: fmt.Sprint(i), Title: fmt.Sprint("task ", i), Role: "worker"})
	}
	if got := c.admitTasks(many); len(got) != 50 {
		t.Fatalf("admitted %d distinct tasks, want 50", len(got))
	}
	for _, m := range c.Current().Messages {
		if strings.Contains(m.Text, "stopped planning") {
			t.Fatalf("planning was stopped: %q", m.Text)
		}
	}
}

func TestAdmitTasksReplansFailedOrCanceledWork(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	c.Graph.Add(taskgraph.Task{ID: "f", Title: "Fix parser", Role: "worker", Status: taskgraph.Failed})
	c.Graph.Add(taskgraph.Task{ID: "x", Title: "Fix lexer", Role: "worker", Status: taskgraph.Canceled})
	got := c.admitTasks([]TurnTask{
		{ID: "p", Title: "Fix parser", Role: "worker"},
		{ID: "l", Title: "Fix lexer", Role: "worker"},
	})
	if len(got) != 2 {
		t.Fatalf("admitted = %+v, want failed and canceled tasks planned again", got)
	}
	// Family-shaped work: the guard's family key does not change what failed or cancelled work is allowed to do.
	// Only an unanswered question closes a family.
	got = c.admitTasks([]TurnTask{
		{ID: "q", Title: "SPT stage 2 per spt-impl-plan contract", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
		{ID: "r", Title: "SPT stage 2", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
	})
	if len(got) != 2 {
		t.Fatalf("admitted = %+v, want family-shaped work planned again when nothing is waiting on an answer", got)
	}
	if fams := c.unresolvedFamilies(); len(fams) != 0 {
		t.Fatalf("families reported as blocked = %v, want none", fams)
	}
}

// A task that failed without asking anything is not parked: its family stays open under any id or title.
// "SPT stage 2 per spt-impl-plan contract" is the incident's verified task title; "SPT stage 2" is a HYPOTHETICAL
// reworded fragment of the same family, reconstructed for the test.
func TestAFailedTaskWithoutAQuestionLeavesItsFamilyOpen(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	c.Graph.Add(taskgraph.Task{ID: "f", Title: "SPT stage 2 per spt-impl-plan contract", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai", Status: taskgraph.Failed})
	c.Graph.Add(taskgraph.Task{ID: "x", Title: "SPT stage 2", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai", Status: taskgraph.Canceled})
	if fams := c.unresolvedFamilies(); len(fams) != 0 {
		t.Fatalf("blocked families = %v, want none for work that asked nothing", fams)
	}
	got := c.admitTasks([]TurnTask{
		{ID: "p", Title: "SPT stage 2 per spt-impl-plan contract", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
		{ID: "l", Title: "SPT stage 2", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
	})
	if len(got) != 2 {
		t.Fatalf("admitted = %+v, want failed and canceled work planned again", got)
	}
}

// A task waiting on a dependency is Blocked for an entirely different reason. It is not an open question, so it
// must not close its family, and settled() must keep treating it as unfinished work.
// "SPT stage 2 per spt-impl-plan contract" is the incident's verified task title.
func TestADependencyBlockedTaskIsNotAnOpenQuestion(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	dep := c.Graph.Add(taskgraph.Task{ID: "dep", Title: "SPT stage 2 per spt-impl-plan contract", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"})
	child := c.Graph.Add(taskgraph.Task{ID: "child", Title: "SPT stage 2 per spt-impl-plan contract", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai", Depends: []string{dep.ID}})
	if got, _ := c.Graph.Get(child.ID); got.Status != taskgraph.Blocked {
		t.Fatalf("child = %s, want the dependency Blocked status", got.Status)
	}
	if open := c.openQuestions(); len(open) != 0 {
		t.Fatalf("open questions = %+v, want none for a dependency wait", open)
	}
	if fams := c.unresolvedFamilies(); len(fams) != 0 {
		t.Fatalf("blocked families = %v, want none for a dependency wait", fams)
	}
	if c.settled() {
		t.Fatal("settled() = true with a task Blocked on a dependency; that status must keep its meaning")
	}
	got := c.admitTasks([]TurnTask{{ID: "p", Title: "SPT stage 2", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"}})
	if len(got) != 1 || got[0].ID != "p" {
		t.Fatalf("admitted = %+v, want a dependency-blocked family left plannable", got)
	}
}

// The answer is the only thing that reopens a blocked family, and it is recorded on the task rather than inferred
// from whatever the user typed next. A reader already holding the block is not rewritten in place.
func TestAnAnswerReopensTheBlockedFamilyAndRestartsItsBudget(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	verified := "SPT stage 2 per spt-impl-plan contract" // the incident's verified task title
	blocked := c.Graph.Add(taskgraph.Task{
		ID: "task-blocked", Title: verified, Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai",
		Status: taskgraph.Failed, Attempts: maxFamilyAttempts,
	})
	question := "Which contract governs stage 2?"
	if _, err := c.Graph.Update(blocked.ID, func(x *taskgraph.Task) {
		x.BlockedOn = &taskgraph.BlockedOn{Question: question, Family: taskgraph.FamilyKey(blocked), BlockedAt: time.Now().UTC()}
	}); err != nil {
		t.Fatal(err)
	}
	open, _ := c.Graph.Get(blocked.ID)
	held := open.BlockedOn

	// An unknown id or an empty answer is not an answer.
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: "task-unknown", Answer: "Follow spt-staged-plan.md lines 67-78."}})
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: blocked.ID, Answer: "   "}})
	if still, _ := c.Graph.Get(blocked.ID); !still.BlockedOn.Unresolved() {
		t.Fatal("an unknown id or a blank answer cleared the block")
	}
	if got := c.admitTasks([]TurnTask{{ID: "n0", Title: "SPT stage 2 per spt-impl-plan contract", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"}}); len(got) != 0 {
		t.Fatalf("admitted = %+v while the question is open", got)
	}

	answer := "Follow spt-staged-plan.md lines 67-78."
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: blocked.ID, Answer: answer}})
	answered, _ := c.Graph.Get(blocked.ID)
	if answered.BlockedOn.Unresolved() {
		t.Fatal("the recorded answer did not clear the block")
	}
	if answered.BlockedOn.Resolution != answer || answered.BlockedOn.ResolvedAt.IsZero() || answered.BlockedOn.Question != question {
		t.Fatalf("answer = %+v", answered.BlockedOn)
	}
	if held.Resolution != "" || held == answered.BlockedOn {
		t.Fatal("the answer was written into the block a reader already held")
	}
	// A second answer changes nothing: the first one stands.
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: blocked.ID, Answer: "Ignore the staged plan."}})
	if again, _ := c.Graph.Get(blocked.ID); again.BlockedOn.Resolution != answer {
		t.Fatalf("resolution = %q after a second answer, want the first one", again.BlockedOn.Resolution)
	}

	// The family may be planned again, under a new id and a reworded title. The reworded title is a HYPOTHETICAL
	// fragment of the same family, reconstructed for the test.
	got := c.admitTasks([]TurnTask{{ID: "n1", Title: "SPT stage 2 per the staged plan", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"}})
	if len(got) != 1 || got[0].ID != "n1" {
		t.Fatalf("admitted = %+v, want the answered family planned again", got)
	}
	// The launches it spent before the answer do not count against the new budget.
	fresh := c.Graph.Add(taskgraph.Task{ID: "task-fresh", Title: "SPT stage 2", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"})
	if n := c.familyAttempts(fresh); n != 0 {
		t.Fatalf("family attempts after the answer = %d, want a fresh budget", n)
	}
}

// A question that a budget compression dropped was a question the user never saw. It is pinned to the Primary's
// state as a constraint, so compression keeps it and a reload brings it back.
// "SPT stage 2 per spt-impl-plan contract" is the incident's verified task title.
func TestAnOpenQuestionSurvivesCompressionAndAReload(t *testing.T) {
	c, _, _ := briefFixture(t)
	primary, _ := c.Agents.Primary()
	task := c.Graph.Add(taskgraph.Task{ID: "task-q", Title: "SPT stage 2 per spt-impl-plan contract", Role: string(agent.Worker), Status: taskgraph.Failed})
	question := "Which contract governs stage 2, spt-impl-plan or spt-staged-plan?"
	if _, err := c.Graph.Update(task.ID, func(x *taskgraph.Task) {
		x.BlockedOn = &taskgraph.BlockedOn{Question: question, Family: taskgraph.FamilyKey(task), BlockedAt: time.Now().UTC()}
	}); err != nil {
		t.Fatal(err)
	}
	c.pinBlocks(primary.ID)

	var pinned ctxeng.Item
	for _, it := range c.Engine.SnapshotPool(primary.ID) {
		if it.Title == "Unresolved questions" {
			pinned = it
		}
	}
	if !pinned.Pinned || pinned.Kind != ctxeng.KindConstraint || pinned.Source != ctxeng.SourceSystem || pinned.Class != ctxeng.ClassTask {
		t.Fatalf("pinned question = %+v", pinned)
	}
	for _, want := range []string{question, task.ID, "resolvedBlocks"} {
		if !strings.Contains(pinned.Content, want) {
			t.Fatalf("pinned question lost %q: %q", want, pinned.Content)
		}
	}

	// Over budget, and after a compression pass, the question is still in the Primary's prompt.
	if _, err := c.Engine.Refresh(primary.ID, "compress"); err != nil {
		t.Fatal(err)
	}
	gen, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: primary.ID, Role: string(agent.Primary), Phase: ctxeng.PhaseDesign, Budget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := ctxeng.Render(gen); !strings.Contains(got, question) {
		t.Fatalf("compression dropped the question: %s", got)
	}

	// A reload rebuilds the engine from the stored generation; the question comes back with it.
	reloaded := ctxeng.New()
	reloaded.RestoreGeneration(*gen)
	c.Engine = reloaded
	next, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: primary.ID, Role: string(agent.Primary), Phase: ctxeng.PhaseDesign, Budget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := ctxeng.Render(next); !strings.Contains(got, question) {
		t.Fatalf("reload dropped the question: %s", got)
	}

	// Pinning runs on every turn, so it must replace the group rather than append to it.
	c.pinBlocks(primary.ID)
	c.pinBlocks(primary.ID)
	n := 0
	for _, it := range c.Engine.SnapshotPool(primary.ID) {
		if it.Title == "Unresolved questions" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Unresolved questions items = %d, want 1", n)
	}
	// Answering clears the group: the question is no longer shown to the Primary.
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: task.ID, Answer: "Follow spt-staged-plan.md lines 67-78."}})
	c.pinBlocks(primary.ID)
	for _, it := range c.Engine.SnapshotPool(primary.ID) {
		if it.Title == "Unresolved questions" {
			t.Fatalf("an answered question is still pinned: %q", it.Content)
		}
	}
}

func TestPrimaryProseIsShownNotRepaired(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime("Sure. The function lives in main.go and returns the sum.")
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	turn, err := c.primaryTurn(context.Background(), a, prompt.Parts{}, "where is sum?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(turn.UserMessage, "main.go") || len(turn.Tasks) != 0 {
		t.Fatalf("turn = %+v", turn)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 1 {
		t.Fatalf("a plain answer cost %d model calls, want 1", len(r.tasks))
	}
}

func TestFailTaskDoesNotRetryWhatCannotSucceed(t *testing.T) {
	for _, tc := range []struct {
		cause     error
		wantRetry bool
	}{
		{errors.New("401 Unauthorized: invalid api key"), false},
		{errors.New("provider quota exceeded"), false},
		{errors.New("stream closed unexpectedly"), true},
		{context.Canceled, false},
	} {
		c := New(logging.New(nil, logging.Error))
		s := c.CreateSession(t.TempDir(), "", ModeBuild)
		c.Settings.MaxRetries = 1
		c.Sched = schedulerForRetry(1)
		c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
		a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace, Model: "mock/t1"})
		task := c.Graph.Add(taskgraph.Task{ID: "x", Title: "x", Role: "worker", Workspace: s.Workspace, Model: "mock/t1", Attempt: 1, MaxAttempts: 2})
		c.failTask(task, a, tc.cause)
		got, _ := c.Graph.Get(task.ID)
		retried := got.Status == taskgraph.Queued
		if retried != tc.wantRetry {
			t.Errorf("cause %q: retried = %v (status %s), want %v", tc.cause, retried, got.Status, tc.wantRetry)
		}
		if errors.Is(tc.cause, context.Canceled) && got.Status != taskgraph.Canceled {
			t.Errorf("a stopped task should be canceled, got %s", got.Status)
		}
	}
}

func TestCancelOrphansRetiresTasksBehindAFailure(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	c.Graph.Add(taskgraph.Task{ID: "root", Title: "root", Role: "worker", Status: taskgraph.Failed})
	c.Graph.Add(taskgraph.Task{ID: "child", Title: "child", Role: "worker", Status: taskgraph.Blocked, Depends: []string{"root"}})
	c.Graph.Add(taskgraph.Task{ID: "other", Title: "other", Role: "worker", Status: taskgraph.Blocked, Depends: []string{"nothing-failed"}})
	c.cancelOrphans()
	if got, _ := c.Graph.Get("child"); got.Status != taskgraph.Canceled {
		t.Fatalf("child = %s", got.Status)
	}
	if got, _ := c.Graph.Get("other"); got.Status != taskgraph.Blocked {
		t.Fatalf("other = %s", got.Status)
	}
}

func TestGuardTurnsPanicIntoError(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	err := c.guard("unit", func() error {
		var m map[string]int
		m["x"] = 1
		return nil
	})
	if err == nil || !strings.HasPrefix(err.Error(), "internal error in unit") {
		t.Fatalf("guard = %v", err)
	}
	if err := c.guard("unit", func() error { return nil }); err != nil {
		t.Fatalf("guard on success = %v", err)
	}
}
