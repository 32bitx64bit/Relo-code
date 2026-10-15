package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/runtime"
	"github.com/relocode/relo/internal/taskgraph"
)

func TestAcceptanceScenario(t *testing.T) {
	log := logging.New(nil, logging.Error)
	c := New(log)
	c.Runtime = runtime.NewMock()
	c.Settings.AutoVerify = false
	c.Settings.PrimaryModel = "mock/relo-primary"
	c.Settings.Tier1Model = "mock/relo-tier1"
	c.Settings.Tier2Model = "mock/relo-tier2"
	c.Settings.Budget = 8000
	c.Settings.Threshold = 0.2
	c.Settings.AutoRefresh = true
	events := []string{}
	var eventsMu sync.Mutex
	c.SetEmitter(func(name string, payload any) { eventsMu.Lock(); events = append(events, name); eventsMu.Unlock() })

	s := c.CreateSession(t.TempDir(), "Add Google and GitHub OAuth login.", ModeBuild)
	if s.ID == "" {
		t.Fatal("session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Submit(ctx, s.Objective, nil); err != nil {
		t.Fatal(err)
	}

	agents := c.Agents.List()
	if len(agents) < 3 {
		t.Fatalf("expected primary + workers, got %d %#v", len(agents), agents)
	}
	var roles []agent.Role
	var probe agent.Agent
	workers := 0
	for _, a := range agents {
		roles = append(roles, a.Role)
		if a.Role == agent.Probe {
			probe = a
		}
		if a.Role == agent.Worker {
			workers++
		}
		if a.ParentID == "" && a.Role != agent.Primary {
			t.Fatalf("non-primary without parent: %+v", a)
		}
	}
	if probe.ID == "" {
		t.Fatalf("probe missing, roles=%v", roles)
	}
	if workers < 2 {
		t.Fatalf("expected parallel workers, got %d roles=%v", workers, roles)
	}

	findings := c.Engine.Findings()
	if len(findings) == 0 {
		t.Fatal("probe produced no findings")
	}

	ins := c.Engine.Inspect(findings[0].AgentID)
	_ = ins
	var worker agent.Agent
	for _, a := range agents {
		if a.Role == agent.Worker {
			worker = a
			break
		}
	}
	wctx := c.Engine.Inspect(worker.ID)
	foundFinding := false
	for _, it := range wctx.Included {
		if it.Class == "finding" || strings.Contains(strings.ToLower(it.Content), "auth") {
			foundFinding = true
		}
	}
	pool := c.Engine.SnapshotPool(worker.ID)
	for _, it := range pool {
		if it.Class == "finding" || strings.Contains(strings.ToLower(it.Content+it.Title), "auth") {
			foundFinding = true
		}
	}
	if !foundFinding {
		t.Fatal("implementation worker context missing probe findings")
	}

	if c.Engine.CurrentNumber(worker.ID) < 1 {
		t.Fatal("worker has no generation")
	}

	primary, ok := c.Agents.Primary()
	if !ok {
		t.Fatal("no primary")
	}
	ptext := c.Engine.Render(primary.ID)
	if strings.Count(ptext, "exploratory grep hit") > 2 {
		t.Fatal("raw worker history leaked into primary")
	}
	if !strings.Contains(strings.ToLower(ptext), "oauth") && !strings.Contains(strings.ToLower(ptext), "auth") && !strings.Contains(strings.ToLower(ptext), "session") {
		t.Fatalf("primary missing compressed report:\n%s", ptext)
	}

	var completed int
	for _, tsk := range c.Graph.List() {
		if tsk.Status == taskgraph.Done {
			completed++
		}
	}
	if completed == 0 {
		t.Fatalf("no completed tasks: %+v", c.Graph.List())
	}

	has := map[string]bool{}
	eventsMu.Lock()
	for _, e := range events {
		has[e] = true
	}
	eventsMu.Unlock()
	for _, need := range []string{"session.created", "agent.created", "task.created", "finding.created"} {
		if !has[need] {
			t.Fatalf("missing event %s in %v", need, events)
		}
	}
}
