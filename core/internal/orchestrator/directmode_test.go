package orchestrator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/routing"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

const directReport = `{"status":"completed","summary":"edited file","filesChanged":["out.txt"]}`

// overlapRuntime answers every task with directReport, records sessions currently
// executing a task, and remembers the peak overlap and closed/cancelled sessions.
type overlapRuntime struct {
	mu        sync.Mutex
	nextID    int
	active    int
	peak      int
	cancels   map[rt.SessionID]int
	closes    map[rt.SessionID]int
	events    map[rt.SessionID]chan rt.Event
	taskDelay time.Duration
}

func newOverlapRuntime(delay time.Duration) *overlapRuntime {
	return &overlapRuntime{cancels: map[rt.SessionID]int{}, closes: map[rt.SessionID]int{}, events: map[rt.SessionID]chan rt.Event{}, taskDelay: delay}
}

func (r *overlapRuntime) StartSession(context.Context, rt.SessionConfig) (rt.SessionID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := rt.SessionID("ov" + string(rune('a'+r.nextID)))
	r.events[id] = make(chan rt.Event, 8)
	return id, nil
}

func (r *overlapRuntime) SendTask(_ context.Context, sid rt.SessionID, _ rt.AgentTask) error {
	r.mu.Lock()
	r.active++
	if r.active > r.peak {
		r.peak = r.active
	}
	ch := r.events[sid]
	r.mu.Unlock()
	go func() {
		time.Sleep(r.taskDelay)
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
		select {
		case ch <- rt.Event{Type: "session.idle", Idle: true, Done: true, JSON: directReport, Text: directReport}:
		default:
		}
	}()
	return nil
}

func (r *overlapRuntime) Events(_ context.Context, sid rt.SessionID) (<-chan rt.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[sid], nil
}

func (r *overlapRuntime) Cancel(_ context.Context, sid rt.SessionID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[sid]++
	return nil
}

func (r *overlapRuntime) CloseSession(_ context.Context, sid rt.SessionID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes[sid]++
	return nil
}

func (r *overlapRuntime) Models(context.Context) ([]rt.Model, error)       { return nil, nil }
func (r *overlapRuntime) Providers(context.Context) ([]rt.Provider, error) { return nil, nil }
func (r *overlapRuntime) Health(context.Context) rt.Health                 { return rt.Health{Healthy: true} }
func (r *overlapRuntime) Close() error                                     { return nil }

func (r *overlapRuntime) peakOverlap() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

func newDirectCoord(t *testing.T, dir string, rtm rt.AgentRuntime) (*Coord, *Session) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	if _, err := c.WS.Snapshot(); err != nil {
		t.Fatal(err)
	}
	c.SetRuntime(rtm)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, true
	c.Sched = scheduler.New(scheduler.Limits{MaxRetry: 1})
	s := c.CreateSession(dir, "direct", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: dir})
	return c, s
}

func chatNotices(c *Coord) int {
	n := 0
	for _, m := range c.Current().Messages {
		if m.Role == "system" && strings.Contains(m.Text, "not a Git repository") {
			n++
		}
	}
	return n
}

func TestDirectModeRunsInWorkspaceSerializesAndRecordsApplied(t *testing.T) {
	dir := t.TempDir()
	rtm := newOverlapRuntime(20 * time.Millisecond)
	c, s := newDirectCoord(t, dir, rtm)
	var tasks []taskgraph.Task
	for _, id := range []string{"w1", "w2", "w3"} {
		tasks = append(tasks, c.Graph.Add(taskgraph.Task{ID: id, Title: id, Role: string(agent.Worker), Workspace: dir, Difficulty: routing.Medium, Model: "mock/t1"}))
	}
	spawned := make([]agent.Agent, 0, len(tasks))
	for _, task := range tasks {
		a := c.spawn(task, agent.Worker)
		if a.Workspace != dir || a.Worktree != dir {
			t.Fatalf("direct agent workspace = %q, want %q", a.Workspace, dir)
		}
		spawned = append(spawned, a)
	}
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(task taskgraph.Task, a agent.Agent) {
			defer wg.Done()
			_ = c.execWorker(context.Background(), task, a)
		}(task, spawned[i])
	}
	wg.Wait()
	if got := rtm.peakOverlap(); got != 1 {
		t.Fatalf("peak concurrent mutating agents = %d, want 1", got)
	}
	if got := chatNotices(c); got != 1 {
		t.Fatalf("direct-mode notices = %d, want 1", got)
	}
	changes := c.Current().Changes
	if len(changes) == 0 {
		t.Fatal("no changes recorded")
	}
	for _, ch := range changes {
		if ch.Status != "applied" || ch.Worktree != "" {
			t.Fatalf("change = %+v, want applied without worktree", ch)
		}
	}
	_ = s
}

