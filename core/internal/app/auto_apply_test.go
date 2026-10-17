package app

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/orchestrator"
)

func waitAutoApply(t *testing.T, core *App) {
	t.Helper()
	core.autoMu.Lock()
	done := core.autoDone
	core.autoMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("auto-apply did not finish (possible recursive changes.updated loop)")
	}
}

func gitText(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAutoApplyDefaultOffAndCheckedOutBranch(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	git(t, dir, "checkout", "-b", "feature/auto-apply")
	head := gitText(t, dir, "rev-parse", "HEAD")
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	core.AttachIPC(ipc.New(strings.NewReader(""), io.Discard, core.Handle))
	core.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) {})
	core.autoMu.Lock()
	running := core.autoDone != nil
	core.autoMu.Unlock()
	if core.autoApply.Load() || running {
		t.Fatal("auto-apply must default off")
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "proposed" {
		t.Fatalf("default-off status = %q", now.Status)
	}
	core.applySettings(map[string]any{"autoApply": true})
	waitAutoApply(t, core)
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "accepted" {
		t.Fatalf("enabled status = %+v", now)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(body) != "agent\n" {
		t.Fatalf("workspace = %q", body)
	}
	git(t, dir, "diff", "--cached", "--exit-code")
	if got := gitText(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved: %s != %s", got, head)
	}
	if got := gitText(t, dir, "branch", "--show-current"); got != "feature/auto-apply" {
		t.Fatalf("branch changed: %s", got)
	}
}

func TestAutoApplySkipsBlockedAndMarksEveryFailure(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := core.Coord.Current()
	s.Changes = append(s.Changes,
		orchestrator.Change{ID: "conflict", Status: "proposed", Conflict: "review required"},
		orchestrator.Change{ID: "resolving", Status: "proposed", Resolving: true},
		orchestrator.Change{ID: "superseded", Status: "proposed", Superseded: true},
		orchestrator.Change{ID: "applied", Status: "applied"},
		orchestrator.Change{ID: "accepted", Status: "accepted"},
		orchestrator.Change{ID: "rejected", Status: "rejected"},
		orchestrator.Change{ID: "invalid", Status: "proposed"},
	)
	core.Coord.Restore(s)
	core.AttachIPC(ipc.New(strings.NewReader(""), io.Discard, core.Handle))
	core.applySettings(map[string]any{"autoApply": true})
	waitAutoApply(t, core)
	for _, before := range s.Changes {
		after, _ := core.Coord.FindChange(before.ID)
		if before.ID == ch.ID || before.ID == "invalid" {
			if after.Status != "proposed" || after.Conflict == "" {
				t.Fatalf("failed auto-accept = %+v", after)
			}
		} else if after.Status != before.Status || after.Conflict != before.Conflict || after.Resolving != before.Resolving || after.Superseded != before.Superseded {
			t.Fatalf("blocked change modified: before=%+v after=%+v", before, after)
		}
	}
	// Removing the local overlap must not retry the failed proposal, even after another event or settings update.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core.Coord.UpdateChange("applied", func(c *orchestrator.Change) {})
	core.applySettings(map[string]any{"autoApply": true})
	waitAutoApply(t, core)
	if after, _ := core.Coord.FindChange(ch.ID); after.Status != "proposed" || after.Conflict == "" {
		t.Fatalf("failed proposal retried: %+v", after)
	}
}

func TestAutoApplyCoalescesAndDrainsNewProposals(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	core.AttachIPC(ipc.New(strings.NewReader(""), io.Discard, core.Handle))
	core.decisionMu.Lock()
	core.autoApply.Store(true)
	core.scheduleAutoApply()
	core.autoMu.Lock()
	done := core.autoDone
	core.autoMu.Unlock()
	for i := 0; i < 100; i++ {
		core.scheduleAutoApply()
		core.autoMu.Lock()
		same := core.autoDone == done
		core.autoMu.Unlock()
		if !same {
			core.decisionMu.Unlock()
			t.Fatal("event burst started multiple workers")
		}
	}
	// An event from the accept path introduces another proposal while the worker is already draining.
	core.Coord.SetEmitter(func(name string, payload any) {
		if name != "changes.updated" {
			return
		}
		if updated, _ := core.Coord.FindChange(ch.ID); updated.Status == "accepted" {
			if _, exists := core.Coord.FindChange("late"); !exists {
				s := core.Coord.Current()
				s.Changes = append(s.Changes, orchestrator.Change{ID: "late", Status: "proposed"})
				core.Coord.Restore(s)
			}
		}
		core.scheduleAutoApply()
	})
	core.decisionMu.Unlock()
	waitAutoApply(t, core)
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "accepted" {
		t.Fatalf("proposal not accepted: %+v", now)
	}
	if late, ok := core.Coord.FindChange("late"); !ok || late.Status != "proposed" || late.Conflict == "" {
		t.Fatalf("event during drain lost: %+v", late)
	}
}

func TestAutoApplyChangesUpdatedHookAndDisable(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
			core.AttachIPC(ipc.New(strings.NewReader(""), io.Discard, core.Handle))
			core.applySettings(map[string]any{"autoApply": true})
			waitAutoApply(t, core)
			if !enabled {
				core.applySettings(map[string]any{"autoApply": false})
			}
			ch := proposeChange(t, core, dir, "a.txt", func(p string) {
				if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			})
			core.Coord.UpdateChange(ch.ID, func(c *orchestrator.Change) {})
			waitAutoApply(t, core)
			want := "proposed"
			if enabled {
				want = "accepted"
			}
			if now, _ := core.Coord.FindChange(ch.ID); now.Status != want {
				t.Fatalf("changes.updated status = %q, want %q", now.Status, want)
			}
		})
	}
}
