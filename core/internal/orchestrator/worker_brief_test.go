package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
)

func briefFixture(t *testing.T, responses ...string) (*Coord, *dispatchRuntime, *Session) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(responses...)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "objective", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	return c, r, s
}

// The worker's first message is its task in full; the pack keeps the title only, so the description is read once, last.
func TestWorkerStartsFromItsTaskNotAPointerToIt(t *testing.T) {
	c, r, s := briefFixture(t, `{"status":"completed","summary":"done"}`)
	task := c.Graph.Add(taskgraph.Task{
		Title: "Rename the flag", Description: "Change --old to --new in cmd/root.go and its help text.",
		Acceptance: []string{"help text shows --new"}, Paths: []string{"cmd/root.go"},
		Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium",
	})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sent := r.tasks[0]
	for _, want := range []string{"Your task: Rename the flag", "Change --old to --new", "- help text shows --new", "Files in scope: cmd/root.go"} {
		if !strings.Contains(sent.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, sent.Prompt)
		}
	}
	if !strings.HasSuffix(sent.Prompt, "return the report as JSON.") {
		t.Errorf("the task should be the last thing the worker reads:\n%s", sent.Prompt)
	}
	if strings.Contains(sent.System, "Change --old to --new") {
		t.Errorf("the description is sent twice; the system prompt has it too:\n%s", sent.System)
	}
	if !strings.Contains(sent.System, "Rename the flag") {
		t.Errorf("the pack lost the title, the anchor a conversation summary keeps")
	}
}

// What the Primary read out of the request reaches every sub agent that changes or judges something.
func TestWorkerSeedCarriesConstraintsButProbesDoNotGetThem(t *testing.T) {
	c, _, s := briefFixture(t)
	c.setConstraints([]string{"Do not touch the database schema", "Keep the public API unchanged"})
	task := c.Graph.Add(taskgraph.Task{Title: "Do it", Workspace: s.Workspace})

	worker := seedTitles(c.workerSeed(task, agent.Worker))
	item, ok := worker["Constraints"]
	if !ok || !item.Pinned || !strings.Contains(item.Content, "Do not touch the database schema") || !strings.Contains(item.Content, "Keep the public API unchanged") {
		t.Fatalf("worker constraints item = %+v (present %v)", item, ok)
	}
	if _, ok := seedTitles(c.workerSeed(task, agent.Reviewer))["Constraints"]; !ok {
		t.Fatal("a reviewer checks the change against the constraints")
	}
	if _, ok := seedTitles(c.workerSeed(task, agent.Probe))["Constraints"]; ok {
		t.Fatal("a probe only reads; the constraints are not for it")
	}
}

func TestConstraintsPersistInTheSessionAndAnEmptyListLeavesThem(t *testing.T) {
	c, _, _ := briefFixture(t)
	primary, _ := c.Agents.Primary()
	c.applyTurn(primary, Turn{Constraints: []string{"Leave the tests alone"}})
	c.applyTurn(primary, Turn{Constraints: nil})
	if got := c.Current().Constraints; len(got) != 1 || got[0] != "Leave the tests alone" {
		t.Fatalf("constraints = %v", got)
	}
	if note := c.constraintsNote(); !strings.Contains(note, "Leave the tests alone") {
		t.Fatalf("the Primary is not shown its own constraints: %q", note)
	}
	c.applyTurn(primary, Turn{Constraints: []string{"Match the existing style"}})
	if got := c.Current().Constraints; len(got) != 1 || got[0] != "Match the existing style" {
		t.Fatalf("a new list should replace the old one: %v", got)
	}
}

