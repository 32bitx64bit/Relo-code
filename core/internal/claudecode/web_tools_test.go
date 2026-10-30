package claudecode

import (
	"slices"
	"testing"

	"github.com/relocode/relo/internal/runtime"
)

// A role offered relo_web gets Claude Code's WebFetch and WebSearch turned off and mcp__relo__web allowed.
func TestWebOfferTurnsOffClaudeCodeBuiltinsAndAllowsReloWeb(t *testing.T) {
	for _, role := range []string{"worker", "probe", "architect"} {
		policy := runtime.WithWeb(runtime.ToolsFor(role, "build", true), role, true)
		builtins, allowed, denied := toolFlags(policy, false)
		if slices.Contains(builtins, "WebFetch") || slices.Contains(builtins, "WebSearch") {
			t.Errorf("%s: builtins still include web tools: %v", role, builtins)
		}
		if !slices.Contains(allowed, "mcp__relo__web") || slices.Contains(denied, "mcp__relo__web") {
			t.Errorf("%s: allowed=%v denied=%v, want mcp__relo__web allowed", role, allowed, denied)
		}
	}
}
