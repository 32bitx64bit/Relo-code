package orchestrator

import (
	"context"
	"strings"
	"testing"

	ctxeng "github.com/relocode/relo/internal/context"
)

func TestWorkerCompletionDoesNotReuseAnsweredLatestUserMessage(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, planOneWorker, headWrap)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "answer the original question", nil) }()
	waitHeadCall(t, r)           // Primary answers the user and plans one worker.
	nextBegun(t, r.gatedRuntime) // The worker runs without any new user input.

	r.release("w1")
	prompt := waitHeadCall(t, r) // The finished worker reprompts Primary.
	driveEnds(t, done)

	if strings.Contains(prompt, "### Latest user message") {
		t.Fatalf("worker-only turn presented the answered message as latest user input:\n%s", prompt)
	}
}

func TestPeerOnlyStepDoesNotReuseAnsweredLatestUserMessage(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, headWrap)
	p := c.ensurePrimary()
	c.mu.Lock()
	c.addChatLocked(Chat{Role: "user", Text: "answer the original question"})
	c.addChatLocked(Chat{Role: "assistant", Text: "Original answer."})
	c.sess.PeerMessages = []PeerMessage{{ID: "peer-1", From: "worker-1", To: p.ID, State: "queued", Text: "Worker update."}}
	c.mu.Unlock()
	c.seedPrimary("answer the original question", nil)
	if _, err := c.Engine.Build(ctxeng.BuildRequest{AgentID: p.ID}); err != nil {
		t.Fatal(err)
	}

	c.stepQueued(context.Background())

	r.hmu.Lock()
	defer r.hmu.Unlock()
	if len(r.headSeen) != 1 {
		t.Fatalf("head calls = %d, want 1", len(r.headSeen))
	}
	if strings.Contains(r.headSeen[0], "### Latest user message") {
		t.Fatalf("peer-only turn presented the answered message as latest user input:\n%s", r.headSeen[0])
	}
}

func TestWorkerCompletionDoesNotRepostStaleAssistantReply(t *testing.T) {
	const staleReply = "Not yet. Phases 1 and 2 are finished and passed the full checks. All the Phase 3 code is now in your working tree."
	const latestReply = "The Phase 3 code is integrated and the final worker is running."
	stalePlan := `{"status":"working","userMessage":"` + staleReply + `","tasks":[{"id":"t1","title":"w1","role":"worker","description":"do it","acceptance":["done"],"priority":1,"deps":[],"difficulty":"medium"}]}`
	latestTurn := `{"status":"working","userMessage":"` + latestReply + `","tasks":[]}`
	repeatedTurn := `{"status":"completed","userMessage":"` + staleReply + `","tasks":[]}`
	c, r, _ := newGuidanceCoord(t, stalePlan, latestTurn, repeatedTurn)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "finish the project", nil) }()
	waitHeadCall(t, r)
	nextBegun(t, r.gatedRuntime)

	if err := c.Submit(context.Background(), "what is the status now?", nil); err != nil {
		t.Fatal(err)
	}
	waitHeadCall(t, r) // The user gets a newer answer while the worker continues.
	r.release("w1")
	waitHeadCall(t, r) // No new user message: the finished worker wakes Primary.
	driveEnds(t, done)

	p, _ := c.Agents.Primary()
	generations := c.Engine.Generations(p.ID)
	prompt := ctxeng.Render(generations[len(generations)-1])
	if !strings.Contains(prompt, latestReply) || strings.Contains(prompt, staleReply) {
		t.Errorf("worker-only turn did not carry the latest assistant reply:\n%s", prompt)
	}
	var stalePosts int
	for _, m := range c.Current().Messages {
		if m.Role == "assistant" && m.Text == staleReply {
			stalePosts++
		}
	}
	if stalePosts != 1 {
		t.Errorf("stale assistant reply posts = %d, want only the original", stalePosts)
	}
}

func TestDeliveredQueuedMessageDoesNotStartAnotherHeadTurn(t *testing.T) {
	c, r, _ := newGuidanceCoord(t, headWrap, headWrap)
	r.holdFirst = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	waitHeadCall(t, r)

	if err := c.Submit(context.Background(), "queued once", nil); err != nil {
		t.Fatal(err)
	}
	close(r.holdFirst)
	prompt := waitHeadCall(t, r)
	driveEnds(t, done)

	if !strings.Contains(prompt, "queued once") {
		t.Fatalf("follow-up turn did not deliver queued input:\n%s", prompt)
	}
	r.hmu.Lock()
	defer r.hmu.Unlock()
	if len(r.headSeen) != 2 {
		t.Fatalf("head calls = %d, want one initial turn and one queued-message turn", len(r.headSeen))
	}
}