// A retried task starts from a new agent; it must be told what the last one ran into.
func TestRetryIsToldWhyTheLastAttemptFailed(t *testing.T) {
	c, r, s := briefFixture(t, `{"status":"failed","summary":"go build fails: undefined: Frobnicate"}`)
	task := c.Graph.Add(taskgraph.Task{Title: "Wire it", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium", MaxAttempts: 3})
	if got := seedTitles(c.workerSeed(task, agent.Worker)); got["Previous attempt failed"].Content != "" {
		t.Fatal("a first attempt has nothing to be told")
	}
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	calls := len(r.tasks)
	r.mu.Unlock()
	if calls != 1 {
		t.Fatalf("model calls = %d", calls)
	}
	again, _ := c.Graph.Get(task.ID)
	note := seedTitles(c.workerSeed(again, agent.Worker))["Previous attempt failed"]
	if !strings.Contains(note.Content, "undefined: Frobnicate") || !strings.Contains(note.Content, "different approach") {
		t.Fatalf("retry note = %q", note.Content)
	}
}

// A blocked worker has asked a question. Retrying it would ask the same question again, so it is not retried: the
// question goes to the Primary in the report.
func TestBlockedWorkerAsksTheQuestionAndIsNotRetried(t *testing.T) {
	c, r, s := briefFixture(t, `{"status":"blocked","summary":"Both files define Load.","question":"Which Load should the CLI call, config.Load or settings.Load?"}`)
	c.Settings.AutoVerify = true // a blocked agent changed nothing worth checking
	task := c.Graph.Add(taskgraph.Task{Title: "Call Load", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium", MaxAttempts: 3})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	calls := len(r.tasks)
	r.mu.Unlock()
	if calls != 1 {
		t.Fatalf("a blocked worker was run %d times, want once", calls)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Failed {
		t.Fatalf("task = %s; it must end so dependents do not wait on it", got.Status)
	}
	rep, ok := got.Report.(agent.Report)
	if !ok || rep.Question != "Which Load should the CLI call, config.Load or settings.Load?" || !strings.HasPrefix(rep.Summary, "Blocked, needs a decision:") {
		t.Fatalf("report = %+v", got.Report)
	}
	ag, _ := c.Agents.Get(a.ID)
	if ag.Status != agent.Interrupted || !strings.Contains(ag.Activity, "Needs a decision") {
		t.Fatalf("agent = %s %q", ag.Status, ag.Activity)
	}
	if n := c.failureNote(got); len(n) != 0 {
		t.Fatal("a question is not a failure to retry from")
	}
	// The Primary reads the question in the report it is given.
	found := false
	for _, it := range c.Engine.SnapshotPool(c.primaryID(t)) {
		if strings.Contains(it.Content, "Which Load should the CLI call") {
			found = true
		}
	}
	if !found {
		t.Fatal("the question never reached the Primary's context")
	}
}

// The question has to live somewhere a later plan cannot overwrite. It is recorded on the task itself, with the
// family key the guard uses, so a re-plan under a new id still finds it.
func TestBlockedWorkerLeavesTheQuestionOnItsTask(t *testing.T) {
	c, r, s := briefFixture(t, `{"status":"blocked","summary":"Both files define Load.","question":"Which Load should the CLI call, config.Load or settings.Load?"}`)
	task := c.Graph.Add(taskgraph.Task{
		Title: "SPT stage 2 per spt-impl-plan contract", Role: string(agent.Worker), Workspace: s.Workspace,
		Model: "mock/t1", Difficulty: "medium", Paths: []string{"pax_game_ai.c"}, Scope: "ai",
	})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Failed {
		t.Fatalf("task = %s; a blocked worker is not done, but it must not stay runnable either", got.Status)
	}
	if got.BlockedOn == nil || !got.BlockedOn.Unresolved() {
		t.Fatalf("blockedOn = %+v, want an open question", got.BlockedOn)
	}
	if got.BlockedOn.Question != "Which Load should the CLI call, config.Load or settings.Load?" {
		t.Fatalf("question = %q", got.BlockedOn.Question)
	}
	if want := taskgraph.FamilyKey(got); got.BlockedOn.Family != want {
		t.Fatalf("family = %q, want %q", got.BlockedOn.Family, want)
	}
	if got.BlockedOn.BlockedAt.IsZero() {
		t.Fatal("the block was not timestamped")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 1 {
		t.Fatalf("model calls = %d, want 1: a blocked worker is not retried", len(r.tasks))
	}
}

// Sixteen copies of one worker task were spawned because every re-plan arrived under a new id and a reworded title.
// The open question, not the id or the wording, is what closes the family.
// "SPT stage 2 per spt-impl-plan contract" is the incident's verified task title.
func TestABlockedFamilyRefusesEveryReplanUntilItIsAnswered(t *testing.T) {
	c, _, _ := briefFixture(t)
	verified := "SPT stage 2 per spt-impl-plan contract" // the incident's verified task title
	blocked := c.Graph.Add(taskgraph.Task{
		ID: "task-blocked", Title: verified, Role: string(agent.Worker), Status: taskgraph.Failed,
		Paths: []string{"pax_game_ai.c"}, Scope: "ai",
	})
	if _, err := c.Graph.Update(blocked.ID, func(x *taskgraph.Task) {
		x.BlockedOn = &taskgraph.BlockedOn{
			Question: "Which contract governs stage 2?", Family: taskgraph.FamilyKey(blocked), BlockedAt: time.Now().UTC(),
		}
	}); err != nil {
		t.Fatal(err)
	}
	specs := []TurnTask{
		{ID: "s1", Title: verified, Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
		// HYPOTHETICAL: reworded fragments of the same stage, reconstructed to show the guard does not depend on the
		// exact title the incident happened to use.
		{ID: "s2", Title: "SPT stage 2", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
		{ID: "s3", Title: "spt  stage   2 per the spt-impl-plan contract!", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
		{ID: "s4", Title: "SPT stage 2 per spt-staged-plan.md lines 67-78", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
		// Other families: another declared scope, and another stage. Neither is the work that is waiting.
		{ID: "s5", Title: "SPT stage 2", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ui"},
		{ID: "s6", Title: "SPT stage 3", Role: "worker", Paths: []string{"pax_game_ai.c"}, Scope: "ai"},
	}
	got := c.admitTasks(specs)
	if len(got) != 2 || got[0].ID != "s5" || got[1].ID != "s6" {
		t.Fatalf("admitted = %+v, want only the tasks outside the blocked family", got)
	}
	if open := c.openQuestions(); len(open) != 1 || open[0].ID != blocked.ID {
		t.Fatalf("open questions = %+v, want the one blocked task", open)
	}
}

// The cap counts launches, not task records: three runs of one family, whether one record retried three times or
// three records under different ids and titles, is the whole budget. The fourth launch never reaches an agent.
// "SPT stage 2 per spt-impl-plan contract" is the incident's verified task title; "SPT stage 2" is a HYPOTHETICAL
// reworded fragment of the same family, reconstructed for the test.
func TestTheFamilyCapCountsLaunchesAcrossIdsAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		launches []int // Attempts already spent by the family's earlier task records
		wantCap  bool
	}{
		{"two launches across two ids", []int{1, 1}, false},
		{"three launches across three ids", []int{1, 1, 1}, true},
		{"one record retried to its third launch", []int{3}, true},
		{"two records, one of them retried", []int{1, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r, _ := briefFixture(t, `{"status":"completed","summary":"done"}`)
			for i, n := range tc.launches {
				c.Graph.Add(taskgraph.Task{
					ID: fmt.Sprintf("task-run-%d", i), Title: "SPT stage 2 per spt-impl-plan contract",
					Role: string(agent.Worker), Paths: []string{"pax_game_ai.c"}, Scope: "ai",
					Status: taskgraph.Failed, Attempts: n,
				})
			}
			next := c.Graph.Add(taskgraph.Task{
				ID: "task-next", Title: "SPT stage 2", Role: string(agent.Worker),
				Paths: []string{"pax_game_ai.c"}, Scope: "ai", Model: "mock/t1", MaxAttempts: 3,
			})
			if started := c.dispatch(context.Background(), c.scheduler()); tc.wantCap && started != 0 {
				t.Fatalf("dispatch started %d workers for a family at its cap", started)
			}
			got, _ := c.Graph.Get(next.ID)
			if capped := got.BlockedOn.Unresolved(); capped != tc.wantCap {
				t.Fatalf("capped = %v, want %v (status %s after %d attempts)", capped, tc.wantCap, got.Status, got.Attempts)
			}
			if !tc.wantCap {
				if got.Status != taskgraph.Done {
					t.Fatalf("a launch inside the cap ended %s", got.Status)
				}
				return
			}
			if got.Status != taskgraph.Failed {
				t.Fatalf("a capped launch is %s, want failed", got.Status)
			}
			if !strings.Contains(got.BlockedOn.Question, "limit 3") {
				t.Fatalf("the cap did not say what the limit is: %q", got.BlockedOn.Question)
			}
			if want := taskgraph.FamilyKey(next); got.BlockedOn.Family != want {
				t.Fatalf("family = %q, want %q", got.BlockedOn.Family, want)
			}
			for _, a := range c.Agents.List() {
				if a.TaskID == next.ID {
					t.Fatalf("agent %s was spawned for a capped task", a.ID)
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.tasks) != 0 {
				t.Fatalf("a capped task cost %d model calls", len(r.tasks))
			}
		})
	}
}

// A Primary that says "completed" while a question is unanswered has not finished: the session stays blocked, and
// may only complete once the answer is on the task.
// "SPT stage 2 per spt-impl-plan contract" is the incident's verified task title.
func TestAnUnansweredQuestionKeepsTheSessionFromCompleting(t *testing.T) {
	c, _, _ := briefFixture(t,
		`{"status":"completed","userMessage":"done","tasks":[]}`,
		`{"status":"completed","userMessage":"done","tasks":[]}`)
	primary, _ := c.Agents.Primary()
	task := c.Graph.Add(taskgraph.Task{
		ID: "task-q", Title: "SPT stage 2 per spt-impl-plan contract", Role: string(agent.Worker), Status: taskgraph.Failed,
	})
	if _, err := c.Graph.Update(task.ID, func(x *taskgraph.Task) {
		x.BlockedOn = &taskgraph.BlockedOn{
			Question: "Which contract governs stage 2?", Family: taskgraph.FamilyKey(task), BlockedAt: time.Now().UTC(),
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.loopPrimary(context.Background(), primary, ""); err != nil {
		t.Fatal(err)
	}
	if got := c.Current().Status; got != "blocked" {
		t.Fatalf("session = %q, want blocked while the question is open", got)
	}
	c.applyResolvedBlocks([]ResolvedBlock{{TaskID: task.ID, Answer: "Follow spt-staged-plan.md lines 67-78."}})
	if err := c.loopPrimary(context.Background(), primary, ""); err != nil {
		t.Fatal(err)
	}
	if got := c.Current().Status; got != "completed" {
		t.Fatalf("session = %q, want completed once the question is answered", got)
	}
}

func (c *Coord) primaryID(t *testing.T) string {
	t.Helper()
	p, ok := c.Agents.Primary()
	if !ok {
		t.Fatal("no primary")
	}
	return p.ID
}

// The handoff is the one part of a report that is not compressed on its way to the tasks that build on it.
func TestHandoffReachesDependentsWhole(t *testing.T) {
	c, _, s := briefFixture(t)
	dep := c.Graph.Add(taskgraph.Task{Title: "Add the API", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1"})
	child := c.Graph.Add(taskgraph.Task{Title: "Use the API", Role: string(agent.Worker), Workspace: s.Workspace, Depends: []string{dep.ID}})
	a := c.spawn(dep, agent.Worker)
	handoff := "func (s *Store) Load(ctx context.Context, id string) (Item, error) in store/store.go"
	c.Agents.SetReport(a.ID, agent.Report{Status: agent.Completed, Summary: "Added Load.", Handoff: handoff})

	var got string
	for _, it := range c.workerSeed(child, agent.Worker) {
		if it.TaskID == dep.ID && strings.Contains(it.Content, "Added Load.") {
			got = it.Content
		}
	}
	if !strings.Contains(got, handoff) {
		t.Fatalf("dependent's copy of the report lost the handoff:\n%s", got)
	}
}

func TestReportKeepsHandoffAndQuestionThroughParsing(t *testing.T) {
	rep, err := ParseReport(`{"status":"completed","summary":"Added Load.","handoff":"Load(ctx, id) in store.go"}`)
	if err != nil || rep.Handoff != "Load(ctx, id) in store.go" {
		t.Fatalf("handoff = %q, err %v", rep.Handoff, err)
	}
	// A blocked report that only asks still says something.
	rep, err = ParseReport(`{"status":"blocked","question":"config.Load or settings.Load?"}`)
	if err != nil || rep.Summary != "config.Load or settings.Load?" || rep.Question != "config.Load or settings.Load?" || rep.Status != "blocked" {
		t.Fatalf("blocked report = %+v, err %v", rep, err)
	}
}

func TestTurnConstraintsAreTrimmedDedupedAndCapped(t *testing.T) {
	lines := []string{"  Keep   the API  stable ", "keep the api stable", "", strings.Repeat("x", 500)}
	for i := 0; i < 10; i++ {
		lines = append(lines, "rule "+string(rune('a'+i)))
	}
	got := cleanConstraints(lines)
	if len(got) != 8 {
		t.Fatalf("kept %d constraints, want 8: %v", len(got), got)
	}
	if got[0] != "Keep the API stable" {
		t.Fatalf("first = %q", got[0])
	}
	if n := len([]rune(got[1])); n != 200 {
		t.Fatalf("long constraint kept at %d runes, want 200", n)
	}
}
