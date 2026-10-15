package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
)

type blockingRuntime struct {
	started    chan struct{}
	canceled   chan struct{}
	closed     chan struct{}
	once       sync.Once
	cancelOnce sync.Once
	closeOnce  sync.Once
}

func newBlockingRuntime() *blockingRuntime {
	return &blockingRuntime{started: make(chan struct{}), canceled: make(chan struct{}), closed: make(chan struct{})}
}
func (r *blockingRuntime) StartSession(context.Context, rt.SessionConfig) (rt.SessionID, error) {
	return "blocking", nil
}
func (r *blockingRuntime) SendTask(context.Context, rt.SessionID, rt.AgentTask) error {
	r.once.Do(func() { close(r.started) })
	return nil
}
func (r *blockingRuntime) Events(ctx context.Context, _ rt.SessionID) (<-chan rt.Event, error) {
	ch := make(chan rt.Event)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}
func (r *blockingRuntime) Cancel(context.Context, rt.SessionID) error {
	r.cancelOnce.Do(func() { close(r.canceled) })
	return nil
}
func (r *blockingRuntime) CloseSession(context.Context, rt.SessionID) error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}
func (r *blockingRuntime) Models(context.Context) ([]rt.Model, error)       { return nil, nil }
func (r *blockingRuntime) Providers(context.Context) ([]rt.Provider, error) { return nil, nil }
func (r *blockingRuntime) Health(context.Context) rt.Health                 { return rt.Health{Healthy: true} }
func (r *blockingRuntime) Close() error                                     { return nil }

func TestPauseResumeRestartsDrive(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Runtime = rt.NewMock()
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	s := c.CreateSession(t.TempDir(), "pause test", ModeBuild)
	p := c.Agents.Create(agent.Agent{Role: agent.Primary, Status: agent.Queued, Workspace: s.Workspace})
	c.Graph.Add(taskgraph.Task{ID: "one", Role: string(agent.Probe), Workspace: s.Workspace})
	c.Pause()
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	select {
	case <-done:
		t.Fatal("drive returned while paused")
	case <-time.After(15 * time.Millisecond):
	}
	c.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not restart")
	}
	if _, ok := c.Agents.Get(p.ID); !ok {
		t.Fatal("primary disappeared")
	}
}

func TestNextAttemptBoundAndReviewOnce(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Sched = scheduler.New(scheduler.Limits{MaxRetry: 4})
	c.Configure(DefaultSettings(), c.Sched, routing.Config{Tier2: "mock/t2", Tier1: "mock/t1", Primary: "mock/p"})
	c.CreateSession(t.TempDir(), "review", ModeBuild)
	p := c.Agents.Create(agent.Agent{Role: agent.Primary})
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, ParentID: p.ID})
	task := c.Graph.Add(taskgraph.Task{ID: "bounded", Title: "change", Role: string(agent.Worker), Attempt: 1})
	for _, tt := range []struct {
		attempt int
		model   string
		retry   bool
	}{
		{1, "mock/t1", true},
		{2, "mock/p", true},
		{3, "mock/p", true},
		{4, "mock/p", true},
		{5, "mock/p", false},
	} {
		task.Attempt = tt.attempt
		model, retry := c.nextAttempt(task, agent.Agent{Model: "mock/t1"})
		if model != tt.model || retry != tt.retry {
			t.Fatalf("attempt %d got (%q,%t), want (%q,%t)", tt.attempt, model, retry, tt.model, tt.retry)
		}
	}
	c.scheduleReview(task, a)
	c.scheduleReview(task, a)
	reviews := 0
	for _, got := range c.Graph.List() {
		if got.Role == string(agent.Reviewer) {
			reviews++
		}
	}
	if reviews != 1 {
		t.Fatalf("got %d reviewer tasks", reviews)
	}
}

func TestProbeTestReviewerAvoidPrimaryModel(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Configure(DefaultSettings(), scheduler.New(scheduler.Limits{}), routing.Config{Primary: "p", Tier1: "t1"})
	for _, role := range []agent.Role{agent.Probe, agent.Test, agent.Reviewer} {
		if got := c.modelForDifficulty(role, "expert"); got == "p" {
			t.Fatalf("%s routed to primary model", role)
		}
	}
	c.Configure(DefaultSettings(), scheduler.New(scheduler.Limits{}), routing.Config{Primary: "p"})
	if got := c.modelForDifficulty(agent.Reviewer, "easy"); got != "p" {
		t.Fatalf("single-model reviewer route = %q", got)
	}
}

