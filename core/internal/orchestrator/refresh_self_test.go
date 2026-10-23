package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// refreshSelfTurn is a head turn that asks for a context reset and keeps its sub agents going.
const refreshSelfTurn = `{"status":"working","userMessage":"Resetting my context.","tasks":[],"refreshSelf":true}`

// refreshedCount counts context.refreshed events for the head agent.
func refreshedCount(k *captured, agentID string) int {
	n := 0
	for _, p := range k.named("context.refreshed") {
		if m, ok := p.(map[string]any); ok && m["agentId"] == agentID {
			n++
		}
	}
	return n
}

// A head turn that steps on queued guidance honors refreshSelf, not only the first head turn.
func TestRefreshSelfHonoredWhenHeadStepsOnQueuedGuidance(t *testing.T) {
	c, r, k := newGuidanceCoord(t, planOneWorker, refreshSelfTurn, headWrap)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	<-r.headCalls // the plan turn
	nextBegun(t, r.gatedRuntime)
	p, ok := c.Agents.Primary()
	if !ok {
		t.Fatal("no primary agent")
	}

	if err := c.Submit(context.Background(), "use tabs, not spaces", nil); err != nil {
		t.Fatalf("submit while running = %v", err)
	}
	select {
	case <-r.headCalls: // the queued-guidance step
	case <-time.After(3 * time.Second):
		t.Fatal("the head did not step on the queued guidance")
	}
	deadline := time.Now().Add(3 * time.Second)
	for refreshedCount(k, p.ID) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := refreshedCount(k, p.ID); got != 1 {
		t.Fatalf("context.refreshed for head = %d, want 1 after queued-guidance step with refreshSelf", got)
	}

	r.release("w1")
	driveEnds(t, done)
}

// A head reprompt after workers finish honors refreshSelf on that turn.
func TestRefreshSelfHonoredWhenHeadRepromptsAfterWorkers(t *testing.T) {
	c, r, k := newGuidanceCoord(t, planOneWorker, refreshSelfTurn)
	done := make(chan error, 1)
	go func() { done <- c.Submit(context.Background(), "first", nil) }()
	<-r.headCalls // the plan turn
	nextBegun(t, r.gatedRuntime)
	p, ok := c.Agents.Primary()
	if !ok {
		t.Fatal("no primary agent")
	}

	r.release("w1")
	driveEnds(t, done)

	r.hmu.Lock()
	seen := append([]string(nil), r.headSeen...)
	r.hmu.Unlock()
	if len(seen) < 2 || !strings.Contains(seen[len(seen)-1], "All scheduled workers finished") {
		t.Fatal("the head was not reprompted after the worker finished")
	}
	if got := refreshedCount(k, p.ID); got != 1 {
		t.Fatalf("context.refreshed for head = %d, want 1 after reprompt with refreshSelf", got)
	}
}
