package memory

// This file holds the parts of the v2 schema the memory store shares with the catalog: the legacy conversion, the
// intake the catalog import registers, and the page scrub that runs at open. The guard SQL and the column steps live
// in persistence, which this package imports, so there is one definition and no import cycle.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/relocode/relo/internal/persistence"
)

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,80}$`)

// LegacyCandidate converts one pre-v2 row into the candidate entry it becomes during migration (design §6): the ID
// and timestamps are kept when valid, unsupported fields are dropped, source becomes legacy, state candidate and
// revision 1. It reports false when the row fails intake and must be scrubbed instead of stored; a payload that is
// not JSON is an error, because it rolls the whole migration back rather than dropping one row.
func LegacyCandidate(id, workspace, payload string, updated int64) (Entry, bool, error) {
	var raw Entry
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return Entry{}, false, malformedLegacy()
	}
	if raw.SchemaVersion == SchemaVersion {
		// Already migrated. A tombstone decodes without content and fails intake, so it must never be scrubbed.
		return Entry{}, false, nil
	}
	at := time.Unix(updated, 0).UTC()
	e := Entry{
		ID: id, Revision: 1, Kind: NormalizeKind(raw.Kind), State: StateCandidate, Source: SourceLegacy,
		Title: raw.Title, Body: raw.Body, Files: raw.Files, Symbols: raw.Symbols,
		Created: raw.Created, Updated: raw.Updated,
	}
	if e.Created.IsZero() {
		e.Created = at
	}
	if e.Updated.IsZero() {
		e.Updated = at
	}
	if !safeIDPattern.MatchString(e.ID) || validate(e) != nil {
		return Entry{}, false, nil
	}
	return e, true, nil
}

// LegacyInsert stores one migrated legacy candidate on the given transaction, so a legacy import leaves only v2 rows
// behind. Workspace is the migrated opaque partition key.
func LegacyInsert(tx *sql.Tx, workspace string, e Entry) error {
	payload, err := encode(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO memory(id, workspace, payload, revision, state, updated_at) VALUES(?,?,?,?,?,?)`,
		e.ID, workspace, payload, e.Revision, e.State, e.Updated.Unix())
	return err
}

// init registers the legacy-row conversion before any catalog is opened: the catalog's legacy import runs this
// package's intake instead of copying pre-v2 rows raw. This package is built on the catalog, so registration has to
// happen here and cannot live in persistence.
func init() {
	persistence.EnsureLegacyMemoryIntake = importLegacyRows
}

func importLegacyRows(tx *sql.Tx, rows []persistence.LegacyMemoryRow) (int, int, error) {
	kept, scrubbed := 0, 0
	for _, r := range rows {
		e, ok, err := LegacyCandidate(r.ID, r.Workspace, r.Payload, r.Updated)
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			scrubbed++
			continue
		}
		if err := LegacyInsert(tx, WorkspaceID(r.Workspace), e); err != nil {
			return 0, 0, err
		}
		kept++
	}
	return kept, scrubbed, nil
}

// scrubFreePages applies the secure data-removal policy of design §6 to the catalog and its WAL, and is best effort:
// a concurrent process holding the catalog can block the truncating checkpoint, which is reported back for logging
// only, never to disable memory. secure_delete makes SQLite overwrite freed cells, and the WAL is checkpointed into
// the main file and truncated, so a payload removed from the table is not left in the WAL. The run happens at open,
// before this process has a writer, which is what lets the truncating checkpoint take the exclusive lock it needs.
// VACUUM is kept for the catalogs where it does run and is a no-op under modernc's WAL mode: pages that older,
// non-secure_delete writes left on the free list can therefore still be read from the file by whoever has it. The
// design allows this; the residual exposure is documented in docs/context-engine.md and is not a claim of erasure.
func scrubFreePages(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA secure_delete=ON`); err != nil {
		return dbErr(err)
	}
	// Read the mode before anything else writes: asking for journal_mode again would recreate the WAL file after
	// the truncating checkpoint below.
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return dbErr(err)
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		return dbErr(err)
	}
	if strings.EqualFold(mode, "wal") {
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return dbErr(err)
		}
	}
	return nil
}

// malformedLegacy is the error a payload that is not JSON produces. It carries no payload text.
func malformedLegacy() error {
	return fmt.Errorf("%w: malformed legacy memory row", ErrUnavailable)
}
