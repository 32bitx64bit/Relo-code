package prompt

import (
	"strings"
	"testing"

	"github.com/relocode/relo/internal/agent"
)

// The role decides the canvas guidance: a builder role keeps the full guidance, and every other role gets guidance
// without the authoring instructions.
func TestCanvasGuidanceFollowsTheRole(t *testing.T) {
	for _, role := range []agent.Role{agent.Worker, agent.Architect, agent.Debug, agent.Test, agent.Integration} {
		if got := CanvasGuidanceFor(role); got != CanvasGuidance {
			t.Errorf("%s must keep the full canvas guidance, got %q", role, got)
		}
	}
	if got := CanvasGuidanceFor(agent.Primary); got != CanvasVerifyGuidance {
		t.Errorf("primary must get the verify-only guidance, got %q", got)
	}
	for _, role := range []agent.Role{agent.Head, agent.Probe, agent.Reviewer, agent.Role("unknown"), agent.Role("")} {
		if got := CanvasGuidanceFor(role); got != CanvasInspectGuidance {
			t.Errorf("%s must get the inspect-only guidance, got %q", role, got)
		}
	}
}

// The read-only guidance names the inspection actions and says that the role does not author canvases.
func TestReadOnlyCanvasGuidanceHasNoAuthoringInstructions(t *testing.T) {
	for name, guidance := range map[string]string{"verify": CanvasVerifyGuidance, "inspect": CanvasInspectGuidance} {
		for _, forbidden := range []string{"create", "update"} {
			if strings.Contains(strings.ToLower(guidance), forbidden) {
				t.Errorf("%s guidance contains authoring word %q: %s", name, forbidden, guidance)
			}
		}
		if !strings.Contains(guidance, "list, open or inspect") {
			t.Errorf("%s guidance must name the read-only actions: %s", name, guidance)
		}
		if !strings.Contains(guidance, "You never make canvases") {
			t.Errorf("%s guidance must state that the role does not author canvases: %s", name, guidance)
		}
	}
	// Only the Primary is told to keep its reads rare: the roles that verify work need to look at a canvas.
	if !strings.Contains(CanvasVerifyGuidance, "Reads are discouraged") {
		t.Errorf("primary canvas guidance must discourage reads: %s", CanvasVerifyGuidance)
	}
	if strings.Contains(CanvasInspectGuidance, "Reads are discouraged") {
		t.Errorf("inspect guidance must not discourage the reads a verifying role needs: %s", CanvasInspectGuidance)
	}
}

// Every read-only role is told that another agent's artifact is invisible to it, and that verification comes from
// the worker's report or artifact summary rather than from opening the canvas.
func TestReadOnlyCanvasGuidancePointsAtTheOwnerFilter(t *testing.T) {
	for name, guidance := range map[string]string{"verify": CanvasVerifyGuidance, "inspect": CanvasInspectGuidance} {
		if !strings.Contains(guidance, "Artifacts owned by another agent are not visible to you") {
			t.Errorf("%s guidance must say another agent's artifacts are invisible: %s", name, guidance)
		}
		if !strings.Contains(guidance, "report or artifact summary") {
			t.Errorf("%s guidance must point to the worker's report or artifact summary: %s", name, guidance)
		}
	}
}

// The Primary's static prompt does not promise canvas authoring and does not claim it can read a worker's canvas:
// the owner filter hides another agent's artifacts, so the prompt states that they are not visible.
func TestPrimaryStaticPromptDoesNotPromiseCanvasAuthoring(t *testing.T) {
	primary := For(agent.Primary)
	if strings.Contains(primary, "relo_canvas creates") {
		t.Fatal("primary prompt still promises that it creates canvas artifacts")
	}
	if strings.Contains(primary, "reads a worker's canvas") {
		t.Fatal("primary prompt still claims it can read a worker's canvas")
	}
	if !strings.Contains(primary, "another agent's artifacts are not visible to you") {
		t.Fatal("primary prompt does not state that another agent's artifacts are not visible")
	}
}

// The guidance a role gets agrees with the actions the role may perform.
func TestCanvasGuidanceAgreesWithTheRolePolicy(t *testing.T) {
	for _, role := range []agent.Role{agent.Primary, agent.Head, agent.Probe, agent.Reviewer} {
		guidance := CanvasGuidanceFor(role)
		for _, action := range agent.CanvasActionsForRole(role) {
			if action == agent.CanvasActionCreate || action == agent.CanvasActionUpdate {
				t.Errorf("%s may %s but its guidance is read-only: %s", role, action, guidance)
			}
		}
	}
}