func TestClampReport(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Settings.ReportMaxChars = 30
	rep := c.clampReport(RawReport{
		Summary:  strings.Repeat("x", 40),
		Findings: []string{"same", "same", strings.Repeat("x", 250), "3", "4", "5", "6", "7", "8", "9"},
		Risks:    []string{"risk", "risk"},
	})
	if len([]rune(rep.Summary)) != 30 || !strings.HasSuffix(rep.Summary, "...[clipped 25]") {
		t.Fatalf("summary cap/marker = %q", rep.Summary)
	}
	if len(rep.Findings) != 8 || rep.Findings[0] != "same" || rep.Findings[1] == "same" || len([]rune(rep.Findings[1])) != 240 {
		t.Fatalf("findings clamp = %#v", rep.Findings)
	}
	if len(rep.Risks) != 1 {
		t.Fatalf("risks dedupe = %#v", rep.Risks)
	}
}

func TestSchemaContractHasStableKeys(t *testing.T) {
	a := schemaContract(map[string]any{"z": "last", "a": "first"})
	b := schemaContract(map[string]any{"a": "first", "z": "last"})
	if a != b || strings.Index(a, `"a"`) > strings.Index(a, `"z"`) {
		t.Fatalf("schema contract unstable: %s / %s", a, b)
	}
}

type dispatchRuntime struct {
	mu        sync.Mutex
	responses []string
	tasks     []rt.AgentTask
	starts    int
	closes    int
	events    chan rt.Event
}

func newDispatchRuntime(responses ...string) *dispatchRuntime {
	return &dispatchRuntime{responses: responses, events: make(chan rt.Event, 16)}
}
func (r *dispatchRuntime) StartSession(context.Context, rt.SessionConfig) (rt.SessionID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	return "dispatch", nil
}
func (r *dispatchRuntime) SendTask(_ context.Context, _ rt.SessionID, task rt.AgentTask) error {
	r.mu.Lock()
	index := len(r.tasks)
	r.tasks = append(r.tasks, task)
	if index >= len(r.responses) {
		r.mu.Unlock()
		return fmt.Errorf("unexpected task %d: only %d responses were scripted", index+1, len(r.responses))
	}
	response := r.responses[index]
	r.mu.Unlock()
	r.events <- rt.Event{Type: "session.idle", Done: true, JSON: response, Usage: &rt.Usage{Input: 10, Output: 4, Reasoning: 2, CacheRead: 8, CacheWrite: 3}}
	return nil
}
func (r *dispatchRuntime) Events(context.Context, rt.SessionID) (<-chan rt.Event, error) {
	return r.events, nil
}
func (r *dispatchRuntime) Cancel(context.Context, rt.SessionID) error { return nil }
func (r *dispatchRuntime) CloseSession(context.Context, rt.SessionID) error {
	r.mu.Lock()
	r.closes++
	r.mu.Unlock()
	return nil
}
func (r *dispatchRuntime) Models(context.Context) ([]rt.Model, error)       { return nil, nil }
func (r *dispatchRuntime) Providers(context.Context) ([]rt.Provider, error) { return nil, nil }
func (r *dispatchRuntime) Health(context.Context) rt.Health                 { return rt.Health{Healthy: true} }
func (r *dispatchRuntime) Close() error                                     { return nil }

func TestPrimaryRepairUsesOneSameSessionRetry(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"working"}`, `{"status":"working","userMessage":"fixed","tasks":[]}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "repair", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	turn, err := c.primaryTurn(context.Background(), a, prompt.Parts{Static: "stable", Dynamic: "context"}, "plan")
	if err != nil || turn.UserMessage != "fixed" {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.starts != 1 || len(r.tasks) != 2 || !strings.Contains(r.tasks[1].Prompt, "Previous output failed validation") {
		t.Fatalf("session starts=%d tasks=%+v", r.starts, r.tasks)
	}
}

