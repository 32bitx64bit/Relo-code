package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/workspace"
)

func TestResolveConflictsCleanIsNotConflicted(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\nfive\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("ONE\ntwo\nthree\nfour\nfive\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\nfour\nFIVE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := core.resolveConflicts()
	if err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	if got := res.(map[string]any)["conflicted"]; got != 0 {
		t.Fatalf("clean merge reported conflicted=%v", got)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "proposed" || now.Conflict != "" {
		t.Fatalf("clean merge: status=%q conflict=%q", now.Status, now.Conflict)
	}
	if _, err := core.acceptChange(ch.ID); err != nil {
		t.Fatalf("accept after clean check: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "ONE\ntwo\nthree\nfour\nFIVE\n" {
		t.Fatalf("merged file = %q", got)
	}
}

func TestResolveConflictsRealConflictPromptsHead(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core.headMu.Lock()
	core.headBusy = true
	core.headMu.Unlock()
	res, err := core.resolveConflicts()
	if err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	m := res.(map[string]any)
	if m["conflicted"] != 1 || m["prompted"] != true {
		t.Fatalf("real conflict result: %+v", m)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "proposed" || now.Conflict == "" {
		t.Fatalf("real conflict: status=%q conflict=%q", now.Status, now.Conflict)
	}
	if now.Integration == nil || now.Integration.State != "conflicted" {
		t.Fatalf("integration = %+v, want conflicted", now.Integration)
	}
	if cur, _ := core.Coord.FindChange(ch.ID); !cur.Resolving {
		t.Fatalf("conflicted change not marked resolving")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "mine\ntwo\n" {
		t.Fatalf("workspace file was touched: %q", got)
	}
	agent, _ := os.ReadFile(ch.Worktree + "/a.txt")
	if string(agent) != "agent\ntwo\n" {
		t.Fatalf("agent worktree changed: %q", agent)
	}
	text := core.conflictPrompt([]orchestrator.Change{now})
	for _, want := range []string{"id: " + ch.ID, "path: a.txt", "reason: " + overlapReason, "agent\ntwo\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("head prompt missing %q:\n%s", want, text)
		}
	}
}

func TestResolveConflictsHandlesMissingTrees(t *testing.T) {
	for _, scenario := range []string{"released", "restart", "open failure"} {
		t.Run(scenario, func(t *testing.T) {
			core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "two\n"})
			first := proposeChange(t, core, dir, "a.txt", func(p string) {
				if err := os.WriteFile(p, []byte("agent a\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			})
			tree, err := core.Coord.WS.EnsureTree("agent_live", false)
			if err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string]string{
				filepath.Join(tree.Path, "b.txt"): "agent b\n",
				filepath.Join(dir, "a.txt"):       "mine a\n",
				filepath.Join(dir, "b.txt"):       "mine b\n",
			} {
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			wantReason := overlapReason
			switch scenario {
			case "released":
				if err := core.Coord.WS.Release(first.AgentID); err != nil {
					t.Fatal(err)
				}
				wantReason = "worktree is unavailable"
			case "restart":
				core.Coord.WS = workspace.New(dir)
			case "open failure":
				core.Coord.WS = workspace.New(dir)
				first.AgentID = "invalid/agent"
				wantReason = "invalid agent id"
			}
			s := core.Coord.Current()
			s.Changes = []orchestrator.Change{
				first,
				{ID: "live", AgentID: tree.AgentID, Path: "b.txt", Status: "proposed", Worktree: tree.Path},
			}
			core.Coord.Restore(s)
			core.headMu.Lock()
			core.headBusy = true
			core.headMu.Unlock()

			res, err := core.resolveConflicts()
			if err != nil {
				t.Fatalf("resolveConflicts: %v", err)
			}
			m := res.(map[string]any)
			if m["conflicted"] != 2 || m["prompted"] != true {
				t.Fatalf("resolve result = %+v", m)
			}
			for id, reason := range map[string]string{first.ID: wantReason, "live": overlapReason} {
				ch, _ := core.Coord.FindChange(id)
				if ch.Status != "proposed" || !ch.Resolving || !strings.Contains(ch.Conflict, reason) {
					t.Fatalf("%s: status=%q resolving=%v conflict=%q, want %q", id, ch.Status, ch.Resolving, ch.Conflict, reason)
				}
			}
			core.headMu.Lock()
			queued := append([]headMessage(nil), core.headQueue...)
			core.headMu.Unlock()
			if len(queued) != 1 || !strings.Contains(queued[0].text, "id: live") || !strings.Contains(queued[0].text, "agent b\n") {
				t.Fatalf("other conflict missing from head prompt: %+v", queued)
			}
			if scenario == "released" {
				if _, err := os.Stat(first.Worktree); !os.IsNotExist(err) {
					t.Fatalf("released worktree was recreated: %v", err)
				}
			}
		})
	}
}

func TestResolveConflictsRechecksStoredReason(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "new.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	path := filepath.Join(dir, ch.Path)
	if err := os.WriteFile(path, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.acceptChange(ch.ID); err == nil {
		t.Fatal("accept must conflict with the existing workspace file")
	}
	stored, _ := core.Coord.FindChange(ch.ID)
	if stored.Conflict == "" || stored.Conflict == overlapReason {
		t.Fatalf("expected a stored non-overlap reason, got %q", stored.Conflict)
	}
	core.headMu.Lock()
	core.headBusy = true
	core.headMu.Unlock()
	if res, err := core.resolveConflicts(); err != nil || res.(map[string]any)["conflicted"] != 1 {
		t.Fatalf("first resolve: result=%+v error=%v", res, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	res, err := core.resolveConflicts()
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	m := res.(map[string]any)
	if m["conflicted"] != 0 || m["prompted"] != false {
		t.Fatalf("clean recheck result = %+v", m)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "proposed" || now.Conflict != "" || now.Resolving || now.Integration == nil || now.Integration.State != "clean" {
		t.Fatalf("clean recheck kept conflict state: change=%+v", now)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("resolve wrote the workspace file: %v", err)
	}
}

func TestNewProposalSupersedesConflictedChange(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	old := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.resolveConflicts(); err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	s := core.Coord.Current()
	s.Changes = append(s.Changes, orchestrator.Change{ID: "chg2", AgentID: old.AgentID, Path: "a.txt", Kind: "modified", Status: "proposed", Worktree: old.Worktree})
	core.Coord.Restore(s)
	core.supersedeStale()
	now, _ := core.Coord.FindChange(old.ID)
	if now.Status != "rejected" {
		t.Fatalf("old conflicted change status = %q, want rejected", now.Status)
	}
	if now.SupersededBy != "chg2" || now.Conflict != "" {
		t.Fatalf("superseded = %q conflict = %q, want chg2 and no conflict", now.SupersededBy, now.Conflict)
	}
}

func TestCheckChangeClassifiesNonTextChanges(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n", "bin.dat": "\x00old"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(ch.Worktree, "new.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ch.Worktree, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ch.Worktree, "bin.dat"), []byte("\x00new"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ch.Worktree, "link.txt")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path, kind, want string
	}{
		{"added", "new.txt", "added", ""},
		{"deleted", "gone.txt", "deleted", ""},
		{"binary", "bin.dat", "modified", ""},
		{"symlink", "link.txt", "added", "conflicted"},
		{"protected", ".git/config", "modified", "conflicted"},
	}
	for _, c := range cases {
		core.Coord.UpdateChange(ch.ID, func(cur *orchestrator.Change) { cur.Conflict = "" })
		change := orchestrator.Change{ID: ch.ID, AgentID: ch.AgentID, Path: c.path, Kind: c.kind, Status: "proposed", Worktree: ch.Worktree}
		outcome, err := core.checkChange(change)
		if err != nil {
			t.Fatalf("%s: checkChange: %v", c.name, err)
		}
		got, _ := core.Coord.FindChange(ch.ID)
		if c.want == "" && (outcome != "clean" || got.Conflict != "") {
			t.Fatalf("%s: outcome=%q conflict=%q, want clean", c.name, outcome, got.Conflict)
		}
		if c.want != "" && (outcome != c.want || got.Conflict == "") {
			t.Fatalf("%s: outcome=%q conflict=%q, want %s", c.name, outcome, got.Conflict, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("conflict check wrote new.txt into the workspace: %v", err)
	}
}

func TestCheckChangeBinaryOverlapConflicts(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"bin.dat": "\x00old"})
	ch := proposeChange(t, core, dir, "bin.dat", func(p string) {
		if err := os.WriteFile(p, []byte("\x00agent"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("\x00mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome, err := core.checkChange(ch)
	if err != nil {
		t.Fatalf("checkChange: %v", err)
	}
	if got, _ := core.Coord.FindChange(ch.ID); outcome != "conflicted" || got.Conflict != overlapReason {
		t.Fatalf("binary overlap: outcome=%q conflict=%q", outcome, got.Conflict)
	}
}

func TestRejectClearsConflictState(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.resolveConflicts(); err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	if _, err := core.rejectChange(ch.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "rejected" || now.Conflict != "" || now.Resolving {
		t.Fatalf("rejected change keeps flags: status=%q conflict=%q resolving=%v", now.Status, now.Conflict, now.Resolving)
	}
}

func TestClearResolvingKeepsConflictForRetry(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.resolveConflicts(); err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	core.clearResolving(core.Coord.SessionID())
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Resolving || now.Conflict == "" {
		t.Fatalf("after head turn: resolving=%v conflict=%q, want resolving cleared and conflict kept", now.Resolving, now.Conflict)
	}
}

func TestResolveConflictsPinsHeadToSession(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origin := core.Coord.SessionID()
	core.headMu.Lock()
	core.headBusy = true
	core.headMu.Unlock()
	if _, err := core.resolveConflicts(); err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	core.headMu.Lock()
	queued := append([]headMessage(nil), core.headQueue...)
	core.headBusy = false
	core.headMu.Unlock()
	if len(queued) != 1 || queued[0].session != origin {
		t.Fatalf("queued head = %+v, want one message for %q", queued, origin)
	}
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	if core.headReady(origin) {
		t.Fatalf("head for %q deliverable while session %q is current", origin, core.Coord.SessionID())
	}
	if !core.headReady(core.Coord.SessionID()) {
		t.Fatalf("idle current session should accept its own head")
	}
}

func TestResolveConflictsSkipsCleanChangesInSessionSet(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.resolveConflicts(); err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.resolveConflicts(); err != nil {
		t.Fatalf("second resolveConflicts: %v", err)
	}
	now, _ := core.Coord.FindChange(proposedID(core))
	if now.Integration == nil || now.Integration.State != "clean" || now.Conflict != "" {
		t.Fatalf("clean recheck left conflict state: %+v", now)
	}
}

// proposedID returns the id of the only proposed change in the current session.
func proposedID(a *App) string {
	for _, ch := range a.Coord.Changes() {
		if ch.Status == "proposed" {
			return ch.ID
		}
	}
	return ""
}

// The projection must survive a restart: it is stored in the session payload, not only in memory.
func TestIntegrationPersistsAndReloads(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core.headMu.Lock()
	core.headBusy = true
	core.headMu.Unlock()
	if _, err := core.resolveConflicts(); err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	core.persist()
	sid := core.Coord.Current().ID
	var stored orchestrator.Session
	if err := core.Store.Get("sessions", sid, &stored); err != nil {
		t.Fatalf("read stored session: %v", err)
	}
	core.Coord.Restore(&stored)
	got, _ := core.Coord.FindChange(ch.ID)
	if got.Integration == nil {
		t.Fatal("the stored change lost its Integration")
	}
	if got.Integration.State != "conflicted" || got.Integration.Kind != "overlap" {
		t.Fatalf("integration = %+v, want conflicted/overlap", got.Integration)
	}
	if len(got.Integration.Hunks) == 0 {
		t.Fatalf("integration has no hunks: %+v", got.Integration)
	}
	if got.Integration.Ours == "" || got.Integration.CheckedAt == 0 {
		t.Fatalf("integration lacks the workspace blob or check time: %+v", got.Integration)
	}
}

// A failure that is not a missing worktree must still store a kind and a check time.
func TestCheckChangeNonWorktreeErrorStoresKind(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	ch.AgentID = "invalid/agent"
	s := core.Coord.Current()
	s.Changes = []orchestrator.Change{ch}
	core.Coord.Restore(s)
	if _, err := core.checkChange(ch); err != nil {
		t.Fatalf("checkChange: %v", err)
	}
	got, _ := core.Coord.FindChange(ch.ID)
	if got.Integration == nil || got.Integration.Kind != "check_failed" || got.Integration.CheckedAt == 0 {
		t.Fatalf("integration = %+v, want a kind and a check time", got.Integration)
	}
	if !strings.Contains(got.Integration.Reason, "invalid agent id") {
		t.Fatalf("reason = %q, want the underlying error", got.Integration.Reason)
	}
}

// Rows written before Integration existed must still load with a classified kind.
func TestLegacyConflictStringMapsToKind(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	s := core.Coord.Current()
	s.Changes = []orchestrator.Change{
		{ID: "old-overlap", Path: "a.txt", Status: "proposed", Conflict: "your edits and the agent's edits overlap"},
		{ID: "old-delete", Path: "a.txt", Status: "proposed", Conflict: "you changed this file after the agent started, and the agent deleted it"},
		{ID: "old-add", Path: "a.txt", Status: "proposed", Conflict: "the file exists in your workspace and the agent created a different one"},
	}
	core.Coord.Restore(s)
	for id, want := range map[string]string{
		"old-overlap": "overlap",
		"old-delete":  "modify_delete",
		"old-add":     "add_add",
	} {
		got, _ := core.Coord.FindChange(id)
		if got.Integration == nil || got.Integration.Kind != want || got.Integration.State != "conflicted" {
			t.Fatalf("%s: integration = %+v, want conflicted/%s", id, got.Integration, want)
		}
		if got.Integration.Reason != got.Conflict {
			t.Fatalf("%s: reason = %q, want the legacy text %q", id, got.Integration.Reason, got.Conflict)
		}
	}
}
