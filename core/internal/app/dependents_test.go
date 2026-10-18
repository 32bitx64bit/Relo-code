package app

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
)

func enableDependentSemantics(core *App) {
	core.applySettings(map[string]any{"dependentSemantics": true})
}

func patchHash(t *testing.T, core *App, agentID, rel string) string {
	t.Helper()
	patch, err := core.Coord.WS.PatchPath(agentID, rel)
	if err != nil {
		t.Fatalf("patch %s/%s: %v", agentID, rel, err)
	}
	sum := sha256.Sum256([]byte(patch))
	return hex.EncodeToString(sum[:])
}

// proposeDependent records a change stacked on parent: the child worktree has the parent's patch applied and then
// edit changes it, exactly as spawn + Stack + the agent's own work would.
func proposeDependent(t *testing.T, core *App, parent orchestrator.Change, agentID, changeID, rel string, edit func(path string)) orchestrator.Change {
	t.Helper()
	tree, err := core.Coord.WS.EnsureTree(agentID, false)
	if err != nil {
		t.Fatal(err)
	}
	parentPatch, err := core.Coord.WS.PatchPath(parent.AgentID, parent.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(parentPatch) != "" {
		if _, err := core.Coord.WS.Stack(agentID, []string{parentPatch}); err != nil {
			t.Fatalf("stack parent: %v", err)
		}
	}
	edit(filepath.Join(tree.Path, rel))
	lineage := []orchestrator.Dep{{ID: parent.ID, PatchHash: patchHash(t, core, parent.AgentID, parent.Path)}}
	s := core.Coord.Current()
	s.Changes = append(s.Changes, orchestrator.Change{
		ID: changeID, AgentID: agentID, Path: rel, Kind: "modified", Status: "proposed", Worktree: tree.Path,
		Needs: []string{parent.ID}, Lineage: lineage,
	})
	core.Coord.Restore(s)
	ch, _ := core.Coord.FindChange(changeID)
	return ch
}

// The pre-P4 hazard pinned by the first test: with the flag off a dependent alone lands its own bytes while its
// parent stays proposed. The flagged tests above invert that half-state.
func TestFlagOffAcceptDependentAloneShowsOldHalfState(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "keep\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "parent\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "b.txt", func(path string) {
		if err := os.WriteFile(path, []byte("child\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := core.acceptChange(child.ID); err != nil {
		t.Fatalf("flag-off accept: %v", err)
	}
	if now, _ := core.Coord.FindChange(child.ID); now.Status != "accepted" {
		t.Fatalf("dependent status = %q, want accepted with the flag off", now.Status)
	}
	if now, _ := core.Coord.FindChange(parent.ID); now.Status != "proposed" {
		t.Fatalf("parent status = %q, want proposed; this is the half-state P4 removes", now.Status)
	}
}

// A2: accepting a dependent alone is refused; nothing is written and the change stays proposed.
func TestDependentAloneIsRefusedWithoutWriting(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n", "b.txt": "keep\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableDependentSemantics(core)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "parent\ntwo\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "b.txt", func(path string) {
		if err := os.WriteFile(path, []byte("child\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := core.acceptChange(child.ID); err == nil || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("dependent accept error = %v, want needs_parent", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\ntwo\n" {
		t.Fatalf("a.txt was written: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "keep\n" {
		t.Fatalf("b.txt was written: %q", got)
	}
	now, _ := core.Coord.FindChange(child.ID)
	if now.Status != "proposed" || now.Integration == nil || now.Integration.State != "blocked" || now.Integration.Kind != "needs_parent" {
		t.Fatalf("dependent state = %+v, want proposed blocked needs_parent", now)
	}
}

// A3: "Accept with parents" writes the parent first; a forced failure of the second member restores the first.
func TestAcceptWithParentsWritesInOrderAndRollsBack(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n", "b.txt": "keep\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableDependentSemantics(core)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "parent\ntwo\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "b.txt", func(path string) {
		if err := os.WriteFile(path, []byte("child\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	// A dirty second path makes the write fail after the first member landed, exercising the journal rollback.
	core.setDirtyPaths([]string{"b.txt"})
	if _, err := core.acceptWithParents(child.ID); err == nil {
		t.Fatal("accept with parents must fail when a member cannot be written")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\ntwo\n" {
		t.Fatalf("parent write was not rolled back: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "keep\n" {
		t.Fatalf("child path was written: %q", got)
	}
	for _, id := range []string{parent.ID, child.ID} {
		if now, _ := core.Coord.FindChange(id); now.Status != "proposed" {
			t.Fatalf("%s status after rollback = %q", id, now.Status)
		}
	}

	// With no dirty path the same group lands in dependency order.
	core.setDirtyPaths(nil)
	out, err := core.acceptWithParents(child.ID)
	if err != nil {
		t.Fatalf("accept with parents: %v", err)
	}
	if m := out.(map[string]any); m["done"] != 2 {
		t.Fatalf("done = %v, want 2", m["done"])
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "parent\ntwo\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "child\n" {
		t.Fatalf("b.txt = %q", got)
	}
}

// A4: rejecting a parent blocks its dependents without rejecting them; rebasing without the parent is the direct
// three-way and is clean when the edits are disjoint.
func TestRejectParentBlocksDependentsAndRebaseIsDirectThreeWay(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\nfive\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableDependentSemantics(core)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "ONE\ntwo\nthree\nfour\nfive\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "a.txt", func(path string) {
		if err := os.WriteFile(path, []byte("ONE\ntwo\nthree\nfour\nFIVE\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := core.rejectChange(parent.ID); err != nil {
		t.Fatalf("reject parent: %v", err)
	}
	now, _ := core.Coord.FindChange(child.ID)
	if now.Status != "proposed" {
		t.Fatalf("dependent status = %q, want proposed", now.Status)
	}
	if now.Integration == nil || now.Integration.State != "blocked" || now.Integration.Kind != "parent_rejected" {
		t.Fatalf("dependent integration = %+v, want blocked parent_rejected", now.Integration)
	}
	if len(now.Lineage) != 1 || now.Lineage[0].ID != parent.ID {
		t.Fatalf("dependent lineage was rewritten: %+v", now.Lineage)
	}

	if _, err := core.rebaseChange(child.ID, true); err != nil {
		t.Fatalf("rebase without parent: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\ntwo\nthree\nfour\nFIVE\n" {
		t.Fatalf("rebased file = %q, want the parent hunk dropped", got)
	}
	now, _ = core.Coord.FindChange(child.ID)
	if now.Status != "accepted" || len(now.Lineage) != 0 {
		t.Fatalf("rebased change = %+v", now)
	}
}

// A4 (conflict half): rebasing when the dependent touches the same lines as the parent writes nothing.
func TestRebaseWithoutOverlappingParentConflicts(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableDependentSemantics(core)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "parent\ntwo\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "a.txt", func(path string) {
		if err := os.WriteFile(path, []byte("child\ntwo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := core.rebaseChange(child.ID, true); err == nil {
		t.Fatal("overlapping rebase must be refused")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\ntwo\n" {
		t.Fatalf("workspace was written: %q", got)
	}
	if now, _ := core.Coord.FindChange(child.ID); now.Status != "proposed" {
		t.Fatalf("dependent status = %q", now.Status)
	}
}

// A8: a component is all-or-nothing while an independent component still lands.
func TestAcceptAllComponentsAreAllOrNothing(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "keep\n", "c.txt": "base\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableDependentSemantics(core)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "parent\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "b.txt", func(path string) {
		if err := os.WriteFile(path, []byte("child\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	independent := proposeFromAgent(t, core, "agent_i", "chgI", "c.txt", "independent\n")

	core.setDirtyPaths([]string{"b.txt"})
	out, err := core.acceptAll(false)
	if err != nil {
		t.Fatalf("accept all: %v", err)
	}
	m := out.(map[string]any)
	if m["done"] != 1 {
		t.Fatalf("done = %v, want only the independent component", m["done"])
	}
	if now, _ := core.Coord.FindChange(independent.ID); now.Status != "accepted" {
		t.Fatalf("independent status = %q, want accepted", now.Status)
	}
	for _, id := range []string{parent.ID, child.ID} {
		if now, _ := core.Coord.FindChange(id); now.Status != "proposed" {
			t.Fatalf("%s status = %q, want proposed after component rollback", id, now.Status)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one\n" {
		t.Fatalf("failed component wrote a.txt: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(got) != "independent\n" {
		t.Fatalf("independent component did not land: %q", got)
	}
}

// A9: auto-apply accepts the parent first; the dependent is released on the next pass.
func TestAutoApplyReleasesDependentOnlyAfterParent(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "keep\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableDependentSemantics(core)
	core.autoApply.Store(true)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "parent\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "b.txt", func(path string) {
		if err := os.WriteFile(path, []byte("child\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := core.acceptAll(true); err != nil {
		t.Fatalf("first auto pass: %v", err)
	}
	if now, _ := core.Coord.FindChange(parent.ID); now.Status != "accepted" {
		t.Fatalf("parent status = %q, want accepted", now.Status)
	}
	if now, _ := core.Coord.FindChange(child.ID); now.Status != "proposed" {
		t.Fatalf("dependent was released too early: %q", now.Status)
	}
	if _, err := core.acceptAll(true); err != nil {
		t.Fatalf("second auto pass: %v", err)
	}
	if now, _ := core.Coord.FindChange(child.ID); now.Status != "accepted" {
		t.Fatalf("dependent status = %q, want accepted", now.Status)
	}
}

// The "Reject all" action next to a blocked dependent rejects it together with its parents.
func TestRejectWithDependentsRejectsTheGroup(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\n", "b.txt": "keep\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableDependentSemantics(core)
	parent := proposeFromAgent(t, core, "agent_p", "chgP", "a.txt", "parent\n")
	child := proposeDependent(t, core, parent, "agent_c", "chgC", "b.txt", func(path string) {
		if err := os.WriteFile(path, []byte("child\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := core.rejectWithDependents(parent.ID); err != nil {
		t.Fatalf("reject parent with dependents: %v", err)
	}
	if now, _ := core.Coord.FindChange(parent.ID); now.Status != "rejected" {
		t.Fatalf("parent status = %q, want rejected", now.Status)
	}
	if now, _ := core.Coord.FindChange(child.ID); now.Status != "rejected" {
		t.Fatalf("dependent status = %q, want rejected", now.Status)
	}
}

// A stacked child whose parent is still only proposed must project clean: its base holds the parent patch, and the
// projection applies that patch in memory. Adjacent-line edits make a live-workspace check report a false overlap.
func TestDependentWithProposedParentProjectsClean(t *testing.T) {
	base := numberedLines(20)
	core, dir := newGitCore(t, map[string]string{"shared.txt": base})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	enableDependentSemantics(core)

	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot for parent: %v", err)
	}
	parent := proposeFromAgent(t, core, "agent_parent", "chg_parent", "shared.txt", replaceLine(base, 14, "parent"))
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot for child: %v", err)
	}
	child := proposeDependent(t, core, parent, "agent_child", "chg_child", "shared.txt", func(path string) {
		if err := os.WriteFile(path, []byte(replaceLine(replaceLine(base, 14, "parent"), 15, "child")), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	core.projectPaths([]string{child.Path})
	now, _ := core.Coord.FindChange(child.ID)
	if now.Integration == nil || now.Integration.State != "clean" {
		t.Fatalf("dependent projection = %+v, want clean against the parent's planned bytes", now.Integration)
	}
	if now.Conflict != "" {
		t.Fatalf("dependent conflict = %q, want none", now.Conflict)
	}
	if p, _ := core.Coord.FindChange(parent.ID); p.Status != "proposed" {
		t.Fatalf("parent status = %q, want proposed", p.Status)
	}
}

// Accept-all must not publish a transient conflicted state for a dependent whose parent is in the same batch.
func TestAcceptAllEmitsNoConflictForDependentWithProposedParent(t *testing.T) {
	base := numberedLines(20)
	core, dir := newGitCore(t, map[string]string{"shared.txt": base})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	enableDependentSemantics(core)

	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot for parent: %v", err)
	}
	parent := proposeFromAgent(t, core, "agent_parent", "chg_parent", "shared.txt", replaceLine(base, 14, "parent"))
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot for child: %v", err)
	}
	childBody := replaceLine(replaceLine(base, 14, "parent"), 15, "child")
	child := proposeDependent(t, core, parent, "agent_child", "chg_child", "shared.txt", func(path string) {
		if err := os.WriteFile(path, []byte(childBody), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	var mu sync.Mutex
	var conflicted []orchestrator.Integration
	core.Coord.SetEmitter(func(name string, payload any) {
		if name != "changes.updated" {
			return
		}
		snapshot, ok := payload.([]orchestrator.Change)
		if !ok {
			return
		}
		for _, ch := range snapshot {
			if ch.ID != child.ID {
				continue
			}
			if ch.Conflict != "" || (ch.Integration != nil && ch.Integration.State == "conflicted") {
				mu.Lock()
				if ch.Integration != nil {
					conflicted = append(conflicted, *ch.Integration)
				} else {
					conflicted = append(conflicted, orchestrator.Integration{State: "conflicted", Reason: ch.Conflict})
				}
				mu.Unlock()
			}
		}
	})

	out, err := core.acceptAll(false)
	if err != nil {
		t.Fatalf("accept all: %v", err)
	}
	if done := out.(map[string]any)["done"]; done != 2 {
		t.Fatalf("done = %v, want parent and child", done)
	}
	mu.Lock()
	got := append([]orchestrator.Integration(nil), conflicted...)
	mu.Unlock()
	if len(got) > 0 {
		t.Fatalf("accept-all published %d conflicted states for the child: %+v", len(got), got)
	}
	now, _ := core.Coord.FindChange(child.ID)
	if now.Status != "accepted" || now.Conflict != "" || now.Integration == nil || now.Integration.State != "clean" {
		t.Fatalf("child after accept-all = %+v, want accepted clean", now)
	}
	content, err := os.ReadFile(filepath.Join(dir, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != childBody {
		t.Fatalf("workspace file:\n%s\nwant:\n%s", content, childBody)
	}
}
