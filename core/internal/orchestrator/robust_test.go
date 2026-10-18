package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
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
