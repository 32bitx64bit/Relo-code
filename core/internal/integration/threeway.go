package integration

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// AnalyzeCount counts Analyze calls. Tests use it to prove a projection memo hit runs no merge-file.
var AnalyzeCount atomic.Int64

// Outcome says what accepting one file did to the workspace.
type Outcome struct {
	Path   string `json:"path"`
	Action string `json:"action"` // created, updated, merged, deleted, unchanged
}

// Conflict kinds reported by Analyze. A clean analysis has an empty Kind.
const (
	KindOverlap      = "overlap"
	KindModifyDelete = "modify_delete"
	KindDeleteModify = "delete_modify"
	KindAddAdd       = "add_add"
	KindBinary       = "binary"
	KindSymlink      = "symlink"
	KindProtected    = "protected"
)

// Hunk is one changed region of a three-way merge. A side with no change has a zero start and length.
type Hunk struct {
	OursStart   int  `json:"oursStart"`
	OursLen     int  `json:"oursLen"`
	TheirsStart int  `json:"theirsStart"`
	TheirsLen   int  `json:"theirsLen"`
	Conflicting bool `json:"conflicting"`
}

// Analysis is the decision Merge would take, without writing anything. Kind is empty when the file merges cleanly;
// otherwise Reason says why and Hunks locates the changed regions.
type Analysis struct {
	Path   string `json:"path"`
	Action string `json:"action"` // created, updated, merged, deleted, unchanged
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason,omitempty"`
	Ours   string `json:"ours,omitempty"` // git blob id of the workspace file, or "-" when absent
	Hunks  []Hunk `json:"hunks,omitempty"`
	// Resolved names the deterministic rule that settled a file that would otherwise conflict: "whitespace",
	// "take-latest" or "union". It is empty when no rule fired.
	Resolved string `json:"resolved,omitempty"`
	// Merged is the merged content, set for created, updated and merged actions. It is never written by Analyze.
	Merged []byte `json:"-"`
	// Fingerprint identifies the three inputs (workspace, base, agent) the preview was computed from, so an
	// accept can refuse a stale preview without writing.
	Fingerprint string `json:"-"`
}

// MergeShadow is called when the default diff algorithm and histogram disagree about whether a merge is clean.
// The histogram algorithm is a hypothesis, not adopted behavior; disagreements are only reported.
var MergeShadow func(path string, defaultClean, histogramClean bool)

// Analyze decides what accepting one file would do to the workspace, without touching it. It returns the conflict
// kind and hunk locations as well as the reason, so a caller can store and display them.
func Analyze(root, worktree, rel string, baseText []byte, hadBase bool) (Analysis, error) {
	return analyze(root, DirSource{worktree}, rel, baseText, hadBase, false, nil, false)
}

// AnalyzeSource is Analyze with the agent's side read from src, so a change held in a virtual overlay is decided by
// exactly the rules that decide one held in a worktree.
func AnalyzeSource(root string, src Source, rel string, baseText []byte, hadBase bool) (Analysis, error) {
	return analyze(root, src, rel, baseText, hadBase, false, nil, false)
}

// AnalyzeWithOurs is Analyze with the workspace side supplied by the caller instead of read from root. A group
// accept uses it to check a later member against the bytes an earlier member of the same group would write, so a
// dependent's stacked base is not mistaken for a conflict while its parent is still only planned.
func AnalyzeWithOurs(root, worktree, rel string, baseText []byte, hadBase bool, ours []byte, hasOurs bool) (Analysis, error) {
	return analyze(root, DirSource{worktree}, rel, baseText, hadBase, true, ours, hasOurs)
}

// AnalyzeSourceWithOurs is AnalyzeWithOurs with the agent's side read from src.
func AnalyzeSourceWithOurs(root string, src Source, rel string, baseText []byte, hadBase bool, ours []byte, hasOurs bool) (Analysis, error) {
	return analyze(root, src, rel, baseText, hadBase, true, ours, hasOurs)
}

