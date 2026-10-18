package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/orchestrator"
)

// queuedHeadText returns the head messages waiting to be submitted.
func queuedHeadText(core *App) []string {
	core.headMu.Lock()
	defer core.headMu.Unlock()
	var out []string
	for _, m := range core.headQueue {
		out = append(out, m.text)
	}
	return out
}

// captureNotes records the system notes posted to the chat.
func captureNotes(core *App) *[]string {
	var notes []string
	core.Coord.SetEmitter(func(name string, payload any) {
		if name == "chat.message" {
			if m, ok := payload.(orchestrator.Chat); ok && m.Role == "system" {
				notes = append(notes, m.Text)
			}
		}
	})
	return &notes
}

// holdHead keeps the head queue from being drained, so the test can read what would have been sent.
func holdHead(core *App) {
	core.headMu.Lock()
	core.headBusy = true
	core.headMu.Unlock()
}

// realConflict proposes an agent edit to rel and the user's edit to the same line, so projection reports a conflict.
func realConflict(t *testing.T, core *App, dir, rel string) orchestrator.Change {
	t.Helper()
	ch := proposeChange(t, core, dir, rel, func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ch
}

// A conflict on an ignored path is rejected before any head prompt, with a reason and its agent copy retained.
func TestAutoRejectIgnoredConflictNeverReachesHead(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		name := "legacy"
		if lazy {
			name = "lazy"
		}
		t.Run(name, func(t *testing.T) {
			core, dir := newGitCore(t, map[string]string{"out.txt": "one\ntwo\n"})
			if lazy {
				enableLazyConflicts(core)
			}
			core.Coord.WS.SetIgnore([]string{"out.txt"})
			ch := realConflict(t, core, dir, "out.txt")
			holdHead(core)

			res, err := core.resolveConflicts()
			if err != nil {
				t.Fatalf("resolveConflicts: %v", err)
			}
			m := res.(map[string]any)
			if m["autoRejected"] != 1 || m["conflicted"] != 0 || m["prompted"] != false {
				t.Fatalf("result = %+v, want autoRejected 1, conflicted 0, prompted false", m)
			}
			if got := queuedHeadText(core); len(got) != 0 {
				t.Fatalf("ignored conflict was sent to the head: %q", got)
			}
			now, _ := core.Coord.FindChange(ch.ID)
			if now.Status != "rejected" || now.Resolving || !strings.Contains(now.Reason, "ignored") {
				t.Fatalf("status=%q resolving=%v reason=%q", now.Status, now.Resolving, now.Reason)
			}
			refs := gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes")
			if !strings.Contains(refs, "-"+ch.ID) {
				t.Fatalf("rejected change kept no blob ref: %q", refs)
			}
		})
	}
}

// A protected-kind conflict is rejected before any head prompt. The stored projection is kept fresh so the
// conflict is the one the head would have seen.
func TestAutoRejectProtectedConflictNeverReachesHead(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	ch := realConflict(t, core, dir, "a.txt")
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	core.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) {
		c.Conflict = "protected metadata path"
		c.Integration = &orchestrator.Integration{
			State: "conflicted", Kind: integration.KindProtected, Reason: "protected metadata path",
			Ours: integration.BlobID(data, true),
		}
	})
	enableLazyConflicts(core)
	holdHead(core)

	res, err := core.resolveConflicts()
	if err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	m := res.(map[string]any)
	if m["autoRejected"] != 1 || m["prompted"] != false {
		t.Fatalf("result = %+v, want autoRejected 1 and prompted false", m)
	}
	if got := queuedHeadText(core); len(got) != 0 {
		t.Fatalf("protected conflict was sent to the head: %q", got)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "rejected" || !strings.Contains(now.Reason, "protected") {
		t.Fatalf("status=%q reason=%q", now.Status, now.Reason)
	}
}

