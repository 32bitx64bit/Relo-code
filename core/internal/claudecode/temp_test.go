package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/relocode/relo/internal/runtime"
)

// TestASessionKeepsItsTempDataInItsOwnRootAndDropsItOnClose checks that the system prompt file lives under the
// session's Relo temp root, that the CLI is told to use that root, and that closing the session removes it.
func TestASessionKeepsItsTempDataInItsOwnRootAndDropsItOnClose(t *testing.T) {
	rt, log := fakeRuntime(t, "ok")
	sid, evs := start(t, rt, runtime.SessionConfig{Directory: t.TempDir()})
	if err := rt.SendTask(context.Background(), sid, task("plain")); err != nil {
		t.Fatal(err)
	}
	collect(t, evs, nil)
	e := startArgs(t, log)
	system, ok := flag(e.Args, "--system-prompt-file")
	if !ok {
		t.Fatal("no system prompt file")
	}
	if _, err := os.Stat(system); err != nil {
		t.Fatalf("system prompt file %s: %v", system, err)
	}
	root := filepath.Dir(system)
	if e.Tmpdir != root {
		t.Fatalf("session TMPDIR = %q, want %s", e.Tmpdir, root)
	}
	if filepath.Base(root) != "relo-session-"+string(sid) {
		t.Fatalf("temp file %s is not in the session's root", system)
	}
	if err := rt.CloseSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("session temp root %s still there after close: %v", root, err)
	}
}
