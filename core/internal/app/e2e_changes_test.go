package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/orchestrator"
)

// TestEndToEndSiblingsUserEditAndDependent is the P3/P4 scenario in one real Git repository: two sibling agents cut
// from the same base edit disjoint hunks, both are accepted, the user edits a third hunk, and a dependent agent
// stacked on a proposed parent edits the same file. Accepting the parent and child as a group must write every
// edit without inventing a conflict for the stacked child.
//
// The child edits the line next to the parent's hunk on purpose: checking it against the workspace before the
// parent is written would report a false overlap even though the two accepts are clean in dependency order.
func TestEndToEndSiblingsUserEditAndDependent(t *testing.T) {
	base := numberedLines(20)
	core, dir := newGitCore(t, map[string]string{"shared.txt": base})
	core.Coord.CreateSession(dir, "", orchestrator.ModeBuild)
	enableLazyConflicts(core)
	enableDependentSemantics(core)

	// The two siblings are cut from the same snapshot, as two workers admitted in one round would be.
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot for siblings: %v", err)
	}
	siblingA := replaceLine(base, 2, "sibling-a")
	siblingB := replaceLine(base, 6, "sibling-b")
	a := proposeFromAgent(t, core, "agent_sibling_a", "chg_sibling_a", "shared.txt", siblingA)
	b := proposeFromAgent(t, core, "agent_sibling_b", "chg_sibling_b", "shared.txt", siblingB)

	if _, err := core.acceptChange(a.ID); err != nil {
		t.Fatalf("accept sibling A: %v", err)
	}
	if _, err := core.acceptChange(b.ID); err != nil {
		t.Fatalf("accept sibling B: %v", err)
	}

	// The user edits a third, disjoint hunk after both siblings were cut.
	user := replaceLine(replaceLine(base, 2, "sibling-a"), 6, "sibling-b")
	user = replaceLine(user, 10, "user-edit")
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	// The parent is cut from the accepted siblings plus the user's edit; the child then stacks the parent's patch.
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot for parent: %v", err)
	}
	parent := proposeFromAgent(t, core, "agent_parent", "chg_parent", "shared.txt", replaceLine(user, 14, "parent"))
	if _, err := core.Coord.WS.Snapshot(); err != nil {
		t.Fatalf("snapshot for child: %v", err)
	}
	childBody := replaceLine(replaceLine(user, 14, "parent"), 15, "child")
	child := proposeDependent(t, core, parent, "agent_child", "chg_child", "shared.txt", func(path string) {
		if err := os.WriteFile(path, []byte(childBody), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := core.acceptAll(false); err != nil {
		t.Fatalf("accept parent and child: %v", err)
	}

	// No accepted change may carry a conflict, including the child whose base already contains its parent.
	changes := core.Coord.Changes()
	if len(changes) != 4 {
		t.Fatalf("changes = %d, want the two siblings, the parent and the child", len(changes))
	}
	for _, ch := range changes {
		if ch.Status != "accepted" {
			t.Fatalf("%s status = %q, want accepted", ch.ID, ch.Status)
		}
		if ch.Conflict != "" {
			t.Fatalf("%s has conflict %q", ch.ID, ch.Conflict)
		}
		if ch.Integration == nil || ch.Integration.State != "clean" {
			t.Fatalf("%s integration = %+v, want clean", ch.ID, ch.Integration)
		}
	}

	// Every edit is in the workspace bytes: two siblings, the user's, the parent's and the child's.
	want := replaceLine(user, 14, "parent")
	want = replaceLine(want, 15, "child")
	got, err := os.ReadFile(filepath.Join(dir, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("workspace file:\n%s\nwant:\n%s", got, want)
	}
	for _, marker := range []string{"sibling-a", "sibling-b", "user-edit", "parent", "child"} {
		if !strings.Contains(string(got), marker) {
			t.Fatalf("workspace lost %q:\n%s", marker, got)
		}
	}

	// Every accepted change has its own journal entry, so each accept can be undone.
	for _, id := range []string{a.ID, b.ID, parent.ID, child.ID} {
		if _, _, err := core.Coord.WS.JournalEntryFor(id); err != nil {
			t.Fatalf("journal entry for %s: %v", id, err)
		}
	}
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line%02d\n", i)
	}
	return b.String()
}

// replaceLine replaces the 1-based line with body and keeps every other line.
func replaceLine(text string, line int, body string) string {
	lines := strings.Split(text, "\n")
	if line-1 >= 0 && line-1 < len(lines) {
		lines[line-1] = body
	}
	return strings.Join(lines, "\n")
}
