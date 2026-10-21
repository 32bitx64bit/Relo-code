package workspace

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/vws"
)

// OverlaySource reads an agent's side of a change from its virtual workspace, so the merge engine decides a change
// held as Git objects by the same rules as one held in a worktree. With a snapshot it reads that frozen revision, so
// what is accepted is what was proposed even if the workspace has moved on.
type OverlaySource struct {
	w    *vws.Workspace
	snap *vws.Snapshot
}

var _ integration.Source = OverlaySource{}

// Revision is the workspace revision this source reads; zero means the live overlay.
func (s OverlaySource) Revision() uint64 {
	if s.snap != nil {
		return s.snap.Revision
	}
	return 0
}

// Check refuses a path that passes through a link in the agent's files. A link in the overlay is only text until it
// is checked out, but following one to read a file would let an agent point a change at a path it does not own.
func (s OverlaySource) Check(rel string) error {
	_, err := s.stat(rel)
	switch {
	case err == nil, vws.IsNotExist(err), errors.Is(err, vws.ErrNotDir):
		return nil
	case errors.Is(err, vws.ErrSymlink):
		return fmt.Errorf("patch path escapes workspace through symlink")
	default:
		return err
	}
}

func (s OverlaySource) stat(rel string) (vws.FileInfo, error) {
	if s.snap != nil {
		return s.w.StatAt(*s.snap, rel)
	}
	return s.w.Stat(rel)
}

// Read returns the agent's file. A link that stays inside the workspace is followed, as reading a checked-out file
// would; one that leaves it is an error.
func (s OverlaySource) Read(rel string) ([]byte, bool, error) {
	var data []byte
	var err error
	if s.snap != nil {
		data, err = s.w.ReadFileAt(*s.snap, rel)
	} else {
		data, err = s.w.ReadFile(rel)
	}
	switch {
	case err == nil:
		return data, true, nil
	case vws.IsNotExist(err), errors.Is(err, vws.ErrNotDir):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

// Mode is the permission the agent's file would have in a checkout: executable files keep their bit.
func (s OverlaySource) Mode(rel string) (os.FileMode, bool) {
	fi, err := s.stat(rel)
	if err != nil || fi.Kind == "dir" {
		return 0, false
	}
	if fi.Mode == 0o100755 {
		return 0o755, true
	}
	return 0o644, true
}

// SourceFor returns where to read an agent's change from. A change on a virtual workspace is read from its overlay
// at the revision it was proposed at (rev, when it was pinned); one on a worktree is read from the directory. ok is
// false when neither exists any more, for example after the agent's workspace was released.
func (m *Manager) SourceFor(agentID, worktree string, rev uint64) (integration.Source, bool) {
	if w, ok := m.virtualOf(worktree); ok && (agentID == "" || w.ID() == agentID) {
		// Tool edits that were never imported would otherwise be invisible to the merge.
		if t, found := m.Tree(w.ID()); found && t.Virtual {
			_ = m.Sync(w.ID())
		}
		if rev != 0 {
			if snap, err := w.SnapshotAt(rev); err == nil {
				return OverlaySource{w: w, snap: &snap}, true
			}
			if w.Info().Revision != rev {
				return nil, false // the proposal's revision is gone and the overlay has moved on
			}
		}
		return OverlaySource{w: w}, true
	}
	if worktree == "" || strings.HasPrefix(worktree, "vws:") {
		return nil, false
	}
	if st, err := os.Stat(worktree); err == nil && st.IsDir() {
		return integration.DirSource{Dir: worktree}, true
	}
	return nil, false
}
