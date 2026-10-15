package memory

import "testing"

func TestDedupAndUnverified(t *testing.T) {
	s := New()
	a := s.Upsert(Entry{Kind: "convention", Title: "Tests", Body: "bun test", Verified: false, Source: "unverified_hypothesis"})
	if a.Verified {
		t.Fatal("hypothesis verified")
	}
	b := s.Upsert(Entry{Kind: "convention", Title: "Tests", Body: "bun test from packages/opencode"})
	if b.ID != a.ID {
		t.Fatal("expected dedup")
	}
	if b.Verified {
		t.Fatal("dedup must not verify")
	}
	c := s.Upsert(Entry{Kind: "convention", Title: "Tests", Verified: true})
	if !c.Verified {
		t.Fatal("explicit verify should stick")
	}
}

func TestBound(t *testing.T) {
	s := New()
	for i := 0; i < Bound+50; i++ {
		s.Upsert(Entry{Kind: "note", Title: idTitle(i), Body: "x", Score: float64(i)})
	}
	if n := len(s.List()); n > Bound {
		t.Fatalf("unbounded memory %d", n)
	}
}

func idTitle(i int) string {
	return string(rune('a'+(i%26))) + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

func TestRecallIgnoresWordsThatSayNothingAboutTheRequest(t *testing.T) {
	s := New()
	for i := 0; i < 20; i++ {
		s.Upsert(Entry{Kind: "report", Title: "Task " + idTitle(i), Body: "I looked at the files and it looks good", Verified: true})
	}
	if got := s.Recall("ALright looks good. I approve"); len(got) != 0 {
		t.Fatalf("a reply that names nothing recalled %d entries", len(got))
	}
}

func TestRecallReturnsTheBestFewAndClipsThem(t *testing.T) {
	s := New()
	s.Upsert(Entry{Kind: "report", Title: "Token saving names", Body: "Renamed the efficiency tab labels in webview main.js. " + repeat("x", 2000), Verified: true})
	s.Upsert(Entry{Kind: "report", Title: "Efficiency labels", Body: "efficiency tab labels live in webview/src/main.js"})
	s.Upsert(Entry{Kind: "report", Title: "Unrelated", Body: "the database migration runs at startup"})
	for i := 0; i < 8; i++ {
		s.Upsert(Entry{Kind: "report", Title: "More " + idTitle(i), Body: "efficiency tab labels " + idTitle(i)})
	}
	got := s.Recall("Propose renames for the efficiency tab labels")
	if len(got) != RecallLimit {
		t.Fatalf("recalled %d entries, want the limit %d", len(got), RecallLimit)
	}
	for _, e := range got {
		if e.Title == "Unrelated" {
			t.Fatal("recalled an entry that matches nothing in the request")
		}
		if n := len([]rune(e.Body)); n > RecallChars+1 {
			t.Fatalf("body is %d characters, want at most %d", n, RecallChars)
		}
	}
}

func TestRecallWithASingleDistinctiveWordNeedsOnlyThatWord(t *testing.T) {
	s := New()
	s.Upsert(Entry{Kind: "note", Title: "Build", Body: "make dev builds and installs the extension"})
	if got := s.Recall("rebuild?"); len(got) != 0 {
		t.Fatalf("'rebuild' is not a word of the entry: %v", got)
	}
	if got := s.Recall("installs?"); len(got) != 1 {
		t.Fatalf("recalled %d entries for 'installs', want 1", len(got))
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
