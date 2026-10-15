package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/memory"
	"github.com/relocode/relo/internal/style"
	"github.com/relocode/relo/internal/taskgraph"
)

// Every call an agent makes carries what it was first sent, so what goes into the first message is paid for many
// times over. These pin the waste found in a real run: a worker's task dropped from its own pack, thirty lines
// describing what it was not shown, the task text twice, and the Primary's memory growing every turn.

// oneByOne matches the old "- title (scope_class): first line of the item" bullets.
var oneByOne = regexp.MustCompile(`\(scope_[a-z_]+\): `)

func seedMemory(c *Coord, n int) {
	for i := 0; i < n; i++ {
		c.Memory.Upsert(memory.Entry{Kind: "report", Title: fmt.Sprintf("Old report %d", i), Body: "The efficiency tab labels were renamed in webview main.js, report " + fmt.Sprint(i), Verified: true})
	}
}

func TestScopedWorkerGetsItsTaskOnceAndNoListOfWhatItWasNotShown(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"completed","summary":"done"}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "Rename the efficiency tab labels", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview, c.Settings.Memory = false, false, true
	c.Settings.Efficiency = style.Prefs{Preset: "auto"}
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	seedMemory(c, 12)
	c.Engine.PromoteFinding(ctxeng.Finding{Summary: "An old finding the worker is not scoped to", Verified: true})
	task := c.Graph.Add(taskgraph.Task{
		Title: "Apply the labels", Description: "Edit webview/src/main.js and add a label map.", Acceptance: []string{"the label map exists"},
		Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium",
	})
	a := c.spawn(task, agent.Worker)
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	sent := r.tasks[0].System + "\n" + r.tasks[0].Prompt

	if n := strings.Count(sent, "Edit webview/src/main.js and add a label map."); n != 1 {
		t.Errorf("the task text reached the model %d times, want once", n)
	}
	for _, want := range []string{"Rename the efficiency tab labels", "the label map exists"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the worker was not told %q", want)
		}
	}
	for _, unwanted := range []string{"Old report", "An old finding", "Generation status"} {
		if strings.Contains(sent, unwanted) {
			t.Errorf("the worker was sent %q, which its scope leaves out", unwanted)
		}
	}
	if oneByOne.MatchString(sent) {
		t.Errorf("the message describes the items it left out one by one:\n%s", sent)
	}
	if !strings.Contains(sent, "Left out of this context:") {
		t.Errorf("the message does not say that anything was left out:\n%s", sent)
	}
}

// A scope names context classes; one the engine does not know drops everything of that class without a word.
func TestEveryClassAScopeNamesIsOneTheEngineKnows(t *testing.T) {
	for _, role := range []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Worker, agent.Debug, agent.Integration, agent.Architect, agent.Test, agent.Reviewer} {
		for _, preset := range []string{"auto", "max"} {
			for _, cls := range style.Resolve(style.Prefs{Preset: preset}, role, "medium", 1, 16000).Scope.Classes {
				if !ctxeng.Class(cls).Valid() {
					t.Errorf("%s/%s: scope names the class %q, which the context engine does not have", role, preset, cls)
				}
			}
		}
	}
}

func TestPrimaryMemoryDoesNotGrowWithEveryTurn(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.SetRuntime(newDispatchRuntime())
	s := c.CreateSession(t.TempDir(), "Propose renames for the efficiency tab labels", ModeBuild)
	c.Settings.Memory = true
	p := c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	seedMemory(c, 20)

	memoryItems := func() int {
		n := 0
		for _, it := range c.Engine.SnapshotPool(p.ID) {
			if it.Class == ctxeng.ClassMemory {
				n++
			}
		}
		return n
	}
	c.seedPrimary("Propose renames for the efficiency tab labels", nil)
	first := memoryItems()
	if first == 0 || first > memory.RecallLimit {
		t.Fatalf("a request about the efficiency tab recalled %d entries, want 1 to %d", first, memory.RecallLimit)
	}
	for _, text := range []string{"ALright looks good.\n\nI approve", "Alright, rebuild the client and install it", "Propose renames for the efficiency tab labels"} {
		c.seedPrimary(text, nil)
	}
	if got := memoryItems(); got > memory.RecallLimit {
		t.Fatalf("after four turns the Primary holds %d memory entries, want at most %d", got, memory.RecallLimit)
	}
}
