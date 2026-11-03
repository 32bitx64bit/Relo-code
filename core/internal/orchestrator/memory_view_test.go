package orchestrator

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/memory"
)

// testRepo is a git worktree with one committed file, a.txt.
type testRepo struct {
	dir  string
	head string
}

// initRepo makes a repository in a temp dir with one commit. It skips the test when git is not installed.
func initRepo(t *testing.T) testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string { return gitIn(t, dir, args...) }
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "first")
	return testRepo{dir: dir, head: run("rev-parse", "HEAD")}
}

func TestWorktreeViewAncestryDirtyAndFiles(t *testing.T) {
	r := initRepo(t)
	dir := r.dir
	v := newWorktreeView(dir)

	if head, err := v.Head(); err != nil || head != r.head {
		t.Fatalf("Head = %q, %v; want %q", head, err, r.head)
	}
	if ok, err := v.HasCommit(r.head); err != nil || !ok {
		t.Fatalf("HasCommit(head) = %v, %v", ok, err)
	}
	if ok, err := v.HasCommit("0123456789abcdef0123456789abcdef01234567"); err != nil || ok {
		t.Fatalf("HasCommit(unknown) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := v.IsAncestor(r.head); err != nil || !ok {
		t.Fatalf("IsAncestor(head) = %v, %v; want true", ok, err)
	}

	// A second commit, then a reset back: the second commit still exists but is no longer an ancestor of HEAD.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "commit", "-qam", "second")
	secondCommit := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "reset", "-q", "--hard", r.head)
	if ok, err := v.HasCommit(secondCommit); err != nil || !ok {
		t.Fatalf("HasCommit(reset-away commit) = %v, %v; want true", ok, err)
	}
	if ok, err := v.IsAncestor(secondCommit); err != nil || ok {
		t.Fatalf("IsAncestor(reset-away commit) = %v, %v; want false, nil", ok, err)
	}

	// Untracked edits make the view dirty only for the paths they touch.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty, err := v.Dirty([]string{"a.txt"}); err != nil || dirty {
		t.Fatalf("Dirty(a.txt) = %v, %v; want clean", dirty, err)
	}
	if dirty, err := v.Dirty([]string{"b.txt"}); err != nil || !dirty {
		t.Fatalf("Dirty(b.txt) = %v, %v; want dirty", dirty, err)
	}
	if dirty, err := v.Dirty(nil); err != nil || !dirty {
		t.Fatalf("Dirty(whole view) = %v, %v; want dirty", dirty, err)
	}

	if _, err := v.FileSHA256("missing.txt"); !errors.Is(err, memory.ErrFileAbsent) {
		t.Fatalf("missing file: err = %v, want ErrFileAbsent", err)
	}
	if _, err := v.FileSHA256("../escape.txt"); err == nil {
		t.Fatal("escaping path was accepted")
	}

	// A file reached through a symlinked directory is unreadable, never absent and never hashed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.FileSHA256("linked/secret.txt"); err == nil || errors.Is(err, memory.ErrFileAbsent) {
		t.Fatalf("symlink escape: err = %v, want unreadable", err)
	}
}

// gitIn runs git in dir with a fixed identity and fails the test on error. It returns trimmed stdout.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
