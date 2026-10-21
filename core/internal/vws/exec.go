package vws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Git lets a Materializer run git through the engine, so its spawns are counted and share its environment.
type Git interface {
	Run(dir string, env []string, stdin []byte, args ...string) ([]byte, error)
}

type gitAdapter struct{ r *repo }

func (g gitAdapter) Run(dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	return g.r.run(dir, env, stdin, args...)
}

// MaterializeRequest names a revision to turn into a directory.
type MaterializeRequest struct {
	AgentID string
	Dir     string
	// Commit is a commit whose tree is the revision to lay out; Tree is that tree.
	Commit string
	Tree   string
}

// Captured is one path a tool changed in a materialized directory. OID is a blob already in the object database.
type Captured struct {
	Path    string
	Mode    uint32
	OID     string
	Deleted bool
}

// Materializer lays a revision out as real files and reads back what tools changed. Git worktrees are the default
// backend; copy-on-write clones, OverlayFS or a mounted virtual filesystem can replace them without changing
// anything above this interface.
type Materializer interface {
	Name() string
	Materialize(ctx context.Context, g Git, req MaterializeRequest) error
	// Scan reports every difference between the directory and the revision it was materialized from. It must not
	// follow symlinks out of the directory.
	Scan(ctx context.Context, g Git, req MaterializeRequest) ([]Captured, error)
	Release(ctx context.Context, g Git, dir string) error
}

// Exec is one open materialization: the execution state of a workspace.
type Exec struct {
	ID      string `json:"id"`
	AgentID string `json:"agentId"`
	Dir     string `json:"dir"`
	Backend string `json:"backend"`
	// Revision, Commit and Tree identify the workspace revision the directory was cut from. The commit is pinned
	// at refs/relo/vws/<agent>/exec for as long as the directory exists.
	Revision uint64    `json:"revision"`
	Commit   string    `json:"commit"`
	Tree     string    `json:"tree"`
	Created  time.Time `json:"created"`
	ReadOnly bool      `json:"readOnly,omitempty"`
}

// Rejection is a path a capture refused to import, and why.
type Rejection struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// CaptureResult says what a capture imported.
type CaptureResult struct {
	Revision uint64      `json:"revision"`
	Changed  bool        `json:"changed"`
	Imported []string    `json:"imported,omitempty"`
	Rejected []Rejection `json:"rejected,omitempty"`
	Ignored  int         `json:"ignored,omitempty"`
}

// execRoot is where disposable execution directories live.
func (e *Engine) execRoot() string {
	if e.opts.ExecRoot != "" {
		return e.opts.ExecRoot
	}
	return filepath.Join(e.repo.root, ".relo", "worktrees")
}

// ExecDir is the directory an agent's materialization uses.
func (e *Engine) ExecDir(agentID string) string { return filepath.Join(e.execRoot(), agentID) }

// Exec returns the open materialization, if any.
func (w *Workspace) Exec() (Exec, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exec == nil {
		return Exec{}, false
	}
	return *w.exec, true
}

