package agent

// Canvas action names. They are exactly the action strings the relo_canvas tool accepts, so a request can be
// checked against this policy without a second translation table and without the policy naming an action the
// transport would reject.
const (
	CanvasActionList    = "list"
	CanvasActionOpen    = "open"
	CanvasActionCreate  = "create"
	CanvasActionUpdate  = "update"
	CanvasActionInspect = "inspect"
	CanvasActionCapture = "capture"
	CanvasActionPopout  = "popout"
	CanvasActionClose   = "close"
)

var (
	// canvasAllActions lists every action the policy knows, in a stable order.
	canvasAllActions = []string{
		CanvasActionList, CanvasActionOpen, CanvasActionCreate, CanvasActionUpdate,
		CanvasActionInspect, CanvasActionCapture, CanvasActionPopout, CanvasActionClose,
	}
	// canvasReadOnlyActions are the inspection actions every role may use: listing the artifacts, opening one for
	// the user and reading its source. They do not write artifact source and do not drive the user's canvas panel.
	canvasReadOnlyActions = []string{
		CanvasActionList, CanvasActionOpen, CanvasActionInspect,
	}
)

// CanvasActionsForRole returns the canvas actions a role may perform, in a stable order. Builder roles get every
// action; every other role, including an unknown or empty role, gets the read-only inspection set.
func CanvasActionsForRole(role Role) []string {
	out := make([]string, 0, len(canvasAllActions))
	for _, action := range canvasAllActions {
		if CanvasActionAllowed(role, action) {
			out = append(out, action)
		}
	}
	return out
}

// CanvasActionAllowed reports whether a role may perform a canvas action. Unknown actions are denied for every role.
// A builder role may do everything; any other role, the Primary included, may only inspect.
func CanvasActionAllowed(role Role, action string) bool {
	if !knownCanvasAction(action) {
		return false
	}
	if CanvasAuthoringRole(role) {
		return true
	}
	for _, readOnly := range canvasReadOnlyActions {
		if readOnly == action {
			return true
		}
	}
	return false
}

// CanvasAuthoringRole reports whether a role may write canvas artifacts. It requires a known role: Mutating answers
// true for every unrecognized role, and an unknown role must not author.
func CanvasAuthoringRole(role Role) bool {
	return knownRole(role) && Mutating(role)
}

// CanvasReadUnownedRole reports whether a role may read a canvas artifact that carries no owner. Such an artifact
// was written before ownership was recorded, so it belongs to nobody and its content is not the caller's: only a
// role that may author artifacts may read one, so no other role can pull a foreign artifact into its context.
func CanvasReadUnownedRole(role Role) bool {
	return CanvasAuthoringRole(role)
}

// knownRole reports whether the role is one of the roles this package defines. The comparison is exact, so a
// differently spelled role is unknown rather than silently mapped onto a builder role.
func knownRole(role Role) bool {
	switch role {
	case Primary, Head, Worker, Probe, Reviewer, Architect, Debug, Test, Integration:
		return true
	default:
		return false
	}
}

// knownCanvasAction reports whether the action is one the policy covers.
func knownCanvasAction(action string) bool {
	for _, known := range canvasAllActions {
		if known == action {
			return true
		}
	}
	return false
}
