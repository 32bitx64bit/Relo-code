package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	rt "github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

// chattyRuntime streams the same narration many times before it answers, the way a model does while it works.
type chattyRuntime struct{ *dispatchRuntime }

func (r chattyRuntime) SendTask(ctx context.Context, sid rt.SessionID, task rt.AgentTask) error {
	for i := 0; i < 8; i++ {
		r.events <- rt.Event{Type: "message.part.updated", PartType: "text", Phase: "writing", Text: "Checking the repository state now"}
	}
	return r.dispatchRuntime.SendTask(ctx, sid, task)
}

// Streamed updates used to count as "repeated actions", which flagged almost every agent as looping and ran it a
// second time after it had already answered. One answer means one run.
func TestFinishedAgentIsNotRunAgain(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"completed","summary":"Found it in main.go."}`)
	c.SetRuntime(chattyRuntime{r})
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Find it", Role: string(agent.Probe), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "easy"})
	a := c.spawn(task, agent.Probe)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 1 {
		t.Fatalf("the agent ran %d times, want once", len(r.tasks))
	}
	if got, _ := c.Graph.Get(task.ID); got.Status != taskgraph.Done {
		t.Fatalf("task = %s, want done", got.Status)
	}
}

// A probe answers with a question, not a diff. Garbled JSON at the end of its reply used to fail it, which sent a
// second probe to read the same files again.
func TestProbeWithBrokenReportKeepsItsAnswerWithoutRepair(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"completed","summary":"The label table is in main.js","findings":[`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "Find labels", Role: string(agent.Probe), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "easy"})
	a := c.spawn(task, agent.Probe)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Graph.Get(task.ID)
	if got.Status != taskgraph.Done {
		t.Fatalf("task = %s, want done", got.Status)
	}
	if rep, ok := got.Report.(agent.Report); !ok || !strings.Contains(rep.Summary, "label table") {
		t.Fatalf("report = %+v", got.Report)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 1 {
		t.Fatalf("the probe was asked %d times, want once", len(r.tasks))
	}
}
