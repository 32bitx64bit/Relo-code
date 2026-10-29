package persistence

import (
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const legacyChunkSize = 256

// legacyPreflight runs before creating the catalog. The WAL is part of the
// database and must be counted when estimating the space needed for import.
func legacyPreflight(path string) error {
	var size uint64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(path + suffix)
		if os.IsNotExist(err) {
			if suffix == "" {
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		size += uint64(info.Size())
	}
	available, err := legacyAvailableSpace(filepath.Dir(path))
	if err != nil {
		return err
	}
	return checkLegacySpace(size, available)
}

func checkLegacySpace(size, available uint64) error {
	required := size + size/5
	if size%5 != 0 {
		required++
	}
	if required < size || available < required {
		return fmt.Errorf("legacy import needs at least 1.2 times %d bytes, only %d available: %w", size, available, syscall.ENOSPC)
	}
	return nil
}

func openLegacy(path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func legacyTableExists(db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count)
	return count != 0, err
}

// importLegacyCatalog imports the global rows and session/object index in one
// transaction. It never modifies or attaches the legacy database.
func importLegacyCatalog(path string, catalog *sql.DB) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	legacy, err := openLegacy(path)
	if err != nil {
		return fmt.Errorf("open legacy database: %w", err)
	}
	defer legacy.Close()
	var version int
	if err := legacy.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 2 {
		return fmt.Errorf("legacy database schema version %d is newer than supported version 2", version)
	}
	for _, name := range []string{"kv", "sessions", "agents", "tasks"} {
		ok, err := legacyTableExists(legacy, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("legacy database missing required table %s", name)
		}
	}
	tx, err := catalog.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS legacy_import (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		return err
	}
	var done string
	err = tx.QueryRow(`SELECT v FROM legacy_import WHERE k='catalog'`).Scan(&done)
	if err == nil && done == "done" {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO ids(kind,id,session_id) SELECT 'sessions',id,id FROM sessions`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	for _, spec := range []struct{ table, columns string }{
		{"kv", "k,v,updated_at"},
		{"memory", "id,workspace,payload,updated_at"},
		{"vworkspaces", "workspace,agent_id,session_id,base_commit,revision,tree_oid,state,payload,updated_at"},
		{"sessions", "id,workspace,updated_at"},
	} {
		ok, err := legacyTableExists(legacy, spec.table)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := copyRows(legacy, tx, spec.table, spec.columns, "", nil); err != nil {
			return fmt.Errorf("import catalog %s: %w", spec.table, err)
		}
	}
	for _, table := range []string{"agents", "tasks", "generations", "findings", "reports", "changes", "verification"} {
		ok, err := legacyTableExists(legacy, table)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		rows, err := legacy.Query(`SELECT id,session_id FROM ` + table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, session string
			if err := rows.Scan(&id, &session); err != nil {
				rows.Close()
				return err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO ids(kind,id,session_id) VALUES(?,?,?)`, table, id, session); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO ids(kind,id,session_id) SELECT 'sessions',id,id FROM sessions`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO legacy_import(k,v) VALUES('catalog','done')`); err != nil {
		return err
	}
	return tx.Commit()
}

