package persistence

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

func (s *Store) initLegacy() error {
	path := filepath.Join(s.root, "relo.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		log.Printf("persistence: legacy import: %v", err)
		return nil
	}
	if err := importLegacyCatalog(path, s.db); err != nil {
		log.Printf("persistence: legacy catalog import: %v", err)
		if errors.Is(err, syscall.ENOSPC) {
			return err
		}
		return nil
	}
	s.legacyReady = true
	return nil
}

// Called with s.mu held before the handle becomes visible to callers.
func (s *Store) importSessionOnOpen(id string, db *sql.DB) error {
	if !s.legacyReady {
		return nil
	}
	var done string
	err := db.QueryRow(`SELECT v FROM meta WHERE k='legacy:complete'`).Scan(&done)
	if err == nil && done == "done" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	path := filepath.Join(s.root, "relo.db")
	legacy, err := openLegacy(path)
	if err == nil {
		var exists int
		err = legacy.QueryRow(`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)`, id).Scan(&exists)
		legacy.Close()
		if err == nil && exists == 0 {
			return nil
		}
	}
	if err == nil {
		err = importLegacySession(path, id, db)
	}
	if err == nil {
		err = importLegacyEvents(path, id, db)
	}
	if err == nil {
		_, err = db.Exec(`INSERT INTO meta(k,v) VALUES('legacy:complete','done') ON CONFLICT(k) DO UPDATE SET v=excluded.v`)
	}
	if err != nil {
		log.Printf("persistence: legacy session %q import: %v", id, err)
		return fmt.Errorf("legacy session %q import: %w", id, err)
	}
	return nil
}
