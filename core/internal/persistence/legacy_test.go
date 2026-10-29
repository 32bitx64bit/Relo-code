package persistence

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenImportsLegacyLiveWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`, testLegacySchema,
		`INSERT INTO sessions VALUES('s','workspace','{"id":"s"}',1)`,
		`INSERT INTO agents(id,session_id,payload,updated_at,steps) VALUES('a','s','{"id":"a"}',1,9)`,
		`INSERT INTO tasks VALUES('t','s','{"id":"t"}',1)`,
		`INSERT INTO reports VALUES('r','s','a','{"id":"r"}',1)`,
		`INSERT INTO events(session_id,name,payload,created_at) VALUES('s','event','{"id":"e"}',1),('s','session.updated','{}',2)`,
	} {
		if _, err := writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(p string) [32]byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	dbHash, walHash := hash(path), hash(path+"-wal")
	shm, err := os.Stat(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM sessions WHERE id='s'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("catalog: %d, %v", count, err)
	}
	sessions, err := s.ListSessions("workspace")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %s, %v", sessions, err)
	}
	for _, table := range []string{"agents", "tasks", "reports"} {
		rows, err := s.List(table, "s")
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: %s, %v", table, rows, err)
		}
	}
	var agent map[string]any
	if err := s.Get("agents", "a", &agent); err != nil {
		t.Fatal(err)
	}
	if err := s.Get("sessions", "s", &agent); err != nil {
		t.Fatal(err)
	}
	events, err := s.EventRows("s", 10)
	if err != nil || len(events) != 1 || events[0].Name != "event" {
		t.Fatalf("events: %+v, %v", events, err)
	}
	if hash(path) != dbHash || hash(path+"-wal") != walHash {
		t.Fatal("legacy database or WAL changed")
	}
	after, err := os.Stat(path + "-shm")
	if err != nil || after.Size() < shm.Size() {
		t.Fatalf("shm: %v, %v", after, err)
	}
}

func TestLegacySessionRetryAfterInterruptedChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.Exec(testLegacySchema + `INSERT INTO sessions VALUES('s','w','{}',1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if _, err = writer.Exec(`INSERT INTO tasks VALUES(?,?,?,?)`, fmt.Sprintf("t%03d", i), "s", `{}`, i); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	target, err := openDB(filepath.Join(dir, "sessions", "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = (&Store{db: target}).migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err = target.Exec(`CREATE TABLE meta(k TEXT PRIMARY KEY,v TEXT NOT NULL); CREATE TRIGGER interrupt BEFORE INSERT ON tasks WHEN NEW.id='t256' BEGIN SELECT RAISE(ABORT,'interrupted'); END;`); err != nil {
		t.Fatal(err)
	}
	target.Close()
	if _, err := s.List("tasks", "s"); err == nil {
		t.Fatal("expected interrupted import")
	}
	target, err = openDB(filepath.Join(dir, "sessions", "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = target.QueryRow(`SELECT count(*) FROM tasks`).Scan(&count); err != nil || count != 256 {
		t.Fatalf("partial: %d, %v", count, err)
	}
	if _, err = target.Exec(`DROP TRIGGER interrupt`); err != nil {
		t.Fatal(err)
	}
	target.Close()
	rows, err := s.List("tasks", "s")
	if err != nil || len(rows) != 300 {
		t.Fatalf("resumed: %d, %v", len(rows), err)
	}
	if err := s.CloseSession("s"); err != nil {
		t.Fatal(err)
	}
	rows, err = s.List("tasks", "s")
	if err != nil || len(rows) != 300 {
		t.Fatalf("reopened: %d, %v", len(rows), err)
	}
}

func TestUnreadableLegacyOpenLogsAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relo.db")
	data := []byte("not a sqlite database")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.Contains(output.String(), "legacy catalog import") {
		t.Fatalf("missing log: %s", output.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("legacy changed: %v", err)
	}
}

// legacyDigest hashes relo.db and relo.db-wal so tests can prove they stay byte-identical.
func legacyDigest(t *testing.T, path string) string {
	t.Helper()
	out := ""
	for _, p := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out += fmt.Sprintf("%s:%x;", filepath.Base(p), sha256.Sum256(b))
	}
	return out
}

func TestPresetCursorBeyondLegacyMaxStillImports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.Exec(testLegacySchema + `
INSERT INTO sessions VALUES('s','w','{"id":"s"}',1);
INSERT INTO tasks VALUES('t1','s','{"id":"t1"}',1),('t2','s','{"id":"t2"}',2);`); err != nil {
		t.Fatal(err)
	}
	before := legacyDigest(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Preset cursors far above the legacy max rowid, as left by the review repro.
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := openDB(filepath.Join(dir, "sessions", "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Store{db: target}).migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Exec(`CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY,v TEXT NOT NULL); INSERT INTO meta(k,v) VALUES('legacy:sessions','9999'),('legacy:tasks','9999')`); err != nil {
		t.Fatal(err)
	}
	target.Close()
	rows, err := s.List("tasks", "s")
	if err != nil || len(rows) != 2 {
		t.Fatalf("tasks: %d, %v", len(rows), err)
	}
	var session map[string]any
	if err := s.GetIn("sessions", "s", "s", &session); err != nil {
		t.Fatalf("session row not imported: %v", err)
	}
	if after := legacyDigest(t, path); after != before {
		t.Fatalf("legacy database changed: %s -> %s", before, after)
	}
}

func TestCatalogImportFailureLeavesNoMarkerAndRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.Exec(testLegacySchema + `
INSERT INTO kv VALUES('k','v',1);
INSERT INTO sessions VALUES('s','w','{"id":"s"}',1);
INSERT INTO tasks VALUES('t1','s','{"id":"t1"}',1);`); err != nil {
		t.Fatal(err)
	}
	before := legacyDigest(t, path)
	// Inject a failure after sessions and kv rows are written but before the marker.
	catalog, err := openDB(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Store{db: catalog}).migrateCatalog(); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Exec(`CREATE TRIGGER fail_ids BEFORE INSERT ON ids WHEN NEW.kind='tasks' BEGIN SELECT RAISE(ABORT,'interrupted'); END;`); err != nil {
		t.Fatal(err)
	}
	catalog.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var marker, rows int
	// The marker table is created inside the failed transaction, so it may not exist.
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='legacy_import'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 0 {
		if err := s.db.QueryRow(`SELECT count(*) FROM legacy_import WHERE k='catalog'`).Scan(&marker); err != nil {
			t.Fatal(err)
		}
	}
	if marker != 0 {
		t.Fatalf("marker after failed import: %d, %v", marker, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("partial sessions after failed import: %d, %v", rows, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM kv`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("partial kv after failed import: %d, %v", rows, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	catalog, err = openDB(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Exec(`DROP TRIGGER fail_ids`); err != nil {
		t.Fatal(err)
	}
	catalog.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.db.QueryRow(`SELECT count(*) FROM legacy_import WHERE k='catalog' AND v='done'`).Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("marker after retry: %d, %v", marker, err)
	}
	sessions, err := s.ListSessions("w")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions after retry: %d, %v", len(sessions), err)
	}
	if after := legacyDigest(t, path); after != before {
		t.Fatalf("legacy database changed: %s -> %s", before, after)
	}
}

func TestListSessionsSkipsFailingSessionDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.Exec(testLegacySchema + `
INSERT INTO sessions VALUES('bad','w','{"id":"bad"}',2);
INSERT INTO sessions VALUES('good','w','{"id":"good"}',1);
INSERT INTO tasks VALUES('tb','bad','{}',1),('tg','good','{}',1);`); err != nil {
		t.Fatal(err)
	}
	before := legacyDigest(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Make the import of the "bad" session DB fail on its first task row.
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := openDB(filepath.Join(dir, "sessions", "bad.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Store{db: target}).migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Exec(`CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY,v TEXT NOT NULL); CREATE TRIGGER fail_tasks BEFORE INSERT ON tasks BEGIN SELECT RAISE(ABORT,'interrupted'); END;`); err != nil {
		t.Fatal(err)
	}
	target.Close()
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	sessions, err := s.ListSessions("w")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(sessions) != 1 || !strings.Contains(string(sessions[0]), `"good"`) {
		t.Fatalf("sessions: %s", sessions)
	}
	if !strings.Contains(output.String(), `skip session "bad"`) {
		t.Fatalf("missing skip log: %s", output.String())
	}
	latest, err := s.LatestSession("w")
	if err != nil || !strings.Contains(string(latest), `"good"`) {
		t.Fatalf("latest: %s, %v", latest, err)
	}
	rows, err := s.List("tasks", "good")
	if err != nil || len(rows) != 1 {
		t.Fatalf("good tasks: %d, %v", len(rows), err)
	}
	if after := legacyDigest(t, path); after != before {
		t.Fatalf("legacy database changed: %s -> %s", before, after)
	}
}
