package runtime

// ReloWebTool is the MCP tool that drives the user's installed browser for search, page fetch and interactive
// browsing. It is offered to the roles in webRoles only; see WithWeb.
const ReloWebTool = "relo_web"

// webRoles are the roles that may browse. The Primary never browses: it plans from curated context. Other roles keep
// the builtin lookup tools as before, and the core refuses relo_web for them as well.
var webRoles = map[string]bool{"worker": true, "probe": true, "architect": true}

// WithWeb adds the relo_web allow rule to a tool policy when enabled for an offered role, and turns off the builtin
// lookup tools for that turn: OpenCode's webfetch and websearch, which Claude Code's WebFetch and WebSearch share
// through claudeTools (internal/claudecode/translate.go). Without the offer the policy is returned unchanged.
func WithWeb(tools map[string]bool, role string, enabled bool) map[string]bool {
	if !enabled || !webRoles[role] {
		return tools
	}
	tools[ReloWebTool] = true
	tools["webfetch"], tools["websearch"] = false, false
	return tools
}
