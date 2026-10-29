package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/runtime"
)

type wakeRuntime struct {
	*runtime.Mock
	config runtime.SessionConfig
	task   runtime.AgentTask
	events chan runtime.Event
	fail   bool
}

func (r *wakeRuntime) StartSession(_ context.Context, config runtime.SessionConfig) (runtime.SessionID, error) {
	r.config = config
	r.events = make(chan runtime.Event, 1)
	return "wake", nil
}
func (r *wakeRuntime) Events(context.Context, runtime.SessionID) (<-chan runtime.Event, error) {
	return r.events, nil
}
func (r *wakeRuntime) SendTask(_ context.Context, _ runtime.SessionID, task runtime.AgentTask) error {
	r.task = task
	if r.fail {
		return errors.New("wake failed")
	}
	var input struct {
		Messages []PeerMessage `json:"messages"`
	}
	start := strings.Index(task.Prompt, "Context: ")
	if start < 0 {
		return errors.New("missing context")
	}
	if err := json.Unmarshal([]byte(task.Prompt[start+len("Context: "):]), &input); err != nil {
		return err
	}
	var replies []peerWakeReply
	for _, m := range input.Messages {
		replies = append(replies, peerWakeReply{To: m.From, InReplyTo: m.ID, Text: "Here is the answer."})
	}
	body, _ := json.Marshal(map[string]any{"replies": replies})
	r.events <- runtime.Event{Done: true, JSON: string(body)}
	return nil
}
func (r *wakeRuntime) CloseSession(context.Context, runtime.SessionID) error { return nil }

func wakeDispatcher(c *Coord) *dispatcher {
	return &dispatcher{c: c, ctx: context.Background(), wake: make(chan struct{}, 1), running: map[string]bool{}, waking: map[string]bool{}}
}

func TestPeerWakeRepliesAndRemainsReadOnly(t *testing.T) {
	c, _, sender, target, key := newPeerCoord(t)
	r := &wakeRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(r)
	c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	m, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "Where is it?"})
	if err != nil {
		t.Fatal(err)
	}
	d := wakeDispatcher(c)
	d.wakePeers()
	d.wg.Wait()
	messages := c.Current().PeerMessages
	if len(messages) != 2 || messages[0].State != "read" || messages[1].To != sender.ID || messages[1].ReplyTo != m.(PeerMessage).ID {
		t.Fatalf("messages: %+v", messages)
	}
	if !r.config.ReadOnly || len(r.task.Tools) != 1 || r.task.Tools["*"] {
		t.Fatalf("wake policy: %+v %+v", r.config, r.task.Tools)
	}
	if a, _ := c.Agents.Get(target.ID); a.Status != agent.Completed {
		t.Fatalf("status = %s", a.Status)
	}
	d.wakePeers()
	d.wg.Wait()
	if c.Current().PeerWakes[target.ID] != 1 {
		t.Fatal("reply caused another wake")
	}
}

func TestPeerWakeFailureAndCapsSettle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fail   bool
		counts map[string]int
	}{
		{"failure", true, nil}, {"agent cap", false, map[string]int{"target": 2}}, {"session cap", false, map[string]int{"other": 40}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _, target, key := newPeerCoord(t)
			c.SetRuntime(&wakeRuntime{Mock: runtime.NewMock(), fail: tc.fail})
			c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.ReviewReq })
			c.mu.Lock()
			c.sess.PeerWakes = tc.counts
			c.mu.Unlock()
			if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "question"}); err != nil {
				t.Fatal(err)
			}
			d := wakeDispatcher(c)
			d.wakePeers()
			d.wg.Wait()
			s := c.Current()
			if s.PeerMessages[0].State != "undelivered" || s.PeerMessages[0].Reason == "" || len(s.Messages) < 2 {
				t.Fatalf("not settled with notice: %+v", s.PeerMessages)
			}
		})
	}
}

func TestPeerDiscardAndCancelledSettle(t *testing.T) {
	for _, discard := range []bool{false, true} {
		c, _, _, target, key := newPeerCoord(t)
		c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Queued })
		if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "question"}); err != nil {
			t.Fatal(err)
		}
		if discard {
			wakeDispatcher(c).discard(target, "Not started: no slot")
		} else {
			c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Cancelled })
			c.finishPeerTurn(c.SessionID(), target.ID, "recipient is cancelled")
		}
		s := c.Current()
		if s.PeerMessages[0].State != "undelivered" || s.PeerMessages[0].Reason == "" || len(s.Messages) < 2 {
			t.Fatalf("not settled: %+v", s.PeerMessages)
		}
	}
}

func TestPeerCancelledDeliveredMessageSettles(t *testing.T) {
	c, _, sender, target, _ := newPeerCoord(t)
	c.mu.Lock()
	c.sess.PeerMessages = append(c.sess.PeerMessages, PeerMessage{ID: "delivered", From: sender.ID, To: target.ID, State: "delivered"})
	c.mu.Unlock()
	c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Cancelled })
	c.finishPeerTurn(c.SessionID(), target.ID, "recipient is cancelled")
	if m := c.Current().PeerMessages[0]; m.State != "undelivered" || m.Reason == "" {
		t.Fatalf("message = %+v", m)
	}
}

