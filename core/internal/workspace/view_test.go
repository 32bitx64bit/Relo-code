package workspace

import (
	"errors"
	"strings"
	"testing"
)

// Both adapters must behave identically for everything a consumer can ask of a view.
func TestWorkspaceViewConformance(t *testing.T) {
	for _, mode := range []Mode{ModeWorktree, ModeVirtual} {
		t.Run(string(mode), func(t *testing.T) {
			m, _ := virtualRepo(t, mode)
			if _, err := m.EnsureTree("agent-1", false); err != nil {
				t.Fatal(err)
			}
			v, err := m.View("agent-1")
			if err != nil {
				t.Fatal(err)
			}
			if v.AgentID() != "agent-1" || v.Kind() != string(mode) {
				t.Fatalf("view = %s/%s", v.AgentID(), v.Kind())
			}

			// Reads see the user's working tree, as it was when the agent was cut.
			if got, err := v.ReadFile("src/a.go"); err != nil || string(got) != "package a // the user's uncommitted edit\n" {
				t.Fatalf("read = %q %v", got, err)
			}
			if got, err := v.ReadFile("notes.txt"); err != nil || string(got) != "untracked user file\n" {
				t.Fatalf("untracked read = %q %v", got, err)
			}
			if _, err := v.ReadFile("nope.txt"); !IsNotExist(err) {
				t.Fatalf("missing file: %v", err)
			}
			if changes, err := v.Changes(); err != nil || len(changes) != 0 {
				t.Fatalf("a fresh view has changes: %+v %v", changes, err)
			}
			before, err := v.Identity()
			if err != nil || before.Base == "" || before.Kind != string(mode) {
				t.Fatalf("identity = %+v %v", before, err)
			}

			// Writes, nested creation, executable bit, deletion.
			if err := v.WriteFile("src/a.go", []byte("package a // agent\n"), 0); err != nil {
				t.Fatal(err)
			}
			if err := v.WriteFile("deep/er/file.txt", []byte("deep\n"), 0); err != nil {
				t.Fatal(err)
			}
			if err := v.WriteFile("run.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := v.DeleteFile("docs/guide.md"); err != nil {
				t.Fatal(err)
			}
			if _, err := v.ReadFile("docs/guide.md"); !IsNotExist(err) {
				t.Fatalf("a deleted file came back: %v", err)
			}
			if err := v.DeleteFile("docs/guide.md"); err == nil {
				t.Fatal("deleting a missing file should fail")
			}
			if fi, err := v.Stat("run.sh"); err != nil || fi.Kind != "file" || !fi.Exec || fi.Size != int64(len("#!/bin/sh\n")) {
				t.Fatalf("stat = %+v %v", fi, err)
			}
			if fi, err := v.Stat("src/a.go"); err != nil || fi.Exec {
				t.Fatalf("an existing file's mode must be kept: %+v %v", fi, err)
			}
			if fi, err := v.Stat("deep/er"); err != nil || fi.Kind != "dir" {
				t.Fatalf("stat dir = %+v %v", fi, err)
			}
			if fi, err := v.Stat("tool.sh"); err != nil || !fi.Exec {
				t.Fatalf("the base's executable bit was lost: %+v %v", fi, err)
			}
			entries, err := v.ListDir("")
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name+":"+e.Kind)
			}
			if got := strings.Join(names, ","); got != ".gitignore:file,README.md:file,deep:dir,notes.txt:file,run.sh:file,src:dir,tool.sh:file" {
				t.Fatalf("listing = %s", got)
			}
			sub, err := v.ListDir("deep")
			if err != nil || len(sub) != 1 || sub[0].Name != "er" || sub[0].Kind != "dir" {
				t.Fatalf("sub listing = %+v %v", sub, err)
			}

			// The diff is what Git reports, with the base it was cut from.
			changes, err := v.Changes()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(sortedPaths(changes), ","); got != "a:deep/er/file.txt,a:run.sh,d:docs/guide.md,m:src/a.go" {
				t.Fatalf("changes = %s", got)
			}
			patch, err := v.Patch("src/a.go")
			if err != nil || !strings.Contains(patch, "-package a // the user's uncommitted edit") || !strings.Contains(patch, "+package a // agent") {
				t.Fatalf("patch = %q %v", patch, err)
			}
			whole, err := v.Patch("")
			if err != nil || !strings.Contains(whole, "deep/er/file.txt") || !strings.Contains(whole, "docs/guide.md") {
				t.Fatalf("whole patch = %q %v", whole, err)
			}
			if base, ok, err := v.BaseFile("src/a.go"); err != nil || !ok || string(base) != "package a // the user's uncommitted edit\n" {
				t.Fatalf("base = %q %v %v", base, ok, err)
			}
			if _, ok, err := v.BaseFile("run.sh"); err != nil || ok {
				t.Fatalf("a file the agent created has no base: %v %v", ok, err)
			}

			// Paths that would leave the workspace or touch Git or Relo's data are refused by both.
			for _, bad := range []string{".git/config", "../escape.txt", "/etc/passwd", ".relo/state", "a/../../x", ""} {
				if err := v.WriteFile(bad, []byte("x"), 0); err == nil {
					t.Fatalf("write to %q was accepted", bad)
				}
				if _, err := v.ReadFile(bad); err == nil {
					t.Fatalf("read of %q was accepted", bad)
				}
			}

			// The identity moved, and the tree it names is the same one Git would build from these files.
			after, err := v.Identity()
			if err != nil || after.Tree == "" || after.Tree == before.Tree || after.Base != before.Base {
				t.Fatalf("identity after writes = %+v (before %+v) %v", after, before, err)
			}
			if mode == ModeVirtual && after.Revision <= before.Revision {
				t.Fatalf("revision did not advance: %d -> %d", before.Revision, after.Revision)
			}
			if again, _ := v.Identity(); again != after {
				t.Fatalf("identity changed without a write: %+v vs %+v", again, after)
			}

			// The integration engine sees the same files through either source.
			src := v.Source()
			if data, exists, err := src.Read("deep/er/file.txt"); err != nil || !exists || string(data) != "deep\n" {
				t.Fatalf("source read = %q %v %v", data, exists, err)
			}
			if _, exists, _ := src.Read("docs/guide.md"); exists {
				t.Fatal("the source shows a deleted file")
			}
			if mode, ok := src.Mode("run.sh"); !ok || mode&0o100 == 0 {
				t.Fatalf("source mode = %v %v", mode, ok)
			}
		})
	}
}

