package memory

import (
	"testing"
	"time"
)

func promote(t *testing.T, s *Store, e Entry) Entry {
	t.Helper()
	out, err := s.UserPromote(e)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	return out
}

// Recall ranks exact file, symbol, kind and keyword matches deterministically and never returns a candidate.
func TestRecallRanksVerifiedMatches(t *testing.T) {
	s, _ := phase2Store(t)
	byFile := promote(t, s, Entry{Kind: "fact", Title: "Alpha", Body: "unrelated content",
		Files: []string{"core/internal/memory/recall.go"}})
	bySymbol := promote(t, s, Entry{Kind: "fact", Title: "Beta", Body: "unrelated content",
		Symbols: []string{"Store.RecallScoped"}})
	byKeyword := promote(t, s, Entry{Kind: "fact", Title: "Recall ranking memory", Body: "deterministic order"})
	byKind := promote(t, s, Entry{Kind: "pitfall", Title: "Gamma", Body: "unrelated content"})
	// A candidate with the strongest file match must still be invisible.
	if _, err := s.ProposeCandidate(Entry{Kind: "fact", Title: "Candidate", Body: "recall ranking memory",
		Files: []string{"core/internal/memory/recall.go"}}); err != nil {
		t.Fatal(err)
	}

	got := s.RecallScoped(RecallQuery{
		Text:    "recall ranking memory deterministic order",
		Files:   []string{"core/internal/memory/recall.go"},
		Symbols: []string{"Store.RecallScoped"},
		Kinds:   []string{"pitfall"},
	})
	want := []string{byFile.ID, bySymbol.ID, byKeyword.ID, byKind.ID}
	if len(got) != len(want) {
		t.Fatalf("recall = %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("rank %d = %s, want %s (order %v)", i, got[i].ID, id, ids(got))
		}
	}
}

// A retired, stale, deleted or candidate entry never reaches recall, even when it matches the query exactly.
func TestRecallExcludesCandidateRetiredStaleDeleted(t *testing.T) {
	s, _ := phase2Store(t)
	live := promote(t, s, Entry{Kind: "fact", Title: "Shared recall topic", Body: "same text"})
	if _, err := s.ProposeCandidate(Entry{Kind: "fact", Title: "Shared recall topic", Body: "same text"}); err != nil {
		t.Fatal(err)
	}
	retired := promote(t, s, Entry{Kind: "fact", Title: "Shared recall topic", Body: "same text"})
	if _, err := s.UserRetire(retired.ID, retired.Revision); err != nil {
		t.Fatal(err)
	}
	stale := promotedVerified(t, s, "Shared recall topic", "ev_stale", Entry{})
	if _, err := s.MarkStale(stale.ID, stale.Revision, "body changed"); err != nil {
		t.Fatal(err)
	}
	deleted := promote(t, s, Entry{Kind: "fact", Title: "Shared recall topic", Body: "same text"})
	if err := s.UserDelete(deleted.ID, deleted.Revision); err != nil {
		t.Fatal(err)
	}

	got := s.Recall("shared recall topic")
	if len(got) != 1 || got[0].ID != live.ID {
		t.Fatalf("recall = %v, want only verified %s", ids(got), live.ID)
	}
}

// Equal scores order newest promotion first, then ascending ID.
func TestRecallTieOrderIsNewestThenID(t *testing.T) {
	s, advance := phase2Store(t)
	older := promote(t, s, Entry{Kind: "fact", Title: "Shared recall", Body: "same text"})
	advance(time.Hour)
	newer := promote(t, s, Entry{Kind: "fact", Title: "Shared recall", Body: "same text"})

	got := s.Recall("shared recall")
	if len(got) != 2 || got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Fatalf("tie order = %v, want %s then %s", ids(got), newer.ID, older.ID)
	}
}

// One keyword is enough only when the query has just one distinctive word; otherwise two must match.
func TestRecallRequiresPathSymbolOrTwoKeywords(t *testing.T) {
	s, _ := phase2Store(t)
	e := promote(t, s, Entry{Kind: "fact", Title: "Recall", Body: "x"})

	if got := s.Recall("recall unrelated"); len(got) != 0 {
		t.Fatalf("single keyword match recalled %v", ids(got))
	}
	if got := s.Recall("recall"); len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("single-word query = %v, want %s", ids(got), e.ID)
	}
}

func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}
