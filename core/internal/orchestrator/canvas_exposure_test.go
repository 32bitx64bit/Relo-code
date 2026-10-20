package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/runtime"
)

// canvasExposureRuntime keeps the prompt and the tool policy of every turn it is given.
type canvasExposureRuntime struct {
	*runtime.Mock
	mu    sync.Mutex
	tasks []runtime.AgentTask
}

func (r *canvasExposureRuntime) ConfigureMessages(context.Context, string, runtime.MessageTools) error {
	return nil
}

func (r *canvasExposureRuntime) DeliverMessage(context.Context, runtime.SessionID, string, string) error {
	return nil
}

func (r *canvasExposureRuntime) SendTask(ctx context.Context, sid runtime.SessionID, task runtime.AgentTask) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()
	return r.Mock.SendTask(ctx, sid, task)
}

func (r *canvasExposureRuntime) last() runtime.AgentTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) == 0 {
		return runtime.AgentTask{}
	}
	return r.tasks[len(r.tasks)-1]
}

// canvasExposure runs one turn for a role with canvas enabled and returns the prompt the model saw and the tool
// policy that turn carried.
func canvasExposure(t *testing.T, role agent.Role) (string, map[string]bool) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	rt := &canvasExposureRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(rt)
	c.CanvasEnabled = true
	work := t.TempDir()
	c.CreateSession(work, "design a landing page", ModeBuild)
	a := c.Agents.Create(agent.Agent{ID: "canvas-" + string(role), Role: role, Workspace: work, Status: agent.Running})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{}, "do the work", nil); err != nil {
		t.Fatalf("runAgent(%s): %v", role, err)
	}
	task := rt.last()
	return task.Prompt, task.Tools
}

// The Primary is offered the read-only canvas path and never the mutating actions.
func TestPrimaryCanvasExposureHasNoMutatingAction(t *testing.T) {
	promptText, tools := canvasExposure(t, agent.Primary)
	if !tools[runtime.ReloCanvasTool] {
		t.Fatalf("primary lost the read-only canvas path: %v", tools)
	}
	if v, ok := tools["*"]; !ok || v {
		t.Fatalf("primary must keep the wildcard deny: %v", tools)
	}
	if !strings.Contains(promptText, "Your canvas actions are: list, open, inspect.") {
		t.Fatalf("primary prompt does not name its canvas scope:\n%s", promptText)
	}
	for _, action := range []string{agent.CanvasActionCreate, agent.CanvasActionUpdate} {
		if strings.Contains(strings.ToLower(promptText), action) {
			t.Errorf("primary prompt advertises %s:\n%s", action, promptText)
		}
	}
}

// Every role that may not author gets the same read-only scope, and none of them keeps the mutating actions.
func TestRestrictedRolesShareTheReadOnlyCanvasScope(t *testing.T) {
	for _, role := range []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Reviewer, agent.Role("unknown")} {
		promptText, tools := canvasExposure(t, role)
		if !tools[runtime.ReloCanvasTool] {
			t.Fatalf("%s lost the read-only canvas path: %v", role, tools)
		}
		if !strings.Contains(promptText, "Your canvas actions are: list, open, inspect.") {
			t.Fatalf("%s prompt does not name the read-only scope:\n%s", role, promptText)
		}
		for _, action := range []string{agent.CanvasActionCreate, agent.CanvasActionUpdate} {
			if strings.Contains(strings.ToLower(promptText), action) {
				t.Errorf("%s prompt advertises %s:\n%s", role, action, promptText)
			}
		}
	}
}

// A builder role keeps the whole canvas tool and the full guidance, exactly as before.
func TestBuilderCanvasExposureKeepsEveryAction(t *testing.T) {
	for _, role := range []agent.Role{agent.Worker, agent.Architect, agent.Debug, agent.Test, agent.Integration} {
		promptText, tools := canvasExposure(t, role)
		if !tools[runtime.ReloCanvasTool] {
			t.Fatalf("%s lost the canvas tool: %v", role, tools)
		}
		if !strings.Contains(promptText, "Actions: list, open, create, update, inspect, capture, popout, close.") {
			t.Fatalf("%s lost the canvas guidance:\n%s", role, promptText)
		}
		if strings.Contains(promptText, "Your canvas actions are:") {
			t.Fatalf("%s prompt gained a restricted scope line:\n%s", role, promptText)
		}
	}
}
