package workspace

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/integration"
)

// virtualRepo makes a repository with some history, uncommitted user work and an untracked file, and a Manager
// in the given mode that has taken its snapshot.
func virtualRepo(t *testing.T, mode Mode) (*Manager, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "user.email", "relo@test")
	run(t, dir, "git", "config", "user.name", "Relo")
	for name, body := range map[string]string{
		".gitignore": "*.o\nbuild/\n", "README.md": "one\ntwo\nthree\n", "src/a.go": "package a\n", "src/b.go": "package b\n",
		"docs/guide.md": "guide\n", "tool.sh": "#!/bin/sh\n",
	} {
		write(t, dir, name, body)
	}
	if err := os.Chmod(filepath.Join(dir, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-q", "-m", "init")
	write(t, dir, "src/a.go", "package a // the user's uncommitted edit\n")
	write(t, dir, "notes.txt", "untracked user file\n")
	m := New(dir)
	m.Configure(Config{Mode: mode})
	t.Cleanup(func() { _ = m.Close() })
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func worktreeCount(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(output(t, dir, "worktree", "list", "--porcelain"), "worktree ")
}

func sortedPaths(files []FileChange) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Kind[:1]+":"+f.Path)
	}
	sort.Strings(out)
	return out
}

// edit is what an agent's tools do inside its directory.
func edit(t *testing.T, dir string) {
	t.Helper()
	write(t, dir, "README.md", "one\nTWO\nthree\nfour\n")
	write(t, dir, "src/new/c.go", "package c\n")
	write(t, dir, "ignored.o", "OBJECT")
	write(t, dir, "build/out.bin", "BIN")
	write(t, dir, "data.bin", "\x00\x01\x02")
	write(t, dir, "tool2.sh", "#!/bin/sh\necho\n")
	if err := os.Chmod(filepath.Join(dir, "tool2.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "docs", "guide.md")); err != nil {
		t.Fatal(err)
	}
}

// Everything the integration engine reads about an agent's work must be identical whether the work lives in a
// worktree or in a virtual workspace.
func TestVirtualModeReportsTheSameChangesAsAWorktree(t *testing.T) {
	type observed struct {
		files   []string
		names   []string
		patches map[string]string
		whole   string
		base    map[string]string
	}
	observe := func(t *testing.T, mode Mode) observed {
		m, _ := virtualRepo(t, mode)
		tree, err := m.EnsureTree("agent-1", false)
		if err != nil {
			t.Fatal(err)
		}
		if tree.Virtual != (mode == ModeVirtual) {
			t.Fatalf("mode %s gave a tree with Virtual=%v", mode, tree.Virtual)
		}
		dir, err := m.Materialize("agent-1")
		if err != nil || dir != tree.Path {
			t.Fatalf("materialize = %q %v, want %q", dir, err, tree.Path)
		}
		edit(t, dir)
		files, err := m.ChangedFiles("agent-1")
		if err != nil {
			t.Fatal(err)
		}
		names, err := m.Changed("agent-1")
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		o := observed{files: sortedPaths(files), names: names, patches: map[string]string{}, base: map[string]string{}}
		for _, f := range files {
			p, err := m.PatchPath("agent-1", f.Path)
			if err != nil {
				t.Fatal(err)
			}
			o.patches[f.Path] = p
			if b, ok, err := m.BaseContent("agent-1", f.Path); err != nil {
				t.Fatal(err)
			} else if ok {
				o.base[f.Path] = string(b)
			}
		}
		if o.whole, err = m.Patch("agent-1"); err != nil {
			t.Fatal(err)
		}
		// Once the tools are done, the same answers come back with no directory at all.
		if err := m.Settle("agent-1"); err != nil {
			t.Fatal(err)
		}
		after, err := m.ChangedFiles("agent-1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(sortedPaths(after), ",") != strings.Join(o.files, ",") {
			t.Fatalf("%s: changes after settle = %v, before = %v", mode, sortedPaths(after), o.files)
		}
		return o
	}
	wt, vw := observe(t, ModeWorktree), observe(t, ModeVirtual)
	want := "a:data.bin,a:src/new/c.go,a:tool2.sh,d:docs/guide.md,m:README.md"
	for name, o := range map[string]observed{"worktree": wt, "virtual": vw} {
		if got := strings.Join(o.files, ","); got != want {
			t.Fatalf("%s changes = %s, want %s", name, got, want)
		}
	}
	if strings.Join(wt.names, ",") != strings.Join(vw.names, ",") {
		t.Fatalf("Changed differs: %v vs %v", wt.names, vw.names)
	}
	for path, p := range wt.patches {
		if vw.patches[path] != p {
			t.Fatalf("patch for %s differs\n worktree:\n%s\n virtual:\n%s", path, p, vw.patches[path])
		}
	}
	if wt.whole != vw.whole {
		t.Fatalf("whole patch differs\n worktree:\n%s\n virtual:\n%s", wt.whole, vw.whole)
	}
	for path, b := range wt.base {
		if vw.base[path] != b {
			t.Fatalf("base of %s differs: %q vs %q", path, b, vw.base[path])
		}
	}
	// The user's uncommitted edit is in the base both agents were cut from.
	if !strings.Contains(vw.base["README.md"], "three") {
		t.Fatalf("base = %+v", vw.base)
	}
}

func TestVirtualModeChecksOutNothingUntilToolsRun(t *testing.T) {
	m, dir := virtualRepo(t, ModeVirtual)
	before := output(t, dir, "status", "--porcelain", "--untracked-files=all", "--", ".", ":(exclude).relo")
	tree, err := m.EnsureTree("agent-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Virtual || tree.Path != filepath.Join(dir, ".relo", "worktrees", "agent-1") || tree.Branch != "relo/agent-1" {
		t.Fatalf("tree = %+v", tree)
	}
	if _, err := os.Stat(tree.Path); err == nil {
		t.Fatal("a virtual agent had a directory before any tool ran")
	}
	if worktreeCount(t, dir) != 1 {
		t.Fatalf("git worktrees = %d, want only the user's", worktreeCount(t, dir))
	}
	// Reading state needs no directory either.
	if files, err := m.ChangedFiles("agent-1"); err != nil || len(files) != 0 {
		t.Fatalf("a fresh agent has changes: %+v %v", files, err)
	}
	if content, ok, err := m.BaseContent("agent-1", "notes.txt"); err != nil || !ok || string(content) != "untracked user file\n" {
		t.Fatalf("the agent's base must hold the user's untracked file: %q %v %v", content, ok, err)
	}
	dirPath, err := m.Materialize("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dirPath, "src", "a.go")); string(got) != "package a // the user's uncommitted edit\n" {
		t.Fatalf("the directory does not hold the user's working tree: %q", got)
	}
	if worktreeCount(t, dir) != 2 {
		t.Fatalf("git worktrees = %d while tools run", worktreeCount(t, dir))
	}
	write(t, dirPath, "made.txt", "by a tool\n")
	if err := m.Settle("agent-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dirPath); err == nil {
		t.Fatal("the directory survived Settle")
	}
	if worktreeCount(t, dir) != 1 {
		t.Fatalf("worktree metadata left behind: %d", worktreeCount(t, dir))
	}
	if files, _ := m.ChangedFiles("agent-1"); len(files) != 1 || files[0].Path != "made.txt" {
		t.Fatalf("settled work = %+v", files)
	}
	if after := output(t, dir, "status", "--porcelain", "--untracked-files=all", "--", ".", ":(exclude).relo"); after != before {
		t.Fatalf("the user's checkout changed:\n%s\n--\n%s", before, after)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(got) != "untracked user file\n" {
		t.Fatalf("user file changed: %q", got)
	}
}

func TestWorktreeModeNeverOpensTheVirtualEngine(t *testing.T) {
	m, dir := virtualRepo(t, ModeWorktree)
	tree, err := m.EnsureTree("agent-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Virtual {
		t.Fatal("the default mode must keep worktrees")
	}
	if _, err := os.Stat(tree.Path); err != nil {
		t.Fatalf("no worktree: %v", err)
	}
	if out := output(t, dir, "for-each-ref", "refs/relo/vws"); out != "" {
		t.Fatalf("worktree mode wrote virtual refs:\n%s", out)
	}
	if _, opened := m.VirtualMetrics(); opened {
		t.Fatal("the virtual engine was opened in worktree mode")
	}
	if got := (Config{}).Normalized(); got.Mode != ModeWorktree || got.Materialization != "automatic" || got.VirtualReads {
		t.Fatalf("defaults = %+v", got)
	}
	if got := (Config{Mode: "nonsense", Materialization: "x"}).Normalized(); got.Mode != ModeWorktree || got.Materialization != "automatic" {
		t.Fatalf("unknown values must fall back to the safe ones: %+v", got)
	}
}

func TestVirtualModeFallsBackWhenTheRepositoryRewritesContent(t *testing.T) {
	for _, tc := range []struct {
		mode   Mode
		notice bool
	}{{ModeVirtual, true}, {ModeAuto, false}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			m, dir := virtualRepo(t, tc.mode)
			var mu sync.Mutex
			var notices []string
			m.SetNotifier(func(kind, msg string) { mu.Lock(); notices = append(notices, kind+": "+msg); mu.Unlock() })
			run(t, dir, "git", "config", "core.autocrlf", "true")
			tree, err := m.EnsureTree("agent-1", false)
			if err != nil {
				t.Fatal(err)
			}
			if tree.Virtual {
				t.Fatal("a repository that rewrites line endings must use a worktree")
			}
			if _, err := os.Stat(tree.Path); err != nil {
				t.Fatalf("the fallback worktree is missing: %v", err)
			}
			if _, err := m.EnsureTree("agent-2", false); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.notice && (len(notices) != 1 || !strings.Contains(notices[0], "autocrlf")) {
				t.Fatalf("notices = %v, want exactly one that names the reason", notices)
			}
			if !tc.notice && len(notices) != 0 {
				t.Fatalf("auto mode should fall back quietly: %v", notices)
			}
		})
	}
}

