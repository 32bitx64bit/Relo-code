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
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/routing"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

// gatedRuntime holds each worker's answer until the test lets that worker go, so a test decides which agent finishes
// and when. Primary answers at once: it only has to wrap up after the workers.
type gatedRuntime struct {
	mu     sync.Mutex
	nextID int
	titles map[rt.SessionID]string
	events map[rt.SessionID]chan rt.Event
	gates  map[string]chan struct{}
	begun  chan string // a worker's title, once its prompt has been sent
	active int
	peak   int
	fails  map[string]int // how many more attempts of a title end in an error
}

func newGatedRuntime() *gatedRuntime {
	return &gatedRuntime{titles: map[rt.SessionID]string{}, events: map[rt.SessionID]chan rt.Event{}, gates: map[string]chan struct{}{}, begun: make(chan string, 64), fails: map[string]int{}}
}

func (r *gatedRuntime) gate(title string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gates[title] == nil {
		r.gates[title] = make(chan struct{})
	}
	return r.gates[title]
}

func (r *gatedRuntime) release(title string) { close(r.gate(title)) }

func (r *gatedRuntime) StartSession(_ context.Context, cfg rt.SessionConfig) (rt.SessionID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := rt.SessionID("gate" + string(rune('a'+r.nextID)))
	r.titles[id] = cfg.Title
	r.events[id] = make(chan rt.Event, 8)
	return id, nil
}

func (r *gatedRuntime) SendTask(ctx context.Context, sid rt.SessionID, task rt.AgentTask) error {
	r.mu.Lock()
	title, ch := r.titles[sid], r.events[sid]
	r.mu.Unlock()
	if task.Role == string(agent.Primary) {
		const wrap = `{"status":"completed","userMessage":"done","tasks":[]}`
		ch <- rt.Event{Type: "session.idle", Idle: true, Done: true, JSON: wrap, Text: wrap}
		return nil
	}
	r.mu.Lock()
	r.active++
	r.peak = max(r.peak, r.active)
	failing := r.fails[title] > 0
	if failing {
		r.fails[title]--
	}
	r.mu.Unlock()
	r.begun <- title
	if failing {
		go func() {
			defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
			select {
			case <-r.gate(title):
			case <-ctx.Done():
				return
			}
			ch <- rt.Event{Type: "session.error", Done: true, Error: "the provider refused the request"}
		}()
		return nil
	}
	go func() {
		defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
		select {
		case <-r.gate(title):
		case <-ctx.Done():
			return
		}
		ch <- rt.Event{Type: "session.idle", Idle: true, Done: true, JSON: directReport, Text: directReport}
	}()
	return nil
}

func (r *gatedRuntime) Events(_ context.Context, sid rt.SessionID) (<-chan rt.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[sid], nil
}
func (r *gatedRuntime) Cancel(context.Context, rt.SessionID) error       { return nil }
func (r *gatedRuntime) CloseSession(context.Context, rt.SessionID) error { return nil }
func (r *gatedRuntime) Models(context.Context) ([]rt.Model, error)       { return nil, nil }
func (r *gatedRuntime) Providers(context.Context) ([]rt.Provider, error) { return nil, nil }
func (r *gatedRuntime) Health(context.Context) rt.Health                 { return rt.Health{Healthy: true} }
func (r *gatedRuntime) Close() error                                     { return nil }

// newQueueCoord is a non-Git workspace (direct mode, so one mutating agent at a time) with the given limits and one
// task of the given role per title.
func newQueueCoord(t *testing.T, limits scheduler.Limits, role agent.Role, titles ...string) (*Coord, *gatedRuntime) {
	t.Helper()
	return newQueueCoordIn(t, t.TempDir(), limits, role, titles...)
}

func newQueueCoordIn(t *testing.T, dir string, limits scheduler.Limits, role agent.Role, titles ...string) (*Coord, *gatedRuntime) {
	t.Helper()
	r := newGatedRuntime()
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	if _, err := c.WS.Snapshot(); err != nil {
		t.Fatal(err)
	}
	c.SetRuntime(r)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	limits.MaxRetry = 1
	c.Sched = scheduler.New(limits)
	c.CreateSession(dir, "queue", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: dir})
	for _, title := range titles {
		c.Graph.Add(taskgraph.Task{ID: title, Title: title, Role: string(role), Workspace: dir, Difficulty: routing.Medium, Model: "mock/t1"})
	}
	return c, r
}

