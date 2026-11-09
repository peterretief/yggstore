package app

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientsSeeOnlyTheirOwn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.db")
	if err := importDesign(path, "testdata/sample.sql", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := openData(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.db.Close() })
	for _, q := range []string{
		`CREATE TABLE notices (id TEXT PRIMARY KEY, text TEXT)`, // no way back to a login
		`INSERT INTO notices VALUES ('n1', 'For staff only')`,
		`INSERT INTO clients (id, name, login) VALUES ('c1', 'Ann Ltd', 'ann@example.org'), ('c2', 'Bob Ltd', 'Bob@Example.org')`,
		`INSERT INTO projects (id, title, client_id, status) VALUES ('p1', 'Ann job', 'c1', 'open'), ('p2', 'Bob job', 'c2', 'open')`,
		`INSERT INTO invoices (id, number, project_id, amount) VALUES ('i1', 'A-1', 'p1', 100), ('i2', 'B-1', 'p2', 900)`,
	} {
		if _, err := d.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db, _ := sql.Open("sqlite3", "file:"+filepath.Join(dir, "s.db"))
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL)`)
	db.Exec(`CREATE TABLE accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'admin')`)
	for token, who := range map[string][2]string{"ann": {"ann@example.org", "client"}, "bob": {"bob@example.org", "client"},
		"boss": {"boss@example.org", "admin"}, "nobody": {"nobody@example.org", "client"}} {
		db.Exec(`INSERT INTO accounts VALUES (?, 'x', ?)`, who[0], who[1])
		db.Exec(`INSERT INTO sessions VALUES (?, ?, ?)`, hashToken(token), who[0], time.Now().Add(time.Hour).Unix())
	}
	h := (&app{db: db, data: d, tries: map[string][]time.Time{}}).routes()
	do := func(as, method, path string, f url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if as != "" {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: as})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	see := func(as, path string) string {
		t.Helper()
		w := do(as, "GET", path, nil)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d", as, path, w.Code)
		}
		return w.Body.String()
	}
	hidden := func(s string, words ...string) {
		t.Helper()
		for _, x := range words {
			if strings.Contains(s, x) {
				t.Fatalf("%q showed: %s", x, s)
			}
		}
	}

	// Their own, down the foreign keys, with totals of their own.
	if s := see("ann", dataBase+"clients/"); !strings.Contains(s, "Ann Ltd") {
		t.Fatal(s)
	} else {
		hidden(s, "Bob", ">Add<")
	}
	hidden(see("ann", dataBase+"projects/"), "Bob job")
	s := see("ann", dataBase+"invoices/")
	hidden(s, "B-1", "900", "1000")
	if !strings.Contains(s, "A-1") || !strings.Contains(s, "100.00") {
		t.Fatal(s)
	}
	// However they ask.
	hidden(see("ann", dataBase+"invoices/?csv=1"), "B-1")
	hidden(see("ann", dataBase+"clients/?q=Bob"), "Bob Ltd")
	hidden(see("ann", dataBase+"projects/?f=client_id&v=c2"), "Bob")
	hidden(see("ann", dataBase+"projects/?by=client_id"), "Bob")
	hidden(see("ann", dataBase+"invoices/?by=project_id"), "Bob job")
	// A login in another case is the same person.
	if !strings.Contains(see("bob", dataBase+"clients/"), "Bob Ltd") {
		t.Fatal("Bob@Example.org should be bob@example.org")
	}

	// A record of theirs reads, with no form and nobody else's names.
	s = see("ann", dataBase+"projects/p1")
	hidden(s, "Bob", "<select", `<form class="edit"`, "Delete", ">Add<")
	if !strings.Contains(s, "Ann Ltd") {
		t.Fatal(s)
	}
	for _, p := range []string{"clients/c2", "projects/p2", "invoices/i2", "notices/", "notices/n1", "sqlite_master/"} {
		if w := do("ann", "GET", dataBase+p, nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}

	// A client changes nothing, not even their own.
	for _, p := range []string{"clients/new", "clients/c1", "clients/c1/delete", "clients/c2/delete"} {
		if w := do("ann", "POST", dataBase+p, url.Values{"name": {"x"}}); w.Code != http.StatusForbidden {
			t.Fatalf("POST %s: %d", p, w.Code)
		}
	}
	if w := do("ann", "GET", dataBase+"clients/new", nil); w.Code != http.StatusForbidden {
		t.Fatalf("the new form: %d", w.Code)
	}
	if n := value(t, d, `SELECT name FROM clients WHERE id = 'c1'`); n != "Ann Ltd" {
		t.Fatal(n)
	}

	// No diagram for clients: their tables, and nothing about the rest.
	s = see("ann", dataBase)
	hidden(s, "<svg", "notices", "Notices")
	if !strings.Contains(s, "Invoices") {
		t.Fatal(s)
	}
	// An account no row names sees nothing.
	if !strings.Contains(see("nobody", dataBase+"clients/"), "0 in all") {
		t.Fatal("rows for nobody")
	}
	// Signed out: the sign-in page, whatever's asked.
	for _, p := range []string{dataBase, dataBase + "clients/", dataBase + "clients/c1"} {
		if w := do("", "GET", p, nil); w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}

	// An admin sees it all, and a change of role counts at once.
	s = see("boss", dataBase+"invoices/")
	if !strings.Contains(s, "B-1") || !strings.Contains(s, "A-1") || !strings.Contains(s, ">Add<") {
		t.Fatal(s)
	}
	if !strings.Contains(see("boss", dataBase), "<svg") || !strings.Contains(see("boss", dataBase+"notices/"), "staff") {
		t.Fatal("the admin's view")
	}
	db.Exec(`UPDATE accounts SET role = 'client' WHERE name = 'boss@example.org'`)
	if w := do("boss", "POST", dataBase+"clients/new", url.Values{"name": {"x"}}); w.Code != http.StatusForbidden {
		t.Fatalf("after losing the role: %d", w.Code)
	}
}

func TestRolesTravel(t *testing.T) {
	a, b := machine(t, "desktop"), machine(t, "box")
	a.sync.write(entryAccount, "", "x@example.org", map[string]any{"hash": "h", "role": roleClient})
	a.sync.write(entryAccount, "", "old@example.org", map[string]any{"hash": "h"}) // from before roles
	meet(t, a, b)
	if value(t, b, `SELECT role FROM _sync_accounts WHERE name = 'x@example.org'`) != roleClient {
		t.Fatal("the role")
	}
	if value(t, b, `SELECT role FROM _sync_accounts WHERE name = 'old@example.org'`) != roleAdmin {
		t.Fatal("an account from before roles could see everything")
	}
}
