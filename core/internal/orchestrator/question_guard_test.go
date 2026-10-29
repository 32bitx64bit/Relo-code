package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/taskgraph"
)

// askingFixture parks one worker task on a question for the user, as a worker's blocked report does.
func askingFixture(t *testing.T, question string, responses ...string) (*Coord, *dispatchRuntime, *captured, taskgraph.Task) {
	t.Helper()
	c, r, s := briefFixture(t, responses...)
	events := &captured{}
	c.SetEmitter(events.emit)
	task := c.Graph.Add(taskgraph.Task{ID: "task-ask", Title: "Widget stage 2 per contract", Role: string(agent.Worker), Workspace: s.Workspace, Paths: []string{"pkg/widget/render.go"}, Scope: "widgets"})
	if _, err := c.Graph.Update(task.ID, func(x *taskgraph.Task) {
		x.Status = taskgraph.Asking
		x.BlockedOn = &taskgraph.BlockedOn{Question: question, Family: taskgraph.FamilyKey(*x), BlockedAt: time.Now().UTC()}
	}); err != nil {
		t.Fatal(err)
	}
	task, _ = c.Graph.Get(task.ID)
	return c, r, events, task
}

// workarounds are the ways the incident's Primary planned around a question instead of waiting for the user.
func workarounds() map[string]TurnTask {
	return map[string]TurnTask{
		// Reworded, other role: the old guard compared role and title only.
		"reworded-other-role": {ID: "w1", Title: "Investigate widget contract at stage 2", Role: "researcher"},
		// Unrelated words, same file.
		"same-path": {ID: "w2", Title: "Vendor a different renderer", Role: "debug", Paths: []string{"./pkg/widget/render.go"}},
		// A directory above the file.
		"parent-dir": {ID: "w3", Title: "Tidy the package", Role: "worker", Paths: []string{"pkg/widget"}},
		"dependent":  {ID: "w4", Title: "Ship the release notes", Role: "worker", Deps: []string{"task-ask"}},
		"names-it":   {ID: "w5", Title: "Unblock things", Role: "architect", Description: "Task task-ask is stuck; pick an answer ourselves."},
		"same-id":    {ID: "task-ask", Title: "Rework everything", Role: "worker"},
		// Disjoint itself, but only runs after a refused workaround.
		"after-workaround": {ID: "w7", Title: "Publish docs", Role: "worker", Paths: []string{"docs/publish.md"}, Deps: []string{"w1"}},
	}
}

func keptIDs(kept []TurnTask) map[string]bool {
	ids := map[string]bool{}
	for _, k := range kept {
		ids[k.ID] = true
	}
	return ids
}

func TestOpenQuestionRefusesRewordedDifferentRoleWorkaround(t *testing.T) {
	c, _, events, asking := askingFixture(t, "Which contract governs stage 2?")
	var specs []TurnTask
	for _, spec := range workarounds() {
		specs = append(specs, spec)
	}
	specs = append(specs, TurnTask{ID: "disjoint", Title: "Update the changelog", Role: "worker", Paths: []string{"docs/changelog.md"}})

	kept := keptIDs(c.admitTasks(specs))
	for name, spec := range workarounds() {
		if kept[spec.ID] {
			t.Errorf("%s: %q was admitted while task %s waits for the user", name, spec.Title, asking.ID)
		}
	}
	if !kept["disjoint"] || len(kept) != 1 {
		t.Fatalf("kept %v, want only the disjoint task", kept)
	}

	// The refusal reaches the user and the Primary, with the reason and the question.
	if n := len(events.named("chat.message")); n != len(workarounds()) {
		t.Fatalf("chat messages = %d, want one per refusal (%d)", n, len(workarounds()))
	}
	primary, _ := c.Agents.Primary()
	var note string
	for _, item := range c.Engine.SnapshotPool(primary.ID) {
		if item.Title == "Refused blocked replans: "+asking.ID && item.Pinned {
			note = item.Content
		}
	}
	for _, want := range []string{"Investigate widget contract at stage 2", "shares pkg/widget/render.go with", "depends on task", "Which contract governs stage 2?", "Wait for the user"} {
		if !strings.Contains(note, want) {
			t.Errorf("pinned refusal note lacks %q:\n%s", want, note)
		}
	}
}

