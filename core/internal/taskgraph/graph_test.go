package taskgraph

import (
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