// agentsOf lists the agents Relo created for one task.
func agentsOf(c *Coord, taskID string) []agent.Agent {
	var out []agent.Agent
	for _, a := range c.Agents.List() {
		if a.TaskID == taskID {
			out = append(out, a)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nextBegun(t *testing.T, r *gatedRuntime) string {
	t.Helper()
	select {
	case title := <-r.begun:
		return title
	case <-time.After(3 * time.Second):
		t.Fatal("no worker began")
		return ""
	}
}

func driveEnds(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drive did not finish")
	}
}

// Three tier-1 slots' worth of work against two slots. The third worker waits for a slot, and must take the first one
// that opens: it does not wait for the whole batch to finish, which in a workspace without Git (one worker at a time)
// meant until every other worker was done.
func TestAQueuedWorkerStartsWhenASlotFrees(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{MaxTier1: 2}, agent.Worker, "w1", "w2", "w3")
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()

	first := nextBegun(t, r) // holds the workspace; the second admitted worker waits for it
	var queued agent.Agent
	waitFor(t, "one worker queued behind the tier limit", func() bool {
		for _, a := range c.Agents.List() {
			if a.Status == agent.Queued && a.Role == agent.Worker && a.Activity != "" {
				queued = a
				return true
			}
		}
		return false
	})
	if queued.Activity != "max tier1 agents" {
		t.Errorf("the queued worker says %q, want the reason it waits", queued.Activity)
	}

	r.release(first)
	waitFor(t, "the queued worker to be admitted once a slot freed", func() bool {
		a, _ := c.Agents.Get(queued.ID)
		return a.Status != agent.Queued
	})
	if got := agentsOf(c, queued.TaskID); len(got) != 1 || got[0].ID != queued.ID {
		t.Errorf("task %s has %d agents; the queued one should be the one that runs", queued.TaskID, len(got))
	}

	for _, title := range []string{"w1", "w2", "w3"} {
		if title != first {
			r.release(title)
		}
	}
	driveEnds(t, done)
	for _, task := range c.Graph.List() {
		if task.Status != taskgraph.Done {
			t.Errorf("task %s ended %s", task.ID, task.Status)
		}
		if got := agentsOf(c, task.ID); len(got) != 1 {
			t.Errorf("task %s has %d agents, want 1: a retry of a queued task must not leave the first agent behind", task.ID, len(got))
		}
	}
	for _, a := range c.Agents.List() {
		if a.Status == agent.Queued {
			t.Errorf("agent %s (%s) is still queued after the run", a.ID, a.Title)
		}
	}
}

// Read-only agents are not serialized by the workspace, so the limit is the only thing between them and running
// together. It holds at every moment, and each queued agent starts as the one before it ends.
func TestTheTierLimitHoldsWhileQueuedAgentsTakeTurns(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{MaxTier2: 1}, agent.Probe, "p1", "p2", "p3")
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()

	for i := 0; i < 3; i++ {
		title := nextBegun(t, r)
		if running := countStatus(c, agent.Running, agent.Starting); running > 1 {
			t.Fatalf("%d tier2 agents active under a limit of 1", running)
		}
		select {
		case extra := <-r.begun:
			t.Fatalf("%s began while %s was still running", extra, title)
		case <-time.After(30 * time.Millisecond):
		}
		r.release(title)
	}
	driveEnds(t, done)
	if r.peak != 1 {
		t.Errorf("peak overlap = %d, want 1", r.peak)
	}
	for _, task := range c.Graph.List() {
		if task.Status != taskgraph.Done || len(agentsOf(c, task.ID)) != 1 {
			t.Errorf("task %s: %s with %d agents", task.ID, task.Status, len(agentsOf(c, task.ID)))
		}
	}
}

func countStatus(c *Coord, statuses ...agent.Status) int {
	n := 0
	for _, a := range c.Agents.List() {
		if a.Role == agent.Primary {
			continue
		}
		for _, s := range statuses {
			if a.Status == s {
				n++
			}
		}
	}
	return n
}

// parked counts the agents that are queued and have been told why.
func parked(c *Coord) int {
	n := 0
	for _, a := range c.Agents.List() {
		if a.Status == agent.Queued && a.Activity != "" {
			n++
		}
	}
	return n
}

