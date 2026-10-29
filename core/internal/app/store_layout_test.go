package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
)

// A new workspace with the default DBPath stores data only in .relo/catalog.db and .relo/sessions/.
func TestNewWorkspaceUsesCatalogAndSessionDBs(t *testing.T) {
	dir := t.TempDir()
	core, err := New(Config{Workspace: dir, Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	core.persist()
	core.Close()

	relo := filepath.Join(dir, ".relo")
	if _, err := os.Stat(filepath.Join(relo, "relo.db")); !os.IsNotExist(err) {
		t.Fatalf("relo.db exists or stat failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(relo, "catalog.db")); err != nil {
		t.Fatalf("catalog.db missing: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(relo, "sessions"))
	if err != nil {
		t.Fatalf("sessions dir missing: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no session DB written")
	}
}
