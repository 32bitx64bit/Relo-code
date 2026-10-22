package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
)

// stackedPair is a parent and a child stacked on it, both editing shared.txt, with every setting at its default
// (auto-apply, lazy projection and dependent semantics all off), as a user who never opened the settings page has.
func stackedPair(t *testing.T) (core *App, dir string, parent, child orchestrator.Change) {
	t.Helper()
	base := numberedLines(20)
	core, dir = newGitCore(t, map[string]string{"shared.txt": base})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	core.applySettings(map[string]any{"autoApply": false, "lazyConflicts": false, "dependentSemantics": false})
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatal(err)
	}
	parentBody := replaceLine(base, 5, "parent")
	parent = proposeFromAgent(t, core, "agent_parent", "chg_parent", "shared.txt", parentBody)
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatal(err)
	}
	child = proposeDependent(t, core, parent, "agent_child", "chg_child", "shared.txt", func(path string) {
		if err := os.WriteFile(path, []byte(replaceLine(parentBody, 6, "child")), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	return core, dir, parent, child
}

func acceptRequest(t *testing.T, core *App, params map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	_, err = core.Handle("changes.accept", raw)
	return err
}

// The drawer lists the newest change first, so a stacked child is usually clicked before its parent. The refusal
// has to say what to do instead; "resolve it by hand" sent users to auto-apply, the one path that orders them.
func TestAcceptingStackedChildFirstPointsAtAcceptWithParents(t *testing.T) {
	core, dir, _, child := stackedPair(t)
	err := acceptRequest(t, core, map[string]any{"id": child.ID})
	if err == nil {
		t.Fatal("a child that overlaps its unaccepted parent was accepted alone")
	}
	for _, want := range []string{"overlap", "Nothing was changed", "shared.txt", "Accept with parents"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "shared.txt")); string(got) != numberedLines(20) {
		t.Fatal("a refused accept changed the workspace")
	}
}

// What the Accept with parents button sends. It must land both, parent first, with default settings.
func TestAcceptWithParentsLandsBothOnDefaultSettings(t *testing.T) {
	core, dir, parent, child := stackedPair(t)
	if err := acceptRequest(t, core, map[string]any{"id": child.ID, "withParents": true}); err != nil {
		t.Fatalf("accept with parents: %v", err)
	}
	for _, id := range []string{parent.ID, child.ID} {
		if ch, _ := core.Coord.FindChange(id); ch.Status != "accepted" {
			t.Fatalf("%s is %q", id, ch.Status)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "shared.txt"))
	want := replaceLine(replaceLine(numberedLines(20), 5, "parent"), 6, "child")
	if string(got) != want {
		t.Fatalf("shared.txt = %q, want both edits", got)
	}
}

// A plain overlap, with no parent behind it, keeps the old message: the hint is only for stacked changes.
func TestOverlapWithoutParentsKeepsPlainMessage(t *testing.T) {
	core, dir := newGitCore(t, map[string]string{"a.txt": "one\ntwo\n"})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	a := proposeFromAgent(t, core, "agent_a", "chgA", "a.txt", "a\ntwo\n")
	b := proposeFromAgent(t, core, "agent_b", "chgB", "a.txt", "b\ntwo\n")
	if err := acceptRequest(t, core, map[string]any{"id": a.ID}); err != nil {
		t.Fatal(err)
	}
	err := acceptRequest(t, core, map[string]any{"id": b.ID})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("second sibling error = %v", err)
	}
	if strings.Contains(err.Error(), "Accept with parents") {
		t.Fatalf("a change with no parents got the stacked hint: %v", err)
	}
}
