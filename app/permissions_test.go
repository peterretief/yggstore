package app

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRulesAreRead(t *testing.T) {
	tb := table{Name: "t", Cols: []column{{Name: "id"}, {Name: "budget"}, {Name: "notes"}}}
	readRules(&tb, `CREATE TABLE t (
  -- allow: staff read, Staff Write, client read own
  -- allow: client write own, staff fly
  id TEXT PRIMARY KEY,
  budget REAL,   -- hide from: client, staff
  notes TEXT     -- label: Our notes; help: Only for us
)`)
	if fmt.Sprint(tb.Rules) != "[{staff read false  } {staff write false  } {client read true login }]" {
		t.Fatalf("%v", tb.Rules)
	}
	if len(tb.Problems) != 2 || !strings.Contains(tb.Problems[0], "only read") || !strings.Contains(tb.Problems[1], "staff fly") {
		t.Fatalf("%q", tb.Problems)
	}
	b, n := tb.col("budget"), tb.col("notes")
	if !b.Hide["client"] || !b.Hide["staff"] || n.title() != "Our notes" || n.Help != "Only for us" {
		t.Fatalf("%+v %+v", b, n)
	}
	staff := tb.access(person{Name: "s@example.org", Role: roleStaff})
	client := tb.access(person{Name: "c@example.org", Role: roleClient})
	nobody := tb.access(person{})
	if !staff.Read || !staff.Write || staff.Delete || !client.Read || client.Write || nobody.Read {
		t.Fatalf("%+v %+v %+v", staff, client, nobody)
	}
	if tb.Rules[2].Own != true || tb.Rules[0].Own {
		t.Fatalf("own: %+v", tb.Rules)
	}
	// "if" names a column of the table, and only reads can be limited.
	pub := table{Name: "p", Cols: []column{{Name: "id"}, {Name: "published"}}}
	readRules(&pub, "CREATE TABLE p (\n -- allow: client read if published, client read if nothing, staff write if published\n id TEXT\n)")
	if len(pub.Rules) != 1 || pub.Rules[0].If != "published" || len(pub.Problems) != 2 {
		t.Fatalf("%+v %q", pub.Rules, pub.Problems)
	}
	// Without rules, a table is for admins only.
	bare := table{Name: "b"}
	if bare.access(person{Name: "s@example.org", Role: roleStaff}).Read || !bare.access(person{Name: "a", Admin: true}).Delete {
		t.Fatal("a table without rules")
	}
}

// The design the leak test runs on: every kind of rule.
const leakDesign = `
CREATE TABLE clients (
  -- allow: staff read, staff write
  -- allow: client read own
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  login TEXT UNIQUE,
  notes TEXT,    -- hide from: client
  rating TEXT    -- hide from: client, staff
);
CREATE TABLE jobs (
  -- allow: staff read, staff delete
  -- allow: client read own
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  client_id TEXT NOT NULL REFERENCES clients(id),
  margin TEXT    -- hide from: client, staff
);
CREATE TABLE salaries (
  id TEXT PRIMARY KEY,
  who TEXT NOT NULL
);`

