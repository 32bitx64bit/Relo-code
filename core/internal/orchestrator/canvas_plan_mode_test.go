package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/agentmsg"
	"github.com/relocode/relo/internal/logging"
	"github.com/relocode/relo/internal/prompt"
	"github.com/relocode/relo/internal/runtime"
)

var canvasTestRoles = []agent.Role{agent.Primary, agent.Head, agent.Worker, agent.Probe, agent.Reviewer, agent.Architect, agent.Debug, agent.Test, agent.Integration}

var canvasTestModes = []Mode{ModeBuild, ModePlan, ModeReview}

var canvasTestActions = []string{"list", "open", "create", "update", "inspect", "capture", "popout", "close"}

// canvasGateAllows asks the real gate (Coord.canvasRequest) whether a role may use an action in a mode. A refusal
// returns at once; an action it lets through is forwarded to the extension, which shows as a canvas.requested event.
func canvasGateAllows(t *testing.T, mode Mode, role agent.Role, action string) bool {
	t.Helper()
	c, ev, _, key := newCanvasRoleCoord(t, mode, role)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.canvasRequest(ctx, canvasRoleRequest(key, action))
		done <- err
	}()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s %s in %s: the request finished with no reply from the extension", role, action, mode)
			}
			if !strings.Contains(err.Error(), "canvas "+action+" is ") {
				t.Fatalf("%s %s in %s: refused for an unexpected reason: %v", role, action, mode, err)
			}
			return false
		case <-deadline:
			t.Fatalf("%s %s in %s: neither refused nor forwarded", role, action, mode)
		case <-time.After(5 * time.Millisecond):
			if len(ev.list()) > 0 {
				return true
			}
		}
	}
}

// canvasModeTurn runs one turn for a role in a mode with Canvas on and returns the prompt and tool policy it sent.
func canvasModeTurn(t *testing.T, role agent.Role, mode Mode) (string, map[string]bool) {
	t.Helper()
	c := New(logging.New(nil, logging.Error))
	rt := &canvasScopeRuntime{Mock: runtime.NewMock()}
	c.SetRuntime(rt)
	c.CanvasEnabled = true
	work := t.TempDir()
	c.CreateSession(work, "design a landing page", mode)
	a := c.Agents.Create(agent.Agent{ID: "mode-" + string(role), Role: role, Workspace: work, Status: agent.Running})
	if _, err := c.runAgent(context.Background(), a, prompt.Parts{}, "do the work", nil); err != nil {
		t.Fatalf("runAgent(%s, %s): %v", role, mode, err)
	}
	task := rt.last()
	return task.Prompt, task.Tools
}

// The actions a turn offers are exactly the actions the gate lets through for that role and mode. Before, a builder in
// plan mode was offered create and update (and told to "create an artifact") and then refused: "canvas create is
// available only in build mode".
func TestCanvasOfferedActionsAreTheGatesActions(t *testing.T) {
	for _, role := range canvasTestRoles {
		for _, mode := range canvasTestModes {
			offered := map[string]bool{}
			for _, action := range canvasActionsFor(role, mode) {
				offered[action] = true
			}
			for _, action := range canvasTestActions {
				if got := canvasGateAllows(t, mode, role, action); got != offered[action] {
					t.Errorf("%s in %s mode: gate allows %s = %v, turn offers it = %v", role, mode, action, got, offered[action])
				}
			}
		}
	}
}

// The prompt a builder gets outside build mode never offers an authoring action the gate refuses, and still offers
// every action it allows. In build mode the text is unchanged.
func TestCanvasPlanModePromptAgreesWithTheGate(t *testing.T) {
	for _, mode := range canvasTestModes {
		text, tools := canvasModeTurn(t, agent.Worker, mode)
		if !tools[runtime.ReloCanvasTool] {
			t.Fatalf("%s: a builder lost the canvas tool: %v", mode, tools)
		}
		if mode == ModeBuild {
			if !strings.Contains(text, prompt.CanvasGuidance+canvasKeyNote) || strings.Contains(text, prompt.CanvasPlanGuidance) {
				t.Fatalf("build mode builder prompt changed:\n%s", text)
			}
			continue
		}
		scope := "\n- Your canvas actions are: " + strings.Join(canvasActionsFor(agent.Worker, mode), ", ") + "."
		if strings.Count(text, prompt.CanvasPlanGuidance+scope+canvasKeyNote) != 1 {
			t.Fatalf("%s mode builder canvas paragraph is not the plan guidance plus its actions:\n%s", mode, text)
		}
		if strings.Contains(text, prompt.CanvasGuidance) || strings.Contains(text, "create an artifact") {
			t.Fatalf("%s mode builder is still told to author:\n%s", mode, text)
		}
		for _, action := range []string{"create", "update"} {
			if canvasGateAllows(t, mode, agent.Worker, action) {
				t.Fatalf("the gate allows %s in %s mode, so the plan guidance is wrong", action, mode)
			}
			if strings.Contains(scope, action) {
				t.Errorf("%s mode scope line offers %s: %q", mode, action, scope)
			}
		}
		if !strings.Contains(prompt.CanvasPlanGuidance, "create and update are refused") {
			t.Errorf("plan guidance does not say create and update are refused: %s", prompt.CanvasPlanGuidance)
		}
	}
}

// The relo_agents list tells a builder which canvas actions it has; outside build mode that omits create and update.
func TestCanvasStatusFollowsTheMode(t *testing.T) {
	for _, mode := range canvasTestModes {
		c, _, _, key := newCanvasRoleCoord(t, mode, agent.Worker)
		v, err := c.peerRequest(context.Background(), agentmsg.Request{Key: key, Action: "list"})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := v.(map[string]any)["canvas"].(map[string]any)
		actions, _ := st["actions"].([]string)
		want := canvasActionsFor(agent.Worker, mode)
		if strings.Join(actions, ",") != strings.Join(want, ",") {
			t.Errorf("%s: roster canvas actions %v, want %v", mode, actions, want)
		}
	}
}
