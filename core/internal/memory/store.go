package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/relocode/relo/internal/id"
	"github.com/relocode/relo/internal/persistence"
)

// SchemaVersion is the payload version of every memory row written by this package. Rows without it are legacy.
const SchemaVersion = 2

const (
	StateCandidate = "candidate"
	StateVerified  = "verified"
	StateStale     = "stale"   // permanent historical record contradicted or expired; excluded from recall
	StateRetired   = "retired" // permanent historical record withdrawn by head or user; excluded from recall
	StateDeleted   = "deleted" // content-free tombstone; never listed, read or recalled

	SourceUser   = "user"
	SourceAgent  = "agent"  // agent-originated candidate, never permanent by itself
	SourceHead   = "head"   // agent-originated, made permanent by a head promotion with evidence
	SourceLegacy = "legacy" // migrated pre-v2 row
)

// Intake limits (design §7). Values over a limit are rejected, never truncated.
const (
	MaxTitle      = 120
	MaxBody       = 600
	MaxPaths      = 8
	MaxSymbols    = 8
	MaxSymbolLen  = 120
	MaxPathLen    = 260
	MaxEntryBytes = 4 << 10
)

// Retention. Candidates are evicted oldest first and expire; verified entries are never evicted, and a write that
// would exceed MaxVerified is refused.
const (
	MaxCandidates = 200
	MaxVerified   = 500
	CandidateTTL  = 30 * 24 * time.Hour
)

// RecallLimit is the most entries one prompt may carry once recall is re-enabled (Phase 3).
const RecallLimit = 4

var kinds = map[string]bool{"fact": true, "pitfall": true, "command": true, "decision": true}

var (
	ErrNotFound    = errors.New("not_found")
	ErrInvalid     = errors.New("invalid_params")
	ErrUnsafe      = errors.New("unsafe_content")
	ErrUnavailable = errors.New("storage_unavailable")
	ErrQuota       = errors.New("storage_unavailable: verified entry quota reached")
	// ErrUserReview refuses an agent write that would act on a user entry. The candidate, if any, waits for the user.
	ErrUserReview = errors.New("user_review_required")
)

// ConflictError reports a stale revision. Current is the sanitized entry the caller should reload.
type ConflictError struct{ Current Entry }

func (e *ConflictError) Error() string { return "conflict" }

// Anchor is the view an entry was captured against: the git commit and the SHA-256 of each workspace-relative file
// the agent inspected, or AbsentMarker when a path was verified absent. Core checks the shape and bounds only; the
// freshness verdict is computed per recipient view at recall time.
type Anchor struct {
	Commit string            `json:"commit,omitempty"`
	Files  map[string]string `json:"files,omitempty"`
}

// AbsentMarker is the Anchor.Files value that records a verified absence of a path.
const AbsentMarker = "absent"

// Entry is one project memory. Persistence fills every server-controlled field (ID, Revision, State, Source,
// timestamps); caller-supplied values for them are ignored. Verified and Score are read-only compatibility fields:
// Verified mirrors State, Score is never stored.
//
// Phase 2 fields are additive JSON with omitempty, so schemaVersion 2 rows written before them load unchanged.
// Files and Symbols carry the design's scope paths and symbols. Scope carries only the tags.
type Entry struct {
	SchemaVersion int        `json:"schemaVersion"`
	ID            string     `json:"id"`
	Revision      int64      `json:"revision"`
	Kind          string     `json:"kind"`
	State         string     `json:"state"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	Files         []string   `json:"files,omitempty"`
	Symbols       []string   `json:"symbols,omitempty"`
	Scope         *Scope     `json:"scope,omitempty"`
	Command       *Command   `json:"command,omitempty"`
	Evidence      []Evidence `json:"evidence,omitempty"`
	Promotion     *Promotion `json:"promotion,omitempty"`
	Anchor        *Anchor    `json:"anchor,omitempty"`
	// SupersedesID links a replacement candidate to the entry it would replace. Core sets Flagged when that entry is
	// user-provenance: the candidate is then a contradiction awaiting user review.
	SupersedesID string `json:"supersedesId,omitempty"`
	// SupersedesRevision is the target's revision when the candidate was proposed. A head decision is refused if the
	// target has changed since, so a promotion retires exactly the entry the head was shown. Core fills it.
	SupersedesRevision int64  `json:"supersedesRevision,omitempty"`
	Flagged            bool   `json:"flagged,omitempty"`
	ReviewRequested    bool   `json:"reviewRequested,omitempty"`
	Source             string `json:"source"`
	// RetiredBy names the actor that retired the entry: user for a direct user retire, empty otherwise.
	RetiredBy string `json:"retiredBy,omitempty"`
	// StaleReason is the bounded reason a verified entry was marked stale. Stale entries stay reviewable but are never
	// recalled as current project memory.
	StaleReason string    `json:"staleReason,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`

	Verified bool    `json:"verified"`
	Score    float64 `json:"-"`
}