func TestOpenQuestionRefusalsAreLaunchedNowhere(t *testing.T) {
	c, _, _, asking := askingFixture(t, "Which contract governs stage 2?")
	primary, _ := c.Agents.Primary()
	var tasks []TurnTask
	for _, spec := range workarounds() {
		tasks = append(tasks, spec)
	}
	tasks = append(tasks, TurnTask{ID: "disjoint", Title: "Update the changelog", Role: "worker", Paths: []string{"docs/changelog.md"}})
	c.applyTurn(primary, Turn{Status: "in_progress", Tasks: tasks})

	if got, _ := c.Graph.Get(asking.ID); got.Status != taskgraph.Asking || !got.BlockedOn.Unresolved() {
		t.Fatalf("the asking task was changed: %+v", got)
	}
	all := c.Graph.List()
	if len(all) != 2 {
		t.Fatalf("graph has %d tasks, want the asking task and the disjoint one: %+v", len(all), all)
	}
	runnable := c.Graph.Runnable()
	if len(runnable) != 1 || runnable[0].Title != "Update the changelog" {
		t.Fatalf("runnable = %+v, want only the disjoint task", runnable)
	}
}

func TestAnsweredQuestionAdmitsRelatedWorkAgain(t *testing.T) {
	c, _, _, asking := askingFixture(t, "Which contract governs stage 2?")
	specs := []TurnTask{workarounds()["reworded-other-role"], workarounds()["same-path"], workarounds()["dependent"], workarounds()["parent-dir"]}
	if kept := c.admitTasks(specs); len(kept) != 0 {
		t.Fatalf("admitted %+v while the question was open", kept)
	}
	if err := c.AnswerQuestion(asking.ID, "Follow the staged plan."); err != nil {
		t.Fatal(err)
	}
	if kept := keptIDs(c.admitTasks(specs)); len(kept) != len(specs) {
		t.Fatalf("kept %v after the answer, want all %d", kept, len(specs))
	}
	primary, _ := c.Agents.Primary()
	for _, item := range c.Engine.SnapshotPool(primary.ID) {
		if item.Title == "Refused blocked replans: "+asking.ID {
			t.Fatal("the refusal note outlived the question")
		}
	}
}

// The user can hand answers to the Primary. Then it may answer, and so act on, an ordinary question; a question only
// the user may answer (provider, quota) stays guarded.
func TestDelegationLiftsTheGuardOnlyForOrdinaryQuestions(t *testing.T) {
	c, _, _, asking := askingFixture(t, "Which contract governs stage 2?")
	workaround := []TurnTask{workarounds()["same-path"]}
	if kept := c.admitTasks(workaround); len(kept) != 0 {
		t.Fatalf("admitted without delegation: %+v", kept)
	}
	c.SetDelegateAnswers(true)
	if kept := c.admitTasks(workaround); len(kept) != 1 {
		t.Fatalf("delegated question still guarded: %+v", kept)
	}
	// The same near-copy rule as before delegation: a reworded copy under the same role never slips through.
	if kept := c.admitTasks([]TurnTask{{ID: "copy", Title: "Widget stage 2 per contract retry", Role: "worker"}}); len(kept) != 0 {
		t.Fatalf("near copy admitted under delegation: %+v", kept)
	}

	c.Graph.Update(asking.ID, func(x *taskgraph.Task) {
		x.BlockedOn.Question = "How should provider quota be handled?"
	})
	if kept := c.admitTasks(workaround); len(kept) != 0 {
		t.Fatalf("a user-only question was worked around under delegation: %+v", kept)
	}
}

// A Primary turn that answers the question itself needs the user's words, evidence or delegation. Without them the
// answer is ignored and the related work in the same turn is refused.
func TestPrimaryCannotSelfAnswerToPlanAroundAQuestion(t *testing.T) {
	c, _, _, asking := askingFixture(t, "Which contract governs stage 2?")
	primary, _ := c.Agents.Primary()
	turn := Turn{
		ResolvedBlocks: []ResolvedBlock{{TaskID: asking.ID, Answer: "Use contract A", AnsweredBy: taskgraph.AnsweredByUser, Cite: "the user said use contract A"}},
		Tasks:          []TurnTask{workarounds()["same-path"]},
	}
	c.applyTurn(primary, turn, "continue please")
	got, _ := c.Graph.Get(asking.ID)
	if !got.BlockedOn.Unresolved() || got.Status != taskgraph.Asking {
		t.Fatalf("Primary answered for the user: %+v", got.BlockedOn)
	}
	if len(c.Graph.List()) != 1 {
		t.Fatalf("a workaround was planned beside the open question: %+v", c.Graph.List())
	}

	// The user's own message that carries the answer lifts the question, and the related work goes in the same turn.
	turn.ResolvedBlocks[0].Cite = "use contract A"
	c.applyTurn(primary, turn, "Please use contract A for this.")
	got, _ = c.Graph.Get(asking.ID)
	if got.BlockedOn.Unresolved() || got.BlockedOn.AnsweredBy != taskgraph.AnsweredByUser {
		t.Fatalf("user answer not recorded: %+v", got.BlockedOn)
	}
	if len(c.Graph.List()) != 2 {
		t.Fatalf("related work not admitted after the answer: %+v", c.Graph.List())
	}
}

