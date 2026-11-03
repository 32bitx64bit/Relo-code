package persistence

import (
	"encoding/json"
	"fmt"
	"log"
)

func (s *Store) migrateCatalog() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, updated_at INTEGER NOT NULL);
-- memory rows are written and read only through memory.Open, which owns the v2 payload schema, the guard triggers
-- installed by MemoryGuardSQL, and the migration that rewrites legacy rows. Generic store access is refused. The
-- revision and state columns are declared here, not added by ALTER TABLE, so a rebuilt catalog keeps SQLite's stable
-- column order and older rows and INSERT lists that name only the original four columns still work.
CREATE TABLE IF NOT EXISTS memory (id TEXT PRIMARY KEY, workspace TEXT NOT NULL, payload TEXT NOT NULL,
  updated_at INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 1, state TEXT NOT NULL DEFAULT 'candidate');
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, workspace TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_sessions_workspace ON sessions(workspace, updated_at);
CREATE TABLE IF NOT EXISTS ids (kind TEXT NOT NULL, id TEXT NOT NULL, session_id TEXT NOT NULL, PRIMARY KEY(kind,id));
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS vworkspaces (
 workspace TEXT NOT NULL, agent_id TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '', base_commit TEXT NOT NULL,
 revision INTEGER NOT NULL, tree_oid TEXT NOT NULL, state TEXT NOT NULL, payload TEXT NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(workspace, agent_id));`)
	return err
}

func (s *Store) indexID(kind, id, session string) error {
	_, err := s.db.Exec(`INSERT INTO ids(kind,id,session_id) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET session_id=excluded.session_id`, kind, id, session)
	return err
}

func (s *Store) resolveID(kind, id string) (string, error) {
	var session string
	err := s.db.QueryRow(`SELECT session_id FROM ids WHERE kind=? AND id=?`, kind, id).Scan(&session)
	return session, err
}

func (s *Store) PutSession(id, workspace string, v any) error {
	return s.PutJSON("sessions", id, "", workspace, v)
}

func (s *Store) GetIn(table, id, session string, dest any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validTable(table) {
		return fmt.Errorf("unknown table %s", table)
	}
	db, err := s.sessionDB(session)
	if err != nil {
		return err
	}
	var payload string
	if err := db.QueryRow(`SELECT payload FROM `+table+` WHERE id=?`, id).Scan(&payload); err != nil {
		return err
	}
	return json.Unmarshal([]byte(payload), dest)
}

func (s *Store) ListSessions(workspace string) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE workspace=? ORDER BY updated_at DESC`, workspace)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, id := range ids {
		db, err := s.sessionDB(id)
		if err != nil {
			log.Printf("persistence: skip session %q in list: %v", id, err)
			continue
		}
		var payload string
		if err := db.QueryRow(`SELECT payload FROM sessions WHERE id=?`, id).Scan(&payload); err != nil {
			log.Printf("persistence: skip session %q in list: %v", id, err)
			continue
		}
		out = append(out, json.RawMessage(payload))
	}
	return out, nil
}