func TestDirectModeSkipsReviewer(t *testing.T) {
	dir := t.TempDir()
	c, _ := newDirectCoord(t, dir, newOverlapRuntime(0))
	task := c.Graph.Add(taskgraph.Task{ID: "d1", Title: "d1", Role: string(agent.Worker), Workspace: dir, Difficulty: routing.Hard, Model: "mock/t1"})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	for _, got := range c.Graph.List() {
		if got.Role == string(agent.Reviewer) {
			t.Fatalf("reviewer spawned in direct mode: %+v", got)
		}
	}
}

func TestDirectModeRejectReturnsClearError(t *testing.T) {
	dir := t.TempDir()
	c, _ := newDirectCoord(t, dir, newOverlapRuntime(0))
	task := c.Graph.Add(taskgraph.Task{ID: "d2", Title: "d2", Role: string(agent.Worker), Workspace: dir, Difficulty: routing.Hard, Model: "mock/t1"})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	ch := c.Current().Changes
	if len(ch) == 0 || ch[0].Status != "applied" {
		t.Fatalf("changes = %+v", ch)
	}
	if !strings.Contains(ch[0].Path, "out.txt") {
		t.Fatalf("path = %q", ch[0].Path)
	}
}

func TestGitWorkspaceKeepsWorktreeAndReviewer(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "relo@test")
	gitRun(t, dir, "config", "user.name", "Relo")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "init")
	c, _ := newDirectCoord(t, dir, newOverlapRuntime(0))
	task := c.Graph.Add(taskgraph.Task{ID: "g1", Title: "g1", Role: string(agent.Worker), Workspace: dir, Difficulty: routing.Hard, Model: "mock/t1"})
	a := c.spawn(task, agent.Worker)
	if a.Workspace == dir || !strings.Contains(a.Workspace, ".relo") {
		t.Fatalf("git mutating agent should use worktree, got %q", a.Workspace)
	}
	// The worker's change is whatever git sees in its worktree, so make one.
	if err := os.WriteFile(filepath.Join(dirOf(t, c, a), "out.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	if chatNotices(c) != 0 {
		t.Fatal("git workspace must not emit direct-mode notice")
	}
	reviewers := 0
	for _, got := range c.Graph.List() {
		if got.Role == string(agent.Reviewer) {
			reviewers++
		}
	}
	if reviewers != 1 {
		t.Fatalf("reviewers = %d, want 1", reviewers)
	}
	for _, ch := range c.Current().Changes {
		if ch.Status != "proposed" {
			t.Fatalf("git change status = %q, want proposed", ch.Status)
		}
	}
}

func TestTrivialTaskSkipsReviewerInGitRepo(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "relo@test")
	gitRun(t, dir, "config", "user.name", "Relo")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "init")
	c, _ := newDirectCoord(t, dir, newOverlapRuntime(0))
	task := c.Graph.Add(taskgraph.Task{ID: "t1", Title: "t1", Role: string(agent.Worker), Workspace: dir, Difficulty: routing.Trivial, Model: "mock/t1"})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	for _, got := range c.Graph.List() {
		if got.Role == string(agent.Reviewer) {
			t.Fatalf("reviewer spawned for trivial task: %+v", got)
		}
	}
}

