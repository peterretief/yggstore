package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImport(t *testing.T) {
	dir := t.TempDir()
	path, file := filepath.Join(dir, "d.db"), filepath.Join(dir, "d.sql")
	run := func(stmts string, drop bool) (string, error) {
		t.Helper()
		os.WriteFile(file, []byte(stmts), 0o644)
		var out bytes.Buffer
		err := importDesign(path, file, drop, &out)
		return out.String(), err
	}
	ok := func(stmts string, drop bool, want string) {
		t.Helper()
		out, err := run(stmts, drop)
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("%q: %v", out, err)
		}
	}
	value := func(q string) string {
		t.Helper()
		db, _ := sql.Open("sqlite3", "file:"+path)
		defer db.Close()
		var s string
		if err := db.QueryRow(q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	const parents = `CREATE TABLE clients (id INTEGER PRIMARY KEY, name TEXT NOT NULL);`
	const kids = `CREATE TABLE jobs (id INTEGER PRIMARY KEY, title TEXT, client_id INTEGER REFERENCES clients(id));`
	const rows = `INSERT INTO clients VALUES (1, 'Acme'); INSERT INTO jobs VALUES (1, 'Site', 1);`

	ok(parents+kids+rows, false, "Added table clients")
	if value(`SELECT name FROM clients`) != "Acme" {
		t.Fatal("the row wasn't added")
	}
	ok(parents+kids+rows, false, "already matches")

	// A mistake changes nothing.
	if _, err := run(parents+"CREATE TABEL oops (id);", false); err == nil {
		t.Fatal("took a mistake")
	}
	// A new column and a new rule keep the rows, and what refers to them.
	const parents2 = `CREATE TABLE clients (id INTEGER PRIMARY KEY, name TEXT NOT NULL CHECK (name <> ''), email TEXT);`
	ok(parents2+kids, false, "Changed table clients: added email")
	if value(`SELECT name FROM clients`) != "Acme" || value(`SELECT client_id FROM jobs`) != "1" {
		t.Fatal("rows were lost")
	}
	if !strings.Contains(value(`SELECT sql FROM sqlite_master WHERE name = 'jobs'`), "clients") {
		t.Fatal("the reference changed")
	}
	ok(parents2+kids, false, "already matches") // the rebuilt table reads the same
	matches, _ := filepath.Glob(path + ".before-import-*")
	if len(matches) == 0 {
		t.Fatal("no copy was kept")
	}

	// Nothing goes without -drop.
	if _, err := run(parents+kids, false); err == nil || !strings.Contains(err.Error(), "clients.email") {
		t.Fatalf("a column: %v", err)
	}
	if _, err := run(parents2, false); err == nil || !strings.Contains(err.Error(), "table jobs") {
		t.Fatalf("a table: %v", err)
	}
	if value(`SELECT COUNT(*) FROM jobs`) != "1" {
		t.Fatal("the table went anyway")
	}
	ok(parents+kids, true, "removed email")

	// Rows that don't fit the new design leave everything as it was.
	const strict = `CREATE TABLE clients (id INTEGER PRIMARY KEY, name TEXT NOT NULL, phone TEXT NOT NULL);`
	if _, err := run(strict+kids, false); err == nil {
		t.Fatal("took a required column the rows don't have")
	}
	if value(`SELECT COUNT(*) FROM clients`) != "1" {
		t.Fatal("not left as it was")
	}
	// A row referring to nothing leaves everything as it was too.
	if _, err := run(parents+kids+`INSERT INTO jobs VALUES (2, 'x', 99);`, false); err == nil {
		t.Fatal("took a broken reference")
	}
	if value(`SELECT COUNT(*) FROM jobs`) != "1" {
		t.Fatal("the row stayed")
	}
	// An index comes and goes with the file.
	ok(parents+kids+`CREATE INDEX jobs_title ON jobs (title);`, false, "already matches")
	if value(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'jobs_title'`) != "1" {
		t.Fatal("no index")
	}
	if _, err := os.Stat(path + ".import-tmp"); err == nil {
		t.Fatal("left its working file")
	}
}

func TestNumberedRowsMoveToRandomIds(t *testing.T) {
	dir := t.TempDir()
	path, file := filepath.Join(dir, "d.db"), filepath.Join(dir, "d.sql")
	run := func(stmts string) string {
		t.Helper()
		os.WriteFile(file, []byte(stmts), 0o644)
		var out bytes.Buffer
		if err := importDesign(path, file, false, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	out := run(`CREATE TABLE clients (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE jobs (id INTEGER PRIMARY KEY, title TEXT, client_id INTEGER REFERENCES clients(id));
		INSERT INTO clients VALUES (1, 'Acme'), (2, 'Zed');
		INSERT INTO jobs VALUES (7, 'Site', 2);`)
	if !strings.Contains(out, "numbered rows in clients, jobs") {
		t.Fatalf("no note: %s", out)
	}
	// Only half done is said too.
	out = run(`CREATE TABLE clients (id TEXT PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE jobs (id INTEGER PRIMARY KEY, title TEXT, client_id INTEGER REFERENCES clients(id));`)
	if !strings.Contains(out, "jobs.client_id is INTEGER but clients.id is TEXT") {
		t.Fatalf("no note: %s", out)
	}
	out = run(`CREATE TABLE clients (id TEXT PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE jobs (id TEXT PRIMARY KEY, title TEXT, client_id TEXT REFERENCES clients(id));`)
	if strings.Contains(out, "Note:") {
		t.Fatalf("a note left: %s", out)
	}
	d, err := openData(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	var name, kind string
	err = d.db.QueryRow(`SELECT c.name, typeof(j.client_id) FROM jobs j JOIN clients c ON c.id = j.client_id WHERE j.id = '7'`).Scan(&name, &kind)
	if err != nil || name != "Zed" || kind != "text" {
		t.Fatalf("the link: %q %q %v", name, kind, err)
	}
}
