package orchestrator

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/relocode/relo/internal/agent"
	ctxeng "github.com/relocode/relo/internal/context"
	"github.com/relocode/relo/internal/permission"
	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/style"
)

// guard runs fn and turns a panic into an error. One bad task must not take the whole core, and every agent running
// in it, down; the stack goes to the log so the bug can still be found.
func (c *Coord) guard(where string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error in %s: %v", where, r)
			if c.Log != nil {
				c.Log.Error("panic recovered", map[string]any{"where": where, "panic": fmt.Sprint(r), "stack": string(debug.Stack())})
			}
		}
	}()
	return fn()
}

func (c *Coord) SetPermission(action permission.Action, decision permission.Decision) {
	if p := c.permissions(); p != nil {
		p.Set(action, decision)
	}
}

func (c *Coord) modelForDifficulty(role agent.Role, difficulty string) string {
	cfg := c.routeConfig()
	model := cfg.For(agent.TierFor(role), role, difficulty)
	if role == agent.Probe || role == agent.Test || role == agent.Reviewer {
		if model == cfg.Primary {
			for _, candidate := range cfg.Ladder() {
				if candidate != cfg.Primary {
					return candidate
				}
			}
		}
	}
	return model
}

func (c *Coord) routeConfig() routing.Config {
	cfg := c.routing()
	if cfg.Primary == "" && cfg.Tier1 == "" && cfg.Tier2 == "" {
		s := c.settings()
		cfg = s.RouteConfig()
	}
	return cfg
}

// Notify emits through the registered emitter (SetEmitter). The legacy Emit field is a no-op default.
func (c *Coord) Notify(name string, payload any) { c.emit(name, payload) }

func (c *Coord) emit(name string, payload any) {
	c.emitMu.RLock()
	fn := c.emitFn
	if fn == nil {
		fn = c.Emit
	}
	c.emitMu.RUnlock()
	if fn != nil {
		fn(name, payload)
	}
}

func (c *Coord) modelFor(role agent.Role) string {
	return c.modelForDifficulty(role, "")
}

func (c *Coord) RespondPermission(id, response string) {
	c.mu.Lock()
	if p := c.pending[id]; p != nil {
		select {
		case p.Reply <- response:
		default:
		}
	}
	c.mu.Unlock()
}

func (c *Coord) RefreshAgent(id, reason string) (*ctxeng.Generation, error) {
	c.syncWorkspaceContext(id)
	gen, err := c.Engine.Refresh(id, reason)
	if err == nil {
		c.Agents.Update(id, func(x *agent.Agent) { x.Generation = gen.Number })
		c.emit("context.refreshed", map[string]any{"agentId": id, "generation": gen.Number, "reason": reason})
	}
	return gen, err
}

func (c *Coord) Redirect(id, instruction string) {
	c.Engine.Ingest(id, []ctxeng.Item{{Class: ctxeng.ClassTask, Source: ctxeng.SourceUser, Title: "Redirect", Content: instruction, Pinned: true, Durability: 1}})
	if _, err := c.RefreshAgent(id, "user redirect"); err != nil {
		c.logWarn("refresh redirected agent", err)
	}
}

func parseRole(s string) agent.Role {
	switch strings.ToLower(s) {
	case "probe":
		return agent.Probe
	case "head":
		return agent.Head
	case "reviewer", "review":
		return agent.Reviewer
	case "architect", "architecture":
		return agent.Architect
	case "debug", "debugger":
		return agent.Debug
	case "test", "verify", "verification":
		return agent.Test
	case "integration", "integrate":
		return agent.Integration
	case "primary":
		return agent.Primary
	default:
		return agent.Worker
	}
}

func phaseFor(r agent.Role) ctxeng.Phase {
	switch r {
	case agent.Probe:
		return ctxeng.PhaseExplore
	case agent.Architect:
		return ctxeng.PhaseDesign
	case agent.Debug:
		return ctxeng.PhaseDebug
	case agent.Test, agent.Reviewer:
		return ctxeng.PhaseVerify
	case agent.Integration:
		return ctxeng.PhaseIntegrate
	default:
		return ctxeng.PhaseImplement
	}
}

func workerBudget(r agent.Role, base int) int {
	if base <= 0 {
		base = 24000
	}
	if (r == agent.Probe || r == agent.Test) && base > 12000 {
		return 12000
	}
	return base
}

func verifyLines(r RawReport) []string {
	var out []string
	for _, v := range r.Verify {
		out = append(out, v.Command+" "+v.Status)
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (c *Coord) styleFor(a agent.Agent) style.Resolved {
	return style.Resolve(c.settings().Efficiency, a.Role, a.Difficulty, a.Attempt, a.Budget)
}

func isContextLimit(err string) bool {
	e := strings.ToLower(err)
	return strings.Contains(e, "context") && (strings.Contains(e, "length") || strings.Contains(e, "window") || strings.Contains(e, "limit") || strings.Contains(e, "too long"))
}

// deliversChanges reports whether the role's result is a change to the code, so that finishing without one means
// it did not do its job. Probes, tests, reviewers and designers deliver an answer.
func deliversChanges(role agent.Role) bool {
	switch role {
	case agent.Worker, agent.Debug, agent.Integration:
		return true
	}
	return false
}
