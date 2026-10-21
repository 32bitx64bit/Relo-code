package vws

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// treeOfFiles asks real Git for the tree id of a set of files, using a scratch work tree and a scratch index.
func treeOfFiles(t *testing.T, repoDir string, files map[string]string, exec map[string]bool) string {
	t.Helper()
	work := t.TempDir()
	for name, content := range files {
		full := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if exec[name] {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	idx := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + idx, "GIT_DIR=" + filepath.Join(repoDir, ".git"), "GIT_WORK_TREE=" + work}
	gitEnvT(t, work, env, "add", "-A", "-f")
	return gitEnvT(t, work, env, "write-tree")
}

// conflicts predicts whether the path cannot be a file given the files that exist.
func modelConflicts(files map[string]string, path string) bool {
	for existing := range files {
		if existing == path {
			continue
		}
		if strings.HasPrefix(path, existing+"/") || strings.HasPrefix(existing, path+"/") {
			return true
		}
	}
	return false
}

// Native tree rewriting must produce exactly the tree Git would: same entries, same order, same ids. Names are
// chosen to exercise Git's directory-sorts-as-if-slash-terminated rule.
func TestNativeTreesMatchGitWriteTree(t *testing.T) {
	names := []string{"a", "a.b", "a-b", "a0", "a/b", "a/b/c", "a/c", "a.b/c", "a-b/c", "ab/c", "b", "B", "z/y/x/w", "z/y/x", "dir/.hidden", "dir/UPPER", "dir/under_score", "é.txt", "sp ace.txt", "x/y.z"}
	for seed := int64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			dir := newRepo(t, map[string]string{"seed.txt": "s\n"})
			e := openEngine(t, dir)
			w := newWS(t, e, "agent-1")
			rng := rand.New(rand.NewSource(seed))
			files := map[string]string{"seed.txt": "s\n"}
			exec := map[string]bool{}
			for step := 0; step < 70; step++ {
				name := names[rng.Intn(len(names))]
				switch rng.Intn(4) {
				case 0, 1:
					content := fmt.Sprintf("v%d-%d\n", seed, step)
					mode := uint32(0)
					if rng.Intn(5) == 0 {
						mode = 0o755
					}
					err := w.WriteFileMode(name, []byte(content), mode)
					want := modelConflicts(files, name)
					if want != (err != nil) {
						t.Fatalf("step %d write %s: model conflict=%v, engine err=%v", step, name, want, err)
					}
					if err == nil {
						files[name] = content
						if mode == 0o755 {
							exec[name] = true
						}
					}
				case 2:
					err := w.DeleteFile(name)
					_, exists := files[name]
					if exists != (err == nil) {
						t.Fatalf("step %d delete %s: exists=%v err=%v", step, name, exists, err)
					}
					if err == nil {
						delete(files, name)
						delete(exec, name)
					}
				case 3:
					// A batch that deletes and re-adds in one revision.
					if _, ok := files[name]; ok {
						content := fmt.Sprintf("b%d\n", step)
						if _, err := w.Apply([]Op{{Kind: "delete", Path: name}, {Kind: "write", Path: name, Data: []byte(content)}}, ApplyOptions{}); err != nil {
							t.Fatalf("step %d rewrite %s: %v", step, name, err)
						}
						files[name] = content
						delete(exec, name)
					}
				}
			}
			want := treeOfFiles(t, dir, files, exec)
			if got := w.Info().EffectiveTree; got != want {
				var have []string
				list, _ := w.Files()
				have = append(have, list...)
				var model []string
				for n := range files {
					model = append(model, n)
				}
				sort.Strings(model)
				t.Fatalf("tree mismatch\n engine %s %v\n git    %s %v", got, have, want, model)
			}
			for name, content := range files {
				if got := mustRead(t, w, name); got != content {
					t.Fatalf("%s = %q, want %q", name, got, content)
				}
			}
			// The objects the engine wrote are well formed: git can walk everything.
			gitT(t, dir, "fsck", "--no-dangling", "--strict")
		})
	}
}

// the changes git reports for an equivalent worktree, the way workspace.Manager.ChangedFiles reads them.
func worktreeDiff(t *testing.T, wt, base string) (string, string) {
	t.Helper()
	gitT(t, wt, "add", "-A")
	status := gitT(t, wt, "diff", "--cached", "--no-renames", "--name-status", base)
	num := gitT(t, wt, "diff", "--cached", "--no-renames", "--numstat", base)
	return status, num
}

