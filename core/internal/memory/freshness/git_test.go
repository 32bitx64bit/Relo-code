package freshness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitViewCurrentAncestorAndDirtyStale(t *testing.T) {
	root, _ := newGitRepo(t)
	view, err := NewGitView(root)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := Capture(view, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := Check(view, anchor); got != Current {
		t.Fatalf("initial Check = %s, want current", got)
	}
	writeGitFile(t, root, "a.txt", "dirty")
	if got := Check(view, anchor); got != Stale {
		t.Fatalf("dirty Check = %s, want stale", got)
	}
	writeGitFile(t, root, "a.txt", "observed")
	writeGitFile(t, root, "unrelated.txt", "new")
	runGit(t, root, "add", "unrelated.txt")
	runGit(t, root, "commit", "-m", "unrelated")
	if got := Check(view, anchor); got != Current {
		t.Fatalf("ancestor Check = %s, want current", got)
	}
}

func TestGitViewRebasedMissingCommitAndMissingFileUnknown(t *testing.T) {
	root, _ := newGitRepo(t)
	view, err := NewGitView(root)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := Capture(view, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "commit", "--amend", "--no-edit", "--date=2000-01-01T00:00:00+00:00")
	if got := Check(view, anchor); got != Unknown {
		t.Fatalf("rebased Check = %s, want unknown", got)
	}
	missing := anchor
	missing.Commit = strings.Repeat("1", 40)
	if got := Check(view, missing); got != Unknown {
		t.Fatalf("missing commit Check = %s, want unknown", got)
	}
	anchor, err = Capture(view, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if got := Check(view, anchor); got != Unknown {
		t.Fatalf("missing file Check = %s, want unknown", got)
	}
}

func TestGitViewRefusesEscapingSymlinkAndOversizedFile(t *testing.T) {
	root, _ := newGitRepo(t)
	view, err := NewGitView(root)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := Capture(view, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("observed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if got := Check(view, anchor); got != Unknown {
		t.Fatalf("escaping symlink Check = %s, want unknown", got)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), make([]byte, maxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Check(view, anchor); got != Unknown {
		t.Fatalf("oversized Check = %s, want unknown", got)
	}
}

func newGitRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "--quiet")
	runGit(t, root, "config", "user.name", "Relo Test")
	runGit(t, root, "config", "user.email", "relo@localhost")
	writeGitFile(t, root, "a.txt", "observed")
	runGit(t, root, "add", "a.txt")
	runGit(t, root, "commit", "-m", "initial")
	return root, runGit(t, root, "rev-parse", "HEAD")
}

func writeGitFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
