package integration

import (
	"os"
	"path/filepath"
)

// Source is where the agent's side of a change is read from. The merge engine decides what to write to the
// workspace from three inputs (the workspace file, the base text and the agent's file) and never needs to know
// whether the agent's file sits in a checked-out directory or in a virtual overlay of Git objects.
type Source interface {
	// Check refuses a path that reaches outside the agent's files, for example through a symlink.
	Check(rel string) error
	// Read returns the agent's file. exists is false when the agent has no such file.
	Read(rel string) (data []byte, exists bool, err error)
	// Mode is the permission bits of the agent's file, used for a file the workspace does not have yet. ok is false
	// when the agent has no such file or its mode is not known.
	Mode(rel string) (mode os.FileMode, ok bool)
}

// DirSource reads the agent's files from a directory: its Git worktree, or the directory a virtual workspace was
// materialized into for the tools.
type DirSource struct{ Dir string }

func (d DirSource) Check(rel string) error { return checkSymlinkPath(d.Dir, rel) }

func (d DirSource) Read(rel string) ([]byte, bool, error) {
	data, err := os.ReadFile(filepath.Join(d.Dir, filepath.FromSlash(rel)))
	if err == nil {
		return data, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, err
}

func (d DirSource) Mode(rel string) (os.FileMode, bool) {
	st, err := os.Stat(filepath.Join(d.Dir, filepath.FromSlash(rel)))
	if err != nil {
		return 0, false
	}
	return st.Mode().Perm(), true
}
