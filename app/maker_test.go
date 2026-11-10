package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const makerDesign = `CREATE TABLE notes (
  -- allow: staff read own by made_by, staff write own by made_by
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  made_by TEXT -- fill: who made it
);
/* allow: staff read own by owner, staff write own by owner */
CREATE TABLE "jobs" (
	"id" TEXT NOT NULL,
	"title" TEXT NOT NULL,
	-- fill: who made it; hide from: staff
	"owner" TEXT,
	PRIMARY KEY("id")
);
`

// "fill: who made it" gives each person what they made.
func TestWhoMadeIt(t *testing.T) {
	file := t.TempDir() + "/design.sql"
	writeFile(t, file, makerDesign)
	if p := DesignProblems(t, file); len(p) > 0 {
		t.Fatal(p)
	}
	s := NewTestSite(t, file, map[string][2]string{"jo": {"jo@example.org", "staff"}, "sam": {"sam@example.org", "staff"}})
	value := func(q string) (v string) { s.DB().QueryRow(q).Scan(&v); return v }

	// What's sent for the column is ignored: it's whoever made the row.
	if w := s.Post("jo", DataBase+"notes/new", url.Values{"title": {"JO-note"}, "made_by": {"sam@example.org"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("jo's note: %d %s", w.Code, w.Body)
	}
	id := value(`SELECT id FROM notes`)
	if by := value(`SELECT made_by FROM notes`); by != "jo@example.org" {
		t.Fatalf("made by %q", by)
	}
	if f := s.Get("jo", DataBase+"notes/new"); strings.Contains(f, `name="made_by"`) {
		t.Fatal("the form asks for it")
	}
	if !strings.Contains(s.Get("jo", DataBase+"notes/"), "JO-note") || strings.Contains(s.Get("sam", DataBase+"notes/"), "JO-note") {
		t.Fatal("who sees jo's note")
	}
	// A change keeps it; nobody else may change the note.
	s.Post("jo", DataBase+"notes/"+id, url.Values{"title": {"JO-note again"}, "made_by": {"sam@example.org"}})
	if v := value(`SELECT title || '/' || made_by FROM notes`); v != "JO-note again/jo@example.org" {
		t.Fatalf("after a change: %s", v)
	}
	if w := s.Post("sam", DataBase+"notes/"+id, url.Values{"title": {"taken"}}); w.Code != http.StatusForbidden {
		t.Fatalf("sam changed jo's note: %d", w.Code)
	}

	// From drawDB's comment boxes, and filled though staff can't see it.
	if w := s.Post("sam", DataBase+"jobs/new", url.Values{"title": {"SAM-job"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("sam's job: %d %s", w.Code, w.Body)
	}
	if by := value(`SELECT owner FROM jobs`); by != "sam@example.org" {
		t.Fatalf("owner %q", by)
	}
	if l := s.Get("sam", DataBase+"jobs/"); !strings.Contains(l, "SAM-job") || strings.Contains(l, "sam@example.org</td>") {
		t.Fatalf("sam's jobs: %s", l)
	}
	if strings.Contains(s.Get("jo", DataBase+"jobs/"), "SAM-job") {
		t.Fatal("jo sees sam's job")
	}
}

func TestFillOnlyWhoMadeIt(t *testing.T) {
	file := t.TempDir() + "/design.sql"
	writeFile(t, file, "CREATE TABLE t (\n  id TEXT PRIMARY KEY,\n  x TEXT -- fill: today\n);")
	if p := DesignProblems(t, file); len(p) != 1 || !strings.Contains(p[0], "fill: who made it") {
		t.Fatalf("%v", p)
	}
}

// With the log on, the maker goes into the entry, so every copy agrees.
func TestWhoMadeItLogged(t *testing.T) {
	file := t.TempDir() + "/design.sql"
	writeFile(t, file, makerDesign)
	s := NewTestSite(t, file, map[string][2]string{"jo": {"jo@example.org", "staff"}})
	var err error
	if s.App.data.sync, err = openSync(s.App.data, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.App.data.sync.adoptAccounts(s.App.db); err != nil { // as the program does
		t.Fatal(err)
	}
	if w := s.Post("jo", DataBase+"notes/new", url.Values{"title": {"JO-note"}, "made_by": {"someone@example.org"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if v := value(t, s.App.data, `SELECT made_by FROM notes`); v != "jo@example.org" {
		t.Fatalf("made by %q", v)
	}
	if v := value(t, s.App.data, `SELECT fields FROM _sync_entries WHERE tbl = 'notes'`); !strings.Contains(v, `"made_by":"jo@example.org"`) {
		t.Fatalf("the entry: %s", v)
	}
}
