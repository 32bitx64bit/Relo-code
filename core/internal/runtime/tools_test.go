package runtime

import (
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
)

func TestToolsForMatrix(t *testing.T) {
	allFalse := func(m map[string]bool) bool {
		for _, v := range m {
			if v {
				return false
			}
		}
		return true
	}

	// Primary: every tool disabled regardless of mode or mutating flag.
	for _, mode := range []string{"build", "plan", "review"} {
		for _, mutating := range []bool{true, false} {
			got := ToolsFor(RolePrimary, mode, mutating)
			// relo_agents is the one tool Primary may use: it reaches the coordination hub, not the workspace.
			withoutRelo := map[string]bool{}
			for k, v := range got {
				if k != ReloAgentsTool {
					withoutRelo[k] = v
				}
			}
			if !allFalse(withoutRelo) {
				t.Fatalf("primary mode=%s mutating=%v has enabled tools: %v", mode, mutating, got)
			}
			if got["read"] || got["bash"] || got["write"] {
				t.Fatalf("primary must not read/bash/write: %v", got)
			}
			// OpenCode v2 calls the command tool "shell"; "*" covers everything else it offers (task, webfetch, ...).
			if v, ok := got["shell"]; !ok || v {
				t.Fatalf("primary must have shell denied: %v", got)
			}
			if v, ok := got["*"]; !ok || v {
				t.Fatalf("primary must deny everything else with *: %v", got)
			}
		}
	}

	// Non-primary roles keep DefaultTools behavior exactly.
	cases := []struct {
		role, mode string
		mutating   bool
		bash       bool
		write      bool
	}{
		{"worker", "build", true, true, true},
		{"probe", "build", false, true, false},
		{"reviewer", "build", false, true, false},
		{"reviewer", "review", false, false, false},
		{"test", "build", false, true, false},
		{"worker", "plan", true, true, false},
		{"worker", "review", true, false, false},
	}
	for _, tc := range cases {
		got := ToolsFor(tc.role, tc.mode, tc.mutating)
		want := DefaultTools(tc.mode, tc.mutating)
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s/%s mutating=%v tool %s: got %v want %v", tc.role, tc.mode, tc.mutating, k, got[k], v)
			}
		}
		if got["bash"] != tc.bash || got["write"] != tc.write {
			t.Fatalf("%s/%s mutating=%v: bash=%v write=%v want bash=%v write=%v", tc.role, tc.mode, tc.mutating, got["bash"], got["write"], tc.bash, tc.write)
		}
		if !got["read"] || !got["grep"] || !got["glob"] || !got["list"] {
			t.Fatalf("%s lost read tools: %v", tc.role, got)
		}
	}
}

// Every role may use relo_agents to reach its peers; the MCP hub is the only tool it adds.
func TestReloAgentsEnabledForEveryRole(t *testing.T) {
	for _, role := range []string{RolePrimary, "worker", "probe", "test", "reviewer", "debug", "architect", "integration"} {
		for _, mode := range []string{"build", "plan", "review"} {
			got := ToolsFor(role, mode, true)
			if v, ok := got[ReloAgentsTool]; !ok || !v {
				t.Errorf("%s/%s: %s = %v (present %v), want true", role, mode, ReloAgentsTool, v, ok)
			}
		}
	}
	if ReloAgentsTool != "relo_agents" {
		t.Fatalf("ReloAgentsTool = %q", ReloAgentsTool)
	}
}

// Enabling relo_agents must not switch on or off any other MCP or unrelated tool.
func TestReloAgentsLeavesOtherMCPToolsUnlisted(t *testing.T) {
	for _, role := range []string{RolePrimary, "worker", "debug"} {
		got := ToolsFor(role, "build", true)
		for name := range got {
			if strings.HasPrefix(name, "relo_") && name != ReloAgentsTool {
				t.Errorf("%s: unexpected relo tool %q", role, name)
			}
			if strings.Contains(name, "_") && name != ReloAgentsTool {
				t.Errorf("%s: unexpected MCP-style tool %q listed", role, name)
			}
		}
	}
}

