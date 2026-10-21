package vws

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeIn(t *testing.T, dir, rel, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

var execFiles = map[string]string{
	".gitignore": "*.o\nbuild/\n", "main.c": "int main(){return 0;}\n", "src/lib.c": "int lib(){return 1;}\n",
	"run.sh*": "#!/bin/sh\n", "obj/keep.txt": "tracked\n",
}

// The directory holds exactly the revision's files, and tools and the model see the same bytes.
func TestMaterializedDirectoryMatchesTheRevision(t *testing.T) {
	dir := newRepo(t, execFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "src/lib.c", "int lib(){return 2;}\n")
	mustWrite(t, w, "added/file.txt", "added\n")
	if err := w.DeleteFile("main.c"); err != nil {
		t.Fatal(err)
	}
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ex.Dir != filepath.Join(dir, ".relo", "worktrees", "agent-1") || ex.Backend != "worktree" {
		t.Fatalf("exec = %+v", ex)
	}
	files, _ := w.Files()
	var onDisk []string
	_ = filepath.WalkDir(ex.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() != ".git" {
			rel, _ := filepath.Rel(ex.Dir, p)
			onDisk = append(onDisk, filepath.ToSlash(rel))
			want, _ := w.ReadFile(filepath.ToSlash(rel))
			got, _ := os.ReadFile(p)
			if string(want) != string(got) {
				t.Errorf("%s: tool sees %q, model sees %q", rel, got, want)
			}
		}
		return nil
	})
	sort.Strings(onDisk)
	if strings.Join(onDisk, ",") != strings.Join(files, ",") {
		t.Fatalf("directory has %v, revision has %v", onDisk, files)
	}
	if st, err := os.Stat(filepath.Join(ex.Dir, "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable bit lost: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(ex.Dir, "main.c")); err == nil {
		t.Fatal("a deleted file came back in the directory")
	}
	// While the directory is open it is authoritative: the overlay cannot be edited behind the tools' back.
	if err := w.WriteFile("x", []byte("x")); !errors.Is(err, ErrMaterialized) {
		t.Fatalf("overlay write while materialized: %v", err)
	}
	if w.Info().State != StateMaterialized {
		t.Fatalf("state = %s", w.Info().State)
	}
	// Materializing again returns the same directory.
	again, err := w.Materialize(context.Background())
	if err != nil || again.Dir != ex.Dir || again.ID != ex.ID {
		t.Fatalf("second materialize = %+v %v", again, err)
	}
}

func TestCaptureImportsToolEditsAndSkipsWhatMustNotBeImported(t *testing.T) {
	dir := newRepo(t, execFiles)
	userBefore := captureUser(t, dir)
	e := openEngine(t, dir, func(o *Options) {
		o.Ignore = func(rel string) bool { return strings.HasPrefix(rel, "generated/") }
	})
	w := newWS(t, e, "agent-1")
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// What a compiler, an editor and a shell script would do.
	writeIn(t, ex.Dir, "src/lib.c", "int lib(){return 42;}\n", 0o644)
	writeIn(t, ex.Dir, "new.c", "int n;\n", 0o644)
	writeIn(t, ex.Dir, "script.sh", "#!/bin/sh\n", 0o755)
	writeIn(t, ex.Dir, "lib.o", "OBJECT", 0o644)        // ignored by .gitignore
	writeIn(t, ex.Dir, "build/out.bin", "BIN", 0o644)   // ignored by .gitignore
	writeIn(t, ex.Dir, "generated/x.txt", "gen", 0o644) // ignored by the caller
	if err := os.Remove(filepath.Join(ex.Dir, "main.c")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(ex.Dir, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Symlink("src/lib.c", filepath.Join(ex.Dir, "fine-link")); err != nil {
		t.Fatal(err)
	}
	res, err := w.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("capture = %+v", res)
	}
	sort.Strings(res.Imported)
	if got := strings.Join(res.Imported, ","); got != "fine-link,main.c,new.c,script.sh,src/lib.c" {
		t.Fatalf("imported %s", got)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Path != "escape" || !strings.Contains(res.Rejected[0].Reason, "symlink") {
		t.Fatalf("rejected = %+v", res.Rejected)
	}
	if res.Ignored != 1 {
		t.Fatalf("ignored = %d", res.Ignored)
	}
	diff, _ := w.ReadDiff()
	samePaths(t, diff, "m:src/lib.c", "d:main.c", "a:new.c", "a:script.sh", "a:fine-link")
	// The model now sees what the tools wrote.
	if got := mustRead(t, w, "src/lib.c"); got != "int lib(){return 42;}\n" {
		t.Fatalf("captured content = %q", got)
	}
	mustNotExist(t, w, "main.c")
	mustNotExist(t, w, "lib.o")
	mustNotExist(t, w, "build/out.bin")
	if info, _ := w.Stat("script.sh"); info.Mode != modeExec {
		t.Fatalf("captured mode = %o", info.Mode)
	}
	// Capturing again with nothing new changes nothing.
	rev := w.Info().Revision
	res, err = w.Capture(context.Background())
	if err != nil || res.Changed || w.Info().Revision != rev {
		t.Fatalf("second capture = %+v %v, revision %d -> %d", res, err, rev, w.Info().Revision)
	}
	// Releasing removes the directory and everything Git knew about it, and the user's checkout never moved.
	if _, err := w.ReleaseExec(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ex.Dir); err == nil {
		t.Fatal("the directory survived release")
	}
	if list := gitT(t, dir, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("worktree metadata left behind:\n%s", list)
	}
	if w.Info().State != StateReady {
		t.Fatalf("state = %s", w.Info().State)
	}
	if after := captureUser(t, dir); !userBefore.equal(after) {
		t.Fatal("materializing changed the user's checkout")
	}
	// After release the overlay is editable again, and a new materialization carries the captured work.
	mustWrite(t, w, "later.txt", "later\n")
	ex2, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(ex2.Dir, "src/lib.c")); string(got) != "int lib(){return 42;}\n" {
		t.Fatalf("second directory has %q", got)
	}
	if _, err := os.Stat(filepath.Join(ex2.Dir, "lib.o")); err == nil {
		t.Fatal("ignored build output came back")
	}
}

// Test-generated outputs cannot corrupt the shared base: every agent's directory is separate and the base
// commit is never touched.
func TestToolOutputNeverReachesTheBaseOrOtherAgents(t *testing.T) {
	dir := newRepo(t, execFiles)
	baseTree := gitT(t, dir, "rev-parse", "HEAD^{tree}")
	e := openEngine(t, dir)
	a, b := newWS(t, e, "agent-a"), newWS(t, e, "agent-b")
	exA, err := a.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exB, err := b.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, exA.Dir, "src/lib.c", "A\n", 0o644)
	writeIn(t, exA.Dir, "outputs/report.txt", "A out\n", 0o644)
	if _, err := a.Capture(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(exB.Dir, "src/lib.c")); string(got) != execFiles["src/lib.c"] {
		t.Fatalf("agent B's directory saw agent A's edit: %q", got)
	}
	if _, err := os.Stat(filepath.Join(exB.Dir, "outputs")); err == nil {
		t.Fatal("agent B's directory has agent A's output")
	}
	if got := gitT(t, dir, "rev-parse", "HEAD^{tree}"); got != baseTree {
		t.Fatal("the base moved")
	}
	if diff, _ := b.ReadDiff(); len(diff) != 0 {
		t.Fatalf("agent B changed nothing, diff = %+v", diff)
	}
}

func TestReadOnlyMaterializationImportsNothing(t *testing.T) {
	dir := newRepo(t, execFiles)
	e := openEngine(t, dir)
	w, err := e.Create("reviewer", "HEAD", CreateOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, ex.Dir, "sneaky.txt", "x", 0o644)
	if err := os.Remove(filepath.Join(ex.Dir, "main.c")); err != nil {
		t.Fatal(err)
	}
	res, err := w.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || len(res.Rejected) != 2 {
		t.Fatalf("capture on a read-only workspace = %+v", res)
	}
	if diff, _ := w.ReadDiff(); len(diff) != 0 {
		t.Fatalf("a read-only workspace changed: %+v", diff)
	}
	if _, err := w.ReleaseExec(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

// Relo stops while a tool's directory is open: on the next start the work in it is imported, then the directory
// is removed, and nothing is lost.
func TestInterruptedMaterializationIsRecovered(t *testing.T) {
	dir := newRepo(t, execFiles)
	e1 := openEngine(t, dir)
	w := newWS(t, e1, "agent-1")
	mustWrite(t, w, "overlay.txt", "from the overlay\n")
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, ex.Dir, "unfinished.c", "int half_done;\n", 0o644)
	writeIn(t, ex.Dir, "src/lib.c", "int lib(){return 7;}\n", 0o644)

	// The process dies here: no capture, no release.
	e2 := reopen(t, dir)
	rep, err := e2.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Interrupted) != 1 || rep.Interrupted[0] != "agent-1" || len(rep.Problems) != 0 {
		t.Fatalf("recover = %+v", rep)
	}
	if res := rep.Captured["agent-1"]; !res.Changed {
		t.Fatalf("capture result = %+v", res)
	}
	w2, _ := e2.Get("agent-1")
	if got := mustRead(t, w2, "unfinished.c"); got != "int half_done;\n" {
		t.Fatalf("unfinished work was lost: %q", got)
	}
	if got := mustRead(t, w2, "overlay.txt"); got != "from the overlay\n" {
		t.Fatalf("overlay content = %q", got)
	}
	if _, err := os.Stat(ex.Dir); err == nil {
		t.Fatal("the recovered directory was not removed")
	}
	if w2.Info().State != StateReady {
		t.Fatalf("state = %s", w2.Info().State)
	}
	if refs := gitT(t, dir, "for-each-ref", "refs/relo/vws/agent-1/exec"); refs != "" {
		t.Fatalf("the exec pin was left behind: %s", refs)
	}
}

func TestInterruptedMaterializationWhoseDirectoryIsGone(t *testing.T) {
	dir := newRepo(t, execFiles)
	e1 := openEngine(t, dir)
	w := newWS(t, e1, "agent-1")
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ex.Dir); err != nil {
		t.Fatal(err)
	}
	e2 := reopen(t, dir)
	rep, err := e2.Recover(context.Background())
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("recover = %+v %v", rep, err)
	}
	w2, _ := e2.Get("agent-1")
	if w2.Info().State != StateReady {
		t.Fatalf("state = %s", w2.Info().State)
	}
	if err := w2.WriteFile("ok.txt", []byte("ok")); err != nil {
		t.Fatalf("workspace should be editable again: %v", err)
	}
}

