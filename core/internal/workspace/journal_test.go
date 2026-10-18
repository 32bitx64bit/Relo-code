package workspace

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJournalUndoRoundTripsAndRefusesAfterAUserEdit(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "a.txt", "old\n")
	run(t, dir, "git", "add", "a.txt")
	run(t, dir, "git", "commit", "-m", "a")
	m := New(dir)

	if _, err := m.JournalChange("chg", []JournalWrite{{Path: "a.txt", Post: []byte("new\n"), PostExists: true}}); err != nil {
		t.Fatalf("journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.JournalEntryFor("chg"); err != nil {
		t.Fatalf("entry not found: %v", err)
	}
	res, err := m.UndoChange("chg")
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != "a.txt" {
		t.Fatalf("restored = %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "old\n" {
		t.Fatalf("undo restored %q, want the pre-image", got)
	}

	// A second accept, then a user edit: the undo must refuse and leave the user's bytes alone.
	if _, err := m.JournalChange("chg2", []JournalWrite{{Path: "a.txt", Post: []byte("accepted\n"), PostExists: true}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("accepted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UndoChange("chg2"); err == nil {
		t.Fatal("undo after a user edit was not refused")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "user\n" {
		t.Fatalf("refused undo touched the file: %q", got)
	}
}

func TestJournalRestoresACreatedPathByRemovingIt(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	if _, err := m.JournalChange("created", []JournalWrite{{Path: "new.txt", Post: []byte("fresh\n"), PostExists: true}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UndoChange("created"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("created file survived the undo: %v", err)
	}
}

func TestSweepPrunesOldJournalAndChangeRefs(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	head := output(t, dir, "rev-parse", "HEAD")
	run(t, dir, "git", "update-ref", "refs/relo/journal/1", head) // UnixNano 1 is far older than 14 days
	if err := os.MkdirAll(filepath.Join(dir, ".relo", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.Sweep(map[string]bool{})
	if refs, _ := m.journalRefs(); len(refs) != 0 {
		t.Fatalf("old journal ref survived the sweep: %v", refs)
	}
	for i := 0; i < changeMax+5; i++ {
		run(t, dir, "git", "update-ref", "refs/relo/changes/c"+strconv.Itoa(i), head)
	}
	m.pruneChangeRefs()
	out, err := gitRaw(m.root, "for-each-ref", "--format=%(refname)", changeRefPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(out)); got != changeMax {
		t.Fatalf("change refs = %d, want %d", got, changeMax)
	}
}

func TestKeepChangeBlobsRetainsEachFile(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	tree, err := m.EnsureTree("blob-agent", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, tree.Path, "one.txt", "first\n")
	write(t, tree.Path, "nested/two.txt", "second\n")
	if err := m.KeepChangeBlobs([]BlobRef{
		{ChangeID: "chgA", Worktree: tree.Path, Path: "one.txt"},
		{ChangeID: "chgB", Worktree: tree.Path, Path: "nested/two.txt"},
		{ChangeID: "chgC", Worktree: tree.Path, Path: "missing.txt"},
	}); err != nil {
		t.Fatalf("keep blobs: %v", err)
	}
	out, err := gitRaw(m.root, "for-each-ref", "--format=%(refname)", changeRefPrefix)
	if err != nil {
		t.Fatal(err)
	}
	refs := strings.Fields(out)
	if len(refs) != 2 {
		t.Fatalf("change refs = %v, want 2", refs)
	}
	got := map[string]string{}
	for _, ref := range refs {
		body, err := gitRaw(m.root, "cat-file", "-p", ref)
		if err != nil {
			t.Fatalf("cat-file %s: %v", ref, err)
		}
		if strings.HasSuffix(ref, "chgA") {
			got["a"] = body
		}
		if strings.HasSuffix(ref, "chgB") {
			got["b"] = body
		}
	}
	if got["a"] != "first\n" || got["b"] != "second\n" {
		t.Fatalf("retained blobs = %+v", got)
	}
}

func TestPruneChangeRefsDropsRefsOlderThanRetention(t *testing.T) {
	dir := initRepo(t)
	m := New(dir)
	head := output(t, dir, "rev-parse", "HEAD")
	old := strconv.FormatInt(time.Now().Add(-journalRetention-time.Hour).UnixNano(), 10)
	fresh := strconv.FormatInt(time.Now().UnixNano(), 10)
	run(t, dir, "git", "update-ref", changeRefPrefix+old+"-stale", head)
	run(t, dir, "git", "update-ref", changeRefPrefix+fresh+"-recent", head)
	m.pruneChangeRefs()
	out, err := gitRaw(m.root, "for-each-ref", "--format=%(refname)", changeRefPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if refs := strings.Fields(out); len(refs) != 1 || !strings.HasSuffix(refs[0], "-recent") {
		t.Fatalf("change refs after prune = %v, want only the fresh ref", refs)
	}
}

func TestUndoOfADeletedPathRestoresItsBytes(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "gone.txt", "keep me\n")
	run(t, dir, "git", "add", "gone.txt")
	run(t, dir, "git", "commit", "-m", "a")
	m := New(dir)
	if _, err := m.JournalChange("deleted", []JournalWrite{{Path: "gone.txt", PostExists: false}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UndoChange("deleted"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "gone.txt")); string(got) != "keep me\n" {
		t.Fatalf("restored %q", got)
	}
}
