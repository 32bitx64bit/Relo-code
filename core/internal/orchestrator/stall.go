package orchestrator

import (
	"strings"
	"time"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/routing"
)

// stallMemory is how long a model that went silent stays out of new work when a backup exists. A route that did not
// answer for five minutes is rarely back a minute later: in one session five reviewers in a row were sent to the
// same dead route and each spent fifteen minutes on it.
const stallMemory = 10 * time.Minute

// routeLevel is the difficulty an agent is routed by: its own, else its role's default. Roles with no difficulty on
// the task (reviewers, probes) still have a routing row, so they have a backup too.
func routeLevel(a agent.Agent) string {
	if level := routing.NormalizeDifficulty(a.Difficulty); level != "" {
		return level
	}
	return agent.DefaultDifficulty(a.Role)
}

// backupModel is the backup configured for the agent's difficulty, or "".
func (c *Coord) backupModel(a agent.Agent) string {
	return c.routing().BackupFor(routeLevel(a), a.Model, a.Model)
}

// stallFailover is the model an agent moves to when its own goes silent: the configured backup, else the next model
// up the routing ladder. It is empty for the Primary, and when the agent has no other model to go to. The ladder
// never lands a read-only role on the Primary's model, which is the most expensive one.
func (c *Coord) stallFailover(a agent.Agent) string {
	if a.Role == agent.Primary {
		return ""
	}
	if backup := c.backupModel(a); backup != "" {
		return backup
	}
	cfg := c.routeConfig()
	next, ok := cfg.Escalate(a.Model)
	if !ok || next == a.Model || next == cfg.Primary {
		return ""
	}
	return next
}

// stallRecoveries is how many times a silent model call is interrupted and the agent told to carry on before the
// agent is failed. An agent that has done almost nothing and has another model to go to is not worth nudging: the
// nudges wait out five more minutes each on a route that is not answering.
func (c *Coord) stallRecoveries(a agent.Agent) int {
	if a.FallbackFrom == "" && c.agentSteps(a.ID) < earlySteps && c.stallFailover(a) != "" {
		return 0
	}
	return agentRecoveries
}

func (c *Coord) agentSteps(id string) int {
	if cur, ok := c.Agents.Get(id); ok {
		return cur.Steps
	}
	return 0
}

// noteStall remembers that model went silent, so new work avoids it for stallMemory.
func (c *Coord) noteStall(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stalledAt == nil {
		c.stalledAt = map[string]time.Time{}
	}
	c.stalledAt[model] = time.Now()
}

func (c *Coord) recentlyStalled(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.stalledAt[strings.TrimSpace(model)]
	return ok && time.Since(at) < stallMemory
}

// avoidStalled swaps a model that has just gone silent for the backup configured for the work, when there is one that
// has not. Without a backup the model is kept: a different model nobody chose is not a safe default for a new task.
func (c *Coord) avoidStalled(role agent.Role, difficulty, model string) string {
	if role == agent.Primary || !c.recentlyStalled(model) {
		return model
	}
	probe := agent.Agent{Role: role, Difficulty: difficulty, Model: model}
	if backup := c.backupModel(probe); backup != "" && !c.recentlyStalled(backup) {
		return backup
	}
	return model
}
