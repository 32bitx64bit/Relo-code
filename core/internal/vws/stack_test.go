package vws

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A tool that reverts an earlier edit takes the overlay back: capture reports what the directory holds, not a
// running total.
func TestCaptureFollowsToolsRevertingTheirEdits(t *testing.T) {
	dir := newRepo(t, execFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, ex.Dir, "src/lib.c", "changed\n", 0o644)
	if res, err := w.Capture(context.Background()); err != nil || !res.Changed {
		t.Fatalf("first capture = %+v %v", res, err)
	}
	writeIn(t, ex.Dir, "src/lib.c", execFiles["src/lib.c"], 0o644)
	if res, err := w.Capture(context.Background()); err != nil || !res.Changed {
		t.Fatalf("second capture = %+v %v", res, err)
	}
	if got := mustRead(t, w, "src/lib.c"); got != execFiles["src/lib.c"] {
		t.Fatalf("reverted file = %q", got)
	}
	if w.Info().Dirty {
		t.Fatal("the workspace should be clean again")
	}
}

// What one agent builds is stacked under another without a checkout, and the second agent's own diff holds only
// its additions.
func TestStackMakesParentWorkTheBaseOfADependent(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	parent := newWS(t, e, "parent")
	child := newWS(t, e, "child")
	mustWrite(t, parent, "api.go", "package api\n\nfunc New() {}\n")
	mustWrite(t, parent, "README.md", "parent edited\n")
	patch, err := parent.Patch()
	if err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, child, "api.go") // no implicit visibility
	userBefore := captureUser(t, dir)
	oldBase := child.Info().BaseCommit
	res, err := child.Stack([]string{patch})
	if err != nil || len(res) != 1 || !res[0].Applied {
		t.Fatalf("stack = %+v %v", res, err)
	}
	if got := mustRead(t, child, "api.go"); got != "package api\n\nfunc New() {}\n" {
		t.Fatalf("stacked content = %q", got)
	}
	if info := child.Info(); info.BaseCommit == oldBase || info.Dirty || info.HeadCommit != "" {
		t.Fatalf("after stacking: %+v", info)
	}
	if diff, _ := child.ReadDiff(); len(diff) != 0 {
		t.Fatalf("stacked work should not count as the child's diff: %+v", diff)
	}
	mustWrite(t, child, "client.go", "package client\n")
	diff, _ := child.ReadDiff()
	samePaths(t, diff, "a:client.go")
	if !userBefore.equal(captureUser(t, dir)) {
		t.Fatal("stacking touched the user's checkout")
	}
	// The parent is unaffected.
	if got := mustRead(t, parent, "README.md"); got != "parent edited\n" {
		t.Fatalf("parent = %q", got)
	}
	mustNotExist(t, parent, "client.go")
}

func TestStackSkipsPatchesThatDoNotApply(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	a, b, c := newWS(t, e, "a"), newWS(t, e, "b"), newWS(t, e, "c")
	mustWrite(t, a, "README.md", "a version\n")
	mustWrite(t, b, "README.md", "b version\n")
	mustWrite(t, b, "b-only.txt", "b\n")
	pa, _ := a.Patch()
	pb, _ := b.Patch()
	res, err := c.Stack([]string{pa, pb})
	if err == nil {
		t.Fatal("expected an error for the patch that cannot apply")
	}
	if !res[0].Applied || res[1].Applied || res[1].Error == "" {
		t.Fatalf("results = %+v", res)
	}
	if got := mustRead(t, c, "README.md"); got != "a version\n" {
		t.Fatalf("README = %q", got)
	}
	mustNotExist(t, c, "b-only.txt") // a patch is all or nothing
}

