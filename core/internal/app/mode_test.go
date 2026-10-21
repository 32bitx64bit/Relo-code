package app

import (
	"os"

	"github.com/relocode/relo/internal/workspace"
)

// RELO_TEST_WORKSPACE_MODE=virtual (or auto) runs this package's whole suite on virtual workspaces, to show that
// accept, reject, undo and conflict behavior does not depend on how an agent's files are held.
func init() {
	if mode := os.Getenv("RELO_TEST_WORKSPACE_MODE"); mode != "" {
		workspace.DefaultConfig = workspace.Config{Mode: workspace.Mode(mode)}
	}
}

// mountTree is EnsureTree for a test that is about to write an agent's files. A virtual workspace has no directory
// until something needs one, so it is laid out here; what the test writes is imported when the change is read, as
// what a tool wrote is.
func mountTree(core *App, agentID string) (workspace.Tree, error) {
	tree, err := core.Coord.WS.EnsureTree(agentID, false)
	if err != nil {
		return tree, err
	}
	return tree, mountDir(core, agentID)
}

// mountDir lays out an existing agent's directory. The orchestrator stacks a parent's patch before the agent runs,
// so a test that stacks does it between EnsureTree and mountDir.
func mountDir(core *App, agentID string) error {
	if t, ok := core.Coord.WS.Tree(agentID); !ok || !t.Virtual {
		return nil
	}
	_, err := core.Coord.WS.Materialize(agentID)
	return err
}

// isVirtual says whether an agent's files are held as a virtual workspace in this run, whatever mode the suite was
// started in; a few tests assert something that is true of one representation and has an equivalent in the other.
func isVirtual(core *App, agentID string) bool {
	t, ok := core.Coord.WS.Tree(agentID)
	return ok && t.Virtual
}