// Patch is a user edit. A nil field keeps the stored value.
type Patch struct {
	Kind, Title, Body *string
	Files, Symbols    *[]string
}

// Store is the workspace partition of the catalog memory table. It keeps no process-local copy: every call reads or
// writes the catalog, so two processes sharing a catalog see each other's commits.
type Store struct {
	db          *sql.DB
	ws          string
	now         func() time.Time
	maxVerified int
	err         error // non-nil when the catalog is not attached or migration failed; every write fails with it
}

// New returns a store with no catalog attached. Reads are empty and writes fail with ErrUnavailable.
func New() *Store {
	return &Store{now: nowUTC, maxVerified: MaxVerified, err: fmt.Errorf("%w: memory catalog not attached", ErrUnavailable)}
}

// MigrationReport counts what migrate did. Counts only; scrubbed payloads are never reported or logged.
type MigrationReport struct {
	Migrated int
	Scrubbed int
}

// Open attaches the store to one workspace partition of the catalog and migrates legacy rows first. On migration
// failure it still returns a usable Store whose every read is empty and every write fails, so no legacy writer or
// recall runs; the error is returned for the caller to show.
func Open(db *sql.DB, workspace string) (*Store, MigrationReport, error) {
	s := &Store{db: db, ws: WorkspaceID(workspace), now: nowUTC, maxVerified: MaxVerified}
	rep, err := migrate(db)
	if err != nil {
		s.err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return s, rep, s.err
	}
	// The page policy runs here, before this process has a writer, so the truncating checkpoint is not blocked by
	// it. Best effort: the error is only logged by the caller, never used to disable memory.
	_ = scrubFreePages(db)
	return s, rep, nil
}

// Scrub applies the page policy of design §6 on demand: secure_delete for later writes, and the WAL checkpointed and
// truncated. Callers report the outcome; failure is not fatal.
func (s *Store) Scrub() error {
	if s.err != nil {
		return s.err
	}
	if _, err := s.db.Exec(`PRAGMA secure_delete=ON`); err != nil {
		return dbErr(err)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return dbErr(err)
	}
	if strings.EqualFold(mode, "wal") {
		if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return dbErr(err)
		}
	}
	return nil
}

// WorkspaceID is the opaque partition key stored in place of a workspace path. Agent worktrees under
// <workspace>/.relo/worktrees/ map to their parent workspace.
func WorkspaceID(path string) string {
	p := path
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	p = filepath.ToSlash(p)
	if i := strings.Index(p, "/.relo/worktrees/"); i >= 0 {
		p = p[:i]
	}
	sum := sha256.Sum256([]byte(p))
	return "ws_" + hex.EncodeToString(sum[:16])
}

// NormalizeKind maps a kind outside the allowlist to "fact" so it can be kept as a quarantined candidate.
func NormalizeKind(k string) string {
	if kinds[k] {
		return k
	}
	return "fact"
}

// Upsert is the agent-side capture entry point still called by orchestrator code. Capture is disabled until Phase 2
// (head promotion): nothing is persisted and the entry is returned without Verified.
func (s *Store) Upsert(e Entry) Entry {
	e.Verified = false
	return e
}

// Recall returns the verified entries that are relevant to text, ranked and capped at RecallLimit. Candidates,
// retired, stale and deleted entries never reach a prompt.
func (s *Store) Recall(text string) []Entry {
	return s.RecallScoped(RecallQuery{Text: text})
}

// Get returns a live entry of this workspace.
func (s *Store) Get(id string) (Entry, bool) {
	if s.err != nil {
		return Entry{}, false
	}
	e, err := s.load(s.db, id)
	return e, err == nil
}

