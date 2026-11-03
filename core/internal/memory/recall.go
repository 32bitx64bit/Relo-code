package memory

// Scoped recall (design §5): only verified, current project memory that matches the caller's task, paths, symbols or
// kind reaches a prompt. Ranking is deterministic and never uses a model-supplied score.

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// Ranking weights and limits from design §5.
const (
	scoreFileMatch     = 8
	scoreSymbolMatch   = 6
	scoreKindMatch     = 4
	scoreKeywordMatch  = 1
	scoreFreshEvidence = 1
	maxKeywordScore    = 5
	freshEvidenceAge   = 30 * 24 * time.Hour
)

// RecallQuery is the sanitized retrieval input derived from the current user text or task description and the paths
// and symbols the task explicitly names. It is not a whole conversation.
type RecallQuery struct {
	Text    string
	Files   []string
	Symbols []string
	Kinds   []string
}

// RecallScoped returns up to RecallLimit verified entries that are relevant to q, ranked by exact file, exact symbol,
// exact kind, distinctive keyword matches and fresh evidence. Ties go to the newest promotion and then ascending ID.
// Candidates, retired, stale and deleted entries are never returned.
func (s *Store) RecallScoped(q RecallQuery) []Entry {
	return s.RecallScopedWhere(q, nil)
}

// RecallScopedWhere is RecallScoped with a caller check. keep is called on each eligible entry in rank order, before the
// RecallLimit cap, so entries it rejects (for example stale in the caller's view) do not use up the limit. keep must not
// call back into the Store: the candidate rows are still open while it runs. A nil keep accepts every entry.
func (s *Store) RecallScopedWhere(q RecallQuery, keep func(Entry) bool) []Entry {
	if s.err != nil {
		return nil
	}
	files := pathSet(q.Files)
	symbols := symbolSet(q.Symbols)
	kinds := canonicalSet(q.Kinds)
	keywords := distinctiveWords(q.Text)
	now := s.now()

	rows, err := s.db.Query(`SELECT payload, revision, state FROM memory WHERE workspace=? AND state=? ORDER BY id`, s.ws, StateVerified)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type hit struct {
		entry Entry
		score int
		at    time.Time
	}
	hits := []hit{}
	for rows.Next() {
		var payload, state string
		var rev int64
		if err := rows.Scan(&payload, &rev, &state); err != nil {
			return nil
		}
		e, err := decode(payload, rev, state)
		if err != nil {
			continue
		}
		// A row that would fail intake (for example one written with the guard lifted) is not safe to inject.
		if validate(e) != nil {
			continue
		}
		score, matched, fileHits, symbolHits := rankEntry(e, files, symbols, kinds, keywords)
		if !eligible(len(keywords), matched, fileHits, symbolHits, kindMatch(e, kinds, keywords)) {
			continue
		}
		if freshEvidence(e, now) {
			score += scoreFreshEvidence
		}
		hits = append(hits, hit{entry: e, score: score, at: promotionTime(e)})
	}
	if err := rows.Err(); err != nil {
		return nil
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if !hits[i].at.Equal(hits[j].at) {
			return hits[i].at.After(hits[j].at)
		}
		return hits[i].entry.ID < hits[j].entry.ID
	})
	out := make([]Entry, 0, RecallLimit)
	for _, h := range hits {
		if keep != nil && !keep(h.entry) {
			continue
		}
		out = append(out, h.entry)
		if len(out) == RecallLimit {
			break
		}
	}
	return out
}

// rankEntry scores one entry and reports how many keyword, file and symbol matches it has.
func rankEntry(e Entry, files, symbols, kinds, keywords map[string]bool) (score, matched, fileHits, symbolHits int) {
	for _, f := range e.Files {
		if files[pathKey(f)] {
			fileHits++
		}
	}
	for _, sym := range e.Symbols {
		if symbols[symbolKey(sym)] {
			symbolHits++
		}
	}
	if kindMatch(e, kinds, keywords) {
		score += scoreKindMatch
	}
	words := distinctiveWords(e.Title + " " + e.Body)
	n := 0
	for _, k := range sortedKeys(keywords) {
		if words[k] {
			n++
			if n == maxKeywordScore {
				break
			}
		}
	}
	score += fileHits*scoreFileMatch + symbolHits*scoreSymbolMatch + n*scoreKeywordMatch
	return score, n, fileHits, symbolHits
}

// eligible applies the match threshold of design §5: an exact path, symbol or kind match, otherwise two distinctive
// keywords (one when the query has only one distinctive word).
func eligible(keywords, matched, fileHits, symbolHits int, kind bool) bool {
	if fileHits > 0 || symbolHits > 0 || kind {
		return true
	}
	need := 2
	if keywords == 1 {
		need = 1
	}
	return keywords >= need && matched >= need
}

// kindMatch reports an explicit kind query, or a distinctive query word that names the entry's kind.
func kindMatch(e Entry, kinds, keywords map[string]bool) bool {
	k := strings.ToLower(e.Kind)
	if kinds[k] {
		return true
	}
	return keywords[k]
}

// promotionTime is the tie breaker: the time the entry became permanent, or its last update for a direct user write.
func promotionTime(e Entry) time.Time {
	if e.Promotion != nil && !e.Promotion.PromotedAt.IsZero() {
		return e.Promotion.PromotedAt
	}
	return e.Updated
}

// freshEvidence reports evidence observed within the last 30 days. It is only a ranking signal; view-bound validity is
// checked separately before injection.
func freshEvidence(e Entry, now time.Time) bool {
	for _, ev := range e.Evidence {
		if ev.ObservedAt.IsZero() || ev.ObservedAt.After(now) {
			continue
		}
		if now.Sub(ev.ObservedAt) < freshEvidenceAge {
			return true
		}
	}
	return false
}

func canonical(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// pathKey normalizes a workspace-relative path for an exact query match.
func pathKey(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, `\`, "/")
	s = strings.TrimPrefix(s, "./")
	return s
}

// symbolKey trims a symbol, keeping its case because identifiers are case-sensitive.
func symbolKey(s string) string { return strings.TrimSpace(s) }

func pathSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		if k := pathKey(v); k != "" {
			out[k] = true
		}
	}
	return out
}

func symbolSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		if k := symbolKey(v); k != "" {
			out[k] = true
		}
	}
	return out
}

func canonicalSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		if c := canonical(v); c != "" {
			out[c] = true
		}
	}
	return out
}

// wordsIgnored are frequent words that cannot identify a memory on their own.
var wordsIgnored = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true, "this": true,
	"into": true, "then": true, "than": true, "when": true, "where": true, "what": true, "which": true,
	"but": true, "not": true, "you": true, "your": true, "our": true, "are": true, "was": true,
	"were": true, "will": true, "would": true, "should": true, "could": true, "can": true, "may": true,
	"its": true, "it": true, "is": true, "of": true, "to": true, "in": true, "on": true, "at": true,
	"by": true, "as": true, "an": true, "or": true, "if": true, "be": true, "we": true, "they": true,
	"all": true, "any": true, "has": true, "have": true, "had": true, "how": true, "use": true,
}

// distinctiveWords converts text to the set of lowercase words that can match a memory.
func distinctiveWords(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(w)) < 3 || wordsIgnored[w] {
			continue
		}
		out[w] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
