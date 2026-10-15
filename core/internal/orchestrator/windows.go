package orchestrator

import (
	"strings"

	"github.com/relocode/relo/internal/routing"
	"github.com/relocode/relo/internal/runtime"
)

// RouteConfig builds the routing config for these settings.
func (s Settings) RouteConfig() routing.Config {
	return routing.Config{Primary: s.PrimaryModel, Tier1: s.Tier1Model, Tier2: s.Tier2Model, ByDifficulty: s.ByDifficulty, Mode: s.RoutingMode}
}

// Window is the context window planned for model ("provider/id"): the smaller of the global limit and the
// model maximum. 0 means unknown and no global cap, so callers must leave their budget alone.
func (s Settings) Window(model string) int {
	return runtime.EffectiveWindow(s.ContextLimit, s.Models[strings.TrimSpace(model)].Context)
}

// BudgetFor caps base (an agent's planning budget in tokens) to what fits the model's window at the configured
// max utilization. An unknown window leaves base unchanged. The result is never below 1 so a tiny window
// still yields a valid, if small, budget.
func (s Settings) BudgetFor(model string, base int) int {
	w := s.Window(model)
	if w <= 0 {
		return base
	}
	util := s.MaxUtil
	if util <= 0 || util > 1 {
		util = 0.85
	}
	limit := int(float64(w) * util)
	if limit < 1 {
		limit = 1
	}
	if base <= 0 || limit < base {
		return limit
	}
	return base
}

// EffortFor returns the reasoning effort to request for model, or "" for the model default. The pick must be
// one of the model's advertised efforts; anything else is ignored rather than sent to the endpoint.
func (s Settings) EffortFor(model string) string {
	model = strings.TrimSpace(model)
	pick := strings.TrimSpace(s.ModelEfforts[model])
	if pick == "" {
		return ""
	}
	for _, e := range s.Models[model].Efforts {
		if e == pick {
			return pick
		}
	}
	return ""
}
