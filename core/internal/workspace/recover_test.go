package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotAt records the working tree as a snapshot commit with a fixed date, the way snapshotCommit does, so the
// order of snapshots in a test never depends on the clock.
func snapshotAt(t *testing.T, dir, date string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	env := append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_COMMITTER_DATE="+date, "GIT_AUTHOR_DATE="+date,
		"GIT_AUTHOR_NAME=Relo", "GIT_AUTHOR_EMAIL=relo@localhost", "GIT_COMMITTER_NAME=Relo", "GIT_COMMITTER_EMAIL=relo@localhost")
	do := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	do("add", "-A")
	return do("commit-tree", do("write-tree"), "-p", "HEAD", "-m", snapshotSubject)
}

// A file that was never committed and then vanished from the working tree (the Pixels at War facts file) is still in
// the snapshot Relo cut the agents from. RecoverableCopy names that snapshot, newest first.
func TestAVanishedUntrackedFileIsFoundInTheNewestSnapshot(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)

	write(t, dir, "engine/facts.c", "version one\n")
	first := snapshotAt(t, dir, "2026-11-02T23:00:00Z")
	run(t, dir, "git", "update-ref", "refs/relo/vws/agent_a/base", first)

	write(t, dir, "engine/facts.c", "version two\n")
	second := snapshotAt(t, dir, "2026-11-02T23:10:00Z")
	run(t, dir, "git", "update-ref", "refs/relo/vws/agent_b/base", second)

	if err := os.Remove(filepath.Join(dir, "engine/facts.c")); err != nil {
		t.Fatal(err)
	}
	third := snapshotAt(t, dir, "2026-11-02T23:20:00Z") // the next agent is cut after the file vanished: no copy in it
	run(t, dir, "git", "update-ref", "refs/relo/vws/agent_c/base", third)

	got, ok := m.RecoverableCopy("engine/facts.c")
	if !ok {
		t.Fatal("no copy found")
	}
	if got.Commit != second || got.Ref != "refs/relo/vws/agent_b/base" {
		t.Fatalf("copy = %+v, want the newest snapshot that still held the file (%s)", got, second)
	}
	if body := output(t, dir, "show", got.Commit+":engine/facts.c"); body != "version two" {
		t.Fatalf("git show gave %q", body)
	}
	if _, ok := m.RecoverableCopy("engine/never.c"); ok {
		t.Fatal("found a copy of a file that never existed")
	}
	if _, ok := m.RecoverableCopy("../outside"); ok {
		t.Fatal("a path outside the repository was searched")
	}
}

// A base that carries proposals nobody accepted is not the user's tree, so it is never offered as a way back.
func TestABaseWithStackedProposalsIsNotARecoverySource(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	write(t, dir, "a.c", "proposed, not accepted\n")
	run(t, dir, "git", "add", "a.c")
	tree := output(t, dir, "write-tree")
	commit := output(t, dir, "commit-tree", tree, "-m", "relo: changes this agent builds on")
	run(t, dir, "git", "update-ref", "refs/relo/vws/agent_x/base", commit)
	if got, ok := m.RecoverableCopy("a.c"); ok {
		t.Fatalf("offered %+v from a base with stacked proposals", got)
	}
}
