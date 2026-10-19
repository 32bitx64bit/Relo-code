package orchestrator

import (
	"slices"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
)

// A dependency on a spec ID that admitTasks dropped as a duplicate of an unfinished task must resolve to that task,
// so the downstream task stays blocked until the prerequisite is done.
func TestApplyTurnDepOnDroppedDuplicateOfExistingTaskStaysBlocked(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	c.Graph.Add(taskgraph.Task{ID: "t1", Title: "Add login", Role: "worker"})

	c.applyTurn(agent.Agent{}, Turn{Tasks: []TurnTask{
		{ID: "a", Title: "add  LOGIN", Role: "worker"}, // duplicate of t1, dropped
		{ID: "b", Title: "Ship login", Role: "worker", Deps: []string{"a"}},
	}})

	ship := findTaskByTitle(t, c, "Ship login")
	if !slices.Equal(ship.Depends, []string{"t1"}) {
		t.Fatalf("Ship depends on %v, want [t1]", ship.Depends)
	}
	if ship.Status != taskgraph.Blocked {
		t.Fatalf("Ship status = %s before t1 is done, want blocked", ship.Status)
	}
	if _, err := c.Graph.Update("t1", func(x *taskgraph.Task) { x.Status = taskgraph.Done }); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.Graph.Runnable(), func(x taskgraph.Task) bool { return x.ID == ship.ID }) {
		t.Fatalf("Ship is not runnable after t1 is done")
	}
}

// A dependency on a spec ID dropped as a duplicate of another spec in the same turn must resolve to the surviving
// task, and stay blocked until that task is done.
func TestApplyTurnDepOnDroppedDuplicateWithinTurnStaysBlocked(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)

	c.applyTurn(agent.Agent{}, Turn{Tasks: []TurnTask{
		{ID: "c", Title: "Write docs", Role: "worker"},
		{ID: "d", Title: "write  DOCS", Role: "worker"}, // repeats c within this turn, dropped
		{ID: "e", Title: "Publish docs", Role: "worker", Deps: []string{"d"}},
	}})

	docs := findTaskByTitle(t, c, "Write docs")
	publish := findTaskByTitle(t, c, "Publish docs")
	if !slices.Equal(publish.Depends, []string{docs.ID}) {
		t.Fatalf("Publish depends on %v, want [%s]", publish.Depends, docs.ID)
	}
	if publish.Status != taskgraph.Blocked {
		t.Fatalf("Publish status = %s before Write docs is done, want blocked", publish.Status)
	}
	if _, err := c.Graph.Update(docs.ID, func(x *taskgraph.Task) { x.Status = taskgraph.Done }); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.Graph.Runnable(), func(x taskgraph.Task) bool { return x.ID == publish.ID }) {
		t.Fatalf("Publish is not runnable after Write docs is done")
	}
}

func findTaskByTitle(t *testing.T, c *Coord, title string) taskgraph.Task {
	t.Helper()
	for _, x := range c.Graph.List() {
		if x.Title == title {
			return x
		}
	}
	t.Fatalf("no task titled %q", title)
	return taskgraph.Task{}
}
