package persistence

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// countRows counts rows in one table for a session and event name.
func countRows(t *testing.T, s *Store, table, session, name string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE session_id=? AND name=?`, session, name).Scan(&n); err != nil {
		t.Fatalf("count %s %s: %v", table, name, err)
	}
	return n
}

// The three full-snapshot kinds keep at most one row per session and kind (in event_latest) no matter how many
// times they are emitted, while a non-snapshot kind still appends one row per emit. The reader still returns the
// newest snapshot and the appended rows.
func TestSnapshotEventsKeepOneRowPerSessionAndKind(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "relo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	write := func(name string, n int, tag string) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := s.LogEvent("s1", name, map[string]any{"tag": tag, "i": i}); err != nil {
				t.Fatalf("LogEvent(%s): %v", name, err)
			}
		}
	}

	// Repeated snapshot writes: one row per kind, none appended to events.
	write("session.updated", 5, "s")
	write("changes.updated", 4, "c")
	write("cache.updated", 3, "u")
	for _, name := range []string{"session.updated", "changes.updated", "cache.updated"} {
		if got := countRows(t, s, "event_latest", "s1", name); got != 1 {
			t.Fatalf("event_latest %s rows = %d, want 1", name, got)
		}
		if got := countRows(t, s, "events", "s1", name); got != 0 {
			t.Fatalf("events %s rows = %d, want 0 (snapshots must not append)", name, got)
		}
	}

	// A non-snapshot kind still appends exactly as before.
	write("agent.completed", 3, "a")
	if got := countRows(t, s, "events", "s1", "agent.completed"); got != 3 {
		t.Fatalf("events agent.completed rows = %d, want 3", got)
	}
	if got := countRows(t, s, "event_latest", "s1", "agent.completed"); got != 0 {
		t.Fatalf("event_latest agent.completed rows = %d, want 0", got)
	}

	// The stored snapshot is the newest one written (i = 4 of 0..4), not a stale copy.
	var payload string
	if err := s.db.QueryRow(`SELECT payload FROM event_latest WHERE session_id='s1' AND name='session.updated'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatal(err)
	}
	if m["i"].(float64) != 4 {
		t.Fatalf("stored session.updated = %v, want the newest (i=4)", m)
	}

	// The reader surfaces the newest snapshot together with the appended rows.
	rows, err := s.Events("s1", 200)
	if err != nil {
		t.Fatal(err)
	}
	var sawSnapshot, sawAppended bool
	for _, r := range rows {
		var p map[string]any
		if err := json.Unmarshal(r, &p); err != nil {
			t.Fatal(err)
		}
		switch p["tag"] {
		case "s":
			sawSnapshot = true
		case "a":
			sawAppended = true
		}
	}
	if !sawSnapshot {
		t.Fatalf("Events() dropped the newest session.updated snapshot: %v", rows)
	}
	if !sawAppended {
		t.Fatalf("Events() dropped the appended agent.completed rows: %v", rows)
	}

	// EventRows (used by the audit/log tests) exposes the snapshot kind with its name.
	named, err := s.EventRows("s1", 200)
	if err != nil {
		t.Fatal(err)
	}
	var sawNamedSnapshot bool
	for _, r := range named {
		if r.Name == "session.updated" {
			sawNamedSnapshot = true
		}
	}
	if !sawNamedSnapshot {
		t.Fatalf("EventRows() dropped the session.updated snapshot: %+v", named)
	}
}

// The change is additive: a database that already carries append-only snapshot rows (as an older build wrote them)
// opens unchanged, keeps every existing row, and only diverts new snapshot writes into event_latest.
func TestSnapshotMigrationIsAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relo.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate rows written by an older build: full-state snapshots appended straight into events.
	for i := 0; i < 3; i++ {
		if _, err := s.db.Exec(`INSERT INTO events(session_id, name, payload, created_at) VALUES('s1','session.updated','{"old":true}',1)`); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening runs migrate() again: idempotent and additive, and user_version stays at 2.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("user_version = %d %v, want 2", version, err)
	}
	oldCount := func() int {
		t.Helper()
		return countRows(t, s, "events", "s1", "session.updated")
	}
	if got := oldCount(); got != 3 {
		t.Fatalf("existing session.updated rows after reopen = %d, want 3 preserved", got)
	}

	// A new snapshot write lands in event_latest and leaves the old rows untouched.
	if err := s.LogEvent("s1", "session.updated", map[string]any{"new": true}); err != nil {
		t.Fatal(err)
	}
	if got := oldCount(); got != 3 {
		t.Fatalf("existing session.updated rows after a new write = %d, want 3 preserved", got)
	}
	if got := countRows(t, s, "event_latest", "s1", "session.updated"); got != 1 {
		t.Fatalf("event_latest session.updated rows = %d, want 1", got)
	}
}