func TestAutoModeUsesVirtualWhereSupported(t *testing.T) {
	m, _ := virtualRepo(t, ModeAuto)
	tree, err := m.EnsureTree("agent-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Virtual {
		t.Fatal("auto should choose a virtual workspace in a plain repository")
	}
}

func TestVirtualStackRecutAndDiscardMatchWorktreeBehavior(t *testing.T) {
	m, dir := virtualRepo(t, ModeVirtual)
	author, _ := m.EnsureTree("author", false)
	ad, err := m.Materialize("author")
	if err != nil {
		t.Fatal(err)
	}
	write(t, ad, "src/a.go", "package a // the worker's\n")
	write(t, ad, "x.txt", "new file\n")
	var patches []string
	for _, rel := range []string{"src/a.go", "x.txt"} {
		p, err := m.PatchPath("author", rel)
		if err != nil || p == "" {
			t.Fatalf("patch %s: %q %v", rel, p, err)
		}
		patches = append(patches, p)
	}
	next, _ := m.EnsureTree("next", false)
	if _, err := os.Stat(next.Path); err == nil {
		t.Fatal("stacking should not need a directory")
	}
	oldBase := next.Base
	results, err := m.Stack("next", patches)
	if err != nil || len(results) != 2 || !results[0].Applied || !results[1].Applied {
		t.Fatalf("stack = %+v %v", results, err)
	}
	if now, _ := m.Tree("next"); now.Base == oldBase || now.Base == "" {
		t.Fatalf("base did not move: %q -> %q", oldBase, now.Base)
	}
	if files, err := m.ChangedFiles("next"); err != nil || len(files) != 0 {
		t.Fatalf("stacked changes were counted as its own: %+v %v", files, err)
	}
	if base, ok, _ := m.BaseContent("next", "src/a.go"); !ok || string(base) != "package a // the worker's\n" {
		t.Fatalf("base of a.go = %q", base)
	}
	nd, err := m.Materialize("next")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(nd, "x.txt")); string(got) != "new file\n" {
		t.Fatalf("stacked file in the directory = %q", got)
	}
	write(t, nd, "y.txt", "its own\n")
	if files, _ := m.ChangedFiles("next"); len(files) != 1 || files[0].Path != "y.txt" {
		t.Fatalf("changes = %+v, want only y.txt", files)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "src", "a.go")); string(got) != "package a // the user's uncommitted edit\n" {
		t.Fatalf("the user's file changed: %q", got)
	}
	// A patch that does not fit is skipped and reported; the one that fits lands.
	third, _ := m.EnsureTree("third", false)
	_ = third
	tdir, _ := m.Materialize("third")
	write(t, tdir, "x.txt", "someone else's\n")
	if err := m.Settle("third"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recut("third", ""); err == nil {
		t.Fatal("recut without a base must fail")
	}
	results, err = m.Stack("third", patches)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 patches did not apply") || results[1].Applied || !results[0].Applied {
		t.Fatalf("stack onto a conflicting overlay = %+v %v", results, err)
	}

	// Discard puts one path back to the base and leaves the rest of the agent's work alone.
	write(t, nd, "src/b.go", "package b // changed\n")
	write(t, nd, "z.txt", "added then discarded\n")
	if err := m.Settle("next"); err != nil {
		t.Fatal(err)
	}
	if err := m.Discard("next", "src/b.go"); err != nil {
		t.Fatal(err)
	}
	if err := m.DiscardMany("next", []string{"z.txt", "y.txt"}); err != nil {
		t.Fatal(err)
	}
	if files, _ := m.ChangedFiles("next"); len(files) != 0 {
		t.Fatalf("after discarding everything: %+v", files)
	}
	// Recut resets an unstarted agent to a fresh base.
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "-m", "accepted")
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recut("next", m.Base().Commit); err != nil {
		t.Fatal(err)
	}
	if now, _ := m.Tree("next"); now.Base != m.Base().Commit {
		t.Fatalf("recut base = %q, want %q", now.Base, m.Base().Commit)
	}
	_ = author
}

