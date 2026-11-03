package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relocode/relo/internal/persistence"
)

// openCatalog opens a real catalog file. Two calls on the same path behave like two Relo processes.
func openCatalog(t *testing.T, path string) *persistence.Store {
	t.Helper()
	p, err := persistence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func attach(t *testing.T, p *persistence.Store, workspace string) *Store {
	t.Helper()
	s, _, err := Open(p.DB(), workspace)
	if err != nil {
		t.Fatalf("memory open: %v", err)
	}
	return s
}

func rawCount(t *testing.T, p *persistence.Store, where string, args ...any) int {
	t.Helper()
	var n int
	if err := p.DB().QueryRow(`SELECT COUNT(*) FROM memory WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rawPayload(t *testing.T, p *persistence.Store, id string) string {
	t.Helper()
	var payload string
	if err := p.DB().QueryRow(`SELECT payload FROM memory WHERE id=?`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// insertLegacy seeds a pre-v2 row the way an older binary left it. The catalog guard refuses such writes, so the
// test lifts it for one statement and Open reinstalls it during migration.
func insertLegacy(t *testing.T, p *persistence.Store, id, ws, payload string) {
	t.Helper()
	if _, err := p.DB().Exec(`DROP TRIGGER IF EXISTS memory_guard_insert; DROP TRIGGER IF EXISTS memory_guard_update;`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at) VALUES(?,?,?,?)`, id, ws, payload, 1767225600); err != nil {
		t.Fatal(err)
	}
}

const legacyOK = `{"id":"mem_a","kind":"fact","title":"Run core tests","body":"Run go test from the core module.","files":["core/go.mod"],"score":0.9,"verified":true,"source":"worker","created":"2026-01-01T00:00:00Z","updated":"2026-01-01T00:00:00Z"}`

func TestLegacyRowsMigrateToCandidatesWithoutLeakingUnsafePayloads(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	ws := t.TempDir()
	insertLegacy(t, p, "mem_a", ws, legacyOK)
	insertLegacy(t, p, "mem_b", ws, `{"id":"mem_b","kind":"finding","title":"Login","body":"Mail alice.secret@example.com for the key","created":"2026-01-01T00:00:00Z"}`)
	insertLegacy(t, p, "mem_c", ws, `{"id":"mem_c","kind":"finding","title":"Shape","body":"Keep the report kind as a fact","created":"2026-01-01T00:00:00Z"}`)

	s, rep, err := Open(p.DB(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated != 2 || rep.Scrubbed != 1 {
		t.Fatalf("report = %+v, want 2 migrated 1 scrubbed", rep)
	}
	items, err := s.Load()
	if err != nil || len(items) != 2 {
		t.Fatalf("load = %d items, err %v", len(items), err)
	}
	for _, e := range items {
		if e.State != StateCandidate || e.Verified || e.Source != SourceLegacy || e.Revision != 1 || e.SchemaVersion != SchemaVersion {
			t.Fatalf("legacy %s not a clean candidate: %+v", e.ID, e)
		}
	}
	if items[0].Kind != "fact" || items[1].Kind != "fact" {
		t.Fatalf("kinds not normalized: %+v", items)
	}
	if n := rawCount(t, p, `payload LIKE '%@example.com%' OR payload LIKE '%alice%'`); n != 0 {
		t.Fatalf("unsafe payload survived migration in %d rows", n)
	}
	if n := rawCount(t, p, `workspace=?`, ws); n != 0 {
		t.Fatal("raw workspace path kept as partition")
	}
	if n := rawCount(t, p, `workspace=?`, WorkspaceID(ws)); n != 2 {
		t.Fatalf("opaque partition holds %d rows, want 2", n)
	}
	if strings.Contains(rawPayload(t, p, "mem_a"), ws) {
		t.Fatal("workspace path copied into payload")
	}
}

func TestMalformedLegacyRowRollsBackAndDisablesMemory(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	ws := t.TempDir()
	insertLegacy(t, p, "mem_a", ws, legacyOK)
	insertLegacy(t, p, "mem_bad", ws, `{"id":"mem_bad","body":"SECRET-PAYLOAD`)

	s, _, err := Open(p.DB(), ws)
	if err == nil {
		t.Fatal("malformed row must fail migration")
	}
	if strings.Contains(err.Error(), "SECRET-PAYLOAD") {
		t.Fatal("migration error leaked payload")
	}
	if n := rawCount(t, p, `workspace=?`, ws); n != 2 {
		t.Fatalf("rollback changed rows: %d still on raw path, want 2", n)
	}
	if items, err := s.Load(); err == nil || len(items) != 0 {
		t.Fatalf("unavailable store must not list: %d items, err %v", len(items), err)
	}
	if _, err := s.UserPromote(Entry{Kind: "fact", Title: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("write on failed migration = %v, want ErrUnavailable", err)
	}
}

func TestCapturedWritesStayDisabledAndRecallIsEmptyWithoutEntries(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	s := attach(t, p, t.TempDir())
	e := s.Upsert(Entry{Kind: "finding", Title: "Report", Body: "x", Verified: true})
	if e.Verified {
		t.Fatal("capture must not mark verified")
	}
	if n := rawCount(t, p, `1=1`); n != 0 || len(s.List()) != 0 {
		t.Fatal("capture persisted an entry")
	}
	if got := s.Recall("run the core tests"); len(got) != 0 {
		t.Fatalf("recall returned %d entries, want none", len(got))
	}
}

func TestUserWriteCommitsWithUserProvenanceAndNoCandidate(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	s := attach(t, p, t.TempDir())
	e, err := s.UserPromote(Entry{Kind: "command", Title: "Run core tests", Body: "go test from core", Source: "agent", State: StateCandidate, ID: "mem_forged"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Source != SourceUser || e.State != StateVerified || !e.Verified || e.Revision != 1 || e.ID == "mem_forged" {
		t.Fatalf("caller-supplied provenance was kept: %+v", e)
	}
	u, err := s.UserUpdate(e.ID, 1, Patch{Body: strPtr("go test ./... from core")})
	if err != nil {
		t.Fatal(err)
	}
	if u.Revision != 2 || u.Source != SourceUser || u.State != StateVerified {
		t.Fatalf("update = %+v", u)
	}
	if n := rawCount(t, p, `state=?`, StateCandidate); n != 0 {
		t.Fatalf("user write created %d candidate rows", n)
	}
}

// A user direct write keeps the scope, command and evidence it was given; core still fills provenance and revision.
func TestUserPromoteKeepsCommandScopeAndEvidence(t *testing.T) {
	s := attach(t, openCatalog(t, t.TempDir()+"/catalog.db"), t.TempDir())
	cmd := &Command{Argv: []string{"go", "test", "./..."}, Cwd: "core", Environment: "local Go toolchain"}
	scope := &Scope{Tags: []string{"tests"}}
	ev := Evidence{Type: EvidenceCommandResult, Ref: "ev_check", ObservedAt: time.Unix(1767225600, 0).UTC(), Result: ResultPassed, Command: "go test ./..."}
	e, err := s.UserPromote(Entry{Kind: "command", Title: "Run core tests", Body: "go test ./...", Command: cmd, Scope: scope, Evidence: []Evidence{ev}})
	if err != nil {
		t.Fatal(err)
	}
	if e.Command == nil || e.Command.Argv[1] != "test" || e.Command.Cwd != "core" || e.Command.Environment != "local Go toolchain" {
		t.Fatalf("command dropped: %+v", e.Command)
	}
	if e.Scope == nil || len(e.Scope.Tags) != 1 || e.Scope.Tags[0] != "tests" {
		t.Fatalf("scope dropped: %+v", e.Scope)
	}
	if len(e.Evidence) != 1 || e.Evidence[0].Ref != "ev_check" || e.Evidence[0].Command != "go test ./..." {
		t.Fatalf("evidence dropped: %+v", e.Evidence)
	}
	got, ok := s.Get(e.ID)
	if !ok || got.Command == nil || got.Scope == nil || len(got.Evidence) != 1 {
		t.Fatalf("reloaded entry lost fields: %+v, ok=%v", got, ok)
	}
}

// MarkStale is a revision-checked transition out of verified, so a replay or a non-verified entry is refused.
func TestMarkStaleIsCASAndStaleEntryStaysReviewable(t *testing.T) {
	s := attach(t, openCatalog(t, t.TempDir()+"/catalog.db"), t.TempDir())
	e := promotedVerified(t, s, "Core module", "ev_cas", Entry{})
	st, err := s.MarkStale(e.ID, e.Revision, "evidence changed on disk")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateStale || st.Verified || st.Revision != e.Revision+1 || st.StaleReason != "evidence changed on disk" {
		t.Fatalf("stale transition = %+v", st)
	}
	if got, ok := s.Get(e.ID); !ok || got.State != StateStale {
		t.Fatalf("stale entry not reviewable: %+v, ok=%v", got, ok)
	}
	_, err = s.MarkStale(e.ID, e.Revision, "replayed")
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current.Revision != e.Revision+1 {
		t.Fatalf("stale revision err = %v, want conflict at revision %d", err, e.Revision+1)
	}
	if _, err := s.MarkStale(e.ID, st.Revision, "already stale"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale entry re-marked: %v, want ErrInvalid", err)
	}
	if _, err := s.MarkStale(e.ID, st.Revision, "alice@example.com"); err == nil {
		t.Fatal("unsafe stale reason accepted")
	}
}

func TestStaleRevisionIsRejectedNotOverwritten(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	s := attach(t, p, t.TempDir())
	e, _ := s.UserPromote(Entry{Kind: "fact", Title: "Core module", Body: "Core is a Go module"})
	if _, err := s.UserUpdate(e.ID, 1, Patch{Body: strPtr("first edit")}); err != nil {
		t.Fatal(err)
	}
	_, err := s.UserUpdate(e.ID, 1, Patch{Body: strPtr("stale edit")})
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current.Revision != 2 || conflict.Current.Body != "first edit" {
		t.Fatalf("stale write err = %v, want conflict at revision 2", err)
	}
	if got, _ := s.Get(e.ID); got.Body != "first edit" {
		t.Fatalf("stale write overwrote body: %q", got.Body)
	}
}

func TestDeleteLeavesContentFreeTombstoneAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	p := openCatalog(t, path)
	ws := t.TempDir()
	s := attach(t, p, ws)
	e, _ := s.UserPromote(Entry{Kind: "pitfall", Title: "Secret plan title", Body: "Body text to remove"})
	if got := s.List(); len(got) != 1 {
		t.Fatalf("restart durability: %d entries before delete", len(got))
	}
	if err := s.UserDelete(e.ID, 1); err != nil {
		t.Fatal(err)
	}
	payload := rawPayload(t, p, e.ID)
	if strings.Contains(payload, "Secret plan title") || strings.Contains(payload, "Body text") {
		t.Fatalf("tombstone kept content: %s", payload)
	}
	if n := rawCount(t, p, `id=? AND state=?`, e.ID, StateDeleted); n != 1 {
		t.Fatal("tombstone row missing")
	}

	// A restarted process (session restore) must not see or resurrect the entry.
	p.Close()
	p2 := openCatalog(t, path)
	s2 := attach(t, p2, ws)
	if got := s2.List(); len(got) != 0 {
		t.Fatalf("deleted entry came back after restart: %+v", got)
	}
	if err := s2.UserDelete(e.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	if _, err := s2.UserUpdate(e.ID, 1, Patch{Body: strPtr("resurrect")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of tombstone = %v, want ErrNotFound", err)
	}
	if payload := rawPayload(t, p2, e.ID); strings.Contains(payload, "Secret plan title") || strings.Contains(payload, "resurrect") {
		t.Fatalf("tombstone reinserted content: %s", payload)
	}
	if n := rawCount(t, p2, `id=?`, e.ID); n != 1 {
		t.Fatalf("tombstone rows = %d, want 1", n)
	}
}

func TestTwoCatalogConnectionsRaceWithoutResurrection(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	ws := t.TempDir()
	pa := openCatalog(t, path)
	pb := openCatalog(t, path)
	a, b := attach(t, pa, ws), attach(t, pb, ws)
	e, _ := a.UserPromote(Entry{Kind: "fact", Title: "Race", Body: "start"})

	if _, err := a.UserUpdate(e.ID, 1, Patch{Body: strPtr("from a")}); err != nil {
		t.Fatal(err)
	}
	var conflict *ConflictError
	if _, err := b.UserUpdate(e.ID, 1, Patch{Body: strPtr("from b")}); !errors.As(err, &conflict) {
		t.Fatalf("b saw stale revision and wrote anyway: %v", err)
	}
	if err := b.UserDelete(e.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UserUpdate(e.ID, 2, Patch{Body: strPtr("late a")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update after delete = %v, want ErrNotFound", err)
	}
	if n := rawCount(t, pa, `id=? AND state<>?`, e.ID, StateDeleted); n != 0 {
		t.Fatal("delete lost to a stale update")
	}

	// Parallel edits of one revision: exactly one may win.
	f, _ := a.UserPromote(Entry{Kind: "fact", Title: "Parallel", Body: "start"})
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := a
			if i%2 == 1 {
				st = b
			}
			_, err := st.UserUpdate(f.ID, 1, Patch{Body: strPtr(fmt.Sprintf("edit %d", i))})
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d writers won revision 1, want exactly 1", wins)
	}
}

func TestWorkspaceIsolation(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	wsA, wsB := t.TempDir(), t.TempDir()
	a, b := attach(t, p, wsA), attach(t, p, wsB)
	e, _ := a.UserPromote(Entry{Kind: "fact", Title: "Only A", Body: "A only"})
	if len(b.List()) != 0 {
		t.Fatal("workspace B lists workspace A entries")
	}
	if _, ok := b.Get(e.ID); ok {
		t.Fatal("workspace B reads workspace A entry")
	}
	if err := b.UserDelete(e.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace delete = %v, want ErrNotFound", err)
	}
	if len(a.List()) != 1 {
		t.Fatal("cross-workspace delete touched workspace A")
	}
}

func TestQuotaNeverEvictsVerifiedEntries(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	s := attach(t, p, t.TempDir())
	s.maxVerified = 2
	for i := 0; i < 2; i++ {
		if _, err := s.UserPromote(Entry{Kind: "fact", Title: fmt.Sprintf("Kept %d", i), Body: "kept"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UserPromote(Entry{Kind: "fact", Title: "Over quota", Body: "x"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("over quota = %v, want ErrQuota", err)
	}
	for i := 0; i < MaxCandidates+5; i++ {
		if _, err := s.ProposeCandidate(Entry{Kind: "fact", Title: fmt.Sprintf("Cand %d", i), Body: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := rawCount(t, p, `state=?`, StateCandidate); n != MaxCandidates {
		t.Fatalf("candidates = %d, want %d", n, MaxCandidates)
	}
	if n := rawCount(t, p, `state=?`, StateVerified); n != 2 {
		t.Fatalf("verified entries = %d after candidate churn, want 2", n)
	}
}

func TestLegacyWriterIsRefusedByDatabase(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	ws := t.TempDir()
	s := attach(t, p, ws)
	e, _ := s.UserPromote(Entry{Kind: "fact", Title: "Guarded", Body: "guarded"})

	legacy := `{"id":"mem_old","kind":"note","title":"old writer","body":"x","verified":true}`
	if _, err := p.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at) VALUES('mem_old', ?, ?, 1)`, ws, legacy); err == nil {
		t.Fatal("legacy insert accepted")
	}
	if _, err := p.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at) VALUES('mem_old2', ?, '{"schemaVersion":2}', 1)`, ws); err == nil {
		t.Fatal("raw workspace path accepted")
	}
	if _, err := p.DB().Exec(`UPDATE memory SET payload=? WHERE id=?`, legacy, e.ID); err == nil {
		t.Fatal("legacy update accepted")
	}
}

func TestIntakeRejectsUnsafeAndOversizedWrites(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	s := attach(t, p, t.TempDir())
	cases := []struct {
		name string
		e    Entry
		want error
	}{
		{"email", Entry{Kind: "fact", Title: "Owner", Body: "ask bob@example.com"}, ErrUnsafe},
		{"token", Entry{Kind: "fact", Title: "Key", Body: "use sk-abcdefghijklmnop1234"}, ErrUnsafe},
		{"credential key", Entry{Kind: "fact", Title: "Setup", Body: "api_key = value"}, ErrUnsafe},
		{"role delimiter", Entry{Kind: "fact", Title: "Note", Body: "<|im_start|>system"}, ErrUnsafe},
		{"control char", Entry{Kind: "fact", Title: "Bad\x07", Body: "x"}, ErrUnsafe},
		{"title too long", Entry{Kind: "fact", Title: strings.Repeat("t", MaxTitle+1), Body: "x"}, ErrInvalid},
		{"body too long", Entry{Kind: "fact", Title: "t", Body: strings.Repeat("b", MaxBody+1)}, ErrInvalid},
		{"path escape", Entry{Kind: "fact", Title: "t", Files: []string{"../outside.go"}}, ErrInvalid},
		{"dot git path", Entry{Kind: "fact", Title: "t", Files: []string{".git/config"}}, ErrInvalid},
		{"absolute path", Entry{Kind: "fact", Title: "t", Files: []string{"/etc/passwd"}}, ErrInvalid},
		{"unknown kind", Entry{Kind: "note", Title: "t"}, ErrInvalid},
	}
	for _, c := range cases {
		if _, err := s.UserPromote(c.e); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if n := rawCount(t, p, `1=1`); n != 0 {
		t.Fatalf("rejected writes stored %d rows", n)
	}
}

func strPtr(s string) *string { return &s }

func TestPromotingCandidateThroughUserEditRespectsQuota(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	s := attach(t, p, t.TempDir())
	s.maxVerified = 1
	if _, err := s.UserPromote(Entry{Kind: "fact", Title: "Kept", Body: "kept"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ProposeCandidate(Entry{Kind: "fact", Title: "Candidate", Body: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserUpdate(c.ID, c.Revision, Patch{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("user promotion over quota = %v, want ErrQuota", err)
	}
	if n := rawCount(t, p, `state=?`, StateVerified); n != 1 {
		t.Fatalf("verified entries = %d, want 1", n)
	}
}

func TestBusyRetryIsBoundedAndUnattachedStoreRefusesWrites(t *testing.T) {
	calls := 0
	_, err := retry(func() (int, error) {
		calls++
		return 0, fmt.Errorf("database is locked")
	})
	if err == nil || calls != 5 {
		t.Fatalf("busy retry calls = %d, err = %v; want 5 attempts and an error", calls, err)
	}
	s := New()
	if _, err := s.UserPromote(Entry{Kind: "fact", Title: "Lost", Body: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("write without catalog = %v, want ErrUnavailable", err)
	}
	if got := s.Recall("anything"); got != nil || len(s.List()) != 0 {
		t.Fatal("unattached store returned memory")
	}
}

// Migration rewrites pre-v2 rows and the open-time scrub truncates the WAL, so nothing of the raw legacy payload
// stays in the WAL while the migrated candidate keeps its content (design §6).
func TestOpenScrubsLegacyWalAndKeepsMigratedCandidates(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	ws := t.TempDir()
	body := strings.Repeat("legacy plain body line ", 20)
	p := openCatalog(t, path)
	insertLegacy(t, p, "mem_big", ws, `{"id":"mem_big","kind":"fact","title":"Big","body":"`+body+`"}`)
	p.Close()

	p2 := openCatalog(t, path)
	_, rep, err := Open(p2.DB(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Migrated != 1 || rep.Scrubbed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("WAL not truncated after migration: %d bytes", info.Size())
	}
	items := attach(t, p2, ws).List()
	if len(items) != 1 || items[0].State != StateCandidate || items[0].Body != body {
		t.Fatalf("migrated candidate = %+v", items)
	}
	if n := rawCount(t, p2, `workspace=?`, ws); n != 0 {
		t.Fatal("raw workspace path kept as partition")
	}
}

// Scrub applies the page policy to a row a user delete freed under secure_delete: the WAL is truncated and asking for
// secure_delete turns it on for every later write of this connection.
func TestScrubEnablesSecureDeleteAndTruncatesWAL(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	p := openCatalog(t, path)
	s := attach(t, p, t.TempDir())
	e, err := s.UserPromote(Entry{Kind: "fact", Title: "Plan", Body: strings.Repeat("secret-plan-step ", 30)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UserDelete(e.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Scrub(); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA secure_delete`).Scan(&mode); err != nil || mode != "1" {
		t.Fatalf("secure_delete=%q err=%v", mode, err)
	}
	var journal string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || !strings.EqualFold(journal, "wal") {
		t.Fatalf("journal_mode=%q err=%v", journal, err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("WAL not truncated: %d bytes", info.Size())
	}
	if payload := rawPayload(t, p, e.ID); strings.Contains(payload, "secret-plan-step") {
		t.Fatalf("tombstone kept content: %s", payload)
	}
}

// A Phase 1 schemaVersion 2 row has none of the Phase 2 fields. It must load with them empty and keep its state,
// source and revision, so the additive JSON fields need no SQL migration.
func TestPhase1RowLoadsUnchangedWithPhase2Fields(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	dir := t.TempDir()
	s := attach(t, p, dir)
	payload := `{"schemaVersion":2,"id":"mem_phase1","revision":4,"kind":"fact","state":"verified","title":"Old fact",` +
		`"body":"kept as written","source":"user","verified":true,"created":"2026-01-01T00:00:00Z","updated":"2026-01-02T00:00:00Z"}`
	if _, err := p.DB().Exec(`INSERT INTO memory(id, workspace, payload, updated_at, revision, state) VALUES(?,?,?,?,?,?)`,
		"mem_phase1", WorkspaceID(dir), payload, 1767312000, 4, StateVerified); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("mem_phase1")
	if !ok {
		t.Fatal("phase 1 row not loaded")
	}
	if e.Revision != 4 || e.State != StateVerified || e.Source != SourceUser || e.Body != "kept as written" {
		t.Fatalf("phase 1 row changed on load: %+v", e)
	}
	if e.Command != nil || e.Scope != nil || e.Evidence != nil || e.Promotion != nil || e.SupersedesID != "" || e.Flagged {
		t.Fatalf("phase 2 fields not empty on phase 1 row: %+v", e)
	}
	if got, err := s.Load(); err != nil || len(got) != 1 {
		t.Fatalf("load = %d entries, %v", len(got), err)
	}
}

// TestLegacyFormatInsertFromSecondConnectionIsRefusedAroundMigration checks that a pre-v2 INSERT from another connection
// is refused by the guard after memory.Open migrates the catalog, not merely failed by a lock.
func TestLegacyFormatInsertFromSecondConnectionIsRefusedAroundMigration(t *testing.T) {
	path := t.TempDir() + "/catalog.db"
	p := openCatalog(t, path)
	ws := t.TempDir()
	insertLegacy(t, p, "mem_seed", ws, legacyOK)

	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	legacy := `INSERT INTO memory(id, workspace, payload, updated_at) VALUES('mem_race', ?, '{"id":"mem_race","kind":"fact","title":"t","body":"b"}', 1)`
	refused := func(stage string) {
		t.Helper()
		_, err := other.Exec(legacy, ws)
		if err == nil || !strings.Contains(err.Error(), "memory write refused") {
			t.Fatalf("%s: legacy insert from second connection = %v, want guard refusal", stage, err)
		}
	}
	if _, _, err := Open(p.DB(), ws); err != nil {
		t.Fatalf("memory open: %v", err)
	}
	refused("after open")
	if n := rawCount(t, p, `id='mem_race'`); n != 0 {
		t.Fatalf("legacy row stored despite guard: %d", n)
	}
}

// TestLegacyWorkspacesKeepDistinctPartitions checks that legacy rows from two workspace values stay in two opaque
// partitions instead of all moving to the workspace that ran the migration.
func TestLegacyWorkspacesKeepDistinctPartitions(t *testing.T) {
	p := openCatalog(t, t.TempDir()+"/catalog.db")
	wsA, wsB := t.TempDir(), t.TempDir()
	insertLegacy(t, p, "mem_a", wsA, legacyOK)
	insertLegacy(t, p, "mem_b", wsB, `{"id":"mem_b","kind":"fact","title":"Other","body":"Second workspace fact","created":"2026-01-01T00:00:00Z"}`)

	if _, _, err := Open(p.DB(), wsA); err != nil {
		t.Fatal(err)
	}
	if got := rawCount(t, p, `workspace=?`, WorkspaceID(wsA)); got != 1 {
		t.Fatalf("rows in workspace A partition = %d, want 1", got)
	}
	if got := rawCount(t, p, `workspace=?`, WorkspaceID(wsB)); got != 1 {
		t.Fatalf("rows in workspace B partition = %d, want 1", got)
	}
}

// Anchors are validated on every write path, kept through promotion and user edits, and never widen the bounds.
func TestAnchorIsValidatedAndKeptOnPromotionAndUpdate(t *testing.T) {
	s, _ := phase2Store(t)
	anchor := &Anchor{Commit: "abcdef1", Files: map[string]string{"core/go.mod": strings.Repeat("b", 64), "core/gone.go": AbsentMarker}}
	c := mustPropose(t, s, Entry{Kind: "fact", Title: "Anchored fact", Body: "anchored", Files: []string{"core/go.mod"},
		Anchor: anchor, Evidence: []Evidence{fileEvidence("ref_anchor", "core/go.mod")}})
	if c.Anchor == nil || c.Anchor.Commit != "abcdef1" || c.Anchor.Files["core/gone.go"] != AbsentMarker {
		t.Fatalf("proposal dropped anchor: %+v", c.Anchor)
	}
	v, err := s.HeadPromote(c.ID, c.Revision, PromotionRequest{HeadTurnID: "turn_anchor", EvidenceRefs: []string{"ref_anchor"}, Rationale: "kept"})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if v.Anchor == nil || v.Anchor.Commit != "abcdef1" || len(v.Anchor.Files) != 2 {
		t.Fatalf("promotion dropped anchor: %+v", v.Anchor)
	}
	u, err := s.UserUpdate(v.ID, v.Revision, Patch{Title: strPtr("Anchored fact edited")})
	if err != nil {
		t.Fatalf("user update: %v", err)
	}
	if got, ok := s.Get(u.ID); !ok || got.Anchor == nil || got.Anchor.Files["core/go.mod"] != strings.Repeat("b", 64) {
		t.Fatalf("user update dropped anchor: %+v", got.Anchor)
	}

	many := map[string]string{}
	for i := 0; i <= MaxPaths; i++ {
		many[fmt.Sprintf("core/f%d.go", i)] = AbsentMarker
	}
	bad := []struct {
		name   string
		anchor *Anchor
		want   error
	}{
		{"bad commit", &Anchor{Commit: "not-hex"}, ErrInvalid},
		{"absolute path", &Anchor{Files: map[string]string{"/etc/passwd": AbsentMarker}}, ErrInvalid},
		{"parent path", &Anchor{Files: map[string]string{"../escape.go": AbsentMarker}}, ErrInvalid},
		{"short hash", &Anchor{Files: map[string]string{"core/go.mod": "abc"}}, ErrInvalid},
		{"too many paths", &Anchor{Files: many}, ErrInvalid},
		{"secret-shaped path", &Anchor{Files: map[string]string{"core/api_key=x.go": AbsentMarker}}, ErrUnsafe},
	}
	for _, b := range bad {
		if _, err := s.ProposeCandidate(Entry{Kind: "fact", Title: "Bad anchor", Body: "x", Anchor: b.anchor}); !errors.Is(err, b.want) {
			t.Fatalf("%s: got %v, want %v", b.name, err, b.want)
		}
	}
}

// UserPromote keeps the candidate's command, scope, evidence and anchor as supplied, with user provenance.
func TestUserPromoteKeepsCandidateFields(t *testing.T) {
	s, _ := phase2Store(t)
	e, err := s.UserPromote(Entry{Kind: "command", Title: "Run memory tests", Body: "go test from core",
		Files: []string{"core/internal/memory/store.go"}, Scope: &Scope{Tags: []string{"core"}},
		Command:  &Command{Argv: []string{"go", "test", "./internal/memory"}, Cwd: "core"},
		Evidence: []Evidence{fileEvidence("ref_user", "core/internal/memory/store.go")},
		Anchor:   &Anchor{Commit: "abcdef1"}})
	if err != nil {
		t.Fatalf("user promote: %v", err)
	}
	got, ok := s.Get(e.ID)
	if !ok || got.Source != SourceUser || got.State != StateVerified {
		t.Fatalf("provenance not filled by core: %+v", got)
	}
	if got.Scope == nil || len(got.Scope.Tags) != 1 || got.Command == nil || got.Command.Cwd != "core" ||
		len(got.Evidence) != 1 || got.Anchor == nil || got.Anchor.Commit != "abcdef1" {
		t.Fatalf("user promote dropped candidate fields: %+v", got)
	}
}

// Stale is reachable only through MarkStale: revision checked, verified agent entries only, never user entries.
// A stale entry leaves the permanent quota counted, and no user edit silently revives it.
func TestMarkStaleIsRevisionCheckedAndNeverTouchesUserEntries(t *testing.T) {
	s, _ := phase2Store(t)
	v := promotedVerified(t, s, "Stale candidate", "ref_stale", Entry{})
	var ce *ConflictError
	if _, err := s.MarkStale(v.ID, v.Revision+1, "bytes changed"); !errors.As(err, &ce) {
		t.Fatalf("stale revision: got %v, want conflict", err)
	}
	st, err := s.MarkStale(v.ID, v.Revision, "bytes changed")
	if err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	if st.State != StateStale || st.Verified || st.Revision != v.Revision+1 || st.Source != SourceHead {
		t.Fatalf("stale transition wrong: %+v", st)
	}
	if _, err := s.MarkStale(v.ID, st.Revision, "bytes changed"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second stale: got %v, want invalid", err)
	}
	if _, err := s.UserUpdate(v.ID, st.Revision, Patch{Title: strPtr("Revived")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("user revive of agent stale entry: got %v, want invalid", err)
	}
	if got, ok := s.Get(v.ID); !ok || got.State != StateStale || got.Title != "Stale candidate" {
		t.Fatalf("stale entry changed: %+v", got)
	}
	s.maxVerified = 1
	if _, err := s.UserPromote(Entry{Kind: "fact", Title: "Over quota", Body: "x"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("stale entry must count toward permanent quota: got %v", err)
	}
	s.maxVerified = MaxVerified

	u, err := s.UserPromote(Entry{Kind: "fact", Title: "User fact", Body: "user"})
	if err != nil {
		t.Fatalf("user promote: %v", err)
	}
	if _, err := s.MarkStale(u.ID, u.Revision, "expired"); !errors.Is(err, ErrUserReview) {
		t.Fatalf("user entry staled: got %v, want user review", err)
	}
	if got, ok := s.Get(u.ID); !ok || got.State != StateVerified || got.Revision != u.Revision || got.Title != u.Title {
		t.Fatalf("user entry changed by refused MarkStale: %+v", got)
	}
	if _, err := s.MarkStale("mem_missing", 1, "expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: got %v, want not found", err)
	}
}