func TestPrimaryInvalidRepairAddsVisibleChat(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime(`{}`, `{}`))
	s := c.CreateSession(t.TempDir(), "invalid turn", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	_, err := c.primaryTurn(context.Background(), a, prompt.Parts{}, "plan")
	if err == nil {
		t.Fatal("invalid turn accepted")
	}
	messages := c.Current().Messages
	if len(messages) != 1 || messages[0].Role != "error" || !strings.Contains(messages[0].Text, "could not be read") {
		t.Fatalf("visible failure chat = %+v", messages)
	}
}

func TestDeltaDispatchAndPrefixWarning(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, want string
		warn                      bool
	}{
		{"suffix", "L2 one", "L2 one\nL2 two", "L2 two", false},
		{"broken prefix", "old prefix", "new full block", "new full block", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			c := New(logging.New(&logs, logging.Warn))
			r := newDispatchRuntime(`{}`, `{}`)
			c.SetRuntime(r)
			s := c.CreateSession(t.TempDir(), "delta", ModeBuild)
			a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace, Model: "mock/t1"})
			_, err := c.runAgent(context.Background(), a, prompt.Parts{Static: "stable", Dynamic: tc.first}, "first", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.runAgent(context.Background(), a, prompt.Parts{Static: "stable", Dynamic: tc.second}, "second", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			if r.starts != 1 || len(r.tasks) != 2 || !strings.Contains(r.tasks[1].Prompt, tc.want) {
				t.Fatalf("starts=%d prompts=%q", r.starts, []string{r.tasks[0].Prompt, r.tasks[1].Prompt})
			}
			if tc.name == "suffix" && strings.Contains(r.tasks[1].Prompt, "L2 one") {
				t.Fatalf("resent old dynamic prefix: %q", r.tasks[1].Prompt)
			}
			r.mu.Unlock()
			if got := strings.Contains(logs.String(), "cache: dynamic prefix changed within generation"); got != tc.warn {
				t.Fatalf("warn=%t logs=%s", got, logs.String())
			}
			c.closeAgentSession(a.ID)
		})
	}
}

func TestUsageLedgerAndCacheUpdatedShape(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "usage", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace, Model: "mock/t1"})
	var payload map[string]any
	c.SetEmitter(func(name string, value any) {
		if name == "cache.updated" {
			payload = value.(map[string]any)
		}
	})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{Static: "stable"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	usage := c.Current().Usage
	if usage["input"] != 10 || usage["output"] != 4 || usage["reasoning"] != 2 || usage["cacheRead"] != 8 || usage["cacheWrite"] != 3 {
		t.Fatalf("session usage = %#v", usage)
	}
	gotSession, ok := payload["session"].(map[string]any)
	if !ok || len(gotSession) != 5 || gotSession["input"] != 10 || gotSession["cacheRead"] != 8 {
		t.Fatalf("session cache payload = %#v", payload)
	}
	rows, ok := payload["agents"].([]map[string]any)
	if !ok || len(rows) != 1 || rows[0]["agentId"] != a.ID || rows[0]["generation"] != 0 || rows[0]["model"] != a.Model {
		t.Fatalf("agent cache payload = %#v", payload["agents"])
	}
	if got := c.CacheSnapshot().Session.HitRate; got != metricsHitRate(8, 10, 3) {
		t.Fatalf("cache hit rate=%f", got)
	}
}