// Each tool OpenCode offers is described to the model in every call, so unused ones are pure cost.
func TestAgentsDoNotCarryToolsReloNeverUses(t *testing.T) {
	for _, role := range []string{"worker", "probe", "test", "reviewer", "debug", "architect", "integration"} {
		got := ToolsFor(role, "build", role == "worker")
		for _, name := range []string{"task", "todowrite", "todoread", "skill", "question"} {
			if v, ok := got[name]; !ok || v {
				t.Errorf("%s: tool %q should be denied, got %v (present %v)", role, name, v, ok)
			}
		}
		// A tool that is not listed stays available; only an explicit false removes it.
		research := role == "debug" || role == "architect"
		for _, name := range []string{"webfetch", "websearch"} {
			if v, listed := got[name]; research && listed && !v || !research && !(listed && !v) {
				t.Errorf("%s: %s = %v (listed %v), research role %v", role, name, v, listed, research)
			}
		}
		if !got["read"] || !got["grep"] || !got["glob"] {
			t.Errorf("%s lost a reading tool: %v", role, got)
		}
	}
}

// relo_canvas is an explicit allow only when enabled. Primary keeps every repository tool denied either way.
func TestCanvasToolAllowedForPrimaryWhileRepositoryDenied(t *testing.T) {
	on := WithCanvas(ToolsFor(RolePrimary, "build", false), true, agent.CanvasActionsForRole(agent.Primary))
	if !on[ReloCanvasTool] {
		t.Fatalf("Primary lacks %s when canvas is enabled: %v", ReloCanvasTool, on)
	}
	for _, name := range []string{"read", "grep", "glob", "list", "write", "edit", "bash", "shell", "webfetch", "websearch"} {
		if v, ok := on[name]; !ok || v {
			t.Errorf("Primary repository tool %q = %v (present %v), want explicit deny", name, v, ok)
		}
	}
	off := WithCanvas(ToolsFor(RolePrimary, "build", false), false, agent.CanvasActionsForRole(agent.Primary))
	if _, ok := off[ReloCanvasTool]; ok {
		t.Fatalf("canvas listed while disabled: %v", off)
	}
}

func TestCanvasToolEnabledForWorkersAndNeverGrantsRepoTools(t *testing.T) {
	got := WithCanvas(ToolsFor("worker", "build", true), true, agent.CanvasActionsForRole(agent.Worker))
	if !got[ReloCanvasTool] {
		t.Fatalf("worker lacks canvas: %v", got)
	}
	if got["write"] != true || got["edit"] != true {
		t.Fatalf("canvas wiring changed worker repo tools: %v", got)
	}
	planned := WithCanvas(ToolsFor("worker", "plan", false), true, agent.CanvasActionsForRole(agent.Worker))
	if planned["write"] || planned["edit"] {
		t.Fatalf("plan mode repo writes enabled by canvas: %v", planned)
	}
}

// The canvas exposure follows the role scope: a builder role keeps every action, a role that may not author keeps
// the read-only path, and a turn with no canvas action at all gets no canvas tool.
func TestCanvasExposureFollowsTheRoleScope(t *testing.T) {
	authoring := agent.CanvasActionsForRole(agent.Worker)
	readOnly := agent.CanvasActionsForRole(agent.Primary)
	if len(authoring) <= len(readOnly) {
		t.Fatalf("builder scope = %v, primary scope = %v", authoring, readOnly)
	}
	for _, action := range []string{agent.CanvasActionCreate, agent.CanvasActionUpdate} {
		if !contains(authoring, action) {
			t.Errorf("builder scope lost %s: %v", action, authoring)
		}
		if contains(readOnly, action) {
			t.Errorf("primary scope advertises %s: %v", action, readOnly)
		}
	}
	// A restricted role keeps the tool: listing, opening and inspecting a canvas is how it checks a worker's result.
	if got := WithCanvas(ToolsFor(RolePrimary, "build", false), true, readOnly); !got[ReloCanvasTool] {
		t.Fatalf("primary lost the read-only canvas path: %v", got)
	}
	// A turn with no canvas action gets no canvas tool, so the tool is never offered without a scope behind it.
	if got := WithCanvas(ToolsFor(RolePrimary, "build", false), true, nil); got[ReloCanvasTool] {
		t.Fatalf("canvas offered without a scope: %v", got)
	}
}

// contains reports whether the scope names an action.
func contains(scope []string, action string) bool {
	for _, name := range scope {
		if name == action {
			return true
		}
	}
	return false
}
