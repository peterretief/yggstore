package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// drawDB's SQLite export, as it writes it: a table's comment above it, a
// field's comment on the line above the field, names quoted.
const drawDB = `/* allow: staff read, staff write
allow: client read own */
CREATE TABLE IF NOT EXISTS "students" (
	"id" TEXT NOT NULL,
	"name" TEXT NOT NULL,
	-- label: Email; help: As on People
	"login" TEXT,
	PRIMARY KEY("id")
);

/* Lessons each student has.
allow: staff read, staff write */
CREATE TABLE IF NOT EXISTS "lessons" (
	"id" TEXT NOT NULL,
	"student_id" TEXT NOT NULL,
	"topic" TEXT NOT NULL,
	-- hide from: client
	-- label: Notes for tutors
	"notes" TEXT,
	PRIMARY KEY("id"),
	FOREIGN KEY ("student_id") REFERENCES "students"("id")
	ON UPDATE NO ACTION ON DELETE NO ACTION
);
`

func TestDrawDBExport(t *testing.T) {
	dir := t.TempDir()
	file, path := filepath.Join(dir, "design.sql"), filepath.Join(dir, "data.db")
	os.WriteFile(file, []byte(drawDB), 0o644)
	if err := importDesign(path, file, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := openData(path)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := d.design()
	d.db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, l := find(tables, "students"), find(tables, "lessons")
	if s == nil || l == nil {
		t.Fatalf("tables: %+v", tables)
	}
	if len(s.Rules) != 3 || len(l.Rules) != 2 || len(s.Problems)+len(l.Problems) > 0 {
		t.Fatalf("rules: %+v %+v, problems %v %v", s.Rules, l.Rules, s.Problems, l.Problems)
	}
	if c := s.col("login"); c.Label != "Email" || c.Help != "As on People" {
		t.Fatalf("login: %+v", c)
	}
	if c := l.col("notes"); !c.Hide[roleClient] || c.Label != "Notes for tutors" {
		t.Fatalf("notes: %+v", c)
	}
	if c := l.col("topic"); c.Label != "" || c.Hide != nil {
		t.Fatalf("a note reached the wrong column: %+v", c)
	}
	// Exported again after a change in drawDB, over the same file: the
	// change goes in, and the rules are still there.
	os.WriteFile(file, []byte(strings.Replace(drawDB, `	"topic" TEXT NOT NULL,`, `	"topic" TEXT NOT NULL,
	"day" DATE,`, 1)), 0o644)
	if err := importDesign(path, file, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, _ = openData(path)
	defer d.db.Close()
	tables, _ = d.design()
	if l := find(tables, "lessons"); l.col("day") == nil || len(l.Rules) != 2 || !l.col("notes").Hide[roleClient] {
		t.Fatalf("after the change: %+v", l)
	}
}

func TestCommentsInside(t *testing.T) {
	// A comment elsewhere stays where it is, and isn't taken for a table's.
	in := "/* about the file */\nCREATE VIEW v AS SELECT 1;\n/* t's */ CREATE TABLE t (\n  id TEXT\n);"
	got := commentsInside(in)
	if !strings.HasPrefix(got, "/* about the file */\nCREATE VIEW") || !strings.Contains(got, "CREATE TABLE t (\n  -- t's\n  id TEXT") {
		t.Fatalf("%s", got)
	}
}
