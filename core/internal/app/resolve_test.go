package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
)

// markConflicted puts a proposed change in the state the conflict prompt sends to the head.
func markConflicted(t *testing.T, core *App, id string) {
	t.Helper()
	core.Coord.UpdateChange(id, func(c *orchestrator.Change) {
		c.Conflict = overlapReason
		c.Integration = &orchestrator.Integration{State: "conflicted", Kind: "overlap", Reason: overlapReason}
		c.Resolving = true
	})
}

// A resolved change is discarded through the reject path: the workspace keeps its bytes, the agent copy is retained,
// and the change no longer looks conflicted.
func TestResolveHeadChangesDiscardsWithoutWriting(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	markConflicted(t, core, ch.ID)

	core.resolveHeadChanges([]string{ch.ID})

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "base\n" {
		t.Fatalf("resolving wrote workspace bytes: %q", got)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "rejected" || now.Conflict != "" || now.Resolving {
		t.Fatalf("resolved change = status %q conflict %q resolving %v", now.Status, now.Conflict, now.Resolving)
	}
	if now.Reason != "resolved by agent" {
		t.Fatalf("reason = %q, want %q", now.Reason, "resolved by agent")
	}
	if _, err := os.Stat(ch.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree should be released after resolve: %v", err)
	}
	refs := strings.Fields(gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes"))
	found := false
	for _, ref := range refs {
		if strings.HasSuffix(ref, "-"+ch.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolved change has no retained blob ref: %v", refs)
	}
}

// Only conflicted or resolving changes of the current session qualify; every other id gets a visible note and is left
// alone.
func TestResolveHeadChangesRefusesUnqualifiedIDs(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	var notes []string
	core.Coord.SetEmitter(func(name string, payload any) {
		if name == "chat.message" {
			if m, ok := payload.(orchestrator.Chat); ok && m.Role == "system" {
				notes = append(notes, m.Text)
			}
		}
	})

	core.resolveHeadChanges([]string{ch.ID, "chg_missing"})

	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "proposed" || now.Conflict != "" {
		t.Fatalf("unqualified change was decided: status %q conflict %q", now.Status, now.Conflict)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "base\n" {
		t.Fatalf("refusal wrote workspace bytes: %q", got)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, ch.ID) || !strings.Contains(joined, "chg_missing") {
		t.Fatalf("system note does not name the refused ids: %q", joined)
	}
}

// When a newer proposed change covers the same path, the discarded change records that supersession.
func TestResolveHeadChangesSetsSupersededBy(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	markConflicted(t, core, ch.ID)
	s := core.Coord.Current()
	s.Changes = append(s.Changes, orchestrator.Change{ID: "chg_new", AgentID: "agent_t2", Path: "a.txt", Status: "proposed"})
	core.Coord.Restore(s)

	core.resolveHeadChanges([]string{ch.ID})

	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "rejected" || now.SupersededBy != "chg_new" {
		t.Fatalf("resolved change = status %q supersededBy %q", now.Status, now.SupersededBy)
	}
}

// The conflict prompt names each change id and asks the head to list the resolved ones in the turn.
func TestConflictPromptAsksForResolvedChanges(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	markConflicted(t, core, ch.ID)
	prompt := core.conflictPrompt([]orchestrator.Change{ch})
	if !strings.Contains(prompt, ch.ID) || !strings.Contains(prompt, "resolvedChanges") {
		t.Fatalf("conflict prompt = %q", prompt)
	}
}
