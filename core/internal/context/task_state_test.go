package ctxeng

import (
	"strings"
	"testing"
)

func TestReplacedTaskStateDoesNotReturnFromPriorGenerations(t *testing.T) {
	e := New()
	e.Bind("p", "", "primary", 24000)
	e.Ingest("p", []Item{
		{Class: ClassTask, Source: SourceUser, Title: "Objective", Content: "old objective", Pinned: true},
		{Class: ClassTask, Source: SourceUser, Title: "Objective", Content: "older objective", Pinned: true},
		{Class: ClassTask, Source: SourceVerified, Title: "Decision", Content: "old decision", Verified: true},
		{Class: ClassFinding, Source: SourceRepo, Content: "keep this evidence"},
	})
	old, err := e.Build(BuildRequest{AgentID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	e.SetTaskState("p", "Objective", []Item{{Source: SourceUser, Content: "current objective", Pinned: true}})
	e.SetTaskState("p", "Decision", []Item{{Source: SourceSummary, Content: "current decision"}})
	for _, build := range []func() (*Generation, error){
		func() (*Generation, error) { return e.Build(BuildRequest{AgentID: "p"}) },
		func() (*Generation, error) { return e.Refresh("p", "refresh") },
		func() (*Generation, error) { return e.Build(BuildRequest{AgentID: "p"}) },
	} {
		g, err := build()
		if err != nil {
			t.Fatal(err)
		}
		got := Render(g)
		for _, stale := range []string{"old objective", "older objective", "old decision"} {
			if strings.Contains(got, stale) {
				t.Fatalf("removed state resurrected: %s", got)
			}
		}
		for _, live := range []string{"current objective", "current decision", "keep this evidence"} {
			if !strings.Contains(got, live) {
				t.Fatalf("live state lost %q: %s", live, got)
			}
		}
	}
	if !strings.Contains(Render(old), "older objective") {
		t.Fatal("replacing live state changed the saved historical generation")
	}
}

func TestUnchangedTaskStatePreservesCachedPrefix(t *testing.T) {
	e := New()
	item := Item{Source: SourceUser, Content: "keep the public API", Pinned: true}
	e.SetTaskState("p", "Objective", []Item{item})
	first, _ := e.Build(BuildRequest{AgentID: "p"})
	e.SetTaskState("p", "Objective", []Item{item})
	second, _ := e.Build(BuildRequest{AgentID: "p"})
	if RenderStable(first) != RenderStable(second) || first.Items[0].ID != second.Items[0].ID {
		t.Fatal("unchanged canonical task state invalidated the stable prefix")
	}
}

func TestRestoredTaskStateComesOnlyFromNewestGeneration(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "oldest-first", true: "newest-first"}[reverse], func(t *testing.T) {
			e := New()
			old := Generation{AgentID: "p", Number: 1, Phase: PhaseExplore, Budget: 1000, Items: []Item{
				{ID: "old-objective", Class: ClassTask, Title: "Objective", Content: "old objective"},
				{ID: "old-decision", Class: ClassTask, Title: "Decision", Content: "old decision"},
				{ID: "evidence", Class: ClassFinding, Content: "prior evidence"},
			}}
			current := Generation{AgentID: "p", Number: 2, Phase: PhaseDesign, Budget: 24000, Items: []Item{
				{ID: "current-objective", Class: ClassTask, Title: "Objective", Content: "current objective"},
				{ID: "current-decision", Class: ClassTask, Title: "Decision", Content: "current decision"},
			}}
			if reverse {
				e.RestoreGeneration(current)
				e.RestoreGeneration(old)
			} else {
				e.RestoreGeneration(old)
				e.RestoreGeneration(current)
			}
			g, err := e.Refresh("p", "after reload")
			if err != nil {
				t.Fatal(err)
			}
			got := Render(g)
			if strings.Contains(got, "old objective") || strings.Contains(got, "old decision") {
				t.Fatalf("reload resurrected stale task state: %s", got)
			}
			if !strings.Contains(got, "prior evidence") || !strings.Contains(got, "current decision") || g.Budget != 24000 || g.Phase != PhaseDesign {
				t.Fatalf("reload lost current state or prior evidence: %+v", g)
			}
		})
	}
}
