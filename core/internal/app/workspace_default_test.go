package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/workspace"
)

// An unset mode means virtual, an explicit worktree still means a worktree, and startup still clears worktrees that
// were left behind before the default changed.
func TestUnsetModeIsVirtualAndSweepStillRuns(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	if got := core.Coord.WS.Config().Mode; got != workspace.ModeVirtual {
		t.Fatalf("unset mode = %s, want virtual", got)
	}
	if tree, err := core.Coord.WS.EnsureTree("agent_default", false); err != nil || !tree.Virtual {
		t.Fatalf("default mode made %+v %v", tree, err)
	}
	core.applySettings(map[string]any{"workspaceMode": "worktree"})
	if got := core.Coord.WS.Config().Mode; got != workspace.ModeWorktree {
		t.Fatalf("explicit worktree mode = %s", got)
	}

	// A worktree left over from before the switch, owned by no pending change.
	stale := filepath.Join(dir, ".relo", "worktrees", "agent_stale")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	core.applySettings(map[string]any{"workspaceMode": "virtual"})
	core.sweepWorktrees()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("startup sweep kept %s in virtual mode: %v", stale, err)
	}
}
