package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/opencode"
	"github.com/relocode/relo/internal/tempdir"
)

// TestStartupStateTempSweepKeepsOwnMarker checks the startup sweep never clears this core's live marker: it is what
// stops another core from collecting temp files this core is still writing. Only Close drops it.
func TestStartupStateTempSweepKeepsOwnMarker(t *testing.T) {
	root := t.TempDir()
	if err := opencode.EnsureTmp(root, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	entry := filepath.Join(opencode.TmpDir(root), "go-build-old")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(entry, old, old); err != nil {
		t.Fatal(err)
	}
	a := &App{Cfg: Config{StateDir: root}, Log: logging.New(&bytes.Buffer{}, logging.Error)}
	a.sweepStateTemp(false) // startup: no marker is dropped, so the live check keeps everything
	if tempDirUnused(root) {
		t.Fatal("startup sweep cleared this core's live marker")
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("startup sweep removed a live core's temp data: %v", err)
	}
	// A second sweep (another startup) still keeps it.
	a.sweepStateTemp(false)
	if !tempdir.Live(opencode.TmpDir(root)) {
		t.Fatal("marker gone after a second startup sweep")
	}
}

// TestCloseStateTempSweepClearsOwnMarkerAfterRuntimeStops checks Close order: the marker goes first, then only old
// unmarked entries are swept while another core's marker would still stop everything.
func TestCloseStateTempSweepClearsOwnMarkerAfterRuntimeStops(t *testing.T) {
	root := t.TempDir()
	if err := opencode.EnsureTmp(root, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	entry := filepath.Join(opencode.TmpDir(root), "go-build-old")
	if err := os.Mkdir(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(entry, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(opencode.TmpDir(root), "fresh")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Another core is live: even after this core's marker goes, nothing is swept.
	other := os.Getppid()
	if !tempdir.ProcessAlive(other) {
		t.Skip("parent process is not a usable live pid here")
	}
	if err := tempdir.Mark(opencode.TmpDir(root), other); err != nil {
		t.Fatal(err)
	}
	a := &App{Cfg: Config{StateDir: root}, Log: logging.New(&bytes.Buffer{}, logging.Error)}
	a.sweepStateTemp(true)
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("swept while another core was live: %v", err)
	}
	if !tempdir.Live(opencode.TmpDir(root)) {
		t.Fatal("another core's marker must survive this core's close")
	}
	// With no core left, Close takes only the old entry.
	if err := os.Remove(filepath.Join(opencode.TmpDir(root), strconv.Itoa(other)+".live")); err != nil {
		t.Fatal(err)
	}
	a.sweepStateTemp(true)
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("old entry kept after close: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh entry removed: %v", err)
	}
}
