package vws

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reopen(t *testing.T, dir string, opts ...func(*Options)) *Engine {
	t.Helper()
	return openEngine(t, dir, opts...)
}

// Restarting Relo preserves both agents' modifications, their revisions and their metadata.
func TestRestartPreservesEveryWorkspace(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e1 := openEngine(t, dir)
	a := newWS(t, e1, "agent-a")
	b := newWS(t, e1, "agent-b")
	mustWrite(t, a, "src/main.go", "package main // a\n")
	mustWrite(t, a, "new-a.txt", "a\n")
	mustWrite(t, b, "src/main.go", "package main // b\n")
	if err := b.DeleteFile("docs/guide.md"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMeta("lineage", `[{"id":"chg-1","patchHash":"abc"}]`); err != nil {
		t.Fatal(err)
	}
	commitA, err := a.Commit()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, a, "after-commit.txt", "uncommitted\n")
	infoA, infoB := a.Info(), b.Info()

	e2 := reopen(t, dir)
	rep, err := e2.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Recovered) != 2 || len(rep.Problems) != 0 {
		t.Fatalf("recover = %+v", rep)
	}
	a2, ok := e2.Get("agent-a")
	if !ok {
		t.Fatal("agent-a was not recovered")
	}
	b2, _ := e2.Get("agent-b")
	got := a2.Info()
	if got.Revision != infoA.Revision || got.EffectiveTree != infoA.EffectiveTree || got.HeadCommit != commitA || got.BaseCommit != infoA.BaseCommit || !got.Dirty {
		t.Fatalf("agent-a after restart = %+v, before = %+v", got, infoA)
	}
	if a2.Meta("lineage") == "" {
		t.Fatal("metadata did not survive the restart")
	}
	if got := b2.Info(); got.Revision != infoB.Revision || got.EffectiveTree != infoB.EffectiveTree {
		t.Fatalf("agent-b after restart = %+v", got)
	}
	if got := mustRead(t, a2, "after-commit.txt"); got != "uncommitted\n" {
		t.Fatalf("uncommitted overlay lost: %q", got)
	}
	mustNotExist(t, b2, "docs/guide.md")
	if got := mustRead(t, b2, "src/main.go"); got != "package main // b\n" {
		t.Fatalf("agent-b content = %q", got)
	}
	mustNotExist(t, a2, "docs/other") // each agent still has its own view
	// Work continues from the recovered revision.
	mustWrite(t, a2, "more.txt", "m\n")
	if a2.Info().Revision != infoA.Revision+1 {
		t.Fatalf("revision after restart = %d", a2.Info().Revision)
	}
	if again, _ := e2.Recover(context.Background()); len(again.Recovered) != 0 {
		t.Fatalf("a second recover should find nothing new: %+v", again)
	}
}

// A crash between writing objects and publishing the revision leaves the previous revision, not a hybrid.
func TestAFailedPublishLeavesThePreviousRevision(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "kept.txt", "kept\n")
	before := w.Info()
	crash := errors.New("simulated crash before the ref moved")
	e.fault = func(string) error { return crash }
	if err := w.WriteFile("lost.txt", []byte("lost\n")); !errors.Is(err, crash) {
		t.Fatalf("write error = %v", err)
	}
	if _, err := w.Commit(); err != nil && !errors.Is(err, crash) {
		t.Fatal(err)
	}
	e.fault = nil
	if got := w.Info(); got.Revision != before.Revision || got.EffectiveTree != before.EffectiveTree || got.HeadCommit != "" {
		t.Fatalf("state moved on a failed publish: %+v", got)
	}
	mustNotExist(t, w, "lost.txt")

	// The same view after a restart: only the published revision exists.
	e2 := reopen(t, dir)
	if _, err := e2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	w2, _ := e2.Get("agent-1")
	if w2.Info().Revision != before.Revision || mustRead(t, w2, "kept.txt") != "kept\n" {
		t.Fatalf("recovered = %+v", w2.Info())
	}
	mustNotExist(t, w2, "lost.txt")
	if refs := gitT(t, dir, "for-each-ref", "refs/heads/relo"); refs != "" {
		t.Fatalf("a commit that failed to publish left a branch: %s", refs)
	}
	// The workspace is still usable afterwards.
	mustWrite(t, w, "lost.txt", "now\n")
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "fsck", "--no-dangling")
}

