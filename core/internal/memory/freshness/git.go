package freshness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitView reads the dirty working tree rooted at a Git workspace.
type GitView struct {
	root string
}

// NewGitView opens a Git-backed view without changing the repository.
func NewGitView(root string) (*GitView, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("freshness: %s is not a directory", root)
	}
	view := &GitView{root: resolved}
	if _, err := view.git("rev-parse", "--git-dir"); err != nil {
		return nil, err
	}
	return view, nil
}

// Head resolves the workspace's current commit.
func (v *GitView) Head() (string, error) {
	head, err := v.git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if !validCommit(head) {
		return "", fmt.Errorf("freshness: invalid HEAD commit %q", head)
	}
	return head, nil
}

// IsAncestor reports whether ancestor is head or one of its ancestors.
func (v *GitView) IsAncestor(ancestor, head string) (bool, error) {
	if !validCommit(ancestor) || !validCommit(head) {
		return false, errors.New("freshness: invalid commit")
	}
	_, err := v.git("merge-base", "--is-ancestor", ancestor, head)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// ReadFile reads a regular file from the current working tree, including dirty
// bytes. It refuses protected paths, escaping symlinks and oversized files.
func (v *GitView) ReadFile(name string) ([]byte, error) {
	if !validPath(name) {
		return nil, fmt.Errorf("freshness: invalid path %q", name)
	}
	full := filepath.Join(v.root, filepath.FromSlash(name))
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(v.root, resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("freshness: path %q escapes workspace", name)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("freshness: %q is not a regular file", name)
	}
	if info.Size() > maxFileBytes {
		return nil, fmt.Errorf("freshness: %q exceeds %d bytes", name, maxFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("freshness: %q exceeds %d bytes", name, maxFileBytes)
	}
	return data, nil
}

// git follows the core workspace helpers: commands run in the workspace with
// prompting, paging and optional index locks disabled.
func (v *GitView) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = v.root
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func validCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, r := range commit {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
