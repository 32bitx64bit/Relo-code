package integration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
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

func TestMergeKeepsLocalEditsAndAppliesAgentEdits(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	base := "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"
	write(t, root, "f.txt", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n") // user edited line 1
	write(t, wt, "f.txt", "one\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n")   // agent edited line 7
	out, err := Merge(root, wt, "f.txt", []byte(base), true)
	if err != nil || out.Action != "merged" {
		t.Fatalf("merge: %+v %v", out, err)
	}
	if got := read(t, root, "f.txt"); got != "ONE\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n" {
		t.Fatalf("both edits should survive: %q", got)
	}
}

// A write must not replace bytes the user changed after the merge was computed. The injected edit is either
// merged on a retry or the write fails closed, and it is never lost.
func TestMergeFaultInjectionRetriesAndKeepsInjectedBytes(t *testing.T) {
	defer func() { beforeCommit = nil }()
	for i := 0; i < 200; i++ {
		root, wt := t.TempDir(), t.TempDir()
		write(t, root, "f.txt", "one\ntwo\nthree\n")
		write(t, wt, "f.txt", "one\ntwo\nTHREE\n")
		injected := "ONE\ntwo\nthree\n"
		beforeCommit = func() {
			if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(injected), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, err := Merge(root, wt, "f.txt", []byte("one\ntwo\nthree\n"), true)
		got := read(t, root, "f.txt")
		if err != nil {
			var changed WorkspaceChangedError
			if !errors.As(err, &changed) {
				t.Fatalf("iteration %d: error = %v", i, err)
			}
			if got != injected {
				t.Fatalf("iteration %d: injected bytes lost: %q", i, got)
			}
			continue
		}
		if out.Action != "merged" || got != "ONE\ntwo\nTHREE\n" {
			t.Fatalf("iteration %d: merge = %+v, file = %q", i, out, got)
		}
	}
}

// An update keeps the workspace file's permission bits, not the agent file's.
func TestMergePreservesWorkspaceMode(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	write(t, root, "run.sh", "old\n")
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, wt, "run.sh", "new\n")
	if err := os.Chmod(filepath.Join(wt, "run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Merge(root, wt, "run.sh", []byte("old\n"), true); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", st.Mode().Perm())
	}
}

func TestMergeReportsOverlapWithoutTouchingTheFile(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	write(t, root, "f.txt", "user\n")
	write(t, wt, "f.txt", "agent\n")
	_, err := Merge(root, wt, "f.txt", []byte("orig\n"), true)
	var ce ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want conflict, got %v", err)
	}
	if got := read(t, root, "f.txt"); got != "user\n" {
		t.Fatalf("workspace file was modified: %q", got)
	}
}