// Identical edits give identical trees, so a worktree agent and a virtual agent are interchangeable witnesses of
// the same state.
func TestViewsOfIdenticalEditsHaveTheSameTree(t *testing.T) {
	var trees []string
	for _, mode := range []Mode{ModeWorktree, ModeVirtual} {
		m, _ := virtualRepo(t, mode)
		if _, err := m.EnsureTree("agent-1", false); err != nil {
			t.Fatal(err)
		}
		v, _ := m.View("agent-1")
		for _, w := range []struct{ path, body string }{{"src/a.go", "package a // agent\n"}, {"new/dir/f.txt", "f\n"}, {"README.md", "rewritten\n"}} {
			if err := v.WriteFile(w.path, []byte(w.body), 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := v.DeleteFile("docs/guide.md"); err != nil {
			t.Fatal(err)
		}
		id, err := v.Identity()
		if err != nil {
			t.Fatal(err)
		}
		trees = append(trees, id.Tree)
	}
	if trees[0] == "" || trees[0] != trees[1] {
		t.Fatalf("worktree tree %q, virtual tree %q", trees[0], trees[1])
	}
}

func TestViewOfAnAgentWithoutAnIsolatedWorkspace(t *testing.T) {
	m, _ := virtualRepo(t, ModeVirtual)
	if _, err := m.View("nobody"); !errors.Is(err, ErrNoView) {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, err := m.EnsureTree("reader", true); err != nil { // read-only agents read the user's folder
		t.Fatal(err)
	}
	if _, err := m.View("reader"); !errors.Is(err, ErrNoView) {
		t.Fatalf("a shared-folder agent must not get a writable view: %v", err)
	}
}

// Tools write behind the virtual view's back; the view still shows what they wrote.
func TestVirtualViewShowsWhatToolsWrote(t *testing.T) {
	m, _ := virtualRepo(t, ModeVirtual)
	if _, err := m.EnsureTree("agent-1", false); err != nil {
		t.Fatal(err)
	}
	dir, err := m.Materialize("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "compiled.txt", "output of a compiler\n")
	v, _ := m.View("agent-1")
	if got, err := v.ReadFile("compiled.txt"); err != nil || string(got) != "output of a compiler\n" {
		t.Fatalf("read = %q %v", got, err)
	}
	if err := v.WriteFile("direct.txt", []byte("x"), 0); err == nil {
		t.Fatal("writing the overlay while a directory is open would split the truth in two")
	}
	if changes, _ := v.Changes(); len(changes) != 1 || changes[0].Path != "compiled.txt" {
		t.Fatalf("changes = %+v", changes)
	}
}
