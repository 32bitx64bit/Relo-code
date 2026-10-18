package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/integration"
	"github.com/relocode/relo/internal/ipc"
	"github.com/relocode/relo/internal/orchestrator"
)

// proposeFromAgent records a proposal for a named agent and writes the agent's version in its worktree.
func proposeFromAgent(t *testing.T, core *App, agentID, changeID, rel, body string) orchestrator.Change {
	t.Helper()
	tree, err := core.Coord.WS.EnsureTree(agentID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Path, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s := core.Coord.Current()
	s.Changes = append(s.Changes, orchestrator.Change{ID: changeID, AgentID: agentID, Path: rel, Kind: "modified", Status: "proposed", Worktree: tree.Path})
	core.Coord.Restore(s)
	ch, _ := core.Coord.FindChange(changeID)
	return ch
}

func enableLazyConflicts(core *App) {
	core.applySettings(map[string]any{"lazyConflicts": true})
}

// Accept A, then B's stored projection changes without a click and names A in Since.
func TestAcceptProjectsEveryOtherProposedChangeOnThePath(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	a := proposeFromAgent(t, core, "agent_a", "chgA", "a.txt", "a\ntwo\n")
	b := proposeFromAgent(t, core, "agent_b", "chgB", "a.txt", "b\ntwo\n")

	if _, err := core.acceptChange(a.ID); err != nil {
		t.Fatalf("accept A: %v", err)
	}
	now, _ := core.Coord.FindChange(b.ID)
	if now.Integration == nil || now.Integration.State != "conflicted" {
		t.Fatalf("B was not projected after accept A: %+v", now.Integration)
	}
	if len(now.Integration.Hunks) == 0 {
		t.Fatalf("B's projection has no hunks: %+v", now.Integration)
	}
	if !contains(now.Integration.Since, a.ID) {
		t.Fatalf("B.Since = %v, want it to name %s", now.Integration.Since, a.ID)
	}
}

// An undo is also an E1 event: the other proposed change on the path is projected again.
func TestUndoProjectsOtherProposedChangeOnThePath(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	a := proposeFromAgent(t, core, "agent_a", "chgA", "a.txt", "a\ntwo\n")
	b := proposeFromAgent(t, core, "agent_b", "chgB", "a.txt", "b\ntwo\n")
	if _, err := core.acceptChange(a.ID); err != nil {
		t.Fatalf("accept A: %v", err)
	}
	if now, _ := core.Coord.FindChange(b.ID); now.Integration == nil || now.Integration.State != "conflicted" {
		t.Fatalf("B before undo = %+v, want conflicted", now.Integration)
	}
	if _, err := core.undoChange(a.ID); err != nil {
		t.Fatalf("undo A: %v", err)
	}
	if now, _ := core.Coord.FindChange(b.ID); now.Integration == nil || now.Integration.State != "clean" {
		t.Fatalf("B after undo = %+v, want clean", now.Integration)
	}
}

// A stat-keyed memo hit must not run git merge-file again.
func TestProjectionMemoSkipsUnchangedBlob(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", "agent\ntwo\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core.projectPaths([]string{ch.Path})
	before := integration.AnalyzeCount.Load()
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	core.projectPaths([]string{ch.Path})
	if after := integration.AnalyzeCount.Load(); after != before {
		t.Fatalf("unchanged blob ran Analyze %d more times after a stat change", after-before)
	}
}

// Project must not write the workspace: it is a check, not an accept.
func TestProjectDoesNotWriteWorkspace(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", "agent\ntwo\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.txt")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	core.projectPaths([]string{ch.Path})
	core.projectPaths([]string{ch.Path})
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "mine\ntwo\n" {
		t.Fatalf("projection wrote the workspace: %q", body)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("projection changed the workspace stat: %v -> %v", before.ModTime(), after.ModTime())
	}
}

// Repeating a projection of unchanged state emits no changes.updated and cannot re-arm auto-apply.
func TestProjectionDoesNotEmitWhenNothingChanged(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", "agent\ntwo\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core.projectPaths([]string{ch.Path})
	var mu sync.Mutex
	emits := 0
	core.Coord.SetEmitter(func(name string, _ any) {
		if name == "changes.updated" {
			mu.Lock()
			emits++
			mu.Unlock()
		}
	})
	for i := 0; i < 5; i++ {
		core.projectPaths([]string{ch.Path})
	}
	mu.Lock()
	got := emits
	mu.Unlock()
	if got != 0 {
		t.Fatalf("%d unchanged projections emitted %d changes.updated events", 5, got)
	}
}

// The projection and SupersededBy are stored in the session payload, not only in memory.
func TestConflictStateAndSupersededBySurviveReload(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	a := proposeFromAgent(t, core, "agent_a", "chgA", "a.txt", "a\ntwo\n")
	b := proposeFromAgent(t, core, "agent_b", "chgB", "a.txt", "b\ntwo\n")
	if _, err := core.acceptChange(a.ID); err != nil {
		t.Fatalf("accept A: %v", err)
	}
	c := proposeFromAgent(t, core, "agent_c", "chgC", "a.txt", "c\ntwo\n")
	core.supersedeStale()

	old, _ := core.Coord.FindChange(b.ID)
	if old.SupersededBy != c.ID {
		t.Fatalf("SupersededBy = %q, want %q", old.SupersededBy, c.ID)
	}
	core.persist()
	sid := core.Coord.Current().ID
	var stored orchestrator.Session
	if err := core.Store.Get("sessions", sid, &stored); err != nil {
		t.Fatalf("read stored session: %v", err)
	}
	core.Coord.Restore(&stored)
	got, _ := core.Coord.FindChange(b.ID)
	if got.SupersededBy != c.ID {
		t.Fatalf("reloaded SupersededBy = %q, want %q", got.SupersededBy, c.ID)
	}
	if got.Integration == nil || got.Integration.State != "conflicted" {
		t.Fatalf("reloaded integration = %+v, want conflicted", got.Integration)
	}
	refs := gitText(t, dir, "for-each-ref", "--format=%(refname)", "refs/relo/changes")
	if !strings.Contains(refs, b.ID) {
		t.Fatalf("superseded change has no retained blob ref: %q", refs)
	}
}

// A workspace edit between preview and accept is refused and writes nothing.
func TestStalePreviewReturnsErrorAndWritesNothing(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	ch := proposeChange(t, core, dir, "a.txt", func(p string) {
		if err := os.WriteFile(p, []byte("agent\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	analysis, err := core.analyzeChange(ch)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = core.acceptChangeExpect(ch.ID, analysis.Fingerprint)
	var fail ipc.Fail
	if err == nil || !errors.As(err, &fail) || fail.C != "stale_preview" {
		t.Fatalf("accept with stale preview: err = %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(body) != "mine\ntwo\n" {
		t.Fatalf("stale accept wrote the workspace: %q", body)
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "proposed" {
		t.Fatalf("stale accept changed the status to %q", now.Status)
	}
}

// The head prompt shows only conflicting hunks with context for a large file, not the 4000-byte head.
func TestConflictPromptUsesConflictingHunksOnly(t *testing.T) {
	var lines []string
	for i := 0; i < 300; i++ {
		lines = append(lines, "line-"+pad3(i)+"-"+strings.Repeat("x", 30))
	}
	base := strings.Join(lines, "\n") + "\n"
	core, dir := newGitCore(t, map[string]string{"a.txt": base})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	agentLines := append([]string(nil), lines...)
	agentLines[150] = "line-150-by-the-agent"
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", strings.Join(agentLines, "\n")+"\n")
	userLines := append([]string(nil), lines...)
	userLines[150] = "line-150-by-the-user"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Join(userLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core.projectPaths([]string{ch.Path})
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Integration == nil || now.Integration.State != "conflicted" {
		t.Fatalf("integration = %+v, want conflicted", now.Integration)
	}
	prompt := core.conflictPrompt([]orchestrator.Change{now})
	if len(prompt) >= promptDiffBytes {
		t.Fatalf("hunk-scoped prompt is %d bytes, want less than %d", len(prompt), promptDiffBytes)
	}
	if !strings.Contains(prompt, "only the conflicting hunks") {
		t.Fatalf("prompt does not say it is hunk-scoped:\n%s", prompt)
	}
	if strings.Contains(prompt, "line-000-") {
		t.Fatalf("prompt contains a clean hunk:\n%s", prompt)
	}
	if !strings.Contains(prompt, "line-150-by-the-agent") {
		t.Fatalf("prompt omits the conflicting hunk:\n%s", prompt)
	}
}

// choice=agent is explicit, journaled, and undo restores the user's bytes.
func TestResolveChoiceAgentJournalsAndWrites(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", "agent\ntwo\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core.projectPaths([]string{ch.Path})
	if _, err := core.resolveChange(ch.ID, "agent"); err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(body) != "agent\ntwo\n" {
		t.Fatalf("workspace = %q, want the agent's bytes", body)
	}
	if now, _ := core.Coord.FindChange(ch.ID); now.Status != "accepted" || now.Landed != "updated" {
		t.Fatalf("resolved change = %+v", now)
	}
	if _, err := core.undoChange(ch.ID); err != nil {
		t.Fatalf("undo: %v", err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(body) != "mine\ntwo\n" {
		t.Fatalf("undo restored %q, want the pre-image", body)
	}
}

// Auto-apply reads the stored projection state, not only the legacy string.
func TestAutoEligibleUsesIntegrationState(t *testing.T) {
	conflicted := orchestrator.Change{Status: "proposed", Integration: &orchestrator.Integration{State: "conflicted"}}
	if autoEligible(conflicted) {
		t.Fatal("a conflicted projection must not be auto-eligible")
	}
	blocked := orchestrator.Change{Status: "proposed", Integration: &orchestrator.Integration{State: "blocked"}}
	if autoEligible(blocked) {
		t.Fatal("a blocked projection must not be auto-eligible")
	}
	clean := orchestrator.Change{Status: "proposed", Integration: &orchestrator.Integration{State: "clean"}}
	if !autoEligible(clean) {
		t.Fatal("a clean projection must be auto-eligible")
	}
	superseded := orchestrator.Change{Status: "proposed", SupersededBy: "new", Integration: &orchestrator.Integration{State: "clean"}}
	if autoEligible(superseded) {
		t.Fatal("a superseded change must not be auto-eligible")
	}
}

// An accept that landed before this change's base was cut is already in BaseRev and must not be named in Since.
func TestSinceExcludesAcceptsBeforeCut(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	a := proposeFromAgent(t, core, "agent_a", "chgA", "a.txt", "a\ntwo\n")
	if _, err := core.acceptChange(a.ID); err != nil {
		t.Fatalf("accept A: %v", err)
	}
	// Move the cut forward: an agent started now is based on the tree that already holds A's accept.
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot after accept A: %v", err)
	}
	b := proposeFromAgent(t, core, "agent_b", "chgB", "a.txt", "b\ntwo\n")
	core.projectPaths([]string{b.Path})
	now, _ := core.Coord.FindChange(b.ID)
	if now.Integration == nil {
		t.Fatal("B has no projection")
	}
	if contains(now.Integration.Since, a.ID) {
		t.Fatalf("B.Since = %v, want it to exclude %s: that accept is part of B's cut", now.Integration.Since, a.ID)
	}
}

// A change carried over without a projection (for example a row written before P3) is projected before accept-all
// writes any member. The pre-write projection's Ours and Fingerprint survive the merge; a write without it leaves
// both empty.
func TestAcceptAllProjectsNilIntegrationBeforeAnyWrite(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", "agent\ntwo\n")
	if ch.Integration != nil {
		t.Fatalf("fixture must start without a projection: %+v", ch.Integration)
	}
	if _, err := core.acceptAll(false); err != nil {
		t.Fatalf("accept all: %v", err)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "accepted" {
		t.Fatalf("status = %q, want accepted (failed: %+v)", now.Status, now.Integration)
	}
	if now.Integration == nil || now.Integration.Ours == "" || now.Integration.Fingerprint == "" {
		t.Fatalf("accept-all wrote without projecting first: %+v", now.Integration)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(body) != "agent\ntwo\n" {
		t.Fatalf("workspace = %q, want the agent's bytes", body)
	}
}

// A nil-Integration change that projects conflicted is left proposed and nothing is written.
func TestAcceptAllSkipsNilIntegrationThatProjectsConflicted(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", "agent\ntwo\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := core.acceptAll(false)
	if err != nil {
		t.Fatalf("accept all: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(body) != "mine\ntwo\n" {
		t.Fatalf("accept-all wrote a conflicted change: %q", body)
	}
	now, _ := core.Coord.FindChange(ch.ID)
	if now.Status != "proposed" || now.Integration == nil || now.Integration.State != "conflicted" {
		t.Fatalf("change = %+v, want proposed with a conflicted projection", now)
	}
	result, _ := out.(map[string]any)
	if failed, _ := result["failed"].([]bulkFailure); len(failed) != 1 || failed[0].ID != ch.ID {
		t.Fatalf("failed = %+v, want the conflicted change", result["failed"])
	}
}

// Projection takes the EnsureTree(false) path but is a check: neither the workspace nor the agent worktree changes.
func TestProjectChangesNoWorkspaceOrAgentTreeBytes(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	ch := proposeFromAgent(t, core, "agent_a", "chg1", "a.txt", "agent\ntwo\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("mine\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceBefore := treeBytes(t, dir)
	agentBefore := treeBytes(t, ch.Worktree)
	core.projectPaths([]string{ch.Path})
	core.projectPaths([]string{ch.Path})
	if now, _ := core.Coord.FindChange(ch.ID); now.Integration == nil || now.Integration.State != "conflicted" {
		t.Fatalf("projection did not run: %+v", now.Integration)
	}
	if got := treeBytes(t, dir); !sameTree(workspaceBefore, got) {
		t.Fatal("projection changed a workspace file")
	}
	if got := treeBytes(t, ch.Worktree); !sameTree(agentBefore, got) {
		t.Fatal("projection changed the agent tree")
	}
}

// treeBytes maps each regular file under root to its bytes, skipping .git and Relo's own metadata folder.
func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" || (d.IsDir() && d.Name() == ".relo") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for path, body := range a {
		if b[path] != body {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func pad3(n int) string {
	s := []byte{'0', '0', '0'}
	for i := 2; i >= 0 && n > 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}