func analyze(root string, src Source, rel string, baseText []byte, hadBase bool, overrideOurs bool, givenOurs []byte, givenHasOurs bool) (Analysis, error) {
	AnalyzeCount.Add(1)
	path, err := normalizePath(rel)
	if err != nil {
		return Analysis{}, err
	}
	out := Analysis{Path: path}
	if protectedPath(path) {
		return conflictAnalysis(out, KindProtected, "protected metadata path"), nil
	}
	if err := checkSymlinkPath(root, path); err != nil {
		return conflictAnalysis(out, KindSymlink, err.Error()), nil
	}
	if err := src.Check(path); err != nil {
		return conflictAnalysis(out, KindSymlink, err.Error()), nil
	}
	oursPath := filepath.Join(root, filepath.FromSlash(path))
	var ours []byte
	var hasOurs bool
	if overrideOurs {
		ours, hasOurs = givenOurs, givenHasOurs
	} else {
		var oursErr error
		ours, oursErr = os.ReadFile(oursPath)
		hasOurs = oursErr == nil
		if oursErr != nil && !os.IsNotExist(oursErr) {
			return out, oursErr
		}
	}
	theirs, hasTheirs, theirsErr := src.Read(path)
	if theirsErr != nil {
		return out, theirsErr
	}
	out.Ours = blobID(ours, hasOurs)
	out.Fingerprint = fingerprint(out.Ours, baseText, hadBase, theirs, hasTheirs)
	// takeLatest paths are declared generated outputs, so every conflict below resolves to the agent's side.
	takeLatest := TakesLatest(path)
	conflict := func(kind, reason string) Analysis {
		if takeLatest {
			return latestWins(out, hasOurs, theirs, hasTheirs)
		}
		return conflictAnalysis(out, kind, reason)
	}
	switch {
	case !hasTheirs && !hadBase:
		out.Action = "unchanged"
		return out, nil
	case !hasTheirs: // the agent deleted it
		if !hasOurs {
			out.Action = "unchanged"
			return out, nil
		}
		if !bytes.Equal(ours, baseText) {
			return conflict(KindModifyDelete, "you changed this file after the agent started, and the agent deleted it"), nil
		}
		out.Action = "deleted"
		return out, nil
	case hasOurs && bytes.Equal(ours, theirs):
		out.Action = "unchanged"
		return out, nil
	case !hadBase: // the agent created it
		if !hasOurs {
			out.Action = "created"
			out.Merged = theirs
			return out, nil
		}
		if takeLatest {
			return latestWins(out, hasOurs, theirs, hasTheirs), nil
		}
		// Line endings and trailing blanks are not content: a copy that differs only there keeps the workspace file.
		if sameIgnoringTrailingSpace(ours, theirs) {
			out.Action = "unchanged"
			out.Resolved = "whitespace"
			return out, nil
		}
		return conflictAnalysis(out, KindAddAdd, "the file exists in your workspace and the agent created a different one"), nil
	case !hasOurs:
		return conflict(KindDeleteModify, "you deleted this file after the agent started, and the agent changed it"), nil
	case bytes.Equal(ours, baseText):
		out.Action = "updated"
		out.Merged = theirs
		return out, nil
	}
	if isBinary(ours) || isBinary(baseText) || isBinary(theirs) {
		// Kind says binary; the reason keeps the wording stored by earlier releases, so a re-check of an old
		// conflict does not change the text the user already saw.
		return conflict(KindBinary, "your edits and the agent's edits overlap"), nil
	}
	merged, clean, err := mergeFile(ours, baseText, theirs)
	if err != nil {
		return out, err
	}
	shadowCompare(path, ours, baseText, theirs, clean)
	if !clean {
		if takeLatest {
			return latestWins(out, hasOurs, theirs, hasTheirs), nil
		}
		if UnionMerges(path) {
			if union, ok := unionMerge(ours, baseText, theirs); ok {
				out.Action = "merged"
				out.Merged = union
				out.Resolved = "union"
				return out, nil
			}
		}
		hunks, herr := diffHunks(ours, baseText, theirs, clean)
		if herr != nil {
			return out, herr
		}
		out.Hunks = hunks
		out.Kind = KindOverlap
		out.Reason = "your edits and the agent's edits overlap"
		return out, nil
	}
	out.Action = "merged"
	out.Merged = merged
	return out, nil
}

