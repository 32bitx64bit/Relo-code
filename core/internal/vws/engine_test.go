package vws

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var baseFiles = map[string]string{
	"README.md":        "hello\n",
	"src/main.go":      "package main\n\nfunc main() {}\n",
	"src/util/util.go": "package util\n",
	"docs/guide.md":    "guide\n",
	"run.sh*":          "#!/bin/sh\n",
}

// The public surface the guideline asks for: create, read, write, delete, list, snapshot, diff, commit, dispose.
func TestEngineSurface(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w, err := e.CreateVirtualWorkspace("agent-1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.ReadFile("agent-1", "README.md"); string(got) != "hello\n" {
		t.Fatalf("base file read %q", got)
	}
	if err := e.WriteFile("agent-1", "README.md", []byte("changed\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteFile("agent-1", "new/dir/file.txt", []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteFile("agent-1", "docs/guide.md"); err != nil {
		t.Fatal(err)
	}
	files, err := e.ListFiles("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "README.md,new/dir/file.txt,run.sh,src/main.go,src/util/util.go" {
		t.Fatalf("files = %v", files)
	}
	snap, err := e.Snapshot("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != 3 || snap.Tree == "" || snap.Commit == "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	diff, err := e.Diff("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	samePaths(t, diff, "m:README.md", "a:new/dir/file.txt", "d:docs/guide.md")
	commit, err := e.Commit("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, dir, "rev-parse", "refs/heads/relo/agent-1"); got != commit {
		t.Fatalf("branch = %s, want %s", got, commit)
	}
	if again, _ := e.Commit("agent-1"); again != commit {
		t.Fatalf("a clean commit should return the same commit, got %s", again)
	}
	if info := w.Info(); info.HeadCommit != commit || info.Dirty {
		t.Fatalf("info after commit = %+v", info)
	}
	if err := e.Dispose("agent-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReadFile("agent-1", "README.md"); err == nil {
		t.Fatal("a disposed workspace should be gone")
	}
	if refs := gitT(t, dir, "for-each-ref", "refs/relo", "refs/heads/relo"); refs != "" {
		t.Fatalf("dispose left refs behind:\n%s", refs)
	}
}

// Two agents edit the same base file; neither sees the other, and the user's checkout never moves.
func TestAgentsAreIsolatedAndTheUserCheckoutIsUntouched(t *testing.T) {
	dir := newRepo(t, baseFiles)
	// A dirty tree and an untracked file make the checkout more than a clean HEAD.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := captureUser(t, dir)
	e := openEngine(t, dir)
	a := newWS(t, e, "agent-a")
	b := newWS(t, e, "agent-b")
	mustWrite(t, a, "src/main.go", "package main // a\n")
	mustWrite(t, b, "src/main.go", "package main // b\n")
	mustWrite(t, a, "only-a.txt", "a\n")
	mustWrite(t, b, "only-b.txt", "b\n")
	if got := mustRead(t, a, "src/main.go"); got != "package main // a\n" {
		t.Fatalf("a sees %q", got)
	}
	if got := mustRead(t, b, "src/main.go"); got != "package main // b\n" {
		t.Fatalf("b sees %q", got)
	}
	mustNotExist(t, a, "only-b.txt")
	mustNotExist(t, b, "only-a.txt")
	// The base is the committed state: the user's uncommitted edit is not part of this base.
	if got := mustRead(t, a, "README.md"); got != "hello\n" {
		t.Fatalf("base README = %q", got)
	}
	if _, err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if after := captureUser(t, dir); !before.equal(after) {
		t.Fatalf("the user's checkout changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "src/main.go")); strings.Contains(string(got), "// a") {
		t.Fatal("an agent write reached the user's file")
	}
}

func TestDeletionsAreTombstonesAndLayersAreReported(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	r, err := w.Resolve("docs/guide.md")
	if err != nil || !r.Exists || r.Layer != "base" || string(r.Content) != "guide\n" || r.BlobOID == "" {
		t.Fatalf("base resolve = %+v %v", r, err)
	}
	if err := w.DeleteFile("docs/guide.md"); err != nil {
		t.Fatal(err)
	}
	r, err = w.Resolve("docs/guide.md")
	if err != nil || r.Exists || r.Layer != "overlay" {
		t.Fatalf("a deleted path must resolve as a tombstone in the overlay, got %+v %v", r, err)
	}
	mustNotExist(t, w, "docs/guide.md")
	if entries, err := w.ListDir("docs"); !IsNotExist(err) {
		t.Fatalf("an emptied directory should vanish, got %v %v", entries, err)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	r, _ = w.Resolve("docs/guide.md")
	if r.Exists || r.Layer != "branch" {
		t.Fatalf("after commit the tombstone belongs to the branch layer, got %+v", r)
	}
	mustWrite(t, w, "README.md", "x\n")
	r, _ = w.Resolve("README.md")
	if r.Layer != "overlay" || r.Revision != w.Info().Revision {
		t.Fatalf("resolve = %+v", r)
	}
	// Deleting and recreating the same path is a normal write, not a fall back to the base.
	mustWrite(t, w, "docs/guide.md", "again\n")
	if got := mustRead(t, w, "docs/guide.md"); got != "again\n" {
		t.Fatalf("recreated file = %q", got)
	}
}

func TestPinnedSnapshotsReadIdentically(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "README.md", "v1\n")
	snap, err := w.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 6; i++ {
		mustWrite(t, w, "README.md", fmt.Sprintf("v%d\n", i))
		data, err := w.ReadFileAt(snap, "README.md")
		if err != nil || string(data) != "v1\n" {
			t.Fatalf("pinned read %d = %q %v", i, data, err)
		}
	}
	if got := mustRead(t, w, "README.md"); got != "v5\n" {
		t.Fatalf("live read = %q", got)
	}
	entries, err := w.ListDirAt(snap, "")
	if err != nil || len(entries) == 0 {
		t.Fatalf("pinned listing %v %v", entries, err)
	}
	// The pin keeps the revision's objects through garbage collection.
	gitT(t, dir, "gc", "--prune=now", "-q")
	if data, err := w.ReadFileAt(snap, "README.md"); err != nil || string(data) != "v1\n" {
		t.Fatalf("pinned read after gc = %q %v", data, err)
	}
	if err := w.ReleaseSnapshot(snap); err != nil {
		t.Fatal(err)
	}
}

func TestWritesCanBeGuardedByRevision(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	res, err := w.WriteFileAt("a.txt", []byte("1"), 0)
	if err != nil || res.Revision != 1 || !res.Changed {
		t.Fatalf("first guarded write = %+v %v", res, err)
	}
	// Two writers both read revision 1; the second is stale.
	if _, err := w.WriteFileAt("a.txt", []byte("2"), 1); err != nil {
		t.Fatal(err)
	}
	_, err = w.WriteFileAt("a.txt", []byte("3"), 1)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale write error = %v", err)
	}
	if got := mustRead(t, w, "a.txt"); got != "2" {
		t.Fatalf("a stale write must not land, file = %q", got)
	}
	// Writing the same bytes is not a new revision.
	before := w.Info().Revision
	mustWrite(t, w, "a.txt", "2")
	if w.Info().Revision != before {
		t.Fatal("an identical write should not publish a revision")
	}
}

func TestPathSafety(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	for _, bad := range []string{"", "../x", "a/../../x", "/etc/passwd", `\\server\share`, "C:/x", ".git/config", "a/.git/hooks/x", ".GIT/x", ".relo/relo.db", "a\nb", "git~1/x", ".git /x"} {
		if err := w.WriteFile(bad, []byte("x")); err == nil {
			t.Errorf("write %q should be refused", bad)
		}
		if _, err := w.ReadFile(bad); err == nil {
			t.Errorf("read %q should be refused", bad)
		}
	}
	if err := w.WriteFile(".relo/x", []byte("x")); !errors.Is(err, ErrProtected) {
		t.Fatalf(".relo write error = %v", err)
	}
	if err := w.WriteFile("src/main.go/oops", []byte("x")); !errors.Is(err, ErrNotDir) {
		t.Fatalf("a file used as a directory: %v", err)
	}
	if err := w.WriteFile("src", []byte("x")); !errors.Is(err, ErrIsDir) {
		t.Fatalf("a directory used as a file: %v", err)
	}
	if err := w.DeleteFile("src"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("deleting a directory: %v", err)
	}
	if err := w.DeleteFile("nope.txt"); !IsNotExist(err) {
		t.Fatalf("deleting a missing file: %v", err)
	}
	if _, err := w.ReadFile("src"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("reading a directory: %v", err)
	}
	if w.Info().Revision != 0 {
		t.Fatalf("refused writes must not publish revisions, at r%d", w.Info().Revision)
	}
	// A path spelled with backslashes or dots is the same path once canonical.
	mustWrite(t, w, "./src//extra.go", "x")
	if got := mustRead(t, w, "src/extra.go"); got != "x" {
		t.Fatalf("canonical path read = %q", got)
	}
}

func TestSymlinksAreNeverFollowedOutOfTheWorkspace(t *testing.T) {
	dir := newRepo(t, map[string]string{"real.txt": "real\n", "dir/inner.txt": "inner\n"})
	if err := os.Symlink("real.txt", filepath.Join(dir, "ok-link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "abs-link"))
	_ = os.Symlink("../../outside", filepath.Join(dir, "up-link"))
	_ = os.Symlink("dir", filepath.Join(dir, "dir-link"))
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "links")
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	if got := mustRead(t, w, "ok-link"); got != "real\n" {
		t.Fatalf("an in-workspace link should read through, got %q", got)
	}
	for _, p := range []string{"abs-link", "up-link"} {
		if _, err := w.ReadFile(p); !errors.Is(err, ErrSymlink) {
			t.Errorf("%s: want ErrSymlink, got %v", p, err)
		}
	}
	// A directory that is a link is never entered, for reads or writes.
	if _, err := w.ReadFile("dir-link/inner.txt"); !errors.Is(err, ErrSymlink) {
		t.Errorf("read through a linked directory: %v", err)
	}
	if err := w.WriteFile("dir-link/new.txt", []byte("x")); !errors.Is(err, ErrSymlink) {
		t.Errorf("write through a linked directory: %v", err)
	}
	if err := w.WriteFile("ok-link", []byte("x")); !errors.Is(err, ErrSymlink) {
		t.Errorf("write over a link: %v", err)
	}
	if err := w.Symlink("escape", "../../etc/passwd"); !errors.Is(err, ErrSymlink) {
		t.Errorf("creating an escaping link: %v", err)
	}
	if err := w.Symlink("good", "real.txt"); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, w, "good"); got != "real\n" {
		t.Fatalf("new link read = %q", got)
	}
	info, _ := w.Stat("good")
	if info.Kind != "symlink" {
		t.Fatalf("stat of a link = %+v", info)
	}
}

func TestReadOnlyWorkspacesRejectEveryMutation(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w, err := e.Create("reviewer", "HEAD", CreateOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"write":  w.WriteFile("x", []byte("x")),
		"delete": w.DeleteFile("README.md"),
		"rename": w.Rename("README.md", "R.md"),
		"chmod":  w.Chmod("run.sh", 0o644),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s on a read-only workspace: %v", name, err)
		}
	}
	if got := mustRead(t, w, "README.md"); got != "hello\n" {
		t.Fatalf("read = %q", got)
	}
}

func TestRenameChmodAndReplacement(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	if err := w.Rename("src/util", "src/helpers"); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, w, "src/helpers/util.go"); got != "package util\n" {
		t.Fatalf("moved directory content = %q", got)
	}
	mustNotExist(t, w, "src/util/util.go")
	if err := w.Rename("README.md", "docs/README.md"); err != nil {
		t.Fatal(err)
	}
	if err := w.Rename("src", "src/inside"); err == nil {
		t.Fatal("moving a directory into itself must fail")
	}
	// Mode bits: an existing file keeps its mode across writes, and chmod flips it.
	info, _ := w.Stat("run.sh")
	if info.Mode != modeExec {
		t.Fatalf("run.sh mode = %o", info.Mode)
	}
	mustWrite(t, w, "run.sh", "#!/bin/sh\necho hi\n")
	if info, _ = w.Stat("run.sh"); info.Mode != modeExec {
		t.Fatalf("a rewrite must keep the executable bit, got %o", info.Mode)
	}
	if err := w.Chmod("run.sh", 0o644); err != nil {
		t.Fatal(err)
	}
	if info, _ = w.Stat("run.sh"); info.Mode != modeFile {
		t.Fatalf("chmod 644 gave %o", info.Mode)
	}
	if err := w.WriteFileMode("tool", []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, _ = w.Stat("tool"); info.Mode != modeExec {
		t.Fatalf("new executable = %o", info.Mode)
	}
	// A file replaced by a directory and the reverse, in one batch.
	if _, err := w.Apply([]Op{{Kind: "delete", Path: "docs/guide.md"}, {Kind: "write", Path: "docs/guide.md/part1.md", Data: []byte("p1")}}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, w, "docs/guide.md/part1.md"); got != "p1" {
		t.Fatalf("file replaced by directory: %q", got)
	}
	diff, _ := w.ReadDiff()
	want := []string{"d:README.md", "a:docs/README.md", "d:docs/guide.md", "a:docs/guide.md/part1.md", "m:run.sh", "a:src/helpers/util.go", "d:src/util/util.go", "a:tool"}
	samePaths(t, diff, want...)
}

// An all-or-nothing batch: a failing op leaves no partial revision behind.
func TestBatchesAreAtomic(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	_, err := w.Apply([]Op{
		{Kind: "write", Path: "one.txt", Data: []byte("1")},
		{Kind: "write", Path: "two.txt", Data: []byte("2")},
		{Kind: "delete", Path: "missing.txt"},
	}, ApplyOptions{})
	if !IsNotExist(err) {
		t.Fatalf("batch error = %v", err)
	}
	mustNotExist(t, w, "one.txt")
	mustNotExist(t, w, "two.txt")
	if w.Info().Revision != 0 {
		t.Fatalf("a failed batch published r%d", w.Info().Revision)
	}
	res, err := w.Apply([]Op{{Kind: "write", Path: "one.txt", Data: []byte("1")}, {Kind: "write", Path: "two.txt", Data: []byte("2")}}, ApplyOptions{})
	if err != nil || res.Revision != 1 {
		t.Fatalf("batch = %+v %v", res, err)
	}
}

func TestConcurrentWritersToOneOverlayAreSerialised(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	const writers, perWriter = 8, 6
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := w.WriteFile(fmt.Sprintf("out/w%d/f%d.txt", g, i), []byte(fmt.Sprintf("%d-%d", g, i))); err != nil {
					errs <- err
				}
				_, _ = w.ReadFile("README.md")
				_, _ = w.ListDir("out")
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if rev := w.Info().Revision; rev != writers*perWriter {
		t.Fatalf("revision = %d, want %d", rev, writers*perWriter)
	}
	for g := 0; g < writers; g++ {
		for i := 0; i < perWriter; i++ {
			if got := mustRead(t, w, fmt.Sprintf("out/w%d/f%d.txt", g, i)); got != fmt.Sprintf("%d-%d", g, i) {
				t.Fatalf("lost write: w%d/f%d = %q", g, i, got)
			}
		}
	}
}

func TestRefusesRepositoriesThatRewriteContent(t *testing.T) {
	dir := newRepo(t, baseFiles)
	gitT(t, dir, "config", "core.autocrlf", "true")
	e := openEngine(t, dir)
	if e.Compat().OK {
		t.Fatal("autocrlf should make the repository incompatible")
	}
	if _, err := e.CreateVirtualWorkspace("agent-1", "HEAD"); !errors.Is(err, ErrUnsupportedRepo) {
		t.Fatalf("create under autocrlf: %v", err)
	}

	attr := newRepo(t, map[string]string{".gitattributes": "* text=auto\n", "a.txt": "a\n"})
	e2 := openEngine(t, attr)
	if _, err := e2.CreateVirtualWorkspace("agent-1", "HEAD"); !errors.Is(err, ErrUnsupportedRepo) {
		t.Fatalf("create under text=auto: %v", err)
	}
	safe := newRepo(t, map[string]string{".gitattributes": "*.png binary\n*.lock -text\n", "a.txt": "a\n"})
	if _, err := openEngine(t, safe).CreateVirtualWorkspace("agent-1", "HEAD"); err != nil {
		t.Fatalf("binary and -text attributes are harmless: %v", err)
	}
	lfs := newRepo(t, map[string]string{".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n", "a.txt": "a\n"})
	if _, err := openEngine(t, lfs).CreateVirtualWorkspace("agent-1", "HEAD"); !errors.Is(err, ErrUnsupportedRepo) {
		t.Fatalf("create under an lfs filter: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	for _, id := range []string{"", "a/b", "..", "a..b", "x.lock", "has space", "a:b"} {
		if _, err := e.CreateVirtualWorkspace(id, "HEAD"); err == nil {
			t.Errorf("agent id %q should be refused", id)
		}
	}
	if _, err := e.CreateVirtualWorkspace("ok", "no-such-rev"); err == nil {
		t.Error("an unknown base should be refused")
	}
	if _, err := e.CreateVirtualWorkspace("ok", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateVirtualWorkspace("ok", "HEAD"); !errors.Is(err, ErrExists) {
		t.Fatalf("creating twice: %v", err)
	}
}

func TestBaseIsPinnedForTheLifeOfTheWorkspace(t *testing.T) {
	dir := newRepo(t, baseFiles)
	// A dangling snapshot commit, like the one Relo records for a dirty working tree.
	tree := gitT(t, dir, "rev-parse", "HEAD^{tree}")
	dangling := gitT(t, dir, "commit-tree", tree, "-p", "HEAD", "-m", "snapshot")
	e := openEngine(t, dir)
	if _, err := e.CreateVirtualWorkspace("agent-1", dangling); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "reflog", "expire", "--expire=now", "--all")
	gitT(t, dir, "gc", "--prune=now", "-q")
	if out := gitT(t, dir, "cat-file", "-t", dangling); out != "commit" {
		t.Fatalf("the base commit was collected: %s", out)
	}
}
