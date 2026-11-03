package persistence

// This file holds the memory schema shared with internal/memory: the guard triggers and the v2 columns. The memory
// store imports this package, so the definitions live here and there is exactly one copy. The catalog installs the
// guard when it opens, before any legacy row can be written, and memory.Open reinstalls it after its migration.

import "database/sql"

// MemoryGuardSQL is the database guard: it refuses any write whose payload is not schema v2 or whose partition is
// not an opaque ID. Older binaries still write legacy payloads with raw workspace paths, so this makes their writes
// fail at the database instead of trusting a marker they ignore.
const MemoryGuardSQL = `
DROP TRIGGER IF EXISTS memory_guard_insert; DROP TRIGGER IF EXISTS memory_guard_update;
CREATE TRIGGER memory_guard_insert BEFORE INSERT ON memory
WHEN json_extract(NEW.payload, '$.schemaVersion') IS NOT 2 OR substr(NEW.workspace, 1, 3) <> 'ws_'
  OR NEW.state IS NULL OR NEW.state NOT IN ('candidate','verified','stale','retired','deleted')
BEGIN SELECT RAISE(ABORT, 'memory write refused: schema is not v2'); END;
CREATE TRIGGER memory_guard_update BEFORE UPDATE ON memory
WHEN json_extract(NEW.payload, '$.schemaVersion') IS NOT 2 OR substr(NEW.workspace, 1, 3) <> 'ws_'
  OR NEW.state IS NULL OR NEW.state NOT IN ('candidate','verified','stale','retired','deleted')
BEGIN SELECT RAISE(ABORT, 'memory write refused: schema is not v2'); END;`

// EnsureMemorySchema adds the revision and state columns and the workspace index the memory store needs, idempotently.
func EnsureMemorySchema(tx *sql.Tx) error {
	rows, err := tx.Query(`PRAGMA table_info(memory)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, col := range []struct{ name, ddl string }{
		{"revision", `ALTER TABLE memory ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`},
		{"state", `ALTER TABLE memory ADD COLUMN state TEXT NOT NULL DEFAULT 'candidate'`},
	} {
		if have[col.name] {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_workspace_state ON memory(workspace, state, updated_at)`)
	return err
}