// ErrWorkspaceChanged says the file changed between the analysis and the compare-and-swap, and did so again on
// every retry. Nothing is written.
var ErrWorkspaceChanged = errors.New("the workspace file kept changing while the change was being accepted")

// WorkspaceChangedError names the path that could not be written because it kept changing.
type WorkspaceChangedError struct {
	Path string
}

func (e WorkspaceChangedError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, ErrWorkspaceChanged)
}

func (e WorkspaceChangedError) Unwrap() error { return ErrWorkspaceChanged }

// ConcurrentWriteError says the target held bytes that Relo did not write when the write finished. The journal
// entry recorded before the write is the recovery path.
type ConcurrentWriteError struct {
	Path string
}

func (e ConcurrentWriteError) Error() string {
	return fmt.Sprintf("%s: another program wrote the file while the change was being accepted", e.Path)
}

// JournalHook runs after the compare-and-swap passes and before the write. Returning an error makes Merge fail
// closed with nothing written, except for a path that does not exist yet.
type JournalHook func(path string, pre []byte, preExists bool, post []byte, postExists bool) error

// beforeCommit is the fault-injection seam for the compare-and-swap. It runs before each re-hash.
var beforeCommit func()

// casAttempts is how many times a changing workspace is read and merged again before Merge fails closed.
const casAttempts = 3

// Merge applies one file of an agent's worktree to the workspace by three-way merge of base (the commit the
// worktree was cut from), ours (what is in the workspace now) and theirs (what the agent left). Local edits made
// while the agent worked are kept: a clean merge is written, anything that overlaps is reported as a ConflictError
// and nothing is touched. baseText is the file at base; hadBase is false when it did not exist there.
func Merge(root, worktree, rel string, baseText []byte, hadBase bool) (Outcome, error) {
	return MergeSource(root, DirSource{worktree}, rel, baseText, hadBase, nil)
}

// MergeWithHook is Merge with a journal hook. Every write is a compare-and-swap: the target is re-hashed after the
// merge is computed, and a mismatch re-reads and recomputes the merge up to casAttempts times before failing with
// WorkspaceChangedError. The write goes to a temporary file in the target directory and is renamed into place;
// the target's existing mode and symlink status are kept.
func MergeWithHook(root, worktree, rel string, baseText []byte, hadBase bool, hook JournalHook) (Outcome, error) {
	return MergeSource(root, DirSource{worktree}, rel, baseText, hadBase, hook)
}

// MergeSource is MergeWithHook with the agent's side read from src. The compare-and-swap, the journal hook and the
// write are the same whatever src is.
func MergeSource(root string, src Source, rel string, baseText []byte, hadBase bool, hook JournalHook) (Outcome, error) {
	for attempt := 0; attempt < casAttempts; attempt++ {
		a, err := AnalyzeSource(root, src, rel, baseText, hadBase)
		if err != nil {
			return Outcome{}, err
		}
		if a.Kind != "" {
			return Outcome{Path: a.Path}, ConflictError{Report: ConflictReport{Files: []string{a.Path}, Reason: a.Reason}}
		}
		if a.Action == "unchanged" {
			return Outcome{Path: a.Path, Action: a.Action}, nil
		}
		if beforeCommit != nil {
			beforeCommit()
		}
		current, exists, err := readTarget(root, a.Path)
		if err != nil {
			return Outcome{Path: a.Path}, err
		}
		if blobID(current, exists) != a.Ours {
			continue
		}
		post, postExists := a.Merged, a.Action != "deleted"
		if hook != nil {
			if err := hook(a.Path, current, exists, post, postExists); err != nil {
				return Outcome{Path: a.Path}, err
			}
		}
		if err := applyAnalysis(root, src, a); err != nil {
			return Outcome{Path: a.Path}, err
		}
		written, writtenExists, err := readTarget(root, a.Path)
		if err != nil {
			return Outcome{Path: a.Path}, err
		}
		if blobID(written, writtenExists) != blobID(post, postExists) {
			return Outcome{Path: a.Path}, ConcurrentWriteError{Path: a.Path}
		}
		return Outcome{Path: a.Path, Action: a.Action}, nil
	}
	path, _ := normalizePath(rel)
	return Outcome{Path: path}, WorkspaceChangedError{Path: path}
}

