package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/runtime"
)

type peerRuntime struct {
	*runtime.Mock
	mu         sync.Mutex
	deliveries []string
	err        error
}

func (r *peerRuntime) ConfigureMessages(context.Context, string, runtime.MessageTools) error {
	return nil
}
func (r *peerRuntime) DeliverMessage(_ context.Context, _ runtime.SessionID, _, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries = append(r.deliveries, text)
	return r.err
}

func newPeerCoord(t *testing.T) (*Coord, *peerRuntime, agent.Agent, agent.Agent, string) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	r := &peerRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(r)
	c.CreateSession(t.TempDir(), "coordinate", ModeBuild)
	sender := c.Agents.Create(agent.Agent{ID: "sender", Role: agent.Worker, Status: agent.Running})
	target := c.Agents.Create(agent.Agent{ID: "target", Role: agent.Worker, Status: agent.Running})
	c.runtimeSessions[sender.ID] = &runtimeSession{id: "runtime-sender"}
	key, err := c.bindPeer(sender, "runtime-sender")
	if err != nil {
		t.Fatal(err)
	}
	return c, r, sender, target, key
}

func TestPeerMessagingBindsSenderAndListsOnlyLivePeers(t *testing.T) {
	c, _, sender, target, key := newPeerCoord(t)
	c.Agents.Create(agent.Agent{ID: "finished", Status: agent.Completed})
	v, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), target.ID) || strings.Contains(string(b), "finished") || strings.Contains(string(b), key) {
		t.Fatalf("invalid peer roster: %s", b)
	}
	v, err = c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "Use Export(ctx), not Export()."})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(PeerMessage)
	if m.From != sender.ID || m.To != target.ID || m.State != "queued" {
		t.Fatalf("message=%+v", m)
	}
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: "forged", Action: "send", To: target.ID, Text: "x"}); err == nil {
		t.Fatal("forged sender capability accepted")
	}
	c.CreateSession(t.TempDir(), "new session", ModeBuild)
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "list"}); err == nil {
		t.Fatal("old capability crossed sessions")
	}
}

func TestPeerMessageInjectsWithoutResettingRuntimeAndSurvivesRefresh(t *testing.T) {
	c, r, _, target, key := newPeerCoord(t)
	c.runtimeSessions[target.ID] = &runtimeSession{id: "runtime-target", generation: 7, lastDynamic: "prefix"}
	v, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "The interface is in api.go:42; do not edit it."})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(PeerMessage)
	if m.State != "injected" || len(r.deliveries) != 1 || !strings.Contains(r.deliveries[0], "untrusted context") {
		t.Fatalf("injection=%+v, calls=%v", m, r.deliveries)
	}
	if c.runtimeSessions[target.ID].generation != 7 || c.runtimeSessions[target.ID].lastDynamic != "prefix" {
		t.Fatal("delivery reset active runtime state")
	}
	g, err := c.Engine.Refresh(target.ID, "retain coordination")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctxeng.Render(g), "api.go:42") {
		t.Fatal("refresh lost peer context")
	}
	for _, item := range g.Items {
		if item.Title == "Peer coordination messages" && (item.Source == ctxeng.SourceUser || item.Verified || item.Kind == ctxeng.KindConstraint) {
			t.Fatalf("peer message acquired user authority: %+v", item)
		}
	}
	targetKey, err := c.bindPeer(target, "runtime-target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: targetKey, Action: "inbox"}); err != nil {
		t.Fatal(err)
	}
	if c.Current().PeerMessages[0].State != "read" {
		t.Fatal("inbox did not acknowledge message")
	}
}

func TestPeerDeliveryFailureAndLimitsDoNotSilentlyDiscardMessages(t *testing.T) {
	c, r, _, target, key := newPeerCoord(t)
	c.runtimeSessions[target.ID] = &runtimeSession{id: "runtime-target"}
	r.err = errors.New("offline")
	for i := 0; i < maxPeerPending; i++ {
		v, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "evidence"})
		if err != nil {
			t.Fatal(err)
		}
		if m := v.(PeerMessage); m.State != "queued" || m.Reason == "" {
			t.Fatalf("failed delivery=%+v", m)
		}
	}
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "overflow"}); err == nil {
		t.Fatal("full mailbox accepted another message")
	}
	if len(c.Current().PeerMessages) != maxPeerPending {
		t.Fatal("full mailbox evicted accepted messages")
	}
	c.Agents.Update(target.ID, func(a *agent.Agent) { a.Status = agent.Completed })
	c.finishPeerTurn(c.SessionID(), target.ID)
	for _, m := range c.Current().PeerMessages {
		if m.State != "undelivered" || m.Reason == "" {
			t.Fatalf("pending message silently disappeared: %+v", m)
		}
	}
}

func TestPeerMessagingRejectsInvalidRecipientsAndOversizedMessages(t *testing.T) {
	c, _, sender, _, key := newPeerCoord(t)
	c.Agents.Create(agent.Agent{ID: "dead", Status: agent.Completed})
	for _, req := range []agentmsg.Request{
		{To: "unknown", Text: "x"}, {To: "dead", Text: "x"}, {To: sender.ID, Text: "x"},
		{To: "target", Text: " "}, {To: "target", Text: strings.Repeat("x", maxPeerText+1)},
	} {
		req.Key, req.Action = key, "send"
		if _, err := c.peerRequest(context.Background(), req); err == nil {
			t.Fatalf("accepted invalid request: %+v", req)
		}
	}
}

func TestPeerMessagingConcurrentSendAndRead(t *testing.T) {
	c, _, _, target, key := newPeerCoord(t)
	c.runtimeSessions[target.ID] = &runtimeSession{id: "runtime-target"}
	targetKey, _ := c.bindPeer(target, "runtime-target")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "evidence"})
			_, _ = c.peerRequest(context.Background(), agentmsg.Request{Key: targetKey, Action: "inbox"})
		}()
	}
	wg.Wait()
	if len(c.Current().PeerMessages) != 12 {
		t.Fatal("concurrent sends lost accepted messages")
	}
}

func TestPeerDeliveryStateDoesNotRegressWhenRuntimeReplyArrivesLate(t *testing.T) {
	c, _, _, target, key := newPeerCoord(t)
	v, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "evidence"})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(PeerMessage)
	sid := c.SessionID()
	c.peerState(sid, m.ID, "injected", "")
	if got := c.peerState(sid, m.ID, "queued", "late delivery error"); got.State != "injected" {
		t.Fatalf("runtime admission regressed: %+v", got)
	}
	c.peerState(sid, m.ID, "delivered", "included in recipient prompt")
	for _, state := range []string{"injected", "queued"} {
		if got := c.peerState(sid, m.ID, state, "late runtime reply"); got.State != "delivered" {
			t.Fatalf("prompt delivery regressed: %+v", got)
		}
	}
	if c.peerPending(target.ID) {
		t.Fatal("late runtime reply scheduled an already delivered message again")
	}
}
