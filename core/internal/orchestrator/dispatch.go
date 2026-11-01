package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/taskgraph"
)

// A request's tasks do not all fit under the limits at once: three tier-1 slots, five workers. The ones that do not
// fit show as queued agents, and each takes a slot the moment one opens. Starting tasks in rounds, a batch at a time,
// meant a queued worker waited for the whole batch to finish; in a workspace without Git, where workers take turns,
// that was every other worker.

// maxQueued is how many agents may exist only to wait for a slot. Each holds a worktree, so the rest of the tasks
// wait as tasks and get their agent when a place in the queue opens.
const maxQueued = 4

// dispatchTick is how often a dispatcher looks again when nothing woke it. Workers wake it as they finish; this
// covers a change it could not have been told about.
var dispatchTick = time.Second

type waiting struct {
	agent agent.Agent
	why   string // what the status line says, so it is written once and not on every look
}

type retryCarryError struct{ err error }

func (e *retryCarryError) Error() string { return e.err.Error() }
func (e *retryCarryError) Unwrap() error { return e.err }

// dispatcher runs the tasks of one round. Only the dispatch goroutine touches queued and started; running and errs
// are shared with the workers.
type dispatcher struct {
	c     *Coord
	ctx   context.Context
	sched *scheduler.Scheduler
	wg    sync.WaitGroup
	wake  chan struct{}

	queued  map[string]waiting // task ID -> the agent that waits for it to fit
	started int
	delayed bool // runnable retry is waiting for its not-before time

	mu      sync.Mutex
	running map[string]bool // task ID -> a worker goroutine for it is still alive
	waking  map[string]bool // agent ID -> a peer reply turn is still alive
	errs    []error
}

// dispatch starts every runnable task as the limits allow and returns when no worker is left, with the number of
// workers it started. It looks at the queue again whenever a worker finishes, not when a whole batch has.
func (c *Coord) dispatch(ctx context.Context, sched *scheduler.Scheduler) int {
	d := &dispatcher{c: c, ctx: ctx, sched: sched, wake: make(chan struct{}, 1), queued: map[string]waiting{}, running: map[string]bool{}, waking: map[string]bool{}}
	tick := time.NewTicker(dispatchTick)
	defer tick.Stop()
	for sched.Wait(ctx) == nil {
		held := d.admit()
		d.wakePeers()
		if d.alive() == 0 {
			if held {
				continue // the worker that held a task has gone since; it can start now
			}
			if d.delayed {
				select {
				case <-tick.C:
				case <-d.c.wake:
					d.c.stepQueued(ctx)
				case <-ctx.Done():
				}
				continue
			}
			// Nothing is running, so nothing will free a slot or finish a dependency: what is left cannot start now.
			break
		}
		select {
		case <-d.wake:
		case <-d.c.wake:
			// The user queued guidance while the head waited here: step the head now, without touching the
			// sub agents, which keep running.
			d.c.stepQueued(ctx)
		case <-tick.C:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	d.wg.Wait()
	// The round's checks ran once, for the batch: the next round's workers must not re-check the same tree.
	c.skipBatchVerify(ctx)
	if median := sched.LeaseWaitMedian(); median > 0 && c.Log != nil {
		fields := map[string]any{"medianStartDelayMs": median.Milliseconds()}
		// The design keeps path leases off when the delay they add exceeds the median agent run time measured in
		// the design data (568 s). The setting already defaults to off; this tells the user why to leave it there.
		if median > 568*time.Second {
			fields["advice"] = "keep relo.scheduler.pathLeases off"
		}
		c.Log.Info("path leases", fields)
	}
	d.dropQueued()
	for _, err := range d.errs {
		if ctx.Err() == nil {
			c.logWarn("worker", err)
		}
	}
	return d.started
}

func (d *dispatcher) alive() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running) + len(d.waking)
}

func (d *dispatcher) working(taskID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[taskID]
}

// skipBatchVerify marks a batch whose checks all passed, so the next one knows its base is already clear. It changes
// what the end-of-batch step re-runs (the base failure is known), never whether a batch's own changes are checked:
// finishBatch still runs the checks once for every batch that held a change.
func (c *Coord) skipBatchVerify(ctx context.Context) {
	c.mu.Lock()
	sess := c.sess
	c.mu.Unlock()
	if sess == nil || sess.Mode != ModeBuild || ctx.Err() != nil {
		return
	}
	passed := 0
	for _, v := range sess.Verify {
		if v.Status == "passed" {
			passed++
		}
	}
	if passed > 0 && passed == len(sess.Verify) {
		c.batchPassed.Store(1)
	}
}