// The head is not prompted while the working tree cannot be recorded. The conflicts are left unflagged and a note
// says why.
func TestSnapshotGateBlocksHeadPrompt(t *testing.T) {
	for _, tc := range []struct{ fault, want string }{
		{"fallback", "fell back to HEAD"},
		{"fail", "Snapshot failed"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
			enableLazyConflicts(core)
			ch := realConflict(t, core, dir, "a.txt")
			holdHead(core)
			notes := captureNotes(core)
			core.Coord.WS.SetSnapshotFault(tc.fault)
			t.Cleanup(func() { core.Coord.WS.SetSnapshotFault("") })

			res, err := core.resolveConflicts()
			if err != nil {
				t.Fatalf("resolveConflicts: %v", err)
			}
			m := res.(map[string]any)
			if m["prompted"] != false || m["conflicted"] != 0 || !strings.Contains(m["blocked"].(string), tc.want) {
				t.Fatalf("result = %+v, want blocked containing %q", m, tc.want)
			}
			if got := queuedHeadText(core); len(got) != 0 {
				t.Fatalf("head was prompted on a failed snapshot: %q", got)
			}
			now, _ := core.Coord.FindChange(ch.ID)
			if now.Resolving || now.Status != "proposed" || now.Conflict == "" {
				t.Fatalf("blocked change: status=%q resolving=%v conflict=%q", now.Status, now.Resolving, now.Conflict)
			}
			if joined := strings.Join(*notes, "\n"); !strings.Contains(joined, "cannot record your working tree") {
				t.Fatalf("no note about the working tree: %q", joined)
			}
		})
	}
}

// A healthy snapshot lets the conflict prompt through.
func TestSnapshotGateAllowsHealthyPrompt(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	enableLazyConflicts(core)
	realConflict(t, core, dir, "a.txt")
	holdHead(core)
	notes := captureNotes(core)

	res, err := core.resolveConflicts()
	if err != nil {
		t.Fatalf("resolveConflicts: %v", err)
	}
	m := res.(map[string]any)
	if m["prompted"] != true || m["conflicted"] != 1 {
		t.Fatalf("healthy result = %+v", m)
	}
	if got := queuedHeadText(core); len(got) != 1 {
		t.Fatalf("healthy snapshot queued %d head messages, want 1", len(got))
	}
	if len(*notes) != 0 {
		t.Fatalf("healthy snapshot posted notes: %q", *notes)
	}
}

// On a takeLatest path the older proposal is superseded by the newest clean one, even when it was not conflicted.
// Without the policy the older proposal is left alone.
func TestSupersedeTakeLatestPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     integration.Policy
		wantStatus string
	}{
		{"takeLatest", integration.Policy{TakeLatest: []string{"a.txt"}}, "rejected"},
		{"ordinary", integration.DefaultPolicy(), "proposed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			integration.SetPolicy(tc.policy)
			t.Cleanup(func() { integration.SetPolicy(integration.DefaultPolicy()) })
			core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
			old := proposeChange(t, core, dir, "a.txt", func(p string) {
				if err := os.WriteFile(p, []byte("agent old\ntwo\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			})
			s := core.Coord.Current()
			s.Changes = append(s.Changes, orchestrator.Change{ID: "chg2", AgentID: old.AgentID, Path: "a.txt", Kind: "modified", Status: "proposed", Worktree: old.Worktree})
			core.Coord.Restore(s)

			core.supersedeStale()

			now, _ := core.Coord.FindChange(old.ID)
			if now.Status != tc.wantStatus {
				t.Fatalf("older clean change status = %q, want %q", now.Status, tc.wantStatus)
			}
			if tc.wantStatus == "rejected" && (now.SupersededBy != "chg2" || !now.Superseded) {
				t.Fatalf("superseded = %v by %q, want true by chg2", now.Superseded, now.SupersededBy)
			}
		})
	}
}

// The head prompt asks for one small task per change and shows the current workspace hunks next to the agent's.
func TestConflictPromptShowsCurrentHunkAndSmallTaskInstruction(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	enableLazyConflicts(core)
	realConflict(t, core, dir, "a.txt")
	core.projectPaths([]string{"a.txt"})
	now, _ := core.Coord.FindChange("chg1")
	if now.Integration == nil || now.Integration.State != "conflicted" {
		t.Fatalf("integration = %+v, want conflicted", now.Integration)
	}
	prompt := core.conflictPrompt([]orchestrator.Change{now})
	for _, want := range []string{"ONE small task", "current version (only the conflicting hunks", "mine\ntwo\n", "agent version", "agent\ntwo\n", "resolvedChanges"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
