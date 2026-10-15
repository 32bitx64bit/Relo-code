package memory

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/relocode/relo/internal/id"
)

const Bound = 200

type Entry struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Files    []string  `json:"files,omitempty"`
	Symbols  []string  `json:"symbols,omitempty"`
	Score    float64   `json:"score"`
	Verified bool      `json:"verified"`
	Source   string    `json:"source"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

type Store struct {
	mu    sync.Mutex
	items []Entry
	now   func() time.Time
}

func New() *Store {
	return &Store{now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) Upsert(e Entry) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if e.ID == "" {
		e.ID = id.New("mem")
	}
	if e.Created.IsZero() {
		e.Created = now
	}
	e.Updated = now
	key := norm(e.Kind + " " + e.Title)
	for i, got := range s.items {
		if got.ID == e.ID || norm(got.Kind+" "+got.Title) == key {
			if e.Verified {
				got.Verified = true
			}
			if e.Body != "" {
				got.Body = e.Body
			}
			got.Updated = now
			got.Score = max(got.Score, e.Score)
			got.Files = union(got.Files, e.Files)
			got.Symbols = union(got.Symbols, e.Symbols)
			if e.Verified && !got.Verified {
				got.Verified = true
			}
			s.items[i] = got
			return got
		}
	}
	if !e.Verified && e.Source == "unverified_hypothesis" {
		e.Verified = false
	}
	s.items = append(s.items, e)
	s.trim()
	return e
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.items {
		if e.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.items))
	copy(out, s.items)
	return out
}

func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.items {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

func (s *Store) Retrieve(keywords []string, files []string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	type hit struct {
		e Entry
		n float64
	}
	var hits []hit
	for _, e := range s.items {
		n := 0.0
		blob := strings.ToLower(e.Title + " " + e.Body + " " + strings.Join(e.Files, " "))
		for _, k := range keywords {
			if k != "" && strings.Contains(blob, strings.ToLower(k)) {
				n += 1
			}
		}
		for _, f := range files {
			for _, got := range e.Files {
				if got == f {
					n += 1.5
				}
			}
		}
		if e.Verified {
			n += 0.5
		}
		n += e.Score
		if n > 0 || len(keywords) == 0 {
			hits = append(hits, hit{e, n})
		}
	}
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].n > hits[i].n {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
	out := make([]Entry, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.e)
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

// What recall puts in front of an agent. Every entry is paid for in every call of the agent that receives it, so
// recall returns the few entries that match the request, not everything that mentions a common word.
const (
	RecallLimit = 4
	RecallChars = 600
)

// Recall returns the entries that are about text: at least two of its distinctive words (one when it has fewer),
// best match first, at most RecallLimit, each body cut to RecallChars.
//
// Retrieve counts any substring, so the word "I" matched nearly every entry and a reply as short as "I approve"
// pulled in nineteen of them, every turn, for the rest of the session.
func (s *Store) Recall(text string) []Entry {
	words := keywords(text)
	if len(words) == 0 {
		return nil
	}
	need := min(2, len(words))
	s.mu.Lock()
	defer s.mu.Unlock()
	type hit struct {
		e Entry
		n float64
	}
	var hits []hit
	for _, e := range s.items {
		blob := strings.ToLower(e.Title + " " + e.Body + " " + strings.Join(e.Files, " "))
		n := 0
		for _, w := range words {
			if strings.Contains(blob, w) {
				n++
			}
		}
		if n < need {
			continue
		}
		score := float64(n) + e.Score
		if e.Verified {
			score += 0.5
		}
		hits = append(hits, hit{e, score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].e.Updated.After(hits[j].e.Updated)
	})
	if len(hits) > RecallLimit {
		hits = hits[:RecallLimit]
	}
	out := make([]Entry, 0, len(hits))
	for _, h := range hits {
		h.e.Body = clip(h.e.Body, RecallChars)
		out = append(out, h.e)
	}
	return out
}

// keywords are the words of text that can say what it is about: four letters or more, no filler, each once.
func keywords(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' && r != '/'
	}) {
		w = strings.Trim(w, "-.")
		if len([]rune(w)) < 4 || filler[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

var filler = map[string]bool{}

func init() {
	for _, w := range strings.Fields("that this with from have will what when your then them they into also been were here there about would could should please just make like want need okay thanks hello alright looks good sure yeah cool nice great yes approve approved proceed continue again still some more other only very much many") {
		filler[w] = true
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

func (s *Store) trim() {
	for len(s.items) > Bound {
		idx := -1
		best := 1e9
		for i, e := range s.items {
			if e.Verified {
				continue
			}
			if e.Score < best {
				best = e.Score
				idx = i
			}
		}
		if idx < 0 {
			idx = 0
		}
		s.items = append(s.items[:idx], s.items[idx+1:]...)
	}
}

func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
