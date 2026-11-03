package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

func TestRetryResumeDiffAndConflict(t *testing.T) {
	for _, tc := range []struct {
		mode     workspace.Mode
		conflict bool
	}{{workspace.ModeWorktree, false}, {workspace.ModeWorktree, true}, {workspace.ModeVirtual, false}, {workspace.ModeVirtual, true}} {
		t.Run(string(tc.mode)+"/"+map[bool]string{false: "applied", true: "conflict"}[tc.conflict], func(t *testing.T) {
			dir := gitRepo(t)
			c := New(logging.New(nil, logging.Error))
			ws := workspace.New(dir)
			ws.Configure(workspace.Config{Mode: tc.mode})
			c.SetWorkspaceManager(ws)
			r := newDispatchRuntime(`{"status":"completed","summary":"done"}`)
			c.SetRuntime(r)
			s := c.CreateSession(dir, "retry", ModeBuild)
			c.Settings.AutoVerify, c.Settings.AutoReview = false, false
			c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
			task := c.Graph.Add(taskgraph.Task{Title: "retry", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Attempt: 1})
			var started taskgraph.Task
			var startedAgent agent.Agent
			var escalation map[string]any
			c.SetEmitter(func(name string, payload any) {
				if v, ok := payload.(taskgraph.Task); name == "task.updated" && ok && v.Status == taskgraph.Running {
					started = v
				}
				if v, ok := payload.(agent.Agent); name == "agent.started" && ok {
					startedAgent = v
				}
				if v, ok := payload.(map[string]any); name == "agent.escalated" && ok {
					escalation = v
				}
			})
			a := c.spawn(task, agent.Worker)
			if err := os.WriteFile(filepath.Join(dirOf(t, c, a), "a.txt"), []byte("prior change\n"), 0644); err != nil {
				t.Fatal(err)
			}
			c.rememberFailure(task.ID, "a failed test")
			c.retryTask(task, a)
			c.escalations[task.ID] = "mock/old"
			queued, _ := c.Graph.Get(task.ID)
			if queued.RetryPatch == "" {
				t.Fatal("retry diff not captured")
			}
			if tc.conflict {
				if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("different base\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if got := c.dispatch(context.Background(), c.scheduler()); got != 1 {
				t.Fatalf("started %d", got)
			}
			result, _ := c.Graph.Get(task.ID)
			got, err := agentFile(c, agentOf(result.AgentID), "a.txt")
			if err != nil {
				t.Fatal(err)
			}
			want, note := "prior change\n", "earlier diff was applied"
			if tc.conflict {
				want, note = "different base\n", "earlier diff conflicted"
			}
			if got != want {
				t.Fatalf("file = %q, want %q", got, want)
			}
			if started.RetryPatch != "" || !strings.Contains(started.RetryState, note) {
				t.Fatalf("started retry state: %+v", started)
			}
			if startedAgent.Attempt != started.Attempt || escalation["attempt"] != started.Attempt || escalation["taskId"] != task.ID {
				t.Fatalf("attempts: task=%d agent=%d escalation=%v", started.Attempt, startedAgent.Attempt, escalation)
			}
			if result.RetryPatch != "" || result.RetryState != "" || len(c.failureNote(result)) != 0 {
				t.Fatalf("successful retry retained context: %+v", result)
			}
			if !strings.Contains(r.tasks[0].Prompt, note) {
				t.Fatal("retry prompt omitted carry-over outcome")
			}
			c.rememberFailure(task.ID, strings.Repeat("x", 2000))
			long := c.failureNote(started)[0].Content
			if len([]rune(long)) > 1500 || !strings.Contains(long, note) {
				t.Fatal("long failure note lost its bound or carry-over outcome")
			}
			patch, err := ws.Patch(result.AgentID)
			if err != nil || (!tc.conflict && !strings.Contains(patch, "prior change")) {
				t.Fatalf("retry patch %q, %v", patch, err)
			}
		})
	}
}

func TestRetryBackoffAndDispatchWait(t *testing.T) {
	for _, tc := range []struct {
		attempt     int
		base, upper time.Duration
	}{
		{1, 2 * time.Second, 2400 * time.Millisecond}, {2, 6 * time.Second, 7200 * time.Millisecond}, {3, 15 * time.Second, 18 * time.Second}, {9, 15 * time.Second, 18 * time.Second},
	} {
		if retryDelay(tc.attempt, 0) != tc.base || retryDelay(tc.attempt, 1) != tc.upper {
			t.Fatalf("attempt %d: %v %v", tc.attempt, retryDelay(tc.attempt, 0), retryDelay(tc.attempt, 1))
		}
	}
	for _, msg := range []string{"HTTP 429", "HTTP 503"} {
		if !retryable(errors.New(msg)) {
			t.Fatalf("%s not retryable", msg)
		}
		c, _, s := briefFixture(t)
		task := c.Graph.Add(taskgraph.Task{Title: "retry", Workspace: s.Workspace, Attempt: 1})
		before := time.Now()
		c.retryTaskAfter(task, agent.Agent{}, errors.New(msg))
		got, _ := c.Graph.Get(task.ID)
		if got.NotBefore.Before(before.Add(2*time.Second)) || got.NotBefore.After(time.Now().Add(2400*time.Millisecond)) {
			t.Fatalf("notBefore = %v", got.NotBefore)
		}
	}
	c, _, s := briefFixture(t)
	task := c.Graph.Add(taskgraph.Task{Title: "delayed", Workspace: s.Workspace, NotBefore: time.Now().Add(time.Hour)})
	d := &dispatcher{c: c, ctx: context.Background(), sched: scheduler.New(scheduler.Limits{}), queued: map[string]waiting{}, running: map[string]bool{}}
	d.admit()
	if !d.delayed || d.started != 0 {
		t.Fatalf("delayed=%v started=%d", d.delayed, d.started)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if started := c.dispatch(ctx, c.scheduler()); started != 0 || ctx.Err() == nil {
		t.Fatalf("delayed dispatch started=%d context error=%v", started, ctx.Err())
	}
	if _, err := c.Graph.Update(task.ID, func(x *taskgraph.Task) { x.NotBefore = time.Now().Add(25 * time.Millisecond) }); err != nil {
		t.Fatal(err)
	}
	oldTick := dispatchTick
	dispatchTick = 10 * time.Millisecond
	defer func() { dispatchTick = oldTick }()
	c.SetRuntime(newDispatchRuntime(`{"status":"completed","summary":"done"}`))
	start := time.Now()
	if started := c.dispatch(context.Background(), c.scheduler()); started != 1 || time.Since(start) < 25*time.Millisecond {
		t.Fatalf("delayed retry started=%d after %v", started, time.Since(start))
	}
	c.retryTaskAfter(task, agent.Agent{}, errors.New("stream closed"))
	got, _ := c.Graph.Get(task.ID)
	if !got.NotBefore.IsZero() {
		t.Fatalf("ordinary error delayed until %v", got.NotBefore)
	}
	if retryable(errors.New("HTTP 429 insufficient quota")) {
		t.Fatal("quota exhaustion must not be retried")
	}
}

func TestDelayedRetryDoesNotBlockReadyTask(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{}, agent.Reviewer, "delayed", "ready")
	_, err := c.Graph.Update("delayed", func(x *taskgraph.Task) { x.NotBefore = time.Now().Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- c.dispatch(ctx, c.scheduler()) }()
	defer func() { cancel(); <-done }()
	select {
	case title := <-r.begun:
		if title != "ready" {
			t.Fatalf("started %q instead of ready task", title)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ready task did not start while another task was delayed")
	}
	r.release("ready")
	waitFor(t, "ready task completion", func() bool {
		got, _ := c.Graph.Get("ready")
		return got.Status == taskgraph.Done
	})
}

func TestManualRetryClearsCarryOver(t *testing.T) {
	c, _, s := briefFixture(t)
	task := c.Graph.Add(taskgraph.Task{Title: "failed", Workspace: s.Workspace, Status: taskgraph.Failed, Attempt: 1, RetryPatch: "stale patch", RetryState: "stale state", NotBefore: time.Now().Add(time.Hour)})
	got, err := c.Graph.Retry(task.ID)
	if err != nil || got.Status != taskgraph.Queued || got.RetryPatch != "" || got.RetryState != "" || !got.NotBefore.Equal(task.NotBefore) {
		t.Fatalf("manual retry = %+v, %v", got, err)
	}
}

func TestRetryApplyFailureAnnouncesEscalation(t *testing.T) {
	c, _, s := briefFixture(t)
	task := c.Graph.Add(taskgraph.Task{Title: "retry", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/new", Attempt: 1, RetryPatch: "patch"})
	c.escalations[task.ID] = "mock/old"
	var event map[string]any
	c.SetEmitter(func(name string, payload any) {
		if name == "agent.escalated" {
			event, _ = payload.(map[string]any)
		}
	})
	if got := c.dispatch(context.Background(), c.scheduler()); got != 0 {
		t.Fatalf("started %d workers", got)
	}
	if event["taskId"] != task.ID || event["from"] != "mock/old" || event["to"] != "mock/new" || event["attempt"] != 2 {
		t.Fatalf("escalation event = %#v", event)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Failed || got.RetryPatch != "patch" {
		t.Fatalf("failed carry-over = %+v", got)
	}
}

func TestQueuedRetryRecutCarriesPatchOnce(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	ws := workspace.New(dir)
	ws.Configure(workspace.Config{Mode: workspace.ModeWorktree})
	c.SetWorkspaceManager(ws)
	r := newGatedRuntime()
	c.SetRuntime(r)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	s := c.CreateSession(dir, "retry", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	task := c.Graph.Add(taskgraph.Task{ID: "retry", Title: "retry", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Attempt: 1})
	a := c.spawn(task, agent.Worker)
	if err := os.WriteFile(filepath.Join(dirOf(t, c, a), "a.txt"), []byte("prior change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c.retryTask(task, a)
	queued, _ := c.Graph.Get(task.ID)
	if queued.RetryPatch == "" {
		t.Fatal("retry diff not captured")
	}
	waitingAgent := c.spawnSoft(queued, agent.Worker, nil)
	d := &dispatcher{c: c, ctx: context.Background(), sched: scheduler.New(scheduler.Limits{}), wake: make(chan struct{}, 1), queued: map[string]waiting{task.ID: {agent: waitingAgent}}, running: map[string]bool{}, waking: map[string]bool{}}
	d.admit()
	defer func() { r.release("retry"); d.wg.Wait() }()
	select {
	case <-r.begun:
	case <-time.After(3 * time.Second):
		t.Fatal("queued retry did not start")
	}
	got, _ := c.Graph.Get(task.ID)
	if got.RetryPatch != "" || !strings.Contains(got.RetryState, "earlier diff was applied") {
		t.Fatalf("queued retry state = %+v", got)
	}
	contents, err := agentFile(c, agentOf(got.AgentID), "a.txt")
	if err != nil || contents != "prior change\n" {
		t.Fatalf("recut retry file = %q, %v", contents, err)
	}
}
