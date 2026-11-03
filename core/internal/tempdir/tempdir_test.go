package tempdir

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// inTemp points os.TempDir() at a fresh directory for the test.
func inTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

// write makes a file with the given age.
func write(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(path, then, then); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// crashed builds a root the way a killed run leaves it: the live marker gone and this process no longer holding
// it. The owner file stays (in production the owner is the core that died and is gone too).
func crashed(t *testing.T, kind, id string, owner int) string {
	t.Helper()
	r, err := New(kind, id, owner)
	if err != nil {
		t.Fatal(err)
	}
	dir := r.Dir()
	if err := os.Remove(filepath.Join(dir, strconv.Itoa(owner)+".live")); err != nil {
		t.Fatal(err)
	}
	registryMu.Lock()
	delete(registry, dir)
	registryMu.Unlock()
	r.mu.Lock()
	r.hold = nil
	r.released = true // a killed process never releases the root itself
	r.mu.Unlock()
	return dir
}

// TestSweepRootLife covers the sweep against a root whose owner and holders are gone, a live root, and entries
// that are not Relo's.
func TestSweepRootLife(t *testing.T) {
	base := inTemp(t)
	live, err := New("agent", "agent_live", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// The kernel's pid 1 runs forever, so use a pid the kernel cannot have run: owner and holder are both gone.
	dead := crashed(t, "agent", "agent_dead", 1<<22-1)
	// Legacy one-shot files: stale ones go, fresh ones stay.
	stale := filepath.Join(base, "relo-claude-system-abc.txt")
	fresh := filepath.Join(base, "relo-claude-mcp-abc.json")
	write(t, stale, 2*staleAge)
	write(t, fresh, 0)
	// Not Relo's: a user's file and a directory that merely starts with "relo-".
	other := filepath.Join(base, "notes.txt")
	write(t, other, 2*staleAge)
	otherDir := filepath.Join(base, "relo-from-somewhere")
	if err := os.Mkdir(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}

	res := Sweep(os.Getpid())
	if len(res.Errors) > 0 {
		t.Fatalf("sweep errors: %v", res.Errors)
	}
	if !exists(t, live.Dir()) {
		t.Fatalf("live root %s removed", live.Dir())
	}
	if exists(t, dead) {
		t.Fatalf("crashed root %s kept", dead)
	}
	if exists(t, stale) {
		t.Fatalf("stale legacy file kept")
	}
	for _, p := range []string{fresh, other, otherDir} {
		if !exists(t, p) {
			t.Fatalf("%s removed", p)
		}
	}
}

// TestSweepKeepsOwnRoots checks that a root this process made is kept even after its marker was lost: a running
// process's temp data is never swept.
func TestSweepKeepsOwnRoots(t *testing.T) {
	inTemp(t)
	r, err := New("session", "s_own", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.Dir(), strconv.Itoa(os.Getpid())+".live")); err != nil {
		t.Fatal(err)
	}
	Sweep(os.Getpid())
	if !exists(t, r.Dir()) {
		t.Fatalf("own root swept")
	}
}

// TestSweepKeepsOtherLiveOwner checks a root made by a live process that lost its marker and is young: the sweep
// keeps it rather than guessing.
func TestSweepKeepsOtherLiveOwner(t *testing.T) {
	inTemp(t)
	// os.Getppid() is the test binary's parent, running for the duration of the test.
	dir := crashed(t, "agent", "agent_peer", os.Getppid())
	Sweep(os.Getpid())
	if !exists(t, dir) {
		t.Fatalf("root of a live owner swept")
	}
	// A fresh copy of a crashed root: owner and holder both dead, so the sweep takes it.
	dead := crashed(t, "agent", "agent_gone", 1<<22-1)
	Sweep(os.Getpid())
	if exists(t, dead) {
		t.Fatalf("root of dead owner and holder kept")
	}
}

func TestRootEndsWithLastHolder(t *testing.T) {
	base := inTemp(t)
	r, err := New("session", "s1", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "relo-session-s1"); r.Dir() != want {
		t.Fatalf("dir = %q, want %q", r.Dir(), want)
	}
	if r.Owner() != os.Getpid() {
		t.Fatalf("owner = %d", r.Owner())
	}
	if err := r.Add(424242); err != nil {
		t.Fatal(err)
	}
	if err := r.Release(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !exists(t, r.Dir()) {
		t.Fatalf("root removed while a holder is live")
	}
	if err := r.Release(424242); err != nil {
		t.Fatal(err)
	}
	if exists(t, r.Dir()) {
		t.Fatalf("root still there after its last release")
	}
	if err := r.Release(424242); err != nil {
		t.Fatalf("repeated release must be a no-op, got %v", err)
	}
	if err := r.Add(424242); err == nil {
		t.Fatalf("Add on a removed root must fail")
	}
}

func TestAddRecordsMarker(t *testing.T) {
	inTemp(t)
	r, err := New("agent", "a1", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(777); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(r.Dir(), "777.live")) {
		t.Fatalf("marker for 777 missing")
	}
}

func TestNewReplacesLeftovers(t *testing.T) {
	base := inTemp(t)
	leftover := filepath.Join(base, "relo-agent-a1")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(leftover, "old"), 0)
	if _, err := New("agent", "a1", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(leftover, "old")) {
		t.Fatalf("root reused with an earlier run's file")
	}
}

func TestRemoveOne(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "relo-claude-system-1.txt")
	write(t, file, 0)
	RemoveOne(file)
	if exists(t, file) {
		t.Fatalf("file kept")
	}
	RemoveOne("")
	if !exists(t, dir) {
		t.Fatalf("parent removed")
	}
}