// Delegation is the user's permission to answer; without it an uncited answer from the Primary is ignored.
func TestPrimaryAnswersOnlyWhenTheUserDelegated(t *testing.T) {
	c, _, _, asking := askingFixture(t, "Which contract governs stage 2?")
	primary, _ := c.Agents.Primary()
	turn := Turn{ResolvedBlocks: []ResolvedBlock{{TaskID: asking.ID, Answer: "Use contract A"}}}
	c.applyTurn(primary, turn)
	if got, _ := c.Graph.Get(asking.ID); !got.BlockedOn.Unresolved() {
		t.Fatal("Primary answered without delegation")
	}
	c.SetDelegateAnswers(true)
	c.applyTurn(primary, turn)
	if got, _ := c.Graph.Get(asking.ID); got.BlockedOn.Unresolved() || got.BlockedOn.AnsweredBy != taskgraph.AnsweredByDelegated {
		t.Fatalf("delegated answer not recorded: %+v", got.BlockedOn)
	}
}

// The Primary's context names each waiting question as wait-for-user, in the pinned state and in the turn message.
func TestPrimaryPromptListsOpenQuestionsAsWaitForUser(t *testing.T) {
	c, r, _, asking := askingFixture(t, "Which contract governs stage 2?", `{"status":"completed","userMessage":"waiting","tasks":[]}`)
	primary, _ := c.Agents.Primary()
	c.pinBlocks(primary.ID)
	var pinned string
	for _, item := range c.Engine.SnapshotPool(primary.ID) {
		if item.Title == "Unresolved questions" && item.Pinned && item.Kind == ctxeng.KindConstraint {
			pinned = item.Content
		}
	}
	for _, want := range []string{"WAIT FOR USER", asking.ID, "Which contract governs stage 2?", "pkg/widget/render.go", "core refuses"} {
		if !strings.Contains(pinned, want) {
			t.Errorf("pinned question lacks %q: %q", want, pinned)
		}
	}

	if err := c.loopPrimary(context.Background(), primary, "carry on"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	prompt := r.tasks[0].Prompt
	r.mu.Unlock()
	if !strings.Contains(prompt, "Waiting for the user") || !strings.Contains(prompt, "task-ask") || !strings.Contains(prompt, "Which contract governs stage 2?") {
		t.Fatalf("turn message does not list the open question:\n%s", prompt)
	}
	if err := c.AnswerQuestion(asking.ID, "Follow the staged plan."); err != nil {
		t.Fatal(err)
	}
	if note := c.waitNote(); note != "" {
		t.Fatalf("answered question still listed: %q", note)
	}
}

func TestRelatedAndSharedPaths(t *testing.T) {
	asking := taskgraph.Task{Title: "Widget stage 2 per contract", Role: "worker", Paths: []string{"pkg/widget", "main.go"}}
	cases := []struct {
		name    string
		other   taskgraph.Task
		related bool
		shared  string
	}{
		{"same stage other role", taskgraph.Task{Title: "Widget stage 2", Role: "researcher"}, true, ""},
		{"reworded other role", taskgraph.Task{Title: "Investigate widget contract at stage 2", Role: "debug"}, true, ""},
		{"other stage of the plan", taskgraph.Task{Title: "Widget stage 3 per contract", Role: "worker"}, false, ""},
		{"other batch", taskgraph.Task{Title: "SPT S3d Build diplomacy", Role: "worker"}, false, ""},
		{"unrelated", taskgraph.Task{Title: "Update the changelog", Role: "worker", Paths: []string{"docs/changelog.md"}}, false, ""},
		{"file under dir", taskgraph.Task{Title: "Unrelated", Paths: []string{"pkg/widget/render.go"}}, false, "pkg/widget"},
		{"cleaned path", taskgraph.Task{Title: "Unrelated", Paths: []string{"./a/../main.go"}}, false, "main.go"},
		{"sibling prefix is not a child", taskgraph.Task{Title: "Unrelated", Paths: []string{"pkg/widgetry/x.go"}}, false, ""},
	}
	for _, tc := range cases {
		if got := taskgraph.Related(asking, tc.other); got != tc.related {
			t.Errorf("%s: Related = %v, want %v", tc.name, got, tc.related)
		}
		shared := strings.Join(taskgraph.SharedPaths(asking, tc.other), ",")
		if shared != tc.shared {
			t.Errorf("%s: SharedPaths = %q, want %q", tc.name, shared, tc.shared)
		}
	}
}
