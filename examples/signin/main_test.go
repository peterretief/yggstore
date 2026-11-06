package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSignIn(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL);
		CREATE TABLE sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL);`)
	db.Exec(`INSERT INTO accounts VALUES ('alice', ?)`, hashPassword("correct horse"))
	a := &app{db: db, tries: map[string][]time.Time{}}
	h := a.routes()
	do := func(method, path, ip, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Forwarded-For", ip)
		r.Header.Set("X-Forwarded-Proto", "https")
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	form := func(pw string) string { return url.Values{"name": {"alice"}, "password": {pw}}.Encode() }

	if w := do("GET", base, "1.1.1.1", "", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("the private page, signed out: %d", w.Code)
	}
	if w := do("GET", base+"me", "1.1.1.1", "", &http.Cookie{Name: cookieName, Value: "made-up"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("a made-up session: %d", w.Code)
	}
	w := do("POST", base+"login", "1.1.1.1", form("correct horse"), nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("signing in: %d %s", w.Code, w.Body)
	}
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.Path != base {
		t.Fatalf("the cookie: %+v", c)
	}
	if w := do("GET", base, "1.1.1.1", "", c); w.Code != 200 || !strings.Contains(w.Body.String(), "alice") {
		t.Fatalf("the private page: %d %s", w.Code, w.Body)
	}
	var stored string
	db.QueryRow(`SELECT token FROM sessions`).Scan(&stored)
	if stored == c.Value {
		t.Fatal("the session is kept as it is in the cookie")
	}
	if w := do("POST", base+"logout", "1.1.1.1", "", c); w.Code != http.StatusSeeOther {
		t.Fatalf("signing out: %d", w.Code)
	}
	if w := do("GET", base+"me", "1.1.1.1", "", c); w.Code != http.StatusUnauthorized {
		t.Fatalf("after signing out: %d", w.Code)
	}
	// Wrong tries stop, from one address and for one name from many.
	for i := range maxTries {
		if w := do("POST", base+"login", "2.2.2."+string(rune('0'+i)), form("wrong"), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("try %d: %d", i, w.Code)
		}
	}
	if w := do("POST", base+"login", "3.3.3.3", form("correct horse"), nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after too many tries: %d", w.Code)
	}
}
