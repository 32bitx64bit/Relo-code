package vws

import (
	"fmt"
	"strings"
)

// ApplyOptions guards a write with the revision it was based on.
type ApplyOptions struct {
	// ExpectRevision, when set, makes the write fail with ErrStaleRevision unless the workspace is still at that
	// revision. Writers that read, think and write back use it so a stale decision is never applied silently.
	ExpectRevision *uint64
}

// Result is what a mutation published.
type Result struct {
	// Revision is the workspace revision after the call. It is unchanged when the call changed nothing.
	Revision uint64
	Tree     string
	Changed  bool
}

// Expect is a convenience for ApplyOptions.
func Expect(rev uint64) ApplyOptions { return ApplyOptions{ExpectRevision: &rev} }

// mutate is the one place the overlay changes. build receives the current tree and returns the new one; the
// result is published as a new revision only if the tree really differs. The workspace lock is held throughout,
// so writers to one overlay are serialised, and the optional expected revision protects writers that are not.
// force lets the engine's own capture write while a directory is open.
func (w *Workspace) mutate(kind string, opt ApplyOptions, force bool, build func(cur string) (string, []string, error)) (Result, error) {
	w.mu.Lock()
	if err := w.live(); err != nil {
		w.mu.Unlock()
		return Result{}, err
	}
	if w.vw.ReadOnly {
		w.mu.Unlock()
		return Result{}, ErrReadOnly
	}
	if w.exec != nil && !force {
		w.mu.Unlock()
		return Result{}, ErrMaterialized
	}
	if opt.ExpectRevision != nil && *opt.ExpectRevision != w.vw.Revision {
		rev := w.vw.Revision
		w.mu.Unlock()
		return Result{Revision: rev}, fmt.Errorf("%w (expected r%d, at r%d)", ErrStaleRevision, *opt.ExpectRevision, rev)
	}
	cur := w.vw.EffectiveTree
	tree, paths, err := build(cur)
	if err != nil {
		w.mu.Unlock()
		return Result{Revision: w.vw.Revision}, err
	}
	if tree == cur {
		res := Result{Revision: w.vw.Revision, Tree: cur}
		w.mu.Unlock()
		return res, nil
	}
	next := w.vw
	next.Meta = copyMeta(w.vw.Meta)
	next.EffectiveTree = tree
	next.Revision++
	if err := w.publishLocked(next); err != nil {
		w.mu.Unlock()
		return Result{Revision: w.vw.Revision}, err
	}
	ev := Event{AgentID: w.id, Revision: w.vw.Revision, Kind: kind, Paths: paths, Tree: tree, Commit: w.overlayCommit, At: w.vw.Updated}
	res := Result{Revision: w.vw.Revision, Tree: tree, Changed: true}
	w.mu.Unlock()
	w.e.revisions.Add(1)
	w.e.emit(ev)
	return res, nil
}

// preparedOp is an Op after validation, with its blob already stored.
type preparedOp struct {
	Op
	parts, to []string
	oid       string
}

func (w *Workspace) allow(op, path string) error {
	if w.e.opts.Allow != nil {
		return w.e.opts.Allow(w.id, op, path)
	}
	return nil
}

// prepare validates one op and stores the blob it carries, before any lock is taken.
func (w *Workspace) prepare(op Op) (preparedOp, error) {
	p := preparedOp{Op: op}
	var err error
	switch op.Kind {
	case "write", "delete", "chmod", "symlink", "restore":
		if p.parts, err = splitFile(op.Path); err != nil {
			return p, err
		}
	case "rename":
		if p.parts, err = splitFile(op.Path); err != nil {
			return p, err
		}
		if p.to, err = splitFile(op.To); err != nil {
			return p, err
		}
	default:
		return p, fmt.Errorf("vws: unknown operation %q", op.Kind)
	}
	if err := w.allow(op.Kind, join(p.parts)); err != nil {
		return p, err
	}
	if op.Kind == "rename" {
		if err := w.allow(op.Kind, join(p.to)); err != nil {
			return p, err
		}
	}
	r := w.e.repo
	switch op.Kind {
	case "write":
		switch {
		case op.OID != "":
			info, err := r.stat(op.OID)
			if err != nil || info.typ != "blob" {
				return p, fmt.Errorf("vws: %s is not a blob in this repository", op.OID)
			}
			if info.size > w.e.opts.MaxFileBytes {
				return p, pathErr(join(p.parts), ErrTooLarge)
			}
			p.oid = op.OID
		default:
			if int64(len(op.Data)) > w.e.opts.MaxFileBytes {
				return p, pathErr(join(p.parts), ErrTooLarge)
			}
			if p.oid, err = r.hashBlob(join(p.parts), op.Data); err != nil {
				return p, err
			}
		}
	case "symlink":
		if !symlinkStaysInside(join(p.parts), op.To) {
			return p, pathErr(join(p.parts), ErrSymlink)
		}
		if p.oid, err = r.writeObject("blob", []byte(op.To)); err != nil {
			return p, err
		}
	}
	return p, nil
}

