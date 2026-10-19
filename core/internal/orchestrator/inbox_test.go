package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/routing"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/workspace"
)

// headWrap is a head turn that has nothing left to do.
const headWrap = `{"status":"completed","userMessage":"All done.","tasks":[]}`

// planOneWorker is a head turn that plans one worker task, so the head ends up waiting on a sub agent.
const planOneWorker = `{"status":"working","userMessage":"Starting the work.","tasks":[{"id":"t1","title":"w1","role":"worker","description":"do it","acceptance":["done"],"priority":1,"deps":[],"difficulty":"medium"}]}`

// guidanceRuntime answers head calls from a scripted list, records every head prompt, and holds sub agent
// answers on their gates exactly like gatedRuntime does.
type guidanceRuntime struct {
	*gatedRuntime
	hmu       sync.Mutex
	replies   []string
	headSeen  []string
	headCalls chan string
	holdFirst chan struct{} // when set, the first head call waits for it (or for cancellation)
	failFirst error         // when set, the first head call fails after its gate opens
}

func (r *guidanceRuntime) SendTask(ctx context.Context, sid rt.SessionID, task rt.AgentTask) error {
	if task.Role != string(agent.Primary) {
		return r.gatedRuntime.SendTask(ctx, sid, task)
	}
	r.hmu.Lock()
	i := len(r.headSeen)
	r.headSeen = append(r.headSeen, task.Prompt)
	reply := headWrap
	if i < len(r.replies) {
		reply = r.replies[i]
	}
	hold := r.holdFirst
	r.hmu.Unlock()
	r.headCalls <- task.Prompt
	if i == 0 && hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if i == 0 && r.failFirst != nil {
		return r.failFirst
	}
	r.gatedRuntime.mu.Lock()
	ch := r.events[sid]
	r.gatedRuntime.mu.Unlock()
	ch <- rt.Event{Type: "session.idle", Idle: true, Done: true, JSON: reply, Text: reply}
	return nil
}

// newGuidanceCoord is a non-Git workspace whose head answers with the given turn replies and whose sub agents
// wait on their gates until released.
func newGuidanceCoord(t *testing.T, replies ...string) (*Coord, *guidanceRuntime, *captured) {
	t.Helper()
	dir := t.TempDir()
	r := &guidanceRuntime{gatedRuntime: newGatedRuntime(), replies: replies, headCalls: make(chan string, 8)}
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	if _, err := c.WS.Snapshot(); err != nil {
		t.Fatal(err)
	}
	c.SetRuntime(r)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Settings.PrimaryModel, c.Settings.Tier1Model, c.Settings.Tier2Model = "mock/p", "mock/t1", "mock/t2"
	c.Route = routing.Config{Primary: "mock/p", Tier1: "mock/t1", Tier2: "mock/t2"}
	c.Sched = scheduler.New(scheduler.Limits{MaxTotal: 4, MaxTier1: 2, MaxTier2: 2, MaxDepth: 2, MaxRetry: 1})
	k := &captured{}
	c.SetEmitter(k.emit)
	c.CreateSession(dir, "guide", ModeBuild)
	return c, r, k
}

func workerAgents(c *Coord) []agent.Agent {
	var out []agent.Agent
	for _, a := range c.Agents.List() {
		if a.Role == agent.Worker {
			out = append(out, a)
		}
	}
	return out
}

// Submit while a request runs queues the message instead of refusing it, and the message keeps its queued
// mark until the head takes it at its next step.
func TestSubmitWhileRunningQueuesTheMessage(t *testing.T) {
	c, r, k := newGuidanceCoord(t, planOneWorker, headWrap, headWrap)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	nextBegun(t, r.gatedRuntime) // the worker runs and is held on its gate; the head waits on it

	if err := c.Submit(context.Background(), "second", nil); err != nil {
		t.Fatalf("submit while running = %v, want success", err)
	}
	var queued Chat
	for _, ev := range k.named("chat.message") {
		if m := ev.(Chat); m.Text == "second" && m.Queued {
			queued = m
		}
	}
	if queued.Role != "user" || queued.ID == "" {
		t.Fatalf("queued message = %+v, want a user message with queued=true", queued)
	}
	for _, m := range c.Current().Messages {
		if m.Role == "system" && strings.Contains(m.Text, "Still working") {
			t.Fatal("the old refusal was still posted")
		}
		if strings.Contains(m.Text, "already running") {
			t.Fatalf("refusal chat leaked: %+v", m)
		}
	}

	r.release("w1")
	driveEnds(t, done)

	var delivered []Chat
	for _, ev := range k.named("chat.message") {
		if m := ev.(Chat); m.Text == "second" {
			delivered = append(delivered, m)
		}
	}
	if len(delivered) != 2 || !delivered[0].Queued || delivered[1].Queued || delivered[0].ID != delivered[1].ID {
		t.Fatalf("delivery chat = %+v, want queued=true then queued=false for one message ID", delivered)
	}
	var inTranscript []Chat
	for _, m := range c.Current().Messages {
		if m.Text == "second" {
			inTranscript = append(inTranscript, m)
		}
	}
	if len(inTranscript) != 1 || inTranscript[0].Queued {
		t.Fatalf("transcript copy = %+v, want one delivered message", inTranscript)
	}
}

