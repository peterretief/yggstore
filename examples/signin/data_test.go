package main

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

func TestData(t *testing.T) {
	t.Run("plain", func(t *testing.T) { testData(t, false) })
	t.Run("with a log", func(t *testing.T) { testData(t, true) })
}

func testData(t *testing.T, logged bool) {
	dir := t.TempDir()
	if err := importDesign(filepath.Join(dir, "d.db"), "design.sql", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := openData(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.db.Close() })
	if logged {
		if d.sync, err = openSync(d, "desktop"); err != nil {
			t.Fatal(err)
		}
	}
	db, _ := sql.Open("sqlite3", "file:"+filepath.Join(dir, "s.db"))
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL)`)
	db.Exec(`INSERT INTO sessions VALUES (?, 'a@example.org', ?)`, hashToken("t"), time.Now().Add(time.Hour).Unix())
	h := (&app{db: db, data: d, tries: map[string][]time.Time{}}).routes()
	do := func(method, path string, form url.Values, in bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if in {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "t"})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	get := func(path string) string {
		t.Helper()
		w := do("GET", path, nil, true)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	saved := func(path string, f url.Values) {
		t.Helper()
		if w := do("POST", path, f, true); w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}

	// Nothing is here for anyone not signed in.
	for _, p := range []string{dataBase, dataBase + "clients/", dataBase + "clients/new"} {
		if w := do("GET", p, nil, false); w.Code != http.StatusSeeOther {
			t.Fatalf("%s signed out: %d", p, w.Code)
		}
	}
	if w := do("POST", dataBase+"clients/new", url.Values{"name": {"x"}}, false); w.Code != http.StatusSeeOther {
		t.Fatalf("saving signed out: %d", w.Code)
	}

	// The diagram shows the tables and a line for each relationship.
	page := get(dataBase)
	if strings.Count(page, "<path") != 2 || !strings.Contains(page, "client_id") {
		t.Fatalf("the diagram: %s", page)
	}
	// Each form follows the design.
	form := get(dataBase + "projects/new")
	for _, want := range []string{`<select name="client_id" required`, `<option value="on hold"`, `type="date"`} {
		if !strings.Contains(form, want) {
			t.Fatalf("no %s in %s", want, form)
		}
	}
	if !strings.Contains(get(dataBase+"clients/new"), "<textarea") || !strings.Contains(get(dataBase+"invoices/new"), `type="checkbox"`) {
		t.Fatal("a text box or tick box is missing")
	}
	if strings.Contains(form, `name="id"`) {
		t.Fatal("the form asks for the number SQLite gives")
	}

	saved(dataBase+"clients/new", url.Values{"name": {"<b>Acme</b>"}, "email": {"a@example.org"}})
	saved(dataBase+"clients/new", url.Values{"name": {"Zed"}})
	id := func(table, where string) string {
		t.Helper()
		var s string
		if err := d.db.QueryRow(`SELECT id FROM ` + table + ` WHERE ` + where).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	acme, zed := id("clients", `name LIKE '%Acme%'`), id("clients", `name = 'Zed'`)
	if acme == zed || len(acme) < 20 || !strings.HasPrefix(acme, "r") {
		t.Fatalf("the ids: %q %q", acme, zed)
	}
	if !strings.Contains(get(dataBase+"projects/new"), `&lt;b&gt;Acme&lt;/b&gt;</option>`) {
		t.Fatal("the client isn't offered, escaped")
	}
	saved(dataBase+"projects/new", url.Values{"title": {"Site"}, "client_id": {acme}, "status": {"open"}})
	site := id("projects", `title = 'Site'`)
	saved(dataBase+"invoices/new", url.Values{"number": {"1"}, "project_id": {site}, "amount": {"100.50"}, "paid": {"1"}})
	saved(dataBase+"invoices/new", url.Values{"number": {"=2"}, "project_id": {site}, "amount": {"50"}})

	// A date comes back as the date box wants it, and survives saving.
	saved(dataBase+"projects/"+site, url.Values{"title": {"Site"}, "client_id": {acme}, "status": {"open"}, "started": {"2026-10-01"}})
	if p := get(dataBase + "projects/" + site); !strings.Contains(p, `value="2026-10-01"`) {
		t.Fatalf("the date: %s", p)
	}
	saved(dataBase+"projects/"+site, url.Values{"title": {"Site 2"}, "client_id": {acme}, "status": {"open"}, "started": {"2026-10-01"}})
	if p := get(dataBase + "projects/"); !strings.Contains(p, "<td>2026-10-01</td>") {
		t.Fatalf("the date in the report: %s", p)
	}
	saved(dataBase+"projects/"+site, url.Values{"title": {"Site"}, "client_id": {acme}, "status": {"open"}})

	// What the design refuses comes back on the form, with what was typed.
	w := do("POST", dataBase+"invoices/new", url.Values{"number": {"1"}, "project_id": {site}, "amount": {"1"}}, true)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "already a record") {
		t.Fatalf("a repeat: %d %s", w.Code, w.Body)
	}
	if w := do("POST", dataBase+"projects/new", url.Values{"title": {"x"}, "client_id": {"99"}, "status": {"open"}}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("a client that isn't there: %d", w.Code)
	}
	if w := do("POST", dataBase+"projects/new", url.Values{"title": {"x"}, "client_id": {acme}, "status": {"nonsense"}}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("a status that isn't allowed: %d", w.Code)
	}

	// Reports name what a row refers to, add up and group.
	rep := get(dataBase + "invoices/")
	if !strings.Contains(rep, ">Site</a>") || !strings.Contains(rep, "150.50") || !strings.Contains(rep, "Yes") {
		t.Fatalf("the report: %s", rep)
	}
	if g := get(dataBase + "invoices/?by=paid"); !strings.Contains(g, "100.50") || !strings.Contains(g, ">No<") {
		t.Fatalf("grouped: %s", g)
	}
	if s := get(dataBase + "clients/?q=zed"); !strings.Contains(s, "Zed") || strings.Contains(s, "Acme") {
		t.Fatalf("a search: %s", s)
	}
	if s := get(dataBase + "projects/?f=client_id&v=" + zed); strings.Contains(s, ">Site<") {
		t.Fatalf("a filter: %s", s)
	}
	if s := get(dataBase + "clients/?sort=" + url.QueryEscape(`name"; DROP TABLE clients;--`) + "&by=nope&f=nope"); !strings.Contains(s, "Zed") {
		t.Fatalf("a bad sort: %s", s)
	}
	if strings.Contains(rep, site) && !strings.Contains(rep, "/"+site) {
		t.Fatal("the report shows a random id as text")
	}
	if strings.Contains(get(dataBase+"clients/"), ">Id<") {
		t.Fatal("the report has an Id column")
	}
	if c := get(dataBase + "invoices/?csv=1"); !strings.Contains(c, "'=2") || !strings.Contains(c, "Project,Amount") {
		t.Fatalf("the csv: %s", c)
	}
	if do("GET", dataBase+"sqlite_master/", nil, true).Code != http.StatusNotFound {
		t.Fatal("a table that isn't in the design")
	}

	// A record shows what refers to it, and can't go while anything does.
	if p := get(dataBase + "clients/" + acme); !strings.Contains(p, "Projects") || !strings.Contains(p, "new?client_id="+acme) {
		t.Fatalf("the record: %s", p)
	}
	if w := do("POST", dataBase+"clients/"+acme+"/delete", nil, true); w.Code != http.StatusConflict {
		t.Fatalf("deleting with projects: %d", w.Code)
	}
	saved(dataBase+"clients/"+zed, url.Values{"name": {"Zed Ltd"}})
	saved(dataBase+"clients/"+zed+"/delete", nil)
	if s := get(dataBase + "clients/"); strings.Contains(s, "Zed") {
		t.Fatalf("after deleting: %s", s)
	}
}
