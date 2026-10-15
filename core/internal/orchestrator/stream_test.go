package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	rt "github.com/relocode/relo/internal/runtime"
)

// scriptRuntime plays a fixed event sequence for every task.
type scriptRuntime struct {
	dispatchRuntime
	script []rt.Event
}

func (r *scriptRuntime) SendTask(_ context.Context, _ rt.SessionID, task rt.AgentTask) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()
	go func() {
		for _, ev := range r.script {
			r.events <- ev
		}
	}()
	return nil
}

type captured struct {
	mu  sync.Mutex
	evs []struct {
		name    string
		payload any
	}
}

func (k *captured) emit(name string, payload any) {
	k.mu.Lock()
	k.evs = append(k.evs, struct {
		name    string
		payload any
	}{name, payload})
	k.mu.Unlock()
}

func (k *captured) named(name string) []any {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []any
	for _, e := range k.evs {
		if e.name == name {
			out = append(out, e.payload)
		}
	}
	return out
}

// primaryOf finds the Primary even after it was cancelled (Registry.Primary hides cancelled ones).
func primaryOf(c *Coord) agent.Agent {
	for _, a := range c.Agents.List() {
		if a.Role == agent.Primary {
			return a
		}
	}
	return agent.Agent{}
}

func newScripted(script ...rt.Event) (*Coord, *captured) {
	c := New(logging.New(nil, logging.Error))
	r := &scriptRuntime{dispatchRuntime: *newDispatchRuntime(), script: script}
	c.SetRuntime(r)
	k := &captured{}
	c.SetEmitter(k.emit)
	return c, k
}

const turnJSON = `{"status":"completed","userMessage":"All done.","tasks":[]}`

func TestSubmitEmitsUserMessageAndSettlesPrimary(t *testing.T) {
	c, k := newScripted(
		rt.Event{Type: "session.reasoning.started", Phase: "reasoning"},
		rt.Event{Type: "session.reasoning.delta", Phase: "reasoning", Delta: ""},
		rt.Event{Type: "session.text.delta", Phase: "writing", Delta: `{"status":"completed","userMessage":"All`},
		rt.Event{Type: "session.text.delta", Phase: "writing", Delta: ` done.","tasks":[]}`},
		rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: turnJSON},
	)
	c.CreateSession(t.TempDir(), "", ModeBuild)
	if err := c.Submit(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	msgs := k.named("chat.message")
	if len(msgs) < 2 {
		t.Fatalf("want user and assistant chat.message, got %d", len(msgs))
	}
	if first := msgs[0].(Chat); first.Role != "user" || first.Text != "hi" {
		t.Fatalf("user message not announced first: %+v", first)
	}
	if c.Busy() {
		t.Fatal("Primary stayed active after a successful turn")
	}
	if s := c.Current(); s.Running {
		t.Fatal("session still marked running")
	}
	p, _ := c.Agents.Primary()
	if p.Status != agent.Completed || p.Phase != "" || p.EndedAt == 0 || p.StartedAt == 0 {
		t.Fatalf("primary not settled: %+v", p)
	}
	// Reply text streams as cumulative text, and only the userMessage value.
	var last map[string]any
	for _, d := range k.named("chat.delta") {
		last = d.(map[string]any)
	}
	if last == nil || last["text"] != "All done." {
		t.Fatalf("streamed reply = %v", last)
	}
}

func TestReasoningNeverReachesReplyBody(t *testing.T) {
	c, k := newScripted(
		rt.Event{Type: "session.reasoning.delta", Phase: "reasoning", Delta: "thinking hard"},
		rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: turnJSON},
	)
	s := c.CreateSession(t.TempDir(), "x", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	var body strings.Builder
	st := &runState{tools: map[string]string{}, inputs: map[string]string{}}
	c.handleEvent(context.Background(), a, rt.Event{Type: "session.reasoning.delta", Phase: "reasoning", Delta: "thinking hard"}, &body, st)
	if body.Len() != 0 || len(k.named("chat.delta")) != 0 {
		t.Fatalf("reasoning leaked: body=%q deltas=%d", body.String(), len(k.named("chat.delta")))
	}
	if cur, _ := c.Agents.Get(a.ID); cur.Phase != "reasoning" || cur.Activity != "Reasoning" {
		t.Fatalf("phase not tracked: %+v", cur)
	}
}

