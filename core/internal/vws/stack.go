package vws

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// StackResult says whether one patch applied, and why not when it did not.
type StackResult struct {
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// ErrRebaseConflict says the workspace's own edits and the new base touch the same lines. Nothing was changed.
type ErrRebaseConflict struct{ Paths []string }

func (e ErrRebaseConflict) Error() string {
	return "vws: rebase conflicts in " + joinStrings(e.Paths)
}

// Stack applies patches made by other agents, which the user has not accepted yet, to this workspace and makes the
// result its new base. The agent then sees the code it is meant to test, review or build on, and its own diff
// holds only what it adds afterwards. A patch that does not apply is skipped and reported; the rest still land.
// Stacking is Relo's own operation, so it is allowed on a read-only workspace.
//
// The patches are applied to a private temporary index: neither the user's index nor a working tree is touched.
func (w *Workspace) Stack(patches []string) ([]StackResult, error) {
	results := make([]StackResult, len(patches))
	r := w.e.repo
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return nil, err
	}
	if w.exec != nil {
		return nil, ErrMaterialized
	}
	if w.vw.HeadCommit != "" {
		return nil, fmt.Errorf("vws: %s already committed; its base cannot be re-stacked", w.id)
	}
	idx, err := os.CreateTemp("", "relo-vws-index-*")
	if err != nil {
		return nil, err
	}
	_ = idx.Close()
	defer os.Remove(idx.Name())
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	if _, err := r.gitIn(r.root, env, nil, "read-tree", w.vw.EffectiveTree); err != nil {
		return nil, err
	}
	var failed []string
	applied := 0
	for i, patch := range patches {
		if strings.TrimSpace(patch) == "" {
			results[i] = StackResult{Applied: true}
			continue
		}
		if _, perr := r.gitIn(r.root, env, []byte(patch), "apply", "--cached", "--whitespace=nowarn"); perr != nil {
			failed = append(failed, perr.Error())
			results[i] = StackResult{Error: perr.Error()}
			continue
		}
		applied++
		results[i] = StackResult{Applied: true}
	}
	if applied > 0 {
		out, err := r.gitIn(r.root, env, nil, "write-tree")
		if err != nil {
			return results, err
		}
		tree := strings.TrimSpace(string(out))
		if err := w.rebaseLocked(tree, "relo: changes this agent builds on", ""); err != nil {
			return results, err
		}
	}
	if len(failed) > 0 {
		return results, fmt.Errorf("%d of %d patches did not apply: %s", len(failed), len(patches), failed[0])
	}
	return results, nil
}

// rebaseLocked makes a new commit holding tree the workspace's base (parented on the old base unless parent is
// given) and resets the overlay to it. The old base pin moves with the overlay in one transaction.
func (w *Workspace) rebaseLocked(tree, message, parent string) error {
	r := w.e.repo
	if parent == "" {
		parent = w.vw.BaseCommit
	}
	now := w.e.opts.Now().UTC()
	commit, err := r.writeCommit(tree, []string{parent}, message, "", now.Unix())
	if err != nil {
		return err
	}
	return w.adoptBaseLocked(commit, tree)
}

// adoptBaseLocked points the workspace at commit as its base with an overlay equal to it, publishing a new revision.
func (w *Workspace) adoptBaseLocked(commit, tree string) error {
	old := w.vw.BaseCommit
	next := w.vw
	next.Meta = copyMeta(w.vw.Meta)
	next.BaseCommit, next.BaseTree = commit, tree
	next.EffectiveTree = tree
	next.HeadCommit, next.HeadTree = "", ""
	next.Revision++
	extra := []refOp{{ref: baseRef(w.id), new: commit, old: old}}
	if w.vw.HeadCommit != "" {
		extra = append(extra, refOp{ref: branchRef(w.vw.Branch), old: w.vw.HeadCommit})
	}
	return w.publishLocked(next, extra...)
}

// Recut resets an agent that has not started to a new base, discarding whatever was in its overlay. The caller
// re-stacks the agent's recorded dependencies afterwards. It never touches the user's workspace.
func (w *Workspace) Recut(baseRev string) error {
	if strings.TrimSpace(baseRev) == "" {
		return fmt.Errorf("vws: no base to re-cut %q onto", w.id)
	}
	base, err := w.e.resolveCommit(baseRev)
	if err != nil {
		return err
	}
	ci, err := w.e.repo.readCommit(base)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return err
	}
	if w.exec != nil {
		return ErrMaterialized
	}
	return w.adoptBaseLocked(base, ci.Tree)
}

// RebaseOnto moves the workspace onto a newer base while keeping the agent's edits: the edits (base to overlay) are
// merged three-way into the new base. This is how a dependent agent advances when its parent has changed, and it
// is always explicit, so the conflicts it can hit are reproducible. On conflict nothing changes and the paths are
// reported.
func (w *Workspace) RebaseOnto(newBaseRev string) error {
	newBase, err := w.e.resolveCommit(newBaseRev)
	if err != nil {
		return err
	}
	ci, err := w.e.repo.readCommit(newBase)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return err
	}
	if w.exec != nil {
		return ErrMaterialized
	}
	if w.vw.ReadOnly {
		return ErrReadOnly
	}
	if newBase == w.vw.BaseCommit {
		return nil
	}
	r := w.e.repo
	merged := ci.Tree
	if w.vw.EffectiveTree != w.vw.BaseTree {
		out, err := r.run(r.root, nil, nil, "merge-tree", "--write-tree", "--name-only", "-z", "--merge-base="+w.vw.BaseCommit, newBase, w.overlayCommit)
		if err != nil {
			var ge *gitError
			if errors.As(err, &ge) && len(out) > 0 {
				fields := strings.Split(string(out), "\x00")
				var paths []string
				for _, f := range fields[1:] {
					if f == "" {
						break
					}
					paths = append(paths, f)
				}
				if len(paths) > 0 {
					return ErrRebaseConflict{Paths: paths}
				}
			}
			return err
		}
		merged = strings.TrimSpace(strings.SplitN(string(out), "\x00", 2)[0])
		if !r.isOID(merged) {
			return fmt.Errorf("vws: git merge-tree returned %q", merged)
		}
	}
	old := w.vw.BaseCommit
	next := w.vw
	next.Meta = copyMeta(w.vw.Meta)
	next.BaseCommit, next.BaseTree = newBase, ci.Tree
	next.EffectiveTree = merged
	next.HeadCommit, next.HeadTree = "", ""
	next.Revision++
	extra := []refOp{{ref: baseRef(w.id), new: newBase, old: old}}
	if w.vw.HeadCommit != "" {
		extra = append(extra, refOp{ref: branchRef(w.vw.Branch), old: w.vw.HeadCommit})
	}
	return w.publishLocked(next, extra...)
}