func TestVirtualWorkspacesSurviveARestartAndAreAdoptedInAnyMode(t *testing.T) {
	m1, dir := virtualRepo(t, ModeVirtual)
	if _, err := m1.EnsureTree("agent-1", false); err != nil {
		t.Fatal(err)
	}
	d, _ := m1.Materialize("agent-1")
	write(t, d, "kept.txt", "kept\n")
	prop, ok, err := m1.Propose("agent-1")
	if err != nil || !ok || prop.Revision == 0 || prop.Branch != "relo/agent-1" {
		t.Fatalf("propose = %+v %v %v", prop, ok, err)
	}
	// Relo stops here without settling, and starts again in the default mode.
	_ = m1.Close()
	m2 := New(dir)
	tree, err := m2.EnsureTree("agent-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !tree.Virtual {
		t.Fatalf("the existing virtual workspace was not adopted: %+v", tree)
	}
	files, err := m2.ChangedFiles("agent-1")
	if err != nil || len(files) != 1 || files[0].Path != "kept.txt" {
		t.Fatalf("after restart: %+v %v", files, err)
	}
	if _, err := os.Stat(d); err == nil {
		t.Fatal("the directory left by the interrupted run was not cleaned up")
	}
	if rep := m2.Recovered(); len(rep.Recovered) != 1 || len(rep.Interrupted) != 1 {
		t.Fatalf("recovery report = %+v", rep)
	}
	src, ok := m2.SourceFor("agent-1", tree.Path, prop.Revision)
	if !ok {
		t.Fatal("no source for the proposed revision")
	}
	if data, exists, err := src.Read("kept.txt"); err != nil || !exists || string(data) != "kept\n" {
		t.Fatalf("read through the source = %q %v %v", data, exists, err)
	}
	// Sweep keeps what a pending change needs and disposes the rest.
	if _, err := m2.EnsureTree("agent-2", false); err != nil {
		t.Fatal(err)
	}
	m3 := New(dir)
	t.Cleanup(func() { _ = m3.Close() })
	if removed := m3.Sweep(map[string]bool{"agent-1": true}); removed != 1 {
		t.Fatalf("swept %d, want only agent-2", removed)
	}
	if out := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/vws/agent-2"); out != "" {
		t.Fatalf("agent-2's refs survived the sweep:\n%s", out)
	}
	if out := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/vws/agent-1"); out == "" {
		t.Fatal("agent-1's refs were swept although a change still needs them")
	}
	if err := m3.Release("agent-1"); err != nil {
		t.Fatal(err)
	}
	if out := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/vws", "refs/heads/relo"); out != "" {
		t.Fatalf("release left refs behind:\n%s", out)
	}
}