func TestToolNameTrackedByID(t *testing.T) {
	c, _ := newScripted()
	s := c.CreateSession(t.TempDir(), "x", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	var body strings.Builder
	st := &runState{tools: map[string]string{}, inputs: map[string]string{}}
	h := func(ev rt.Event) { c.handleEvent(context.Background(), a, ev, &body, st) }
	h(rt.Event{Type: "session.tool.input.started", Phase: "tool", Tool: "grep", ToolID: "t1"})
	h(rt.Event{Type: "session.tool.called", Phase: "tool", ToolID: "t1"}) // no name on this event
	cur, _ := c.Agents.Get(a.ID)
	if cur.Activity != "grep" || cur.Tools != 1 || cur.Phase != "tool" {
		t.Fatalf("tool tracking: %+v", cur)
	}
	h(rt.Event{Type: "session.tool.success", Phase: "model", ToolID: "t1", ToolDone: true})
	cur, _ = c.Agents.Get(a.ID)
	if cur.Phase != "model" || cur.Tools != 1 {
		t.Fatalf("after tool: %+v", cur)
	}
}

func TestStopShowsStoppedNotAnError(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newBlockingRuntime()
	c.SetRuntime(r)
	k := &captured{}
	c.SetEmitter(k.emit)
	c.CreateSession(t.TempDir(), "", ModeBuild)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "go", nil) }()
	<-r.started
	c.StopAll(context.Background())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Submit did not return after Stop")
	}
	p := primaryOf(c)
	if p.Status != agent.Cancelled || p.Activity != "Stopped" {
		t.Fatalf("primary after stop: %+v", p)
	}
	for _, m := range c.Current().Messages {
		if m.Role == "error" {
			t.Fatalf("stop reported as error: %+v", m)
		}
	}
	if c.Busy() || c.Current().Running {
		t.Fatal("still busy after stop")
	}
}

func TestProviderErrorBecomesOneErrorChat(t *testing.T) {
	c, k := newScripted(rt.Event{Type: "session.execution.failed", Done: true, Error: "free tier only inside OpenCode"})
	c.CreateSession(t.TempDir(), "", ModeBuild)
	if err := c.Submit(context.Background(), "hi", nil); err == nil {
		t.Fatal("want error")
	}
	var errs int
	for _, m := range c.Current().Messages {
		if m.Role == "error" {
			errs++
			if !strings.Contains(m.Text, "free tier") {
				t.Fatalf("error text: %q", m.Text)
			}
		}
	}
	if errs != 1 {
		t.Fatalf("want exactly one error chat, got %d", errs)
	}
	p := primaryOf(c)
	if p.Status != agent.Failed || !strings.Contains(p.Activity, "free tier") {
		t.Fatalf("primary: %+v", p)
	}
	failed := k.named("agent.failed")
	if len(failed) == 0 {
		t.Fatal("no agent.failed")
	}
	if full, ok := failed[len(failed)-1].(agent.Agent); !ok || full.Status != agent.Failed {
		t.Fatalf("agent.failed must carry the full agent with status: %#v", failed[len(failed)-1])
	}
}

func TestInvalidTurnIsNotReportedTwice(t *testing.T) {
	c, _ := newScripted(rt.Event{Type: "session.idle", Done: true, Idle: true, JSON: `{}`})
	c.CreateSession(t.TempDir(), "", ModeBuild)
	_ = c.Submit(context.Background(), "hi", nil)
	var shown int
	for _, m := range c.Current().Messages {
		if m.Role != "user" {
			shown++
		}
	}
	if shown != 1 {
		t.Fatalf("invalid-turn failure shown %d times: %+v", shown, c.Current().Messages)
	}
}

func TestRegistryAnnouncesEveryChangeWithRisingRev(t *testing.T) {
	c, k := newScripted()
	s := c.CreateSession(t.TempDir(), "", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Running; x.Phase = "reasoning" })
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Completed })
	got := k.named("agent.updated")
	if len(got) != 2 {
		t.Fatalf("agent.updated count = %d", len(got))
	}
	r1, r2 := got[0].(agent.Agent), got[1].(agent.Agent)
	if r2.Rev <= r1.Rev || r2.Phase != "" || r2.EndedAt == 0 || r1.StartedAt == 0 {
		t.Fatalf("stamping: %+v then %+v", r1, r2)
	}
}

func TestSessionUsesNewRegistryHook(t *testing.T) {
	c, k := newScripted()
	c.CreateSession(t.TempDir(), "a", ModeBuild)
	s := c.CreateSession(t.TempDir(), "b", ModeBuild) // replaces the registry
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Activity = "x" })
	if len(k.named("agent.updated")) != 1 {
		t.Fatal("replacement registry lost its change hook")
	}
}

func TestEarlySubmitRefusalIsVisible(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	if err := c.Submit(context.Background(), "hi", nil); err == nil {
		t.Fatal("want error without runtime")
	}
	ms := c.Current().Messages
	if len(ms) != 1 || ms[0].Role != "system" {
		t.Fatalf("user sees nothing: %+v", ms)
	}
}

func TestPrimaryStreamIsDiscardedOnInvalidOutput(t *testing.T) {
	c, k := newScripted()
	r := c.RuntimeSnapshot().(*scriptRuntime)
	r.script = []rt.Event{
		{Type: "session.text.delta", Phase: "writing", Delta: `{"userMessage":"half`},
		{Type: "session.idle", Done: true, Idle: true, JSON: `{}`},
	}
	s := c.CreateSession(t.TempDir(), "x", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	_, _ = c.primaryTurn(context.Background(), a, prompt.Parts{}, "plan")
	if len(k.named("chat.discard")) == 0 {
		t.Fatal("half-written bubble never discarded")
	}
}
