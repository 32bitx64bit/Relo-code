package persistence

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySpacePreflight(t *testing.T) {
	if err := checkLegacySpace(101, 121); err == nil {
		t.Fatal("expected insufficient space")
	}
	if err := checkLegacySpace(101, 122); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyImportFailurePreservesSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relo.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if err := importLegacyCatalog(path, catalog); err == nil {
		t.Fatal("expected import failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source changed")
	}
}

func TestLegacyEventsResumeNewestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err = legacy.Exec(testLegacySchema); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 300; i++ {
		if _, err = legacy.Exec(`INSERT INTO events(id,session_id,name,payload,created_at) VALUES(?,?,?,?,?)`, i, "s", "event", `{}`, i); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(testLegacySchema + `CREATE TABLE meta(k TEXT PRIMARY KEY,v TEXT NOT NULL); CREATE TRIGGER fail_event BEFORE INSERT ON events WHEN NEW.id=44 BEGIN SELECT RAISE(ABORT,'interrupted'); END;`); err != nil {
		t.Fatal(err)
	}
	if err = importLegacyEvents(path, "s", db); err == nil {
		t.Fatal("expected interruption")
	}
	var count, oldest int
	if err = db.QueryRow(`SELECT count(*),min(id) FROM events`).Scan(&count, &oldest); err != nil || count != 256 || oldest != 45 {
		t.Fatalf("count=%d oldest=%d err=%v", count, oldest, err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_event`); err != nil {
		t.Fatal(err)
	}
	if err = importLegacyEvents(path, "s", db); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil || count != 300 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

const testLegacySchema = `
CREATE TABLE kv(k TEXT PRIMARY KEY,v TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE sessions(id TEXT PRIMARY KEY,workspace TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE agents(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL,steps INTEGER NOT NULL DEFAULT 0,cache_read INTEGER NOT NULL DEFAULT 0,cache_write INTEGER NOT NULL DEFAULT 0);
CREATE TABLE tasks(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE generations(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,agent_id TEXT NOT NULL,number INTEGER NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE findings(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE memory(id TEXT PRIMARY KEY,workspace TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE reports(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,agent_id TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE usage(id INTEGER PRIMARY KEY AUTOINCREMENT,session_id TEXT NOT NULL,payload TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE changes(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE verification(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE events(id INTEGER PRIMARY KEY AUTOINCREMENT,session_id TEXT,name TEXT NOT NULL,payload TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE event_latest(session_id TEXT NOT NULL,name TEXT NOT NULL,payload TEXT NOT NULL,created_at INTEGER NOT NULL,PRIMARY KEY(session_id,name));
CREATE TABLE vworkspaces(workspace TEXT NOT NULL,agent_id TEXT NOT NULL,session_id TEXT NOT NULL,base_commit TEXT NOT NULL,revision INTEGER NOT NULL,tree_oid TEXT NOT NULL,state TEXT NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(workspace,agent_id));`

func TestLegacyImportResumesAndPreservesWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	legacy.SetMaxOpenConns(1)
	if _, err = legacy.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Exec(testLegacySchema); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err = legacy.Exec(`INSERT INTO sessions VALUES(?,?,?,?)`, id, "workspace", fmt.Sprintf(`{"id":%q}`, id), 1); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 300; i++ {
			if _, err = legacy.Exec(`INSERT INTO tasks(id,session_id,payload,updated_at) VALUES(?,?,?,?)`, fmt.Sprintf("%s-%03d", id, i), id, fmt.Sprintf(`{"n":%d}`, i), i); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = legacy.Exec(`INSERT INTO events(session_id,name,payload,created_at) VALUES(?,?,?,?),(?,?,?,?)`, id, "useful", `{"ok":true}`, 1, id, "session.updated", `{}`, 2); err != nil {
			t.Fatal(err)
		}
		if _, err = legacy.Exec(`INSERT INTO agents(id,session_id,payload,updated_at,steps,cache_read,cache_write) VALUES(?,?,?,?,?,?,?)`, "agent-"+id, id, `{}`, 1, 4, 5, 6); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(name string) [32]byte {
		t.Helper()
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	beforeDB, beforeWAL := hash(path), hash(path+"-wal")
	catalog, err := sql.Open("sqlite", filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	_, err = catalog.Exec(`CREATE TABLE kv(k TEXT PRIMARY KEY,v TEXT,updated_at INTEGER);
CREATE TABLE memory(id TEXT PRIMARY KEY,workspace TEXT,payload TEXT,updated_at INTEGER);
CREATE TABLE vworkspaces(workspace TEXT,agent_id TEXT,session_id TEXT,base_commit TEXT,revision INTEGER,tree_oid TEXT,state TEXT,payload TEXT,updated_at INTEGER,PRIMARY KEY(workspace,agent_id));
CREATE TABLE sessions(id TEXT PRIMARY KEY,workspace TEXT,updated_at INTEGER);
CREATE TABLE ids(kind TEXT,id TEXT,session_id TEXT,PRIMARY KEY(kind,id));`)
	if err != nil {
		t.Fatal(err)
	}
	if err = importLegacyCatalog(path, catalog); err != nil {
		t.Fatal(err)
	}
	if err = importLegacyCatalog(path, catalog); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = catalog.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("sessions=%d err=%v", n, err)
	}
	for _, id := range []string{"one", "two"} {
		db, err := sql.Open("sqlite", filepath.Join(dir, id+".db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(testLegacySchema); err != nil {
			t.Fatal(err)
		}
		if id == "one" {
			if _, err = db.Exec(`CREATE TRIGGER fail_import BEFORE INSERT ON tasks WHEN NEW.id='one-256' BEGIN SELECT RAISE(ABORT,'interrupted'); END`); err != nil {
				t.Fatal(err)
			}
			if err = importLegacySession(path, id, db); err == nil {
				t.Fatal("expected interrupted import")
			}
			if err = db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n); err != nil || n != 256 {
				t.Fatalf("first chunk=%d err=%v", n, err)
			}
			if _, err = db.Exec(`DROP TRIGGER fail_import`); err != nil {
				t.Fatal(err)
			}
		}
		if err = importLegacySession(path, id, db); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO events(session_id,name,payload,created_at) VALUES(?,?,?,?)`, id, "new", `{}`, 3); err != nil {
			t.Fatal(err)
		}
		if err = importLegacySession(path, id, db); err != nil {
			t.Fatal(err)
		}
		if err = importLegacyEvents(path, id, db); err != nil {
			t.Fatal(err)
		}
		if err = importLegacyEvents(path, id, db); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n); err != nil || n != 300 {
			t.Fatalf("tasks=%d err=%v", n, err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("events=%d err=%v", n, err)
		}
		if err = db.QueryRow(`SELECT steps FROM agents WHERE id=?`, "agent-"+id).Scan(&n); err != nil || n != 4 {
			t.Fatalf("steps=%d err=%v", n, err)
		}
		var payload string
		if err = db.QueryRow(`SELECT payload FROM sessions WHERE id=?`, id).Scan(&payload); err != nil || payload != fmt.Sprintf(`{"id":%q}`, id) {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if hash(path) != beforeDB || hash(path+"-wal") != beforeWAL {
		t.Fatal("legacy database or WAL changed")
	}
}
