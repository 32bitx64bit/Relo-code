package app

import (
	"encoding/json"
	"fmt"

	"github.com/relocode/relo/internal/persistence"
	"github.com/relocode/relo/internal/vws"
)

// Git holds the truth about virtual workspaces: their overlays, bases and pins are refs under refs/relo/vws. The
// store mirrors them so the UI and diagnostics can list them without running Git, and so that a restart can tell
// when the two disagree. A mirror row is always rebuilt from Git, never the other way round.

// workspaceEvent runs for every revision a virtual workspace publishes, and for its removal. It records the revision
// in the event log and refreshes the mirror. Updates are serialized and read the workspace when they run, so the
// mirror cannot be left on an older revision by two events that finish out of order.
func (a *App) workspaceEvent(ev vws.Event) {
	if sess := a.Coord.SessionID(); sess != "" {
		if err := a.Store.LogEvent(sess, "workspace."+ev.Kind, ev); err != nil {
			a.Log.Warn("persist workspace event failed", map[string]any{"agent": ev.AgentID, "error": err.Error()})
		}
	}
	a.vwMu.Lock()
	defer a.vwMu.Unlock()
	a.mirrorLocked(ev.AgentID)
}

// mirrorLocked brings the stored row of one agent in line with Git.
func (a *App) mirrorLocked(agentID string) {
	ws := a.Coord.WS
	if ws == nil || a.Store == nil {
		return
	}
	root := ws.Root()
	info, ok := ws.VirtualWorkspace(agentID)
	if !ok {
		if err := a.Store.DeleteVWorkspace(root, agentID); err != nil {
			a.Log.Warn("forget workspace failed", map[string]any{"agent": agentID, "error": err.Error()})
		}
		return
	}
	payload, err := json.Marshal(info)
	if err != nil {
		return
	}
	err = a.Store.PutVWorkspace(persistence.VWorkspace{
		Workspace: root, AgentID: agentID, SessionID: a.Coord.SessionID(), BaseCommit: info.BaseCommit,
		Revision: info.Revision, Tree: info.EffectiveTree, State: string(info.State), Payload: payload,
	})
	if err != nil {
		a.Log.Warn("mirror workspace failed", map[string]any{"agent": agentID, "error": err.Error()})
	}
}

// reconcileWorkspaces compares the stored mirror with what Git holds. It is called once at start, after the engine
// has restored what it could. Workspaces Git holds are mirrored (adopting ones the store never saw). A stored one
// that Git no longer holds is forgotten; if it had edits nobody had decided on, the user is told, because those edits
// cannot be restored. A clean workspace that vanished lost nothing and is not worth a message.
func (a *App) reconcileWorkspaces() {
	ws := a.Coord.WS
	if ws == nil || a.Store == nil {
		return
	}
	a.vwMu.Lock()
	defer a.vwMu.Unlock()
	root := ws.Root()
	live := map[string]bool{}
	for _, info := range ws.VirtualWorkspaces() {
		live[info.AgentID] = true
		a.mirrorLocked(info.AgentID)
	}
	rows, err := a.Store.ListVWorkspaces(root)
	if err != nil {
		a.Log.Warn("list stored workspaces failed", map[string]any{"error": err.Error()})
		return
	}
	for _, row := range rows {
		if live[row.AgentID] {
			continue
		}
		var was vws.VirtualWorkspace
		_ = json.Unmarshal(row.Payload, &was)
		if err := a.Store.DeleteVWorkspace(root, row.AgentID); err != nil {
			a.Log.Warn("forget workspace failed", map[string]any{"agent": row.AgentID, "error": err.Error()})
		}
		a.Log.Warn("virtual workspace is gone from Git", map[string]any{"agent": row.AgentID, "revision": row.Revision, "dirty": was.Dirty})
		if was.Dirty {
			a.workspaceNotice("workspace.lost", fmt.Sprintf("Agent %s's virtual workspace has unaccepted edits that are no longer in Git (its refs under refs/relo/vws were removed), so they cannot be restored.", row.AgentID))
		}
	}
}
