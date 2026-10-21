package orchestrator

import (
	"os"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/workspace"
)

// RELO_TEST_WORKSPACE_MODE=virtual (or auto) runs this package's whole suite on virtual workspaces, to show that
// accept, reject, undo and conflict behavior does not depend on how an agent's files are held.
func init() {
	if mode := os.Getenv("RELO_TEST_WORKSPACE_MODE"); mode != "" {
		workspace.DefaultConfig = workspace.Config{Mode: workspace.Mode(mode)}
	}
}

// mountTree is EnsureTree for a test that is about to write an agent's files as its tools would. A virtual workspace
// has no directory until something needs one, so it is laid out here; what is written is imported when the change is
// read, as it is for any tool.
func mountTree(c *Coord, agentID string) (workspace.Tree, error) {
	tree, err := c.WS.EnsureTree(agentID, false)
	if err != nil {
		return tree, err
	}
	_, err = c.WS.Materialize(agentID)
	return tree, err
}

// dirOf is the directory an agent's tools would work in, laid out if it was not. For a worktree it is the worktree.
func dirOf(t *testing.T, c *Coord, a agent.Agent) string {
	t.Helper()
	dir, err := c.WS.Materialize(a.ID)
	if err != nil {
		t.Fatalf("materialize %s: %v", a.ID, err)
	}
	return dir
}

// agentFile reads a file as the agent sees it, through its workspace view, however its files are held.
func agentFile(c *Coord, a agent.Agent, rel string) (string, error) {
	v, err := c.WS.View(a.ID)
	if err != nil {
		return "", err
	}
	b, err := v.ReadFile(rel)
	return string(b), err
}

// agentOf is the agent record the helpers above need when a test knows only the id.
func agentOf(id string) agent.Agent { return agent.Agent{ID: id} }