// wakePeers starts bounded reply-only turns for finished workers with unanswered messages.
func (d *dispatcher) wakePeers() {
	c := d.c
	for _, a := range c.Agents.List() {
		if a.Role == agent.Primary || (a.Status != agent.Completed && a.Status != agent.ReviewReq) || d.working(a.TaskID) {
			continue
		}
		d.mu.Lock()
		if d.waking[a.ID] {
			d.mu.Unlock()
			continue
		}
		d.mu.Unlock()
		c.mu.Lock()
		if c.sess == nil {
			c.mu.Unlock()
			return
		}
		sid := c.sess.ID
		answered := map[string]bool{}
		for _, m := range c.sess.PeerMessages {
			if m.ReplyTo != "" {
				answered[m.ReplyTo] = true
			}
		}
		// Replies (ReplyTo set) are what wake turns produce, so they never wake anyone: a finished pair cannot ping-pong.
		var messages []PeerMessage
		for _, m := range c.sess.PeerMessages {
			if m.To == a.ID && m.ReplyTo == "" && !answered[m.ID] && (m.State == "queued" || m.State == "injected" || m.State == "delivered") {
				messages = append(messages, m)
			}
		}
		if len(messages) == 0 {
			c.mu.Unlock()
			continue
		}
		if c.sess.PeerWakes == nil {
			c.sess.PeerWakes = map[string]int{}
		}
		total := 0
		for _, n := range c.sess.PeerWakes {
			total += n
		}
		if c.sess.PeerWakes[a.ID] >= 2 || total >= 40 || d.ctx.Err() != nil {
			c.mu.Unlock()
			ids := map[string]bool{}
			for _, m := range messages {
				ids[m.ID] = true
			}
			c.settlePeerWake(sid, a.ID, ids, "peer wake limit reached or session stopped")
			continue
		}
		c.sess.PeerWakes[a.ID]++
		c.mu.Unlock()
		d.mu.Lock()
		if d.waking == nil {
			d.waking = map[string]bool{}
		}
		d.waking[a.ID] = true
		d.mu.Unlock()
		d.wg.Add(1)
		go func(a agent.Agent, messages []PeerMessage) {
			defer d.wg.Done()
			defer func() {
				d.mu.Lock()
				delete(d.waking, a.ID)
				d.mu.Unlock()
				select {
				case d.wake <- struct{}{}:
				default:
				}
			}()
			if err := c.guard("peer wake "+a.ID, func() error { c.peerReplyTurn(d.ctx, sid, a, messages); return nil }); err != nil {
				c.closeAgentSession(a.ID)
				ids := map[string]bool{}
				for _, m := range messages {
					ids[m.ID] = true
				}
				c.settlePeerWake(sid, a.ID, ids, "peer wake failed: "+err.Error())
			}
		}(a, messages)
	}
}

