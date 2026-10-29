package orchestrator

import (
	"testing"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
)

// agent.escalated must carry the attempt number that dispatch recorded for the run, and only when a run starts.
func TestEscalationEventCarriesDispatchedAttempt(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	var events []map[string]any
	c.SetEmitter(func(name string, payload any) {
		if m, ok := payload.(map[string]any); ok && name == "agent.escalated" {
			events = append(events, m)
		}
	})
	started := taskgraph.Task{ID: "esc", Model: "mock/p", Attempt: 3}
	c.announceEscalation(started)
	if len(events) != 0 {
		t.Fatalf("escalation announced without a queued model change: %+v", events)
	}
	c.escalations = map[string]string{"esc": "mock/t1"}
	c.announceEscalation(started)
	c.announceEscalation(started)
	if len(events) != 1 {
		t.Fatalf("escalation announced %d times, want once", len(events))
	}
	if got := events[0]["attempt"]; got != 3 || events[0]["from"] != "mock/t1" || events[0]["to"] != "mock/p" {
		t.Fatalf("escalation payload = %+v, want attempt 3 from mock/t1 to mock/p", events[0])
	}
}