func TestCacheUsagePerCompletedTurn(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{}`, `{}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "usage twice", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace, Model: "mock/t1"})
	for i := 0; i < 2; i++ {
		if _, err := c.runAgent(context.Background(), a, prompt.Parts{Static: "stable"}, "turn", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.CacheSnapshot().Session.Usage; got.Input != 20 || got.Output != 8 || got.CacheRead != 16 || got.CacheWrite != 6 {
		t.Fatalf("cache usage = %+v", got)
	}
}

func TestStrictWorkerReportRetriesOnceThenFailsWithoutChanges(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"completed"}`, `{"status":"completed"}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "invalid report", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	c.Sched = scheduler.New(scheduler.Limits{MaxRetry: 1})
	task := c.Graph.Add(taskgraph.Task{ID: "invalid-report", Title: "task", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Attempt: 2, MaxAttempts: 1})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Failed || got.Attempt != 2 || !strings.Contains(got.Report.(agent.Report).Summary, "Invalid structured worker report") {
		t.Fatalf("invalid report task = %+v", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 2 || !strings.Contains(r.tasks[1].Prompt, "Previous report failed validation") || r.starts != 1 {
		t.Fatalf("worker repair sessions=%d tasks=%+v", r.starts, r.tasks)
	}
}

func metricsHitRate(read, input, write int) float64 {
	return float64(read) / float64(read+input+write)
}

func TestStopAllCancelsClosesRuntimeAndWorkers(t *testing.T) {
	before := goruntime.NumGoroutine()
	c := New(logging.New(nil, logging.Error))
	br := newBlockingRuntime()
	c.SetRuntime(br)
	s := c.CreateSession(t.TempDir(), "stop test", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Title: "Primary", Workspace: s.Workspace, Model: "mock/primary"})
	c.Graph.Add(taskgraph.Task{ID: "wait", Title: "wait", Role: string(agent.Probe), Workspace: s.Workspace})
	done := make(chan error, 1)
	go func() {
		primary, _ := c.Agents.Primary()
		c.mu.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		c.runCancel, c.sessionCtx = cancel, ctx
		c.runWG.Add(1)
		c.mu.Unlock()
		defer c.runWG.Done()
		defer cancel()
		_, err := c.runAgent(ctx, agent.Agent{ID: primary.ID, Role: agent.Primary, Workspace: s.Workspace, Model: "mock/primary"}, prompt.Parts{}, "test", nil)
		done <- err
	}()
	select {
	case <-br.started:
	case <-time.After(time.Second):
		t.Fatal("runtime task not started")
	}
	c.StopAll(context.Background())
	select {
	case <-br.canceled:
	case <-time.After(time.Second):
		t.Fatal("runtime not canceled")
	}
	select {
	case <-br.closed:
	case <-time.After(time.Second):
		t.Fatal("runtime not closed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker still running")
	}
	if got := goruntime.NumGoroutine(); got > before+3 {
		t.Fatalf("goroutines before=%d after=%d", before, got)
	}
}

func TestVerificationFailureFailsAndBoundsRetry(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "verify", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = true, false
	c.Sched = schedulerForRetry(1)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	if err := os.WriteFile(filepath.Join(s.Workspace, "go.mod"), []byte("this is invalid go.mod"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Workspace, "Makefile"), []byte("test:\n\tfalse\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	task := c.Graph.Add(taskgraph.Task{ID: "verify-task", Role: string(agent.Worker), Workspace: s.Workspace, Attempt: 1})
	passed, failure := c.runVerify(context.Background(), a)
	if passed || failure == "" {
		t.Fatalf("broken project passed its checks: %q", failure)
	}
	results := c.Current().Verify
	// The Makefile's test target is the project's own definition of "the tests", so it replaces go test.
	if len(results) != 1 || results[0].Command != "make test" || results[0].Status != "failed" {
		t.Fatalf("verify did not run the project's test target: %+v", results)
	}
	// Checks that still fail after the repair rounds end the task as failed but are not retried: the work is intact
	// in the worktree and a fresh attempt would only redo it.
	c.acceptReport(task, a, RawReport{Status: "completed", Summary: "changed files"}, verifyOutcome{ran: true, failure: failure, repairs: maxRepairRounds})
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Failed {
		t.Fatalf("failing checks did not fail the task: %+v", got)
	}
	if again, _ := c.Graph.Get(task.ID); again.Attempt != 1 {
		t.Fatalf("failing checks started another attempt: %+v", again)
	}
	c.Graph.Update(task.ID, func(x *taskgraph.Task) { x.Attempt = 2; x.Status = taskgraph.Failed })
	c.retryTask(taskgraph.Task{ID: task.ID, Attempt: 2}, a)
	got, _ = c.Graph.Get(task.ID)
	if got.Status != taskgraph.Failed {
		t.Fatalf("retry exceeded cap: %+v", got)
	}
}

func TestPermissionWaitCancelsAndRepeatedReplyIsSafe(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "permission", ModeBuild)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		c.askPerm(ctx, agent.Agent{ID: "a"}, rt.Event{Session: "s", Permission: map[string]any{"id": "p1", "title": "network", "pattern": "curl https://example.com"}})
	}()
	<-started
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		_, exists := c.pending["p1"]
		c.mu.Unlock()
		if exists {
			break
		}
		select {
		case <-deadline:
			t.Fatal("permission not registered")
		default:
			goruntime.Gosched()
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("permission wait ignored cancellation")
	}
	deadline = time.After(time.Second)
	for {
		c.mu.Lock()
		_, exists := c.pending["p1"]
		c.mu.Unlock()
		if !exists {
			break
		}
		select {
		case <-deadline:
			t.Fatal("pending permission leaked")
		default:
			goruntime.Gosched()
		}
	}
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		c.askPerm(context.Background(), agent.Agent{ID: "a"}, rt.Event{Session: "s", Permission: map[string]any{"id": "p2", "title": "network", "pattern": "curl https://example.com"}})
	}()
	deadline = time.After(time.Second)
	for {
		c.mu.Lock()
		_, exists := c.pending["p2"]
		c.mu.Unlock()
		if exists {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second permission not registered")
		default:
			goruntime.Gosched()
		}
	}
	c.RespondPermission("p2", "once")
	c.RespondPermission("p2", "once")
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("repeated permission replies blocked")
	}
}

