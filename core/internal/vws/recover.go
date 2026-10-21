package vws

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Problem is a workspace Recover could not fully restore.
type Problem struct {
	AgentID string `json:"agentId"`
	Err     string `json:"error"`
}

// RecoverReport says what a restart found.
type RecoverReport struct {
	// Recovered lists the workspaces restored from their refs.
	Recovered []string `json:"recovered,omitempty"`
	// Interrupted lists workspaces whose directory was still open when Relo stopped. Their unimported work was
	// captured when possible; Captured holds the result per agent.
	Interrupted []string                 `json:"interrupted,omitempty"`
	Captured    map[string]CaptureResult `json:"captured,omitempty"`
	Problems    []Problem                `json:"problems,omitempty"`
}

// Recover rebuilds every workspace from the refs alone. It validates the objects each one needs, restores the
// metadata that was published with them, marks materializations that were still open as interrupted and imports
// what their directories hold, and never deletes a directory that may hold work it could not import. It is safe to
// call repeatedly: workspaces the engine already holds are left as they are.
func (e *Engine) Recover(ctx context.Context) (RecoverReport, error) {
	report := RecoverReport{Captured: map[string]CaptureResult{}}
	refs, err := e.repo.listRefs(refPrefix)
	if err != nil {
		return report, err
	}
	var ids []string
	for name := range refs {
		rest := strings.TrimPrefix(name, refPrefix)
		if id, kind, ok := strings.Cut(rest, "/"); ok && kind == "overlay" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, live := e.Get(id); live {
			continue
		}
		w, err := e.restore(id, refs)
		if err != nil {
			report.Problems = append(report.Problems, Problem{AgentID: id, Err: err.Error()})
			continue
		}
		report.Recovered = append(report.Recovered, id)
		if w.exec != nil {
			report.Interrupted = append(report.Interrupted, id)
			res, err := w.resumeInterrupted(ctx)
			report.Captured[id] = res
			if err != nil {
				report.Problems = append(report.Problems, Problem{AgentID: id, Err: err.Error()})
			}
		}
	}
	// An exec pin without a workspace that is materialized is left over from a crash between its two writes.
	for name := range refs {
		rest := strings.TrimPrefix(name, refPrefix)
		id, kind, ok := strings.Cut(rest, "/")
		if !ok || kind != "exec" {
			continue
		}
		if w, live := e.Get(id); live {
			w.mu.Lock()
			open := w.exec != nil
			w.mu.Unlock()
			if open {
				continue
			}
		}
		_ = e.repo.updateRefs(refOp{ref: name})
	}
	return report, nil
}

// restore reads one workspace's checkpoint, validates it and registers it.
func (e *Engine) restore(id string, refs map[string]string) (*Workspace, error) {
	commit := refs[overlayRef(id)]
	vw, err := e.repo.readCheckpoint(commit)
	if err != nil {
		return nil, err
	}
	if vw.AgentID != id {
		return nil, fmt.Errorf("checkpoint names agent %q", vw.AgentID)
	}
	if err := e.validate(vw); err != nil {
		return nil, err
	}
	if refs[baseRef(id)] != vw.BaseCommit {
		// The pin is gone or stale (a crash between the two writes of a rebase): restore it from the checkpoint.
		if err := e.repo.updateRefs(refOp{ref: baseRef(id), new: vw.BaseCommit}); err != nil {
			return nil, err
		}
	}
	w := &Workspace{e: e, id: id, vw: vw, overlayCommit: commit}
	if vw.Exec != nil {
		ex := *vw.Exec
		w.exec = &ex
		w.vw.State = StateInterrupted
	}
	e.mu.Lock()
	if _, ok := e.spaces[id]; ok {
		e.mu.Unlock()
		return e.spaces[id], nil
	}
	e.spaces[id] = w
	e.mu.Unlock()
	return w, nil
}

// validate checks that every object a workspace needs is present: its base, its trees, and the blob behind every
// file it changed. A damaged workspace is reported rather than half restored.
func (e *Engine) validate(vw VirtualWorkspace) error {
	r := e.repo
	if !r.hasObject(vw.BaseCommit) {
		return fmt.Errorf("base commit %s is missing", short(vw.BaseCommit))
	}
	if _, err := r.loadTree(vw.BaseTree); err != nil {
		return fmt.Errorf("base tree: %w", err)
	}
	if _, err := r.loadTree(vw.EffectiveTree); err != nil {
		return fmt.Errorf("overlay tree: %w", err)
	}
	raw, err := r.diffRaw(vw.BaseTree, vw.EffectiveTree)
	if err != nil {
		return fmt.Errorf("overlay tree: %w", err)
	}
	for _, c := range raw {
		if c.NewOID != "" && c.NewMode != modeGitlink && !r.hasObject(c.NewOID) {
			return fmt.Errorf("blob %s for %s is missing", short(c.NewOID), c.Path)
		}
	}
	if vw.HeadCommit != "" && !r.hasObject(vw.HeadCommit) {
		return fmt.Errorf("head commit %s is missing", short(vw.HeadCommit))
	}
	return nil
}

// resumeInterrupted imports what an interrupted execution directory holds, then closes it. If the directory is
// gone the record is cleared. If the import fails the directory is left in place and the workspace stays
// interrupted, so the work can still be recovered by hand.
func (w *Workspace) resumeInterrupted(ctx context.Context) (CaptureResult, error) {
	w.mu.Lock()
	ex := *w.exec
	w.mu.Unlock()
	if _, err := os.Stat(ex.Dir); err != nil {
		w.execMu.Lock()
		defer w.execMu.Unlock()
		w.mu.Lock()
		defer w.mu.Unlock()
		next := w.vw
		next.Meta = copyMeta(w.vw.Meta)
		next.State, next.Exec = StateReady, nil
		if perr := w.publishLocked(next, refOp{ref: execRef(w.id)}); perr != nil {
			return CaptureResult{}, perr
		}
		w.exec = nil
		return CaptureResult{Revision: w.vw.Revision}, nil
	}
	return w.ReleaseExec(ctx, false)
}
