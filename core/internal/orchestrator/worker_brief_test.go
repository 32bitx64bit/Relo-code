package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
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

// Every role keeps its scratch work under the temp dir Relo owns and cleans up, never a hard-coded /tmp path.
func TestEveryRoleKeepsScratchFilesUnderTMPDIR(t *testing.T) {
	roles := []agent.Role{agent.Primary, agent.Head, agent.Worker, agent.Probe, agent.Reviewer, agent.Architect, agent.Debug, agent.Test, agent.Integration}
	for _, role := range roles {
		promptText, _ := canvasExposure(t, role)
		if !strings.Contains(promptText, "$TMPDIR") {
			t.Fatalf("%s prompt does not name $TMPDIR:\n%s", role, promptText)
		}
		if !strings.Contains(promptText, "never hard-coded /tmp") {
			t.Fatalf("%s prompt does not ban hard-coded /tmp:\n%s", role, promptText)
		}
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
	if got.Status != taskgraph.Asking {
		t.Fatalf("task = %s; it must wait for an answer", got.Status)
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
	if got.Status != taskgraph.Asking {
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
	if len(got) != 3 || got[0].ID != "s2" || got[1].ID != "s5" || got[2].ID != "s6" {
		t.Fatalf("admitted = %+v, want only the tasks outside the blocked family", got)
	}
	if open := c.openQuestions(); len(open) != 1 || open[0].ID != blocked.ID {
		t.Fatalf("open questions = %+v, want the one blocked task", open)
	}
}

// There is no run limit: prior launches of one family, whether one record retried three times or three records under
// different ids and titles, never park the next launch. It spawns, runs and finishes.
// "SPT stage 2 per spt-impl-plan contract" is the incident's verified task title; "SPT stage 2" is a HYPOTHETICAL
// reworded fragment of the same family, reconstructed for the test.
func TestTheFamilyLaunchesWithoutARunLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		launches []int // Attempts already spent by the family's earlier task records
	}{
		{"two launches across two ids", []int{1, 1}},
		{"three launches across three ids", []int{1, 1, 1}},
		{"one record retried to its third launch", []int{3}},
		{"two records, one of them retried", []int{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := briefFixture(t, `{"status":"completed","summary":"done"}`)
			for i, n := range tc.launches {
				c.Graph.Add(taskgraph.Task{
					ID: fmt.Sprintf("task-run-%d", i), Title: "SPT stage 2 per spt-impl-plan contract",
					Role: string(agent.Worker), Paths: []string{"pax_game_ai.c"}, Scope: "ai",
					Status: taskgraph.Failed, Attempt: n,
				})
			}
			next := c.Graph.Add(taskgraph.Task{
				ID: "task-next", Title: "SPT stage 2 per spt-impl-plan contract", Role: string(agent.Worker),
				Paths: []string{"pax_game_ai.c"}, Scope: "ai", Model: "mock/t1", MaxAttempts: 3,
			})
			if started := c.dispatch(context.Background(), c.scheduler()); started != 1 {
				t.Fatalf("dispatch started %d workers, want 1", started)
			}
			got, _ := c.Graph.Get(next.ID)
			if got.Status != taskgraph.Done || got.BlockedOn != nil {
				t.Fatalf("launch ended %s with block %+v, want done without a question", got.Status, got.BlockedOn)
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
	if err := c.AnswerQuestion(task.ID, "Follow spt-staged-plan.md lines 67-78."); err != nil {
		t.Fatal(err)
	}
	resumed, _ := c.Graph.Get(task.ID)
	if resumed.Status != taskgraph.Queued || c.Current().Status != "active" {
		t.Fatalf("task = %s, session = %s; answer must queue the original task", resumed.Status, c.Current().Status)
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

// A probe reads only what its dependencies concluded; a report from an unrelated task stays out.
func TestProbeBriefTakesOnlyItsDependencyReports(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	dep := c.Graph.Add(taskgraph.Task{ID: "task-dep", Title: "dep"})
	probe := c.Graph.Add(taskgraph.Task{ID: "task-probe", Title: "probe", Depends: []string{dep.ID}})
	depAgent := c.Agents.Create(agent.Agent{Role: agent.Worker, Title: "dep work", TaskID: dep.ID})
	otherAgent := c.Agents.Create(agent.Agent{Role: agent.Worker, Title: "other work", TaskID: "task-other"})
	c.Agents.SetReport(depAgent.ID, agent.Report{Status: agent.Completed, Summary: "dep summary", Files: []string{"pkg/dep.go"}})
	c.Agents.SetReport(otherAgent.ID, agent.Report{Status: agent.Completed, Summary: "other summary", Files: []string{"pkg/other.go"}})

	items := seedTitles(c.workerSeed(probe, agent.Probe))
	if rep, ok := items["dep work"]; !ok || rep.TaskID != dep.ID {
		t.Fatalf("probe lost its dependency report: %+v", rep)
	}
	if _, ok := items["other work"]; ok {
		t.Fatal("probe brief carries a report from a non-dependency task")
	}
}

// An architect pack holds the findings related to its task, at most architectFindingCap, the dep's first.
func TestArchitectBriefHoldsRelatedFindingsCappedAt40(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	dep := c.Graph.Add(taskgraph.Task{ID: "task-dep", Title: "dep"})
	task := c.Graph.Add(taskgraph.Task{ID: "task-main", Title: "Rework parser", Description: "Change pkg/parse.go.", Depends: []string{dep.ID}})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-dep", TaskID: dep.ID, Summary: "dep finding", Confidence: 0.1})
	for i := 0; i < architectFindingCap+5; i++ {
		c.Engine.PromoteFinding(ctxeng.Finding{ID: fmt.Sprintf("f-rel-%02d", i), TaskID: "task-x", Summary: fmt.Sprintf("related %02d", i), Files: []string{"pkg/parse.go"}, Confidence: 0.5})
	}
	for i := 0; i < 5; i++ {
		c.Engine.PromoteFinding(ctxeng.Finding{ID: fmt.Sprintf("f-off-%d", i), TaskID: "task-y", Summary: fmt.Sprintf("unrelated %d", i), Files: []string{"net/http/h.go"}, Verified: true, Confidence: 0.9})
	}

	var findings []string
	for _, it := range c.workerSeed(task, agent.Architect) {
		if it.Class == ctxeng.ClassFinding {
			findings = append(findings, it.Title)
		}
	}
	if len(findings) != architectFindingCap {
		t.Fatalf("architect findings = %d, want %d", len(findings), architectFindingCap)
	}
	if findings[0] != "dep finding" {
		t.Errorf("dependency finding not ranked first: %v", findings[:2])
	}
	for _, title := range findings {
		if strings.HasPrefix(title, "unrelated") {
			t.Fatalf("architect pack holds an unrelated finding: %q", title)
		}
	}
}

// A task with no dependencies gets no dependency reports, even when other tasks have finished reports.
func TestNoDepTaskGetsNoDependencyReports(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	task := c.Graph.Add(taskgraph.Task{ID: "task-solo", Title: "solo"})
	other := c.Agents.Create(agent.Agent{Role: agent.Worker, Title: "other work", TaskID: "task-other"})
	c.Agents.SetReport(other.ID, agent.Report{Status: agent.Completed, Summary: "other summary"})

	for _, role := range []agent.Role{agent.Worker, agent.Probe} {
		for _, it := range c.workerSeed(task, role) {
			if it.Class == ctxeng.ClassReport {
				t.Fatalf("%s with no deps got a report: %+v", role, it)
			}
		}
	}
}
