package ctxeng

import (
	"sort"
	"strings"
	"time"
)

type Selection struct {
	Included       []Item
	Dropped        []Dropped
	Compressed     []Compressed
	Tokens         int
	OverflowTokens int
}

type Strategy interface {
	Name() string
	Select(items []Item, budget int, role string, phase Phase, now time.Time) Selection
}

func StrategyByName(name string) Strategy {
	switch strings.ToLower(name) {
	case "fullhistory", "full_history":
		return FullHistory{}
	case "summary":
		return Summary{}
	case "aggressive", "aggressive_refresh":
		return Aggressive{}
	case "role", "role_optimized":
		return RoleOptimized{}
	default:
		return Variable{}
	}
}

type Variable struct{}

func (Variable) Name() string { return "variable" }

func (Variable) Select(items []Item, budget int, role string, phase Phase, now time.Time) Selection {
	scored := scoreAll(items, role, phase, now)
	sortScored(scored)
	return pack(scored, budget, phase)
}

type FullHistory struct{}

func (FullHistory) Name() string { return "full_history" }

func (FullHistory) Select(items []Item, budget int, role string, phase Phase, now time.Time) Selection {
	scored := scoreAll(items, role, phase, now)
	sort.Slice(scored, func(i, j int) bool {
		if !scored[i].item.Created.Equal(scored[j].item.Created) {
			return scored[i].item.Created.Before(scored[j].item.Created)
		}
		return itemKey(scored[i].item) < itemKey(scored[j].item)
	})
	return packItems(scored, budget, phase, true)
}

type Summary struct{}

func (Summary) Name() string { return "summary" }

func (Summary) Select(items []Item, budget int, role string, phase Phase, now time.Time) Selection {
	var keep []scoredItem
	var rest []scoredItem
	for _, it := range items {
		EnsureTokens(&it)
		s := scoredItem{item: it, score: score(it, role, phase, now)}
		if it.Pinned || it.Class == ClassTask || it.Class == ClassFinding || it.Class == ClassReport || it.Source == SourceUser {
			keep = append(keep, s)
		} else {
			rest = append(rest, s)
		}
	}
	sortScored(keep)
	sortScored(rest)
	sel := pack(keep, budget, phase)
	if sel.Tokens < budget {
		extra := pack(rest, budget-sel.Tokens, phase)
		sel.Included = append(sel.Included, extra.Included...)
		sel.Dropped = append(sel.Dropped, extra.Dropped...)
		sel.Compressed = append(sel.Compressed, extra.Compressed...)
		sel.Tokens += extra.Tokens
	} else {
		for _, s := range rest {
			sel.Dropped = append(sel.Dropped, Dropped{Item: s.item, Reason: "summary_omitted"})
		}
	}
	sortItems(sel.Included)
	return sel
}

type Aggressive struct{}

func (Aggressive) Name() string { return "aggressive_refresh" }

func (Aggressive) Select(items []Item, budget int, role string, phase Phase, now time.Time) Selection {
	tight := budget
	if tight > 8000 {
		tight = budget / 2
		if tight < 4000 {
			tight = 4000
		}
	}
	var filtered []Item
	for _, it := range items {
		if it.Class == ClassHistory || it.Class == ClassEphemeral {
			continue
		}
		if it.Phase != "" && it.Phase != phase && !it.Pinned && it.Class != ClassTask && it.Class != ClassFinding {
			continue
		}
		filtered = append(filtered, it)
	}
	return Variable{}.Select(filtered, tight, role, phase, now)
}

type RoleOptimized struct{}

func (RoleOptimized) Name() string { return "role_optimized" }

func (RoleOptimized) Select(items []Item, budget int, role string, phase Phase, now time.Time) Selection {
	var filtered []Item
	for _, it := range items {
		if hideForRole(role, it) && !it.Pinned {
			continue
		}
		filtered = append(filtered, it)
	}
	return Variable{}.Select(filtered, budget, role, phase, now)
}

