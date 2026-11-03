package persistence

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSplitStoreAndSessionLRU(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "relo.db")
	original := []byte("legacy bytes must remain untouched")
	if err := os.WriteFile(legacy, original, 0o600); err != nil {
		t.Fatal(err)
	}
	wal := []byte("legacy wal bytes must remain untouched")
	if err := os.WriteFile(legacy+"-wal", wal, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var first *sql.DB
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("s%d", i)
		if err := s.PutSession(id, "workspace", map[string]string{"id": id}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutJSON("tasks", "task-"+id, id, "", map[string]string{"id": id}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = s.sessions[id].db
		}
	}
	if len(s.sessions) != 4 || s.sessions["s0"] != nil {
		t.Fatalf("LRU sessions: %v", s.sessions)
	}
	if err := first.Ping(); err == nil {
		t.Fatal("eviction left database open")
	}
	if err := s.PutGeneration("s1", "agent", "generation", 1, map[string]string{"id": "generation"}); err != nil {
		t.Fatal(err)
	}
	var generation map[string]string
	if err := s.Get("generations", "generation", &generation); err != nil || generation["id"] != "generation" {
		t.Fatalf("generation lookup: %v, %v", generation, err)
	}
	var got map[string]string
	if err := s.Get("tasks", "task-s0", &got); err != nil || got["id"] != "s0" {
		t.Fatalf("Get after eviction: %v, %v", got, err)
	}
	h := s.sessions["s0"]
	if err := s.CloseSession("s0"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Ping(); err == nil {
		t.Fatal("CloseSession left database open")
	}
	if err := s.GetIn("tasks", "task-s0", "s0", &got); err != nil || got["id"] != "s0" {
		t.Fatalf("GetIn after close: %v, %v", got, err)
	}
	list, err := s.ListSessions("workspace")
	if err != nil || len(list) != 5 {
		t.Fatalf("ListSessions: %d, %v", len(list), err)
	}
	for _, path := range []string{"catalog.db", "sessions/s0.db"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("legacy changed: %v", err)
	}
	after, err = os.ReadFile(legacy + "-wal")
	if err != nil || !bytes.Equal(wal, after) {
		t.Fatalf("legacy WAL changed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Get("tasks", "task-s0", &got); err != nil || got["id"] != "s0" {
		t.Fatalf("Get after restart: %v, %v", got, err)
	}
}

func TestFreshStoreDoesNotCreateLegacy(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "relo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutSession("s1", "workspace", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "relo.db")); !os.IsNotExist(err) {
		t.Fatalf("legacy file exists: %v", err)
	}
}

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
	info, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o", info.Mode().Perm())
	}
}

func TestAgentMetricsMigrateFromV1(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relo.db")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(dbPath), "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(filepath.Dir(dbPath), "sessions", "s1.db"))
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
	db := testSessionDB(t, s, "s1")
	var steps, cacheRead, cacheWrite int
	var payload string
	if err := db.QueryRow(`SELECT payload, steps, cache_read, cache_write FROM agents WHERE id='a1'`).Scan(&payload, &steps, &cacheRead, &cacheWrite); err != nil {
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
	if err := db.QueryRow(`SELECT steps, cache_read, cache_write FROM agents WHERE id='a1'`).Scan(&steps, &cacheRead, &cacheWrite); err != nil {
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
	if err := testSessionDB(t, s, "s1").QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("user_version = %d %v, want 2", version, err)
	}
}

