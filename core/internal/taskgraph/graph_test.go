package taskgraph

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDependencies(t *testing.T) {
	g := New()
	a := g.Add(Task{ID: "A", Title: "Investigate"})
	b := g.Add(Task{ID: "B", Title: "Design", Depends: []string{a.ID}})
	c := g.Add(Task{ID: "C", Title: "Backend", Depends: []string{b.ID}})
	d := g.Add(Task{ID: "D", Title: "UI", Depends: []string{b.ID}})
	e := g.Add(Task{ID: "E", Title: "Integrate", Depends: []string{c.ID, d.ID}})
	run := ids(g.Runnable())
	if !has(run, "A") || has(run, "B") {
		t.Fatalf("runnable %v", run)
	}
	_, _ = g.Update("A", func(t *Task) { t.Status = Done })
	run = ids(g.Runnable())
	if !has(run, "B") {
		t.Fatalf("B should run %v", run)
	}
	_, _ = g.Update("B", func(t *Task) { t.Status = Done })
	run = ids(g.Runnable())
	if !has(run, "C") || !has(run, "D") || has(run, "E") {
		t.Fatalf("parallel CD %v", run)
	}
	_, _ = g.Update("C", func(t *Task) { t.Status = Done })
	_, _ = g.Update("D", func(t *Task) { t.Status = Done })
	run = ids(g.Runnable())
	if !has(run, e.ID) {
		t.Fatalf("E %v", run)
	}
}

func TestDetectCycle(t *testing.T) {
	tests := []struct {
		name  string
		tasks []Task
		cycle bool
	}{
		{"acyclic", []Task{{ID: "a"}, {ID: "b", Depends: []string{"a"}}}, false},
		{"cycle", []Task{{ID: "a", Depends: []string{"b"}}, {ID: "b", Depends: []string{"a"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DetectCycle(tt.tasks)
			if (err != nil) != tt.cycle {
				t.Fatalf("DetectCycle() error = %v, cycle = %v", err, tt.cycle)
			}
		})
	}
}

func TestCheckedDependencies(t *testing.T) {
	g := New()
	if _, err := g.AddChecked(Task{ID: "a", Depends: []string{"missing"}}); err == nil || !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("AddChecked unknown dependency error = %v", err)
	}
	if _, err := g.AddChecked(Task{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddChecked(Task{ID: "b", Depends: []string{"c"}}); err == nil || !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("AddChecked unresolved dependency error = %v", err)
	}
	if _, err := g.AddChecked(Task{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SetDependencies("a", []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SetDependencies("b", []string{"a"}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("SetDependencies cycle error = %v", err)
	}
}

func TestValidateDependencies(t *testing.T) {
	if err := ValidateDependencies([]Task{{ID: "new", Depends: []string{"outside"}}}, func(id string) bool { return id == "outside" }); err != nil {
		t.Fatalf("known external dependency rejected: %v", err)
	}
}

func ids(ts []Task) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func has(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func TestLegacyBlockedOnRoundTrip(t *testing.T) {
	const stored = `{"id":"task-3","title":"Widget","status":"blocked","dependencies":[],"children":[],"blockedOn":{"question":"which?","family":"widget:2","blockedAt":"2026-10-23T12:00:00Z"}}`
	var got Task
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatal(err)
	}
	if got.BlockedOn == nil || !got.BlockedOn.Unresolved() || got.BlockedOn.Options != nil || got.BlockedOn.AnsweredBy != "" {
		t.Fatalf("legacy blockedOn changed: %+v", got.BlockedOn)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	// answeredAt is left out: time.Time ignores omitempty, as resolvedAt already does.
	for _, key := range []string{"options", "answer", "answeredBy", "cite"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Fatalf("empty %s written to json: %s", key, raw)
		}
	}
}

func TestLegacyFailedWithOpenBlockLoadsAsking(t *testing.T) {
	g := New()
	legacy := Task{ID: "f1", Title: "Widget", Role: "worker", Status: Failed,
		BlockedOn: &BlockedOn{Question: "which?", Family: "widget:2", BlockedAt: mustTime(t)}}
	got := g.Add(legacy)
	if got.Status != Asking {
		t.Fatalf("status = %q, want %q", got.Status, Asking)
	}
	answered := Task{ID: "f2", Title: "Widget", Role: "worker", Status: Failed,
		BlockedOn: &BlockedOn{Question: "which?", Resolution: "the new one", ResolvedAt: mustTime(t)}}
	if got := g.Add(answered); got.Status != Failed {
		t.Fatalf("answered failed task status = %q, want %q", got.Status, Failed)
	}
}

func TestAskingKeepsGraphUnsettled(t *testing.T) {
	g := New()
	g.Add(Task{ID: "a", Title: "first", Status: Asking, BlockedOn: &BlockedOn{Question: "which?", BlockedAt: mustTime(t)}})
	if g.Settled() {
		t.Fatal("graph settled while a task is Asking")
	}
	g.Add(Task{ID: "b", Title: "second", Depends: []string{"a"}})
	if got, _ := g.Get("a"); got.Status != Asking {
		t.Fatalf("recompute changed Asking to %q", got.Status)
	}
	if _, err := g.Update("a", func(t *Task) { t.Status = Done }); err != nil {
		t.Fatal(err)
	}
	if g.Settled() {
		t.Fatal("graph settled while task b is queued")
	}
	if _, err := g.Update("b", func(t *Task) { t.Status = Done }); err != nil {
		t.Fatal(err)
	}
	if !g.Settled() {
		t.Fatal("graph not settled once every task is done")
	}
}