func TestStackWorksOnReadOnlyWorkspacesAndNeverOnOpenDirectories(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	src := newWS(t, e, "src")
	mustWrite(t, src, "feature.txt", "feature\n")
	patch, _ := src.Patch()
	rv, err := e.Create("reviewer", "HEAD", CreateOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rv.Stack([]string{patch}); err != nil {
		t.Fatalf("a reviewer must be able to be shown the work it reviews: %v", err)
	}
	if got := mustRead(t, rv, "feature.txt"); got != "feature\n" {
		t.Fatalf("reviewer sees %q", got)
	}
	if err := rv.WriteFile("x", []byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a reviewer wrote: %v", err)
	}
	other := newWS(t, e, "other")
	if _, err := other.Materialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Stack([]string{patch}); !errors.Is(err, ErrMaterialized) {
		t.Fatalf("stack while materialized: %v", err)
	}
}

func TestRecutDropsTheOverlayAndMovesTheBase(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "draft.txt", "draft\n")
	oldBase := w.Info().BaseCommit
	// The user accepts something, so HEAD moves.
	if err := os.WriteFile(filepath.Join(dir, "accepted.txt"), []byte("accepted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "accepted.txt")
	gitT(t, dir, "commit", "-q", "-m", "accepted")
	if err := w.Recut("HEAD"); err != nil {
		t.Fatal(err)
	}
	info := w.Info()
	if info.BaseCommit == oldBase || info.BaseCommit != gitT(t, dir, "rev-parse", "HEAD") || info.Dirty {
		t.Fatalf("after recut: %+v", info)
	}
	mustNotExist(t, w, "draft.txt")
	if got := mustRead(t, w, "accepted.txt"); got != "accepted\n" {
		t.Fatalf("recut base content = %q", got)
	}
	// The old base is no longer pinned by this workspace, and the new one is.
	if got := gitT(t, dir, "rev-parse", baseRef("agent-1")); got != info.BaseCommit {
		t.Fatalf("base pin = %s", got)
	}
	if err := w.Recut(""); err == nil {
		t.Fatal("recut without a base should fail")
	}
}

func TestRebaseOntoKeepsEditsAndReportsConflicts(t *testing.T) {
	dir := newRepo(t, map[string]string{"a.txt": "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "b.txt": "b\n", "c.txt": "c\n"})
	e := openEngine(t, dir)
	clean := newWS(t, e, "clean")
	clash := newWS(t, e, "clash")
	// Agent edits the top of a.txt (clean) / the bottom of b.txt (clash).
	mustWrite(t, clean, "a.txt", "ONE\n2\n3\n4\n5\n6\n7\n8\n9\n")
	mustWrite(t, clash, "b.txt", "mine\n")
	// The user's accepted work edits the bottom of a.txt and b.txt.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1\n2\n3\n4\n5\n6\n7\n8\nNINE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "commit", "-aqm", "accepted")
	head := gitT(t, dir, "rev-parse", "HEAD")

	if err := clean.RebaseOnto(head); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, clean, "a.txt"); got != "ONE\n2\n3\n4\n5\n6\n7\n8\nNINE\n" {
		t.Fatalf("merged = %q", got)
	}
	if info := clean.Info(); info.BaseCommit != head || !info.Dirty {
		t.Fatalf("after rebase: %+v", info)
	}
	diff, _ := clean.ReadDiff()
	samePaths(t, diff, "m:a.txt") // only the agent's own line now differs from the new base

	rev := clash.Info().Revision
	err := clash.RebaseOnto(head)
	var rc ErrRebaseConflict
	if !errors.As(err, &rc) || len(rc.Paths) != 1 || rc.Paths[0] != "b.txt" {
		t.Fatalf("rebase = %v", err)
	}
	if info := clash.Info(); info.Revision != rev || info.BaseCommit == head {
		t.Fatalf("a conflicting rebase changed the workspace: %+v", info)
	}
	if got := mustRead(t, clash, "b.txt"); got != "mine\n" {
		t.Fatalf("agent's work after a failed rebase = %q", got)
	}
	// Rebasing onto the base it already has is a no-op.
	if err := clean.RebaseOnto(head); err != nil {
		t.Fatal(err)
	}
}

func TestImportFromIsExplicitAndSelective(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	src := newWS(t, e, "src")
	dst := newWS(t, e, "dst")
	mustWrite(t, src, "pkg/one.go", "one\n")
	mustWrite(t, src, "pkg/two.go", "two\n")
	if err := src.DeleteFile("docs/guide.md"); err != nil {
		t.Fatal(err)
	}
	snap, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, src, "pkg/one.go", "one changed later\n")
	mustNotExist(t, dst, "pkg/one.go")

	res, err := dst.ImportFrom(snap, []string{"pkg/one.go", "docs/guide.md"}, ApplyOptions{})
	if err != nil || !res.Changed {
		t.Fatalf("import = %+v %v", res, err)
	}
	if got := mustRead(t, dst, "pkg/one.go"); got != "one\n" {
		t.Fatalf("imported the snapshot, not the live file: %q", got)
	}
	mustNotExist(t, dst, "pkg/two.go") // not requested, so not visible
	mustNotExist(t, dst, "docs/guide.md")
	if dst.Meta("import:src") == "" {
		t.Fatal("the import was not recorded")
	}
	// A stale expectation is refused.
	stale := dst.Info().Revision - 1
	if _, err := dst.ImportFrom(snap, []string{"pkg/two.go"}, ApplyOptions{ExpectRevision: &stale}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale import: %v", err)
	}
	for _, bad := range []string{".git/config", "../x", ".relo/state"} {
		if _, err := dst.ImportFrom(snap, []string{bad}, ApplyOptions{}); err == nil {
			t.Fatalf("import of %q should be refused", bad)
		}
	}
}

func TestSnapshotAtReturnsThePinnedRevisionAfterTheWorkspaceMovesOn(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "run.sh", "v1\n")
	if err := w.Chmod("run.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	snap, err := w.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, w, "run.sh", "v2\n")
	if err := w.DeleteFile("README.md"); err != nil {
		t.Fatal(err)
	}
	got, err := w.SnapshotAt(snap.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tree != snap.Tree || got.Commit != snap.Commit || got.BaseCommit != snap.BaseCommit || got.Branch != snap.Branch {
		t.Fatalf("SnapshotAt = %+v, want %+v", got, snap)
	}
	if data, err := w.ReadFileAt(got, "run.sh"); err != nil || string(data) != "v1\n" {
		t.Fatalf("pinned read = %q %v", data, err)
	}
	if fi, err := w.StatAt(got, "run.sh"); err != nil || fi.Mode != modeExec || fi.Revision != snap.Revision {
		t.Fatalf("pinned stat = %+v %v", fi, err)
	}
	if _, err := w.ReadFileAt(got, "README.md"); err != nil {
		t.Fatalf("the snapshot should still have the file deleted later: %v", err)
	}
	// Restart: the pin is in Git, not in memory.
	e2 := reopen(t, dir)
	if _, err := e2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	w2, _ := e2.Get("agent-1")
	again, err := w2.SnapshotAt(snap.Revision)
	if err != nil || again.Tree != snap.Tree {
		t.Fatalf("after restart: %+v %v", again, err)
	}
	if err := w2.ReleaseSnapshot(again); err != nil {
		t.Fatal(err)
	}
	if _, err := w2.SnapshotAt(snap.Revision); err == nil {
		t.Fatal("a released snapshot should be gone")
	}
	if _, err := w2.SnapshotAt(9999); err == nil {
		t.Fatal("a revision that was never pinned should be refused")
	}
}
