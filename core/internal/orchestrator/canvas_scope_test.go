package orchestrator

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/runtime"
)

// canvasScopeRuntime keeps the prompt and the tool policy of every turn it is given.
type canvasScopeRuntime struct {
	*runtime.Mock
	mu    sync.Mutex
	tasks []runtime.AgentTask
}

func (r *canvasScopeRuntime) ConfigureMessages(context.Context, string, runtime.MessageTools) error {
	return nil
}

func (r *canvasScopeRuntime) DeliverMessage(context.Context, runtime.SessionID, string, string) error {
	return nil
}

func (r *canvasScopeRuntime) SendTask(ctx context.Context, sid runtime.SessionID, task runtime.AgentTask) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, task)
	r.mu.Unlock()
	return r.Mock.SendTask(ctx, sid, task)
}

func (r *canvasScopeRuntime) last() runtime.AgentTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tasks) == 0 {
		return runtime.AgentTask{}
	}
	return r.tasks[len(r.tasks)-1]
}

// canvasScopeTurn runs one turn for a role and returns the prompt the model saw and the tool policy that turn
// carried. Canvas is on unless the caller turns it off.
func canvasScopeTurn(t *testing.T, role agent.Role, canvas bool) (string, map[string]bool) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	rt := &canvasScopeRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(rt)
	c.CanvasEnabled = canvas
	work := t.TempDir()
	c.CreateSession(work, "design a landing page", ModeBuild)
	a := c.Agents.Create(agent.Agent{ID: "scope-" + string(role), Role: role, Workspace: work, Status: agent.Running})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{}, "do the work", nil); err != nil {
		t.Fatalf("runAgent(%s): %v", role, err)
	}
	task := rt.last()
	return task.Prompt, task.Tools
}

// canvasScopeLine is the line the turn adds to name the canvas actions a restricted role may use.
const canvasScopeLine = "\n- Your canvas actions are: list, open, inspect."

// canvasKeyNote is the tail of the canvas paragraph, the same for every role.
const canvasKeyNote = " Your Canvas key is the same private key: pass it as key to relo_canvas. Never copy it into artifacts or reports."

// The Primary is offered the canvas tool on a read-only scope: it may list, open and inspect, and the turn never
// names an authoring action.
func TestCanvasScopePrimaryIsReadOnly(t *testing.T) {
	promptText, tools := canvasScopeTurn(t, agent.Primary, true)
	if !tools[runtime.ReloCanvasTool] {
		t.Fatalf("primary lost the read-only canvas path: %v", tools)
	}
	scope := agent.CanvasActionsForRole(agent.Primary)
	want := []string{agent.CanvasActionList, agent.CanvasActionOpen, agent.CanvasActionInspect}
	if !reflect.DeepEqual(scope, want) {
		t.Fatalf("primary canvas scope = %v, want %v", scope, want)
	}
	segment := prompt.CanvasVerifyGuidance + canvasScopeLine + canvasKeyNote
	if strings.Count(promptText, segment) != 1 {
		t.Fatalf("primary canvas paragraph = %q, want %q", promptText, segment)
	}
	for _, action := range []string{agent.CanvasActionCreate, agent.CanvasActionUpdate} {
		if strings.Contains(promptText, action) {
			t.Errorf("primary prompt advertises %s:\n%s", action, promptText)
		}
	}
}

// A builder role keeps today's exposure: the same tool policy and the same canvas paragraph, byte for byte. The
// guidance already lists every action, so the scope line is not added to it.
func TestCanvasScopeBuilderIsUnchanged(t *testing.T) {
	promptText, tools := canvasScopeTurn(t, agent.Worker, true)
	want := runtime.ToolsFor(string(agent.Worker), string(ModeBuild), true)
	want[runtime.ReloCanvasTool] = true
	// relo_image does not depend on Canvas: it is offered because this fixture's model is not known to be text only.
	want = runtime.WithImages(want, string(agent.Worker), true)
	if !reflect.DeepEqual(tools, want) {
		t.Fatalf("builder tool exposure changed:\n got %v\nwant %v", tools, want)
	}
	segment := prompt.CanvasGuidance + canvasKeyNote
	if strings.Count(promptText, segment) != 1 {
		t.Fatalf("builder canvas paragraph changed:\n%s", promptText)
	}
	if strings.Contains(promptText, "Your canvas actions are:") {
		t.Fatalf("builder prompt gained a scope line:\n%s", promptText)
	}
	if got := canvasScopeText(agent.Worker, agent.CanvasActionsForRole(agent.Worker)); got != "" {
		t.Fatalf("builder scope text = %q, want empty", got)
	}
}

// A turn with no canvas scope carries no canvas tool: the scope is what puts the tool in the policy.
func TestCanvasScopeEmptyKeepsTheToolOut(t *testing.T) {
	for _, scope := range [][]string{nil, {}} {
		policy := runtime.WithCanvas(runtime.ToolsFor(string(agent.Worker), string(ModeBuild), true), true, scope)
		if _, ok := policy[runtime.ReloCanvasTool]; ok {
			t.Fatalf("canvas offered without a scope: %v", policy)
		}
	}
	promptText, tools := canvasScopeTurn(t, agent.Worker, false)
	if _, ok := tools[runtime.ReloCanvasTool]; ok {
		t.Fatalf("canvas tool offered to a turn with no canvas scope: %v", tools)
	}
	if strings.Contains(promptText, "Relo Canvas (relo_canvas) is available") {
		t.Fatalf("canvas guidance offered without the canvas tool:\n%s", promptText)
	}
}