func TestPeerWakeDeliveredAndPrimaryExcluded(t *testing.T) {
	c, _, sender, target, _ := newPeerCoord(t)
	r := &wakeRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(r)
	c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	p := c.Agents.Create(agent.Agent{ID: "primary-done", Role: agent.Primary, Status: agent.Completed})
	c.mu.Lock()
	c.sess.PeerMessages = append(c.sess.PeerMessages,
		PeerMessage{ID: "missed", From: sender.ID, To: target.ID, State: "delivered", Text: "question"},
		PeerMessage{ID: "primary-message", From: sender.ID, To: p.ID, State: "delivered", Text: "question"})
	c.mu.Unlock()
	d := wakeDispatcher(c)
	d.wakePeers()
	d.wg.Wait()
	s := c.Current()
	if s.PeerMessages[0].State != "read" || s.PeerMessages[1].State != "delivered" || s.PeerWakes[p.ID] != 0 {
		t.Fatalf("unexpected wakes: %+v %+v", s.PeerMessages, s.PeerWakes)
	}
}

func TestPeerSendToCancelledSettlesImmediately(t *testing.T) {
	c, _, _, target, key := newPeerCoord(t)
	c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Cancelled })
	got, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "question"})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(PeerMessage)
	if m.State != "undelivered" || m.Reason == "" || len(c.Current().Messages) < 2 {
		t.Fatalf("message = %+v", m)
	}
}

func poolHas(c *Coord, agentID, text string) bool {
	for _, it := range c.Engine.SnapshotPool(agentID) {
		if strings.Contains(it.Content, text) {
			return true
		}
	}
	return false
}

func TestReplyToFinishedSenderShownNotQueued(t *testing.T) {
	c, _, sender, target, key := newPeerCoord(t)
	c.SetRuntime(&wakeRuntime{Mock: runtime.NewMock()})
	primary := c.Agents.Create(agent.Agent{ID: "primary", Role: agent.Primary, Status: agent.Running})
	c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "Where is it?"}); err != nil {
		t.Fatal(err)
	}
	c.Agents.Update(sender.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	d := wakeDispatcher(c)
	d.wakePeers()
	d.wg.Wait()
	s := c.Current()
	if len(s.PeerMessages) != 2 || s.PeerMessages[1].State != "delivered" || s.PeerMessages[1].ReplyTo == "" {
		t.Fatalf("reply not settled delivered: %+v", s.PeerMessages)
	}
	inChat := false
	for _, m := range s.Messages {
		if m.AgentID == target.ID && strings.Contains(m.Text, "Here is the answer.") {
			inChat = true
		}
	}
	if !inChat || !poolHas(c, primary.ID, "Here is the answer.") {
		t.Fatalf("reply missing from chat (%v) or Primary context", inChat)
	}
}

func TestFinishedPairGetsOneWakeRound(t *testing.T) {
	c, _, sender, target, _ := newPeerCoord(t)
	c.SetRuntime(&wakeRuntime{Mock: runtime.NewMock()})
	c.Agents.Update(sender.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	c.mu.Lock()
	c.sess.PeerMessages = append(c.sess.PeerMessages, PeerMessage{ID: "ask", From: sender.ID, To: target.ID, State: "delivered", Text: "question"})
	c.mu.Unlock()
	for pass := 0; pass < 3; pass++ {
		d := wakeDispatcher(c)
		d.wakePeers()
		d.wg.Wait()
	}
	s := c.Current()
	if s.PeerWakes[target.ID] != 1 || s.PeerWakes[sender.ID] != 0 || len(s.PeerMessages) != 2 {
		t.Fatalf("wakes = %+v, messages = %+v", s.PeerWakes, s.PeerMessages)
	}
	if s.PeerMessages[1].State != "delivered" || s.PeerMessages[1].ReplyTo != "ask" {
		t.Fatalf("reply = %+v", s.PeerMessages[1])
	}
}

func TestSettledMessagesNeverFillQueue(t *testing.T) {
	c, _, _, target, key := newPeerCoord(t)
	send := func() error {
		_, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "question"})
		return err
	}
	c.mu.Lock()
	for i := 0; i < maxPeerPending; i++ {
		c.sess.PeerMessages = append(c.sess.PeerMessages, PeerMessage{ID: "old" + string(rune('a'+i)), From: "x", To: target.ID, State: "delivered", Text: "old"})
	}
	c.mu.Unlock()
	if err := send(); err != nil {
		t.Fatalf("settled-by-state messages counted as pending: %v", err)
	}
	c.mu.Lock()
	c.sess.PeerMessages = nil
	for i := 0; i < maxPeerMessages; i++ {
		state := []string{"read", "undelivered", "delivered"}[i%3]
		c.sess.PeerMessages = append(c.sess.PeerMessages, PeerMessage{ID: "s" + string(rune('a'+i%26)) + string(rune('a'+i/26)), From: "x", To: "other", State: state, ReplyTo: "r"})
	}
	c.mu.Unlock()
	if err := send(); err != nil {
		t.Fatalf("settled messages filled queue: %v", err)
	}
	if n := len(c.Current().PeerMessages); n != maxPeerMessages {
		t.Fatalf("messages = %d, want %d", n, maxPeerMessages)
	}
	c.mu.Lock()
	for i := range c.sess.PeerMessages {
		c.sess.PeerMessages[i].State = "queued"
	}
	c.mu.Unlock()
	if err := send(); err == nil || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("active queue not reported full: %v", err)
	}
}
