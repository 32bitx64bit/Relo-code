package persistence

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	type sess struct {
		ID        string
		Workspace string
		Objective string
		Updated   time.Time
	}
	in := sess{ID: "s1", Workspace: "/tmp/p", Objective: "ship", Updated: time.Now().UTC()}
	if err := s.PutJSON("sessions", in.ID, "", in.Workspace, in); err != nil {
		t.Fatal(err)
	}
	var out sess
	if err := s.Get("sessions", "s1", &out); err != nil {
		t.Fatal(err)
	}
	if out.Objective != "ship" {
		t.Fatalf("%+v", out)
	}
	list, err := s.List("sessions", "/tmp/p")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
}

func TestRejectsUnknownTableAndSecuresFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "data #1.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Get("sessions; DROP TABLE sessions", "x", &struct{}{}); err == nil {
		t.Fatal("expected invalid table rejection")
	}
	if _, err := s.List("other", "x"); err == nil {
		t.Fatal("expected invalid table rejection")
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o", info.Mode().Perm())
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
}

func TestAgentMetricsMigrateFromV1(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relo.db")
	old, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
CREATE TABLE agents (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, payload TEXT NOT NULL, updated_at INTEGER NOT NULL);
INSERT INTO agents(id, session_id, payload, updated_at) VALUES('a1', 's1', '{"id":"a1"}', 1);
PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var steps, cacheRead, cacheWrite int
	var payload string
	if err := s.db.QueryRow(`SELECT payload, steps, cache_read, cache_write FROM agents WHERE id='a1'`).Scan(&payload, &steps, &cacheRead, &cacheWrite); err != nil {
		t.Fatal(err)
	}
	if payload != `{"id":"a1"}` || steps != 0 || cacheRead != 0 || cacheWrite != 0 {
		t.Fatalf("migrated row = %q %d %d %d", payload, steps, cacheRead, cacheWrite)
	}

	type agentRow struct {
		ID         string `json:"id"`
		Steps      int    `json:"steps"`
		CacheRead  int    `json:"cacheRead"`
		CacheWrite int    `json:"cacheWrite"`
	}
	if err := s.PutJSON("agents", "a1", "s1", "", agentRow{ID: "a1", Steps: 3, CacheRead: 80, CacheWrite: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT steps, cache_read, cache_write FROM agents WHERE id='a1'`).Scan(&steps, &cacheRead, &cacheWrite); err != nil {
		t.Fatal(err)
	}
	if steps != 3 || cacheRead != 80 || cacheWrite != 10 {
		t.Fatalf("persisted metrics = %d %d %d", steps, cacheRead, cacheWrite)
	}
}

func TestVWorkspaceMirrorKeepsTheNewestRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relo.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	put := func(agent string, rev uint64, tree string) {
		t.Helper()
		err := s.PutVWorkspace(VWorkspace{Workspace: "/repo", AgentID: agent, SessionID: "s1", BaseCommit: "base1", Revision: rev, Tree: tree, State: "ready", Payload: []byte(`{"dirty":true}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	put("a1", 1, "t1")
	put("a1", 3, "t3")
	put("a1", 2, "t2") // arrives late: must not move the workspace backwards
	put("a2", 1, "u1")
	if err := s.PutVWorkspace(VWorkspace{Workspace: "/other", AgentID: "a1", BaseCommit: "b", Revision: 9, Tree: "x", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListVWorkspaces("/repo")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v %v", rows, err)
	}
	byAgent := map[string]VWorkspace{rows[0].AgentID: rows[0], rows[1].AgentID: rows[1]}
	if got := byAgent["a1"]; got.Revision != 3 || got.Tree != "t3" || string(got.Payload) != `{"dirty":true}` || got.SessionID != "s1" {
		t.Fatalf("a1 = %+v", got)
	}

	// A workspace recreated from a new base starts over, whatever revision it had before.
	if err := s.PutVWorkspace(VWorkspace{Workspace: "/repo", AgentID: "a1", BaseCommit: "base2", Revision: 1, Tree: "n1", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	// It survives a reopen, and deleting one leaves the rest.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, _ = s.ListVWorkspaces("/repo")
	if len(rows) != 2 {
		t.Fatalf("after reopen: %+v", rows)
	}
	for _, r := range rows {
		if r.AgentID == "a1" && (r.BaseCommit != "base2" || r.Revision != 1) {
			t.Fatalf("a recreated workspace kept its old revision: %+v", r)
		}
	}
	if err := s.DeleteVWorkspace("/repo", "a1"); err != nil {
		t.Fatal(err)
	}
	if rows, _ = s.ListVWorkspaces("/repo"); len(rows) != 1 || rows[0].AgentID != "a2" {
		t.Fatalf("after delete: %+v", rows)
	}
	if rows, _ = s.ListVWorkspaces("/other"); len(rows) != 1 {
		t.Fatalf("another repository's mirror was touched: %+v", rows)
	}
}

// The table is additive: a database written before it existed opens, and the schema version is unchanged so an
// older build can still read what a newer one wrote.
func TestVWorkspaceTableNeedsNoSchemaVersion(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("user_version = %d %v, want 2", version, err)
	}
}
