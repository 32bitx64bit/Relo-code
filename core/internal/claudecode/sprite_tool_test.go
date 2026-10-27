package claudecode

import (
	"strings"
	"testing"

	"github.com/relocode/relo/internal/runtime"
)

// The sprite tool reaches Claude Code under its hub name when the policy allows it, and is denied by name otherwise,
// so a model that does not hold the capability cannot reach it through the shared hub.
func TestSpriteToolFollowsThePolicy(t *testing.T) {
	off := runtime.ToolsFor("worker", "build", true)
	_, allowed, denied := toolFlags(off, false)
	if strings.Contains(strings.Join(allowed, ","), "mcp__relo__sprite") || !strings.Contains(strings.Join(denied, ","), "mcp__relo__sprite") {
		t.Fatalf("without the tool in the policy: allowed %v, denied %v", allowed, denied)
	}
	on := runtime.WithSprite(runtime.ToolsFor("worker", "build", true), "worker", true)
	_, allowed, denied = toolFlags(on, false)
	if !strings.Contains(strings.Join(allowed, ","), "mcp__relo__sprite") || strings.Contains(strings.Join(denied, ","), "mcp__relo__sprite") {
		t.Fatalf("with the tool in the policy: allowed %v, denied %v", allowed, denied)
	}
	if got := reloTool("mcp__relo__sprite"); got != runtime.ReloSpriteTool {
		t.Fatalf("a sprite call is reported as %q, want %q", got, runtime.ReloSpriteTool)
	}
}