// Load returns every live entry of this workspace in ID order.
func (s *Store) Load() ([]Entry, error) {
	if s.err != nil {
		return nil, s.err
	}
	rows, err := s.db.Query(`SELECT payload, revision, state FROM memory WHERE workspace=? AND state<>? ORDER BY id`, s.ws, StateDeleted)
	if err != nil {
		return nil, dbErr(err)
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var payload, state string
		var rev int64
		if err := rows.Scan(&payload, &rev, &state); err != nil {
			return nil, dbErr(err)
		}
		e, err := decode(payload, rev, state)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, dbErr(rows.Err())
}

// List returns the live entries, or none when the catalog is unavailable.
func (s *Store) List() []Entry {
	out, _ := s.Load()
	return out
}

// ProposeCandidate stores an agent-originated candidate under a fresh ID. Candidates are never recalled. Core fills
// ID, revision, state, source and timestamps; caller values for them, and for Promotion and the review flags, are
// ignored. When SupersedesID names a user entry, the candidate is stored Flagged and the user entry is not touched.
// When the workspace holds MaxCandidates, the oldest candidates are evicted; candidates past CandidateTTL go first.
func (s *Store) ProposeCandidate(e Entry) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	return retry(func() (Entry, error) {
		now := s.now()
		c := Entry{SchemaVersion: SchemaVersion, ID: id.New("mem"), Revision: 1, Kind: e.Kind, State: StateCandidate,
			Title: e.Title, Body: e.Body, Files: e.Files, Symbols: e.Symbols, Scope: e.Scope, Command: e.Command,
			Evidence: e.Evidence, Anchor: e.Anchor, SupersedesID: e.SupersedesID, Source: SourceAgent, Created: now, Updated: now}
		if err := validate(c); err != nil {
			return Entry{}, err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		if c.SupersedesID != "" {
			target, err := s.load(tx, c.SupersedesID)
			if err != nil {
				return Entry{}, err
			}
			c.Flagged = target.Source == SourceUser
			c.SupersedesRevision = target.Revision
		}
		if _, err := tx.Exec(`DELETE FROM memory WHERE workspace=? AND state=? AND updated_at<?`, s.ws, StateCandidate, now.Add(-CandidateTTL).Unix()); err != nil {
			return Entry{}, dbErr(err)
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM memory WHERE workspace=? AND state=?`, s.ws, StateCandidate).Scan(&n); err != nil {
			return Entry{}, dbErr(err)
		}
		for ; n >= MaxCandidates; n-- {
			if _, err := tx.Exec(`DELETE FROM memory WHERE id=(SELECT id FROM memory WHERE workspace=? AND state=? ORDER BY updated_at, id LIMIT 1)`, s.ws, StateCandidate); err != nil {
				return Entry{}, dbErr(err)
			}
		}
		if err := s.insert(tx, c); err != nil {
			return Entry{}, err
		}
		return c, dbErr(tx.Commit())
	})
}

// UserPromote stores a direct user write as a verified entry with user provenance. It never creates a candidate and
// needs no session or head turn. The caller's ID is ignored.
func (s *Store) UserPromote(e Entry) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	return retry(func() (Entry, error) {
		now := s.now()
		v := Entry{SchemaVersion: SchemaVersion, ID: id.New("mem"), Revision: 1, Kind: e.Kind, State: StateVerified,
			Title: e.Title, Body: e.Body, Files: e.Files, Symbols: e.Symbols, Scope: e.Scope, Command: e.Command,
			Evidence: e.Evidence, Anchor: e.Anchor, Source: SourceUser, Created: now, Updated: now, Verified: true}
		if err := validate(v); err != nil {
			return Entry{}, err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		if err := s.checkQuota(tx); err != nil {
			return Entry{}, err
		}
		if err := s.insert(tx, v); err != nil {
			return Entry{}, err
		}
		return v, dbErr(tx.Commit())
	})
}

// UserUpdate applies a user edit in place under a revision check. It commits as verified with user provenance and
// the revision bumped by one. A stale revision returns *ConflictError; a deleted or unknown ID returns ErrNotFound.
func (s *Store) UserUpdate(id string, rev int64, p Patch) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	return retry(func() (Entry, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, id)
		if err != nil {
			return Entry{}, err
		}
		if cur.Revision != rev {
			return Entry{}, &ConflictError{Current: cur}
		}
		// Reinstating a retired agent-originated entry needs a new candidate, not a user write (design §3). A stale
		// agent-originated entry needs a head promotion after reverification, so a user edit cannot revive it silently.
		if (cur.State == StateRetired || cur.State == StateStale) && cur.Source != SourceUser {
			return Entry{}, ErrInvalid
		}
		next := cur
		if p.Kind != nil {
			next.Kind = *p.Kind
		}
		if p.Title != nil {
			next.Title = *p.Title
		}
		if p.Body != nil {
			next.Body = *p.Body
		}
		if p.Files != nil {
			next.Files = *p.Files
		}
		if p.Symbols != nil {
			next.Symbols = *p.Symbols
		}
		next.Source = SourceUser
		next.State = StateVerified
		next.Verified = true
		// The user has decided, so any contradiction flag or review request on this row is resolved.
		next.Flagged = false
		next.ReviewRequested = false
		next.Revision = rev + 1
		next.Updated = s.now()
		if err := validate(next); err != nil {
			return Entry{}, err
		}
		if cur.State != StateVerified {
			// Promoting a candidate, stale or retired entry adds a permanent entry, so the verified quota applies.
			if err := s.checkQuota(tx); err != nil {
				return Entry{}, err
			}
		}
		if err := s.write(tx, next, rev); err != nil {
			return Entry{}, err
		}
		return next, dbErr(tx.Commit())
	})
}

// UserDelete replaces the entry with a content-free tombstone under a revision check. The tombstone keeps only the
// opaque ID, the partition, the new revision and the deleted state; it cannot be read, listed or written back.
func (s *Store) UserDelete(id string, rev int64) error {
	_, err := s.UserDeleteRevision(id, rev)
	return err
}

// UserDeleteRevision is UserDelete that also returns the revision the tombstone was committed at, so callers report
// the stored revision rather than assuming it.
func (s *Store) UserDeleteRevision(id string, rev int64) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return retry(func() (int64, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return 0, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, id)
		if err != nil {
			return 0, err
		}
		if cur.Revision != rev {
			return 0, &ConflictError{Current: cur}
		}
		next := rev + 1
		payload, err := json.Marshal(tombstone{SchemaVersion: SchemaVersion, ID: id, Revision: next, State: StateDeleted})
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(`UPDATE memory SET payload=?, revision=?, state=?, updated_at=? WHERE workspace=? AND id=? AND revision=? AND state<>?`,
			string(payload), next, StateDeleted, s.now().Unix(), s.ws, id, rev, StateDeleted)
		if err != nil {
			return 0, dbErr(err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return 0, s.missing(tx, id, rev)
		}
		return next, dbErr(tx.Commit())
	})
}

// UserRetire withdraws a verified entry as a direct user write, whatever its provenance. It needs no head turn. It
// records user as the retiring actor and keeps the entry's source. A stale revision
// returns *ConflictError; a deleted or unknown ID returns ErrNotFound; an entry that is not verified returns ErrInvalid.
func (s *Store) UserRetire(id string, rev int64) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	return retry(func() (Entry, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, id)
		if err != nil {
			return Entry{}, err
		}
		if cur.Revision != rev {
			return Entry{}, &ConflictError{Current: cur}
		}
		// The retiring actor is recorded as user. Source is kept, so an agent-originated entry still needs a new
		// candidate to come back (design §3).
		cur.RetiredBy = SourceUser
		next, err := s.retire(tx, cur, rev, s.now())
		if err != nil {
			return Entry{}, err
		}
		return next, dbErr(tx.Commit())
	})
}

// MarkStale moves a verified entry to stale under a revision check. Stale entries stay reviewable but are excluded
// from recall until reverified by a new promotion, so a stale revision or an entry that is not verified is refused.
// The reason is bounded and scanned like other stored text; it carries no payload.
func (s *Store) MarkStale(id string, rev int64, reason string) (Entry, error) {
	if s.err != nil {
		return Entry{}, s.err
	}
	return retry(func() (Entry, error) {
		if err := validateStaleReason(reason); err != nil {
			return Entry{}, err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return Entry{}, dbErr(err)
		}
		defer tx.Rollback()
		cur, err := s.load(tx, id)
		if err != nil {
			return Entry{}, err
		}
		if cur.Revision != rev {
			return Entry{}, &ConflictError{Current: cur}
		}
		if cur.State != StateVerified {
			return Entry{}, ErrInvalid
		}
		if cur.Source == SourceUser {
			return Entry{}, ErrUserReview
		}
		next := cur
		next.State = StateStale
		next.Verified = false
		next.StaleReason = reason
		next.ReviewRequested = false
		next.Revision = rev + 1
		next.Updated = s.now()
		if err := validate(next); err != nil {
			return Entry{}, err
		}
		if err := s.write(tx, next, rev); err != nil {
			return Entry{}, err
		}
		return next, dbErr(tx.Commit())
	})
}

// validateStaleReason bounds and scans the reason recorded on a stale entry.
func validateStaleReason(reason string) error {
	if n := utf8.RuneCountInString(reason); n == 0 || n > MaxBody {
		return ErrInvalid
	}
	if unsafe(reason) {
		return ErrUnsafe
	}
	return nil
}

// retire commits cur as retired over the row whose revision is rev. Only verified entries retire. The retired entry
// is validated again, so the secret scan runs on every path that changes state. It must run inside tx.
func (s *Store) retire(tx *sql.Tx, cur Entry, rev int64, now time.Time) (Entry, error) {
	if cur.State != StateVerified {
		return Entry{}, ErrInvalid
	}
	next := cur
	next.State = StateRetired
	next.Verified = false
	next.ReviewRequested = false
	next.Revision = rev + 1
	next.Updated = now
	if err := validate(next); err != nil {
		return Entry{}, err
	}
	if err := s.write(tx, next, rev); err != nil {
		return Entry{}, err
	}
	return next, nil
}

type tombstone struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	Revision      int64  `json:"revision"`
	State         string `json:"state"`
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Store) load(q querier, id string) (Entry, error) {
	var payload, state string
	var rev int64
	err := q.QueryRow(`SELECT payload, revision, state FROM memory WHERE workspace=? AND id=? AND state<>?`, s.ws, id, StateDeleted).Scan(&payload, &rev, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, dbErr(err)
	}
	return decode(payload, rev, state)
}

// missing explains a failed CAS write: a deleted or unknown entry is not found, otherwise the revision is stale.
func (s *Store) missing(q querier, id string, rev int64) error {
	cur, err := s.load(q, id)
	if err != nil {
		return err
	}
	return &ConflictError{Current: cur}
}

// checkQuota refuses a new verified entry once MaxVerified is reached. Verified and stale entries are permanent and
// are never evicted to make room.
func (s *Store) checkQuota(tx *sql.Tx) error { return s.checkQuotaFreeing(tx, 0) }

// checkQuotaFreeing is checkQuota when freed permanent entries are retired in the same transaction, as a replacement
// promotion does: the swap is quota-neutral, so a full quota still admits the replacement.
func (s *Store) checkQuotaFreeing(tx *sql.Tx, freed int) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM memory WHERE workspace=? AND state IN (?,?)`, s.ws, StateVerified, StateStale).Scan(&n); err != nil {
		return dbErr(err)
	}
	if n-freed >= s.maxVerified {
		return ErrQuota
	}
	return nil
}

func (s *Store) insert(tx *sql.Tx, e Entry) error {
	payload, err := encode(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO memory(id, workspace, payload, updated_at, revision, state) VALUES(?,?,?,?,?,?)`,
		e.ID, s.ws, payload, e.Updated.Unix(), e.Revision, e.State)
	return dbErr(err)
}

// write stores next over the row whose revision is rev. Zero rows changed means another writer won the race.
func (s *Store) write(tx *sql.Tx, next Entry, rev int64) error {
	payload, err := encode(next)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE memory SET payload=?, revision=?, state=?, updated_at=? WHERE workspace=? AND id=? AND revision=? AND state<>?`,
		payload, next.Revision, next.State, next.Updated.Unix(), s.ws, next.ID, rev, StateDeleted)
	if err != nil {
		return dbErr(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return s.missing(tx, next.ID, rev)
	}
	return nil
}

func encode(e Entry) (string, error) {
	e.SchemaVersion = SchemaVersion
	e.Verified = e.State == StateVerified
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decode(payload string, rev int64, state string) (Entry, error) {
	var e Entry
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return Entry{}, fmt.Errorf("%w: unreadable memory row", ErrUnavailable)
	}
	e.Revision = rev
	e.State = state
	e.Verified = state == StateVerified
	return e, nil
}

func validate(e Entry) error {
	if !kinds[e.Kind] {
		return ErrInvalid
	}
	if n := utf8.RuneCountInString(e.Title); n == 0 || n > MaxTitle {
		return ErrInvalid
	}
	if utf8.RuneCountInString(e.Body) > MaxBody {
		return ErrInvalid
	}
	if len(e.Files) > MaxPaths || len(e.Symbols) > MaxSymbols {
		return ErrInvalid
	}
	for _, f := range e.Files {
		if !validPath(f) {
			return ErrInvalid
		}
	}
	for _, sym := range e.Symbols {
		if utf8.RuneCountInString(sym) == 0 || utf8.RuneCountInString(sym) > MaxSymbolLen {
			return ErrInvalid
		}
		if unsafe(sym) {
			return ErrUnsafe
		}
	}
	if unsafe(e.Title) || unsafe(e.Body) {
		return ErrUnsafe
	}
	for _, f := range e.Files {
		if unsafe(f) {
			return ErrUnsafe
		}
	}
	if e.SupersedesID != "" && !safeIDPattern.MatchString(e.SupersedesID) {
		return ErrInvalid
	}
	if e.StaleReason != "" {
		if err := validateStaleReason(e.StaleReason); err != nil {
			return err
		}
	}
	if err := validateScope(e.Scope); err != nil {
		return err
	}
	if err := validateCommand(e.Command); err != nil {
		return err
	}
	if err := validateEvidence(e.Evidence); err != nil {
		return err
	}
	if err := validateAnchor(e.Anchor); err != nil {
		return err
	}
	if e.Promotion != nil {
		if err := validateDecisionText(e.Promotion.HeadTurnID, e.Promotion.Rationale); err != nil {
			return err
		}
	}
	b, err := json.Marshal(e)
	if err != nil || len(b) > MaxEntryBytes {
		return ErrInvalid
	}
	return nil
}

// validateAnchor bounds the anchor: a hex commit, at most MaxPaths safe relative paths, each with a hex SHA-256 or the
// absence marker. Values over a bound are rejected, never truncated.
func validateAnchor(a *Anchor) error {
	if a == nil {
		return nil
	}
	if a.Commit != "" && !commitPattern.MatchString(a.Commit) {
		return ErrInvalid
	}
	if len(a.Files) > MaxPaths {
		return ErrInvalid
	}
	for p, h := range a.Files {
		if !validPath(p) {
			return ErrInvalid
		}
		if unsafe(p) {
			return ErrUnsafe
		}
		if h != AbsentMarker && !sha256Pattern.MatchString(h) {
			return ErrInvalid
		}
	}
	return nil
}

// validPath accepts only normalized relative paths inside the workspace.
func validPath(p string) bool {
	if p == "" || len(p) > MaxPathLen || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, ":") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" || part == ".relo" {
			return false
		}
	}
	return true
}

var unsafePatterns = []*regexp.Regexp{
	regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), // email
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY`),                    // private key
	regexp.MustCompile(`\b(sk|pk|rk)[-_][A-Za-z0-9_\-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_\-]{30,}`), // token shapes
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|passw(or)?d|passphrase|credentials?|private[_-]?key|authorization|bearer)\s*[:=]`),
	regexp.MustCompile(`://[^\s/@:]+:[^\s/@]+@`),                                          // credentials in URL
	regexp.MustCompile(`(?i)[?&](token|key|secret|sig|signature|password|access_token)=`), // sensitive query parameter
	regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}=\S`),                                         // environment assignment
	regexp.MustCompile(`<\|[^|>]*\|>|(?im)^\s*(system|assistant|human)\s*:`),              // role delimiters
}

var longToken = regexp.MustCompile(`[A-Za-z0-9+/_\-=]{32,}`)

// unsafe reports text that must never be stored: control characters, credential or contact shapes, instruction
// delimiters, and long high-entropy tokens. It is defense in depth, not a complete secret detector.
func unsafe(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return true
		}
	}
	for _, re := range unsafePatterns {
		if re.MatchString(s) {
			return true
		}
	}
	for _, tok := range longToken.FindAllString(s, -1) {
		hasDigit, hasLetter := false, false
		for _, r := range tok {
			hasDigit = hasDigit || unicode.IsDigit(r)
			hasLetter = hasLetter || unicode.IsLetter(r)
		}
		if hasDigit && hasLetter {
			return true
		}
	}
	return false
}

func nowUTC() time.Time { return time.Now().UTC() }

// migrate brings the catalog memory table to schema v2 in one transaction. Legacy rows become candidates and keep
// their ID and timestamps when valid; rows that fail intake are deleted and only counted; a malformed row rolls the
// whole migration back. The guard triggers are installed last, so only v2 payloads can be written afterwards.
// It is idempotent: v2 rows are skipped and the schema steps check before they change anything. Intake (and therefore
// what counts as malformed or scrubbed) is shared with the catalog legacy import through LegacyCandidate.
func migrate(db *sql.DB) (MigrationReport, error) {
	rep, scrub, err := migrateTx(db)
	if err != nil {
		return rep, err
	}
	if scrub {
		// Deleted legacy payloads must not survive in free pages or the WAL: VACUUM rewrites the file under
		// secure_delete, and the open-time Scrub truncates the WAL.
		if err := scrubFreePages(db); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// migrateTx is migrate up to the commit. scrub reports whether rows were deleted and need a page scrub.
func migrateTx(db *sql.DB) (rep MigrationReport, scrub bool, err error) {
	tx, err := db.Begin()
	if err != nil {
		return rep, false, dbErr(err)
	}
	defer tx.Rollback()
	if err := ensureColumns(tx); err != nil {
		return rep, false, err
	}
	// The file is scrubbed one row at a time while rows are rewritten, so deleted payloads never reach free pages.
	if _, err := tx.Exec(`PRAGMA secure_delete=ON`); err != nil {
		return rep, false, dbErr(err)
	}
	if _, err := tx.Exec(`DROP TRIGGER IF EXISTS memory_guard_insert; DROP TRIGGER IF EXISTS memory_guard_update;`); err != nil {
		return rep, false, dbErr(err)
	}
	rows, err := tx.Query(`SELECT id, workspace, payload, updated_at FROM memory`)
	if err != nil {
		return rep, false, dbErr(err)
	}
	type row struct {
		id, ws, payload string
		updated         int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ws, &r.payload, &r.updated); err != nil {
			rows.Close()
			return rep, false, dbErr(err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, false, dbErr(err)
	}
	for _, r := range all {
		var raw Entry
		if err := json.Unmarshal([]byte(r.payload), &raw); err != nil {
			return rep, false, malformedLegacy()
		}
		if raw.SchemaVersion == SchemaVersion {
			continue // already migrated, including content-free tombstones
		}
		e, ok, err := LegacyCandidate(r.id, r.ws, r.payload, r.updated)
		if err != nil {
			return rep, false, err
		}
		if !ok {
			if _, err := tx.Exec(`DELETE FROM memory WHERE id=?`, r.id); err != nil {
				return rep, false, dbErr(err)
			}
			rep.Scrubbed++
			continue
		}
		payload, err := encode(e)
		if err != nil {
			return rep, false, err
		}
		if _, err := tx.Exec(`UPDATE memory SET workspace=?, payload=?, revision=1, state=?, updated_at=? WHERE id=?`,
			WorkspaceID(r.ws), payload, StateCandidate, e.Updated.Unix(), r.id); err != nil {
			return rep, false, dbErr(err)
		}
		rep.Migrated++
	}
	if _, err := tx.Exec(persistence.MemoryGuardSQL); err != nil {
		return rep, false, dbErr(err)
	}
	if err := tx.Commit(); err != nil {
		return rep, false, dbErr(err)
	}
	return rep, rep.Scrubbed > 0, nil
}

// ensureColumns adds the v2 columns and index; the definition is shared with the catalog import.
func ensureColumns(tx *sql.Tx) error {
	if err := persistence.EnsureMemorySchema(tx); err != nil {
		return dbErr(err)
	}
	return nil
}

// dbErr maps a driver error to ErrUnavailable. The text carries no payload.
func dbErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// retry repeats an operation a bounded number of times while SQLite reports a busy or locked database. After that
// the failure is returned, never a blind overwrite.
func retry[T any](op func() (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		v, err := op()
		if err == nil || attempt == 5 || !Busy(err) {
			return v, err
		}
		time.Sleep(time.Duration(10*attempt) * time.Millisecond)
	}
}

// Busy reports whether err is a SQLite busy or locked error.
func Busy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_LOCKED")
}