func schedulerForRetry(max int) *scheduler.Scheduler {
	return scheduler.New(scheduler.Limits{MaxRetry: max})
}

// failRuntime answers every task with a terminal provider failure: Done and Error on the same event,
// exactly what the OpenCode adapter emits for session.execution.failed.
type failRuntime struct {
	dispatchRuntime
	message string
}

func (r *failRuntime) SendTask(_ context.Context, _ rt.SessionID, task rt.AgentTask) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()
	r.events <- rt.Event{Type: "session.execution.failed", Done: true, Error: r.message}
	return nil
}

func TestPrimaryProviderErrorIsReportedNotAsInvalidJSON(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := &failRuntime{dispatchRuntime: *newDispatchRuntime(), message: "OpenCode's free tier can only be used from within OpenCode."}
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "provider error", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	_, err := c.primaryTurn(context.Background(), a, prompt.Parts{Static: "s", Dynamic: "d"}, "plan")
	if err == nil || !strings.Contains(err.Error(), "free tier") {
		t.Fatalf("provider error lost: %v", err)
	}
	if strings.Contains(err.Error(), "JSON") {
		t.Fatalf("provider failure misreported as a JSON problem: %v", err)
	}
	r.mu.Lock()
	n := len(r.tasks)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("a provider failure must not trigger the JSON repair prompt; tasks sent = %d", n)
	}
}

func TestSubmitRefusesUnavailablePrimaryWithClearChat(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime())
	c.CreateSession(t.TempDir(), "", ModeBuild)
	c.UpdateSettings(func(s *Settings) {
		s.PrimaryModel = "opencode/gone"
		s.Models, s.ModelsKnown = map[string]rt.Model{"acme/big": {ID: "big", Provider: "acme"}}, true
	})
	err := c.Submit(context.Background(), "do it", nil)
	if err == nil || !strings.Contains(err.Error(), "opencode/gone") {
		t.Fatalf("err = %v", err)
	}
	msgs := c.Current().Messages
	if len(msgs) != 1 || msgs[0].Role != "system" || !strings.Contains(msgs[0].Text, "not available") {
		t.Fatalf("user must see why in chat: %+v", msgs)
	}
	if c.Busy() {
		t.Fatal("a refused submit must not start an agent")
	}

	c.UpdateSettings(func(s *Settings) { s.PrimaryModel = "" })
	if err := c.Submit(context.Background(), "again", nil); err == nil || !strings.Contains(err.Error(), "No Primary model") {
		t.Fatalf("empty primary: %v", err)
	}

	c.UpdateSettings(func(s *Settings) { s.PrimaryModel, s.Models = "acme/big", map[string]rt.Model{} })
	if err := c.Submit(context.Background(), "empty", nil); err == nil || !strings.Contains(err.Error(), "No models are available") {
		t.Fatalf("a loaded but empty catalog means no provider: %v", err)
	}

	c.UpdateSettings(func(s *Settings) { s.Models, s.ModelsKnown = nil, false })
	if msg := c.settings().modelProblem(); msg != "" {
		t.Fatalf("a catalog that was never loaded must not block: %q", msg)
	}
}