func TestMergeCreateDeleteAndFastForward(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	write(t, wt, "new/dir/a.txt", "fresh\n")
	if out, err := Merge(root, wt, "new/dir/a.txt", nil, false); err != nil || out.Action != "created" || read(t, root, "new/dir/a.txt") != "fresh\n" {
		t.Fatalf("create: %+v %v", out, err)
	}
	write(t, root, "b.txt", "same\n")
	if out, err := Merge(root, wt, "b.txt", []byte("same\n"), true); err != nil || out.Action != "deleted" {
		t.Fatalf("delete: %+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("b.txt should be gone")
	}
	write(t, root, "c.txt", "old\n")
	write(t, wt, "c.txt", "newer\n")
	if out, err := Merge(root, wt, "c.txt", []byte("old\n"), true); err != nil || out.Action != "updated" || read(t, root, "c.txt") != "newer\n" {
		t.Fatalf("fast-forward: %+v %v", out, err)
	}
}

func TestMergeRefusesUnsafePaths(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	for _, rel := range []string{"../escape.txt", "/etc/passwd", ".git/config", ".relo/x"} {
		if _, err := Merge(root, wt, rel, nil, false); err == nil {
			t.Errorf("accepted unsafe path %q", rel)
		}
	}
}

func TestCheckMatchesMergeWithoutWriting(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	write(t, root, "f.txt", "ONE\ntwo\n")
	write(t, wt, "f.txt", "one\ntwo\n")
	write(t, wt, "new.txt", "fresh\n")
	if reason, err := Check(root, wt, "new.txt", nil, false); err != nil || reason != "" {
		t.Fatalf("clean add: reason=%q err=%v", reason, err)
	}
	if reason, err := Check(root, wt, "f.txt", []byte("one\ntwo\n"), true); err != nil || reason != "" {
		t.Fatalf("clean update: reason=%q err=%v", reason, err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("Check wrote new.txt: %v", err)
	}
	if got := read(t, root, "f.txt"); got != "ONE\ntwo\n" {
		t.Fatalf("Check changed f.txt: %q", got)
	}
	write(t, root, "f.txt", "ONE\ntwo\n")
	write(t, wt, "f.txt", "agent\ntwo\n")
	if reason, err := Check(root, wt, "f.txt", []byte("one\ntwo\n"), true); err != nil || reason != "your edits and the agent's edits overlap" {
		t.Fatalf("overlap: reason=%q err=%v", reason, err)
	}
	if reason, err := Check(root, wt, ".git/config", nil, false); err != nil || reason != "protected metadata path" {
		t.Fatalf("protected: reason=%q err=%v", reason, err)
	}
}

// Every refusal must say what kind it is, so a stored conflict can be classified and shown.
func TestAnalyzeReportsKindForEveryRefusal(t *testing.T) {
	base := []byte("one\ntwo\nthree\n")
	cases := []struct {
		name    string
		root    func(t *testing.T, dir string)
		wt      func(t *testing.T, dir string)
		hadBase bool
		want    string
	}{
		{"overlap", func(t *testing.T, dir string) { write(t, dir, "f.txt", "user\n") },
			func(t *testing.T, dir string) { write(t, dir, "f.txt", "agent\n") }, true, KindOverlap},
		{"modify_delete", func(t *testing.T, dir string) { write(t, dir, "f.txt", "user changed\n") },
			func(t *testing.T, dir string) {}, true, KindModifyDelete},
		{"delete_modify", func(t *testing.T, dir string) {},
			func(t *testing.T, dir string) { write(t, dir, "f.txt", "agent changed\n") }, true, KindDeleteModify},
		{"add_add", func(t *testing.T, dir string) { write(t, dir, "f.txt", "mine\n") },
			func(t *testing.T, dir string) { write(t, dir, "f.txt", "agent\n") }, false, KindAddAdd},
		{"binary", func(t *testing.T, dir string) { write(t, dir, "f.txt", "user\x00\n") },
			func(t *testing.T, dir string) { write(t, dir, "f.txt", "agent\x00\n") }, true, KindBinary},
		{"protected", func(t *testing.T, dir string) {},
			func(t *testing.T, dir string) { write(t, dir, ".git/config", "x\n") }, false, KindProtected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, wt := t.TempDir(), t.TempDir()
			tc.root(t, root)
			tc.wt(t, wt)
			a, err := Analyze(root, wt, "f.txt", base, tc.hadBase)
			if tc.name == "protected" {
				a, err = Analyze(root, wt, ".git/config", nil, tc.hadBase)
			}
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if a.Kind != tc.want {
				t.Fatalf("%s: kind = %q, want %q (reason %q)", tc.name, a.Kind, tc.want, a.Reason)
			}
			if a.Reason == "" {
				t.Fatalf("%s: refusal has no reason", tc.name)
			}
		})
	}
}

func TestAnalyzeSymlinkRefusalHasAKind(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(wt, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	a, err := Analyze(root, wt, "link.txt", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindSymlink {
		t.Fatalf("kind = %q, want %q", a.Kind, KindSymlink)
	}
}

// Two changed regions, one of which conflicts: the analysis must locate both and mark only the conflicting one.
func TestAnalyzeHunkCounts(t *testing.T) {
	root, wt := t.TempDir(), t.TempDir()
	base := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	write(t, root, "f.txt", "1\nTWO\n3\n4\n5\n6\n7\n8\n9\n10\n")
	write(t, wt, "f.txt", "1\nagent\n3\n4\n5\n6\n7\nEIGHT\n9\n10\n")
	a, err := Analyze(root, wt, "f.txt", []byte(base), true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindOverlap {
		t.Fatalf("kind = %q, want overlap", a.Kind)
	}
	if len(a.Hunks) != 2 {
		t.Fatalf("hunks = %+v, want 2", a.Hunks)
	}
	conflicting := 0
	for _, h := range a.Hunks {
		if h.Conflicting {
			conflicting++
		}
	}
	if conflicting != 1 {
		t.Fatalf("conflicting hunks = %d in %+v, want 1", conflicting, a.Hunks)
	}
	if got := read(t, root, "f.txt"); got != "1\nTWO\n3\n4\n5\n6\n7\n8\n9\n10\n" {
		t.Fatalf("Analyze wrote the workspace file: %q", got)
	}
}