// leakSite is a site where every secret a role mustn't see says so: a
// client may see only ANN-, staff only ANN- and STAFF-.
func leakSite(t *testing.T) *site {
	dir := t.TempDir()
	path, file := filepath.Join(dir, "d.db"), filepath.Join(dir, "d.sql")
	os.WriteFile(file, []byte(leakDesign), 0o644)
	if err := importDesign(path, file, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, _ := openData(path)
	t.Cleanup(func() { d.db.Close() })
	for _, q := range []string{
		`INSERT INTO clients VALUES ('ca', 'ANN-co', 'ann@example.org', 'STAFF-notes-on-ann', 'SECRET-rating-ann')`,
		`INSERT INTO clients VALUES ('cb', 'SECRET-bob-co', 'bob@example.org', 'SECRET-notes-on-bob', 'SECRET-rating-bob')`,
		`INSERT INTO jobs VALUES ('ja', 'ANN-job', 'ca', 'SECRET-margin-ann')`,
		`INSERT INTO jobs VALUES ('jb', 'SECRET-bob-job', 'cb', 'SECRET-margin-bob')`,
		`INSERT INTO salaries VALUES ('s1', 'SECRET-salary')`,
	} {
		if _, err := d.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db, _ := sql.Open("sqlite3", "file:"+filepath.Join(dir, "s.db"))
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL)`)
	db.Exec(`CREATE TABLE accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'admin')`)
	for token, who := range map[string][2]string{"boss": {"boss@example.org", "admin"}, "ann": {"ann@example.org", "client"},
		"sam": {"sam@example.org", "staff"}, "bob": {"bob@example.org", "client"}} {
		db.Exec(`INSERT INTO accounts VALUES (?, 'x', ?)`, who[0], who[1])
		db.Exec(`INSERT INTO sessions VALUES (?, ?, ?)`, hashToken(token), who[0], time.Now().Add(time.Hour).Unix())
	}
	a := &app{db: db, data: d, tries: map[string][]time.Time{}}
	var err error
	if a.docs, err = openDocs(d.db, filepath.Join(dir, "documents")); err != nil {
		t.Fatal(err)
	}
	s := &site{t: t, a: a, h: a.routes(), db: db}
	s.upload("boss", "", map[string]string{"SECRET-doc.txt": "SECRET-in-a-document"})
	return s
}

// TestNothingLeaks is the test every app gets: it walks every page an admin
// can reach, then asks for each, and sends each form, as everyone else.
func TestNothingLeaks(t *testing.T) {
	s := leakSite(t)
	ts := &TestSite{t: t, App: s.a, h: s.h}
	// Staff may write clients and delete jobs; nothing else may change.
	staffMay := func(as, action string, _ url.Values) bool {
		return as == "sam" && (strings.HasPrefix(action, dataBase+"clients/") || strings.HasPrefix(action, dataBase+"jobs/") && strings.HasSuffix(action, "/delete"))
	}
	pages, posts := ts.NothingLeaks("boss", regexp.MustCompile(`(ANN|STAFF|SECRET)-[a-z-]+`), map[string]*regexp.Regexp{
		"ann": regexp.MustCompile(`ANN-`),
		"sam": regexp.MustCompile(`ANN-|STAFF-|SECRET-bob-co|SECRET-notes-on-bob|SECRET-bob-job`),
	}, staffMay)
	if pages < 15 || posts < 10 {
		t.Fatalf("the walk found too little: %d pages, %d forms", pages, posts)
	}
	// And what staff may do, they can.
	if w := s.post("sam", dataBase+"clients/ca", url.Values{"name": {"ANN-co renamed"}, "login": {"ann@example.org"}, "notes": {"STAFF-notes-on-ann"}, "rating": {"forged"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("staff writing: %d %s", w.Code, w.Body)
	}
	if n := value(t, s.a.data, `SELECT name || '/' || rating FROM clients WHERE id = 'ca'`); n != "ANN-co renamed/SECRET-rating-ann" {
		t.Fatalf("a column staff can't see was changed by their save: %s", n)
	}
	if w := s.post("sam", dataBase+"jobs/jb/delete", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("staff deleting: %d", w.Code)
	}
	if w := s.post("sam", dataBase+"jobs/new", url.Values{"title": {"x"}, "client_id": {"ca"}}); w.Code != http.StatusForbidden {
		t.Fatalf("staff adding a job they may only read and delete: %d", w.Code)
	}
}

func TestStaffOnThePeoplePage(t *testing.T) {
	s := newSite(t)
	if !strings.Contains(s.get("boss", base+"people"), `value="staff"`) {
		t.Fatal("no staff to choose")
	}
	s.post("boss", base+"people", url.Values{"do": {"add"}, "email": {"sue@example.org"}, "role": {"staff"}})
	s.post("boss", base+"people", url.Values{"do": {"role"}, "email": {"ann@example.org"}, "role": {"staff"}})
	s.post("boss", base+"people", url.Values{"do": {"role"}, "email": {"bob@example.org"}, "role": {"owner"}})
	if s.a.accountRole("sue@example.org") != roleStaff || s.a.accountRole("ann@example.org") != roleStaff || s.a.accountRole("bob@example.org") != roleClient {
		t.Fatal("roles")
	}
	// Staff aren't admins: no People page.
	if s.do("ann", "GET", base+"people", nil, "").Code != http.StatusForbidden {
		t.Fatal("staff on the people page")
	}
}
