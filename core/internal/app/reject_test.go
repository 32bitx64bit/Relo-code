package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/orchestrator"
	"github.com/relocode/relo/internal/workspace"
)

// buildRejectChanges creates n proposed changes spread over a agents, each with its own file.
func buildRejectChanges(t *testing.T, core *App, dir string, n, a int) {
	t.Helper()
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	s := core.Coord.Current()
	trees := make([]string, a)
	for i := 0; i < a; i++ {
		tree, err := mountTree(core, fmt.Sprintf("agent_%d", i))
		if err != nil {
			t.Fatal(err)
		}
		trees[i] = tree.Path
	}
	for i := 0; i < n; i++ {
		agent := i % a
		rel := fmt.Sprintf("f%d.txt", i)
		if err := os.WriteFile(filepath.Join(trees[agent], rel), []byte(fmt.Sprintf("agent %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		s.Changes = append(s.Changes, orchestrator.Change{
			ID: fmt.Sprintf("chg%d", i), AgentID: fmt.Sprintf("agent_%d", agent), Path: rel, Kind: "modified",
			Status: "proposed", Worktree: trees[agent],
		})
	}
	core.Coord.Restore(s)
}

// A rejected change keeps its agent bytes under refs/relo/changes/<stamp>-<id> after the worktree is gone.
func TestRejectRetainsBlobUnderChangeRef(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := core.rejectChange(ch.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := os.Stat(ch.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree should be released after reject: %v", err)
	}
	refs := strings.Fields(gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes"))
	var ref string
	for _, r := range refs {
		if strings.HasSuffix(r, "-"+ch.ID) {
			ref = r
		}
	}
	if ref == "" {
		t.Fatalf("rejected change has no retained blob ref: %v", refs)
	}
	if body := gitText(t, dir, "cat-file", "-p", ref); body != "agent" {
		t.Fatalf("retained blob = %q, want %q", body, "agent")
	}
}

// Reject-all must not run git while decisionMu is held. Blob retention, discard, release and persistence all
// run after the unlock.
func TestRejectAllRunsGitOutsideDecisionMu(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	buildRejectChanges(t, core, dir, 12, 3)

	var mu sync.Mutex
	var locked []string
	workspace.GitProbe = func(args []string) {
		if core.decisionMu.TryLock() {
			core.decisionMu.Unlock()
			return
		}
		mu.Lock()
		locked = append(locked, strings.Join(args, " "))
		mu.Unlock()
	}
	defer func() { workspace.GitProbe = nil }()

	out, err := core.rejectAll()
	if err != nil {
		t.Fatalf("rejectAll: %v", err)
	}
	if done := out.(map[string]any)["done"]; done != 12 {
		t.Fatalf("done = %v, want 12", done)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(locked) != 0 {
		t.Fatalf("%d git calls under decisionMu: %v", len(locked), locked)
	}
	refs := gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes")
	if n := len(strings.Fields(refs)); n != 12 {
		t.Fatalf("retained %d blob refs, want 12", n)
	}
}

// Reject-all marks the whole batch in one state update, not one event per change.
// Reject-all marks the whole batch in one state update, not one event per change.
func TestRejectAllMarksBatchInOneEvent(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	buildRejectChanges(t, core, dir, 8, 2)
	var emits int
	core.Coord.SetEmitter(func(name string, payload any) {
		if name == "changes.updated" {
			emits++
		}
	})
	if _, err := core.rejectAll(); err != nil {
		t.Fatal(err)
	}
	if emits != 1 {
		t.Fatalf("rejectAll emitted changes.updated %d times, want 1", emits)
	}
	if now, _ := core.Coord.FindChange("chg0"); now.Status != "rejected" {
		t.Fatalf("status = %q", now.Status)
	}
}

func TestRejectRetentionFailureKeepsWorktreeAndNotes(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	injected := errors.New("injected retention failure")
	core.keepBlobs = func([]workspace.BlobRef) error { return injected }
	core.keepBlob = func(string, string, string) error { return injected }
	var notes []string
	core.Coord.SetEmitter(func(name string, payload any) {
		if name == "chat.message" {
			if m, ok := payload.(orchestrator.Chat); ok && m.Role == "system" {
				notes = append(notes, m.Text)
			}
		}
	})

	if _, err := core.rejectChange(ch.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Fail closed: the agent bytes and branch stay recoverable in the worktree.
	if _, err := os.Stat(filepath.Join(ch.Worktree, "a.txt")); err != nil {
		t.Fatalf("worktree file was discarded after a failed retention: %v", err)
	}
	if isVirtual(core, ch.AgentID) {
		// The agent's work is held by its overlay ref until the workspace is released, not by a branch.
		gitText(t, dir, "show-ref", "--verify", "refs/relo/vws/agent_t1/overlay")
	} else {
		gitText(t, dir, "show-ref", "--verify", "refs/heads/relo/agent_t1")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "could not be kept") || !strings.Contains(joined, ch.ID) {
		t.Fatalf("retention failure has no visible note: %q", joined)
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "rejected" {
		t.Fatalf("status = %q, want rejected", now.Status)
	}
}

// A failed retention holds only the affected change: the other rejections still discard and release.
func TestRejectRetentionFailureDoesNotBlockOtherChanges(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	buildRejectChanges(t, core, dir, 2, 2)
	injected := errors.New("injected retention failure")
	core.keepBlobs = func([]workspace.BlobRef) error { return injected }
	core.keepBlob = func(changeID, worktree, path string) error {
		if changeID == "chg1" {
			return injected
		}
		return core.Coord.WS.KeepChangeBlob(changeID, worktree, path)
	}

	if _, err := core.rejectAll(); err != nil {
		t.Fatalf("rejectAll: %v", err)
	}

	ch0, _ := core.Coord.FindChange("chg0")
	ch1, _ := core.Coord.FindChange("chg1")
	if _, err := os.Stat(ch0.Worktree); !os.IsNotExist(err) {
		t.Fatalf("retained change's worktree should be released: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ch1.Worktree, "f1.txt")); err != nil {
		t.Fatalf("failed change's worktree file should survive: %v", err)
	}
	refs := strings.Fields(gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes"))
	found := false
	for _, ref := range refs {
		if strings.HasSuffix(ref, "-chg0") {
			found = true
		}
	}
	if !found {
		t.Fatalf("retained change has no blob ref: %v", refs)
	}
}

// The decision must be recorded without waiting for cleanupMu; the caller holds cleanupMu while another
// reject decision completes.
func TestRejectDecisionDoesNotWaitForCleanupMu(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	core.cleanupMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := core.rejectChange(ch.ID)
		done <- err
	}()
	waitForStatus(t, core, ch.ID, "rejected")
	select {
	case err := <-done:
		t.Fatalf("reject returned while cleanupMu was held: %v", err)
	default:
	}
	core.cleanupMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reject: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reject did not finish after cleanupMu was released")
	}
}

func TestAcceptDecisionDoesNotWaitForCleanupMu(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	core.cleanupMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := core.acceptChange(ch.ID)
		done <- err
	}()
	waitForStatus(t, core, ch.ID, "accepted")
	select {
	case err := <-done:
		t.Fatalf("accept returned while cleanupMu was held: %v", err)
	default:
	}
	core.cleanupMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept did not finish after cleanupMu was released")
	}
}

func waitForStatus(t *testing.T, core *App, id, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if now, _ := core.Coord.FindChange(id); now.Status == status {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	now, _ := core.Coord.FindChange(id)
	t.Fatalf("change %s status = %q, want %q", id, now.Status, status)
}

// A concurrent accept must not release the worktree while a reject is still retaining its blob.
func TestRejectRetentionCompletesBeforeAcceptReleasesWorktree(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	buildRejectChanges(t, core, dir, 2, 1)
	ch0, _ := core.Coord.FindChange("chg0")
	ch1, _ := core.Coord.FindChange("chg1")
	entered := make(chan struct{})
	release := make(chan struct{})
	core.keepBlobs = func(refs []workspace.BlobRef) error {
		close(entered)
		<-release
		return core.Coord.WS.KeepChangeBlobs(refs)
	}

	rejectDone := make(chan error, 1)
	go func() {
		_, err := core.rejectChange("chg1")
		rejectDone <- err
	}()
	<-entered
	acceptDone := make(chan error, 1)
	go func() {
		_, err := core.acceptChange("chg0")
		acceptDone <- err
	}()
	waitForStatus(t, core, "chg0", "accepted")
	if _, err := os.Stat(ch1.Worktree); err != nil {
		t.Fatalf("accept released the worktree before reject retention finished: %v", err)
	}
	select {
	case err := <-acceptDone:
		t.Fatalf("accept released the agent before retention finished: %v", err)
	default:
	}
	close(release)
	if err := <-rejectDone; err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := <-acceptDone; err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := os.Stat(ch0.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree should be released after both decisions: %v", err)
	}
	refs := strings.Fields(gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes"))
	found := false
	for _, ref := range refs {
		if strings.HasSuffix(ref, "-chg1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejected change has no retained blob ref: %v", refs)
	}
}

// Resolving a batch discards every id in one state update, so the UI gets one changes.updated event.
func TestResolveHeadChangesEmitsOneEvent(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "base\n"})
	buildRejectChanges(t, core, dir, 3, 3)
	ids := []string{"chg0", "chg1", "chg2"}
	for _, id := range ids {
		markConflicted(t, core, id)
	}
	var emits int
	core.Coord.SetEmitter(func(name string, payload any) {
		if name == "changes.updated" {
			emits++
		}
	})
	core.resolveHeadChanges(ids)
	if emits != 1 {
		t.Fatalf("resolve emitted changes.updated %d times, want 1", emits)
	}
	for _, id := range ids {
		if now, _ := core.Coord.FindChange(id); now.Status != "rejected" {
			t.Fatalf("%s status = %q, want rejected", id, now.Status)
		}
	}
}
