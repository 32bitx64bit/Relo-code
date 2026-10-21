package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
)

// openVirtualCore opens a core over an existing repository and database, with virtual workspaces on, so a test can
// stop it and open it again as a restart.
func openVirtualCore(t *testing.T, dir, db string) (*App, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	core, err := New(Config{Workspace: dir, DBPath: db, Runtime: "mock"}, logging.New(&logs, logging.Info))
	if err != nil {
		t.Fatal(err)
	}
	core.applySettings(map[string]any{"workspaceMode": "virtual"})
	return core, &logs
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "relo@test")
	git(t, dir, "config", "user.name", "Relo")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")
	return dir
}

// The stored mirror follows the revisions Git holds, and is forgotten when the workspace is released.
func TestWorkspaceMirrorFollowsGit(t *testing.T) {
	dir := initRepo(t)
	core, _ := openVirtualCore(t, dir, filepath.Join(t.TempDir(), "relo.db"))
	defer core.Close()
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)

	if _, err := mountTree(core, "agent_m"); err != nil {
		t.Fatal(err)
	}
	stored := func() (rev uint64, tree, state string, n int) {
		rows, err := core.Store.ListVWorkspaces(core.Coord.WS.Root())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.AgentID == "agent_m" {
				rev, tree, state = r.Revision, r.Tree, r.State
			}
		}
		return rev, tree, state, len(rows)
	}
	eventually(t, "the new workspace to be mirrored", func() bool { _, _, _, n := stored(); return n == 1 })

	dirPath, _ := core.Coord.WS.Materialize("agent_m")
	if err := os.WriteFile(filepath.Join(dirPath, "tool.txt"), []byte("made by a tool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.Coord.WS.Sync("agent_m"); err != nil {
		t.Fatal(err)
	}
	info, _ := core.Coord.WS.VirtualWorkspace("agent_m")
	if info.Revision < 1 || !info.Dirty {
		t.Fatalf("workspace = %+v", info)
	}
	eventually(t, "the captured revision to be mirrored", func() bool {
		rev, tree, _, _ := stored()
		return rev == info.Revision && tree == info.EffectiveTree
	})
	// The same revision is in the durable event log.
	rows, err := core.Store.EventRows(core.Coord.SessionID(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var logged bool
	for _, r := range rows {
		if r.Name == "workspace.capture" && strings.Contains(string(r.Payload), "tool.txt") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("the capture is not in the event log: %+v", rows)
	}

	if err := core.Coord.WS.Release("agent_m"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the released workspace to be forgotten", func() bool { _, _, _, n := stored(); return n == 0 })
}

// A stop in the middle of a run loses nothing: the next start finds the workspace in Git, imports what the tools
// had written, and mirrors it.
func TestWorkspaceSurvivesAnInterruptedRun(t *testing.T) {
	dir := initRepo(t)
	db := filepath.Join(t.TempDir(), "relo.db")
	core, _ := openVirtualCore(t, dir, db)
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	if _, err := mountTree(core, "agent_i"); err != nil {
		t.Fatal(err)
	}
	dirPath, _ := core.Coord.WS.Materialize("agent_i")
	if err := os.WriteFile(filepath.Join(dirPath, "unsaved.txt"), []byte("not yet imported\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the workspace to be mirrored", func() bool {
		rows, _ := core.Store.ListVWorkspaces(core.Coord.WS.Root())
		return len(rows) == 1
	})
	core.Close() // the agent never settled: the directory still holds work

	core2, _ := openVirtualCore(t, dir, db)
	defer core2.Close()
	core2.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	tree, err := core2.Coord.WS.EnsureTree("agent_i", false)
	if err != nil || !tree.Virtual {
		t.Fatalf("the workspace was not adopted: %+v %v", tree, err)
	}
	files, err := core2.Coord.WS.ChangedFiles("agent_i")
	if err != nil || len(files) != 1 || files[0].Path != "unsaved.txt" {
		t.Fatalf("what the tools wrote was lost: %+v %v", files, err)
	}
	rows, err := core2.Store.ListVWorkspaces(core2.Coord.WS.Root())
	if err != nil || len(rows) != 1 || rows[0].AgentID != "agent_i" {
		t.Fatalf("mirror after restart = %+v %v", rows, err)
	}
}

// Refs that were deleted behind Relo's back are noticed, and only reported when edits were lost with them.
func TestLostWorkspaceRefsAreReportedOnlyWhenEditsWereLost(t *testing.T) {
	dir := initRepo(t)
	db := filepath.Join(t.TempDir(), "relo.db")
	core, _ := openVirtualCore(t, dir, db)
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	for _, id := range []string{"agent_clean", "agent_dirty"} {
		if _, err := core.Coord.WS.EnsureTree(id, false); err != nil {
			t.Fatal(err)
		}
	}
	dirPath, _ := core.Coord.WS.Materialize("agent_dirty")
	if err := os.WriteFile(filepath.Join(dirPath, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.Coord.WS.Settle("agent_dirty"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both workspaces to be mirrored with their edits", func() bool {
		rows, _ := core.Store.ListVWorkspaces(core.Coord.WS.Root())
		dirty := false
		for _, r := range rows {
			if r.AgentID == "agent_dirty" && strings.Contains(string(r.Payload), `"dirty":true`) {
				dirty = true
			}
		}
		return len(rows) == 2 && dirty
	})
	core.Close()

	for _, ref := range strings.Fields(gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/vws/")) {
		git(t, dir, "update-ref", "-d", ref)
	}

	core2, logs := openVirtualCore(t, dir, db)
	defer core2.Close()
	if rows, _ := core2.Store.ListVWorkspaces(core2.Coord.WS.Root()); len(rows) != 0 {
		t.Fatalf("workspaces that are gone from Git are still mirrored: %+v", rows)
	}
	out := logs.String()
	if !strings.Contains(out, "workspace.lost") || !strings.Contains(out, "agent_dirty") {
		t.Fatalf("the lost edits were not reported:\n%s", out)
	}
	if strings.Count(out, "workspace.lost") != 1 {
		t.Fatalf("a clean workspace that vanished was reported as a loss:\n%s", out)
	}
}
