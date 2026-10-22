package app

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/workspace"
)

// The rollout setting reaches the workspace layer, only the keys that are present change anything, and a value the
// core does not know can never turn virtual workspaces on.
func TestWorkspaceSettingsReachTheWorkspaceLayer(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	if got := core.Coord.WS.Config().Mode; got != workspace.ModeVirtual {
		t.Fatalf("unset mode = %s, want virtual", got)
	}

	core.applySettings(map[string]any{"workspaceMode": "worktree"})
	if got := core.Coord.WS.Config(); got.Mode != workspace.ModeWorktree || got.Materialization != "automatic" || got.VirtualReads {
		t.Fatalf("config = %+v", got)
	}
	tree, err := core.Coord.WS.EnsureTree("agent_w", false)
	if err != nil || tree.Virtual {
		t.Fatalf("worktree mode made %+v %v", tree, err)
	}

	core.applySettings(map[string]any{"workspaceMode": "virtual", "workspaceMaterialization": "worktree"})
	if got := core.Coord.WS.Config(); got.Mode != workspace.ModeVirtual || got.Materialization != "worktree" {
		t.Fatalf("config = %+v", got)
	}
	if tree, err := core.Coord.WS.EnsureTree("agent_v", false); err != nil || !tree.Virtual {
		t.Fatalf("virtual mode made %+v %v", tree, err)
	}
	// The agent that already had a worktree keeps it.
	if tree, _ := core.Coord.WS.EnsureTree("agent_w", false); tree.Virtual {
		t.Fatal("changing the mode moved an agent that was already working")
	}

	// A payload without the keys leaves the setting alone; an unknown mode means worktrees.
	core.applySettings(map[string]any{"autoApply": false})
	if got := core.Coord.WS.Config().Mode; got != workspace.ModeVirtual {
		t.Fatalf("an unrelated settings change moved the mode to %s", got)
	}
	core.applySettings(map[string]any{"workspaceMode": "turbo"})
	if got := core.Coord.WS.Config().Mode; got != workspace.ModeWorktree {
		t.Fatalf("unknown mode = %s, want worktree", got)
	}
}

// OpenCode runs its own file tools, so asking for virtual reads is answered with a notice and changes nothing.
func TestVirtualReadsAreRefusedWithANoticeWhereToolsAreNotRouted(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	var mu sync.Mutex
	var notes []string
	core.Coord.SetEmitter(func(name string, payload any) {
		if m, ok := payload.(orchestrator.Chat); ok && name == "chat.message" && m.Role == "system" {
			mu.Lock()
			notes = append(notes, m.Text)
			mu.Unlock()
		}
	})
	core.applySettings(map[string]any{"workspaceMode": "virtual", "workspaceVirtualReads": true})
	if core.Coord.WS.VirtualReads() {
		t.Fatal("virtual reads were enabled for a runtime that cannot route its file tools")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(notes)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], "virtualReads") {
		t.Fatalf("notices = %q", notes)
	}
}