// Materialize lays the current revision out in a directory so tools that need real files can run, and pins the
// revision for as long as the directory exists. Calling it again while the directory is open returns the same
// materialization. While it is open the directory is authoritative for this agent: direct edits of the overlay
// are refused, and Capture imports what the tools did.
func (w *Workspace) Materialize(ctx context.Context) (Exec, error) {
	w.execMu.Lock()
	defer w.execMu.Unlock()
	w.mu.Lock()
	if err := w.live(); err != nil {
		w.mu.Unlock()
		return Exec{}, err
	}
	if w.exec != nil {
		if _, err := os.Stat(w.exec.Dir); err == nil {
			ex := *w.exec
			w.mu.Unlock()
			return ex, nil
		}
		// The directory was removed from outside; the record is stale.
		w.exec = nil
	}
	commit, tree, rev := w.overlayCommit, w.vw.EffectiveTree, w.vw.Revision
	readOnly := w.vw.ReadOnly
	w.mu.Unlock()

	dir := w.e.ExecDir(w.id)
	if err := ensureUnder(w.e.execRoot(), dir); err != nil {
		return Exec{}, err
	}
	m := w.e.opts.Materializer
	g := gitAdapter{w.e.repo}
	if _, err := os.Lstat(dir); err == nil {
		// Nothing of ours owns this path any more, so a leftover is cleared rather than adopted: adopting would
		// show the agent files that are not in its revision.
		if err := m.Release(ctx, g, dir); err != nil {
			return Exec{}, fmt.Errorf("vws: clearing a stale directory for %s: %w", w.id, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Exec{}, err
	}
	req := MaterializeRequest{AgentID: w.id, Dir: dir, Commit: commit, Tree: tree}
	// Pin before laying out: the revision must survive even if the process dies halfway through.
	if err := w.e.repo.updateRefs(refOp{ref: execRef(w.id), new: commit}); err != nil {
		return Exec{}, err
	}
	if err := m.Materialize(ctx, g, req); err != nil {
		_ = m.Release(context.Background(), g, dir)
		_ = w.e.repo.updateRefs(refOp{ref: execRef(w.id)})
		return Exec{}, fmt.Errorf("vws: materialize %s: %w", w.id, err)
	}
	ex := Exec{ID: fmt.Sprintf("%s-r%d", w.id, rev), AgentID: w.id, Dir: dir, Backend: m.Name(), Revision: rev, Commit: commit, Tree: tree, Created: w.e.opts.Now().UTC(), ReadOnly: readOnly}
	w.mu.Lock()
	defer w.mu.Unlock()
	next := w.vw
	next.Meta = copyMeta(w.vw.Meta)
	next.State = StateMaterialized
	next.Exec = &ex
	if err := w.publishLocked(next); err != nil {
		_ = m.Release(context.Background(), g, dir)
		_ = w.e.repo.updateRefs(refOp{ref: execRef(w.id)})
		return Exec{}, err
	}
	w.exec = &ex
	w.e.mats.Add(1)
	return ex, nil
}

// Capture imports what tools changed in the materialized directory into the overlay, as one new revision. Paths
// that are protected, ignored, oversized, links that leave the workspace, or submodules are not imported; a
// read-only workspace imports nothing. It can be called repeatedly while the directory stays open: each call
// describes the directory against the revision it was cut from, so it is idempotent.
func (w *Workspace) Capture(ctx context.Context) (CaptureResult, error) {
	w.execMu.Lock()
	defer w.execMu.Unlock()
	return w.captureLocked(ctx)
}

func (w *Workspace) captureLocked(ctx context.Context) (CaptureResult, error) {
	w.mu.Lock()
	if err := w.live(); err != nil {
		w.mu.Unlock()
		return CaptureResult{}, err
	}
	if w.exec == nil {
		rev := w.vw.Revision
		w.mu.Unlock()
		return CaptureResult{Revision: rev}, nil
	}
	ex := *w.exec
	w.mu.Unlock()

	r := w.e.repo
	req := MaterializeRequest{AgentID: w.id, Dir: ex.Dir, Commit: ex.Commit, Tree: ex.Tree}
	scanned, err := w.e.opts.Materializer.Scan(ctx, gitAdapter{r}, req)
	if err != nil {
		return CaptureResult{}, fmt.Errorf("vws: scan %s: %w", ex.Dir, err)
	}
	w.e.captures.Add(1)
	var res CaptureResult
	var changes []change
	for _, c := range scanned {
		parts, perr := splitFile(c.Path)
		if perr != nil {
			res.Rejected = append(res.Rejected, Rejection{Path: c.Path, Reason: reasonFor(perr)})
			continue
		}
		path := join(parts)
		if w.e.opts.Ignore != nil && w.e.opts.Ignore(path) {
			res.Ignored++
			continue
		}
		if ex.ReadOnly {
			res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: "read-only workspace"})
			continue
		}
		if err := w.allow("capture", path); err != nil {
			res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: err.Error()})
			continue
		}
		if c.Deleted {
			changes = append(changes, change{parts: parts})
			res.Imported = append(res.Imported, path)
			continue
		}
		mode := normalMode(c.Mode)
		switch mode {
		case modeGitlink:
			res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: "nested repositories are not imported"})
			continue
		case modeSymlink:
			target, rerr := r.readBlob(c.OID)
			if rerr != nil || !symlinkStaysInside(path, string(target)) {
				res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: "symlink leaves the workspace"})
				continue
			}
		case modeTree:
			continue
		default:
			if info, serr := r.stat(c.OID); serr != nil {
				res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: "object is missing"})
				continue
			} else if info.size > w.e.opts.MaxFileBytes {
				res.Rejected = append(res.Rejected, Rejection{Path: path, Reason: ErrTooLarge.Error()})
				continue
			}
		}
		changes = append(changes, change{parts: parts, leaf: &treeEntry{Mode: mode, OID: c.OID}})
		res.Imported = append(res.Imported, path)
	}
	if ex.ReadOnly {
		// Nothing is ever imported, and the overlay cannot have moved from the materialized tree.
		w.mu.Lock()
		res.Revision = w.vw.Revision
		w.mu.Unlock()
		return res, nil
	}
	applied, err := w.mutate("capture", ApplyOptions{}, true, func(cur string) (string, []string, error) {
		tree, err := r.applyChanges(ex.Tree, changes)
		return tree, res.Imported, err
	})
	if err != nil {
		return res, err
	}
	res.Revision, res.Changed = applied.Revision, applied.Changed
	return res, nil
}

