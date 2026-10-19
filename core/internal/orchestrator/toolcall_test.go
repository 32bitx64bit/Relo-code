package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/taskgraph"
)

// A Primary has no tools. Asked to "stop", a model once answered with a call to a tool named noop, and Relo showed
// that markup to the user as its closing message.

func TestStripToolMarkup(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		had      bool
	}{
		{"<invoke name=\"noop\">\n</invoke>", "", true},
		{"All done.\n<invoke name=\"noop\">\n</invoke>", "All done.", true},
		{"<function_calls><invoke name=\"x\"><parameter name=\"a\">1</parameter></invoke></function_calls>", "", true},
		{"Built it.\n<invoke name=\"shell\">\n<parameter name=\"cmd\">ls", "Built it.", true}, // never closed
		{"Use <invoked> as a name and <invoke_all> too.", "Use <invoked> as a name and <invoke_all> too.", false},
		{"Keep <code>x</code> but not <invoke name=\"a\">1</invoke> or the rest.", "Keep <code>x</code> but not  or the rest.", true},
		{"Cut off mid-call <invoke> and then nothing closes it.", "Cut off mid-call", true}, // an open tag with no end takes the rest with it
		{"plain prose", "plain prose", false},
		{"", "", false},
	} {
		got, had := stripToolMarkup(tc.in)
		if got != tc.want || had != tc.had {
			t.Errorf("stripToolMarkup(%q) = %q, %v; want %q, %v", tc.in, got, had, tc.want, tc.had)
		}
	}
}

func primaryFor(t *testing.T, c *Coord) agent.Agent {
	t.Helper()
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	return c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
}

func TestPrimaryToolCallIsAskedAgainNotShown(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime("<invoke name=\"noop\">\n</invoke>", `{"status":"completed","userMessage":"The client is rebuilt.","tasks":[]}`)
	c.SetRuntime(r)
	a := primaryFor(t, c)
	turn, err := c.primaryTurn(context.Background(), a, prompt.Parts{Static: "s", Dynamic: "d"}, "integrate")
	if err != nil || turn.UserMessage != "The client is rebuilt." {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 2 || !strings.Contains(r.tasks[1].Prompt, "no repository tools") || !strings.Contains(r.tasks[1].Prompt, "relo_agents") {
		t.Fatalf("the second ask did not distinguish messaging from repository tools: %+v", r.tasks)
	}
}

func TestPrimaryThatOnlyCallsToolsGetsTheWorkersReportsShown(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime("<invoke name=\"noop\">\n</invoke>", "<invoke name=\"noop\">\n</invoke>"))
	a := primaryFor(t, c)
	task := c.Graph.Add(taskgraph.Task{Title: "Rebuild client via make client", Role: string(agent.Worker), Workspace: a.Workspace})
	if _, err := c.Graph.Update(task.ID, func(x *taskgraph.Task) {
		x.Status, x.Finished = taskgraph.Done, time.Now()
		x.Report = agent.Report{Status: "completed", Summary: "make client succeeded; client/build/pax-client is newer than HEAD."}
	}); err != nil {
		t.Fatal(err)
	}
	turn, err := c.primaryTurn(context.Background(), a, prompt.Parts{}, "integrate")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Rebuild client via make client", "pax-client is newer than HEAD"} {
		if !strings.Contains(turn.UserMessage, want) {
			t.Errorf("the closing message lacks %q: %s", want, turn.UserMessage)
		}
	}
	if strings.Contains(turn.UserMessage, "invoke") {
		t.Errorf("tool-call markup reached the user: %s", turn.UserMessage)
	}
}

func TestPrimaryProseBesideAToolCallIsKeptWithoutAnotherCall(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime("The rebuild finished and the client is current.\n<invoke name=\"noop\">\n</invoke>")
	c.SetRuntime(r)
	a := primaryFor(t, c)
	turn, err := c.primaryTurn(context.Background(), a, prompt.Parts{}, "integrate")
	if err != nil || turn.UserMessage != "The rebuild finished and the client is current." {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) != 1 {
		t.Fatalf("took %d model calls, want 1", len(r.tasks))
	}
}

func TestWorkerThatEndsWithAToolCallHasNoReportToShow(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime())
	s := c.CreateSession(t.TempDir(), "q", ModeBuild)
	a := agent.Agent{ID: "w", Role: agent.Probe, Workspace: s.Workspace}
	rep, err := c.workerReport(context.Background(), a, prompt.Parts{}, "<invoke name=\"noop\">\n</invoke>")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "failed" || strings.Contains(rep.Summary, "invoke") || !strings.Contains(rep.Summary, "tool call") {
		t.Fatalf("report = %+v", rep)
	}
}
