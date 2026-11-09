package app

import (
	"bytes"
	"database/sql"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type site struct {
	t  *testing.T
	a  *app
	h  http.Handler
	db *sql.DB
}

// newSite is the program with a data file, documents, and three people:
// boss (an admin), ann and bob (clients), signed in with tokens of their names.
func newSite(t *testing.T) *site {
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
	db, _ := sql.Open("sqlite3", "file:"+filepath.Join(dir, "s.db"))
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL)`)
	db.Exec(`CREATE TABLE accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'admin')`)
	for token, who := range map[string][2]string{"boss": {"boss@example.org", "admin"}, "ann": {"ann@example.org", "client"}, "bob": {"bob@example.org", "client"}} {
		db.Exec(`INSERT INTO accounts VALUES (?, ?, ?)`, who[0], hashPassword("old-password-1"), who[1])
		db.Exec(`INSERT INTO sessions VALUES (?, ?, ?)`, hashToken(token), who[0], time.Now().Add(time.Hour).Unix())
	}
	a := &app{db: db, data: d, tries: map[string][]time.Time{}}
	if a.docs, err = openDocs(d.db, filepath.Join(dir, "documents")); err != nil {
		t.Fatal(err)
	}
	return &site{t: t, a: a, h: a.routes(), db: db}
}

func (s *site) do(as, method, path string, body io.Reader, ctype string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	if as != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: as})
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func (s *site) post(as, path string, f url.Values) *httptest.ResponseRecorder {
	return s.do(as, "POST", path, strings.NewReader(f.Encode()), "application/x-www-form-urlencoded")
}

func (s *site) get(as, path string) string {
	s.t.Helper()
	w := s.do(as, "GET", path, nil, "")
	if w.Code != 200 {
		s.t.Fatalf("%s %s: %d %s", as, path, w.Code, w.Body)
	}
	return w.Body.String()
}

// upload sends files, as the page's script does: path is where each was in
// the folder chosen.
func (s *site) upload(as, parent string, files map[string]string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	m.WriteField("parent", parent)
	for path, body := range files {
		fw, _ := m.CreateFormFile("file", filepath.Base(path))
		io.WriteString(fw, body)
		m.WriteField("path", path)
	}
	m.Close()
	r := httptest.NewRequest("POST", docsBase+"upload", &b)
	r.Header.Set("Content-Type", m.FormDataContentType())
	r.Header.Set("X-Upload", "1")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: as})
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func (s *site) id(name string) string {
	s.t.Helper()
	var id string
	if err := s.a.docs.db.QueryRow(`SELECT id FROM _docs WHERE name = ?`, name).Scan(&id); err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	return id
}

func TestDocuments(t *testing.T) {
	s := newSite(t)
	hidden := func(page string, words ...string) {
		t.Helper()
		for _, w := range words {
			if strings.Contains(page, w) {
				t.Fatalf("%q showed: %s", w, page)
			}
		}
	}

	// A whole folder, with a folder inside it, one file at a time.
	if w := s.upload("boss", "", map[string]string{"Contracts/ann.pdf": "ANN", "Contracts/old/bob.pdf": "BOB"}); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s.upload("boss", "", map[string]string{"staff-only.txt": "STAFF"})
	contracts, old := s.id("Contracts"), s.id("old")
	if !strings.Contains(s.get("boss", docsBase+"f/"+contracts), "ann.pdf") || !strings.Contains(s.get("boss", docsBase+"f/"+old), "bob.pdf") {
		t.Fatal("the folders")
	}
	// A file of the same name is replaced, and keeps its id.
	ann := s.id("ann.pdf")
	s.upload("boss", contracts, map[string]string{"ann.pdf": "ANN v2"})
	if s.id("ann.pdf") != ann || s.get("boss", docsBase+"d/"+ann) != "ANN v2" {
		t.Fatal("replacing a file")
	}

	// Nothing is shared yet: a client sees nothing, however they ask.
	hidden(s.get("ann", docsBase), "Contracts", "staff-only", "Upload")
	for _, p := range []string{"f/" + contracts, "d/" + ann, "d/" + s.id("staff-only.txt")} {
		if w := s.do("ann", "GET", docsBase+p, nil, ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}

	// Share the file with ann, and the folder "old" with bob.
	if w := s.post("boss", docsBase+"share/"+ann, url.Values{"name": {"ann@example.org", "nobody@example.org"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("%d", w.Code)
	}
	s.post("boss", docsBase+"share/"+old, url.Values{"name": {"bob@example.org"}})
	var n int
	s.a.docs.db.QueryRow(`SELECT COUNT(*) FROM _docs_access`).Scan(&n)
	if n != 2 {
		t.Fatalf("a share for someone with no account was kept: %d", n)
	}

	page := s.get("ann", docsBase)
	if !strings.Contains(page, "ann.pdf") {
		t.Fatal(page)
	}
	hidden(page, "Contracts", "bob.pdf", ">old<", "staff-only", ">Share<", ">Delete<")
	w := s.do("ann", "GET", docsBase+"d/"+ann, nil, "")
	if w.Body.String() != "ANN v2" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") ||
		w.Header().Get("Content-Security-Policy") != "sandbox" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("the download: %v %q", w.Header(), w.Body)
	}
	for _, p := range []string{"d/" + s.id("bob.pdf"), "f/" + old, "f/" + contracts} {
		if w := s.do("ann", "GET", docsBase+p, nil, ""); w.Code != http.StatusNotFound {
			t.Fatalf("ann reached %s: %d", p, w.Code)
		}
	}
	// A shared folder brings what's in it, and only that.
	page = s.get("bob", docsBase)
	if !strings.Contains(page, ">old<") {
		t.Fatal(page)
	}
	hidden(page, "ann.pdf", "Contracts")
	if !strings.Contains(s.get("bob", docsBase+"f/"+old), "bob.pdf") || s.get("bob", docsBase+"d/"+s.id("bob.pdf")) != "BOB" {
		t.Fatal("inside the shared folder")
	}
	hidden(s.get("bob", docsBase+"f/"+old), "Contracts") // the way back up stops where sharing does

	// Clients change nothing.
	for _, p := range []string{"upload", "folder", "delete", "share/" + ann} {
		if w := s.post("ann", docsBase+p, url.Values{"id": {ann}, "name": {"x"}}); w.Code != http.StatusForbidden {
			t.Fatalf("POST %s as a client: %d", p, w.Code)
		}
	}
	if w := s.upload("ann", "", map[string]string{"x.txt": "x"}); w.Code != http.StatusForbidden {
		t.Fatalf("upload as a client: %d", w.Code)
	}
	if w := s.do("", "GET", docsBase+"d/"+ann, nil, ""); w.Code != http.StatusSeeOther {
		t.Fatalf("signed out: %d", w.Code)
	}

	// Names that could climb out or hide are refused.
	for _, bad := range []string{"../evil.txt", "a/../../evil.txt", "..", "a\\b.txt", "nul\x00.txt"} {
		if w := s.upload("boss", "", map[string]string{bad: "x"}); w.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d", bad, w.Code)
		}
	}
	entries, _ := os.ReadDir(s.a.docs.dir)
	for _, e := range entries {
		if !regexp.MustCompile(`^r[a-z0-9]{26}$`).MatchString(e.Name()) {
			t.Fatalf("a file kept under its own name: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.a.docs.dir), "evil.txt")); err == nil {
		t.Fatal("climbed out")
	}

	// Deleting a folder takes what's in it, from the disk too, and its sharing.
	bob := s.id("bob.pdf")
	if w := s.post("boss", docsBase+"delete", url.Values{"id": {old}}); w.Code != http.StatusSeeOther {
		t.Fatalf("%d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(s.a.docs.dir, bob)); err == nil {
		t.Fatal("the file stayed on the disk")
	}
	s.a.docs.db.QueryRow(`SELECT COUNT(*) FROM _docs_access WHERE doc = ?`, old).Scan(&n)
	if n != 0 || s.do("bob", "GET", docsBase+"d/"+bob, nil, "").Code != http.StatusNotFound {
		t.Fatal("still shared")
	}
}

func TestPeople(t *testing.T) {
	s := newSite(t)
	if w := s.do("ann", "GET", base+"people", nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("a client on the people page: %d", w.Code)
	}
	w := s.post("boss", base+"people", url.Values{"do": {"add"}, "email": {" New@Example.org "}, "role": {"client"}})
	m := regexp.MustCompile(`<code>([a-z0-9-]+)</code>`).FindStringSubmatch(w.Body.String())
	if w.Code != 200 || m == nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !matches(s.a.accountHash("new@example.org"), m[1]) || s.a.accountRole("new@example.org") != roleClient {
		t.Fatal("the new account")
	}
	if len(m[1]) < 10 {
		t.Fatal("the temporary password is too short to sign in with")
	}
	if w := s.post("boss", base+"people", url.Values{"do": {"add"}, "email": {"new@example.org"}}); w.Code != http.StatusBadRequest {
		t.Fatal("added twice")
	}
	if w := s.post("boss", base+"people", url.Values{"do": {"add"}, "email": {"not an email"}}); w.Code != http.StatusBadRequest {
		t.Fatal("took a non-email")
	}

	// A reset gives a new password and signs them out.
	w = s.post("boss", base+"people", url.Values{"do": {"reset"}, "email": {"ann@example.org"}})
	if pw := regexp.MustCompile(`<code>([a-z0-9-]+)</code>`).FindStringSubmatch(w.Body.String()); pw == nil || !matches(s.a.accountHash("ann@example.org"), pw[1]) {
		t.Fatal("the reset")
	}
	if s.do("ann", "GET", base+"me", nil, "").Code != http.StatusUnauthorized {
		t.Fatal("still signed in after a reset")
	}

	// An admin can't lock themselves out.
	for _, do := range []string{"remove", "role"} {
		if w := s.post("boss", base+"people", url.Values{"do": {do}, "email": {"boss@example.org"}}); w.Code != http.StatusBadRequest {
			t.Fatalf("%s themselves: %d", do, w.Code)
		}
	}
	s.post("boss", base+"people", url.Values{"do": {"role"}, "email": {"bob@example.org"}, "role": {"admin"}})
	if s.a.accountRole("bob@example.org") != roleAdmin {
		t.Fatal("the role")
	}

	// Removing someone ends their sessions and what was shared with them.
	s.upload("boss", "", map[string]string{"a.txt": "A"})
	s.post("boss", docsBase+"share/"+s.id("a.txt"), url.Values{"name": {"new@example.org"}})
	s.post("boss", base+"people", url.Values{"do": {"remove"}, "email": {"new@example.org"}})
	var n int
	s.a.docs.db.QueryRow(`SELECT COUNT(*) FROM _docs_access`).Scan(&n)
	if s.a.accountRole("new@example.org") != "" || n != 0 {
		t.Fatal("the removal")
	}
}

func TestChangingYourPassword(t *testing.T) {
	s := newSite(t)
	try := func(now, next, again string) int {
		return s.post("ann", base+"password", url.Values{"current": {now}, "new": {next}, "again": {again}}).Code
	}
	if try("wrong", "new-password-1", "new-password-1") != http.StatusBadRequest ||
		try("old-password-1", "short", "short") != http.StatusBadRequest ||
		try("old-password-1", "new-password-1", "new-password-2") != http.StatusBadRequest {
		t.Fatal("took a bad change")
	}
	if try("old-password-1", "new-password-1", "new-password-1") != 200 || !matches(s.a.accountHash("ann@example.org"), "new-password-1") {
		t.Fatal("the change")
	}
}

func TestPeopleWithALog(t *testing.T) {
	s := newSite(t)
	var err error
	if s.a.data.sync, err = openSync(s.a.data, "desktop"); err != nil {
		t.Fatal(err)
	}
	s.a.data.sync.adoptAccounts(s.db)
	s.post("boss", base+"people", url.Values{"do": {"add"}, "email": {"logged@example.org"}})
	if s.a.accountRole("logged@example.org") != roleClient {
		t.Fatal("added")
	}
	s.post("boss", base+"people", url.Values{"do": {"remove"}, "email": {"logged@example.org"}})
	if s.a.accountRole("logged@example.org") != "" {
		t.Fatal("removed")
	}
	if err := s.a.data.sync.rebuild(); err != nil {
		t.Fatal(err)
	}
	if s.a.accountRole("logged@example.org") != "" || s.a.accountRole("ann@example.org") != roleClient {
		t.Fatal("after a replay")
	}
}

func TestDocumentsGoIntoTheCopyOneByOne(t *testing.T) {
	s := newSite(t)
	for len(copySoon) > 0 {
		<-copySoon
	}
	s.upload("boss", "", map[string]string{"x/y.txt": "Y", "z.txt": "Z"})
	if len(copySoon) != 1 {
		t.Fatal("an upload didn't ask for a copy")
	}
	out := t.TempDir()
	copied := func() []string {
		entries, _ := os.ReadDir(filepath.Join(out, "documents"))
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	if err := copyAll(nil, []string{s.a.docs.dir}, out); err != nil {
		t.Fatal(err)
	}
	y, z := s.id("y.txt"), s.id("z.txt")
	if got := copied(); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "documents", y)); string(b) != "Y" {
		t.Fatal("the copy")
	}
	// The outbox stores each and leaves a stub; unchanged files aren't sent again.
	for _, id := range []string{y, z} {
		os.Rename(filepath.Join(out, "documents", id), filepath.Join(out, "documents", id+".ystub"))
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(out, "documents", y+".ystub"), later, later)
	os.Chtimes(filepath.Join(out, "documents", z+".ystub"), later, later)
	if err := copyAll(nil, []string{s.a.docs.dir}, out); err != nil {
		t.Fatal(err)
	}
	if got := copied(); len(got) != 2 {
		t.Fatalf("sent again: %v", got)
	}
	// A replaced file goes again, as a new version.
	s.upload("boss", s.id("x"), map[string]string{"y.txt": "Y2"})
	later = later.Add(time.Minute)
	os.Chtimes(filepath.Join(s.a.docs.dir, y), later, later)
	if err := copyAll(nil, []string{s.a.docs.dir}, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "documents", y)); string(b) != "Y2" {
		t.Fatal("the new version wasn't copied")
	}
	if _, err := os.Stat(filepath.Join(out, "documents", z)); err == nil {
		t.Fatal("an unchanged file was copied again")
	}
}

func TestGroups(t *testing.T) {
	s := newSite(t)
	gid := func(name string) string {
		t.Helper()
		var id string
		if err := s.a.docs.db.QueryRow(`SELECT id FROM _groups WHERE name = ?`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	group := func(do string, f url.Values) int {
		f.Set("do", do)
		return s.post("boss", base+"groups", f).Code
	}
	sees := func(as, id string) bool { return s.do(as, "GET", docsBase+"d/"+id, nil, "").Code == 200 }

	if s.post("ann", base+"groups", url.Values{"do": {"new"}, "name": {"x"}}).Code != http.StatusForbidden {
		t.Fatal("a client made a group")
	}
	group("new", url.Values{"name": {"  Acme   staff "}})
	if group("new", url.Values{"name": {"Acme staff"}}) != http.StatusBadRequest {
		t.Fatal("two groups of one name")
	}
	acme := gid("Acme staff")
	group("add", url.Values{"group": {acme}, "email": {"ann@example.org"}})
	if group("add", url.Values{"group": {acme}, "email": {"nobody@example.org"}}) != http.StatusBadRequest {
		t.Fatal("added someone with no account")
	}

	s.upload("boss", "", map[string]string{"Acme/plan.pdf": "PLAN", "other.pdf": "OTHER"})
	folder, plan := s.id("Acme"), s.id("plan.pdf")
	// Shared with the group, and with bob directly: both reach it.
	s.post("boss", docsBase+"share/"+folder, url.Values{"name": {groupKey + acme, "bob@example.org", groupKey + "made-up"}})
	if !sees("ann", plan) || !sees("bob", plan) || sees("ann", s.id("other.pdf")) {
		t.Fatal("who sees it")
	}
	var fake int
	s.a.docs.db.QueryRow(`SELECT COUNT(*) FROM _docs_access WHERE name = ?`, groupKey+"made-up").Scan(&fake)
	if fake != 0 {
		t.Fatal("a share with a group that isn't there was kept")
	}
	if p := s.get("boss", docsBase); !strings.Contains(p, "Acme staff (group)") || !strings.Contains(p, "bob@example.org") {
		t.Fatalf("the admin's list: %s", p)
	}
	if p := s.get("boss", docsBase+"share/"+folder); !strings.Contains(p, "Acme staff (ann@example.org)") {
		t.Fatalf("the share page: %s", p)
	}

	// Someone who joins later sees it at once; whoever leaves loses it at once.
	s.a.db.Exec(`INSERT INTO accounts VALUES ('cat@example.org', 'x', 'client')`)
	s.a.db.Exec(`INSERT INTO sessions VALUES (?, 'cat@example.org', ?)`, hashToken("cat"), time.Now().Add(time.Hour).Unix())
	if sees("cat", plan) {
		t.Fatal("cat before joining")
	}
	group("add", url.Values{"group": {acme}, "email": {"cat@example.org"}})
	if !sees("cat", plan) || !strings.Contains(s.get("cat", docsBase), ">Acme<") {
		t.Fatal("cat after joining")
	}
	group("remove", url.Values{"group": {acme}, "email": {"ann@example.org"}})
	if sees("ann", plan) || s.do("ann", "GET", docsBase+"f/"+folder, nil, "").Code != http.StatusNotFound {
		t.Fatal("ann after leaving")
	}

	// Removing an account takes it out of its groups.
	s.post("boss", base+"people", url.Values{"do": {"remove"}, "email": {"cat@example.org"}})
	var n int
	s.a.docs.db.QueryRow(`SELECT COUNT(*) FROM _group_members WHERE name = 'cat@example.org'`).Scan(&n)
	if n != 0 {
		t.Fatal("still a member")
	}

	// Deleting a group ends its shares; the documents and the people stay.
	group("add", url.Values{"group": {acme}, "email": {"ann@example.org"}})
	group("delete", url.Values{"group": {acme}})
	s.a.docs.db.QueryRow(`SELECT COUNT(*) FROM _docs_access WHERE name LIKE 'group:%'`).Scan(&n)
	if n != 0 || sees("ann", plan) || !sees("bob", plan) || s.a.accountRole("ann@example.org") == "" {
		t.Fatal("deleting the group")
	}

	// No account can pass itself off as a group.
	if s.post("boss", base+"people", url.Values{"do": {"add"}, "email": {groupKey + "x@example.org"}}).Code != http.StatusBadRequest {
		t.Fatal("an account named like a group")
	}
	if !strings.Contains(s.get("boss", base+"people"), "id=\"groups\"") {
		t.Fatal("the people page has no groups")
	}
}