func TestMaterializedRevisionSurvivesGarbageCollection(t *testing.T) {
	dir := newRepo(t, execFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	mustWrite(t, w, "src/lib.c", "unique new content for gc\n")
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "reflog", "expire", "--expire=now", "--all")
	gitT(t, dir, "gc", "--prune=now", "-q")
	if _, err := w.Capture(context.Background()); err != nil {
		t.Fatalf("capture after gc: %v", err)
	}
	if got := mustRead(t, w, "src/lib.c"); got != "unique new content for gc\n" {
		t.Fatalf("content after gc = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(ex.Dir, "src/lib.c")); string(got) != "unique new content for gc\n" {
		t.Fatalf("directory after gc = %q", got)
	}
}

func TestDisposeRefusesAnOpenMaterialization(t *testing.T) {
	dir := newRepo(t, execFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	if _, err := w.Materialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Dispose(false); !errors.Is(err, ErrMaterialized) {
		t.Fatalf("dispose = %v", err)
	}
	if err := w.DiscardExec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Dispose(false); err != nil {
		t.Fatal(err)
	}
}

// A proposal is committed while the tools' directory is still open, and later edits in it are still captured.
func TestCommitWhileMaterializedRecordsTheImportedState(t *testing.T) {
	dir := newRepo(t, execFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, ex.Dir, "first.txt", "first\n", 0o644)
	if _, err := w.Capture(context.Background()); err != nil {
		t.Fatal(err)
	}
	c1, err := w.Commit()
	if err != nil {
		t.Fatalf("commit while materialized: %v", err)
	}
	if again, err := w.Commit(); err != nil || again != c1 {
		t.Fatalf("a second commit of the same state = %q %v, want %q", again, err, c1)
	}
	if out := gitT(t, dir, "show", "--stat", "--format=", c1); !strings.Contains(out, "first.txt") {
		t.Fatalf("commit does not hold the imported file:\n%s", out)
	}
	// The agent keeps working in the same directory.
	writeIn(t, ex.Dir, "second.txt", "second\n", 0o644)
	if res, err := w.Capture(context.Background()); err != nil || !res.Changed {
		t.Fatalf("capture after commit = %+v %v", res, err)
	}
	c2, err := w.Commit()
	if err != nil || c2 == c1 {
		t.Fatalf("second commit = %q %v", c2, err)
	}
	if n := gitT(t, dir, "rev-list", "--count", "HEAD.."+"relo/agent-1"); n != "2" {
		t.Fatalf("branch is %s commits ahead, want 2", n)
	}
	if _, err := w.ReleaseExec(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, w, "second.txt"); got != "second\n" {
		t.Fatalf("second.txt = %q", got)
	}
}

// ReadLive answers for a single tracked file without scanning the directory, and gives the bytes a capture would
// have imported. Anything it cannot answer exactly the same way is left to a capture.
func TestReadLiveMatchesACaptureWithoutScanning(t *testing.T) {
	dir := newRepo(t, execFiles)
	e := openEngine(t, dir, func(o *Options) {
		o.Ignore = func(rel string) bool { return strings.HasPrefix(rel, "generated/") }
	})
	w := newWS(t, e, "agent-1")
	if _, ok, _ := w.ReadLive("main.c"); ok {
		t.Fatal("answered with no directory open")
	}
	ex, err := w.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, ex.Dir, "src/lib.c", "int lib(){return 42;}\n", 0o644)
	writeIn(t, ex.Dir, "new.c", "int n;\n", 0o644)
	writeIn(t, ex.Dir, "lib.o", "OBJECT", 0o644)
	if err := os.Remove(filepath.Join(ex.Dir, "main.c")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/lib.c", filepath.Join(ex.Dir, "obj", "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	before := e.Metrics().Captures

	if data, ok, err := w.ReadLive("src/lib.c"); !ok || err != nil || string(data) != "int lib(){return 42;}\n" {
		t.Fatalf("edited file = %q %v %v", data, ok, err)
	}
	if _, ok, err := w.ReadLive("main.c"); !ok || !IsNotExist(err) {
		t.Fatalf("a file the tools deleted: ok=%v err=%v", ok, err)
	}
	for _, rel := range []string{"new.c", "lib.o", "generated/x.txt", "obj/link", "src", ".git/config", "../x", ""} {
		if _, ok, _ := w.ReadLive(rel); ok {
			t.Fatalf("%q was answered without a capture", rel)
		}
	}
	if got := e.Metrics().Captures; got != before {
		t.Fatalf("ReadLive scanned the directory (%d captures)", got-before)
	}

	// After a capture the two agree on every path, and the overlay holds what ReadLive said.
	if _, err := w.Capture(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, _ := w.ReadFile("src/lib.c"); string(data) != "int lib(){return 42;}\n" {
		t.Fatalf("overlay = %q", data)
	}
	mustNotExist(t, w, "main.c")
	if data, ok, err := w.ReadLive("new.c"); !ok || err != nil || string(data) != "int n;\n" {
		t.Fatalf("a captured file = %q %v %v", data, ok, err)
	}

	// A directory that was removed from outside is not read as "the tools deleted everything".
	if err := os.RemoveAll(ex.Dir); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := w.ReadLive("src/lib.c"); ok {
		t.Fatal("a vanished directory was read as deletions")
	}
}