// TestAppendEnv checks a child gets the three temp variables pointing at its root, replacing inherited values.
func TestAppendEnv(t *testing.T) {
	got := AppendEnv([]string{"PATH=/bin", "TMPDIR=/tmp", "TMP=/var/tmp", "GOTMPDIR=/go/tmp", "HOME=/home/u"}, "/root/tmp")
	joined := strings.Join(got, "\n")
	for _, want := range []string{"TMPDIR=/root/tmp", "TMP=/root/tmp", "GOTMPDIR=/root/tmp", "PATH=/bin", "HOME=/home/u"} {
		if !strings.Contains(joined, want+"\n") && !strings.HasSuffix(joined, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if strings.Contains(joined, "=/tmp\n") || strings.Contains(joined, "=/tmp\t") || strings.Contains(joined, "/go/tmp") {
		t.Errorf("inherited temp value kept: %v", got)
	}
	if !reflect.DeepEqual(AppendEnv([]string{"A=1"}, ""), []string{"A=1"}) {
		t.Errorf("empty root must change nothing")
	}
}

// TestMarkAndLive covers the marker a directory outside os.TempDir() gets so a sweep can tell it is owned.
func TestMarkAndLive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp")
	if Live(dir) {
		t.Fatal("a missing dir must not read as live")
	}
	if err := Mark(dir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !Live(dir) {
		t.Fatal("marked dir not live")
	}
	if err := Unmark(dir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if Live(dir) {
		t.Fatal("unmarked dir still live")
	}
	if err := Unmark(dir, os.Getpid()); err != nil {
		t.Fatalf("unmark of a missing marker must be a no-op, got %v", err)
	}
	// Two cores in one process: each MarkUnique takes its own name, so dropping one keeps the dir live for the other.
	// The stand-in pid is the test binary's own: it runs, so the slots stay live and are not reused underneath the test.
	self := os.Getpid()
	first, err := MarkUnique(dir, self)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarkUnique(dir, self)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two cores got the same marker %d", first)
	}
	if err := UnmarkUnique(dir, first); err != nil {
		t.Fatal(err)
	}
	if !Live(dir) {
		t.Fatal("dropping one core's marker unmarked a dir another core still holds")
	}
	if err := UnmarkUnique(dir, second); err != nil {
		t.Fatal(err)
	}
	if Live(dir) {
		t.Fatal("dir still live after the last core dropped its marker")
	}
}

// deadPid is a pid no live process can hold: above every pid_max (4194304), so a process-table check always says it
// is gone. A pid below that is never a safe stand-in, because the kernel may hand it to a live process.
const deadPid = 100000000

// TestOldUniqueMarkerWithDeadPidIsNotLive is the leftover-unique-marker defect: a core that was never Closed left a
// slot behind, and once its process is gone the slot neither keeps the dir live nor stays taken forever.
func TestOldUniqueMarkerWithDeadPidIsNotLive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Dead-process slots record a real pid, so liveness is decided by that pid, never by the synthetic slot name.
	// Slot 1 holds a fresh dead-pid marker; slot 2 holds the same marker aged past the freshness shortcut.
	freshSlot, oldSlot := uniqueNames+uniqueSlots, uniqueNames+2*uniqueSlots
	// Liveness is decided by content, never by the slot name: a name must never read as a live pid by itself.
	for _, slot := range []int{freshSlot, oldSlot} {
		marker := filepath.Join(dir, markerName(slot))
		if err := os.WriteFile(marker, []byte(strconv.Itoa(deadPid)), 0o644); err != nil {
			t.Fatal(err)
		}
		if slot == oldSlot {
			old := time.Now().Add(-2 * liveMarkerAge)
			if err := os.Chtimes(marker, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if Live(dir) {
		t.Fatal("a unique marker whose process is gone must not keep the dir live")
	}
	again, err := MarkUnique(dir, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if again != freshSlot {
		t.Fatalf("reused slot %d, want the lowest free slot %d", again, freshSlot)
	}
	marker := filepath.Join(dir, markerName(again))
	if got, err := os.ReadFile(marker); err != nil || string(got) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("unique marker content %q (%v), want the real pid %d", got, err, os.Getpid())
	}
}

// TestAllUniqueSlotsTakenDoesNotFailStartup documents the full-range behaviour: every slot held by a live process
// means the dir stays live, and the caller still gets a usable marker instead of an error that stops a core.
func TestAllUniqueSlotsTakenDoesNotFailStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp")
	// The test binary stands in for live pids: ProcessAlive only asks whether a process with the recorded pid runs.
	live := os.Getpid()
	taken := map[int]bool{}
	for i := 0; i < uniqueMarkers; i++ {
		slot, err := MarkUnique(dir, live)
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		if taken[slot] {
			t.Fatalf("slot %d handed out twice", slot)
		}
		taken[slot] = true
	}
	slot, err := MarkUnique(dir, live)
	if err != nil {
		t.Fatalf("a full marker range must not fail startup, got %v", err)
	}
	if slot != live {
		t.Fatalf("full range returned slot %d, want pid %d back", slot, live)
	}
	if !Live(dir) {
		t.Fatal("a dir with a live slot must stay live")
	}
}

// TestSweepReapsRootWhoseUniqueMarkerNamesADeadProcess is the sweep half of the same defect: a root left by a core
// that was never Closed must be collected, and one a live core still marks must not be.
func TestSweepReapsRootWhoseUniqueMarkerNamesADeadProcess(t *testing.T) {
	base := inTemp(t)
	staleRoot := filepath.Join(base, "relo-tmp-dead")
	liveRoot := filepath.Join(base, "relo-tmp-live")
	for _, root := range []string{staleRoot, liveRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	slot := markerName(uniqueNames + uniqueSlots)
	if err := os.WriteFile(filepath.Join(staleRoot, slot), []byte(strconv.Itoa(deadPid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveRoot, slot), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	Sweep(os.Getpid())
	if exists(t, staleRoot) {
		t.Fatal("sweep kept a root whose only marker names a dead process")
	}
	if !exists(t, liveRoot) {
		t.Fatal("sweep removed a root a live process still marks")
	}
}
