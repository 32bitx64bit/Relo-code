package vws

import (
	"strconv"
	"strings"
)

// rawChange is one line of `git diff-tree --raw`.
type rawChange struct {
	Path             string
	Status           byte // A, M, D, T
	OldMode, NewMode uint32
	OldOID, NewOID   string
}

func (c rawChange) kind() string {
	switch c.Status {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	}
	return "modified"
}

// diffRaw compares two trees. It reads nothing from the working directory, so it is identical for any two trees
// however they came about.
func (r *repo) diffRaw(a, b string) ([]rawChange, error) {
	out, err := r.gitRaw("diff-tree", "-r", "--no-renames", "--raw", "--no-abbrev", "-z", a, b)
	if err != nil {
		return nil, err
	}
	return r.parseRaw(out), nil
}

// parseRaw reads `git diff-tree|diff-index --raw -z` output: ":old new oldoid newoid S\0path\0" records.
func (r *repo) parseRaw(out []byte) []rawChange {
	fields := strings.Split(string(out), "\x00")
	var changes []rawChange
	for i := 0; i < len(fields); {
		meta := fields[i]
		if !strings.HasPrefix(meta, ":") || i+1 >= len(fields) {
			i++
			continue
		}
		parts := strings.Fields(meta[1:])
		if len(parts) >= 5 {
			oldMode, _ := strconv.ParseUint(parts[0], 8, 32)
			newMode, _ := strconv.ParseUint(parts[1], 8, 32)
			c := rawChange{Path: fields[i+1], Status: parts[4][0], OldMode: uint32(oldMode), NewMode: uint32(newMode), OldOID: parts[2], NewOID: parts[3]}
			if c.OldOID == r.zero() {
				c.OldOID = ""
			}
			if c.NewOID == r.zero() {
				c.NewOID = ""
			}
			changes = append(changes, c)
		}
		i += 2
	}
	return changes
}

// diffCounts returns added and deleted line counts per path.
func (r *repo) diffCounts(a, b string) (map[string][2]int, error) {
	out, err := r.gitRaw("diff-tree", "-r", "--no-renames", "--numstat", "-z", a, b)
	if err != nil {
		return nil, err
	}
	counts := map[string][2]int{}
	for _, rec := range strings.Split(string(out), "\x00") {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) == 3 {
			added, _ := strconv.Atoi(f[0]) // "-" for a binary file parses to 0
			deleted, _ := strconv.Atoi(f[1])
			counts[f[2]] = [2]int{added, deleted}
		}
	}
	return counts, nil
}

// DiffTrees lists the files that differ between two trees, with line counts.
func (e *Engine) DiffTrees(a, b string) ([]FileChange, error) {
	raw, err := e.repo.diffRaw(a, b)
	if err != nil {
		return nil, err
	}
	counts, err := e.repo.diffCounts(a, b)
	if err != nil {
		return nil, err
	}
	out := make([]FileChange, 0, len(raw))
	for _, c := range raw {
		n := counts[c.Path]
		out = append(out, FileChange{Path: c.Path, Kind: c.kind(), Added: n[0], Deleted: n[1]})
	}
	return out, nil
}

// ReadDiff lists what the workspace differs from its base by: the same information `git diff --cached --name-status
// --numstat <base>` gives for an equivalent worktree.
func (w *Workspace) ReadDiff() ([]FileChange, error) {
	st, err := w.snapshotState()
	if err != nil {
		return nil, err
	}
	return w.e.DiffTrees(st.base, st.overlay)
}

// Patch returns the whole change as a binary-safe patch against the base.
func (w *Workspace) Patch() (string, error) {
	st, err := w.snapshotState()
	if err != nil {
		return "", err
	}
	out, err := w.e.repo.gitRaw("diff-tree", "-p", "-r", "--no-renames", "--binary", st.base, st.overlay)
	return string(out), err
}

// PatchPath returns the patch for one path.
func (w *Workspace) PatchPath(rel string) (string, error) {
	parts, err := splitFile(rel)
	if err != nil {
		return "", err
	}
	st, err := w.snapshotState()
	if err != nil {
		return "", err
	}
	out, err := w.e.repo.gitRaw("diff-tree", "-p", "-r", "--no-renames", "--binary", st.base, st.overlay, "--", ":(literal)"+join(parts))
	return string(out), err
}

// BaseContent returns a file as it was in the base snapshot; ok is false when it did not exist there.
func (w *Workspace) BaseContent(rel string) (content []byte, ok bool, err error) {
	parts, err := splitFile(rel)
	if err != nil {
		return nil, false, err
	}
	st, err := w.snapshotState()
	if err != nil {
		return nil, false, err
	}
	e, found, err := w.e.repo.lookup(st.base, parts)
	if err != nil || !found || !e.isFile() {
		return nil, false, nil
	}
	data, err := w.e.repo.readBlob(e.OID)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Proposals converts a frozen revision into the change records the integration engine consumes: agent and branch
// identity, base revision, before and after blob identities, path and operation. The snapshot fixes the revision,
// so a proposal never describes a state the agent has since left.
func (w *Workspace) Proposals(s Snapshot) ([]ChangeProposal, error) {
	raw, err := w.e.repo.diffRaw(s.BaseTree, s.Tree)
	if err != nil {
		return nil, err
	}
	counts, err := w.e.repo.diffCounts(s.BaseTree, s.Tree)
	if err != nil {
		return nil, err
	}
	out := make([]ChangeProposal, 0, len(raw))
	for _, c := range raw {
		n := counts[c.Path]
		out = append(out, ChangeProposal{
			AgentID: s.AgentID, Branch: s.Branch, BaseCommit: s.BaseCommit, Commit: s.Commit, Tree: s.Tree, Revision: s.Revision,
			Path: c.Path, Op: c.kind(), BeforeBlob: c.OldOID, AfterBlob: c.NewOID, BeforeMode: c.OldMode, AfterMode: c.NewMode,
			Added: n[0], Deleted: n[1],
		})
	}
	return out, nil
}

// Diff lists an agent's changes against its base.
func (e *Engine) Diff(agentID string) ([]FileChange, error) {
	w, err := e.mustGet(agentID)
	if err != nil {
		return nil, err
	}
	return w.ReadDiff()
}
