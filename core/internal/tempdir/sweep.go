package tempdir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Result counts what a Sweep removed.
type Result struct {
	Removed []string // paths taken away
	Kept    int      // entries left because a live process holds them or they are not Relo's
	Errors  []error
}

// Sweep removes the Relo-owned temp entries whose owning processes are gone. self is the calling process: roots it
// created, or joined, are never touched. A root is stale when none of its live markers names a running process, or
// when it has no live marker and its owner file names a pid that is gone; a root with no marker at all is stale
// once it is older than staleAge. Entries that are not Relo's are left alone.
func Sweep(self int) Result {
	res := Result{}
	base := os.TempDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			res.Errors = append(res.Errors, err)
		}
		return res
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		path := filepath.Join(base, name)
		if registry[path] {
			res.Kept++
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			res.Kept++
			continue
		}
		stale := false
		if info.IsDir() && isRootName(name) {
			stale = rootStale(path, info, self)
		} else if strings.HasPrefix(name, "relo-claude-") && time.Since(info.ModTime()) > staleAge {
			stale = true // a one-shot file from before roots existed
		}
		if !stale {
			res.Kept++
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		res.Removed = append(res.Removed, path)
	}
	return res
}

// rootStale reports whether a Relo root was left behind by processes that are gone. A root whose live markers all
// name dead pids is stale even if its creator still runs: the creators remove their own roots on Release.
func rootStale(path string, info fs.FileInfo, self int) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false // unreadable: not ours to judge
	}
	owners, live := []int{}, []int{}
	for _, e := range entries {
		if pid, ok := pidNamed(e.Name(), ".owner"); ok {
			owners = append(owners, pid)
		} else if pid, ok := pidNamed(e.Name(), ".live"); ok {
			live = append(live, pid)
		}
	}
	if len(live) > 0 {
		return !anyAlive(live)
	}
	if len(owners) > 0 {
		return !anyAlive(owners)
	}
	return self != 0 && time.Since(info.ModTime()) > staleAge
}

func pidNamed(name, suffix string) (int, bool) {
	raw, ok := strings.CutSuffix(name, suffix)
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(raw)
	return pid, err == nil
}

// anyAlive reports whether any pid is a running process.
func anyAlive(pids []int) bool {
	for _, pid := range pids {
		if pid > 0 && ProcessAlive(pid) {
			return true
		}
	}
	return false
}

// isRootName reports whether name is a tracked root (relo-<kind>-<id>). Legacy one-shot files carry an extension
// and no marker, so they are not roots; they age out instead.
func isRootName(name string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	return ok && strings.Contains(rest, "-") && !strings.Contains(rest, ".")
}