// changesFor turns one prepared op into tree changes against the tree it will be applied to.
func (w *Workspace) changesFor(cur string, p preparedOp) ([]change, error) {
	r := w.e.repo
	name := join(p.parts)
	existing, found, err := r.lookup(cur, p.parts)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case "write":
		mode := modeFile
		if found {
			switch {
			case existing.isTree():
				return nil, pathErr(name, ErrIsDir)
			case existing.Mode == modeSymlink:
				return nil, pathErr(name, ErrSymlink)
			case existing.Mode == modeGitlink:
				return nil, pathErr(name, ErrNotRegular)
			}
			mode = existing.Mode
		}
		if p.Mode != 0 {
			if m := normalMode(p.Mode); m == modeFile || m == modeExec {
				mode = m
			} else {
				return nil, pathErr(name, ErrNotRegular)
			}
		}
		return []change{{parts: p.parts, leaf: &treeEntry{Mode: mode, OID: p.oid}}}, nil
	case "delete":
		if !found {
			return nil, notExist(name)
		}
		if existing.isTree() {
			return nil, pathErr(name, ErrIsDir)
		}
		return []change{{parts: p.parts}}, nil
	case "chmod":
		if !found {
			return nil, notExist(name)
		}
		if !existing.isFile() {
			return nil, pathErr(name, ErrNotRegular)
		}
		mode := normalMode(p.Mode)
		if mode != modeFile && mode != modeExec {
			return nil, pathErr(name, ErrNotRegular)
		}
		return []change{{parts: p.parts, leaf: &treeEntry{Mode: mode, OID: existing.OID}}}, nil
	case "symlink":
		if found {
			switch {
			case existing.isTree():
				return nil, pathErr(name, ErrIsDir)
			case existing.Mode != modeSymlink:
				return nil, pathErr(name, ErrExists)
			}
		}
		return []change{{parts: p.parts, leaf: &treeEntry{Mode: modeSymlink, OID: p.oid}}}, nil
	case "rename":
		if !found {
			return nil, notExist(name)
		}
		to := join(p.to)
		if name == to {
			return nil, nil
		}
		if strings.HasPrefix(to, name+"/") {
			return nil, pathErr(to, ErrInvalidPath)
		}
		dest, destFound, err := r.lookup(cur, p.to)
		if err != nil {
			return nil, err
		}
		if destFound {
			switch {
			case dest.isTree():
				return nil, pathErr(to, ErrIsDir)
			case existing.isTree():
				return nil, pathErr(to, ErrNotDir)
			}
		}
		moved := existing
		return []change{{parts: p.parts}, {parts: p.to, leaf: &moved}}, nil
	case "restore":
		base, baseFound, err := r.lookup(w.vw.BaseTree, p.parts)
		if err != nil {
			baseFound = false
		}
		if !baseFound {
			if !found {
				return nil, nil
			}
			return []change{{parts: p.parts}}, nil
		}
		restored := base
		if found && existing.isTree() && !base.isTree() {
			return []change{{parts: p.parts}, {parts: p.parts, leaf: &restored}}, nil
		}
		return []change{{parts: p.parts, leaf: &restored}}, nil
	}
	return nil, fmt.Errorf("vws: unknown operation %q", p.Kind)
}

// Apply runs ops in order as one published revision. Either every op applies or none does.
func (w *Workspace) Apply(ops []Op, opt ApplyOptions) (Result, error) {
	if len(ops) == 0 {
		return Result{Revision: w.Info().Revision}, nil
	}
	if w.Info().ReadOnly {
		return Result{}, ErrReadOnly
	}
	prepared := make([]preparedOp, len(ops))
	for i, op := range ops {
		p, err := w.prepare(op)
		if err != nil {
			return Result{}, err
		}
		prepared[i] = p
	}
	kind := "batch"
	if len(ops) == 1 {
		kind = ops[0].Kind
	}
	return w.mutate(kind, opt, false, func(cur string) (string, []string, error) {
		var paths []string
		for _, p := range prepared {
			changes, err := w.changesFor(cur, p)
			if err != nil {
				return "", nil, err
			}
			if len(changes) == 0 {
				continue
			}
			if cur, err = w.e.repo.applyChanges(cur, changes); err != nil {
				return "", nil, err
			}
			paths = append(paths, join(p.parts))
			if p.Kind == "rename" {
				paths = append(paths, join(p.to))
			}
		}
		return cur, paths, nil
	})
}

