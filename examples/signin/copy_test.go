package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopy(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "d.db")+"?_pragma=journal_mode(wal)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE a (x TEXT); INSERT INTO a VALUES ('one')`)
	out := filepath.Join(dir, "outbox", "local-apps")
	src := []source{{db, "d.db"}}
	if !lastCopy(src, out).IsZero() {
		t.Fatal("a copy before any was made")
	}
	read := func() (n int) {
		t.Helper()
		c, _ := sql.Open("sqlite3", "file:"+filepath.Join(out, "d.db")+"?mode=ro")
		defer c.Close()
		if err := c.QueryRow(`SELECT COUNT(*) FROM a`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := copyAll(src, out); err != nil {
		t.Fatal(err)
	}
	if read() != 1 {
		t.Fatal("the copy is empty")
	}
	// The next copy replaces one the outbox hasn't taken.
	db.Exec(`INSERT INTO a VALUES ('two')`)
	if err := copyAll(src, out); err != nil {
		t.Fatal(err)
	}
	if read() != 2 {
		t.Fatal("the copy is old")
	}
	if unstored(src, out) != "d.db" {
		t.Fatal("it should be waiting for the outbox")
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	if fi, _ := os.Stat(filepath.Join(out, "d.db")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("readable by others: %v", fi.Mode())
	}
	// Stored, the stub says when.
	os.Rename(filepath.Join(out, "d.db"), filepath.Join(out, "d.db.ystub"))
	if unstored(src, out) != "" || time.Since(lastCopy(src, out)) > time.Minute {
		t.Fatal("the stub wasn't counted")
	}
}
