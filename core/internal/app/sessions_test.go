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
	tree, err := core.Coord.WS.EnsureTree("agent_t1", false)
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
	tree, err := core.Coord.WS.EnsureTree("agent_t2", false)
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