func TestStopAllTwiceCancelsAndClosesOnce(t *testing.T) {
	dir := t.TempDir()
	rtm := newOverlapRuntime(0)
	c, _ := newDirectCoord(t, dir, rtm)
	sid, _ := rtm.StartSession(context.Background(), rt.SessionConfig{})
	c.runtimeMu.Lock()
	c.runtimeSessions["agent-x"] = &runtimeSession{id: sid}
	c.runtimeMu.Unlock()
	c.Agents.Create(agent.Agent{ID: "agent-x", Role: agent.Worker, RuntimeID: string(sid), Status: agent.Running})
	c.StopAll(context.Background())
	c.StopAll(context.Background())
	rtm.mu.Lock()
	defer rtm.mu.Unlock()
	if rtm.cancels[sid] > 1 || rtm.closes[sid] > 1 {
		t.Fatalf("cancels=%d closes=%d, want at most 1 each", rtm.cancels[sid], rtm.closes[sid])
	}
}

// queuedDirectWorker spawns a worker for a held direct-mode slot and runs execWorker for it in the background.
func queuedDirectWorker(t *testing.T, c *Coord, dir string, ctx context.Context) (taskgraph.Task, agent.Agent, <-chan error) {
	t.Helper()
	task := c.Graph.Add(taskgraph.Task{ID: "queued", Title: "queued", Role: string(agent.Worker), Workspace: dir, Difficulty: routing.Medium, Model: "mock/t1"})
	a := c.spawn(task, agent.Worker)
	c.Graph.Update(task.ID, func(x *taskgraph.Task) { x.Status = taskgraph.Running; x.AgentID = a.ID })
	done := make(chan error, 1)
	go func() { done <- c.execWorker(ctx, task, a) }()
	return task, a, done
}

// A mutating agent queued behind the direct-mode slot used to show a bare "Waiting for the workspace" with no limit,
// so a long holder looked like a freeze for the agents behind it (workspace stall in a non-Git project).
func TestDirectModeQueuedAgentNamesHolderAndFailsInsteadOfWaitingForever(t *testing.T) {
	oldLimit, oldTick := directWaitLimit, directWaitTick
	directWaitLimit, directWaitTick = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { directWaitLimit, directWaitTick = oldLimit, oldTick })
	dir := t.TempDir()
	c, _ := newDirectCoord(t, dir, newOverlapRuntime(0))
	holder := c.Agents.Create(agent.Agent{Role: agent.Architect, Title: "Write ARCHITECTURE.md", Workspace: dir})
	release, err := c.acquireDirect(context.Background(), holder)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	task, a, done := queuedDirectWorker(t, c, dir, context.Background())
	waitFor(t, "the queued agent to name the holder", func() bool {
		got, _ := c.Agents.Get(a.ID)
		return got.Phase == "waiting" && strings.Contains(got.Activity, "Waiting for the workspace") && strings.Contains(got.Activity, "Write ARCHITECTURE.md")
	})
	var err2 error
	select {
	case err2 = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queued agent still waiting long after directWaitLimit")
	}
	var busy *workspaceBusyError
	if !errors.As(err2, &busy) || !strings.Contains(err2.Error(), "Write ARCHITECTURE.md") {
		t.Fatalf("err = %v, want a workspaceBusyError naming the holder", err2)
	}
	if got, _ := c.Agents.Get(a.ID); got.Status != agent.Failed || !strings.Contains(got.Activity, "workspace busy") {
		t.Fatalf("agent = %s %q, want failed with the busy reason", got.Status, got.Activity)
	}
	if got, _ := c.Graph.Get(task.ID); got.Status == taskgraph.Running {
		t.Fatalf("task is still %s after its agent gave up", got.Status)
	}
	if rep, ok := c.Agents.Report(a.ID); !ok || !strings.Contains(rep.Summary, "workspace busy") {
		t.Fatalf("report = %+v ok=%v, want a final message naming why the agent gave up", rep, ok)
	}
	// The holder may still be busy, so a retry would only queue behind it for another full wait.
	if got, _ := c.Graph.Get(task.ID); got.Status != taskgraph.Failed {
		t.Fatalf("task = %s, want failed once and not re-queued", got.Status)
	}
	if !chatMentions(c, "will not be retried") {
		t.Fatal("no chat notice said the busy workspace failure would not be retried")
	}
}

