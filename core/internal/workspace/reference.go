package workspace

import (
	"errors"
	"fmt"

	"github.com/relocode/relo/internal/vws"
)

// ChangeReference points at changes in another agent's workspace: a message carries it instead of the code, and the
// recipient sees none of it until it imports some paths.
type ChangeReference = vws.ChangeReference

// ErrNoReferences says the sender's files are held in a Git worktree, which has no pinned revision to point at.
var ErrNoReferences = errors.New("workspace: change references need the sender's files to be in a virtual workspace (relo.workspace.mode virtual or auto)")

// Reference pins the sender's current files and returns a reference to the named paths, each of which the sender has
// changed. Tool edits not yet imported are captured first, so the reference describes what is really there.
func (m *Manager) Reference(agentID string, paths []string) (ChangeReference, error) {
	t, ok := m.Tree(agentID)
	if !ok || !t.Virtual {
		return ChangeReference{}, ErrNoReferences
	}
	w, err := m.virtual(agentID)
	if err != nil {
		return ChangeReference{}, err
	}
	if err := m.Sync(agentID); err != nil {
		return ChangeReference{}, err
	}
	return w.Reference(paths)
}

// ImportChange brings the named paths (all of them when paths is empty) of a referenced change into the agent's own
// workspace and returns the paths written. This is the only way another agent's code reaches a workspace. It never
// writes the user's folder: an agent without an isolated workspace is refused. When the agent's tools are running
// the files go into its directory, as tool output would; otherwise into its overlay.
func (m *Manager) ImportChange(agentID string, ref ChangeReference, paths []string) ([]string, error) {
	t, ok := m.Tree(agentID)
	if !ok || t.Path == m.root || t.Direct {
		return nil, fmt.Errorf("%w (%s)", ErrNoView, agentID)
	}
	if agentID == ref.SenderAgent {
		return nil, fmt.Errorf("workspace: %s cannot import its own change", agentID)
	}
	eng := m.engine(false)
	if eng == nil {
		return nil, fmt.Errorf("%w: no virtual workspaces exist", vws.ErrNoReference)
	}
	if !t.Virtual {
		return eng.WriteReferenceInto(t.Path, ref, paths)
	}
	w, err := m.virtual(agentID)
	if err != nil {
		return nil, err
	}
	if _, open := w.Exec(); open {
		ctx, cancel := execContext()
		defer cancel()
		return eng.ImportReferenceIntoExec(ctx, agentID, ref, paths)
	}
	_, names, err := eng.ImportReference(agentID, ref, paths, vws.ApplyOptions{})
	return names, err
}