// WriteFile writes a file, creating its directories. An existing file keeps its mode.
func (w *Workspace) WriteFile(rel string, data []byte) error {
	_, err := w.Apply([]Op{{Kind: "write", Path: rel, Data: data}}, ApplyOptions{})
	return err
}

// WriteFileMode is WriteFile with an explicit mode (0755 makes the file executable).
func (w *Workspace) WriteFileMode(rel string, data []byte, mode uint32) error {
	_, err := w.Apply([]Op{{Kind: "write", Path: rel, Data: data, Mode: mode}}, ApplyOptions{})
	return err
}

// WriteFileAt writes a file only if the workspace is still at revision rev.
func (w *Workspace) WriteFileAt(rel string, data []byte, rev uint64) (Result, error) {
	return w.Apply([]Op{{Kind: "write", Path: rel, Data: data}}, Expect(rev))
}

// DeleteFile removes a file. The deletion is a tombstone: the path does not fall back to the base.
func (w *Workspace) DeleteFile(rel string) error {
	_, err := w.Apply([]Op{{Kind: "delete", Path: rel}}, ApplyOptions{})
	return err
}

// Rename moves a file or directory.
func (w *Workspace) Rename(from, to string) error {
	_, err := w.Apply([]Op{{Kind: "rename", Path: from, To: to}}, ApplyOptions{})
	return err
}

// Chmod sets or clears the executable bit of a regular file.
func (w *Workspace) Chmod(rel string, mode uint32) error {
	_, err := w.Apply([]Op{{Kind: "chmod", Path: rel, Mode: mode}}, ApplyOptions{})
	return err
}

// Symlink creates a link whose target stays inside the workspace.
func (w *Workspace) Symlink(rel, target string) error {
	_, err := w.Apply([]Op{{Kind: "symlink", Path: rel, To: target}}, ApplyOptions{})
	return err
}

// RestoreToBase puts paths back to the base snapshot's version, deleting those the base did not have. It is how an
// agent's change to one file is rejected without touching its others.
func (w *Workspace) RestoreToBase(paths ...string) (Result, error) {
	ops := make([]Op, 0, len(paths))
	for _, p := range paths {
		ops = append(ops, Op{Kind: "restore", Path: p})
	}
	res, err := w.Apply(ops, ApplyOptions{})
	return res, err
}

// Commit records the current state on the agent's branch and returns the commit. When nothing changed since the
// last commit it returns that commit, which is the base when the agent never committed. While a directory is open
// the commit records the overlay as last imported, so call Capture first to include what the tools wrote since;
// the directory stays authoritative and later captures carry on from it.
func (w *Workspace) Commit() (string, error) { return w.CommitMessage("relo: agent changes") }

// CommitMessage is Commit with a message. The branch moves with an expected old value, so a branch that someone
// else moved is reported, not overwritten.
func (w *Workspace) CommitMessage(msg string) (string, error) {
	w.mu.Lock()
	if err := w.live(); err != nil {
		w.mu.Unlock()
		return "", err
	}
	parent := w.vw.HeadCommit
	if parent == "" {
		parent = w.vw.BaseCommit
	}
	if w.vw.EffectiveTree == w.committedTreeLocked() {
		w.mu.Unlock()
		return parent, nil
	}
	if w.vw.ReadOnly {
		w.mu.Unlock()
		return "", ErrReadOnly
	}
	now := w.e.opts.Now().UTC()
	commit, err := w.e.repo.writeCommit(w.vw.EffectiveTree, []string{parent}, msg, "", now.Unix())
	if err != nil {
		w.mu.Unlock()
		return "", err
	}
	old := w.vw.HeadCommit
	if old == "" {
		old = w.e.repo.zero()
	}
	next := w.vw
	next.Meta = copyMeta(w.vw.Meta)
	next.HeadCommit, next.HeadTree = commit, w.vw.EffectiveTree
	if err := w.publishLocked(next, refOp{ref: branchRef(w.vw.Branch), new: commit, old: old}); err != nil {
		w.mu.Unlock()
		return "", err
	}
	ev := Event{AgentID: w.id, Revision: w.vw.Revision, Kind: "commit", Tree: w.vw.EffectiveTree, Commit: commit, At: now}
	w.mu.Unlock()
	w.e.emit(ev)
	return commit, nil
}

// Commit commits an agent's workspace.
func (e *Engine) Commit(agentID string) (string, error) {
	w, err := e.mustGet(agentID)
	if err != nil {
		return "", err
	}
	return w.Commit()
}
