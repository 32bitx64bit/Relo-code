package vws

import "fmt"

// Snapshot is an immutable, pinned revision of a workspace. Reads against it return identical bytes however the
// workspace changes afterwards, and the objects it needs cannot be garbage collected until it is released.
type Snapshot struct {
	AgentID  string `json:"agentId"`
	Revision uint64 `json:"revision"`
	Tree     string `json:"tree"`
	// Commit is the checkpoint commit of this revision.
	Commit     string `json:"commit"`
	BaseCommit string `json:"baseCommit"`
	BaseTree   string `json:"baseTree"`
	Branch     string `json:"branch"`
}

// Snapshot freezes the current revision and pins it.
func (w *Workspace) Snapshot() (Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{
		AgentID: w.id, Revision: w.vw.Revision, Tree: w.vw.EffectiveTree, Commit: w.overlayCommit,
		BaseCommit: w.vw.BaseCommit, BaseTree: w.vw.BaseTree, Branch: w.vw.Branch,
	}
	if err := w.e.repo.updateRefs(refOp{ref: snapRef(w.id, s.Revision), new: s.Commit}); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// SnapshotAt returns the pinned snapshot of an earlier revision, for a proposal that must be read exactly as it was
// proposed however the workspace has moved on. It fails if that revision was never pinned or has been released.
func (w *Workspace) SnapshotAt(rev uint64) (Snapshot, error) {
	r := w.e.repo
	commit, err := r.resolveRef(snapRef(w.id, rev))
	if err != nil || commit == "" {
		return Snapshot{}, fmt.Errorf("vws: %s has no pinned snapshot of r%d", w.id, rev)
	}
	vw, err := r.readCheckpoint(commit)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		AgentID: w.id, Revision: rev, Tree: vw.EffectiveTree, Commit: commit,
		BaseCommit: vw.BaseCommit, BaseTree: vw.BaseTree, Branch: vw.Branch,
	}, nil
}

// ReleaseSnapshot drops the pin of a snapshot. Its objects become collectable once nothing else holds them.
func (w *Workspace) ReleaseSnapshot(s Snapshot) error {
	return w.e.repo.updateRefs(refOp{ref: snapRef(w.id, s.Revision)})
}

// Snapshot freezes an agent's current revision.
func (e *Engine) Snapshot(agentID string) (Snapshot, error) {
	w, err := e.mustGet(agentID)
	if err != nil {
		return Snapshot{}, err
	}
	return w.Snapshot()
}

// ImportFrom copies the named paths from another workspace's snapshot into this overlay. It is the explicit
// visibility operation: nothing from another agent reaches this workspace except through it, and a message saying
// an API changed is information, not an import. A path the source does not have is removed here. The write is
// refused if this workspace moved on from expect.
func (w *Workspace) ImportFrom(src Snapshot, paths []string, opt ApplyOptions) (Result, error) {
	if len(paths) == 0 {
		return Result{Revision: w.Info().Revision}, nil
	}
	r := w.e.repo
	var changes []change
	var names []string
	for _, p := range paths {
		parts, err := splitFile(p)
		if err != nil {
			return Result{}, err
		}
		if err := w.allow("import", join(parts)); err != nil {
			return Result{}, err
		}
		e, found, err := r.lookup(src.Tree, parts)
		if err != nil {
			return Result{}, err
		}
		if found && e.Mode == modeSymlink {
			target, rerr := r.readBlob(e.OID)
			if rerr != nil || !symlinkStaysInside(join(parts), string(target)) {
				return Result{}, pathErr(join(parts), ErrSymlink)
			}
		}
		c := change{parts: parts}
		if found {
			ent := e
			c.leaf = &ent
		}
		changes = append(changes, c)
		names = append(names, join(parts))
	}
	res, err := w.mutate("import", opt, false, func(cur string) (string, []string, error) {
		tree, err := r.applyChanges(cur, changes)
		return tree, names, err
	})
	if err != nil {
		return res, err
	}
	if res.Changed {
		_ = w.SetMeta(fmt.Sprintf("import:%s", src.AgentID), fmt.Sprintf("r%d %s", src.Revision, src.Commit))
	}
	return res, nil
}
