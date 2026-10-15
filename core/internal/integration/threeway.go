package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Outcome says what accepting one file did to the workspace.
type Outcome struct {
	Path   string `json:"path"`
	Action string `json:"action"` // created, updated, merged, deleted, unchanged
}

// Merge applies one file of an agent's worktree to the workspace by three-way merge of base (the commit the
// worktree was cut from), ours (what is in the workspace now) and theirs (what the agent left). Local edits made
// while the agent worked are kept: a clean merge is written, anything that overlaps is reported as a ConflictError
// and nothing is touched. baseText is the file at base; hadBase is false when it did not exist there.
func Merge(root, worktree, rel string, baseText []byte, hadBase bool) (Outcome, error) {
	path, err := normalizePath(rel)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Path: path}
	lower := strings.ToLower(path)
	if lower == ".git" || strings.HasPrefix(lower, ".git/") || lower == ".relo" || strings.HasPrefix(lower, ".relo/") {
		return out, conflictErr(path, "protected metadata path")
	}
	if err := checkSymlinkPath(root, path); err != nil {
		return out, conflictErr(path, err.Error())
	}
	if err := checkSymlinkPath(worktree, path); err != nil {
		return out, conflictErr(path, err.Error())
	}
	oursPath := filepath.Join(root, filepath.FromSlash(path))
	theirsPath := filepath.Join(worktree, filepath.FromSlash(path))
	ours, oursErr := os.ReadFile(oursPath)
	hasOurs := oursErr == nil
	if oursErr != nil && !os.IsNotExist(oursErr) {
		return out, oursErr
	}
	theirs, theirsErr := os.ReadFile(theirsPath)
	hasTheirs := theirsErr == nil
	if theirsErr != nil && !os.IsNotExist(theirsErr) {
		return out, theirsErr
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
			return out, conflictErr(path, "you changed this file after the agent started, and the agent deleted it")
		}
		if err := os.Remove(oursPath); err != nil {
			return out, err
		}
		out.Action = "deleted"
		return out, nil
	case hasOurs && bytes.Equal(ours, theirs):
		out.Action = "unchanged"
		return out, nil
	case !hadBase: // the agent created it
		if hasOurs {
			return out, conflictErr(path, "the file exists in your workspace and the agent created a different one")
		}
		out.Action = "created"
		return out, writeLike(oursPath, theirsPath, theirs)
	case !hasOurs:
		return out, conflictErr(path, "you deleted this file after the agent started, and the agent changed it")
	case bytes.Equal(ours, baseText):
		out.Action = "updated"
		return out, writeLike(oursPath, theirsPath, theirs)
	}
	merged, clean, err := mergeFile(ours, baseText, theirs)
	if err != nil {
		return out, err
	}
	if !clean {
		return out, conflictErr(path, "your edits and the agent's edits overlap")
	}
	out.Action = "merged"
	return out, writeLike(oursPath, theirsPath, merged)
}

func conflictErr(path, reason string) error {
	return ConflictError{Report: ConflictReport{Files: []string{path}, Reason: reason}}
}

// writeLike writes data to dst, creating folders, and gives it the permission bits of src (the agent's file).
func writeLike(dst, src string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(src); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(dst, data, mode)
}

// mergeFile runs `git merge-file`, which exits 0 for a clean merge, with the conflict count when there are
// conflicts, and above 127 for errors. Binary content is never merged: it is a conflict.
func mergeFile(ours, base, theirs []byte) (merged []byte, clean bool, err error) {
	if bytes.IndexByte(ours, 0) >= 0 || bytes.IndexByte(base, 0) >= 0 || bytes.IndexByte(theirs, 0) >= 0 {
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
	cmd := exec.Command("git", "merge-file", "-p", "ours", "base", "theirs")
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
