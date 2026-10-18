package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Some repositories track .relo/relo.db. Relo writes that file while it runs, and git add fails on a file that
// changes while it is hashed, which used to cut every agent from HEAD instead of the user's working tree.
func TestSnapshotSurvivesAChurningTrackedReloDatabase(t *testing.T) {
	const dbSize = 4 << 20
	dir := initRepo(t)
	write(t, dir, ".relo/relo.db", strings.Repeat("r", dbSize))
	write(t, dir, ".relo/relo.db-wal", "wal\n")
	write(t, dir, "src/x.go", "package x\n")
	run(t, dir, "git", "add", "-f", ".relo", "src/x.go")
	run(t, dir, "git", "commit", "-m", "track relo database")
	write(t, dir, "src/x.go", "package x\n\nvar Edited = true\n") // the user's edit, left uncommitted

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		db := filepath.Join(dir, ".relo", "relo.db")
		wal := filepath.Join(dir, ".relo", "relo.db-wal")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if f, err := os.OpenFile(db, os.O_WRONLY, 0); err == nil {
				off := int64((i * 4096) % (dbSize - 64<<10))
				_, _ = f.WriteAt(bytes.Repeat([]byte{byte(i)}, 64<<10), off)
				_ = f.Close()
			}
			if i%5 == 0 {
				_ = os.Remove(wal)
				_ = os.WriteFile(wal, []byte{byte(i)}, 0o644)
			}
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	m := New(dir)
	for i := 0; i < 20; i++ {
		snap, err := m.Snapshot()
		if err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		if snap.Commit == "" || snap.HeadFallback {
			t.Fatalf("snapshot %d: commit %q, head fallback %v; agents would be cut from HEAD", i, snap.Commit, snap.HeadFallback)
		}
		for _, p := range strings.Split(output(t, dir, "ls-tree", "-r", "--name-only", snap.Commit), "\n") {
			if p == ".relo" || strings.HasPrefix(p, ".relo/") {
				t.Fatalf("snapshot %d records %q", i, p)
			}
		}
		if got := output(t, dir, "show", snap.Commit+":src/x.go"); !strings.Contains(got, "Edited") {
			t.Fatalf("snapshot %d does not hold the user's edit: %q", i, got)
		}
	}
}

// Relo's own database alone is not the user's uncommitted work, so there is nothing to snapshot.
func TestSnapshotOfOnlyTheTrackedReloDatabaseIsClean(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, ".relo/relo.db", "v1")
	run(t, dir, "git", "add", "-f", ".relo")
	run(t, dir, "git", "commit", "-m", "track relo database")
	head := output(t, dir, "rev-parse", "HEAD")
	write(t, dir, ".relo/relo.db", "v2")
	write(t, dir, ".relo/relo.db-wal", "wal")

	snap, err := New(dir).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Dirty) != 0 || len(snap.Untracked) != 0 {
		t.Fatalf("dirty %v, untracked %v; want none", snap.Dirty, snap.Untracked)
	}
	if snap.Commit != head {
		t.Fatalf("commit = %q, want HEAD %s", snap.Commit, head)
	}
}

// The agent's worktree changes .relo/relo.db and src/x.go and adds a/obj/z.dll. Relo's folder is hidden even
// without settings; the user's ignore paths hide the build output.
func TestChangedHidesReloFolderAndIgnoredPaths(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, ".relo/relo.db", "v1")
	write(t, dir, "src/x.go", "package x\n")
	run(t, dir, "git", "add", "-f", ".relo", "src/x.go")
	run(t, dir, "git", "commit", "-m", "base")
	m := New(dir)
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	tree, err := m.EnsureTree("agent-1", false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, tree.Path, ".relo/relo.db", "v2 from agent")
	write(t, tree.Path, "src/x.go", "package x\n\nvar Changed = true\n")
	write(t, tree.Path, "a/obj/z.dll", "binary")

	check := func(label, want string) {
		t.Helper()
		changed, err := m.Changed("agent-1")
		if err != nil {
			t.Fatal(err)
		}
		files, err := m.ChangedFiles("agent-1")
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		if got := strings.Join(changed, ","); got != want {
			t.Fatalf("%s: Changed = %q, want %q", label, got, want)
		}
		if got := strings.Join(paths, ","); got != want {
			t.Fatalf("%s: ChangedFiles = %q, want %q", label, got, want)
		}
	}
	check("no ignore paths", "a/obj/z.dll,src/x.go")
	m.SetIgnore([]string{"**/obj/**"})
	check("with ignore paths", "src/x.go")
}
