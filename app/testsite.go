package app

import (
	"database/sql"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSite is an app with its data and some people signed in, for an app's
// own tests: requests go straight to its pages, as people would send them.
type TestSite struct {
	t   testing.TB
	App *App
	h   http.Handler
}

// NewTestSite starts an app on a design file, with these people signed in:
// token (the name a test asks as) to their email and role.
func NewTestSite(t testing.TB, design string, people map[string][2]string) *TestSite {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	if err := importDesign(path, design, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := openData(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.db.Close() })
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "signin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL)`)
	db.Exec(`CREATE TABLE accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'admin')`)
	for token, who := range people {
		db.Exec(`INSERT INTO accounts VALUES (?, 'x', ?)`, who[0], who[1])
		db.Exec(`INSERT INTO sessions VALUES (?, ?, ?)`, hashToken(token), who[0], time.Now().Add(time.Hour).Unix())
	}
	a := &app{db: db, data: d, tries: map[string][]time.Time{}}
	if a.docs, err = openDocs(d.db, filepath.Join(dir, "documents")); err != nil {
		t.Fatal(err)
	}
	return &TestSite{t: t, App: a, h: a.routes()}
}

// DB is the site's data file.
func (s *TestSite) DB() *sql.DB { return s.App.data.db }

// Do sends a request as a person (by their token; "" for nobody).
func (s *TestSite) Do(as, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if as != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: as})
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

// Post sends a form; Get a page, failing the test if it doesn't answer 200.
func (s *TestSite) Post(as, path string, form url.Values) *httptest.ResponseRecorder {
	return s.Do(as, "POST", path, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
}

func (s *TestSite) Get(as, path string) string {
	s.t.Helper()
	w := s.Do(as, "GET", path, nil, "")
	if w.Code != http.StatusOK {
		s.t.Fatalf("%s %s: %d %s", as, path, w.Code, w.Body)
	}
	return w.Body.String()
}

// DesignProblems imports a design into a scratch file and says what in it
// the rules or sync can't use: none, for a design ready to go live.
func DesignProblems(t testing.TB, design string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "check.db")
	if err := importDesign(path, design, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := openData(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	tables, err := d.design()
	if err != nil {
		t.Fatal(err)
	}
	return designNotes(tables)
}

var (
	hrefs = regexp.MustCompile(`href="(/app/[^"#]*)`)
	forms = regexp.MustCompile(`(?s)<form[^>]*method="post"[^>]*action="(/app/[^"]*)"[^>]*>(.*?)</form>`)
	input = regexp.MustCompile(`name="([^"]+)"(?:[^>]*value="([^"]*)")?`)
)

type formTo struct {
	action string
	fields url.Values
}

// crawl is everything reachable from /app/ as one person: pages, and the
// forms on them.
func (s *TestSite) crawl(as string) (map[string]string, []formTo) {
	pages := map[string]string{}
	var posts []formTo
	todo := []string{base, dataBase, docsBase, base + "people", base + "password"}
	for _, l := range ownLinks {
		todo = append(todo, l.Href)
	}
	for len(todo) > 0 {
		u := todo[0]
		todo = todo[1:]
		if _, seen := pages[u]; seen || strings.Contains(u, "logout") {
			continue
		}
		w := s.Do(as, "GET", u, nil, "")
		pages[u] = fmt.Sprint(w.Code) + " " + w.Body.String()
		if w.Code != 200 {
			continue
		}
		for _, m := range hrefs.FindAllStringSubmatch(w.Body.String(), -1) {
			todo = append(todo, unsorted(html.UnescapeString(m[1])))
		}
		for _, m := range forms.FindAllStringSubmatch(w.Body.String(), -1) {
			f := formTo{action: html.UnescapeString(m[1]), fields: url.Values{}}
			for _, in := range input.FindAllStringSubmatch(m[2], -1) {
				f.fields.Add(in[1], html.UnescapeString(in[2]))
			}
			if !strings.Contains(f.action, "logout") {
				posts = append(posts, f)
			}
		}
	}
	return pages, posts
}

// unsorted is a link without its order or its CSV: these show the same rows,
// and every page is asked for as CSV anyway.
func unsorted(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.RawQuery == "" {
		return link
	}
	q := u.Query()
	q.Del("sort")
	q.Del("desc")
	q.Del("csv")
	u.RawQuery = q.Encode()
	return u.String()
}

// state is everything in the site's two files, as it stands, but sessions
// (signing out is allowed): to show nothing changed.
func (s *TestSite) state() string {
	var out strings.Builder
	for _, q := range []*sql.DB{s.App.data.db, s.App.db} {
		rows, _ := q.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
		var names []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		rows.Close()
		for _, n := range names {
			if n == "sessions" {
				continue
			}
			r, _ := q.Query(`SELECT * FROM ` + quote(n) + ` ORDER BY 1`)
			cols, _ := r.Columns()
			for r.Next() {
				v := make([]any, len(cols))
				p := make([]any, len(cols))
				for i := range v {
					p[i] = &v[i]
				}
				r.Scan(p...)
				fmt.Fprintln(&out, n, v)
			}
			r.Close()
		}
	}
	return out.String()
}

// NothingLeaks is the check every app should keep passing. It walks every
// page admin can reach (the app's own pages too, from their menu links), then
// asks for each, and sends each form, as each of others and as nobody.
//
// Put marked words in the test data, such as SECRET-salary or ANN-job: any
// word secret matches may show to a person only if their pattern in others
// matches it too. Nothing may change, unless mayChange (which may be nil)
// says that person may send that form, with those fields. It returns how many pages and forms
// it found, for the test to check the walk got far enough.
func (s *TestSite) NothingLeaks(admin string, secret *regexp.Regexp, others map[string]*regexp.Regexp, mayChange func(as, action string, fields url.Values) bool) (int, int) {
	s.t.Helper()
	pages, posts := s.crawl(admin)
	var urls []string
	for u := range pages {
		csv := u + "?csv=1"
		if strings.Contains(u, "?") {
			csv = u + "&csv=1"
		}
		urls = append(urls, u, csv)
	}
	sort.Strings(urls)
	people := []string{""}
	for as := range others {
		people = append(people, as)
	}
	sort.Strings(people)
	before := s.state()
	for _, as := range people {
		for _, u := range urls {
			w := s.Do(as, "GET", u, nil, "")
			if as == "" {
				if w.Code == 200 && u != base+"login" {
					s.t.Errorf("nobody got %s", u)
				}
				continue
			}
			for _, m := range secret.FindAllString(w.Body.String(), -1) {
				if others[as] == nil || !others[as].MatchString(m) {
					s.t.Errorf("%s saw %s on %s", as, m, u)
				}
			}
		}
		for _, f := range posts {
			if as != "" && f.action == base+"password" {
				continue // their own password
			}
			if as != "" && mayChange != nil && mayChange(as, f.action, f.fields) {
				s.Post(as, f.action, f.fields)
				before = s.state()
				continue
			}
			s.Post(as, f.action, f.fields)
			if after := s.state(); after != before {
				s.t.Fatalf("%q changed something with %s %v", as, f.action, f.fields)
			}
		}
	}
	return len(pages), len(posts)
}