// A virtual overlay produces the same logical diff, and the same patches, as an equivalent worktree.
func TestOverlayDiffMatchesAnEquivalentWorktree(t *testing.T) {
	dir := newRepo(t, map[string]string{
		"README.md": "one\ntwo\nthree\n", "src/a.go": "package a\n", "src/b.go": "package b\n", "run.sh*": "#!/bin/sh\n",
		"data.bin": "\x00\x01\x02\x03", "gone.txt": "bye\n",
	})
	base := gitT(t, dir, "rev-parse", "HEAD")
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, dir, "worktree", "add", "--detach", wt, base)

	edit := func(rel, content string, mode os.FileMode) {
		full := filepath.Join(wt, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteFileMode(rel, []byte(content), uint32(mode)); err != nil {
			t.Fatal(err)
		}
	}
	edit("README.md", "one\nTWO\nthree\nfour\n", 0o644)
	edit("src/new/c.go", "package c\n", 0o644)
	edit("tool.sh", "#!/bin/sh\necho\n", 0o755)
	edit("data.bin", "\x00\x01\x09\x03\x04", 0o644)
	if err := os.Remove(filepath.Join(wt, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteFile("gone.txt"); err != nil {
		t.Fatal(err)
	}
	wantStatus, wantNum := worktreeDiff(t, wt, base)

	diff, err := w.ReadDiff()
	if err != nil {
		t.Fatal(err)
	}
	var gotStatus, gotNum []string
	for _, c := range diff {
		letter := map[string]string{"added": "A", "modified": "M", "deleted": "D"}[c.Kind]
		gotStatus = append(gotStatus, letter+"\t"+c.Path)
		a, d := fmt.Sprint(c.Added), fmt.Sprint(c.Deleted)
		if c.Path == "data.bin" {
			a, d = "-", "-" // the numstat of a binary file; the engine reports 0, as Manager.ChangedFiles does
		}
		gotNum = append(gotNum, a+"\t"+d+"\t"+c.Path)
	}
	sort.Strings(gotStatus)
	sort.Strings(gotNum)
	ws := strings.Split(wantStatus, "\n")
	sort.Strings(ws)
	wn := strings.Split(wantNum, "\n")
	sort.Strings(wn)
	if strings.Join(gotStatus, "\n") != strings.Join(ws, "\n") {
		t.Fatalf("name-status differs\n engine:\n%s\n worktree:\n%s", strings.Join(gotStatus, "\n"), strings.Join(ws, "\n"))
	}
	if strings.Join(gotNum, "\n") != strings.Join(wn, "\n") {
		t.Fatalf("numstat differs\n engine:\n%s\n worktree:\n%s", strings.Join(gotNum, "\n"), strings.Join(wn, "\n"))
	}
	for _, c := range diff {
		got, err := w.PatchPath(c.Path)
		if err != nil {
			t.Fatal(err)
		}
		want := gitT(t, wt, "diff", "--cached", "--binary", base, "--", ":(literal)"+c.Path)
		if strings.TrimSpace(got) != want {
			t.Fatalf("patch for %s differs\n engine:\n%s\n worktree:\n%s", c.Path, got, want)
		}
	}
	// And the tree is the one the worktree would stage.
	if got, want := w.Info().EffectiveTree, gitT(t, wt, "write-tree"); got != want {
		t.Fatalf("effective tree %s, worktree tree %s", got, want)
	}
}

func TestProposalsCarryBlobIdentities(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	w := newWS(t, e, "agent-1")
	before, _ := w.Resolve("README.md")
	mustWrite(t, w, "README.md", "new\n")
	mustWrite(t, w, "added.txt", "a\n")
	if err := w.DeleteFile("docs/guide.md"); err != nil {
		t.Fatal(err)
	}
	snap, err := w.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, w, "after-snapshot.txt", "later\n") // a proposal describes the frozen revision only
	props, err := w.Proposals(snap)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ChangeProposal{}
	for _, p := range props {
		by[p.Path] = p
		if p.AgentID != "agent-1" || p.BaseCommit != snap.BaseCommit || p.Commit != snap.Commit || p.Branch != "relo/agent-1" {
			t.Fatalf("proposal identity = %+v", p)
		}
	}
	if _, ok := by["after-snapshot.txt"]; ok || len(props) != 3 {
		t.Fatalf("proposals = %+v", props)
	}
	readme := by["README.md"]
	if readme.Op != "modified" || readme.BeforeBlob != before.BlobOID || readme.AfterBlob == "" || readme.AfterBlob == readme.BeforeBlob {
		t.Fatalf("README proposal = %+v", readme)
	}
	if p := by["added.txt"]; p.Op != "added" || p.BeforeBlob != "" || p.AfterBlob == "" {
		t.Fatalf("added proposal = %+v", p)
	}
	if p := by["docs/guide.md"]; p.Op != "deleted" || p.BeforeBlob == "" || p.AfterBlob != "" {
		t.Fatalf("deleted proposal = %+v", p)
	}
}