// admit starts what fits now, the agents that waited longest first, and queues what does not. It reports whether it
// passed over a task because the worker of its last attempt had not finished yet.
func (d *dispatcher) admit() (held bool) {
	c := d.c
	d.delayed = false
	tasks := d.sched.Next(c.Graph, 0)
	if d.sched.Paused() {
		return false // Next has nothing to say while paused, which must not read as "no task wants to run"
	}
	d.forget(tasks)
	sort.SliceStable(tasks, func(i, j int) bool {
		first, second := d.queued[tasks[i].ID], d.queued[tasks[j].ID]
		switch {
		case first.agent.ID != "" && second.agent.ID != "":
			return first.agent.Created.Before(second.agent.Created)
		case first.agent.ID != "" || second.agent.ID != "":
			return first.agent.ID != ""
		}
		return tasks[i].Created.Before(tasks[j].Created)
	})
	for _, task := range tasks {
		if d.ctx.Err() != nil || d.sched.Paused() {
			return held
		}
		if time.Now().Before(task.NotBefore) {
			d.delayed = true
			continue
		}
		// A task that failed and was queued for another attempt is still held by the worker that failed it, which is
		// closing its session. The new attempt starts after that worker has gone, as it did between rounds.
		if d.working(task.ID) {
			held = true
			continue
		}
		role := parseRole(task.Role)
		wait, queued := d.queued[task.ID]
		a := wait.agent
		if !queued {
			if ok, why := d.sched.CanReserve(c.Agents, agent.Agent{Role: role, Tier: agent.TierFor(role), Depth: 1}); !ok && (!scheduler.Full(why) || len(d.queued) >= maxQueued) {
				continue
			}
			a = c.spawnSoft(task, role, d.sched.PriorHolders(task.Paths))
			if agent.Terminal(a.Status) {
				// spawn failed closed (for example an unverifiable snapshot); do not run this agent.
				c.failTask(task, a, fmt.Errorf("%s", a.Activity))
				d.discard(a, a.Activity)
				continue
			}
		}
		release, ok, why := d.sched.Reserve(c.Agents, a)
		if !ok {
			if !queued || wait.why != why {
				c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Queued; x.Activity = why })
			}
			d.queued[task.ID] = waiting{agent: a, why: why}
			continue
		}
		lease, leaseOK, leaseWhy := d.sched.AcquirePaths(task.ID, task.Paths)
		if !leaseOK {
			release()
			if !queued || wait.why != leaseWhy {
				c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Queued; x.Activity = leaseWhy })
			}
			d.queued[task.ID] = waiting{agent: a, why: leaseWhy}
			continue
		}
		delete(d.queued, task.ID)
		updated, err := c.Graph.Update(task.ID, func(x *taskgraph.Task) {
			x.Status = taskgraph.Running
			x.AgentID = a.ID
			x.Started = time.Now().UTC()
			x.Attempt++
		})
		if err != nil {
			release()
			lease()
			c.logWarn("mark task running", err)
			d.discard(a, "The task is gone")
			continue
		}
		c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Starting; x.Phase = "context"; x.Activity = "Starting" })
		c.Agents.Update(a.ID, func(x *agent.Agent) { x.Attempt = updated.Attempt })
		a.Attempt = updated.Attempt
		if queued {
			// The holder may have finished while this task waited, so record its work before the re-cut restacks.
			c.leaseLineage(a, d.sched.PriorHolders(task.Paths))
			c.recutQueued(updated, a)
		}
		if updated.RetryPatch != "" {
			ws := c.workspace()
			applied, err := false, fmt.Errorf("workspace unavailable")
			if ws != nil {
				applied, err = ws.ApplyRetry(a.ID, updated.RetryPatch)
			}
			if err != nil {
				release()
				lease()
				c.announceEscalation(updated)
				c.fail(a.ID, err)
				c.failTask(updated, a, &retryCarryError{err: err})
				continue
			}
			state := "The earlier diff was applied; continue from those changes."
			if !applied {
				state = "The earlier diff conflicted with the new base; starting fresh."
			}
			updated, err = c.Graph.Update(task.ID, func(x *taskgraph.Task) { x.RetryPatch, x.RetryState = "", state })
			if err != nil {
				release()
				lease()
				c.logWarn("record retry diff", err)
				continue
			}
		}
		c.announceEscalation(updated)
		c.emit("task.updated", updated)
		d.launch(updated, a)
	}
	return held
}

// forget retires the waiting agents whose task has been cancelled or taken over since they queued.
func (d *dispatcher) forget(runnable []taskgraph.Task) {
	wanted := make(map[string]bool, len(runnable))
	for _, t := range runnable {
		wanted[t.ID] = true
	}
	for id, wait := range d.queued {
		if !wanted[id] {
			d.discard(wait.agent, "The task no longer needs to run")
			delete(d.queued, id)
		}
	}
}

// recutQueued moves a worktree that waited for a slot onto a base taken now, then re-stacks the dependencies it was
// spawned with. A worktree holds no agent edits yet, so resetting it loses nothing; the user's folder is untouched.
// When a recorded parent's patch no longer hashes to what was stacked, the recut refuses and the worktree keeps its
// old base. That fails closed: no reset runs unless every recorded parent can be re-stacked.
func (c *Coord) recutQueued(t taskgraph.Task, a agent.Agent) {
	ws := c.workspace()
	if ws == nil || ws.Direct() {
		return
	}
	tree, ok := ws.Tree(a.ID)
	if !ok || tree.Direct || tree.Path == "" {
		return
	}
	if _, err := os.Stat(tree.Path); err != nil && !tree.Virtual {
		return // a virtual agent has no directory until it runs, and needs none to be re-cut
	}
	snap, err := ws.Snapshot()
	if err != nil {
		c.noticeSnapshotFailure(err)
		c.logWarn("re-cut queued worktree", err)
		return
	}
	c.noticeSnapshot(snap.HeadFallback, snap.UntrackedTruncated)
	base := ws.Base().Commit
	if base == "" {
		return
	}
	patches, patchDeps, accepted, ok := c.lineagePatches(c.lineageOf(a.ID))
	if !ok {
		c.setLineageProblem(a.ID, "parent_superseded", "a recorded dependency is no longer available; the worktree keeps its old base")
		c.logWarn("re-cut queued worktree", fmt.Errorf("agent %s: a recorded dependency is no longer available; keeping the old base", a.ID))
		return
	}
	if _, err := ws.Recut(a.ID, base); err != nil {
		c.logWarn("re-cut queued worktree", err)
		return
	}
	if len(patches) == 0 {
		c.setLineage(a.ID, accepted)
		return
	}
	results, err := ws.Stack(a.ID, patches)
	if err != nil {
		c.setLineageProblem(a.ID, "parent_superseded", "a recorded dependency could not be re-stacked after the re-cut")
		c.logWarn("re-stack after re-cut", err)
		return
	}
	applied := map[string]bool{}
	for i, r := range results {
		if !r.Applied {
			c.setLineageProblem(a.ID, "parent_superseded", "a recorded dependency did not apply after the re-cut")
			c.logWarn("re-stack after re-cut", fmt.Errorf("patch %d did not apply", i))
			return
		}
		if i < len(patchDeps) {
			applied[patchDeps[i].ID] = true
		}
	}
	lineage := c.lineageOf(a.ID)
	next := make([]Dep, 0, len(lineage))
	for _, dep := range lineage {
		if applied[dep.ID] || containsDep(accepted, dep.ID) {
			next = append(next, dep)
		}
	}
	c.setLineage(a.ID, next)
}

