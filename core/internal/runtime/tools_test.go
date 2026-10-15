package runtime

import "testing"

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
			if !allFalse(got) {
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