// ReadLive reads one file as the tools see it, without importing the whole directory first. A reader that wants a
// single file would otherwise pay for a scan of every file in the directory on each read. ok is false whenever the
// answer could differ from what an import would produce, and the caller captures instead: no open directory, a
// path the overlay does not hold yet (an import decides whether that is new work or ignored), a link, a directory,
// a file too large to import, or a path an import would refuse.
func (w *Workspace) ReadLive(rel string) (data []byte, ok bool, err error) {
	parts, perr := splitFile(rel)
	if perr != nil {
		return nil, false, nil
	}
	path := join(parts)
	// Capture, materialize and release hold execMu, so the directory cannot be removed under this read.
	w.execMu.Lock()
	defer w.execMu.Unlock()
	w.mu.Lock()
	if w.live() != nil || w.exec == nil || w.exec.ReadOnly {
		w.mu.Unlock()
		return nil, false, nil
	}
	ex, overlay := *w.exec, w.vw.EffectiveTree
	w.mu.Unlock()
	if w.allow("capture", path) != nil || (w.e.opts.Ignore != nil && w.e.opts.Ignore(path)) {
		return nil, false, nil
	}
	// A tracked file's edits and deletion are always imported, so the directory is the truth for it. A new file is
	// not: whether it is imported depends on the ignore rules.
	entry, found, lerr := w.e.repo.lookup(overlay, parts)
	if lerr != nil || !found || !entry.isFile() {
		return nil, false, nil
	}
	if st, derr := os.Stat(ex.Dir); derr != nil || !st.IsDir() { // removed from outside: a capture reports it
		return nil, false, nil
	}
	if noLinksOnPath(ex.Dir, path) != nil {
		return nil, false, nil
	}
	full := filepath.Join(ex.Dir, filepath.FromSlash(path))
	fi, serr := os.Lstat(full)
	switch {
	case os.IsNotExist(serr):
		return nil, true, notExist(path) // the tools deleted it
	case serr != nil, !fi.Mode().IsRegular(), fi.Size() > w.e.opts.MaxFileBytes:
		return nil, false, nil
	}
	data, rerr := os.ReadFile(full)
	if rerr != nil {
		return nil, false, nil
	}
	return data, true, nil
}

// ReleaseExec captures what the tools did (unless discard is set) and removes the directory. If the capture
// fails the directory is kept, so nothing an agent wrote is lost to a failed import.
func (w *Workspace) ReleaseExec(ctx context.Context, discard bool) (CaptureResult, error) {
	w.execMu.Lock()
	defer w.execMu.Unlock()
	var res CaptureResult
	if !discard {
		var err error
		if res, err = w.captureLocked(ctx); err != nil {
			return res, err
		}
	}
	w.mu.Lock()
	ex := w.exec
	w.mu.Unlock()
	if ex == nil {
		return res, nil
	}
	if err := w.e.opts.Materializer.Release(ctx, gitAdapter{w.e.repo}, ex.Dir); err != nil {
		return res, fmt.Errorf("vws: release %s: %w", ex.Dir, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	next := w.vw
	next.Meta = copyMeta(w.vw.Meta)
	next.State = StateReady
	next.Exec = nil
	if err := w.publishLocked(next, refOp{ref: execRef(w.id)}); err != nil {
		return res, err
	}
	w.exec = nil
	res.Revision = w.vw.Revision
	return res, nil
}

// Discard drops the directory without importing anything.
func (w *Workspace) DiscardExec(ctx context.Context) error {
	_, err := w.ReleaseExec(ctx, true)
	return err
}

func reasonFor(err error) string {
	switch {
	case errors.Is(err, ErrProtected):
		return "protected metadata path"
	case errors.Is(err, ErrInvalidPath):
		return "invalid path"
	}
	return err.Error()
}

// ensureUnder refuses a directory that would resolve outside root, including through a symlinked parent.
func ensureUnder(root, target string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	probe, suffix := target, []string{}
	for {
		if resolved, rerr := filepath.EvalSymlinks(probe); rerr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			rel, relErr := filepath.Rel(resolvedRoot, resolved)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("vws: directory escapes %s: %s", root, target)
			}
			return nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("vws: cannot resolve %s", target)
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}
