package orchestrator

import (
	"context"
	"strings"
	"testing"

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
