package persistence

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

type sessionHandle struct {
	db   *sql.DB
	used uint64
}

func openDB(path string) (*sql.DB, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// sessionDB requires s.mu; callers hold it until all queries and rows are closed.
func (s *Store) sessionDB(id string) (*sql.DB, error) {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	s.clock++
	if h := s.sessions[id]; h != nil {
		h.used = s.clock
		return h.db, nil
	}
	if len(s.sessions) >= 4 {
		var oldest string
		var used uint64 = ^uint64(0)
		for key, h := range s.sessions {
			if h.used < used {
				oldest, used = key, h.used
			}
		}
		if err := s.sessions[oldest].db.Close(); err != nil {
			return nil, err
		}
		delete(s.sessions, oldest)
	}
	dir := filepath.Join(s.root, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(dir, id+".db"))
	if err != nil {
		return nil, err
	}
	if err := (&Store{db: db}).migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.importSessionOnOpen(id, db); err != nil {
		db.Close()
		return nil, err
	}
	s.sessions[id] = &sessionHandle{db: db, used: s.clock}
	return db, nil
}

func (s *Store) CloseSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.sessions[id]; h != nil {
		if err := h.db.Close(); err != nil {
			return err
		}
		delete(s.sessions, id)
	}
	return nil
}
