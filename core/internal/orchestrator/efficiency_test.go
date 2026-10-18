package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/scheduler"
	"github.com/relocode/relo/internal/style"
	"github.com/relocode/relo/internal/taskgraph"
)

func TestEfficiencyOffTaskPromptStaticNoFragment(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"completed","summary":"done"}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "off", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "task", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium", Attempt: 0})
	a := c.spawn(task, agent.Worker)
	c.Settings.Efficiency = style.Prefs{Preset: "off"}
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.tasks[0].System, "Answer first.") || strings.Contains(r.tasks[0].System, "Need it?") {
		t.Fatalf("unexpected fragment with preset off: %s", r.tasks[0].System)
	}
}

func TestEfficiencyAutoWorkerContainsFragmentsPrimaryNoCraft(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	r := newDispatchRuntime(`{"status":"completed","summary":"done"}`)
	c.SetRuntime(r)
	s := c.CreateSession(t.TempDir(), "auto", ModeBuild)
	c.Settings.AutoVerify, c.Settings.AutoReview = false, false
	c.Agents.Create(agent.Agent{Role: agent.Primary, Workspace: s.Workspace, Model: "mock/p"})
	task := c.Graph.Add(taskgraph.Task{Title: "task", Role: string(agent.Worker), Workspace: s.Workspace, Model: "mock/t1", Difficulty: "medium", Attempt: 0})
	a := c.spawn(task, agent.Worker)
	c.Settings.Efficiency = style.Prefs{Preset: "auto"}
	if err := c.execWorker(context.Background(), task, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.tasks[0].System, "Answer first.") {
		t.Fatalf("missing voice fragment")
	}
	if !strings.Contains(r.tasks[0].System, "smallest change that works") {
		t.Fatalf("missing craft fragment")
	}

	// Primary gets voice-or-nothing, never craft marker.
	prefs := style.Prefs{Preset: "auto"}
	resolved := style.Resolve(prefs, agent.Primary, "", 0, c.settings().Budget)
	if strings.Contains(resolved.Profile.Fragment(agent.Primary), "smallest change that works") {
		t.Fatalf("primary craft fragment leaked")
	}
}

func TestEfficiencyFragmentStableAcrossRefresh(t *testing.T) {
	prefs := style.Prefs{Preset: "auto"}
	r1 := style.Resolve(prefs, agent.Worker, "medium", 1, 16000)
	r2 := style.Resolve(prefs, agent.Worker, "medium", 1, 16000)
	g := taskgraph.Task{}
	_ = g
	a := prompt.ComposePartsWith(agent.Worker, ctxeng.Generation{}, "x", r1.Profile.Fragment(agent.Worker))
	b := prompt.ComposePartsWith(agent.Worker, ctxeng.Generation{}, "y", r2.Profile.Fragment(agent.Worker))
	if a.PrefixHash() != b.PrefixHash() {
		t.Fatalf("fragment hash unstable")
	}
}

func TestEfficiencyRetryDownshiftsVoice(t *testing.T) {
	prefs := style.Prefs{Preset: "auto"}
	r1 := style.Resolve(prefs, agent.Worker, "medium", 1, 16000)
	r2 := style.Resolve(prefs, agent.Worker, "medium", 2, 16000)
	if r2.Profile.Voice == r1.Profile.Voice {
		t.Fatalf("voice did not downshift: %s vs %s", r1.Profile.Voice, r2.Profile.Voice)
	}
}

func TestEfficiencyRetrySpawnSetsAttempt(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	c.Configure(DefaultSettings(), scheduler.New(scheduler.Limits{MaxRetry: 4}), routing.Config{Tier2: "mock/t2", Tier1: "mock/t1", Primary: "mock/p"})
	s := c.CreateSession(t.TempDir(), "retry", ModeBuild)
	c.Agents.Create(agent.Agent{Role: agent.Primary})
	task := c.Graph.Add(taskgraph.Task{Title: "work", Role: string(agent.Worker), Workspace: s.Workspace, Attempt: 1})
	a := c.spawn(task, agent.Worker)
	if a.Attempt != task.Attempt+1 {
		t.Fatalf("spawn attempt=%d, want %d", a.Attempt, task.Attempt+1)
	}
}

func TestEfficiencyScopeDropsMemoryForAutoWorker(t *testing.T) {
	prefs := style.Prefs{Preset: "auto"}
	res := style.Resolve(prefs, agent.Worker, "medium", 1, 16000)
	if res.Scope.Memory {
		t.Fatalf("worker scope should drop memory")
	}
	if len(res.Scope.Classes) > 0 {
		for _, cls := range res.Scope.Classes {
			if cls == "memory" {
				t.Fatalf("memory class leaked into worker scope")
			}
		}
	}
}

func TestFreshAgentReflectsRegistryStatus(t *testing.T) {
	c := New(logging.New(nil, logging.Error))
	a := c.Agents.Create(agent.Agent{Role: agent.Worker, Status: agent.Queued})
	c.Agents.Update(a.ID, func(x *agent.Agent) { x.Status = agent.Completed })
	if got := c.freshAgent(a).Status; got != agent.Completed {
		t.Fatalf("stale status %q", got)
	}
}
