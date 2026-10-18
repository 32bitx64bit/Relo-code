package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorktreeIsolation(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "relo@test")
	run(t, dir, "git", "config", "user.name", "Relo")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "init")
	m := New(dir)
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	tree, err := m.EnsureTree("agent-001", false)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Path == dir {
		t.Fatal("expected isolated worktree")
	}
	if err := os.WriteFile(filepath.Join(tree.Path, "worker.txt"), []byte("from worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "worker.txt")); err == nil {
		t.Fatal("worker file leaked into main workspace")
	}
	changed, err := m.Changed("agent-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "worker.txt" {
		t.Fatalf("untracked file missing from changed files: %v", changed)
	}
	patch, err := m.Patch("agent-001")
	if err != nil || !strings.Contains(patch, "worker.txt") || !strings.Contains(patch, "from worker") {
		t.Fatalf("untracked file missing from patch: %v %q", err, patch)
	}
}

func TestWorktreeLinksNodeModulesWithoutStagingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "relo@test")
	run(t, dir, "git", "config", "user.name", "Relo")
	for p, c := range map[string]string{".gitignore": "node_modules/\n", "README.md": "hi\n", "ext/node_modules/.bin/tsx": "x"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run(t, dir, "git", "add", ".gitignore", "README.md")
	run(t, dir, "git", "commit", "-m", "init")
	m := New(dir)
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	tree, err := m.EnsureTree("agent-001", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tree.Path, "ext", "node_modules", ".bin", "tsx")); err != nil {
		t.Fatalf("node_modules not linked into worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tree.Path, "worker.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := m.Changed("agent-001")
	if err != nil || len(changed) != 1 || changed[0] != "worker.txt" {
		t.Fatalf("symlinked node_modules leaked into changes: %v %v", changed, err)
	}
}

func TestRejectInvalidAgentIDAndNonGitMutation(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	for _, agentID := range []string{"../escape", "a/b", "", "..", "agent..name", "trailing."} {
		if _, err := m.EnsureTree(agentID, false); err == nil {
			t.Errorf("accepted unsafe agent id %q", agentID)
		}
	}
	tree, err := m.EnsureTree("worker-1", false)
	if err != nil {
		t.Fatalf("non-Git mutating tree should fall back to direct mode: %v", err)
	}
	if !tree.Direct || tree.Path != dir || !m.Direct() {
		t.Fatalf("expected direct tree at workspace root, got %+v direct=%v", tree, m.Direct())
	}
	ro, err := m.EnsureTree("probe-1", true)
	if err != nil || ro.Path != dir {
		t.Fatalf("read-only tree: %+v %v", ro, err)
	}
}

func TestDiscardAndPrune(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	_, _ = m.Snapshot()
	tree, err := m.EnsureTree("discard-agent", false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tree.Path, "new.txt")
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Discard("discard-agent", "new.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("discard left untracked file: %v", err)
	}
	if runtime.GOOS != "windows" {
		outside := filepath.Join(t.TempDir(), "sentinel")
		if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Dir(outside), filepath.Join(tree.Path, "escape")); err != nil {
			t.Fatal(err)
		}
		if err := m.Discard("discard-agent", "escape/sentinel"); err == nil {
			t.Fatal("discard followed symlink outside worktree")
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
			t.Fatalf("outside sentinel changed: %q %v", data, err)
		}
	}
	if err := m.Remove("discard-agent"); err != nil {
		t.Fatal(err)
	}
	if err := m.Prune(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardManyRestoresModifiedAndRemovesNew(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	tree, err := m.EnsureTree("many-agent", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, tree.Path, "README.md", "changed\n")
	write(t, tree.Path, "new.txt", "new\n")
	if err := m.DiscardMany("many-agent", []string{"README.md", "new.txt"}); err != nil {
		t.Fatalf("discard many: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(tree.Path, "README.md")); string(got) != "hi\n" {
		t.Fatalf("tracked file = %q, want base content", got)
	}
	if _, err := os.Stat(filepath.Join(tree.Path, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("discard left untracked file: %v", err)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "relo@test")
	run(t, dir, "git", "config", "user.name", "Relo")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "init")
	return dir
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
