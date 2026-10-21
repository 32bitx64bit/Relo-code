package app

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/orchestrator"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newGitCore(t *testing.T, files map[string]string) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "relo@test")
	git(t, dir, "config", "user.name", "Relo")
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")
	core, err := New(Config{Workspace: dir, DBPath: filepath.Join(t.TempDir(), "relo.db"), Runtime: "mock"}, logging.New(&bytes.Buffer{}, logging.Error))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	return core, dir
}

// proposeChange makes an isolated worktree for an agent, lets edit change it, and records the resulting change the way a
// worker would.
func proposeChange(t *testing.T, core *App, dir, rel string, edit func(path string)) orchestrator.Change {
	t.Helper()
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	tree, err := mountTree(core, "agent_t1")
	if err != nil {
		t.Fatal(err)
	}
	edit(filepath.Join(tree.Path, rel))
	s := core.Coord.Current()
	s.Changes = append(s.Changes, orchestrator.Change{ID: "chg1", AgentID: "agent_t1", Path: rel, Kind: "modified", Status: "proposed", Worktree: tree.Path})
	core.Coord.Restore(s)
	ch, _ := core.Coord.FindChange("chg1")
	return ch
}

func TestAcceptChangeKeepsLocalEditsAndReleasesWorktree(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\nfive\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("ONE\ntwo\nthree\nfour\nfive\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	// The user edits a different part of the same file while the agent works.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\nfour\nFIVE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.acceptChange(ch.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "ONE\ntwo\nthree\nfour\nFIVE\n" {
		t.Fatalf("merged file = %q", got)
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "accepted" {
		t.Fatalf("status = %q", now.Status)
	}
	if _, err := os.Stat(ch.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree was not released: %v", err)
	}
}

func TestAcceptChangeConflictChangesNothing(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := core.acceptChange(ch.ID)
	if err == nil || !strings.Contains(err.Error(), "a.txt") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("conflict error = %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "mine\ntwo\n" {
		t.Fatalf("workspace file was touched: %q", got)
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "proposed" {
		t.Fatalf("a failed accept must leave the change proposed, got %q", now.Status)
	}
	if _, err := os.Stat(ch.Worktree); err != nil {
		t.Fatalf("worktree must stay for a conflicting change: %v", err)
	}
}

func TestRejectChangeLeavesWorkspaceAlone(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) { _ = os.WriteFile(p, []byte("agent\n"), 0o644) })
	if _, err := core.rejectChange(ch.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "one\n" {
		t.Fatalf("workspace file = %q", got)
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "rejected" {
		t.Fatalf("status = %q", now.Status)
	}
}

func TestChangeDiffReturnsBothSides(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) { _ = os.WriteFile(p, []byte("agent\n"), 0o644) })
	res, err := core.changeDiff(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["before"] != "one\n" || m["after"] != "agent\n" || m["hasBefore"] != true || m["hasAfter"] != true {
		t.Fatalf("diff = %+v", m)
	}
}

func TestAcceptAllReportsConflictsAndAppliesTheRest(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "uno\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	tree, err := mountTree(core, "agent_t2")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tree.Path, "a.txt"), []byte("agent\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tree.Path, "b.txt"), []byte("agent b\n"), 0o644)
	s := core.Coord.Current()
	s.Changes = []orchestrator.Change{
		{ID: "c1", AgentID: "agent_t2", Path: "a.txt", Status: "proposed", Worktree: tree.Path},
		{ID: "c2", AgentID: "agent_t2", Path: "b.txt", Status: "proposed", Worktree: tree.Path},
	}
	core.Coord.Restore(s)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\n"), 0o644)
	res, err := core.decideAll(true)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["done"] != 1 || len(m["failed"].([]bulkFailure)) != 1 {
		t.Fatalf("bulk result = %+v", m)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "agent b\n" {
		t.Fatalf("b.txt = %q", got)
	}
}

