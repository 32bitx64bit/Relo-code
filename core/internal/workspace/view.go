package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/vws"
)

// WorkspaceView is one agent's isolated file state and the diff it has produced, whether the agent lives in a Git
// worktree or in a virtual overlay. Consumers that read or change an agent's files program against it and never
// need to know which. Paths are slash-separated and relative to the workspace root.
type WorkspaceView interface {
	AgentID() string
	// Kind is "worktree" or "virtual".
	Kind() string
	// ReadFile returns the version of the file this workspace sees. IsNotExist reports a missing file.
	ReadFile(rel string) ([]byte, error)
	Stat(rel string) (FileInfo, error)
	ListDir(rel string) ([]DirEntry, error)
	// WriteFile creates or replaces a file; a zero mode keeps an existing file's mode, or 0644 for a new one.
	WriteFile(rel string, data []byte, mode os.FileMode) error
	// DeleteFile removes a file. It stays gone: it never falls back to the base.
	DeleteFile(rel string) error
	// Changes lists what differs from the base, taken from Git and not from what the agent says it did.
	Changes() ([]FileChange, error)
	// Patch is the diff against the base for one path, or for every path when rel is empty.
	Patch(rel string) (string, error)
	// BaseFile is the file at the base the agent was cut from; ok is false when it did not exist there.
	BaseFile(rel string) (data []byte, ok bool, err error)
	// Identity names the state the view is in. Two equal identities of one agent mean equal files.
	Identity() (ViewIdentity, error)
	// Source adapts the view for the integration engine.
	Source() integration.Source
}

