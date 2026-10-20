package agent

import "testing"

// create and update are denied for every restricted role, including the Primary and an unknown role.
func TestCanvasPolicyDeniesAuthoringForRestrictedRoles(t *testing.T) {
	for _, role := range []Role{Primary, Head, Probe, Reviewer, Role("designer"), Role("")} {
		for _, action := range []string{CanvasActionCreate, CanvasActionUpdate} {
			if CanvasActionAllowed(role, action) {
				t.Errorf("role %q may %s", role, action)
			}
		}
	}
}

// A builder role keeps create and update.
func TestCanvasPolicyAllowsAuthoringForBuilderRole(t *testing.T) {
	for _, action := range []string{CanvasActionCreate, CanvasActionUpdate} {
		if !CanvasActionAllowed(Worker, action) {
			t.Errorf("builder worker may not %s", action)
		}
	}
}

// The Primary's exposed set is exactly the read-only inspection actions.
func TestCanvasPolicyPrimaryExposesOnlyReadActions(t *testing.T) {
	got := CanvasActionsForRole(Primary)
	want := []string{CanvasActionList, CanvasActionOpen, CanvasActionInspect}
	if len(got) != len(want) {
		t.Fatalf("primary actions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("primary actions = %v, want %v", got, want)
		}
	}
}
