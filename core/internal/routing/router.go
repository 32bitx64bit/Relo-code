package routing

import (
	"sort"
	"strings"

	"github.com/relocode/relo/internal/agent"
	"github.com/relocode/relo/internal/runtime"
)

const (
	Trivial = "trivial"
	Easy    = "easy"
	Medium  = "medium"
	Complex = "complex"
	Hard    = "hard"
	Expert  = "expert"
)

// Routing modes. Simple uses only Primary/Tier1/Tier2; Advanced also honours ByDifficulty.
const (
	ModeSimple   = "simple"
	ModeAdvanced = "advanced"
)

// NormalizeMode maps any unknown value to simple, the default.
func NormalizeMode(m string) string {
	if strings.ToLower(strings.TrimSpace(m)) == ModeAdvanced {
		return ModeAdvanced
	}
	return ModeSimple
}

type Config struct {
	Primary      string            `json:"primary"`
	Tier1        string            `json:"tier1"`
	Tier2        string            `json:"tier2"`
	ByDifficulty map[string]string `json:"byDifficulty"`
	// Mode "advanced" enables ByDifficulty. Empty or "simple" ignores it.
	Mode string `json:"mode,omitempty"`
}

// byLevel returns the explicit per-difficulty model, or "" when the mode is not advanced.
func (c Config) byLevel(level string) string {
	if NormalizeMode(c.Mode) != ModeAdvanced {
		return ""
	}
	return strings.TrimSpace(c.ByDifficulty[level])
}

func NormalizeDifficulty(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case Trivial, Easy, Medium, Complex, Hard, Expert:
		return s
	default:
		return ""
	}
}

func (c Config) For(tier agent.Tier, role agent.Role, difficulty string) string {
	if role == agent.Primary {
		return c.Primary
	}
	level := NormalizeDifficulty(difficulty)
	if level == "" {
		level = agent.DefaultDifficulty(role)
	}
	if level == "" {
		level = difficultyForTier(tier)
	}
	if explicit := c.byLevel(level); explicit != "" {
		return explicit
	}
	// Complex was split off hard, so a table without a complex row keeps sending that work where hard goes.
	if level == Complex {
		if explicit := c.byLevel(Hard); explicit != "" {
			return explicit
		}
	}
	wanted := tierForDifficulty(level)
	if wanted < 0 {
		wanted = tierRank(tier)
	}
	entries := c.rankedModels()
	for _, e := range entries {
		if e.rank >= wanted {
			return e.model
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].rank < wanted {
			return entries[i].model
		}
	}
	return ""
}

func (c Config) Ladder() []string {
	entries := c.rankedModels()
	seen := map[string]bool{}
	var ladder []string
	for _, e := range entries {
		if !seen[e.model] {
			seen[e.model] = true
			ladder = append(ladder, e.model)
		}
	}
	return ladder
}

func (c Config) rankedModels() []struct {
	rank  int
	order int
	model string
} {
	type entry struct {
		rank  int
		order int
		model string
	}
	entries := []entry{{0, 0, c.Tier2}, {1, 0, c.Tier1}, {2, 0, c.Primary}}
	levels := []string{Trivial, Easy, Medium, Complex, Hard, Expert}
	for order, level := range levels {
		if model := c.byLevel(level); model != "" {
			entries = append(entries, entry{tierForDifficulty(level), order + 1, model})
		}
	}
	var out []struct {
		rank  int
		order int
		model string
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
		}
		return entries[i].order < entries[j].order
	})
	for _, e := range entries {
		model := strings.TrimSpace(e.model)
		if model != "" {
			out = append(out, struct {
				rank  int
				order int
				model string
			}{e.rank, e.order, model})
		}
	}
	return out
}

func (c Config) Escalate(current string) (string, bool) {
	ladder := c.Ladder()
	for i, model := range ladder {
		if model == strings.TrimSpace(current) && i+1 < len(ladder) {
			return ladder[i+1], true
		}
	}
	return "", false
}

// Seeing returns model when sees(model). Otherwise it returns the nearest ladder model that sees, looking above model
// first and then below, so a task that must look at images gets eyes at the least extra cost. A model outside the
// ladder is replaced by the lowest ladder model that sees. When no model sees, model is returned unchanged.
func (c Config) Seeing(model string, sees func(string) bool) string {
	model = strings.TrimSpace(model)
	if sees == nil || sees(model) {
		return model
	}
	ladder := c.Ladder()
	at := -1
	for i, m := range ladder {
		if m == model {
			at = i
			break
		}
	}
	for i := at + 1; i < len(ladder); i++ {
		if sees(ladder[i]) {
			return ladder[i]
		}
	}
	for i := at - 1; i >= 0; i-- {
		if sees(ladder[i]) {
			return ladder[i]
		}
	}
	return model
}

// EscalateSeeing is Escalate for a task that must look at images: it skips models that cannot see. When current sees
// and nothing above it does, there is no escalation, because a stronger model that cannot see the reference is not
// stronger at this task. When current does not see either, it is plain Escalate.
func (c Config) EscalateSeeing(current string, sees func(string) bool) (string, bool) {
	current = strings.TrimSpace(current)
	if sees == nil {
		return c.Escalate(current)
	}
	ladder := c.Ladder()
	for i, model := range ladder {
		if model != current {
			continue
		}
		for _, next := range ladder[i+1:] {
			if sees(next) {
				return next, true
			}
		}
		if sees(current) {
			return "", false
		}
		break
	}
	return c.Escalate(current)
}

func difficultyForTier(tier agent.Tier) string {
	switch tier {
	case agent.Tier2:
		return Easy
	case agent.TierPrimary:
		return Expert
	default:
		return Medium
	}
}

func tierForDifficulty(level string) int {
	switch level {
	case Trivial, Easy:
		return 0
	case Medium, Complex, Hard:
		return 1
	case Expert:
		return 2
	default:
		return -1
	}
}

func tierRank(tier agent.Tier) int {
	switch tier {
	case agent.Tier2:
		return 0
	case agent.TierPrimary:
		return 2
	default:
		return 1
	}
}

func Split(model string) (provider, id string) {
	model = strings.TrimSpace(model)
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

func Join(p, id string) string {
	if p == "" {
		return id
	}
	return p + "/" + id
}

// PickDefault returns the first model matching one of prefer, in preference order, or "" when none
// match. It never falls back to an arbitrary catalog entry: an unconfigured role must stay empty so the
// user is told to pick a model instead of silently getting one they did not choose.
func PickDefault(models []runtime.Model, prefer []string) string {
	for _, p := range prefer {
		pl := strings.ToLower(p)
		for _, m := range models {
			id := strings.ToLower(Join(m.Provider, m.ID))
			if strings.Contains(id, pl) || strings.Contains(strings.ToLower(m.Name), pl) {
				return Join(m.Provider, m.ID)
			}
		}
	}
	return ""
}
