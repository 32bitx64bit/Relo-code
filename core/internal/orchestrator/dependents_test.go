package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
	"github.com/relocode/relo/internal/workspace"
)

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// A5: a dependent that records after its parent was rejected is blocked, not accepted or discarded.
func TestRunningDependentAfterParentRejectIsBlocked(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))
	c.CreateSession(dir, "", ModeBuild)
	parent := Change{ID: "chgP", AgentID: "agent_p", Path: "a.txt", Status: "rejected"}
	c.Restore(&Session{Workspace: dir, Mode: ModeBuild, Changes: []Change{parent}})

	tree, err := mountTree(c, "agent_run")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.Path, "a.txt"), []byte("dependent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.setLineage("agent_run", []Dep{{ID: parent.ID, PatchHash: "whatever"}})
	out := c.changesFor(agent.Agent{ID: "agent_run", Role: agent.Worker, Worktree: tree.Path}, RawReport{})
	if len(out) != 1 {
		t.Fatalf("changes = %+v, want one", out)
	}
	if out[0].Integration == nil || out[0].Integration.State != "blocked" || out[0].Integration.Kind != "parent_rejected" {
		t.Fatalf("integration = %+v, want blocked parent_rejected", out[0].Integration)
	}
	if len(out[0].Lineage) != 1 || out[0].Lineage[0].ID != parent.ID {
		t.Fatalf("lineage = %+v, want the rejected parent kept", out[0].Lineage)
	}
}

// A6: a retry that proposes the same path rewrites the dependent's Lineage and Needs, keeping the old hash in the
// audit message.
func TestRetriedParentRewritesDependentLineage(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))

	oldTree, err := mountTree(c, "agent_old")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldTree.Path, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPatch, err := c.WS.PatchPath("agent_old", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	oldHash := hashText(oldPatch)

	newTree, err := mountTree(c, "agent_new")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newTree.Path, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newPatch, err := c.WS.PatchPath("agent_new", "a.txt")
	if err != nil {
		t.Fatal(err)
	}

	parent := Change{ID: "chgOld", AgentID: "agent_old", TaskID: "t1", Path: "a.txt", Status: "proposed", Worktree: oldTree.Path}
	dependent := Change{
		ID: "chgDep", AgentID: "agent_dep", TaskID: "t2", Path: "b.txt", Status: "proposed",
		Lineage: []Dep{{ID: parent.ID, PatchHash: oldHash}}, Needs: []string{parent.ID},
	}
	c.Restore(&Session{Workspace: dir, Mode: ModeBuild, Changes: []Change{parent, dependent}})
	retry := Change{ID: "chgNew", AgentID: "agent_new", Path: "a.txt", Status: "proposed", Worktree: newTree.Path}
	c.recordChanges(taskgraph.Task{ID: "t1"}, agent.Agent{ID: "agent_new", Worktree: newTree.Path}, []Change{retry})

	got, _ := c.FindChange("chgDep")
	if len(got.Lineage) != 1 || got.Lineage[0].ID != "chgNew" {
		t.Fatalf("lineage = %+v, want the retry's change", got.Lineage)
	}
	if got.Lineage[0].PatchHash != hashText(newPatch) {
		t.Fatalf("patch hash = %q, want the retry's patch hash", got.Lineage[0].PatchHash)
	}
	if len(got.Needs) != 1 || got.Needs[0] != "chgNew" {
		t.Fatalf("needs = %v, want [chgNew]", got.Needs)
	}
	audit := ""
	for _, m := range c.Current().Messages {
		if strings.Contains(m.Text, "Dependency rewritten after a retry") {
			audit = m.Text
		}
	}
	if !strings.Contains(audit, oldHash[:12]) {
		t.Fatalf("audit = %q, want the old patch hash kept", audit)
	}
}

// A6 (no replacement): a retry that does not touch the dependent's parent path blocks it parent_superseded.
func TestRetriedParentWithoutThePathBlocksDependent(t *testing.T) {
	dir := gitRepo(t)
	c := New(logging.New(nil, logging.Error))
	c.SetWorkspaceManager(workspace.New(dir))

	oldTree, err := mountTree(c, "agent_old")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldTree.Path, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newTree, err := mountTree(c, "agent_new")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newTree.Path, "b.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := Change{ID: "chgOld", AgentID: "agent_old", TaskID: "t1", Path: "a.txt", Status: "proposed", Worktree: oldTree.Path}
	dependent := Change{ID: "chgDep", AgentID: "agent_dep", TaskID: "t2", Path: "c.txt", Status: "proposed", Lineage: []Dep{{ID: parent.ID, PatchHash: "old"}}}
	c.Restore(&Session{Workspace: dir, Mode: ModeBuild, Changes: []Change{parent, dependent}})
	retry := Change{ID: "chgNew", AgentID: "agent_new", Path: "b.txt", Status: "proposed", Worktree: newTree.Path}
	c.recordChanges(taskgraph.Task{ID: "t1"}, agent.Agent{ID: "agent_new", Worktree: newTree.Path}, []Change{retry})

	got, _ := c.FindChange("chgDep")
	if got.Integration == nil || got.Integration.State != "blocked" || got.Integration.Kind != "parent_superseded" {
		t.Fatalf("integration = %+v, want blocked parent_superseded", got.Integration)
	}
	if got.Status != "proposed" || len(got.Lineage) != 1 || got.Lineage[0].ID != parent.ID {
		t.Fatalf("dependent = %+v, want proposed with its old lineage", got)
	}
}
