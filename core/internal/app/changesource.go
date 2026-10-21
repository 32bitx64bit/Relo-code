package app

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/orchestrator"
)

// changeSource is where the agent's side of a change is read from: its virtual workspace at the revision the change
// was proposed at, or its worktree directory. ok is false when neither exists any more, for example after the
// agent's workspace was released.
func (a *App) changeSource(ch orchestrator.Change) (integration.Source, bool) {
	if a.Coord.WS == nil || !a.ownWorktree(ch.Worktree) {
		return nil, false
	}
	return a.Coord.WS.SourceFor(ch.AgentID, ch.Worktree, ch.Rev)
}

// agentWorkspaceExists reports whether an agent's files can still be read, without reading them.
func (a *App) agentWorkspaceExists(agentID, worktree string) bool {
	if a.Coord.WS == nil || !a.ownWorktree(worktree) {
		return false
	}
	return a.Coord.WS.Available(agentID, worktree)
}

// agentFile returns the agent's version of a change's file; exists is false when the agent deleted it.
func (a *App) agentFile(ch orchestrator.Change) (data []byte, exists bool, err error) {
	src, ok := a.changeSource(ch)
	if !ok {
		return nil, false, os.ErrNotExist
	}
	return src.Read(filepath.ToSlash(ch.Path))
}

// capText applies the diff view's limits to bytes that were already read.
func capText(data []byte) (text string, binary, truncated bool) {
	if len(data) > maxDiffBytes {
		data, truncated = data[:maxDiffBytes], true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", true, false
	}
	return string(data), false, truncated
}

// openAgentDirectory returns the folder to show for an agent. An agent that lives in a virtual workspace has no
// folder between runs, so one is laid out for the user to look at; it is imported and removed when the agent's
// changes are decided, like any other.
func (a *App) openAgentDirectory(ag agent.Agent) (any, error) {
	path := ag.Worktree
	if a.Coord.WS != nil && a.ownWorktree(ag.Worktree) {
		if tree, err := a.Coord.WS.EnsureTree(ag.ID, false); err == nil && tree.Virtual {
			dir, merr := a.Coord.WS.Materialize(ag.ID)
			if merr != nil {
				return nil, ipc.Fail{C: "worktree_unavailable", M: "the agent's files could not be laid out: " + merr.Error()}
			}
			path = dir
		}
	}
	return map[string]any{"path": path}, nil
}
