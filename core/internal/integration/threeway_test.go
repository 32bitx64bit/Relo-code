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