// copyRows copies a fixed, caller-supplied column list; identifiers never come
// from the database or user input.
func copyRows(src *sql.DB, dst *sql.Tx, table, columns, where string, args []any) error {
	rows, err := src.Query(`SELECT `+columns+` FROM `+table+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols := strings.Split(columns, ",")
	placeholders := strings.TrimRight(strings.Repeat("?,", len(cols)), ",")
	stmt, err := dst.Prepare(`INSERT OR IGNORE INTO ` + table + `(` + columns + `) VALUES(` + placeholders + `)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if _, err := stmt.Exec(values...); err != nil {
			return err
		}
	}
	return rows.Err()
}

type legacySessionTable struct {
	name, columns, filter string
}

var legacySessionTables = []legacySessionTable{
	{"sessions", "id,workspace,payload,updated_at", "id=?"},
	{"agents", "id,session_id,payload,updated_at", "session_id=?"},
	{"tasks", "id,session_id,payload,updated_at", "session_id=?"},
	{"generations", "id,session_id,agent_id,number,payload,updated_at", "session_id=?"},
	{"findings", "id,session_id,payload,updated_at", "session_id=?"},
	{"reports", "id,session_id,agent_id,payload,updated_at", "session_id=?"},
	{"usage", "id,session_id,payload,created_at", "session_id=?"},
	{"changes", "id,session_id,payload,updated_at", "session_id=?"},
	{"verification", "id,session_id,payload,updated_at", "session_id=?"},
	{"event_latest", "session_id,name,payload,created_at", "session_id=?"},
}

func importLegacySession(path, sessionID string, session *sql.DB) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	legacy, err := openLegacy(path)
	if err != nil {
		return err
	}
	defer legacy.Close()
	if _, err := session.Exec(`CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY,v TEXT NOT NULL)`); err != nil {
		return err
	}
	for _, spec := range legacySessionTables {
		ok, err := legacyTableExists(legacy, spec.name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		// Newer schemas contain metrics; v1 defaults are supplied by the target.
		if spec.name == "agents" {
			var n int
			if err := legacy.QueryRow(`SELECT count(*) FROM pragma_table_info('agents') WHERE name IN ('steps','cache_read','cache_write')`).Scan(&n); err != nil {
				return err
			}
			if n == 3 {
				spec.columns += ",steps,cache_read,cache_write"
			}
		}
		if err := importLegacyChunks(legacy, session, sessionID, spec, false); err != nil {
			return fmt.Errorf("import session %s %s: %w", sessionID, spec.name, err)
		}
	}
	// Reserve the entire legacy ID range before new events or usage can be
	// inserted, including when events are copied asynchronously.
	for _, table := range []string{"events", "usage"} {
		ok, err := legacyTableExists(legacy, table)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var max sql.NullInt64
		if err := legacy.QueryRow(`SELECT max(id) FROM `+table+` WHERE session_id=?`, sessionID).Scan(&max); err != nil {
			return err
		}
		if max.Valid {
			if _, err := session.Exec(`INSERT INTO sqlite_sequence(name,seq) SELECT ?,? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name=?)`, table, max.Int64, table); err != nil {
				return err
			}
			if _, err := session.Exec(`UPDATE sqlite_sequence SET seq=max(seq,?) WHERE name=?`, max.Int64, table); err != nil {
				return err
			}
		}
	}
	return nil
}

func importLegacyEvents(path, sessionID string, session *sql.DB) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	legacy, err := openLegacy(path)
	if err != nil {
		return err
	}
	defer legacy.Close()
	ok, err := legacyTableExists(legacy, "events")
	if err != nil || !ok {
		return err
	}
	spec := legacySessionTable{"events", "id,session_id,name,payload,created_at", "session_id=? AND name NOT IN ('session.updated','changes.updated','cache.updated')"}
	return importLegacyChunks(legacy, session, sessionID, spec, true)
}

// Each batch and cursor commit together. Re-running after interruption is safe,
// including when a destination row was written before the import started.
func importLegacyChunks(legacy, target *sql.DB, id string, spec legacySessionTable, reverse bool) error {
	key := "legacy:" + spec.name
	initial := int64(math.MinInt64)
	op, order := ">", "ASC"
	if reverse {
		initial = math.MaxInt64
		op, order = "<", "DESC"
	}
	// A forward cursor above this session's legacy max rowid would skip every
	// row forever; reset it so INSERT OR IGNORE re-imports safely.
	var legacyMax int64
	if !reverse {
		if err := legacy.QueryRow(`SELECT COALESCE(max(rowid),0) FROM `+spec.name+` WHERE `+spec.filter, id).Scan(&legacyMax); err != nil {
			return err
		}
	}
	for {
		tx, err := target.Begin()
		if err != nil {
			return err
		}
		var cursor int64
		err = tx.QueryRow(`SELECT CAST(v AS INTEGER) FROM meta WHERE k=?`, key).Scan(&cursor)
		if err == sql.ErrNoRows {
			cursor = initial
		} else if err != nil {
			tx.Rollback()
			return err
		} else if !reverse && cursor > legacyMax {
			cursor = initial
		}
		query := `SELECT rowid,` + spec.columns + ` FROM ` + spec.name + ` WHERE ` + spec.filter + ` AND rowid ` + op + ` ? ORDER BY rowid ` + order + ` LIMIT ?`
		rows, err := legacy.Query(query, id, cursor, legacyChunkSize)
		if err != nil {
			tx.Rollback()
			return err
		}
		cols := strings.Split(spec.columns, ",")
		placeholders := strings.TrimRight(strings.Repeat("?,", len(cols)), ",")
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO ` + spec.name + `(` + spec.columns + `) VALUES(` + placeholders + `)`)
		if err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		count := 0
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols)+1)
			ptrs[0] = &cursor
			for i := range values {
				ptrs[i+1] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				break
			}
			if _, err = stmt.Exec(values...); err != nil {
				break
			}
			count++
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		stmt.Close()
		if err == nil && count > 0 {
			_, err = tx.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, cursor)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if count < legacyChunkSize {
			return nil
		}
	}
}
