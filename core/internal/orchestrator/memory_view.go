package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/relocode/relo/internal/memory"
)

// gitViewTimeout bounds one git call made for a memory view. A slow or hung repository reads as unknown.
const gitViewTimeout = 10 * time.Second

// errNotRegular, errTooLarge and errEscapes name why a worktree file cannot be hashed. Callers map them to a verdict.
var (
	errNotRegular = errors.New("not a regular file")
	errTooLarge   = errors.New("file too large to hash")
	errEscapes    = errors.New("file resolves outside the worktree")
)

// worktreeView is the memory.View of one agent worktree: its git history and the bytes on disk, including uncommitted
// edits. It never follows a link out of root.
type worktreeView struct {
	root string
}

var _ memory.View = worktreeView{}

func newWorktreeView(root string) worktreeView {
	return worktreeView{root: root}
}

// Head returns the commit the worktree currently has checked out. Promotion stamps it onto file observations.
func (v worktreeView) Head() (string, error) {
	out, err := v.git("rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// HasCommit reports whether the commit object exists. A git exit status means it is missing; a failure to start git
// is an error.
func (v worktreeView) HasCommit(commit string) (bool, error) {
	_, err := v.git("cat-file", "-e", commit+"^{commit}")
	if err == nil {
		return true, nil
	}
	if _, ok := gitExitCode(err); ok {
		return false, nil
	}
	return false, err
}

// IsAncestor reports whether commit is HEAD or an ancestor of HEAD. Exit status 1 means not an ancestor.
func (v worktreeView) IsAncestor(commit string) (bool, error) {
	_, err := v.git("merge-base", "--is-ancestor", commit, "HEAD")
	if err == nil {
		return true, nil
	}
	if code, ok := gitExitCode(err); ok && code == 1 {
		return false, nil
	}
	return false, err
}

// FileSHA256 hashes one workspace-relative path in the worktree. A missing path wraps memory.ErrFileAbsent.
func (v worktreeView) FileSHA256(path string) (string, error) {
	if !memory.ValidEvidencePath(path) {
		return "", fmt.Errorf("invalid evidence path")
	}
	return hashWorktreeFile(v.root, path)
}

// Dirty reports whether git sees uncommitted changes under paths, or anywhere in the worktree when paths is empty.
func (v worktreeView) Dirty(paths []string) (bool, error) {
	args := []string{"status", "--porcelain", "-z", "--untracked-files=all"}
	if len(paths) > 0 {
		args = append(args, "--")
		for _, p := range paths {
			if !memory.ValidEvidencePath(p) {
				return false, fmt.Errorf("invalid evidence path")
			}
			args = append(args, p)
		}
	}
	out, err := v.git(args...)
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}

// git runs one read-only git command in the worktree. Literal pathspecs and no optional locks keep it from reading
// paths as patterns or writing the index.
func (v worktreeView) git(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitViewTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", v.root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}

// gitExitCode returns the exit status of a git process that ran and failed.
func gitExitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// hashWorktreeFile hashes one workspace-relative file under root. A missing path wraps memory.ErrFileAbsent. A
// non-regular file, an oversized file or a path that resolves outside root is refused, and nothing is read from it.
func hashWorktreeFile(root, rel string) (string, error) {
	full := filepath.Join(root, rel)
	info, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", memory.ErrFileAbsent, rel)
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errNotRegular
	}
	if info.Size() > maxEvidenceFileBytes {
		return "", errTooLarge
	}
	// A symlinked directory on the path can lead out of the worktree even when the leaf is a regular file.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realFile, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(realRoot, realFile); err != nil || !filepath.IsLocal(r) {
		return "", errEscapes
	}
	return hashFile(full)
}