func hideForRole(role string, it Item) bool {
	switch role {
	case "primary", "head":
		return it.Class == ClassHistory || it.Class == ClassEphemeral || (it.Class == ClassCode && len(it.Content) > 2000)
	case "probe":
		return it.Class == ClassReport && it.Source != SourceParent
	case "reviewer":
		return it.Class == ClassEphemeral
	}
	return it.Class == ClassHistory
}

type scoredItem struct {
	item  Item
	score float64
}

func scoreAll(items []Item, role string, phase Phase, now time.Time) []scoredItem {
	out := make([]scoredItem, 0, len(items))
	for _, it := range items {
		EnsureTokens(&it)
		out = append(out, scoredItem{item: it, score: score(it, role, phase, now)})
	}
	return out
}

func score(it Item, role string, phase Phase, now time.Time) float64 {
	s := it.Relevance
	s += it.Confidence
	s += it.Durability
	if it.Verified {
		s += 2
	}
	if it.Pinned {
		s += 10
	}
	if it.Source == SourceUser {
		s += 4
	}
	if it.Class == ClassHistory {
		s -= 3
	}
	if it.Class == ClassEphemeral {
		s -= 2
	}
	if it.Phase != "" && it.Phase == phase {
		s += 1.5
	}
	if it.Phase != "" && it.Phase != phase && !Durable(it) {
		s -= 2
	}
	if !it.Created.IsZero() && !now.IsZero() {
		age := now.Sub(it.Created).Hours()
		if age < 1 {
			s += 1
		} else if age > 24 {
			s -= 0.5
		}
	}
	if role == "primary" && it.Class == ClassReport {
		s += 2
	}
	if role == "probe" && it.Class == ClassCode {
		s += 1
	}
	return s
}

func pack(scored []scoredItem, budget int, phase Phase) Selection {
	return packItems(scored, budget, phase, false)
}

func sortScored(scored []scoredItem) {
	sort.Slice(scored, func(i, j int) bool {
		pi, pj := Priority(scored[i].item), Priority(scored[j].item)
		if pi != pj {
			return pi < pj
		}
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return itemKey(scored[i].item) < itemKey(scored[j].item)
	})
}

func packItems(scored []scoredItem, budget int, phase Phase, keepHistory bool) Selection {
	if budget <= 0 {
		budget = 16000
	}
	sel := Selection{}
	used := 0
	for _, s := range scored {
		it := s.item
		EnsureTokens(&it)
		must := it.Pinned || Priority(it) == 1
		if it.Phase != "" && it.Phase != phase && !must && it.Class == ClassEphemeral {
			sel.Dropped = append(sel.Dropped, Dropped{Item: it, Reason: "obsolete_phase"})
			continue
		}
		if !must && it.Class == ClassHistory && !keepHistory {
			sel.Dropped = append(sel.Dropped, Dropped{Item: it, Reason: "raw_history"})
			continue
		}
		if used+it.Tokens <= budget {
			sel.Included = append(sel.Included, it)
			used += it.Tokens
			continue
		}
		if must || it.Durability >= 0.8 || it.Class == ClassFinding || it.Class == ClassTask {
			c := CompressItem(it)
			c.Tokens = Tokens(c.Content)
			if used+c.Tokens <= budget || must {
				sel.Compressed = append(sel.Compressed, Compressed{From: it.ID, Content: c.Content, Tokens: c.Tokens})
				sel.Included = append(sel.Included, c)
				used += c.Tokens
				continue
			}
		}
		reason := "budget"
		if it.Class == ClassEphemeral {
			reason = "ephemeral"
		}
		sel.Dropped = append(sel.Dropped, Dropped{Item: it, Reason: reason})
	}
	sel.Tokens = used
	if used > budget {
		sel.OverflowTokens = used - budget
	}
	return sel
}