func TestAcceptAllClearsConflictState(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "two\n"})
	first := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(first.Worktree, "b.txt"), []byte("agent b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := core.Coord.Current()
	s.Changes = append(s.Changes, orchestrator.Change{ID: "c2", AgentID: first.AgentID, Path: "b.txt", Status: "proposed", Worktree: first.Worktree})
	for i := range s.Changes {
		s.Changes[i].Conflict = overlapReason
		s.Changes[i].Resolving = true
	}
	core.Coord.Restore(s)

	res, err := core.decideAll(true)
	if err != nil {
		t.Fatalf("accept all: %v", err)
	}
	m := res.(map[string]any)
	if m["done"] != 2 || len(m["failed"].([]bulkFailure)) != 0 {
		t.Fatalf("bulk result = %+v", m)
	}
	changes := m["changes"].([]orchestrator.Change)
	if len(changes) != 2 {
		t.Fatalf("returned changes = %+v", changes)
	}
	for _, ch := range changes {
		if ch.Status != "accepted" || ch.Conflict != "" || ch.Resolving {
			t.Fatalf("accepted change keeps conflict state: %+v", ch)
		}
	}
	if _, err := os.Stat(first.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree was not released: %v", err)
	}
}

func TestAcceptAllBatchKeepsSameFileOrderAndPartialFailures(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "uno\n", "c.txt": "tres\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	tree, err := mountTree(core, "agent_batch")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tree.Path, "a.txt"), []byte("agent a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tree.Path, "b.txt"), []byte("agent b\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tree.Path, "c.txt"), []byte("agent c\n"), 0o644)
	s := core.Coord.Current()
	s.Changes = []orchestrator.Change{
		{ID: "c1", AgentID: "agent_batch", Path: "a.txt", Status: "proposed", Worktree: tree.Path},
		{ID: "c2", AgentID: "agent_batch", Path: "b.txt", Status: "proposed", Worktree: tree.Path},
		{ID: "c3", AgentID: "agent_batch", Path: "a.txt", Status: "proposed", Worktree: tree.Path},
		{ID: "c4", AgentID: "agent_batch", Path: "c.txt", Status: "proposed", Worktree: tree.Path},
	}
	core.Coord.Restore(s)
	_ = os.WriteFile(filepath.Join(dir, "c.txt"), []byte("mine c\n"), 0o644)
	res, err := core.decideAll(true)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	failed := m["failed"].([]bulkFailure)
	if m["done"] != 3 || len(failed) != 1 || failed[0].ID != "c4" {
		t.Fatalf("bulk result = %+v", m)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if now, _ := core.Coord.FindChange(id); now.Status != "accepted" {
			t.Fatalf("%s status = %q", id, now.Status)
		}
	}
	if now, _ := core.Coord.FindChange("c4"); now.Status != "proposed" {
		t.Fatalf("failed change status = %q", now.Status)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "agent a\n" {
		t.Fatalf("a.txt = %q (same-file changes must apply in order)", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "agent b\n" {
		t.Fatalf("b.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(got) != "mine c\n" {
		t.Fatalf("c.txt was touched: %q", got)
	}
}

func TestAcceptAllMergesSamePathChangesFromDifferentAgents(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	firstTree, err := mountTree(core, "agent_first")
	if err != nil {
		t.Fatal(err)
	}
	secondTree, err := mountTree(core, "agent_second")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstTree.Path, "a.txt"), []byte("ONE\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondTree.Path, "a.txt"), []byte("one\ntwo\nthree\nFOUR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := core.Coord.Current()
	s.Changes = []orchestrator.Change{
		{ID: "first", AgentID: "agent_first", Path: "a.txt", Status: "proposed", Worktree: firstTree.Path},
		{ID: "second", AgentID: "agent_second", Path: "a.txt", Status: "proposed", Worktree: secondTree.Path},
	}
	core.Coord.Restore(s)

	res, err := core.decideAll(true)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["done"] != 2 || len(m["failed"].([]bulkFailure)) != 0 {
		t.Fatalf("bulk result = %+v", m)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ONE\ntwo\nthree\nFOUR\n" {
		t.Fatalf("merged file = %q", got)
	}
}

func TestAcceptWritesJournalAndUndoRestoresBytes(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		_ = os.WriteFile(p, []byte("agent\n"), 0o644)
	})
	if _, err := core.acceptChange(ch.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "agent\n" {
		t.Fatalf("accepted file = %q", got)
	}
	if _, _, err := core.Coord.WS.JournalEntryFor(ch.ID); err != nil {
		t.Fatalf("accepted change has no journal entry: %v", err)
	}
	if _, err := core.undoChange(ch.ID); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\n" {
		t.Fatalf("undo restored %q, want the pre-image", got)
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "rejected" {
		t.Fatalf("status after undo = %q", now.Status)
	}
}

func TestUndoRefusesAfterTheUserEditsTheFile(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		_ = os.WriteFile(p, []byte("agent\n"), 0o644)
	})
	if _, err := core.acceptChange(ch.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.undoChange(ch.ID); err == nil {
		t.Fatal("undo after a user edit was not refused")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "user\n" {
		t.Fatalf("refused undo touched the file: %q", got)
	}
}

func TestDirtyPathIsRefusedAndOtherPathsStillLand(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "uno\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	tree, err := mountTree(core, "agent_dirty")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tree.Path, "a.txt"), []byte("agent a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tree.Path, "b.txt"), []byte("agent b\n"), 0o644)
	s := core.Coord.Current()
	s.Changes = []orchestrator.Change{
		{ID: "d1", AgentID: "agent_dirty", Path: "a.txt", Status: "proposed", Worktree: tree.Path},
		{ID: "d2", AgentID: "agent_dirty", Path: "b.txt", Status: "proposed", Worktree: tree.Path},
	}
	core.Coord.Restore(s)

	core.setDirtyPaths([]string{"a.txt"})
	if _, err := core.acceptChange("d1"); err == nil {
		t.Fatal("a dirty path was accepted")
	}
	now, _ := core.Coord.FindChange("d1")
	if now.Status != "proposed" || now.Integration == nil || now.Integration.Kind != "unsaved_buffer" {
		t.Fatalf("dirty change = %+v", now)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\n" {
		t.Fatalf("dirty path was written: %q", got)
	}

	res, err := core.decideAll(true)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["done"] != 1 || len(m["failed"].([]bulkFailure)) != 1 {
		t.Fatalf("bulk result = %+v", m)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "agent b\n" {
		t.Fatalf("clean path did not land: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\n" {
		t.Fatalf("dirty path was written by accept-all: %q", got)
	}
}

func TestAutoApplySkipsADirtyPath(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		_ = os.WriteFile(p, []byte("agent\n"), 0o644)
	})
	core.setDirtyPaths([]string{"a.txt"})
	core.applySettings(map[string]any{"autoApply": true})
	waitAutoApply(t, core)
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "proposed" {
		t.Fatalf("auto-apply accepted a dirty path: %+v", now)
	}
	if now.Integration == nil || now.Integration.Kind != "unsaved_buffer" {
		t.Fatalf("dirty auto-apply state = %+v", now.Integration)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "base\n" {
		t.Fatalf("dirty path was written: %q", got)
	}
}

func TestAutoApplyLandsCleanPathBesideADirtyOne(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n", "b.txt": "uno\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	tree, err := mountTree(core, "agent_t1")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tree.Path, "a.txt"), []byte("agent a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(tree.Path, "b.txt"), []byte("agent b\n"), 0o644)
	s := core.Coord.Current()
	s.Changes = []orchestrator.Change{
		{ID: "dirty", AgentID: "agent_t1", Path: "a.txt", Status: "proposed", Worktree: tree.Path},
		{ID: "clean", AgentID: "agent_t1", Path: "b.txt", Status: "proposed", Worktree: tree.Path},
	}
	core.Coord.Restore(s)
	core.setDirtyPaths([]string{"a.txt"})
	core.applySettings(map[string]any{"autoApply": true})
	waitAutoApply(t, core)
	if now, _ := core.Coord.FindChange("dirty"); now.Status != "proposed" || now.Integration == nil || now.Integration.Kind != "unsaved_buffer" {
		t.Fatalf("dirty change = %+v", now)
	}
	if now, _ := core.Coord.FindChange("clean"); now.Status != "accepted" {
		t.Fatalf("clean change did not land: %+v", now)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "base\n" {
		t.Fatalf("dirty path was written: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "agent b\n" {
		t.Fatalf("clean path not written: %q", got)
	}
}

// A journal that cannot be written fails an overwrite closed, but a create still succeeds because it cannot lose
// user bytes.
func TestJournalFailureFailsOverwriteClosedAndAllowsCreate(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		_ = os.WriteFile(p, []byte("agent\n"), 0o644)
	})
	refs := filepath.Join(dir, ".git", "refs", "relo")
	if err := os.MkdirAll(refs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refs, "journal"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.acceptChange(ch.ID); err == nil {
		t.Fatal("an overwrite whose journal failed was accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\n" {
		t.Fatalf("failed journal still wrote the file: %q", got)
	}

	tree, err := mountTree(core, ch.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Path, "new.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := core.Coord.Current()
	s.Changes = append(s.Changes, orchestrator.Change{ID: "created", AgentID: ch.AgentID, Path: "new.txt", Status: "proposed", Worktree: tree.Path})
	core.Coord.Restore(s)
	if _, err := core.acceptChange("created"); err != nil {
		t.Fatalf("a create with a failed journal was refused: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "new.txt")); string(got) != "fresh\n" {
		t.Fatalf("created file = %q", got)
	}
}

func TestStalePreviewWritesNothing(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		_ = os.WriteFile(p, []byte("agent\n"), 0o644)
	})
	analysis, err := core.analyzeChange(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.acceptChangeExpect(ch.ID, "not-the-fingerprint"); err == nil {
		t.Fatal("a stale preview was accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\n" {
		t.Fatalf("stale preview wrote the file: %q", got)
	}
	if _, err := core.acceptChangeExpect(ch.ID, analysis.Fingerprint); err != nil {
		t.Fatalf("fresh preview refused: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "agent\n" {
		t.Fatalf("fresh accept = %q", got)
	}
}

func TestSessionHistoryNewOpenDelete(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "x\n"})
	core.Coord.CreateSession(dir, "first goal", orchestrator.ModeBuild)
	core.Coord.Notify("noop", nil)
	s := core.Coord.Current()
	s.Messages = append(s.Messages, orchestrator.Chat{ID: "m1", Role: "user", Text: "first goal"})
	core.Coord.Restore(s)
	firstID := s.ID

	snap, err := core.Handle("session.create", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	sess := snap.(map[string]any)["session"].(*orchestrator.Session)
	if sess.ID == firstID || len(snap.(map[string]any)["messages"].([]orchestrator.Chat)) != 0 {
		t.Fatalf("new session did not start blank: %+v", snap)
	}
	list, err := core.listSessions()
	if err != nil || len(list) != 1 || list[0].ID != firstID || list[0].Title != "first goal" || list[0].Current {
		t.Fatalf("history = %+v err=%v", list, err)
	}
	// Opening the old session brings its chat back; the blank one it replaces is not kept.
	opened, err := core.Handle("session.open", json.RawMessage(`{"id":"`+firstID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if msgs := opened.(map[string]any)["messages"].([]orchestrator.Chat); len(msgs) != 1 || msgs[0].Text != "first goal" {
		t.Fatalf("opened messages = %+v", msgs)
	}
	list, _ = core.listSessions()
	if len(list) != 1 || !list[0].Current {
		t.Fatalf("history after open = %+v", list)
	}
	if _, err := core.Handle("session.delete", json.RawMessage(`{"id":"`+firstID+`"}`)); err != nil {
		t.Fatal(err)
	}
	list, _ = core.listSessions()
	if len(list) != 0 {
		t.Fatalf("history after delete = %+v", list)
	}
	if cur := core.Coord.Current(); cur == nil || cur.ID == firstID {
		t.Fatalf("deleting the open session must leave a fresh one, got %+v", cur)
	}
}

// An accept records what it did to the file, so a later conflict can be attributed to a landed change.
func TestAcceptsRecordLandedAction(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "one\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	s := core.Coord.Current()
	for _, name := range []string{"a.txt", "b.txt"} {
		agentID := "agent-" + name
		tree, err := mountTree(core, agentID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tree.Path, name), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		s.Changes = append(s.Changes, orchestrator.Change{ID: "chg-" + name, AgentID: agentID, Path: name, Kind: "modified", Status: "proposed", Worktree: tree.Path})
	}
	core.Coord.Restore(s)
	if _, err := core.acceptAll(false); err != nil {
		t.Fatalf("acceptAll: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		got, _ := core.Coord.FindChange("chg-" + name)
		if got.Status != "accepted" || got.Landed != "updated" {
			t.Fatalf("%s: status=%q landed=%q, want accepted/updated", name, got.Status, got.Landed)
		}
	}
}
