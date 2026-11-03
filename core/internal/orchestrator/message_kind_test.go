package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/runtime"
)

// A message asks something of its recipient only when its sender says so, or when it is a question. Progress, results
// and acknowledgements are for the record. In a Pixels at War run they woke the Primary for 100 of its 123 turns.
func TestOnlyAMessageThatAsksNeedsAReply(t *testing.T) {
	for _, tc := range []struct {
		kind, text string
		want       bool
	}{
		{"", "Seed1337 10y events: emitted/accepted/refused 18/18/0.", false},
		{"", "Acknowledged: bits 64 and 128 are reserved.", false},
		{"", "Is another heavy build currently running?", true},
		{"ask", "Which of the two flag layouts do you want", true},
		{"blocked", "the registry rejects NULL names", true},
		{"deviation", "facts.c has no ai_fact_industry", true},
		{"fyi", "Is this one ok?", false},
	} {
		if got := needsReply(tc.kind, tc.text); got != tc.want {
			t.Errorf("needsReply(%q, %q) = %v, want %v", tc.kind, tc.text, got, tc.want)
		}
	}
}

// A progress note does not start a Primary turn. It waits and rides along with the next turn, which a question starts.
func TestAProgressNoteWaitsForTheNextPrimaryTurn(t *testing.T) {
	c, g, _ := newGuidanceCoord(t, planOneWorker, headWrap, headWrap)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "build it", nil) }()
	waitHeadCall(t, g)
	nextBegun(t, g.gatedRuntime)

	p, _ := c.Agents.Primary()
	ws := workerAgents(c)
	if len(ws) != 1 {
		t.Fatalf("workers = %d, want 1", len(ws))
	}
	c.runtimeMu.Lock()
	live := c.runtimeSessions[ws[0].ID]
	c.runtimeMu.Unlock()
	key, err := c.bindPeer(ws[0], live.id)
	if err != nil {
		t.Fatal(err)
	}
	const note = "Module 123 patch is at 40 percent; tests pass so far."
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: p.ID, Text: note}); err != nil {
		t.Fatal(err)
	}
	select {
	case prompt := <-g.headCalls:
		t.Fatalf("a progress note started a Primary turn:\n%s", prompt)
	case <-time.After(400 * time.Millisecond):
	}

	const question = "The registry rejects NULL names; should the dispatch pass the module name?"
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: p.ID, Text: question}); err != nil {
		t.Fatal(err)
	}
	prompt := waitHeadCall(t, g)
	if !strings.Contains(prompt, question) || !strings.Contains(prompt, note) {
		t.Fatalf("the question's turn lacks the question or the note that waited:\n%s", prompt)
	}
	g.release("w1")
	driveEnds(t, done)
}

// A finished worker is woken to answer a question, not to be told something: the wake is a model turn.
func TestAFinishedWorkerIsNotWokenToBeToldSomething(t *testing.T) {
	c, _, _, _, key := newPeerCoord(t)
	c.SetRuntime(&wakeRuntime{Mock: runtime.NewMock()})
	dead := c.Agents.Create(agent.Agent{ID: "done-worker", Role: agent.Worker, Status: agent.Completed})
	if _, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "send", To: dead.ID, Text: "The flag moved to 128.", Kind: "fyi"}); err != nil {
		t.Fatal(err)
	}
	d := wakeDispatcher(c)
	d.wakePeers()
	d.wg.Wait()
	msgs := c.Current().PeerMessages
	if len(msgs) != 1 || msgs[0].State != "read" || msgs[0].Reason == "" {
		t.Fatalf("a note to a finished worker = %+v, want read at once with no reply turn", msgs)
	}
}

func TestAMessageKeepsTheKindItsSenderNamed(t *testing.T) {
	c, _, _, target, key := newPeerCoord(t)
	req := agentmsg.Request{Key: key, Action: "send", To: target.ID, Text: "x", Kind: "fyi"}
	if _, err := c.peerRequest(context.Background(), req); err != nil {
		t.Fatalf("fyi refused: %v", err)
	}
	if got := c.Current().PeerMessages[0].Kind; got != "fyi" {
		t.Fatalf("kind = %q, want fyi", got)
	}
}
