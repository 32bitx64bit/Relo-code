package orchestrator

import (
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
)

// A head turn that lists resolved changes passes them to the app's reject path, and a turn without the field calls
// nothing.
func TestApplyTurnResolvesChangesThroughTheHook(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	var calls [][]string
	c.ResolveChanges = func(ids []string) { calls = append(calls, append([]string(nil), ids...)) }

	c.applyTurn(agent.Agent{}, Turn{})
	if len(calls) != 0 {
		t.Fatalf("a turn without resolvedChanges called the hook: %v", calls)
	}
	c.applyTurn(agent.Agent{}, Turn{ResolvedChanges: []string{"chg_a", "chg_b"}})
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != "chg_a" || calls[0][1] != "chg_b" {
		t.Fatalf("hook calls = %v", calls)
	}
}