// TakeWithHook replaces the workspace file with the agent's version. It is the explicit "take the agent's" choice,
// so it does not merge, but it still refuses protected paths and symlinks and journals the pre-image through hook
// before writing. The target's mode is kept and a symlink is never followed.
func TakeWithHook(root, worktree, rel string, hook JournalHook) (Outcome, error) {
	return TakeSource(root, DirSource{worktree}, rel, hook)
}

// TakeSource is TakeWithHook with the agent's side read from src.
func TakeSource(root string, src Source, rel string, hook JournalHook) (Outcome, error) {
	path, err := normalizePath(rel)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Path: path}
	if protectedPath(path) {
		return out, conflictErr(path, "protected metadata path")
	}
	if err := checkSymlinkPath(root, path); err != nil {
		return out, err
	}
	if err := src.Check(path); err != nil {
		return out, err
	}
	theirs, hasTheirs, theirsErr := src.Read(path)
	if theirsErr != nil {
		return out, theirsErr
	}
	ours, hasOurs, err := readTarget(root, path)
	if err != nil {
		return out, err
	}
	if !hasTheirs && !hasOurs {
		out.Action = "unchanged"
		return out, nil
	}
	if !hasTheirs {
		out.Action = "deleted"
	} else if !hasOurs {
		out.Action = "created"
	} else if bytes.Equal(ours, theirs) {
		out.Action = "unchanged"
		return out, nil
	} else {
		out.Action = "updated"
	}
	if hook != nil {
		if err := hook(path, ours, hasOurs, theirs, hasTheirs); err != nil {
			return out, err
		}
	}
	if !hasTheirs {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil && !os.IsNotExist(err) {
			return out, err
		}
	} else if err := writeAtomic(filepath.Join(root, filepath.FromSlash(path)), src, path, theirs); err != nil {
		return out, err
	}
	return out, nil
}

