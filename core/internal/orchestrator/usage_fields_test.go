package orchestrator

import (
	"context"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
)

// usage.updated carries cacheRead and cacheWrite apart from the uncached usedTokens sum.
func TestUsageUpdatedCarriesCacheFieldsApartFromUsedTokens(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime(`{}`))
	s := c.CreateSession(t.TempDir(), "usage fields", ModeBuild)
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace, Model: "mock/t1"})
	var payload map[string]any
	c.SetEmitter(func(name string, value any) {
		if name == "usage.updated" {
			payload = value.(map[string]any)
		}
	})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{Static: "stable"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	if payload == nil {
		t.Fatal("usage.updated not emitted")
	}
	want := map[string]any{"agentId": a.ID, "usedTokens": 16, "input": 10, "output": 4, "cacheRead": 8, "cacheWrite": 3}
	for key, v := range want {
		if payload[key] != v {
			t.Fatalf("usage.updated %s=%v, want %v; payload=%#v", key, payload[key], v, payload)
		}
	}
}
