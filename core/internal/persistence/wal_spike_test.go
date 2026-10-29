package persistence

import (
	"crypto/sha256"
	"database/sql"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyWALSpike(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relo.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE x (n INTEGER); INSERT INTO x VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	hash := func(p string) [32]byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	beforeDB, beforeWAL := hash(path), hash(path+"-wal")
	ro, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ro.QueryRow(`SELECT n FROM x`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// Keep the read-only connection open while the last writer closes.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if n != 1 || hash(path) != beforeDB || hash(path+"-wal") != beforeWAL {
		t.Fatal("read-only open changed legacy database or WAL")
	}
}

// Copying the database and WAL lets SQLite update only the copy's shared memory.
func TestCopiedWALReadLeavesLegacyFilesUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relo.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE x (n INTEGER); INSERT INTO x VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	files := []string{path, path + "-wal", path + "-shm"}
	before := make([][32]byte, len(files))
	for i, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		before[i] = sha256.Sum256(data)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	for _, suffix := range []string{"", "-wal"} {
		src, err := os.Open(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		dst, err := os.OpenFile(copyPath+suffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			src.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	ro, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: copyPath}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ro.QueryRow(`SELECT n FROM x`).Scan(&n); err != nil {
		ro.Close()
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("copied WAL row = %d, want 1", n)
	}
	for i, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(data) != before[i] {
			t.Fatalf("legacy file %s changed", filepath.Base(name))
		}
	}
}