// readTarget reads the workspace file at path; exists is false when it is absent.
func readTarget(root, path string) ([]byte, bool, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err == nil {
		return data, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, err
}

// Check runs the decisions of Merge without writing anything. It returns the reason Merge would refuse the file, or ""
// when Merge would apply it.
func Check(root, worktree, rel string, baseText []byte, hadBase bool) (string, error) {
	return CheckSource(root, DirSource{worktree}, rel, baseText, hadBase)
}

// CheckSource is Check with the agent's side read from src.
func CheckSource(root string, src Source, rel string, baseText []byte, hadBase bool) (string, error) {
	if _, err := normalizePath(rel); err != nil {
		return err.Error(), nil
	}
	a, err := AnalyzeSource(root, src, rel, baseText, hadBase)
	if err != nil {
		return "", err
	}
	return a.Reason, nil
}

func applyAnalysis(root string, src Source, a Analysis) error {
	dst := filepath.Join(root, filepath.FromSlash(a.Path))
	switch a.Action {
	case "created", "updated", "merged":
		return writeAtomic(dst, src, a.Path, a.Merged)
	case "deleted":
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return nil
}

func conflictAnalysis(out Analysis, kind, reason string) Analysis {
	out.Kind, out.Reason = kind, reason
	return out
}

// latestWins resolves a conflict by taking the agent's side: its file replaces the workspace file, or the
// workspace file is removed when the agent deleted it.
func latestWins(out Analysis, hasOurs bool, theirs []byte, hasTheirs bool) Analysis {
	out.Kind, out.Reason, out.Hunks = "", "", nil
	out.Resolved = "take-latest"
	switch {
	case hasTheirs && hasOurs:
		out.Action, out.Merged = "updated", theirs
	case hasTheirs:
		out.Action, out.Merged = "created", theirs
	case hasOurs:
		out.Action = "deleted"
	default:
		out.Action = "unchanged"
	}
	return out
}

// sameIgnoringTrailingSpace reports whether two text files differ only in line endings, trailing spaces and tabs,
// or trailing blank lines. Interior whitespace still counts, since it can change the meaning of Python or YAML.
func sameIgnoringTrailingSpace(a, b []byte) bool {
	if isBinary(a) || isBinary(b) {
		return false
	}
	return normalizeTrailing(a) == normalizeTrailing(b)
}

func normalizeTrailing(data []byte) string {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// unionMerge keeps the lines both sides added. It applies only when neither side changed or removed a base line,
// so the base lines are a subsequence of each side. It reports false, and the caller keeps the conflict, when the
// result fails a check: a conflict marker, or a line of either side missing from it.
func unionMerge(ours, base, theirs []byte) ([]byte, bool) {
	if !isSubsequence(textLines(base), textLines(ours)) || !isSubsequence(textLines(base), textLines(theirs)) {
		return nil, false
	}
	merged, clean, err := mergeFile(ours, base, theirs, "--union")
	if err != nil || !clean {
		return nil, false
	}
	for _, line := range textLines(merged) {
		if strings.HasPrefix(line, "<<<<<<<") || strings.HasPrefix(line, ">>>>>>>") || line == "=======" {
			return nil, false
		}
	}
	if !isSubsequence(textLines(ours), textLines(merged)) || !isSubsequence(textLines(theirs), textLines(merged)) {
		return nil, false
	}
	return merged, true
}

// textLines splits data into lines without their newlines; a final newline does not start an empty line.
func textLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// isSubsequence reports whether every element of sub appears in seq in the same order. Matching greedily from the
// front is exact for subsequences.
func isSubsequence(sub, seq []string) bool {
	i := 0
	for _, s := range seq {
		if i < len(sub) && s == sub[i] {
			i++
		}
	}
	return i == len(sub)
}

func blobID(data []byte, present bool) string {
	if !present {
		return "-"
	}
	sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...))
	return hex.EncodeToString(sum[:])
}

// BlobID returns the git blob id of data, or "-" when the file is absent.
func BlobID(data []byte, present bool) string { return blobID(data, present) }

// fingerprint is sha1 over the workspace, base and agent blobs of one projection.
func fingerprint(ours string, base []byte, hadBase bool, theirs []byte, hasTheirs bool) string {
	sum := sha1.Sum([]byte(ours + "|" + blobID(base, hadBase) + "|" + blobID(theirs, hasTheirs)))
	return hex.EncodeToString(sum[:])
}

func isBinary(data []byte) bool { return bytes.IndexByte(data, 0) >= 0 }

func conflictErr(path, reason string) error {
	return ConflictError{Report: ConflictReport{Files: []string{path}, Reason: reason}}
}

func protectedPath(path string) bool {
	lower := strings.ToLower(path)
	return lower == ".git" || strings.HasPrefix(lower, ".git/") || lower == ".relo" || strings.HasPrefix(lower, ".relo/")
}

// writeAtomic writes data through a temporary file in dst's directory and renames it into place. An existing
// target keeps its permission bits; a new file takes the agent file's mode. A symlink target is refused, never
// followed: the pre-write symlink check in Analyze already refuses one, and Lstat here closes the gap.
func writeAtomic(dst string, src Source, rel string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Lstat(dst); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: refusing to replace a symlink", dst)
		}
		mode = st.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	} else if m, ok := src.Mode(rel); ok {
		mode = m
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".relo-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		cleanup()
		return err
	}
	// Directory fsync is best effort: it is unavailable on some platforms, and the rename already happened.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// mergeFile runs `git merge-file`, which exits 0 for a clean merge, with the conflict count when there are
// conflicts, and above 127 for errors. Binary content is never merged: it is a conflict.
func mergeFile(ours, base, theirs []byte, extra ...string) (merged []byte, clean bool, err error) {
	if isBinary(ours) || isBinary(base) || isBinary(theirs) {
		return nil, false, nil
	}
	dir, err := os.MkdirTemp("", "relo-merge-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	files := map[string][]byte{"ours": ours, "base": base, "theirs": theirs}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, false, err
		}
	}
	args := append([]string{"merge-file", "-p"}, extra...)
	args = append(args, "ours", "base", "theirs")
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return stdout.Bytes(), true, nil
	}
	if exit, ok := runErr.(*exec.ExitError); ok && exit.ExitCode() > 0 && exit.ExitCode() < 128 {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("git merge-file: %v: %s", runErr, strings.TrimSpace(stderr.String()))
}

