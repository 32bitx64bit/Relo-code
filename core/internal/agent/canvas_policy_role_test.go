package agent

import "testing"

// rolePolicyHas reports whether an action is in a role's action set.
func rolePolicyHas(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

// Authoring is reserved for builder roles. The Primary and the roles that only investigate may not write source.
func TestCanvasRolePolicyAuthoringIsBuilderOnly(t *testing.T) {
	builders := []Role{Worker, Architect, Debug, Test, Integration}
	for _, role := range builders {
		for _, action := range []string{CanvasActionCreate, CanvasActionUpdate} {
			if !CanvasActionAllowed(role, action) {
				t.Errorf("builder %q may not %s", role, action)
			}
		}
	}
	others := []Role{Primary, Head, Probe, Reviewer, Role(""), Role("designer")}
	for _, role := range others {
		for _, action := range []string{CanvasActionCreate, CanvasActionUpdate} {
			if CanvasActionAllowed(role, action) {
				t.Errorf("role %q may %s", role, action)
			}
		}
	}
}

// Every role keeps the read-only inspection path.
func TestCanvasRolePolicyInspectionIsAllowedForEveryRole(t *testing.T) {
	for _, role := range []Role{Primary, Head, Worker, Probe, Reviewer, Architect, Debug, Test, Integration, Role(""), Role("designer")} {
		for _, action := range []string{CanvasActionList, CanvasActionOpen, CanvasActionInspect} {
			if !CanvasActionAllowed(role, action) {
				t.Errorf("role %q may not %s", role, action)
			}
		}
	}
}

// A builder role keeps every canvas action, so the authoring path is unchanged for it.
func TestCanvasRolePolicyBuilderKeepsEveryAction(t *testing.T) {
	for _, role := range []Role{Worker, Architect, Debug, Test, Integration} {
		actions := CanvasActionsForRole(role)
		for _, action := range canvasAllActions {
			if !rolePolicyHas(actions, action) {
				t.Errorf("builder %q lost the %s action: %v", role, action, actions)
			}
		}
	}
}

// The policy knows exactly the canvas actions the relo_canvas tool accepts: eight names, none added and none
// renamed. A policy that named an action the transport rejects would advertise a call that can never work.
func TestCanvasRolePolicyKnowsOnlyTheTransportActions(t *testing.T) {
	want := []string{"list", "open", "create", "update", "inspect", "capture", "popout", "close"}
	if len(canvasAllActions) != len(want) {
		t.Fatalf("the policy knows %d actions, the transport accepts %d: %v", len(canvasAllActions), len(want), canvasAllActions)
	}
	for i, action := range want {
		if canvasAllActions[i] != action {
			t.Errorf("action %d = %q, want %q", i, canvasAllActions[i], action)
		}
	}
}

// The exposed action set and the per-action check never disagree.
func TestCanvasRolePolicyExposedSetMatchesCheck(t *testing.T) {
	for _, role := range []Role{Primary, Head, Worker, Probe, Reviewer, Architect, Debug, Test, Integration, Role(""), Role("designer")} {
		exposed := CanvasActionsForRole(role)
		for _, action := range canvasAllActions {
			allowed := CanvasActionAllowed(role, action)
			if allowed != rolePolicyHas(exposed, action) {
				t.Errorf("role %q action %q: allowed=%v but the exposed set says %v", role, action, allowed, rolePolicyHas(exposed, action))
			}
		}
	}
}

// An unknown action is denied for every role: the policy fails closed.
func TestCanvasRolePolicyUnknownActionIsDenied(t *testing.T) {
	for _, action := range []string{"", "delete", "CREATE", "remove"} {
		if CanvasActionAllowed(Worker, action) {
			t.Errorf("builder may %q", action)
		}
	}
}

// An artifact that carries no owner belongs to nobody: only a role that may author artifacts may read one, so
// no other role can pull a foreign artifact's content into its context.
func TestCanvasRolePolicyReadUnownedIsAuthoringOnly(t *testing.T) {
	for _, role := range []Role{Worker, Architect, Debug, Test, Integration} {
		if !CanvasReadUnownedRole(role) {
			t.Errorf("builder %q may not read an unowned artifact", role)
		}
	}
	for _, role := range []Role{Primary, Head, Probe, Reviewer, Role(""), Role("designer")} {
		if CanvasReadUnownedRole(role) {
			t.Errorf("role %q may read an unowned artifact", role)
		}
	}
}

// The action list is a copy: a caller cannot widen another role's permissions by editing the returned slice.
func TestCanvasRolePolicyActionListIsACopy(t *testing.T) {
	first := CanvasActionsForRole(Worker)
	for i := range first {
		first[i] = "delete"
	}
	for _, action := range CanvasActionsForRole(Worker) {
		if action == "delete" {
			t.Fatalf("CanvasActionsForRole(Worker) leaked a mutated action: %v", CanvasActionsForRole(Worker))
		}
	}
}
