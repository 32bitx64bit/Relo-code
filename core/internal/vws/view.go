package vws

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// View is the workspace abstraction the rest of Relo programs against. It can be backed by a virtual overlay, by
// a Git worktree, or by the directory a tool is running in, and callers do not need to know which: the file state
// an agent sees, the diff it has produced and the commit it makes are all expressed here.
type View interface {
	// ReadFile returns the version of path this workspace sees, never whatever is in the user's folder.
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	// DeleteFile removes a file. The path stays gone: it never falls back to the base.
	DeleteFile(path string) error
	ListDir(path string) ([]Entry, error)
	Stat(path string) (FileInfo, error)
	// ReadDiff lists what the workspace differs from its base by.
	ReadDiff() ([]FileChange, error)
	// Commit records the current state and returns the commit id.
	Commit() (string, error)
}

var _ View = (*Workspace)(nil)

// treeSnapshot is the immutable state a read runs against.
type treeSnapshot struct {
	overlay, committed, base string
	revision                 uint64
}

func (w *Workspace) snapshotState() (treeSnapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return treeSnapshot{}, err
	}
	return treeSnapshot{overlay: w.vw.EffectiveTree, committed: w.committedTreeLocked(), base: w.vw.BaseTree, revision: w.vw.Revision}, nil
}

// Resolve looks a path up in the effective view and says which layer supplied it.
func (w *Workspace) Resolve(rel string) (ResolvedFile, error) {
	st, err := w.snapshotState()
	if err != nil {
		return ResolvedFile{}, err
	}
	return w.resolveAt(st, rel, true)
}

func (w *Workspace) resolveAt(st treeSnapshot, rel string, withContent bool) (ResolvedFile, error) {
	parts, err := splitFile(rel)
	if err != nil {
		return ResolvedFile{}, err
	}
	r := w.e.repo
	out := ResolvedFile{Path: join(parts), Revision: st.revision}
	ov, ovFound, err := r.lookup(st.overlay, parts)
	if err != nil {
		return out, err
	}
	cm, cmFound, err := r.lookup(st.committed, parts)
	if err != nil {
		cmFound = false
	}
	bs, bsFound, err := r.lookup(st.base, parts)
	if err != nil {
		bsFound = false
	}
	same := func(aFound bool, a treeEntry, bFound bool, b treeEntry) bool {
		return aFound == bFound && (!aFound || (a.OID == b.OID && a.Mode == b.Mode))
	}
	switch {
	case !same(ovFound, ov, cmFound, cm):
		out.Layer = "overlay"
	case !same(cmFound, cm, bsFound, bs):
		out.Layer = "branch"
	case ovFound:
		out.Layer = "base"
	}
	if !ovFound {
		return out, nil
	}
	out.Exists, out.BlobOID, out.Mode = true, ov.OID, ov.Mode
	if withContent && ov.isFile() {
		data, err := r.readBlob(ov.OID)
		if err != nil {
			return out, err
		}
		out.Content = data
	}
	return out, nil
}

// ReadFile returns a file's bytes. A link is followed only when it resolves inside the workspace.
func (w *Workspace) ReadFile(rel string) ([]byte, error) {
	st, err := w.snapshotState()
	if err != nil {
		return nil, err
	}
	return w.readAt(st.overlay, rel)
}

// ReadFileAt is ReadFile against a frozen snapshot, so repeated reads at one revision return identical bytes.
func (w *Workspace) ReadFileAt(s Snapshot, rel string) ([]byte, error) {
	if s.AgentID != w.id {
		return nil, fmt.Errorf("vws: snapshot belongs to %q, not %q", s.AgentID, w.id)
	}
	return w.readAt(s.Tree, rel)
}

func (w *Workspace) readAt(root, rel string) ([]byte, error) {
	parts, err := splitFile(rel)
	if err != nil {
		return nil, err
	}
	r := w.e.repo
	for hops := 0; hops < 8; hops++ {
		e, found, err := r.lookup(root, parts)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, notExist(join(parts))
		}
		switch {
		case e.isFile():
			if info, err := r.stat(e.OID); err == nil && info.size > w.e.opts.MaxFileBytes {
				return nil, pathErr(join(parts), ErrTooLarge)
			}
			return r.readBlob(e.OID)
		case e.isTree():
			return nil, pathErr(join(parts), ErrIsDir)
		case e.Mode == modeSymlink:
			target, err := r.readBlob(e.OID)
			if err != nil {
				return nil, err
			}
			if !symlinkStaysInside(join(parts), string(target)) {
				return nil, pathErr(join(parts), ErrSymlink)
			}
			dir := path.Dir(join(parts))
			if dir == "." {
				dir = ""
			}
			next, err := SplitPath(path.Join(dir, string(target)))
			if err != nil || len(next) == 0 {
				return nil, pathErr(join(parts), ErrSymlink)
			}
			parts = next
		default:
			return nil, pathErr(join(parts), ErrNotRegular)
		}
	}
	return nil, pathErr(rel, ErrSymlink) // a loop of links
}