func containsDep(deps []Dep, id string) bool {
	for _, d := range deps {
		if d.ID == id {
			return true
		}
	}
	return false
}

// lineagePatches resolves the patches for recorded dependencies in order. Parents that are already accepted are
// returned separately and skipped: the fresh base has them, and their worktree may be gone. A proposed parent whose
// patch no longer hashes to the recorded value, or a rejected or missing parent, makes the whole recut refuse.
func (c *Coord) lineagePatches(lineage []Dep) (patches []string, patchDeps []Dep, accepted []Dep, ok bool) {
	if len(lineage) == 0 {
		return nil, nil, nil, true
	}
	ws := c.workspace()
	changes := c.Changes()
	for _, dep := range lineage {
		var parent Change
		found := false
		for _, ch := range changes {
			if ch.ID == dep.ID {
				parent, found = ch, true
				break
			}
		}
		if !found {
			return nil, nil, nil, false
		}
		switch parent.Status {
		case "accepted":
			accepted = append(accepted, dep)
			continue
		case "proposed":
		default:
			return nil, nil, nil, false
		}
		patch, err := ws.PatchPath(parent.AgentID, parent.Path)
		if err != nil || strings.TrimSpace(patch) == "" {
			return nil, nil, nil, false
		}
		sum := sha256.Sum256([]byte(patch))
		if hex.EncodeToString(sum[:]) != dep.PatchHash {
			return nil, nil, nil, false
		}
		patches = append(patches, patch)
		patchDeps = append(patchDeps, dep)
	}
	return patches, patchDeps, accepted, true
}

func (d *dispatcher) launch(t taskgraph.Task, a agent.Agent) {
	c, ctx := d.c, d.ctx
	d.mu.Lock()
	d.running[t.ID] = true
	d.mu.Unlock()
	d.wg.Add(1)
	d.started++
	go func() {
		defer func() {
			// The slot is free before anything is told, so a dispatcher that wakes finds it.
			d.sched.ReleasePaths(t.ID)
			d.sched.Unreserve(a.ID)
			d.mu.Lock()
			delete(d.running, t.ID)
			d.mu.Unlock()
			select {
			case d.wake <- struct{}{}:
			default:
			}
			d.wg.Done()
		}()
		err := c.guard("worker "+t.ID, func() error { return c.execWorker(ctx, t, a) })
		if err != nil && strings.HasPrefix(err.Error(), "internal error") {
			// A panic skipped the worker's own bookkeeping: close the task here so the run can end.
			c.fail(a.ID, err)
			c.failTask(t, a, err)
		}
		c.clearSucceededRetry(t.ID, a.ID)
		c.Agents.Update(a.ID, func(x *agent.Agent) {
			if err != nil && ctx.Err() != nil {
				x.Status, x.Activity = agent.Cancelled, "Stopped"
			} else if err != nil {
				x.Status, x.Activity = agent.Failed, err.Error()
			}
		})
		if current, ok := c.Agents.Get(a.ID); ok && (current.Status == agent.Cancelled || current.Status == agent.Failed || current.Status == agent.Interrupted) {
			c.finishPeerTurn(c.SessionID(), a.ID, "recipient is "+string(current.Status))
		}
		if err != nil {
			d.mu.Lock()
			d.errs = append(d.errs, err)
			d.mu.Unlock()
		}
	}()
}

// dropQueued retires the agents that were still waiting when the round ended. They cannot start now, and the next
// round makes new ones; left queued they would show as waiting for ever and count as busy.
func (d *dispatcher) dropQueued() {
	for id, wait := range d.queued {
		d.discard(wait.agent, "Not started: "+wait.why)
		delete(d.queued, id)
	}
}

// discard retires an agent that never ran.
func (d *dispatcher) discard(a agent.Agent, note string) {
	d.c.Agents.Update(a.ID, func(x *agent.Agent) {
		if !agent.Terminal(x.Status) {
			x.Status, x.Activity = agent.Cancelled, note
		}
	})
	d.c.finishPeerTurn(d.c.SessionID(), a.ID, note)
	if ws := d.c.workspace(); ws != nil {
		_ = ws.Release(a.ID)
	}
}