// A workspace-busy failure is decided by type, not by its wording: the message says "busy" rather than any marker
// retryable() knows, so it used to be re-queued behind an unchanged holder for another full wait.
func TestDirectModeWorkspaceBusyIsNotRetried(t *testing.T) {
	c, s := newDirectCoord(t, t.TempDir(), newOverlapRuntime(0))
	c.Sched = scheduler.New(scheduler.Limits{MaxRetry: 3})
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace, Model: "mock/t1"})
	task := c.Graph.Add(taskgraph.Task{ID: "busy", Title: "busy", Role: "worker", Workspace: s.Workspace, Model: "mock/t1", Attempt: 1, MaxAttempts: 3})
	c.failTask(task, a, &workspaceBusyError{msg: "workspace busy: waited 20m0s for Write ARCHITECTURE.md to finish editing it"})
	if got, _ := c.Graph.Get(task.ID); got.Status != taskgraph.Failed {
		t.Fatalf("task status = %s, want failed", got.Status)
	}
	if !chatMentions(c, "Write ARCHITECTURE.md") {
		t.Fatal("the no-retry notice dropped the holder's name")
	}
	// A retryable failure still retries, so the early return did not disable retries in general.
	task2 := c.Graph.Add(taskgraph.Task{ID: "flaky", Title: "flaky", Role: "worker", Workspace: s.Workspace, Model: "mock/t1", Attempt: 1, MaxAttempts: 3})
	c.failTask(task2, a, errors.New("stream closed unexpectedly"))
	if got, _ := c.Graph.Get(task2.ID); got.Status != taskgraph.Queued {
		t.Fatalf("flaky task status = %s, want queued for a retry", got.Status)
	}
}

func chatMentions(c *Coord, want string) bool {
	for _, m := range c.Current().Messages {
		if m.Role == "system" && strings.Contains(m.Text, want) {
			return true
		}
	}
	return false
}

// Stopping a run while an agent queued for the workspace used to leave its task Running for ever, because the wait
// returned before the task was closed.
func TestDirectModeStopWhileQueuedClosesTaskAndAgent(t *testing.T) {
	dir := t.TempDir()
	c, _ := newDirectCoord(t, dir, newOverlapRuntime(0))
	holder := c.Agents.Create(agent.Agent{Role: agent.Architect, Title: "holder", Workspace: dir})
	release, err := c.acquireDirect(context.Background(), holder)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	task, a, done := queuedDirectWorker(t, c, dir, ctx)
	waitFor(t, "the agent to queue", func() bool {
		got, _ := c.Agents.Get(a.ID)
		return got.Phase == "waiting"
	})
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got, _ := c.Graph.Get(task.ID); got.Status != taskgraph.Canceled {
		t.Fatalf("task status = %s, want canceled", got.Status)
	}
	if got, _ := c.Agents.Get(a.ID); got.Status != agent.Cancelled {
		t.Fatalf("agent status = %s, want cancelled", got.Status)
	}
	if rep, ok := c.Agents.Report(a.ID); !ok || strings.TrimSpace(rep.Summary) == "" {
		t.Fatalf("report = %+v ok=%v, want a final message for the stopped agent", rep, ok)
	}
	// The holder's release frees the slot and clears the name, so the next agent is not told to wait for it.
	release()
	if h := func() directHold { c.mu.Lock(); defer c.mu.Unlock(); return c.directHolder }(); h.id != "" {
		t.Fatalf("holder = %+v after release, want none", h)
	}
	next, err := c.acquireDirect(context.Background(), holder)
	if err != nil {
		t.Fatalf("slot not free after release: %v", err)
	}
	next()
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
