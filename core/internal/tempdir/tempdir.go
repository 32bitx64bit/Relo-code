// Package tempdir gives every Relo session and agent its own directory under the system temp dir, removes it when
// the session or agent ends, and sweeps the ones an earlier run left behind.
//
// A root is os.TempDir()/relo-<kind>-<id>. It holds an owner file (every process that may write there, the root's
// directory included) and the live marker of each process holding the root. Cleanup only ever removes a root whose
// marker names a process that is gone, so a crash cleans up after itself on the next start without ever touching
// the temp data of a live session or agent.
//
// Only directories named relo-tmp-* or relo-claude-* under os.TempDir() are Relo's; everything else there, and
// anything not created by Relo, is left alone.
package tempdir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// prefix is what every tracked root under os.TempDir() starts with: relo-<kind>-<unique id>.
const prefix = "relo-"

// staleAge is how old an untracked relo-claude-* file or directory must be before the sweep removes it. Temp files
// written since the marker fix live inside a tracked root and never rely on age.
const staleAge = time.Hour

// uniqueMarkers is the count of distinct live markers one process can plant in a directory through MarkUnique. Several
// cores in one process can share a directory, each with its own marker, so one core's end does not unmark the others.
const uniqueMarkers = 8

// Root is one Relo-owned temp directory. A session shares its Root with what it starts: every process added with
// Add must call Release, and the session's own Release goes last, before the directory can go.
type Root struct {
	dir      string
	owner    int  // pid recorded in the owner file; the sweep keeps a root whose owner still runs and holds it fresh
	released bool // the directory is gone; late Releases are no-ops
	mu       sync.Mutex
	hold     map[int]bool // pids that keep the directory alive
}

var (
	registryMu sync.Mutex
	registry   = map[string]bool{} // roots this process created or joined, by path
)

// Owner is the pid recorded in a root's owner file.
func (r *Root) Owner() int { return r.owner }

// Dir is the root's path; it is safe to hand to a child as its TMPDIR.
func (r *Root) Dir() string { return r.dir }

// Add keeps the directory alive for pid. It must be called before the process that writes there starts, so the
// sweep can never see the root as unowned in between.
func (r *Root) Add(pid int) error {
	if pid <= 0 || r.dir == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return errors.New("temp root already removed")
	}
	if !r.hold[pid] {
		if err := os.WriteFile(filepath.Join(r.dir, markerName(pid)), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			return err
		}
	}
	if r.hold == nil {
		r.hold = map[int]bool{}
	}
	r.hold[pid] = true
	return nil
}

// Release drops pid's hold and removes the directory once nobody holds it any more. Every writer of the root has
// released by then, so removing the whole tree cannot race a live process.
func (r *Root) Release(pid int) error {
	if r.dir == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hold, pid)
	if r.released || len(r.hold) > 0 {
		return nil
	}
	r.released = true
	registryMu.Lock()
	delete(registry, r.dir)
	registryMu.Unlock()
	return os.RemoveAll(r.dir)
}

// Mark records pid as a live holder of dir without taking a hold this process has to release: the marker says the
// pid has a process writing there, and that process's end is what removes the root. An unmarked entry is not live.
func Mark(dir string, pid int) error {
	if dir == "" || pid <= 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, markerName(pid)), []byte(strconv.Itoa(pid)), 0o644)
}

