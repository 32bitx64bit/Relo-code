package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func output(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Agents used to be cut from HEAD, so the user's uncommitted work was invisible to them: one agent reported the
// change they wanted as missing while another, reading the real folder, reported it present.
func TestWorktreeStartsFromTheUsersWorkingTree(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "a.txt", "one\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "a")
	write(t, dir, ".gitignore", "*.log\n")
	run(t, dir, "git", "add", ".gitignore")
	run(t, dir, "git", "commit", "-m", "ignore")
	head := output(t, dir, "rev-parse", "HEAD")

	write(t, dir, "a.txt", "two\n")       // uncommitted edit
	write(t, dir, "new.txt", "fresh\n")   // untracked file
	write(t, dir, "build.log", "noise\n") // ignored
	run(t, dir, "git", "add", "README.md")
	statusBefore := output(t, dir, "status", "--porcelain")

	m := New(dir)
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Commit == "" || snap.Commit == head {
		t.Fatalf("snapshot commit = %q, want a commit holding the working tree (HEAD %s)", snap.Commit, head)
	}
	tree, err := m.EnsureTree("agent-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Base != snap.Commit {
		t.Fatalf("worktree base = %s, want the snapshot %s", tree.Base, snap.Commit)
	}
	for rel, want := range map[string]string{"a.txt": "two\n", "new.txt": "fresh\n"} {
		got, err := os.ReadFile(filepath.Join(tree.Path, rel))
		if err != nil || string(got) != want {
			t.Fatalf("%s in the worktree = %q (%v), want %q", rel, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(tree.Path, "build.log")); err == nil {
		t.Fatal("an ignored file was copied into the worktree")
	}
	// The user's edits are not the agent's work.
	if files, err := m.ChangedFiles("agent-1"); err != nil || len(files) != 0 {
		t.Fatalf("changes before the agent did anything = %+v (%v)", files, err)
	}
	write(t, tree.Path, "a.txt", "three\n")
	files, err := m.ChangedFiles("agent-1")
	if err != nil || len(files) != 1 || files[0].Path != "a.txt" || files[0].Added != 1 || files[0].Deleted != 1 {
		t.Fatalf("changes = %+v (%v), want just a.txt +1 -1", files, err)
	}
	// What the agent changed is measured against what the user had, which is what a three-way accept needs.
	base, ok, err := m.BaseContent("agent-1", "a.txt")
	if err != nil || !ok || string(base) != "two\n" {
		t.Fatalf("base content = %q ok=%v err=%v, want the user's version", base, ok, err)
	}

	// Taking the snapshot left the user's repository exactly as it was.
	if got := output(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %s", got)
	}
	if got := output(t, dir, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("status changed:\nbefore %q\nafter  %q", statusBefore, got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "two\n" {
		t.Fatalf("the user's file was touched: %q", got)
	}
	if got := output(t, dir, "stash", "list"); got != "" {
		t.Fatalf("a stash was created: %q", got)
	}
}

func TestSnapshotOfACleanTreeIsHeadAndAnUnchangedTreeKeepsItsBase(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if head := output(t, dir, "rev-parse", "HEAD"); snap.Commit != head {
		t.Fatalf("clean tree: commit %s, want HEAD %s", snap.Commit, head)
	}
	write(t, dir, "wip.txt", "wip\n")
	first, _ := m.Snapshot()
	second, _ := m.Snapshot()
	if first.Commit == "" || first.Commit != second.Commit {
		t.Fatalf("an unchanged tree produced %s then %s", first.Commit, second.Commit)
	}
	write(t, dir, "wip.txt", "more\n")
	if third, _ := m.Snapshot(); third.Commit == second.Commit {
		t.Fatal("a changed tree kept the old base")
	}
}

// Relo's own folder is not the user's work, even in a repository that does not ignore it.
func TestSnapshotIgnoresReloFolder(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, ".relo/relo.db", "x")
	m := New(dir)
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(snap.Untracked, snap.Dirty...) {
		if strings.HasPrefix(p, ".relo") {
			t.Fatalf("%q reported as the user's change", p)
		}
	}
	if snap.Commit != output(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("a repository whose only extra is .relo/ should snapshot as HEAD, got %s", snap.Commit)
	}
}

// A test or review task that depends on a worker has to see the worker's change, which the user has not accepted yet.
func TestStackPutsAnotherAgentsChangesUnderTheNextOne(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "a.txt", "mine\n") // uncommitted work the user already has
	m := New(dir)
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	author, err := m.EnsureTree("author", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, author.Path, "a.txt", "mine, then the worker's\n")
	write(t, author.Path, "x.txt", "new file\n")
	var patches []string
	for _, rel := range []string{"a.txt", "x.txt"} {
		p, err := m.PatchPath("author", rel)
		if err != nil || p == "" {
			t.Fatalf("patch %s: %q %v", rel, p, err)
		}
		patches = append(patches, p)
	}

	next, err := m.EnsureTree("next", false)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := m.Stack("next", patches)
	if err != nil || applied != 2 {
		t.Fatalf("stack applied %d (%v), want 2", applied, err)
	}
	for rel, want := range map[string]string{"a.txt": "mine, then the worker's\n", "x.txt": "new file\n"} {
		if got, _ := os.ReadFile(filepath.Join(next.Path, rel)); string(got) != want {
			t.Fatalf("%s in the stacked worktree = %q, want %q", rel, got, want)
		}
	}
	// What it builds on is its base, not its own work.
	if files, err := m.ChangedFiles("next"); err != nil || len(files) != 0 {
		t.Fatalf("stacked changes were counted as its own: %+v (%v)", files, err)
	}
	write(t, next.Path, "y.txt", "its own\n")
	files, err := m.ChangedFiles("next")
	if err != nil || len(files) != 1 || files[0].Path != "y.txt" {
		t.Fatalf("changes = %+v (%v), want only y.txt", files, err)
	}
	if base, ok, _ := m.BaseContent("next", "a.txt"); !ok || string(base) != "mine, then the worker's\n" {
		t.Fatalf("base of a.txt = %q", base)
	}
	// The user's folder and the author's worktree are untouched.
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "mine\n" {
		t.Fatalf("the user's file changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); err == nil {
		t.Fatal("the author's new file reached the user's folder")
	}

	// A patch that does not fit is skipped, not fatal, and the ones that do fit still count.
	third, _ := m.EnsureTree("third", false)
	write(t, third.Path, "x.txt", "someone else's x\n") // x.txt exists here, so the creation patch cannot apply
	run(t, third.Path, "git", "add", "-A")
	applied, err = m.Stack("third", patches)
	if applied != 1 || err == nil || !strings.Contains(err.Error(), "1 of 2 patches did not apply") {
		t.Fatalf("applied %d, err %v; want 1 applied and a note about the other", applied, err)
	}
}
