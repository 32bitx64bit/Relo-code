package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/verification"
)

// countingRunner counts the project checks a batch runs, through verification.RunObserver, so the tests need no hook
// on Coord. The observer is package-wide, so an install is undone when its test ends.
type countingRunner struct {
	mu   sync.Mutex
	dirs []string
	cmds []string
}

func newCountingRunner(t *testing.T) *countingRunner {
	t.Helper()
	r := &countingRunner{}
	verification.RunObserver = func(res verification.Result) {
		r.mu.Lock()
		r.dirs = append(r.dirs, res.Dir)
		r.cmds = append(r.cmds, res.Command)
		r.mu.Unlock()
	}
	t.Cleanup(func() { verification.RunObserver = nil })
	return r
}

func (r *countingRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dirs)
}

func (r *countingRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

// batchFixture is a session with a project whose checks pass, and a change the scripted workers did not really make.
// A real change would need a git worktree; changesFor reads its file list, and the tests say it saw one.
func batchFixture(t *testing.T, checks string, responses ...string) (*Coord, *dispatchRuntime, *Session, *countingRunner) {
	t.Helper()
	c, r, s := briefFixture(t, responses...)
	if err := os.WriteFile(filepath.Join(s.Workspace, "Makefile"), []byte(checks), 0600); err != nil {
		t.Fatal(err)
	}
	c.Settings.AutoVerify, c.Settings.AutoReview = true, false
	changes := c.changeSeenForTest
	c.changeSeenForTest = func(agent.Agent) bool { return true }
	t.Cleanup(func() { c.changeSeenForTest = changes })
	return c, r, s, newCountingRunner(t)
}

func batchTask(c *Coord, s *Session, title, path string) taskgraph.Task {
	return c.Graph.Add(taskgraph.Task{
		Title: title, Description: "edit " + path, Acceptance: []string{path + " shows the new value"},
		Paths: []string{path}, Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "easy",
	})
}

// A batch's checks run once, after every worker of the batch is in, not after each one.
func TestBatchChecksRunOnceAtTheEnd(t *testing.T) {
	c, _, s, runner := batchFixture(t, "test:\n\ttrue\n", `{"status":"completed","summary":"done one"}`, `{"status":"completed","summary":"done two"}`)
	first, second := batchTask(c, s, "First", "a.txt"), batchTask(c, s, "Second", "b.txt")
	a1, a2 := c.spawn(first, agent.Worker), c.spawn(second, agent.Worker)

	c.batching.Store(1)
	defer c.batching.Store(0)
	for _, pair := range []struct {
		task  taskgraph.Task
		agent agent.Agent
	}{{first, a1}, {second, a2}} {
		if err := c.execWorker(context.Background(), pair.task, pair.agent); err != nil {
			t.Fatal(err)
		}
	}
	if got := runner.calls(); got != 0 {
		t.Fatalf("checks ran %d times inside a running batch, want 0", got)
	}
	c.mu.Lock()
	held := len(c.batchReports)
	c.mu.Unlock()
	if held != 2 {
		t.Fatalf("held reports = %d, want the 2 workers of the batch", held)
	}
	c.batching.Store(0)
	c.finishBatch(context.Background(), c.verifier())
	if got := runner.calls(); got != 1 {
		t.Fatalf("checks ran %d times at the end of the batch, want once", got)
	}
	if got := runner.commands(); len(got) != 1 || got[0] != "make test" {
		t.Fatalf("batch ran %v; a Makefile target is the full check and must stay whole", got)
	}
	c.mu.Lock()
	left := len(c.batchReports)
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("batch reports left = %d", left)
	}
	for _, pair := range []struct {
		task  taskgraph.Task
		agent agent.Agent
	}{{first, a1}, {second, a2}} {
		got, _ := c.Graph.Get(pair.task.ID)
		rep, ok := got.Report.(agent.Report)
		if !ok || len(rep.Summary) == 0 {
			t.Fatalf("%s has no report after the batch checks: %+v", pair.task.Title, got.Report)
		}
		if ag, _ := c.Agents.Get(pair.agent.ID); ag.Status != agent.Completed {
			t.Fatalf("%s = %s %q; the batch checks passed", pair.task.Title, ag.Status, ag.Activity)
		}
	}
}

// When the batch's check fails, the worker is sent back to fix it and the check runs again: the repair is kept, and
// the worker whose changes the output names is the one asked.
func TestBatchCheckFailureIsSentBackAndRerun(t *testing.T) {
	c, _, s, runner := batchFixture(t, "test:\n\tfalse\n", `{"status":"completed","summary":"done"}`, `{"status":"completed","summary":"fixed the cause"}`)
	task := batchTask(c, s, "Break it", "a.txt")
	a := c.spawn(task, agent.Worker)

	c.batching.Store(1)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	c.batching.Store(0)
	c.finishBatch(context.Background(), c.verifier())

	if got := runner.calls(); got != 2 {
		t.Fatalf("checks ran %d times, want once and once after the repair", got)
	}
	rep, ok := c.reportOf(t, task.ID)
	if !ok || rep.Summary != "fixed the cause" {
		t.Fatalf("report = %+v, want the repair's summary", rep)
	}
	// A failing repair the worker cannot place ends the loop with the failure on the report, not a silent pass.
	if len(rep.Risks) == 0 || !strings.Contains(rep.Risks[0], "Checks still failing") {
		t.Fatalf("the failure did not reach the report: %+v", rep.Risks)
	}
}

// A later batch's own changes are still checked once at its end, even after an earlier batch passed its base.
func TestLaterBatchStillRunsItsOwnChecks(t *testing.T) {
	c, _, s, runner := batchFixture(t, "test:\n\ttrue\n", `{"status":"completed","summary":"done"}`)
	c.batchPassed.Store(1)
	defer c.batchPassed.Store(0)
	task := batchTask(c, s, "Second batch", "a.txt")
	a := c.spawn(task, agent.Worker)

	c.batching.Store(1)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	c.batching.Store(0)
	c.finishBatch(context.Background(), c.verifier())
	if got := runner.calls(); got != 1 {
		t.Fatalf("checks ran %d times for the later batch, want once", got)
	}
}

// reportOf waits for the task to settle and returns the report its agent left. A worker that failed its checks is
// queued for another attempt, so the report, not the status, is what the Primary reads.
func (c *Coord) reportOf(t *testing.T, taskID string) (agent.Report, bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if got, _ := c.Graph.Get(taskID); got.Report != nil {
			rep, ok := got.Report.(agent.Report)
			return rep, ok
		}
		time.Sleep(5 * time.Millisecond)
	}
	return agent.Report{}, false
}
