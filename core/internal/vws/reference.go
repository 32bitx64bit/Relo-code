package vws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// MaxReferencePaths bounds how many files one reference may name, so a message stays a pointer and never becomes a
// bulk copy.
const MaxReferencePaths = 32

// ErrNoReference means a reference does not name a revision the sender pinned: it was never sent, its sender's
// workspace is gone, or its commit is not the one that was pinned.
var ErrNoReference = errors.New("vws: the referenced change is not available")

// ChangeReference points at changes held in another agent's workspace. It is what a message carries instead of the
// code: the recipient learns that a change exists and where, and sees none of it until it chooses to import some
// paths. The commit is the sender's pinned checkpoint, so the reference stays exact while the sender keeps working.
type ChangeReference struct {
	SenderAgent string `json:"senderAgent"`
	Branch      string `json:"branch"`
	// Commit is the sender's checkpoint commit at Revision, pinned for as long as the sender's workspace lives.
	Commit     string `json:"commit"`
	Revision   uint64 `json:"revision"`
	BaseCommit string `json:"baseCommit"`
	// Paths are the files the sender named; every one differs from the sender's base.
	Paths []string `json:"paths"`
}

// Reference pins the workspace's current revision and describes the named paths of it. Each path must be a file the
// agent changed (added, modified or deleted relative to its base): a reference to an untouched file would only be a
// way to read the repository through another agent. Callers capture a materialized directory first.
func (w *Workspace) Reference(paths []string) (ChangeReference, error) {
	if len(paths) == 0 || len(paths) > MaxReferencePaths {
		return ChangeReference{}, fmt.Errorf("vws: a reference names 1 to %d paths", MaxReferencePaths)
	}
	clean := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		parts, err := splitFile(p)
		if err != nil {
			return ChangeReference{}, err
		}
		if name := join(parts); !seen[name] {
			seen[name] = true
			clean = append(clean, name)
		}
	}
	sort.Strings(clean)
	snap, err := w.Snapshot()
	if err != nil {
		return ChangeReference{}, err
	}
	changes, err := w.e.DiffTrees(snap.BaseTree, snap.Tree)
	if err != nil {
		return ChangeReference{}, err
	}
	changed := make(map[string]bool, len(changes))
	for _, c := range changes {
		changed[c.Path] = true
	}
	for _, p := range clean {
		if !changed[p] {
			return ChangeReference{}, pathErr(p, fmt.Errorf("not changed in this workspace, so there is nothing to reference"))
		}
	}
	return ChangeReference{SenderAgent: w.id, Branch: snap.Branch, Commit: snap.Commit, Revision: snap.Revision, BaseCommit: snap.BaseCommit, Paths: clean}, nil
}

// Resolve finds the sender's pinned snapshot a reference names. It accepts only a commit the sender's workspace
// really pinned at that revision, so a reference cannot be edited to point anywhere else in the repository.
func (e *Engine) Resolve(ref ChangeReference) (Snapshot, *Workspace, error) {
	w, ok := e.Get(ref.SenderAgent)
	if !ok {
		return Snapshot{}, nil, fmt.Errorf("%w: %s's workspace no longer exists", ErrNoReference, ref.SenderAgent)
	}
	snap, err := w.SnapshotAt(ref.Revision)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("%w: %v", ErrNoReference, err)
	}
	if snap.Commit != ref.Commit {
		return Snapshot{}, nil, fmt.Errorf("%w: revision %d is not the commit that was sent", ErrNoReference, ref.Revision)
	}
	return snap, w, nil
}