// A head waiting on its sub agents steps at once when guidance arrives; the sub agents are not cancelled.
func TestQueuedGuidanceWakesHeadWaitingOnSubAgents(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, planOneWorker, headWrap, headWrap)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	<-r.headCalls // the plan turn
	nextBegun(t, r.gatedRuntime)

	start := time.Now()
	if err := c.Submit(context.Background(), "use tabs, not spaces", nil); err != nil {
		t.Fatalf("submit while running = %v", err)
	}
	var prompt string
	select {
	case prompt = <-r.headCalls:
	case <-time.After(3 * time.Second):
		t.Fatal("the head did not step on the queued guidance")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("the head woke after %v, want within about a second", elapsed)
	}
	if !strings.Contains(prompt, "use tabs, not spaces") {
		t.Fatalf("head step missed the queued text: %q", prompt)
	}
	workers := workerAgents(c)
	if len(workers) != 1 || !activeStatus(workers[0].Status) {
		t.Fatalf("sub agents = %+v, want the one worker still running", workers)
	}

	r.release("w1")
	driveEnds(t, done)
}

// Guidance that arrives too late for the turn in flight is delivered by a follow-up head turn.
func TestQueuedGuidanceStartsFollowUpTurnWhenTurnEnds(t *testing.T) {
	c, r, k := newGuidanceCoord(t, headWrap, headWrap)
	r.holdFirst = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	<-r.headCalls // the head turn is in flight

	if err := c.Submit(context.Background(), "also rename the config afterwards", nil); err != nil {
		t.Fatalf("submit while running = %v", err)
	}
	close(r.holdFirst) // the turn ends with no tasks; the guidance is still queued

	var prompt string
	select {
	case prompt = <-r.headCalls:
	case <-time.After(3 * time.Second):
		t.Fatal("no follow-up head turn for the queued guidance")
	}
	if !strings.Contains(prompt, "also rename the config afterwards") {
		t.Fatalf("follow-up turn missed the queued text: %q", prompt)
	}
	driveEnds(t, done)

	var found bool
	for _, m := range c.Current().Messages {
		if m.Text == "also rename the config afterwards" {
			found = true
			if m.Queued {
				t.Fatalf("queued mark kept after delivery: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("the queued message is not in the transcript")
	}
	if n := len(k.named("chat.message")); n < 3 {
		t.Fatalf("chat.message events = %d, want queued message plus delivery", n)
	}
}

func TestFailedRunPreservesUndeliveredGuidance(t *testing.T) {
	c, r, k := newGuidanceCoord(t, headWrap)
	r.holdFirst = make(chan struct{})
	r.failFirst = errors.New("provider unavailable")
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	<-r.headCalls
	const guidance = "keep the public API unchanged"
	if err := c.Submit(context.Background(), guidance, []string{"api.go"}); err != nil {
		t.Fatal(err)
	}
	close(r.holdFirst)
	if err := <-done; err == nil {
		t.Fatal("expected the first run to fail")
	}
	if c.queuedPending(c.SessionID()) {
		t.Fatal("failed run left guidance waiting in an inactive inbox")
	}
	var updated, notified bool
	for _, ev := range k.named("chat.message") {
		m := ev.(Chat)
		if m.Text == guidance && !m.Queued {
			updated = true
		}
		if m.Role == "system" && strings.Contains(m.Text, "queued message") && strings.Contains(m.Text, "next request") {
			notified = true
		}
	}
	if !updated || !notified {
		t.Fatalf("undelivered guidance: cleared=%v, explained=%v", updated, notified)
	}
	if err := c.Submit(context.Background(), "retry", nil); err != nil {
		t.Fatal(err)
	}
	r.hmu.Lock()
	prompt := r.headSeen[len(r.headSeen)-1]
	r.hmu.Unlock()
	if !strings.Contains(prompt, guidance) || !strings.Contains(prompt, "api.go") {
		t.Fatalf("next request lost undelivered guidance or references: %s", prompt)
	}
}

func TestGuidanceArrivingDuringRunShutdownIsPreserved(t *testing.T) {
	c, r, k := newGuidanceCoord(t, headWrap)
	const guidance = "also preserve the existing command names"
	var queued bool
	c.SetEmitter(func(name string, payload any) {
		k.emit(name, payload)
		if a, ok := payload.(agent.Agent); name == "agent.updated" && ok && a.Role == agent.Primary && a.Status == agent.Completed && !queued {
			queued = true
			if err := c.Submit(context.Background(), guidance, nil); err != nil {
				t.Errorf("late Submit: %v", err)
			}
		}
	})
	if err := c.Submit(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if !queued || c.queuedPending(c.SessionID()) || c.Current().Running {
		t.Fatalf("shutdown state: guidance queued=%v, pending=%v, running=%v", queued, c.queuedPending(c.SessionID()), c.Current().Running)
	}
	if err := c.Submit(context.Background(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	r.hmu.Lock()
	prompt := r.headSeen[len(r.headSeen)-1]
	r.hmu.Unlock()
	if !strings.Contains(prompt, guidance) {
		t.Fatalf("shutdown race lost the user's guidance: %s", prompt)
	}
}

func TestQueuedGuidanceFollowsOriginalMessageInChronologicalOrder(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, headWrap)
	p := c.ensurePrimary()
	c.seedPrimary("use spaces", nil)
	c.mu.Lock()
	c.enqueueLocked("actually use tabs", nil)
	c.mu.Unlock()
	if err := c.loopPrimary(context.Background(), p, "use spaces"); err != nil {
		t.Fatal(err)
	}
	r.hmu.Lock()
	prompt := r.headSeen[0]
	r.hmu.Unlock()
	if !strings.Contains(prompt, "User message:\nuse spaces\n\nactually use tabs") {
		t.Fatalf("newer guidance did not follow the original message: %s", prompt)
	}
}

func TestStoppedGuidanceDoesNotBecomeFutureInstructions(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, headWrap)
	r.holdFirst = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	<-r.headCalls
	const dropped = "remove all tests after finishing"
	if err := c.Submit(context.Background(), dropped, nil); err != nil {
		t.Fatal(err)
	}
	c.StopAll(context.Background())
	<-done
	if err := c.Submit(context.Background(), "continue without the stopped guidance", nil); err != nil {
		t.Fatal(err)
	}
	r.hmu.Lock()
	prompt := r.headSeen[len(r.headSeen)-1]
	r.hmu.Unlock()
	if strings.Contains(prompt, dropped) {
		t.Fatalf("stopped guidance was resurrected as a requirement: %s", prompt)
	}
	for _, m := range c.Current().Messages {
		if m.Text == dropped && !m.Dropped {
			t.Fatal("stopped guidance lost its delivery state")
		}
	}
}

// Stop drops what never reached the head, clears its queued mark and says so in a system note.
func TestStopDropsQueuedMessages(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, headWrap)
	r.holdFirst = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	<-r.headCalls // the head turn is in flight

	if err := c.Submit(context.Background(), "never delivered note", nil); err != nil {
		t.Fatalf("submit while running = %v", err)
	}
	c.StopAll(context.Background())

	select {
	case p := <-r.headCalls:
		t.Fatalf("the head got %q after Stop", p)
	default:
	}
	if c.queuedPending(c.SessionID()) {
		t.Fatal("the inbox still holds messages after Stop")
	}
	var note, kept bool
	for _, m := range c.Current().Messages {
		if m.Text == "never delivered note" {
			kept = true
			if m.Queued {
				t.Fatalf("queued mark kept after Stop: %+v", m)
			}
		}
		if m.Role == "system" && strings.Contains(m.Text, "queued message") {
			note = true
		}
	}
	if !kept {
		t.Fatal("the dropped message vanished from the transcript")
	}
	if !note {
		t.Fatal("no system note about the dropped message")
	}
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v, want context.Canceled", err)
	}
}

// The inbox is keyed by session ID: a message queued on one session is never handed to another.
func TestQueuedInboxIsScopedToItsSession(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, headWrap)
	sid := c.SessionID()
	c.mu.Lock()
	queued := c.enqueueLocked("only for this session", nil)
	c.mu.Unlock()

	if _, _, ok := c.takeQueued("sess_other"); ok {
		t.Fatal("another session took this session's queued message")
	}
	if !c.queuedPending(sid) {
		t.Fatal("the message was lost when another session looked")
	}
	text, _, ok := c.takeQueued(sid)
	if !ok || text != "only for this session" {
		t.Fatalf("takeQueued = %q, %v", text, ok)
	}
	for _, m := range c.Current().Messages {
		if m.ID == queued.ID && m.Queued {
			t.Fatal("queued mark kept after delivery")
		}
	}

	// A session change drops what was waiting; the next session never sees it.
	c.mu.Lock()
	c.enqueueLocked("stale note for the old session", nil)
	c.mu.Unlock()
	old := sid
	c.CreateSession(t.TempDir(), "fresh", ModeBuild)
	if c.queuedPending(old) {
		t.Fatal("the replaced session kept its inbox")
	}
	if err := c.Submit(context.Background(), "fresh request", nil); err != nil {
		t.Fatal(err)
	}
	r.hmu.Lock()
	defer r.hmu.Unlock()
	for _, p := range r.headSeen {
		if strings.Contains(p, "stale note") {
			t.Fatalf("a queued message crossed sessions: %q", p)
		}
	}
	for _, m := range c.Current().Messages {
		if strings.Contains(m.Text, "stale note") {
			t.Fatalf("a queued message crossed sessions: %+v", m)
		}
	}
}
