package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectConflicts(t *testing.T) {
	c := Detect(map[string][]string{
		"a": {"internal/auth/session.go", "README.md"},
		"b": {"internal/auth/session.go", "ui/app.tsx"},
	})
	if len(c) != 1 || c[0].Path != "internal/auth/session.go" {
		t.Fatalf("%+v", c)
	}
}

func TestOverlap(t *testing.T) {
	got := Overlap([]string{"a", "b"}, []string{"b", "c"})
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("%v", got)
	}
}

func TestApplyCleanAndNewFile(t *testing.T) {
	root := repo(t)
	patch := "diff --git a/new.txt b/new.txt\nnew file mode 100644\nindex 0000000..3e75765\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+new file\n"
	res, err := Apply(root, patch, nil)
	if err != nil || len(res.Applied) != 1 {
		t.Fatalf("apply: result=%+v err=%v", res, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "new.txt")); err != nil || string(data) != "new file\n" {
		t.Fatalf("new file not applied: %q %v", data, err)
	}
}

func TestApplyRejectsTraversalAndProtectedPaths(t *testing.T) {
	root := repo(t)
	for _, path := range []string{"../escape", ".relo/db", ".git/config", "/tmp/absolute", "link/outside.txt"} {
		if path == "link/outside.txt" {
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
		}
		patch := "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1 +1 @@\n-old\n+new\n"
		res, err := Apply(root, patch, nil)
		var conflictErr ConflictError
		if !errors.As(err, &conflictErr) || res.Conflict == nil {
			t.Errorf("path %q not rejected with structured conflict: %+v %v", path, res, err)
		}
	}
}

func TestPatchFilesParsesQuotedPathsAndDeletes(t *testing.T) {
	got, err := patchFiles("diff --git \"a/name with space.txt\" \"b/name with space.txt\"\n--- \"a/name with space.txt\"\n+++ \"b/name with space.txt\"\n")
	if err != nil || len(got) != 1 || got[0] != "name with space.txt" {
		t.Fatalf("quoted paths: %v %v", got, err)
	}
	got, err = patchFiles("diff --git a/old.txt b/old.txt\n--- a/old.txt\n+++ /dev/null\n")
	if err != nil || len(got) != 1 || got[0] != "old.txt" {
		t.Fatalf("delete paths: %v %v", got, err)
	}
}

func TestApplyRejectsDirtyAndSnapshotProtectedFile(t *testing.T) {
	root := repo(t)
	patch := modifyPatch("tracked.txt", "base", "worker")
	res, err := Apply(root, patch, []string{"./tracked.txt"})
	if err == nil || res.Conflict == nil || len(res.Blocked) != 1 {
		t.Fatalf("snapshot conflict missing: %+v %v", res, err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Apply(root, patch, nil)
	if err == nil || res.Conflict == nil {
		t.Fatalf("current dirty conflict missing: %+v %v", res, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "tracked.txt")); string(data) != "user\n" {
		t.Fatalf("user change overwritten: %q", data)
	}
}

func TestApplyRejectsPatchWithLaterFailureWithoutPartialWrite(t *testing.T) {
	root := repo(t)
	patch := modifyPatch("tracked.txt", "base", "changed") + "\ndiff --git a/missing.txt b/missing.txt\n--- a/missing.txt\n+++ b/missing.txt\n@@ -1 +1 @@\n-old\n+new\n"
	res, err := Apply(root, patch, nil)
	if err == nil || res.Conflict == nil {
		t.Fatalf("expected check conflict: %+v %v", res, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "tracked.txt")); string(data) != "base\n" {
		t.Fatalf("failed patch partially applied: %q", data)
	}
}

func modifyPatch(path, old, next string) string {
	return "diff --git a/" + path + " b/" + path + "\nindex 9d3f98b..0a1b2c3 100644\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1 +1 @@\n-" + old + "\n+" + next + "\n"
}

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	command(t, root, "git", "init")
	command(t, root, "git", "config", "user.email", "relo@test")
	command(t, root, "git", "config", "user.name", "Relo")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command(t, root, "git", "add", ".")
	command(t, root, "git", "commit", "-m", "initial")
	return root
}

func command(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, strings.TrimSpace(string(out)))
	}
}
