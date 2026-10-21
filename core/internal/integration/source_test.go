package integration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// memSource holds the agent's files in memory, the way a virtual overlay holds them in Git objects.
type memSource struct {
	files map[string]string
	exec  map[string]bool
	deny  map[string]bool // paths Check refuses, standing in for a symlink that leaves the agent's files
}

func (m memSource) Check(rel string) error {
	if m.deny[rel] {
		return fmt.Errorf("patch path escapes workspace through symlink")
	}
	return nil
}

func (m memSource) Read(rel string) ([]byte, bool, error) {
	s, ok := m.files[rel]
	return []byte(s), ok, nil
}

func (m memSource) Mode(rel string) (os.FileMode, bool) {
	if _, ok := m.files[rel]; !ok {
		return 0, false
	}
	if m.exec[rel] {
		return 0o755, true
	}
	return 0o644, true
}

// asDir writes the same files to a directory so the two sources can be compared on identical input.
func (m memSource) asDir(t *testing.T) string {
	dir := t.TempDir()
	for rel, body := range m.files {
		write(t, dir, rel, body)
		if m.exec[rel] {
			if err := os.Chmod(filepath.Join(dir, rel), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// The merge engine must decide identically whether the agent's side is a directory or an overlay.
func TestSourcesDecideIdentically(t *testing.T) {
	lines := "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"
	cases := []struct {
		name   string
		ours   map[string]string // the workspace now
		theirs map[string]string // the agent's files
		base   string
		had    bool
		path   string
		kind   string
		action string
	}{
		{"clean merge", map[string]string{"f.txt": "ONE\n" + lines[4:]}, map[string]string{"f.txt": lines[:len(lines)-6] + "SEVEN\n"}, lines, true, "f.txt", "", "merged"},
		{"workspace untouched", map[string]string{"f.txt": lines}, map[string]string{"f.txt": "new\n"}, lines, true, "f.txt", "", "updated"},
		{"agent created", map[string]string{}, map[string]string{"new.txt": "hello\n"}, "", false, "new.txt", "", "created"},
		{"agent deleted", map[string]string{"f.txt": lines}, map[string]string{}, lines, true, "f.txt", "", "deleted"},
		{"overlap", map[string]string{"f.txt": "A\n" + lines[4:]}, map[string]string{"f.txt": "B\n" + lines[4:]}, lines, true, "f.txt", KindOverlap, ""},
		{"both created", map[string]string{"x.txt": "mine\n"}, map[string]string{"x.txt": "theirs\n"}, "", false, "x.txt", KindAddAdd, ""},
		{"modify delete", map[string]string{"f.txt": "changed\n"}, map[string]string{}, lines, true, "f.txt", KindModifyDelete, ""},
		{"binary", map[string]string{"b.bin": "a\x00b"}, map[string]string{"b.bin": "a\x00c"}, "a\x00z", true, "b.bin", KindBinary, ""},
		{"protected", map[string]string{}, map[string]string{".git/config": "x"}, "", false, ".git/config", KindProtected, ""},
		{"symlink escape", map[string]string{}, map[string]string{"link/f.txt": "x"}, "", false, "link/f.txt", KindSymlink, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memSource{files: tc.theirs, deny: map[string]bool{}}
			if tc.kind == KindSymlink {
				mem.deny["link/f.txt"] = true
			}
			rootA, rootB := t.TempDir(), t.TempDir()
			for rel, body := range tc.ours {
				write(t, rootA, rel, body)
				write(t, rootB, rel, body)
			}
			viaMem, err := AnalyzeSource(rootA, mem, tc.path, []byte(tc.base), tc.had)
			if err != nil {
				t.Fatal(err)
			}
			var viaDir Analysis
			if tc.kind == KindSymlink {
				// A real escaping link in a directory source reaches the same conclusion by the same rule.
				dir := t.TempDir()
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
					t.Skip("symlinks unavailable")
				}
				viaDir, err = Analyze(rootB, dir, tc.path, []byte(tc.base), tc.had)
			} else {
				viaDir, err = Analyze(rootB, mem.asDir(t), tc.path, []byte(tc.base), tc.had)
			}
			if err != nil {
				t.Fatal(err)
			}
			if viaMem.Kind != tc.kind || (tc.action != "" && viaMem.Action != tc.action) {
				t.Fatalf("overlay analysis = %+v, want kind %q action %q", viaMem, tc.kind, tc.action)
			}
			if viaMem.Kind != viaDir.Kind || viaMem.Action != viaDir.Action || viaMem.Fingerprint != viaDir.Fingerprint && tc.kind != KindSymlink {
				t.Fatalf("overlay %+v, directory %+v", viaMem, viaDir)
			}
			if !reflect.DeepEqual(viaMem.Hunks, viaDir.Hunks) || string(viaMem.Merged) != string(viaDir.Merged) {
				t.Fatalf("overlay hunks/merged differ from the directory's\n overlay %+v\n dir     %+v", viaMem, viaDir)
			}
		})
	}
}

func TestMergeSourceWritesLikeMerge(t *testing.T) {
	base := "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"
	root := t.TempDir()
	write(t, root, "f.txt", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n")
	src := memSource{files: map[string]string{"f.txt": "one\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n", "tool.sh": "#!/bin/sh\n"}, exec: map[string]bool{"tool.sh": true}}
	var journaled []string
	hook := func(path string, pre []byte, preExists bool, post []byte, postExists bool) error {
		journaled = append(journaled, path+":"+string(pre))
		return nil
	}
	out, err := MergeSource(root, src, "f.txt", []byte(base), true, hook)
	if err != nil || out.Action != "merged" {
		t.Fatalf("merge: %+v %v", out, err)
	}
	if got := read(t, root, "f.txt"); got != "ONE\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n" {
		t.Fatalf("both edits should survive: %q", got)
	}
	if len(journaled) != 1 || !strings.HasPrefix(journaled[0], "f.txt:ONE") {
		t.Fatalf("the journal hook did not see the pre-image: %v", journaled)
	}
	// A new file takes the mode the overlay recorded, so an executable agent file stays executable.
	if out, err := TakeSource(root, src, "tool.sh", nil); err != nil || out.Action != "created" {
		t.Fatalf("take: %+v %v", out, err)
	}
	if st, err := os.Stat(filepath.Join(root, "tool.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v %v", st, err)
	}
	// A conflict reports and writes nothing.
	write(t, root, "g.txt", "A\n"+base[4:])
	conflicting := memSource{files: map[string]string{"g.txt": "B\n" + base[4:]}}
	_, err = MergeSource(root, conflicting, "g.txt", []byte(base), true, nil)
	var ce ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if got := read(t, root, "g.txt"); got != "A\n"+base[4:] {
		t.Fatalf("a refused merge changed the workspace: %q", got)
	}
}

func TestSourceCheckRefusesTake(t *testing.T) {
	root := t.TempDir()
	src := memSource{files: map[string]string{"x": "x"}, deny: map[string]bool{"x": true}}
	if _, err := TakeSource(root, src, "x", nil); err == nil {
		t.Fatal("a path the source refuses must not be taken")
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
		t.Fatal("the refused file was written")
	}
}