// Unmark drops pid's live marker from dir; a marker that is not there is not an error. A marker MarkUnique planted is
// named for the process it keeps alive, not for pid, so it is left alone: UnmarkUnique drops that one.
func Unmark(dir string, pid int) error {
	if dir == "" || pid <= 0 {
		return nil
	}
	err := os.Remove(filepath.Join(dir, markerName(pid)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// MarkUnique plants a live marker that no other MarkUnique call in this process shares: the lowest free slot of a
// small fixed range, where a slot whose recorded process is gone counts as free and is reused. Several cores in one
// process can share a directory, and each drops only its own marker on Close. The marker records the real process
// pid, so a slot whose process is gone never pins the directory. When every slot is held by a live process it returns
// pid unchanged and no error: the directory stays live until one of them ends, which is safe, and startup must not
// fail on a full marker range. It returns the name to hand to UnmarkUnique.
func MarkUnique(dir string, pid int) (int, error) {
	if dir == "" || pid <= 0 {
		return pid, nil
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return pid, err
	}
	uniqueMu.Lock()
	defer uniqueMu.Unlock()
	for slot := 1; slot <= uniqueMarkers; slot++ {
		marker := filepath.Join(dir, uniqueName(uniqueNames+slot*uniqueSlots))
		held, err := os.ReadFile(marker)
		if err == nil {
			// The slot is free only when the process it records is gone. An unreadable or unparsable marker is never
			// reused, so a half-written or foreign file is left alone rather than claimed.
			old, convErr := strconv.Atoi(strings.TrimSpace(string(held)))
			if convErr != nil || ProcessAlive(old) {
				continue
			}
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return pid, err
		}
		// uniqueMu serialises every MarkUnique in this process, so no other plant can race this claim: an existing
		// marker is one its own process is gone from and is overwritten here, which is what reuses a dead core's slot.
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return pid, err
		}
		// The marker records a real process pid, never the slot name: Live and the slot check both decide by that pid,
		// so a core that is never Closed stops pinning the directory once its process is gone. A pid that is already
		// gone is still recorded, so the slot is free at once; 1 would never be, since pid 1 always lives.
		_, werr := f.WriteString(strconv.Itoa(pid))
		cerr := f.Close()
		if werr != nil {
			_ = os.Remove(marker)
			return pid, werr
		}
		if cerr != nil {
			_ = os.Remove(marker)
			return pid, cerr
		}
		return uniqueNames + slot*uniqueSlots, nil
	}
	// Every slot names a live process: the directory stays live until one of them ends, which is exactly right, and
	// the caller keeps its own marker rather than failing startup.
	return pid, nil
}

// uniqueNames is the lowest MarkUnique slot. Slot 0 would be pid's own marker from Mark, which Unmark drops.
const uniqueNames = uniqueSlots + 1

// uniqueSlots is the name stride between slots, so every slot name stays above pid_max (4194304 by default) and a
// name is never mistaken for a live pid. Only a marker's content decides liveness: the slot names name no process.
const uniqueSlots = 10000000

// uniqueMu serialises MarkUnique, so two cores in one process never plant the same slot at once.
var uniqueMu sync.Mutex

// liveMarkerAge is how fresh a marker this process planted is for Live to count it live without a process lookup, so
// a core that just planted one is never mistaken for a dead one before the process table can see it.
const liveMarkerAge = 10 * time.Minute

// UnmarkUnique drops the marker MarkUnique returned. A marker that is not there is not an error.
func UnmarkUnique(dir string, pid int) error {
	if dir == "" || pid <= 0 {
		return nil
	}
	err := os.Remove(filepath.Join(dir, uniqueName(pid)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Live reports whether any pid marker in dir names a running process. An unreadable or missing dir has nothing live.
// It is what a sweep outside os.TempDir() uses to tell an entry another core still owns from a leftover.
//
// A marker's name is a slot, not a pid: MarkUnique names a file after the slot it took, and records the real process
// in its content. Only a marker named for this process, this young, counts without a lookup, which keeps a just-planted
// marker live where the process table cannot be trusted (a container without /proc) and never trusts a slot name.
func Live(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	self := markerName(os.Getpid())
	young := time.Now().Add(-liveMarkerAge)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".live") {
			continue
		}
		if e.Name() == self {
			if info, err := e.Info(); err == nil && info.ModTime().After(young) {
				return true
			}
		}
		if recorded, ok := recordedPid(dir, e.Name()); ok && ProcessAlive(recorded) {
			return true
		}
	}
	return false
}

// recordedPid returns the pid a marker's content names. A marker with no readable, numeric content names none.
func recordedPid(dir, name string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid, err == nil && pid > 0
}

// New creates a root os.TempDir()/relo-<kind>-<id> for pid. An existing directory of the same name is emptied: the
// name is unique per session or agent, so anything there belongs to the run that just ended.
func New(kind, id string, pid int) (*Root, error) {
	if kind == "" || id == "" {
		return nil, errors.New("temp root needs a kind and an id")
	}
	base := os.TempDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(base, prefix+kindName(kind)+"-"+safe(id))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &Root{dir: dir, owner: pid}
	if pid > 0 {
		if err := os.WriteFile(filepath.Join(dir, ownerName(pid)), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		r.hold = map[int]bool{pid: true}
		if err := os.WriteFile(filepath.Join(dir, markerName(pid)), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
	}
	registryMu.Lock()
	registry[dir] = true
	registryMu.Unlock()
	return r, nil
}

// AppendEnv returns env with TMPDIR, TMP and GOTMPDIR pointing at dir, replacing any value it already has. Every
// child Relo starts gets these, so nothing it writes through a temp helper lands in the shared system temp dir.
func AppendEnv(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == "TMPDIR" || k == "TMP" || k == "GOTMPDIR" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TMPDIR="+dir, "TMP="+dir, "GOTMPDIR="+dir)
}

// RemoveOne removes a one-shot temp file (or directory) Relo just finished with, keeping its parent.
func RemoveOne(path string) {
	if path != "" {
		_ = os.RemoveAll(path)
	}
}

func uniqueName(pid int) string { return markerName(pid) }

func ownerName(pid int) string  { return strconv.Itoa(pid) + ".owner" }
func markerName(pid int) string { return strconv.Itoa(pid) + ".live" }

// kindName keeps a kind a single path element with no dash, so a root's kind can be read off its name.
func kindName(kind string) string {
	var b strings.Builder
	for _, c := range kind {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "tmp"
	}
	return b.String()
}

// safe keeps an id a single path element.
func safe(id string) string {
	id = strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			return c
		}
		return '_'
	}, id)
	return strings.TrimLeft(id, ".")
}