// Stat describes one path, without following a final link.
func (w *Workspace) Stat(rel string) (FileInfo, error) {
	st, err := w.snapshotState()
	if err != nil {
		return FileInfo{}, err
	}
	return w.statAt(st.overlay, st.revision, rel)
}

// StatAt is Stat against a frozen snapshot.
func (w *Workspace) StatAt(s Snapshot, rel string) (FileInfo, error) {
	if s.AgentID != w.id {
		return FileInfo{}, fmt.Errorf("vws: snapshot belongs to %q, not %q", s.AgentID, w.id)
	}
	return w.statAt(s.Tree, s.Revision, rel)
}

func (w *Workspace) statAt(root string, revision uint64, rel string) (FileInfo, error) {
	parts, err := SplitPath(rel)
	if err != nil {
		return FileInfo{}, err
	}
	r := w.e.repo
	if len(parts) == 0 {
		return FileInfo{Path: "", Kind: "dir", Mode: modeTree, OID: root, Revision: revision}, nil
	}
	e, found, err := r.lookup(root, parts)
	if err != nil {
		return FileInfo{}, err
	}
	if !found {
		return FileInfo{}, notExist(join(parts))
	}
	fi := FileInfo{Path: join(parts), Kind: kindOf(e.Mode), Mode: e.Mode, OID: e.OID, Revision: revision}
	if !e.isTree() && e.Mode != modeGitlink {
		if info, err := r.stat(e.OID); err == nil {
			fi.Size = info.size
		}
	}
	return fi, nil
}

// ListDir lists one directory of the effective view in name order. An empty path or "." is the root.
func (w *Workspace) ListDir(rel string) ([]Entry, error) {
	st, err := w.snapshotState()
	if err != nil {
		return nil, err
	}
	return w.listAt(st.overlay, rel)
}

// ListDirAt is ListDir against a frozen snapshot.
func (w *Workspace) ListDirAt(s Snapshot, rel string) ([]Entry, error) {
	return w.listAt(s.Tree, rel)
}

func (w *Workspace) listAt(root, rel string) ([]Entry, error) {
	parts, err := SplitPath(rel)
	if err != nil {
		return nil, err
	}
	r := w.e.repo
	dir := root
	if len(parts) > 0 {
		e, found, err := r.lookup(root, parts)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, notExist(join(parts))
		}
		if !e.isTree() {
			return nil, pathErr(join(parts), ErrNotDir)
		}
		dir = e.OID
	}
	t, err := r.loadTree(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		ent := Entry{Name: e.Name, Kind: kindOf(e.Mode), Mode: e.Mode, OID: e.OID}
		if !e.isTree() && e.Mode != modeGitlink {
			if info, err := r.stat(e.OID); err == nil {
				ent.Size = info.size
			}
		}
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Files lists every file path in the effective view, in path order. It is the workspace's file inventory, for
// indexing and search that should see the agent's revision rather than the user's folder.
func (w *Workspace) Files() ([]string, error) {
	st, err := w.snapshotState()
	if err != nil {
		return nil, err
	}
	return w.filesAt(st.overlay)
}

func (w *Workspace) filesAt(root string) ([]string, error) {
	var files []string
	err := w.e.repo.walk(root, "", func(p string, e treeEntry) (bool, error) {
		if !e.isTree() {
			files = append(files, p)
		}
		return true, nil
	})
	return files, err
}

// ListFiles lists the effective file paths of an agent's workspace.
func (e *Engine) ListFiles(agentID string) ([]string, error) {
	w, err := e.mustGet(agentID)
	if err != nil {
		return nil, err
	}
	return w.Files()
}

// ReadFile reads a file from an agent's workspace.
func (e *Engine) ReadFile(agentID, rel string) ([]byte, error) {
	w, err := e.mustGet(agentID)
	if err != nil {
		return nil, err
	}
	return w.ReadFile(rel)
}

// WriteFile writes a file into an agent's workspace.
func (e *Engine) WriteFile(agentID, rel string, data []byte) error {
	w, err := e.mustGet(agentID)
	if err != nil {
		return err
	}
	return w.WriteFile(rel, data)
}

// DeleteFile removes a file from an agent's workspace.
func (e *Engine) DeleteFile(agentID, rel string) error {
	w, err := e.mustGet(agentID)
	if err != nil {
		return err
	}
	return w.DeleteFile(rel)
}

// Describe renders a workspace for logs.
func (w *Workspace) Describe() string {
	i := w.Info()
	return fmt.Sprintf("%s r%d %s base=%s tree=%s", i.AgentID, i.Revision, i.State, short(i.BaseCommit), short(i.EffectiveTree))
}

func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// joinStrings is strings.Join for error text.
func joinStrings(parts []string) string { return strings.Join(parts, ", ") }
