package workspace

import (
	"os"
	"testing"
)

// TestMain keeps this package's existing worktree-focused tests on worktrees. They build a manager with New and no
// Configure call, so they would otherwise take the virtual default; the services now default virtual on purpose, and
// virtualRepo and the explicit-mode tests cover that. RELO_TEST_WORKSPACE_MODE still overrides the whole suite.
func TestMain(m *testing.M) {
	if DefaultConfig.Mode == "" {
		DefaultConfig = Config{Mode: ModeWorktree}
	}
	os.Exit(m.Run())
}
