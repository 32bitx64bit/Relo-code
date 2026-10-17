package orchestrator

import (
	"testing"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
)

func seedTitles(items []ctxeng.Item) map[string]ctxeng.Item {
	out := map[string]ctxeng.Item{}
	for _, it := range items {
		out[it.Title] = it
	}
	return out
}

// A worker pack carries findings from its dep tasks, transitively, with the source task kept as provenance.
func TestWorkerSeedIncludesDepFindingsTransitively(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	grand := c.Graph.Add(taskgraph.Task{ID: "task-grand", Title: "grand"})
	parent := c.Graph.Add(taskgraph.Task{ID: "task-parent", Title: "parent", Depends: []string{grand.ID}})
	child := c.Graph.Add(taskgraph.Task{ID: "task-child", Title: "child", Depends: []string{parent.ID}})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-grand", TaskID: grand.ID, Summary: "grand finding"})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-parent", TaskID: parent.ID, Summary: "parent finding"})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-far", TaskID: "task-far", Summary: "far finding", Files: []string{"zzz/far.go"}})

	items := seedTitles(c.workerSeed(child, agent.Worker))
	grandItem, ok := items["grand finding"]
	if !ok {
		t.Fatal("transitive dep finding missing from worker pack")
	}
	if grandItem.TaskID != grand.ID {
		t.Fatalf("finding provenance = %q, want %q", grandItem.TaskID, grand.ID)
	}
	if _, ok := items["parent finding"]; !ok {
		t.Fatal("direct dep finding missing from worker pack")
	}
	if _, ok := items["far finding"]; ok {
		t.Fatal("finding from an unrelated task leaked into worker pack")
	}
}

// A finding naming a file in the task's scope is included even without a dep link.
func TestWorkerSeedIncludesScopeFindings(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	task := c.Graph.Add(taskgraph.Task{
		ID:          "task-scope",
		Title:       "Fix the style matrix",
		Description: "Update core/internal/style/style.go so roles resolve cleanly.",
		Acceptance:  []string{"style_test.go passes"},
	})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-exact", TaskID: "task-x", Summary: "exact scope finding", Files: []string{"core/internal/style/style.go"}})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-suffix", TaskID: "task-y", Summary: "suffix scope finding", Files: []string{"internal/style/style_test.go"}})

	items := seedTitles(c.workerSeed(task, agent.Worker))
	if _, ok := items["exact scope finding"]; !ok {
		t.Fatal("finding naming a scoped file missing from worker pack")
	}
	if _, ok := items["suffix scope finding"]; !ok {
		t.Fatal("finding naming a scoped file by short spelling missing from worker pack")
	}
}

// A finding with no dep link and no file in the task's scope is excluded.
func TestWorkerSeedExcludesUnrelatedFindings(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	task := c.Graph.Add(taskgraph.Task{ID: "task-scope", Title: "Fix parser", Description: "Rework pkg/parse.go."})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-other", TaskID: "task-other", Summary: "other task finding", Files: []string{"net/http/handler.go"}})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-nofiles", TaskID: "task-nofiles", Summary: "fileless finding"})

	items := seedTitles(c.workerSeed(task, agent.Worker))
	if _, ok := items["other task finding"]; ok {
		t.Fatal("unrelated finding leaked into worker pack")
	}
	if _, ok := items["fileless finding"]; ok {
		t.Fatal("finding without scope files leaked into worker pack")
	}
}

// Reports travel with the pack only for dep tasks.
func TestWorkerSeedIncludesDepReportsOnly(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	dep := c.Graph.Add(taskgraph.Task{ID: "task-dep", Title: "dep"})
	task := c.Graph.Add(taskgraph.Task{ID: "task-main", Title: "main", Depends: []string{dep.ID}})
	depAgent := c.Agents.Create(agent.Agent{Role: agent.Worker, Title: "dep work", TaskID: dep.ID})
	otherAgent := c.Agents.Create(agent.Agent{Role: agent.Worker, Title: "other work", TaskID: "task-other"})
	c.Agents.SetReport(depAgent.ID, agent.Report{Status: agent.Completed, Summary: "dep summary", Files: []string{"pkg/dep.go"}})
	c.Agents.SetReport(otherAgent.ID, agent.Report{Status: agent.Completed, Summary: "other summary", Files: []string{"pkg/other.go"}})

	items := seedTitles(c.workerSeed(task, agent.Integration))
	rep, ok := items["dep work"]
	if !ok {
		t.Fatal("dep report missing from worker pack")
	}
	if rep.Class != ctxeng.ClassReport || rep.TaskID != dep.ID {
		t.Fatalf("dep report item = %+v", rep)
	}
	if _, ok := items["other work"]; ok {
		t.Fatal("non-dep report leaked into worker pack")
	}
}

// Architect packs are unchanged: every finding still arrives.
func TestWorkerSeedArchitectKeepsAllFindings(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	task := c.Graph.Add(taskgraph.Task{ID: "task-main", Title: "Fix parser", Description: "Rework pkg/parse.go."})
	c.Engine.PromoteFinding(ctxeng.Finding{ID: "f-other", TaskID: "task-other", Summary: "other task finding", Files: []string{"net/http/handler.go"}})

	items := seedTitles(c.workerSeed(task, agent.Architect))
	if _, ok := items["other task finding"]; !ok {
		t.Fatal("architect pack lost findings outside dep scope")
	}
}

func TestDepTaskIDsTransitiveAndCycleSafe(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	a := c.Graph.Add(taskgraph.Task{ID: "task-a", Title: "a", Depends: []string{"task-b"}})
	b := c.Graph.Add(taskgraph.Task{ID: "task-b", Title: "b", Depends: []string{"task-c"}})
	c.Graph.Add(taskgraph.Task{ID: "task-c", Title: "c", Depends: []string{"task-a"}})

	got := c.depTaskIDs(a)
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("duplicate dep id in %v", got)
		}
		seen[id] = true
	}
	if !seen[b.ID] || !seen["task-c"] {
		t.Fatalf("depTaskIDs = %v, want transitive deps", got)
	}
}