// The source decides a change from the revision it was proposed at, and a retained blob is the proposed one.
func TestSourceForReadsTheProposedRevision(t *testing.T) {
	m, dir := virtualRepo(t, ModeVirtual)
	tree, _ := m.EnsureTree("agent-1", false)
	d, _ := m.Materialize("agent-1")
	write(t, d, "result.txt", "proposed\n")
	if err := os.Chmod(filepath.Join(d, "result.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	prop, _, err := m.Propose("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	// The agent keeps working after the proposal.
	write(t, d, "result.txt", "changed after the proposal\n")
	write(t, d, "later.txt", "later\n")
	if err := m.Settle("agent-1"); err != nil {
		t.Fatal(err)
	}
	src, ok := m.SourceFor("agent-1", tree.Path, prop.Revision)
	if !ok {
		t.Fatal("no source")
	}
	if data, exists, _ := src.Read("result.txt"); !exists || string(data) != "proposed\n" {
		t.Fatalf("the proposal reads %q", data)
	}
	if _, exists, _ := src.Read("later.txt"); exists {
		t.Fatal("work done after the proposal is visible in it")
	}
	if mode, ok := src.Mode("result.txt"); !ok || mode != 0o755 {
		t.Fatalf("mode = %v %v", mode, ok)
	}
	if err := src.Check("result.txt"); err != nil {
		t.Fatal(err)
	}
	if live, _ := m.SourceFor("agent-1", tree.Path, 0); live == nil {
		t.Fatal("no live source")
	} else if data, _, _ := live.Read("result.txt"); string(data) != "changed after the proposal\n" {
		t.Fatalf("the live overlay reads %q", data)
	}
	// An unknown revision is refused rather than answered from whatever the overlay holds now.
	if _, ok := m.SourceFor("agent-1", tree.Path, prop.Revision+50); ok {
		t.Fatal("a revision that was never proposed must not resolve")
	}
	// Rejecting keeps the proposed bytes recoverable.
	if err := m.KeepChangeBlobs([]BlobRef{{ChangeID: "chg-1", Worktree: tree.Path, Path: "result.txt", Revision: prop.Revision}, {ChangeID: "chg-2", Worktree: tree.Path, Path: "never-existed.txt", Revision: prop.Revision}}); err != nil {
		t.Fatal(err)
	}
	refs := strings.Fields(output(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes"))
	if len(refs) != 1 || !strings.HasSuffix(refs[0], "chg-1") {
		t.Fatalf("kept refs = %v", refs)
	}
	if got := output(t, dir, "cat-file", "blob", refs[0]); got != "proposed" {
		t.Fatalf("the kept blob is %q", got)
	}
	if err := m.KeepChangeBlobAt("chg-3", tree.Path, "result.txt", prop.Revision); err != nil {
		t.Fatal(err)
	}
	if refs := strings.Fields(output(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes")); len(refs) != 2 {
		t.Fatalf("kept refs = %v", refs)
	}
}

// The same merge engine accepts a virtual change and a worktree change identically, and a user's edit made while
// the agent worked is kept.
func TestMergeFromAnOverlayKeepsTheUsersConcurrentEdits(t *testing.T) {
	m, dir := virtualRepo(t, ModeVirtual)
	tree, _ := m.EnsureTree("agent-1", false)
	d, _ := m.Materialize("agent-1")
	base, hadBase, err := m.BaseContent("agent-1", "README.md")
	if err != nil || !hadBase {
		t.Fatal(err)
	}
	write(t, d, "README.md", "one\ntwo\nthree\nagent line\n")
	prop, _, err := m.Propose("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Settle("agent-1"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "README.md", "user line\none\ntwo\nthree\n") // the user edits at the top meanwhile
	src, ok := m.SourceFor("agent-1", tree.Path, prop.Revision)
	if !ok {
		t.Fatal("no source")
	}
	out, err := integration.MergeSource(dir, src, "README.md", base, hadBase, nil)
	if err != nil || out.Action != "merged" {
		t.Fatalf("merge = %+v %v", out, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(got) != "user line\none\ntwo\nthree\nagent line\n" {
		t.Fatalf("merged file = %q", got)
	}
	// The same file accepted from a worktree agent gives the same bytes.
	wm, wdir := virtualRepo(t, ModeWorktree)
	wt, _ := wm.EnsureTree("agent-1", false)
	write(t, wt.Path, "README.md", "one\ntwo\nthree\nagent line\n")
	write(t, wdir, "README.md", "user line\none\ntwo\nthree\n")
	if _, err := integration.Merge(wdir, wt.Path, "README.md", base, hadBase); err != nil {
		t.Fatal(err)
	}
	if a, b := read(t, dir, "README.md"), read(t, wdir, "README.md"); a != b {
		t.Fatalf("virtual %q, worktree %q", a, b)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSourceCheckRefusesPathsThroughLinks(t *testing.T) {
	m, _ := virtualRepo(t, ModeVirtual)
	tree, _ := m.EnsureTree("agent-1", false)
	d, _ := m.Materialize("agent-1")
	if err := os.Symlink("../../outside", filepath.Join(d, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Symlink("src", filepath.Join(d, "inner")); err != nil {
		t.Fatal(err)
	}
	write(t, d, "ok.txt", "fine\n")
	if err := m.Settle("agent-1"); err != nil {
		t.Fatal(err)
	}
	files, _ := m.ChangedFiles("agent-1")
	if got := strings.Join(sortedPaths(files), ","); got != "a:inner,a:ok.txt" {
		t.Fatalf("changes = %s: a link that leaves the workspace must never become a change", got)
	}
	src, _ := m.SourceFor("agent-1", tree.Path, 0)
	if err := src.Check("inner/a.go"); err == nil {
		t.Fatal("a path through a link in the agent's files must be refused")
	}
	if err := src.Check("ok.txt"); err != nil {
		t.Fatal(err)
	}
	if err := src.Check("not/there.txt"); err != nil {
		t.Fatalf("a missing path is not a link: %v", err)
	}
	if _, exists, err := src.Read("escape"); err != nil || exists {
		t.Fatalf("the rejected link should simply be absent: %v %v", exists, err)
	}
	// A link that stays inside is read as the file it points to, as a checkout would.
	if data, exists, err := src.Read("inner"); err == nil && exists {
		t.Fatalf("a link to a directory read as a file: %q", data)
	}
}

// A user's ignore path hides a change from the listing in either mode; it must not make the edit disappear, or
// changing the setting later would find nothing where a worktree still holds the file.
func TestUserIgnorePathsHideChangesWithoutDiscardingThem(t *testing.T) {
	for _, mode := range []Mode{ModeWorktree, ModeVirtual} {
		t.Run(string(mode), func(t *testing.T) {
			m, _ := virtualRepo(t, mode)
			m.SetIgnore([]string{"generated/**"})
			if _, err := m.EnsureTree("agent-1", false); err != nil {
				t.Fatal(err)
			}
			dir, err := m.Materialize("agent-1")
			if err != nil {
				t.Fatal(err)
			}
			write(t, dir, "generated/out.txt", "kept\n")
			write(t, dir, "src/new.go", "package n\n")
			files, err := m.ChangedFiles("agent-1")
			if err != nil || strings.Join(sortedPaths(files), ",") != "a:src/new.go" {
				t.Fatalf("listing = %v %v", sortedPaths(files), err)
			}
			if err := m.Settle("agent-1"); err != nil {
				t.Fatal(err)
			}
			m.SetIgnore(nil)
			files, err = m.ChangedFiles("agent-1")
			if err != nil || strings.Join(sortedPaths(files), ",") != "a:generated/out.txt,a:src/new.go" {
				t.Fatalf("after the setting changed, listing = %v %v", sortedPaths(files), err)
			}
		})
	}
}

// A link an agent's tool made that points outside the workspace is not imported, and the user is told once.
func TestRefusedToolFilesAreReportedOnce(t *testing.T) {
	m, _ := virtualRepo(t, ModeVirtual)
	var mu sync.Mutex
	var notes []string
	m.SetNotifier(func(kind, msg string) {
		mu.Lock()
		defer mu.Unlock()
		notes = append(notes, kind+": "+msg)
	})
	if _, err := m.EnsureTree("agent-1", false); err != nil {
		t.Fatal(err)
	}
	dir, err := m.Materialize("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	write(t, filepath.Dir(outside), filepath.Base(outside), "elsewhere\n")
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write(t, dir, "ok.txt", "fine\n")
	for i := 0; i < 3; i++ {
		if err := m.Sync("agent-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Settle("agent-1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], "link.txt") || !strings.HasPrefix(notes[0], "workspace.skipped") {
		t.Fatalf("notices = %q", notes)
	}
	files, err := m.ChangedFiles("agent-1")
	if err != nil || strings.Join(sortedPaths(files), ",") != "a:ok.txt" {
		t.Fatalf("changes = %v %v", sortedPaths(files), err)
	}
}