// subset returns want if it is non-empty and every entry is in the reference, otherwise all of the reference's paths.
func (ref ChangeReference) subset(want []string) ([]string, error) {
	if len(want) == 0 {
		return append([]string(nil), ref.Paths...), nil
	}
	in := make(map[string]bool, len(ref.Paths))
	for _, p := range ref.Paths {
		in[p] = true
	}
	out := make([]string, 0, len(want))
	seen := map[string]bool{}
	for _, p := range want {
		parts, err := splitFile(p)
		if err != nil {
			return nil, err
		}
		name := join(parts)
		if !in[name] {
			return nil, pathErr(name, fmt.Errorf("not named by the reference"))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ImportReference brings the named paths of another agent's change into this workspace's overlay. It is the only way
// code from another agent enters a workspace: a path the reference does not name is refused, and nothing is copied
// unless the caller asks. The write is refused if this workspace moved on from opt.ExpectRevision. For a workspace
// whose tools are running, use ImportReferenceIntoExec.
func (e *Engine) ImportReference(recipient string, ref ChangeReference, paths []string, opt ApplyOptions) (Result, []string, error) {
	dst, err := e.mustGet(recipient)
	if err != nil {
		return Result{}, nil, err
	}
	if recipient == ref.SenderAgent {
		return Result{}, nil, fmt.Errorf("vws: %s cannot import its own change", recipient)
	}
	names, err := ref.subset(paths)
	if err != nil {
		return Result{}, nil, err
	}
	snap, src, err := e.Resolve(ref)
	if err != nil {
		return Result{}, nil, err
	}
	if err := src.checkImportSize(snap, names, e.opts.MaxFileBytes*int64(MaxReferencePaths)); err != nil {
		return Result{}, nil, err
	}
	res, err := dst.ImportFrom(snap, names, opt)
	return res, names, err
}

// WriteReferenceInto writes the named paths of a referenced change into dir, which must be a directory that belongs
// to the recipient (a worktree, or an open materialization). Every write is checked to stay inside dir and never to
// follow a link. It returns the paths written; it changes nothing else, so for an open materialization the caller
// follows it with a Capture, as for any tool output.
func (e *Engine) WriteReferenceInto(dir string, ref ChangeReference, paths []string) ([]string, error) {
	names, err := ref.subset(paths)
	if err != nil {
		return nil, err
	}
	snap, src, err := e.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if err := src.checkImportSize(snap, names, e.opts.MaxFileBytes*int64(MaxReferencePaths)); err != nil {
		return nil, err
	}
	for _, name := range names {
		if err := writeImported(src, snap, dir, name); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// ImportReferenceIntoExec is ImportReference for a workspace whose directory is open: the directory is the authority
// there, so the files are written into it and then captured, exactly as if its tools had written them.
func (e *Engine) ImportReferenceIntoExec(ctx context.Context, recipient string, ref ChangeReference, paths []string) ([]string, error) {
	dst, err := e.mustGet(recipient)
	if err != nil {
		return nil, err
	}
	if recipient == ref.SenderAgent {
		return nil, fmt.Errorf("vws: %s cannot import its own change", recipient)
	}
	dst.execMu.Lock()
	defer dst.execMu.Unlock()
	dst.mu.Lock()
	if err := dst.live(); err != nil {
		dst.mu.Unlock()
		return nil, err
	}
	if dst.vw.ReadOnly {
		dst.mu.Unlock()
		return nil, ErrReadOnly
	}
	if dst.exec == nil {
		dst.mu.Unlock()
		return nil, fmt.Errorf("vws: %s has no open directory; import into its overlay instead", recipient)
	}
	dir := dst.exec.Dir
	dst.mu.Unlock()

	names, err := ref.subset(paths)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if err := dst.allow("import", name); err != nil {
			return nil, err
		}
	}
	if names, err = e.WriteReferenceInto(dir, ref, names); err != nil {
		return nil, err
	}
	if _, err := dst.captureLocked(ctx); err != nil {
		return nil, err
	}
	return names, nil
}

// checkImportSize refuses an import larger than limit bytes in total.
func (w *Workspace) checkImportSize(snap Snapshot, names []string, limit int64) error {
	var total int64
	for _, name := range names {
		fi, err := w.StatAt(snap, name)
		if err != nil {
			if IsNotExist(err) {
				continue // a deletion carries no bytes
			}
			return err
		}
		total += fi.Size
		if total > limit {
			return fmt.Errorf("%w: the referenced change is larger than %d bytes", ErrTooLarge, limit)
		}
	}
	return nil
}

// writeImported puts one path of the sender's snapshot into dir: a file, an inner link, or its deletion.
func writeImported(src *Workspace, snap Snapshot, dir, name string) error {
	fi, err := src.StatAt(snap, name)
	missing := err != nil && IsNotExist(err)
	if err != nil && !missing {
		return err
	}
	target := filepath.Join(dir, filepath.FromSlash(name))
	if err := noLinksOnPath(dir, name); err != nil {
		return err
	}
	if missing {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if fi.Kind == "dir" || fi.Kind == "submodule" {
		return pathErr(name, ErrNotRegular)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if fi.Kind == "symlink" {
		linkTarget, err := src.readLink(snap, name)
		if err != nil {
			return err
		}
		if !symlinkStaysInside(name, linkTarget) {
			return pathErr(name, ErrSymlink)
		}
		_ = os.Remove(target)
		return os.Symlink(linkTarget, target)
	}
	data, err := src.ReadFileAt(snap, name)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi.Mode == modeExec {
		mode = 0o755
	}
	if st, err := os.Lstat(target); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

// noLinksOnPath refuses a write whose path passes through a link, which a tool may have planted in the directory: the
// write would land wherever the link points.
func noLinksOnPath(dir, name string) error {
	cur := dir
	parts, err := splitFile(name)
	if err != nil {
		return err
	}
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return pathErr(name, ErrSymlink)
		}
		if !st.IsDir() {
			return pathErr(name, ErrNotDir)
		}
	}
	return nil
}

// readLink returns the target of the link stored at rel in a snapshot, without following it.
func (w *Workspace) readLink(s Snapshot, rel string) (string, error) {
	parts, err := splitFile(rel)
	if err != nil {
		return "", err
	}
	e, found, err := w.e.repo.lookup(s.Tree, parts)
	if err != nil {
		return "", err
	}
	if !found || e.Mode != modeSymlink {
		return "", pathErr(rel, ErrNotRegular)
	}
	target, err := w.e.repo.readBlob(e.OID)
	return string(target), err
}
