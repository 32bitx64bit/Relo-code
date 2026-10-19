package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/style"
	"github.com/relocode/relo/internal/taskgraph"
)

// With efficiency auto, the scoped worker prompt must still carry the user's constraints.
func TestEfficiencyAutoScopedWorkerKeepsConstraints(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"completed","summary":"done"}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "auto", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.setConstraints([]string{"Do not touch the database schema"})
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "task", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium", Attempt: 0})
	a := c.spawn(task, agent.Worker)
	c.Settings.Efficiency = style.Prefs{Preset: "auto"}
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	if len(r.tasks) == 0 {
		t.Fatal("no worker task dispatched")
	}
	got := r.tasks[0].System + "\n" + r.tasks[0].Prompt
	if !strings.Contains(got, "Do not touch the database schema") {
		t.Fatalf("scoped worker prompt lost the constraint:\n%s", got)
	}
}
