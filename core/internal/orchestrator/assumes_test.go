package orchestrator

import (
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/taskgraph"
)

// The Primary hands a worker facts it was told, not facts it read. The worker is asked to check them before building,
// which is what catches a reader that was wrong.
func TestTheBriefAsksAWorkerToCheckWhatThePlanRelies(t *testing.T) {
	brief := taskBrief(taskgraph.Task{Title: "Wire the facts API", Assumes: []string{"engine/facts.c defines ai_facts_military(snap)", "flags are uint32_t"}})
	for _, want := range []string{"Check before you build", "1. engine/facts.c defines ai_facts_military(snap)", "2. flags are uint32_t", "deviations", "status blocked"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if plain := taskBrief(taskgraph.Task{Title: "Wire the facts API"}); strings.Contains(plain, "Check before you build") {
		t.Errorf("a task with no assumptions was told to check some:\n%s", plain)
	}
}

func TestAssumptionsAreBounded(t *testing.T) {
	long := strings.Repeat("x", 400)
	got := cleanAssumes([]string{"  a  ", "", "a", long, "b", "c", "d", "e", "f", "g"})
	if len(got) != maxAssumes || got[0] != "a" {
		t.Fatalf("assumes = %q, want %d trimmed, deduplicated entries", got, maxAssumes)
	}
	if n := len([]rune(got[1])); n > maxAssumeRunes {
		t.Fatalf("an assumption kept %d runes, want at most %d", n, maxAssumeRunes)
	}
}

func TestATurnCarriesTheAssumptionsOntoTheTask(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.CreateSession(t.TempDir(), "", ModeBuild)
	c.applyTurn(agent.Agent{}, Turn{Tasks: []TurnTask{{ID: "a", Title: "Use the facts API", Role: "worker", Difficulty: "easy", Assumes: []string{"facts.c defines ai_facts_military"}}}})
	task := findTaskByTitle(t, c, "Use the facts API")
	if len(task.Assumes) != 1 || task.Assumes[0] != "facts.c defines ai_facts_military" {
		t.Fatalf("task assumes = %q", task.Assumes)
	}
}

// A task that found a plan fact wrong tells the Primary at once, even though a finished task wakes nobody, and its
// report is not collapsed to a status line.
func TestAWrongPlanFactReachesThePrimaryAtOnce(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	s := c.CreateSession(t.TempDir(), "memory", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	p := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace})
	w := c.Agents.Create(agent.Agent{Role: agent.Worker, Workspace: s.Workspace})
	task := c.Graph.Add(taskgraph.Task{ID: "dev", Title: "Use the facts API", Role: string(agent.Worker), Workspace: s.Workspace, Attempt: 1})

	c.acceptReport(task, w, RawReport{Status: "completed", Summary: "Done.", Deviations: []string{"ai_facts_military takes (snap, out), not (snap): facts.c:61"}}, verifyOutcome{})

	if !c.peerNeedsHead(p.ID) {
		t.Fatalf("the Primary has nothing waiting that asks for it: %+v", c.Current().PeerMessages)
	}
	var found PeerMessage
	for _, m := range c.Current().PeerMessages {
		if m.To == p.ID && m.Kind == "deviation" {
			found = m
		}
	}
	if found.ID == "" || !strings.Contains(found.Text, "facts.c:61") || !strings.Contains(found.Text, "Use the facts API") {
		t.Fatalf("deviation message = %+v", found)
	}
	rep, _ := c.Agents.Report(w.ID)
	if len(rep.Deviations) != 1 || !strings.Contains(strings.Join(rep.Risks, "\n"), "Wrong plan fact") {
		t.Fatalf("report = %+v, want the deviation kept and listed as a risk", rep)
	}

	// A worker that already sent the message itself is not echoed.
	before := len(c.Current().PeerMessages)
	c.raiseDeviations(task, w, []string{"again"})
	if len(c.Current().PeerMessages) != before {
		t.Fatalf("a second deviation message was added for one task")
	}
}

// A clean finish costs the Primary a status line; anything it must weigh keeps the whole summary. A probe's summary
// is what it was sent to find, so it is never collapsed.
func TestOnlyABuildersCleanFinishIsCollapsed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    agent.Role
		status  agent.Status
		problem bool
		changed int
		risks   []string
		want    bool
	}{
		{"clean worker", agent.Worker, agent.Completed, false, 3, nil, true},
		{"worker with a risk", agent.Worker, agent.Completed, false, 3, []string{"untested on Windows"}, false},
		{"worker that changed nothing", agent.Worker, agent.Completed, false, 0, nil, false},
		{"failed checks or a question", agent.Worker, agent.Completed, true, 3, nil, false},
		{"failed worker", agent.Worker, agent.Failed, false, 3, nil, false},
		{"probe", agent.Probe, agent.Completed, false, 0, nil, false},
		{"reviewer", agent.Reviewer, agent.Completed, false, 0, nil, false},
	} {
		if got := cleanFinish(tc.role, tc.status, tc.problem, tc.changed, tc.risks); got != tc.want {
			t.Errorf("%s: cleanFinish = %v, want %v", tc.name, got, tc.want)
		}
	}
}
