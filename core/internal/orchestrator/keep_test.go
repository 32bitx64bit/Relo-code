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
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

// A worker's edits live in its worktree and are invisible until they are recorded as changes. A worker that was
// stopped, or hung, never got that far: in a real session one left 127 lines across ten files on disk while the
// Changes tab stayed empty.

// editingRuntime writes a file into the worktree it is given, then behaves as the embedded runtime does (answers
// with the next scripted response) or, with no responses, goes silent.
type editingRuntime struct {
	*dispatchRuntime
	file   string
	edited chan struct{}
}

func (r *editingRuntime) SendTask(ctx context.Context, sid rt.SessionID, task rt.AgentTask) error {
	if err := os.WriteFile(filepath.Join(task.Directory, r.file), []byte("work\n"), 0o644); err != nil {
		return err
	}
	select {
	case r.edited <- struct{}{}:
	default:
	}
	if len(r.responses) == 0 {
		r.mu.Lock()
		r.tasks = append(r.tasks, task)
		r.mu.Unlock()
		return nil
	}
	return r.dispatchRuntime.SendTask(ctx, sid, task)
}

func workerInRepo(t *testing.T, r rt.AgentRuntime) (*Coord, taskgraph.Task, agent.Agent) {
	t.Helper()
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.SetRuntime(r)
	c.CreateSession(dir, "Make b", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: dir, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Write b", Description: "Create b.txt", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "medium", MaxAttempts: 2})
	return c, task, c.spawn(task, agent.Worker)
}

func TestWhatAStoppedWorkerEditedReachesTheChangesTab(t *testing.T) {
	r := &editingRuntime{dispatchRuntime: newDispatchRuntime(), file: "b.txt", edited: make(chan struct{}, 1)}
	c, task, a := workerInRepo(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.execWorker(ctx, task, a) }()
	select {
	case <-r.edited:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never started")
	}
	cancel() // the user presses Stop
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}

	changes := c.Changes()
	if len(changes) != 1 || changes[0].Path != "b.txt" || changes[0].Status != "proposed" || changes[0].AgentID != a.ID {
		t.Fatalf("the Changes tab holds %+v, want the stopped worker's b.txt", changes)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Canceled {
		t.Fatalf("task is %s, want it cancelled (a Stop is not retried)", got.Status)
	}
	told := false
	for _, m := range c.Current().Messages {
		told = told || (m.Role == "system" && strings.Contains(m.Text, "was stopped") && strings.Contains(m.Text, "(1 file) are in the Changes tab"))
	}
	if !told {
		t.Fatalf("the user was not told where the work went: %+v", c.Current().Messages)
	}
}

// A worker that hangs after editing is not started again from nothing: the retry would redo the work and meet the
// same hang. Its edits are kept and the user is told.
func TestAHungWorkerKeepsItsEditsAndIsNotRestarted(t *testing.T) {
	setLimits(t, 80*time.Millisecond, 0)
	r := &editingRuntime{dispatchRuntime: newDispatchRuntime(), file: "b.txt", edited: make(chan struct{}, 1)}
	c, task, a := workerInRepo(t, r)
	err := c.execWorker(context.Background(), task, a)
	var stalled *stalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("err = %v, want a stalled agent", err)
	}
	if got := c.Changes(); len(got) != 1 || got[0].Path != "b.txt" {
		t.Fatalf("changes = %+v", got)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Failed {
		t.Fatalf("task is %s, want it failed and left alone, not queued for another attempt", got.Status)
	}
	told := false
	for _, m := range c.Current().Messages {
		told = told || (strings.Contains(m.Text, "not restarted from scratch") && strings.Contains(m.Text, "Changes tab"))
	}
	if !told {
		t.Fatalf("the user was not told: %+v", c.Current().Messages)
	}
}

// With nothing to keep, a fresh attempt is the right answer to a hang.
func TestAHungWorkerThatChangedNothingIsRetried(t *testing.T) {
	setLimits(t, 80*time.Millisecond, 0)
	r := newDispatchRuntime()
	r.responses = nil
	c, task, a := workerInRepo(t, &silentRuntime{r})
	if err := c.execWorker(context.Background(), task, a); err == nil {
		t.Fatal("a silent worker did not fail")
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Queued {
		t.Fatalf("task is %s, want it queued for another attempt", got.Status)
	}
	if len(c.Changes()) != 0 {
		t.Fatalf("changes = %+v", c.Changes())
	}
}

type silentRuntime struct{ *dispatchRuntime }

func (r *silentRuntime) SendTask(_ context.Context, _ rt.SessionID, task rt.AgentTask) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()
	return nil
}

// A failed attempt that will be run again leaves nothing in the Changes tab: the next attempt's work replaces it.
func TestAFailureThatWillBeRetriedDoesNotLeaveHalfDoneChanges(t *testing.T) {
	r := &editingRuntime{dispatchRuntime: newDispatchRuntime(), file: "b.txt", edited: make(chan struct{}, 1)}
	r.responses = []string{`not json at all, and no files either`}
	c, task, a := workerInRepo(t, r)
	_ = c.execWorker(context.Background(), task, a)
	// The report was unusable but the edit is real, so this is a success with a note: it is recorded.
	if got := c.Changes(); len(got) != 1 {
		t.Fatalf("changes = %+v", got)
	}

	// An error from the runtime itself is retried, and then nothing half-done is shown.
	c2, task2, a2 := workerInRepo(t, &failingRuntime{editingRuntime: &editingRuntime{dispatchRuntime: newDispatchRuntime(), file: "b.txt", edited: make(chan struct{}, 1)}})
	_ = c2.execWorker(context.Background(), task2, a2)
	got, _ := c2.Graph.Get(task2.ID)
	if got.Status != taskgraph.Queued {
		t.Fatalf("task is %s, want it queued for another attempt", got.Status)
	}
	if len(c2.Changes()) != 0 {
		t.Fatalf("a failed attempt that is about to be redone left %+v in the Changes tab", c2.Changes())
	}
}

type failingRuntime struct{ *editingRuntime }

func (r *failingRuntime) SendTask(ctx context.Context, sid rt.SessionID, task rt.AgentTask) error {
	_ = r.editingRuntime.SendTask(ctx, sid, task)
	r.events <- rt.Event{Type: "session.error", Done: true, Error: "provider overloaded, try again"}
	return nil
}

func TestACompletedWorkerThatChangedNothingSaysSo(t *testing.T) {
	r := newDispatchRuntime(`{"status":"completed","summary":"Not implemented: the engine has no map size."}`)
	c, task, a := workerInRepo(t, r)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Agents.Get(a.ID)
	if got.Activity != "Done, no changes" {
		t.Fatalf("the card says %q, want it to say nothing was changed", got.Activity)
	}
	rep, _ := c.Agents.Report(a.ID)
	if len(rep.Risks) == 0 || rep.Risks[0] != "Changed no files." {
		t.Fatalf("risks = %v: the Primary reads the report, so it must say so too", rep.Risks)
	}
	if len(c.Changes()) != 0 {
		t.Fatalf("changes = %+v", c.Changes())
	}

	// A worker that did change something is simply done.
	r2 := &editingRuntime{dispatchRuntime: newDispatchRuntime(`{"status":"completed","summary":"Wrote b."}`), file: "b.txt", edited: make(chan struct{}, 1)}
	c2, task2, a2 := workerInRepo(t, r2)
	if err := c2.execWorker(context.Background(), task2, a2); err != nil {
		t.Fatal(err)
	}
	got2, _ := c2.Agents.Get(a2.ID)
	rep2, _ := c2.Agents.Report(a2.ID)
	if got2.Activity != "Done" || len(rep2.Risks) != 0 || len(c2.Changes()) != 1 {
		t.Fatalf("activity=%q risks=%v changes=%+v", got2.Activity, rep2.Risks, c2.Changes())
	}
}