// Someone else moved the ref: the write is refused and the engine's view does not change.
func TestAMovedRefIsReportedNotOverwritten(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "a.txt", "a\n")
	gitT(t, dir, "update-ref", overlayRef("agent-1"), gitT(t, dir, "rev-parse", "HEAD"))
	err := w.WriteFile("b.txt", []byte("b\n"))
	if !errors.Is(err, ErrRefConflict) {
		t.Fatalf("write after the ref moved: %v", err)
	}
	if got := mustRead(t, w, "a.txt"); got != "a\n" {
		t.Fatalf("in-memory state changed: %q", got)
	}
	mustNotExist(t, w, "b.txt")
}

func TestGitGarbageCollectionKeepsLiveOverlays(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	a := newWS(t, e, "agent-a")
	b := newWS(t, e, "agent-b")
	for i := 0; i < 5; i++ {
		mustWrite(t, a, "gen/file.txt", strings.Repeat("a", 100+i))
		mustWrite(t, b, "other/file.txt", strings.Repeat("b", 100+i))
	}
	if _, err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, b, "later.txt", "uncommitted\n")
	snap, _ := a.Snapshot()
	mustWrite(t, a, "gen/file.txt", "newest")
	gitT(t, dir, "reflog", "expire", "--expire=now", "--all")
	gitT(t, dir, "gc", "--prune=now", "--aggressive", "-q")
	if got := mustRead(t, a, "gen/file.txt"); got != "newest" {
		t.Fatalf("live overlay content after gc = %q", got)
	}
	if got, err := a.ReadFileAt(snap, "gen/file.txt"); err != nil || string(got) != strings.Repeat("a", 104) {
		t.Fatalf("pinned snapshot after gc = %q %v", got, err)
	}
	e2 := reopen(t, dir)
	rep, err := e2.Recover(context.Background())
	if err != nil || len(rep.Problems) != 0 || len(rep.Recovered) != 2 {
		t.Fatalf("recover after gc = %+v %v", rep, err)
	}
	b2, _ := e2.Get("agent-b")
	if got := mustRead(t, b2, "later.txt"); got != "uncommitted\n" {
		t.Fatalf("uncommitted file after gc and restart = %q", got)
	}
	// Disposing releases the objects: a prune after that may collect them, and the other agent is unaffected.
	if err := e2.Dispose("agent-a"); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "gc", "--prune=now", "-q")
	if got := mustRead(t, b2, "later.txt"); got != "uncommitted\n" {
		t.Fatalf("other agent after disposing: %q", got)
	}
}

func TestRecoverReportsDamagedWorkspacesAndRestoresTheRest(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	good := newWS(t, e, "good")
	bad := newWS(t, e, "bad")
	mustWrite(t, good, "g.txt", "good\n")
	mustWrite(t, bad, "b.txt", "unique bad content\n")
	badBlob, _ := bad.Resolve("b.txt")
	loose := filepath.Join(dir, ".git", "objects", badBlob.BlobOID[:2], badBlob.BlobOID[2:])
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}
	e2 := reopen(t, dir)
	rep, err := e2.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Recovered) != 1 || rep.Recovered[0] != "good" || len(rep.Problems) != 1 || rep.Problems[0].AgentID != "bad" {
		t.Fatalf("recover = %+v", rep)
	}
	if !strings.Contains(rep.Problems[0].Err, "missing") {
		t.Fatalf("problem = %q", rep.Problems[0].Err)
	}
	if g, ok := e2.Get("good"); !ok || mustRead(t, g, "g.txt") != "good\n" {
		t.Fatal("the healthy workspace was not restored")
	}
}

func TestOverlayRefsDoNotAppearInTheUsersBranches(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "x", "x")
	if out := gitT(t, dir, "branch", "--list"); strings.Contains(out, "relo") {
		t.Fatalf("an uncommitted overlay should not create a branch:\n%s", out)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if out := gitT(t, dir, "branch", "--list", "relo/*"); !strings.Contains(out, "relo/agent-1") {
		t.Fatalf("a commit should create the agent branch:\n%s", out)
	}
	// The branch carries exactly the agent's change on top of its base.
	if out := gitT(t, dir, "diff", "--name-status", "HEAD", "relo/agent-1"); out != "A\tx" {
		t.Fatalf("branch diff = %q", out)
	}
	if n := gitT(t, dir, "rev-list", "--count", "HEAD..relo/agent-1"); n != "1" {
		t.Fatalf("branch should be one commit ahead, got %s", n)
	}
}
