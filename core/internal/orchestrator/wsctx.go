package orchestrator

import (
	"errors"
	"sort"

	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/workspace"
)

// What an agent knows (its context), what its files are (its workspace) and what its tools did (its directory) are
// three states. They are linked by revision identifiers only: a generation names the workspace revision it was built
// against, and an item cut from a file names that file's content hash. These helpers make the link; they never move
// files and never touch the workspace except to read it.

// workspaceView returns the agent's isolated workspace, or ok=false for an agent that works in the user's own folder
// or has no workspace at all.
func (c *Coord) workspaceView(agentID string) (workspace.WorkspaceView, bool) {
	ws := c.workspace()
	if ws == nil {
		return nil, false
	}
	v, err := ws.View(agentID)
	if err != nil {
		if !errors.Is(err, workspace.ErrNoView) {
			c.logWarn("workspace view for context", err)
		}
		return nil, false
	}
	return v, true
}

// bindWorkspaceContext pins the agent's next generation to the repository state it has now.
func (c *Coord) bindWorkspaceContext(agentID string) {
	v, ok := c.workspaceView(agentID)
	if !ok {
		return
	}
	id, err := v.Identity()
	if err != nil {
		c.logWarn("workspace identity for context", err)
		return
	}
	c.Engine.BindWorkspace(agentID, ctxeng.WorkspaceContextRef{
		AgentID: agentID, BaseCommit: id.Base, OverlayRevision: id.Revision, EffectiveTreeOID: id.Tree,
	})
}

// deriveFiles records the content hash of each path in the agent's workspace now, for an item cut from those files.
// It returns nil when the agent has no isolated workspace, so the item is not claimed to come from one.
func (c *Coord) deriveFiles(agentID string, paths []string) *ctxeng.Derivation {
	v, ok := c.workspaceView(agentID)
	if !ok || len(paths) == 0 {
		return nil
	}
	id, err := v.Identity()
	if err != nil {
		return nil
	}
	d := &ctxeng.Derivation{AgentID: agentID, Revision: id.Revision, Hashes: map[string]string{}}
	for _, p := range paths {
		data, err := v.ReadFile(p)
		switch {
		case err == nil:
			d.Hashes[p] = ctxeng.BlobHash(data)
		case workspace.IsNotExist(err):
			d.Hashes[p] = ""
		default:
			return nil // a file that cannot be read cannot be vouched for
		}
	}
	return d
}

// revalidateContext checks the agent's file-derived items against its files as they are now.
func (c *Coord) revalidateContext(agentID string) ctxeng.Revalidation {
	v, ok := c.workspaceView(agentID)
	if !ok {
		return ctxeng.Revalidation{}
	}
	return c.Engine.Revalidate(agentID, func(path string) (string, bool, error) {
		data, err := v.ReadFile(path)
		switch {
		case err == nil:
			return ctxeng.BlobHash(data), true, nil
		case workspace.IsNotExist(err):
			return "", false, nil
		default:
			return "", false, err
		}
	})
}

// syncWorkspaceContext is what runs before a context is rebuilt: it checks what the agent knows against what it
// holds and binds the new generation to the revision it holds. A refresh changes the first and never the second.
func (c *Coord) syncWorkspaceContext(agentID string) {
	if res := c.revalidateContext(agentID); len(res.Stale) > 0 {
		paths := map[string]bool{}
		for _, s := range res.Stale {
			for _, p := range s.Paths {
				paths[p] = true
			}
		}
		list := make([]string, 0, len(paths))
		for p := range paths {
			list = append(list, p)
		}
		sort.Strings(list)
		c.emit("context.stale", map[string]any{"agentId": agentID, "items": len(res.Stale), "paths": list})
	}
	c.bindWorkspaceContext(agentID)
}
