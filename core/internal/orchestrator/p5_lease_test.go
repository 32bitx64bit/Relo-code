package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

// The dispatcher must not start a task whose declared path another running task holds.
func TestPathLeasesKeepOverlappingTasksSequential(t *testing.T) {
	c, r := newQueueCoordIn(t, gitRepo(t), scheduler.Limits{PathLeases: true}, agent.Worker, "w1", "w2")
	for _, id := range []string{"w1", "w2"} {
		if _, err := c.Graph.Update(id, func(x *taskgraph.Task) { x.Paths = []string{"a.txt"} }); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()

	first := nextBegun(t, r)
	waitFor(t, "the second task to wait on the path lease", func() bool {
		for _, a := range c.Agents.List() {
			if a.Role == agent.Worker && a.Status == agent.Queued && a.Activity == scheduler.WhyPaths {
				return true
			}
		}
		return false
	})
	select {
	case extra := <-r.begun:
		t.Fatalf("%s began while %s held a.txt", extra, first)
	case <-time.After(50 * time.Millisecond):
	}
	r.release(first)
	second := nextBegun(t, r)
	r.release(second)
	driveEnds(t, done)
	if r.peak != 1 {
		t.Fatalf("peak overlap = %d, want 1", r.peak)
	}
	for _, task := range c.Graph.List() {
		if task.Status != taskgraph.Done {
			t.Errorf("task %s ended %s", task.ID, task.Status)
		}
	}
}

// Declaring no paths must not change scheduling: two pathless tasks still start together.
func TestPathLeasesDoNotSerializeTasksWithoutPaths(t *testing.T) {
	c, r := newQueueCoordIn(t, gitRepo(t), scheduler.Limits{PathLeases: true, MaxTotal: 4, MaxTier1: 2}, agent.Worker, "w1", "w2")
	done := make(chan error, 1)
	go func() { done <- c.drive(context.Background()) }()
	first := nextBegun(t, r)
	second := nextBegun(t, r)
	r.release(first)
	r.release(second)
	driveEnds(t, done)
	if r.peak != 2 {
		t.Fatalf("peak overlap = %d, want 2 without declared paths", r.peak)
	}
}

// A lease overlap is a soft dependency: the later agent waits for the earlier to finish and is then cut with the
// earlier agent's still-pending patch on the leased path, recording it in lineage and needs.
func TestLeasedOverlapStacksTheEarlierPendingPatch(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.Sched = scheduler.New(scheduler.Limits{PathLeases: true})
	c.SetRuntime(newDispatchRuntime(`{"status":"completed","summary":"first"}`, `{"status":"completed","summary":"second"}`))
	s := c.CreateSession(dir, "leases", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	c.refreshSnapshot()

	first := c.Graph.Add(taskgraph.Task{Title: "First edit", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "medium", Paths: []string{"a.txt"}})
	releaseFirst, ok, why := c.Sched.AcquirePaths(first.ID, first.Paths)
	if !ok {
		t.Fatalf("first lease: %s", why)
	}
	a := c.spawnSoft(first, agent.Worker, c.Sched.PriorHolders(first.Paths))
	if a.Worktree == dir {
		t.Fatal("the worker should have its own worktree")
	}
	if err := os.WriteFile(filepath.Join(dirOf(t, c, a), "a.txt"), []byte("one\nfirst\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Graph.Update(first.ID, func(x *taskgraph.Task) { x.AgentID = a.ID }); err != nil {
		t.Fatal(err)
	}
	if err := c.execWorker(context.Background(), first, a); err != nil {
		t.Fatal(err)
	}
	var firstChange Change
	for _, ch := range c.Changes() {
		if ch.AgentID == a.ID {
			firstChange = ch
		}
	}
	if firstChange.ID == "" || firstChange.Status != "proposed" {
		t.Fatalf("first change = %+v", firstChange)
	}
	releaseFirst()

	second := c.Graph.Add(taskgraph.Task{Title: "Second edit", Role: string(agent.Worker), Workspace: dir, Model: "mock/t1", Difficulty: "easy", Paths: []string{"a.txt"}})
	soft := c.Sched.PriorHolders(second.Paths)
	if len(soft) != 1 || soft[0].TaskID != first.ID {
		t.Fatalf("prior holders = %+v, want the finished first task", soft)
	}
	releaseSecond, ok, why := c.Sched.AcquirePaths(second.ID, second.Paths)
	if !ok {
		t.Fatalf("second lease: %s", why)
	}
	defer releaseSecond()

	b := c.spawnSoft(second, agent.Worker, soft)
	if got, _ := agentFile(c, b, "a.txt"); got != "one\nfirst\n" {
		t.Fatalf("the later agent saw %q, want the earlier pending patch", got)
	}
	if err := os.WriteFile(filepath.Join(dirOf(t, c, b), "a.txt"), []byte("one\nfirst\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Graph.Update(second.ID, func(x *taskgraph.Task) { x.AgentID = b.ID }); err != nil {
		t.Fatal(err)
	}
	if err := c.execWorker(context.Background(), second, b); err != nil {
		t.Fatal(err)
	}
	var got Change
	for _, ch := range c.Changes() {
		if ch.AgentID == b.ID {
			got = ch
		}
	}
	if got.ID == "" {
		t.Fatal("the later agent recorded no change")
	}
	if len(got.Needs) != 1 || got.Needs[0] != firstChange.ID {
		t.Fatalf("needs = %v, want [%s]", got.Needs, firstChange.ID)
	}
	if len(got.Lineage) != 1 || got.Lineage[0].ID != firstChange.ID || got.Lineage[0].PatchHash == "" {
		t.Fatalf("lineage = %+v, want the leased parent with its patch hash", got.Lineage)
	}
	if got.BaseRev == "" {
		t.Fatal("the change has no BaseRev")
	}
}
