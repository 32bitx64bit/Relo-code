package persistence

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" && path != "file::memory:?cache=shared" {
		dir := filepath.Dir(path)
		_, statErr := os.Stat(dir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if os.IsNotExist(statErr) {
			if err := os.Chmod(dir, 0o700); err != nil {
				return nil, err
			}
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	} else {
		dsn = "file:relo?mode=memory&cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" && path != "file::memory:?cache=shared" {
		if err := os.Chmod(path, 0o600); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 2 {
		return fmt.Errorf("database schema version %d is newer than supported version 2", version)
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS kv (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  workspace TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS generations (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  number INTEGER NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS findings (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS memory (
  id TEXT PRIMARY KEY,
  workspace TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS reports (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS changes (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS verification (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT,
  name TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
-- event_latest holds only the newest snapshot of each full-state event kind (session.updated, changes.updated,
-- cache.updated) per session, so the log stops growing one row per emit. It is additive like vworkspaces: no
-- schema-version bump, older builds ignore it, and existing events rows are never rewritten or deleted.
CREATE TABLE IF NOT EXISTS event_latest (
  session_id TEXT NOT NULL,
  name TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (session_id, name)
);
-- vworkspaces mirrors the virtual workspaces Git holds (refs/relo/vws/*), for the UI, diagnostics and for noticing
-- when Git and Relo disagree after a restart. Git is the source of truth: a row is rebuilt from refs, never trusted
-- over them. The table is additive, so it needs no schema version and older builds ignore it.
CREATE TABLE IF NOT EXISTS vworkspaces (
  workspace TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  session_id TEXT NOT NULL DEFAULT '',
  base_commit TEXT NOT NULL,
  revision INTEGER NOT NULL,
  tree_oid TEXT NOT NULL,
  state TEXT NOT NULL,
  payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (workspace, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_agents_session ON agents(session_id);
CREATE INDEX IF NOT EXISTS idx_tasks_session ON tasks(session_id);
CREATE INDEX IF NOT EXISTS idx_gen_agent ON generations(agent_id);
`)
	if err != nil {
		return err
	}
	if version < 2 {
		return s.migrateV2()
	}
	return nil
}

// migrateV2 adds the per-agent metric columns. Existing rows keep their payload and read 0 for each.
func (s *Store) migrateV2() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, col := range []string{"steps", "cache_read", "cache_write"} {
		if _, err := tx.Exec(`ALTER TABLE agents ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version=2`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutJSON(table, id, session string, extra string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	switch table {
	case "sessions":
		_, err = s.db.Exec(`INSERT INTO sessions(id, workspace, payload, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET workspace=excluded.workspace, payload=excluded.payload, updated_at=excluded.updated_at`, id, extra, string(b), now)
	case "agents":
		var m struct {
			Steps      int `json:"steps"`
			CacheRead  int `json:"cacheRead"`
			CacheWrite int `json:"cacheWrite"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		_, err = s.db.Exec(`INSERT INTO agents(id, session_id, payload, updated_at, steps, cache_read, cache_write) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET session_id=excluded.session_id, payload=excluded.payload, updated_at=excluded.updated_at,
			steps=excluded.steps, cache_read=excluded.cache_read, cache_write=excluded.cache_write`, id, session, string(b), now, m.Steps, m.CacheRead, m.CacheWrite)
	case "tasks":
		_, err = s.db.Exec(`INSERT INTO tasks(id, session_id, payload, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET session_id=excluded.session_id, payload=excluded.payload, updated_at=excluded.updated_at`, id, session, string(b), now)
	case "findings":
		_, err = s.db.Exec(`INSERT INTO findings(id, session_id, payload, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`, id, session, string(b), now)
	case "memory":
		_, err = s.db.Exec(`INSERT INTO memory(id, workspace, payload, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`, id, extra, string(b), now)
	case "changes":
		_, err = s.db.Exec(`INSERT INTO changes(id, session_id, payload, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`, id, session, string(b), now)
	case "verification":
		_, err = s.db.Exec(`INSERT INTO verification(id, session_id, payload, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`, id, session, string(b), now)
	case "reports":
		_, err = s.db.Exec(`INSERT INTO reports(id, session_id, agent_id, payload, updated_at) VALUES(?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`, id, session, extra, string(b), now)
	default:
		return fmt.Errorf("unknown table %s", table)
	}
	return err
}

func (s *Store) PutGeneration(session, agent, id string, number int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO generations(id, session_id, agent_id, number, payload, updated_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`, id, session, agent, number, string(b), time.Now().Unix())
	return err
}

func (s *Store) PutUsage(session string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO usage(session_id, payload, created_at) VALUES(?,?,?)`, session, string(b), time.Now().Unix())
	return err
}

// snapshotEventKinds are the full-state payloads whose current state is also persisted in the dedicated tables
// (sessions, changes, usage/agents). They are high-frequency, so the event log keeps only the newest snapshot
// per session in event_latest instead of appending a row for every emit. Every other event still appends to the
// events table exactly as before. Live IPC delivery is decided by the caller and is unaffected by where a row
// is stored; the readers below merge event_latest back in so the newest snapshot is still visible.
var snapshotEventKinds = map[string]bool{
	"session.updated": true,
	"changes.updated": true,
	"cache.updated":   true,
}

func (s *Store) LogEvent(session, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if snapshotEventKinds[name] {
		_, err = s.db.Exec(`INSERT INTO event_latest(session_id, name, payload, created_at) VALUES(?,?,?,?)
			ON CONFLICT(session_id, name) DO UPDATE SET payload=excluded.payload, created_at=excluded.created_at`, session, name, string(b), now)
		return err
	}
	_, err = s.db.Exec(`INSERT INTO events(session_id, name, payload, created_at) VALUES(?,?,?,?)`, session, name, string(b), now)
	return err
}

func (s *Store) Get(table, id string, dest any) error {
	if !validTable(table) {
		return fmt.Errorf("unknown table %s", table)
	}
	q := "SELECT payload FROM " + table + " WHERE id=?"
	var payload string
	if err := s.db.QueryRow(q, id).Scan(&payload); err != nil {
		return err
	}
	return json.Unmarshal([]byte(payload), dest)
}

func (s *Store) List(table, session string) ([]json.RawMessage, error) {
	if !validTable(table) {
		return nil, fmt.Errorf("unknown table %s", table)
	}
	var rows *sql.Rows
	var err error
	switch table {
	case "sessions":
		rows, err = s.db.Query(`SELECT payload FROM sessions WHERE workspace=? ORDER BY updated_at DESC`, session)
	case "memory":
		rows, err = s.db.Query(`SELECT payload FROM memory WHERE workspace=? ORDER BY updated_at DESC`, session)
	case "generations":
		rows, err = s.db.Query(`SELECT payload FROM generations WHERE session_id=? ORDER BY agent_id, number`, session)
	case "usage":
		rows, err = s.db.Query(`SELECT payload FROM usage WHERE session_id=? ORDER BY id`, session)
	case "events":
		rows, err = s.db.Query(`SELECT payload FROM events WHERE session_id=? ORDER BY id`, session)
	default:
		rows, err = s.db.Query(`SELECT payload FROM `+table+` WHERE session_id=? ORDER BY updated_at DESC`, session)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(p))
	}
	return out, rows.Err()
}

// EventRow is one stored event: the name of the event and the payload that was written with it.
type EventRow struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

// EventRows returns the stored events of a session, newest first, with the name of each event.
func (s *Store) EventRows(session string, limit int) ([]EventRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`
		SELECT name, payload FROM (
			SELECT name, payload, created_at FROM event_latest WHERE session_id=?
			UNION ALL
			SELECT name, payload, created_at FROM events WHERE session_id=? AND name NOT IN ('session.updated','changes.updated','cache.updated')
		) ORDER BY created_at DESC LIMIT ?`, session, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var row EventRow
		var payload string
		if err := rows.Scan(&row.Name, &payload); err != nil {
			return nil, err
		}
		row.Payload = json.RawMessage(payload)
		out = append(out, row)
	}
	return out, rows.Err()
}

// DeleteSession removes a session and everything stored for it.
func (s *Store) DeleteSession(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"agents", "tasks", "generations", "findings", "reports", "usage", "changes", "verification", "events", "event_latest"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session_id=?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func validTable(table string) bool {
	switch table {
	case "sessions", "agents", "tasks", "generations", "findings", "memory", "reports", "usage", "changes", "verification", "events":
		return true
	default:
		return false
	}
}

func (s *Store) LatestSession(workspace string) (json.RawMessage, error) {
	var p string
	err := s.db.QueryRow(`SELECT payload FROM sessions WHERE workspace=? ORDER BY updated_at DESC LIMIT 1`, workspace).Scan(&p)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(p), nil
}

func (s *Store) SetKV(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO kv(k,v,updated_at) VALUES(?,?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v, updated_at=excluded.updated_at`, k, v, time.Now().Unix())
	return err
}

func (s *Store) GetKV(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	return v, err
}

func (s *Store) Events(session string, limit int) ([]json.RawMessage, error) {
	if limit <= 0 {
		limit = 200
	}
	// Merge the newest snapshots (event_latest) with the appended events, newest first, so the operator log
	// still shows the latest snapshot of each kind. The three snapshot kinds are excluded from events to avoid
	// showing both a stored snapshot and any historical append-only rows left by an older build.
	rows, err := s.db.Query(`
		SELECT payload FROM (
			SELECT payload, created_at FROM event_latest WHERE session_id=?
			UNION ALL
			SELECT payload, created_at FROM events WHERE session_id=? AND name NOT IN ('session.updated','changes.updated','cache.updated')
		) ORDER BY created_at DESC LIMIT ?`, session, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(p))
	}
	return out, rows.Err()
}

// VWorkspace is the stored mirror of one virtual workspace: enough to list it and to compare it with what Git holds.
// Payload is the workspace record as the engine published it.
type VWorkspace struct {
	Workspace  string          `json:"workspace"`
	AgentID    string          `json:"agentId"`
	SessionID  string          `json:"sessionId,omitempty"`
	BaseCommit string          `json:"baseCommit"`
	Revision   uint64          `json:"revision"`
	Tree       string          `json:"tree"`
	State      string          `json:"state"`
	Payload    json.RawMessage `json:"payload"`
	Updated    time.Time       `json:"updated"`
}

// PutVWorkspace inserts or replaces the mirror of one workspace. A write carrying an older revision than the stored
// one is ignored, so mirrors written out of order by concurrent callers cannot move a workspace backwards.
func (s *Store) PutVWorkspace(v VWorkspace) error {
	payload := string(v.Payload)
	if payload == "" {
		payload = "{}"
	}
	_, err := s.db.Exec(`INSERT INTO vworkspaces(workspace, agent_id, session_id, base_commit, revision, tree_oid, state, payload, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(workspace, agent_id) DO UPDATE SET session_id=excluded.session_id, base_commit=excluded.base_commit,
		revision=excluded.revision, tree_oid=excluded.tree_oid, state=excluded.state, payload=excluded.payload, updated_at=excluded.updated_at
		WHERE excluded.base_commit<>vworkspaces.base_commit OR excluded.revision>=vworkspaces.revision`,
		v.Workspace, v.AgentID, v.SessionID, v.BaseCommit, int64(v.Revision), v.Tree, v.State, payload, time.Now().Unix())
	return err
}

// ListVWorkspaces returns the stored workspaces of one repository, oldest first.
func (s *Store) ListVWorkspaces(workspace string) ([]VWorkspace, error) {
	rows, err := s.db.Query(`SELECT agent_id, session_id, base_commit, revision, tree_oid, state, payload, updated_at
		FROM vworkspaces WHERE workspace=? ORDER BY updated_at, agent_id`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VWorkspace
	for rows.Next() {
		v := VWorkspace{Workspace: workspace}
		var rev, at int64
		var payload string
		if err := rows.Scan(&v.AgentID, &v.SessionID, &v.BaseCommit, &rev, &v.Tree, &v.State, &payload, &at); err != nil {
			return nil, err
		}
		v.Revision, v.Payload, v.Updated = uint64(rev), json.RawMessage(payload), time.Unix(at, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteVWorkspace forgets the mirror of a workspace that was released. Its history stays in the event log.
func (s *Store) DeleteVWorkspace(workspace, agentID string) error {
	_, err := s.db.Exec(`DELETE FROM vworkspaces WHERE workspace=? AND agent_id=?`, workspace, agentID)
	return err
}
