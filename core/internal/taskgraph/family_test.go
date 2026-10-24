package taskgraph

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFamilyKeyIgnoresRewording(t *testing.T) {
	verified := Task{
		Title: "SPT stage 2 per spt-impl-plan contract",
		Role:  "worker",
		Paths: []string{"core/internal/taskgraph/graph.go"},
		Scope: "core",
	}
	hypothetical := Task{
		Title: "SPT stage 2",
		Role:  "worker",
		Paths: []string{"core/internal/taskgraph/graph.go"},
		Scope: "core",
	}
	if got, want := FamilyKey(hypothetical), FamilyKey(verified); got != want {
		t.Fatalf("reworded title key = %q, verified key = %q", got, want)
	}
}

func TestFamilyKeySeparatesVariants(t *testing.T) {
	base := Task{Title: "Widget stage 2", Role: "worker", Paths: []string{"a.go"}, Scope: "core"}
	variants := map[string]Task{
		"stage": {Title: "Widget stage 3", Role: "worker", Paths: []string{"a.go"}, Scope: "core"},
		"role":  {Title: "Widget stage 2", Role: "probe", Paths: []string{"a.go"}, Scope: "core"},
		"paths": {Title: "Widget stage 2", Role: "worker", Paths: []string{"b.go"}, Scope: "core"},
		"scope": {Title: "Widget stage 2", Role: "worker", Paths: []string{"a.go"}, Scope: "ui"},
		"none":  {Title: "Widget", Role: "worker", Paths: []string{"a.go"}, Scope: "core"},
	}
	want := FamilyKey(base)
	for name, v := range variants {
		if got := FamilyKey(v); got == want {
			t.Fatalf("%s variant shares family key %q", name, got)
		}
	}
}

func TestFamilyKeyPathOrderAndPunctuation(t *testing.T) {
	a := Task{Title: "Widget stage 2", Role: "Worker", Paths: []string{"a.go", "b.go"}}
	b := Task{Title: "widget  stage   2!", Role: "worker", Paths: []string{"b.go", "a.go"}}
	if FamilyKey(a) != FamilyKey(b) {
		t.Fatalf("path order or punctuation changed key: %q vs %q", FamilyKey(a), FamilyKey(b))
	}
}

func TestFamilyKeyNormalizesPaths(t *testing.T) {
	a := Task{Title: "Widget stage 2", Role: "worker", Paths: []string{"a.go", "b/c.go"}}
	b := Task{Title: "Widget stage 2", Role: "worker", Paths: []string{"./a.go", "b/../b/c.go", "  ", "./"}}
	if FamilyKey(a) != FamilyKey(b) {
		t.Fatalf("path spelling changed key: %q vs %q", FamilyKey(a), FamilyKey(b))
	}
}

func TestFamilyKeyDoesNotMutatePaths(t *testing.T) {
	in := []string{"b.go", ".//a.go", "c.go"}
	task := Task{Title: "Widget stage 2", Role: "worker", Paths: in}
	FamilyKey(task)
	want := []string{"b.go", ".//a.go", "c.go"}
	if len(task.Paths) != len(want) {
		t.Fatalf("Paths length = %d, want %d (%v)", len(task.Paths), len(want), task.Paths)
	}
	for i := range want {
		if task.Paths[i] != want[i] {
			t.Fatalf("Paths[%d] = %q, want %q; input mutated", i, task.Paths[i], want[i])
		}
	}
}

func TestBlockedOnUnresolved(t *testing.T) {
	now := mustTime(t)
	cases := []struct {
		name string
		b    *BlockedOn
		want bool
	}{
		{"nil", nil, false},
		{"pending", &BlockedOn{Question: "which contract?", Family: "widget:2", BlockedAt: now}, true},
		{"resolved", &BlockedOn{Question: "which contract?", Family: "widget:2", BlockedAt: now, Resolution: "use the new one", ResolvedAt: now}, false},
		{"whitespace", &BlockedOn{Question: "which contract?", Family: "widget:2", BlockedAt: now, Resolution: "  \t\n"}, true},
	}
	for _, tc := range cases {
		if got := tc.b.Unresolved(); got != tc.want {
			t.Fatalf("%s: Unresolved() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTaskJSONWithoutNewFields(t *testing.T) {
	const stored = `{"id":"task-1","title":"Fix parser","status":"queued","dependencies":[],"children":[]}`
	var got Task
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatalf("stored task without scope/blockedOn failed to load: %v", err)
	}
	if got.Scope != "" || got.BlockedOn != nil {
		t.Fatalf("unexpected defaults: scope %q blockedOn %+v", got.Scope, got.BlockedOn)
	}
	if got.ID != "task-1" || got.Title != "Fix parser" || got.Status != Queued {
		t.Fatalf("stored task changed: %+v", got)
	}
}

func TestTaskJSONRoundTripNewFields(t *testing.T) {
	task := Task{
		ID:     "task-2",
		Title:  "Widget stage 2",
		Status: Blocked,
		Scope:  "core",
		BlockedOn: &BlockedOn{
			Question:  "which contract wins?",
			Family:    FamilyKey(Task{Title: "Widget stage 2", Role: "worker"}),
			BlockedAt: mustTime(t),
		},
	}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"scope":"core"`) || !strings.Contains(string(raw), `"blockedOn":{`) {
		t.Fatalf("new fields missing from json: %s", raw)
	}
	var back Task
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Scope != task.Scope || back.BlockedOn == nil || !back.BlockedOn.Unresolved() || back.BlockedOn.Family != task.BlockedOn.Family {
		t.Fatalf("round trip mismatch: %+v", back)
	}
}

func TestDependencyBlockedUnchanged(t *testing.T) {
	g := New()
	a := g.Add(Task{ID: "a", Title: "first"})
	g.Add(Task{ID: "b", Title: "second", Depends: []string{a.ID}})
	if got, ok := g.Get("b"); !ok || got.Status != Blocked {
		t.Fatalf("unmet dependency status = %v, want %v", got.Status, Blocked)
	}
	if _, err := g.Update("a", func(t *Task) { t.Status = Done }); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.Get("b"); got.Status != Queued {
		t.Fatalf("met dependency status = %v, want %v", got.Status, Queued)
	}
}

func mustTime(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2026-10-23T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