// The generic store refuses the memory table: rows exist only through memory.Store, with v2 payloads.
func TestGenericAccessRefusesMemoryTable(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutSession("s1", "workspace", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutJSON("memory", "m1", "s1", "", map[string]string{"id": "m1"}); err == nil {
		t.Fatal("PutJSON accepted memory")
	}
	if err := s.Get("memory", "m1", &struct{}{}); err == nil {
		t.Fatal("Get accepted memory")
	}
	if err := s.GetIn("memory", "m1", "s1", &struct{}{}); err == nil {
		t.Fatal("GetIn accepted memory")
	}
	if _, err := s.List("memory", "workspace"); err == nil {
		t.Fatal("List accepted memory")
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM memory`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("memory rows=%d err=%v", n, err)
	}
}

// A catalog opened without memory.Open still carries the guard triggers, so an older writer is refused from the start.
func TestOpenInstallsMemoryGuard(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name IN ('memory_guard_insert','memory_guard_update')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("guards=%d err=%v", n, err)
	}
	_, err = s.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at) VALUES('m1', '/home/me@example.com/repo', '{"id":"m1","title":"legacy"}', 1)`)
	if err == nil {
		t.Fatal("legacy memory write accepted")
	}
}

// The guard accepts a v2 retired or stale row in an opaque partition, still refuses a legacy payload, and refuses a
// state it does not know, including on UPDATE of an existing row.
func TestMemoryGuardAllowsRetiredAndStale(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at, revision, state) VALUES('m2', 'ws_abc', '{"schemaVersion":2,"id":"m2"}', 1, 1, 'retired')`); err != nil {
		t.Fatalf("retired v2 write refused: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE memory SET state='stale' WHERE id='m2'`); err != nil {
		t.Fatalf("stale v2 write refused: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at, revision, state) VALUES('m4', 'ws_abc', '{"schemaVersion":2,"id":"m4"}', 1, 1, 'stale')`); err != nil {
		t.Fatalf("stale v2 insert refused: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE memory SET state='archived' WHERE id='m2'`); err == nil {
		t.Fatal("unknown state accepted")
	}
	if _, err := s.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at, revision, state) VALUES('m3', 'ws_abc', '{"id":"m3","title":"legacy"}', 1, 1, 'retired')`); err == nil {
		t.Fatal("legacy payload accepted for retired state")
	}
	if _, err := s.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at, revision, state) VALUES('m5', 'ws_abc', '{"id":"m5","title":"legacy"}', 1, 1, 'stale')`); err == nil {
		t.Fatal("legacy payload accepted for stale state")
	}
}

// Legacy rows are converted in the import transaction: unsafe rows are scrubbed, the rest become v2 candidates in an
// opaque partition and stay covered by the guard afterwards.
func TestLegacyCatalogImportRewritesMemoryRows(t *testing.T) {
	// This package does not link internal/memory (memory is built on it), so the test installs the intake memory
	// registers at init in a real binary.
	prev := EnsureLegacyMemoryIntake
	t.Cleanup(func() { EnsureLegacyMemoryIntake = prev })
	EnsureLegacyMemoryIntake = func(tx *sql.Tx, rows []LegacyMemoryRow) (int, int, error) {
		kept, scrubbed := 0, 0
		for _, r := range rows {
			var raw struct {
				Kind  string `json:"kind"`
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if err := json.Unmarshal([]byte(r.Payload), &raw); err != nil {
				return 0, 0, err
			}
			if strings.Contains(raw.Title, "key") || strings.Contains(raw.Body, "sk-live-") {
				scrubbed++
				continue
			}
			payload, _ := json.Marshal(map[string]any{"schemaVersion": 2, "id": r.ID, "revision": 1,
				"kind": "fact", "state": "candidate", "title": raw.Title, "body": raw.Body, "source": "legacy"})
			if _, err := tx.Exec(`INSERT INTO memory(id, workspace, payload, revision, state, updated_at) VALUES(?,?,?,1,'candidate',?)`,
				r.ID, "ws_test", payload, r.Updated); err != nil {
				return 0, 0, err
			}
			kept++
		}
		return kept, scrubbed, nil
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacy.SetMaxOpenConns(1)
	if _, err := legacy.Exec(testLegacySchema); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ id, workspace, payload string }{
		{"mem_ok", "/home/me@example.com/repo", `{"id":"mem_ok","kind":"fact","title":"Run core tests","body":"go test from core","files":["core/go.mod"],"verified":true,"source":"worker","score":0.9}`},
		{"mem_bad", "/home/me@example.com/repo", `{"id":"mem_bad","kind":"fact","title":"key","body":"sk-live-0123456789abcdef0123456789abcdef"}`},
		{"mem_kind", "/home/me@example.com/repo", `{"id":"mem_kind","kind":"note","title":"Note","body":"kept as fact"}`},
	}
	for _, r := range rows {
		if _, err := legacy.Exec(`INSERT INTO memory(id,workspace,payload,updated_at) VALUES(?,?,?,?)`, r.id, r.workspace, r.payload, 1767225600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM memory WHERE id='mem_bad'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unsafe row kept: %d %v", n, err)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM memory WHERE workspace LIKE '/home/%' OR json_extract(payload, ?) IS NOT 2 OR json_extract(payload, ?) IS NOT 'candidate'`, "$.schemaVersion", "$.state").Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw or non-candidate rows=%d err=%v", n, err)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM memory`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("migrated rows=%d err=%v", n, err)
	}
	if _, err := s.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at) VALUES('mem_new', '/home/me@example.com/repo', '{"id":"mem_new"}', 2)`); err == nil {
		t.Fatal("guard missing after legacy import")
	}
}