// Agents that have waited longest go first.
func TestTheLongestWaitingAgentStartsFirst(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{MaxTier2: 1}, agent.Probe, "p1", "p2", "p3")
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	first := nextBegun(t, r)
	waitFor(t, "the other two to queue", func() bool { return parked(c) == 2 })
	created := map[string]time.Time{}
	for _, a := range c.Agents.List() {
		if a.Status == agent.Queued {
			created[a.Title] = a.Created
		}
	}
	r.release(first)
	second := nextBegun(t, r)
	for title, at := range created {
		if title != second && at.Before(created[second]) {
			t.Errorf("%s began before %s, which had waited longer", second, title)
		}
	}
	r.release(second)
	r.release(nextBegun(t, r))
	driveEnds(t, done)
}

// A worker that failed is queued for another attempt while its goroutine is still closing down. The retry starts
// afterwards and the request still ends in one drive.
func TestAFailedWorkersRetryStartsAfterItAndFinishes(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{}, agent.Probe, "p1")
	r.fails["p1"] = 1
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	nextBegun(t, r)
	r.release("p1") // the failing attempt ends; the retry reuses the same gate
	nextBegun(t, r)
	driveEnds(t, done)
	task, _ := c.Graph.Get("p1")
	if task.Status != taskgraph.Done || task.Attempt != 2 {
		t.Errorf("task = %s after %d attempts, want done after 2", task.Status, task.Attempt)
	}
	if got := countStatus(c, agent.Queued); got != 0 {
		t.Errorf("%d agents left queued", got)
	}
}

// While dispatch is paused nothing new starts, however many slots are free, and the queue moves on resume.
func TestPausedDispatchStartsNothingAndResumeTakesTheQueue(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{MaxTier2: 1}, agent.Probe, "p1", "p2")
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	first := nextBegun(t, r)
	waitFor(t, "the second to queue", func() bool { return parked(c) == 1 })
	c.Pause()
	r.release(first)
	select {
	case title := <-r.begun:
		t.Fatalf("%s began while dispatch was paused", title)
	case <-time.After(150 * time.Millisecond):
	}
	c.Resume()
	second := nextBegun(t, r)
	r.release(second)
	driveEnds(t, done)
	for _, task := range c.Graph.List() {
		if task.Status != taskgraph.Done {
			t.Errorf("task %s ended %s", task.ID, task.Status)
		}
	}
}