// shadowCompare runs the same merge with the histogram algorithm and reports a disagreement. A git that does not
// support the option is ignored: histogram is only a hypothesis being measured.
func shadowCompare(path string, ours, base, theirs []byte, defaultClean bool) {
	_, histClean, err := mergeFile(ours, base, theirs, "--diff-algorithm=histogram")
	if err != nil || histClean == defaultClean {
		return
	}
	if MergeShadow != nil {
		MergeShadow(path, defaultClean, histClean)
		return
	}
	log.Printf("integration: diff algorithms disagree on %s (default clean=%v, histogram clean=%v)", path, defaultClean, histClean)
}

type sideHunk struct {
	baseStart int // 1-based, inclusive
	baseEnd   int
	start     int // new-side start, 1-based
	length    int
	theirs    bool
}

// diffHunks locates the changed regions of each side against base and pairs overlapping ones. It is used to show
// which hunks conflict instead of the whole file: the merge-file exit code says only whether any hunk does.
func diffHunks(ours, base, theirs []byte, clean bool) ([]Hunk, error) {
	if isBinary(ours) || isBinary(base) || isBinary(theirs) {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "relo-hunks-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for name, data := range map[string][]byte{"base": base, "ours": ours, "theirs": theirs} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	var sides []sideHunk
	for _, side := range []struct {
		name   string
		theirs bool
	}{{"ours", false}, {"theirs", true}} {
		out, derr := diffUnified(dir, side.name)
		if derr != nil {
			return nil, derr
		}
		for _, h := range out {
			h.theirs = side.theirs
			sides = append(sides, h)
		}
	}
	if len(sides) == 0 {
		return nil, nil
	}
	sort.SliceStable(sides, func(i, j int) bool { return sides[i].baseStart < sides[j].baseStart })
	var out []Hunk
	var group []sideHunk
	hasOurs, hasTheirs := false, false
	end := 0
	flush := func() {
		h := Hunk{Conflicting: !clean && hasOurs && hasTheirs}
		for _, s := range group {
			if s.theirs {
				h.TheirsStart, h.TheirsLen = s.start, s.length
			} else {
				h.OursStart, h.OursLen = s.start, s.length
			}
		}
		out = append(out, h)
		group = nil
		hasOurs, hasTheirs = false, false
	}
	for _, s := range sides {
		if len(group) > 0 && s.baseStart > end {
			flush()
			end = 0
		}
		group = append(group, s)
		if s.theirs {
			hasTheirs = true
		} else {
			hasOurs = true
		}
		if s.baseEnd > end {
			end = s.baseEnd
		}
	}
	if len(group) > 0 {
		flush()
	}
	return out, nil
}

// diffUnified returns the changed regions of one side against base, from `git diff --no-index --unified=0`.
func diffUnified(dir, side string) ([]sideHunk, error) {
	cmd := exec.Command("git", "diff", "--no-index", "--unified=0", "--no-color", "--", "base", side)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("git diff %s: %w", side, err)
		}
	}
	var hunks []sideHunk
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "@@ -") {
			continue
		}
		baseStart, baseLen, start, length, ok := parseHunkHeader(line)
		if !ok {
			continue
		}
		end := baseStart
		if baseLen > 0 {
			end = baseStart + baseLen - 1
		}
		hunks = append(hunks, sideHunk{baseStart: baseStart, baseEnd: end, start: start, length: length})
	}
	return hunks, nil
}

// parseHunkHeader reads `@@ -a,b +c,d @@` (counts default to 1 when omitted).
func parseHunkHeader(line string) (baseStart, baseLen, start, length int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, 0, 0, false
	}
	var err error
	baseStart, baseLen, err = parseRange(strings.TrimPrefix(fields[1], "-"))
	if err != nil {
		return 0, 0, 0, 0, false
	}
	start, length, err = parseRange(strings.TrimPrefix(fields[2], "+"))
	if err != nil {
		return 0, 0, 0, 0, false
	}
	return baseStart, baseLen, start, length, true
}

func parseRange(s string) (start, length int, err error) {
	length = 1
	parts := strings.SplitN(s, ",", 2)
	start, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	if len(parts) == 2 {
		length, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, err
		}
	}
	return start, length, nil
}
