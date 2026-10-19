package ctxeng

import "testing"

// A worker's pinned constraint is a SourceParent item with no TaskID. Dependency-report filtering
// must not drop it, while unrelated parent reports stay excluded.
func TestDependencyReportsOnlyKeepsConstraintAndDropsUnrelatedParentReport(t *testing.T) {
	e := New()
	e.Bind("scope-constraint", "task", "worker", 100000)
	e.Ingest("scope-constraint", []Item{
		{ID: "constraint", Class: ClassTask, Source: SourceParent, Kind: KindConstraint, Title: "Constraints", Content: "Do not touch the database schema.", Pinned: true, Durability: 1},
		{ID: "dep-report", Class: ClassReport, Source: SourceParent, TaskID: "dep", Title: "Dependency", Content: "dependency report"},
		{ID: "unrelated-report", Class: ClassReport, Source: SourceParent, TaskID: "unrelated", Title: "Unrelated", Content: "unrelated report"},
	})
	scope := &Scope{DependencyReportsOnly: true, Classes: []Class{ClassTask, ClassReport}}

	check := func(label string, g *Generation) {
		t.Helper()
		included := map[string]bool{}
		for _, it := range g.Items {
			included[it.ID] = true
		}
		if !included["constraint"] {
			t.Fatalf("%s: constraint dropped by dependency scope; items=%v", label, included)
		}
		if !included["dep-report"] {
			t.Fatalf("%s: dependency report dropped; items=%v", label, included)
		}
		if included["unrelated-report"] {
			t.Fatalf("%s: unrelated parent report kept by dependency scope", label)
		}
	}

	g, err := e.Build(BuildRequest{AgentID: "scope-constraint", Scope: scope, DependencyTaskIDs: []string{"dep"}})
	if err != nil {
		t.Fatal(err)
	}
	check("build", g)

	g, err = e.Refresh("scope-constraint", "refresh")
	if err != nil {
		t.Fatal(err)
	}
	check("refresh", g)
}