// Stop ends the run with agents waiting for a slot: none is left looking queued.
func TestStoppingARunLeavesNoQueuedAgent(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{MaxTier2: 1}, agent.Probe, "p1", "p2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.drive(ctx) }()
	nextBegun(t, r)
	waitFor(t, "the second to queue", func() bool { return parked(c) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drive did not return after the run was stopped")
	}
	if got := countStatus(c, agent.Queued); got != 0 {
		t.Errorf("%d agents still queued after the stop", got)
	}
	if got := countStatus(c, agent.Starting, agent.Running, agent.Waiting); got != 0 {
		t.Errorf("%d workers still active after the stop", got)
	}
}

// Capacity held by something that is not this run's worker cannot be waited for: nothing here would ever wake. The
// run ends without a queued agent left behind to look as if it were still going to start.
func TestWhenNothingCanFreeASlotTheRunEndsWithoutAZombie(t *testing.T) {
	c, _ := newQueueCoord(t, scheduler.Limits{MaxTier2: 1}, agent.Probe, "p1")
	c.Agents.Create(agent.Agent{Role: agent.Probe, Status: agent.Running, Workspace: t.TempDir()})
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	driveEnds(t, done)
	if got := countStatus(c, agent.Queued); got != 0 {
		t.Errorf("%d agents left queued", got)
	}
	for _, a := range agentsOf(c, "p1") {
		if a.Status != agent.Cancelled {
			t.Errorf("the agent that never started is %s, want cancelled", a.Status)
		}
	}
	if task, _ := c.Graph.Get("p1"); task.Status != taskgraph.Queued {
		t.Errorf("the task is %s; it should still be waiting to run", task.Status)
	}
}

// In a Git repository workers have worktrees of their own and run side by side. A waiting worker holds a worktree
// too, and takes the first slot that opens.
func TestInAGitRepoAQueuedWorkerRunsBesideTheOthersWhenASlotFrees(t *testing.T) {
	dir := gitRepo(t)
	c, r := newQueueCoordIn(t, dir, scheduler.Limits{MaxTier1: 2}, agent.Worker, "w1", "w2", "w3")
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()

	first, second := nextBegun(t, r), nextBegun(t, r) // two at once: nothing serializes them here
	var queued agent.Agent
	waitFor(t, "the third worker to queue", func() bool {
		for _, a := range c.Agents.List() {
			if a.Status == agent.Queued && a.Activity != "" {
				queued = a
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(dir, ".relo", "worktrees", queued.ID)); err != nil {
		t.Errorf("the queued worker has no worktree to start in: %v", err)
	}
	r.release(first)
	third := nextBegun(t, r)
	if third == first || third == second {
		t.Fatalf("%s began twice", third)
	}
	r.release(second)
	r.release(third)
	driveEnds(t, done)
	if r.peak != 2 {
		t.Errorf("peak overlap = %d, want 2", r.peak)
	}
	for _, task := range c.Graph.List() {
		if task.Status != taskgraph.Done || len(agentsOf(c, task.ID)) != 1 {
			t.Errorf("task %s: %s with %d agents", task.ID, task.Status, len(agentsOf(c, task.ID)))
		}
	}
}

// An agent that waited and never got to start gives its worktree back when the run ends.
func TestAnAgentThatNeverStartedGivesBackItsWorktree(t *testing.T) {
	dir := gitRepo(t)
	c, _ := newQueueCoordIn(t, dir, scheduler.Limits{MaxTier1: 1}, agent.Worker, "w1")
	c.Agents.Create(agent.Agent{Role: agent.Worker, Status: agent.Running, Workspace: dir})
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	driveEnds(t, done)
	for _, a := range agentsOf(c, "w1") {
		if a.Status != agent.Cancelled {
			t.Errorf("the agent that never started is %s, want cancelled", a.Status)
		}
		if !strings.Contains(a.Worktree, filepath.Join(".relo", "worktrees")) {
			t.Fatalf("the agent had no worktree of its own (%q), so this test shows nothing", a.Worktree)
		}
		if _, err := os.Stat(filepath.Join(dir, ".relo", "worktrees", a.ID)); !os.IsNotExist(err) {
			t.Errorf("the worktree of agent %s is still there (%v)", a.ID, err)
		}
	}
	if len(agentsOf(c, "w1")) != 1 {
		t.Errorf("agents for the task = %d, want 1", len(agentsOf(c, "w1")))
	}
}

// A task that was waiting on a dependency starts when that dependency is done, not when every other worker of the
// batch is.
func TestADependentStartsWhenItsDependencyFinishes(t *testing.T) {
	c, r := newQueueCoord(t, scheduler.Limits{}, agent.Probe, "p1", "p2")
	c.Graph.Add(taskgraph.Task{ID: "p3", Title: "p3", Role: string(agent.Probe), Workspace: c.Current().Workspace, Depends: []string{"p2"}, Difficulty: routing.Medium, Model: "mock/t1"})
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	got := map[string]bool{nextBegun(t, r): true, nextBegun(t, r): true}
	if !got["p1"] || !got["p2"] {
		t.Fatalf("began %v, want p1 and p2 first", got)
	}
	r.release("p2") // p1 is still working
	if title := nextBegun(t, r); title != "p3" {
		t.Fatalf("%s began, want p3 as soon as p2 was done", title)
	}
	r.release("p1")
	r.release("p3")
	driveEnds(t, done)
	for _, task := range c.Graph.List() {
		if task.Status != taskgraph.Done {
			t.Errorf("task %s ended %s", task.ID, task.Status)
		}
	}
}

// A task cancelled while its agent waits for a slot takes the agent with it.
func TestCancellingAQueuedTaskRetiresItsAgent(t *testing.T) {
	old := dispatchTick
	dispatchTick = 10 * time.Millisecond
	defer func() { dispatchTick = old }()
	c, r := newQueueCoord(t, scheduler.Limits{MaxTier2: 1}, agent.Probe, "p1", "p2")
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	first := nextBegun(t, r)
	waitFor(t, "the second to queue", func() bool { return parked(c) == 1 })
	other := "p1"
	if first == "p1" {
		other = "p2"
	}
	if err := c.Graph.Cancel(other); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cancelled task's agent to be retired", func() bool { return parked(c) == 0 })
	for _, a := range agentsOf(c, other) {
		if a.Status != agent.Cancelled {
			t.Errorf("agent of the cancelled task is %s, want cancelled", a.Status)
		}
	}
	r.release(first)
	driveEnds(t, done)
}
