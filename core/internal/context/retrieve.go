package ctxeng

import "strings"

func Retrieve(pool []Item, q Query) []Item {
	if len(pool) == 0 {
		return nil
	}
	// An empty query carries no signal; keep caller-assigned Relevance instead of zeroing it.
	scored := !emptyQuery(q)
	out := make([]Item, 0, len(pool))
	for _, it := range pool {
		if scored {
			// Scored afresh per build (see docs/context-engine.md Retrieval), never accumulated.
			it.Relevance = match(it, q)
		}
		out = append(out, it)
	}
	return out
}

func emptyQuery(q Query) bool {
	return q.Task == "" && len(q.Files) == 0 && len(q.Symbols) == 0 && len(q.Keywords) == 0 && !q.Failures && q.Phase == ""
}

func match(it Item, q Query) float64 {
	s := 0.0
	blob := strings.ToLower(it.Content + " " + it.Title + " " + strings.Join(it.Files, " ") + " " + strings.Join(it.Symbols, " "))
	for _, k := range q.Keywords {
		if k != "" && strings.Contains(blob, strings.ToLower(k)) {
			s += 0.35
		}
	}
	if q.Task != "" && strings.Contains(blob, strings.ToLower(q.Task)) {
		s += 0.4
	}
	for _, f := range q.Files {
		if f == "" {
			continue
		}
		for _, got := range it.Files {
			if got == "" {
				continue
			}
			if got == f || strings.Contains(got, f) || strings.Contains(f, got) {
				s += 0.5
			}
		}
		if f != "" && strings.Contains(blob, strings.ToLower(f)) {
			s += 0.2
		}
	}
	for _, sym := range q.Symbols {
		for _, got := range it.Symbols {
			if strings.EqualFold(got, sym) {
				s += 0.5
			}
		}
	}
	if q.Failures && (it.Source == SourceTest || it.Kind == KindTest || it.Kind == KindBug) {
		s += 0.6
	}
	if q.Phase != "" && it.Phase == q.Phase {
		s += 0.2
	}
	if it.Verified {
		s += 0.2
	}
	if s > 1 {
		s = 1
	}
	return s
}
