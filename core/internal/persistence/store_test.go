package persistence

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	type sess struct {
		ID        string
		Workspace string
		Objective string
		Updated   time.Time
	}
	in := sess{ID: "s1", Workspace: "/tmp/p", Objective: "ship", Updated: time.Now().UTC()}
	if err := s.PutJSON("sessions", in.ID, "", in.Workspace, in); err != nil {
		t.Fatal(err)
	}
	var out sess
	if err := s.Get("sessions", "s1", &out); err != nil {
		t.Fatal(err)
	}
	if out.Objective != "ship" {
		t.Fatalf("%+v", out)
	}
	list, err := s.List("sessions", "/tmp/p")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
}

func TestRejectsUnknownTableAndSecuresFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "data #1.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Get("sessions; DROP TABLE sessions", "x", &struct{}{}); err == nil {
		t.Fatal("expected invalid table rejection")
	}
	if _, err := s.List("other", "x"); err == nil {
		t.Fatal("expected invalid table rejection")
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o", info.Mode().Perm())
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
}