// ViewIdentity is the stable revision identifier that links what an agent knows to what it holds: the base it was
// cut from and the tree of its current files. Revision counts published states of a virtual workspace and is zero
// for a worktree, which has none.
type ViewIdentity struct {
	AgentID  string `json:"agentId"`
	Kind     string `json:"kind"`
	Base     string `json:"base"`
	Tree     string `json:"tree,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

// FileInfo describes one path of a view.
type FileInfo struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // file, dir, symlink
	Size int64  `json:"size"`
	Exec bool   `json:"exec,omitempty"`
}

// DirEntry is one name in a directory listing.
type DirEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // file, dir, symlink
	Size int64  `json:"size"`
}

// ErrNoView says the agent has no isolated workspace: it reads the user's folder directly.
var ErrNoView = errors.New("workspace: the agent has no isolated workspace")

// View returns the agent's workspace as a WorkspaceView.
func (m *Manager) View(agentID string) (WorkspaceView, error) {
	t, ok := m.Tree(agentID)
	if !ok || t.Path == m.root || t.Direct {
		return nil, fmt.Errorf("%w (%s)", ErrNoView, agentID)
	}
	if t.Virtual {
		w, err := m.virtual(agentID)
		if err != nil {
			return nil, err
		}
		return &virtualView{m: m, w: w}, nil
	}
	return &worktreeView{m: m, id: agentID}, nil
}

// worktreeView is the WorkspaceView of an agent in a Git worktree. Every method goes through the same Manager code
// that worked before the abstraction existed, so behavior is unchanged.
type worktreeView struct {
	m  *Manager
	id string
}

func (v *worktreeView) AgentID() string { return v.id }
func (v *worktreeView) Kind() string    { return "worktree" }

func (v *worktreeView) tree() (Tree, error) {
	t, ok := v.m.Tree(v.id)
	if !ok || t.Virtual || t.Path == v.m.root {
		return Tree{}, fmt.Errorf("%w (%s)", ErrNoView, v.id)
	}
	return t, nil
}

// path resolves rel inside the worktree and refuses anything that would leave it or touch Git's or Relo's own data.
func (v *worktreeView) path(t Tree, rel string) (string, error) {
	clean, err := cleanPath(rel)
	if err != nil {
		return "", err
	}
	if vws.Protected(clean) {
		return "", fmt.Errorf("%w: %s", vws.ErrProtected, clean)
	}
	full := filepath.Join(t.Path, filepath.FromSlash(clean))
	if err := ensureWithin(t.Path, full); err != nil {
		return "", err
	}
	return full, nil
}

func (v *worktreeView) ReadFile(rel string) ([]byte, error) {
	t, err := v.tree()
	if err != nil {
		return nil, err
	}
	full, err := v.path(t, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (v *worktreeView) Stat(rel string) (FileInfo, error) {
	t, err := v.tree()
	if err != nil {
		return FileInfo{}, err
	}
	full, err := v.path(t, rel)
	if err != nil {
		return FileInfo{}, err
	}
	st, err := os.Lstat(full)
	if err != nil {
		return FileInfo{}, err
	}
	return fileInfo(filepath.ToSlash(filepath.Clean(rel)), st), nil
}

func fileInfo(rel string, st fs.FileInfo) FileInfo {
	fi := FileInfo{Path: rel, Kind: "file", Size: st.Size(), Exec: st.Mode().Perm()&0o100 != 0}
	switch {
	case st.IsDir():
		fi.Kind, fi.Size, fi.Exec = "dir", 0, false
	case st.Mode()&os.ModeSymlink != 0:
		fi.Kind, fi.Exec = "symlink", false
	}
	return fi
}

func (v *worktreeView) ListDir(rel string) ([]DirEntry, error) {
	t, err := v.tree()
	if err != nil {
		return nil, err
	}
	full := t.Path
	if rel != "" && rel != "." {
		if full, err = v.path(t, rel); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".git" && (rel == "" || rel == ".") {
			continue // a worktree's link to its repository is not part of the agent's files
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		fi := fileInfo(e.Name(), st)
		out = append(out, DirEntry{Name: e.Name(), Kind: fi.Kind, Size: fi.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (v *worktreeView) WriteFile(rel string, data []byte, mode os.FileMode) error {
	t, err := v.tree()
	if err != nil {
		return err
	}
	full, err := v.path(t, rel)
	if err != nil {
		return err
	}
	if st, err := os.Lstat(full); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", vws.ErrSymlink, rel)
		}
		if mode == 0 {
			mode = st.Mode().Perm()
		}
	} else if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, data, mode); err != nil {
		return err
	}
	return os.Chmod(full, mode)
}

func (v *worktreeView) DeleteFile(rel string) error {
	t, err := v.tree()
	if err != nil {
		return err
	}
	full, err := v.path(t, rel)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		return err
	}
	// Git does not track empty directories and a virtual workspace cannot hold one, so a directory this left empty
	// goes too: both kinds of view then list the same names.
	for dir := filepath.Dir(full); dir != t.Path && strings.HasPrefix(dir, t.Path+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

func (v *worktreeView) Changes() ([]FileChange, error) { return v.m.ChangedFiles(v.id) }

func (v *worktreeView) Patch(rel string) (string, error) {
	if rel == "" {
		return v.m.Patch(v.id)
	}
	return v.m.PatchPath(v.id, rel)
}

func (v *worktreeView) BaseFile(rel string) ([]byte, bool, error) { return v.m.BaseContent(v.id, rel) }

func (v *worktreeView) Identity() (ViewIdentity, error) {
	t, err := v.tree()
	if err != nil {
		return ViewIdentity{}, err
	}
	id := ViewIdentity{AgentID: v.id, Kind: "worktree", Base: t.Base}
	// The tree of the worktree as it is now, from a private index so the agent's own index is not disturbed.
	if tree, err := v.m.worktreeTree(t); err == nil {
		id.Tree = tree
	}
	return id, nil
}

func (v *worktreeView) Source() integration.Source {
	if t, err := v.tree(); err == nil {
		return integration.DirSource{Dir: t.Path}
	}
	return integration.DirSource{}
}

// worktreeTree returns the tree id of everything the worktree holds, ignoring files Git ignores, without touching
// the worktree's own index.
func (m *Manager) worktreeTree(t Tree) (string, error) {
	idx, err := os.CreateTemp("", "relo-view-index-*")
	if err != nil {
		return "", err
	}
	name := idx.Name()
	_ = idx.Close()
	defer os.Remove(name)
	env := append(os.Environ(), "GIT_INDEX_FILE="+name)
	if _, err := gitEnv(t.Path, env, "read-tree", t.Base); err != nil {
		return "", err
	}
	if _, err := gitEnv(t.Path, env, "add", "-A"); err != nil {
		return "", err
	}
	return gitEnv(t.Path, env, "write-tree")
}

// virtualView is the WorkspaceView of an agent held as Git objects.
type virtualView struct {
	m *Manager
	w *vws.Workspace
}

func (v *virtualView) AgentID() string { return v.w.ID() }
func (v *virtualView) Kind() string    { return "virtual" }

// ReadFile returns the file as the agent sees it. While its tools run the directory is the truth, so what they
// wrote is imported first and the model never reads a version older than the compiler's.
func (v *virtualView) ReadFile(rel string) ([]byte, error) {
	if data, ok, err := v.w.ReadLive(rel); ok {
		return data, err
	}
	if err := v.m.Sync(v.w.ID()); err != nil {
		return nil, err
	}
	return v.w.ReadFile(rel)
}

func (v *virtualView) Stat(rel string) (FileInfo, error) {
	if err := v.m.Sync(v.w.ID()); err != nil {
		return FileInfo{}, err
	}
	fi, err := v.w.Stat(rel)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Path: fi.Path, Kind: fi.Kind, Size: fi.Size, Exec: fi.Mode == 0o100755}, nil
}

func (v *virtualView) ListDir(rel string) ([]DirEntry, error) {
	if err := v.m.Sync(v.w.ID()); err != nil {
		return nil, err
	}
	entries, err := v.w.ListDir(rel)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		kind := e.Kind
		if kind == "submodule" {
			kind = "dir"
		}
		out = append(out, DirEntry{Name: e.Name, Kind: kind, Size: e.Size})
	}
	return out, nil
}

func (v *virtualView) WriteFile(rel string, data []byte, mode os.FileMode) error {
	var m uint32
	if mode&0o100 != 0 {
		m = 0o755
	} else if mode != 0 {
		m = 0o644
	}
	return v.w.WriteFileMode(rel, data, m)
}

func (v *virtualView) DeleteFile(rel string) error { return v.w.DeleteFile(rel) }

func (v *virtualView) Changes() ([]FileChange, error) { return v.m.vChangedFiles(v.w.ID()) }

func (v *virtualView) Patch(rel string) (string, error) {
	if rel == "" {
		return v.m.vPatch(v.w.ID())
	}
	return v.m.vPatchPath(v.w.ID(), rel)
}

func (v *virtualView) BaseFile(rel string) ([]byte, bool, error) { return v.w.BaseContent(rel) }

func (v *virtualView) Identity() (ViewIdentity, error) {
	if err := v.m.Sync(v.w.ID()); err != nil {
		return ViewIdentity{}, err
	}
	info := v.w.Info()
	return ViewIdentity{AgentID: v.w.ID(), Kind: "virtual", Base: info.BaseCommit, Tree: info.EffectiveTree, Revision: info.Revision}, nil
}

func (v *virtualView) Source() integration.Source { return OverlaySource{w: v.w} }

var (
	_ WorkspaceView = (*worktreeView)(nil)
	_ WorkspaceView = (*virtualView)(nil)
)

// IsNotExist reports whether err means the file or directory is absent, whichever kind of view returned it.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || vws.IsNotExist(err)
}
