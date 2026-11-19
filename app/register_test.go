package app

import (
	"database/sql"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// mailbag keeps what an app emails.
type mailbag struct {
	mu   sync.Mutex
	sent []struct{ To, Subject, Body string }
}

func (m *mailbag) send(to []string, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, struct{ To, Subject, Body string }{strings.Join(to, ","), subject, body})
	return nil
}

func (m *mailbag) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *mailbag) last() (to, subject, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x := m.sent[len(m.sent)-1]
	return x.To, x.Subject, x.Body
}

var linkRE = regexp.MustCompile(`https?://[^\s"<]+/app/welcome\?[^\s"<]+`)

// site is an app with an admin, as a browser sees it.
type testSite struct {
	t    *testing.T
	a    *app
	h    http.Handler
	mail *mailbag
}

func newTestSite(t *testing.T, d *data, secret []byte, withMail bool) *testSite {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Exec(`CREATE TABLE accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'admin');
		CREATE TABLE sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL);`)
	s := &testSite{t: t, mail: &mailbag{}}
	s.a = &app{db: db, data: d, tries: map[string][]time.Time{}, registering: true, secret: secret, site: "school.example.org"}
	if withMail {
		s.a.mail = s.mail.send
	}
	s.h = s.a.routes()
	if err := s.a.setAccount("boss@example.org", hashPassword("boss password 1"), roleAdmin); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO sessions VALUES (?, 'boss@example.org', ?)`, hashToken("boss"), time.Now().Add(time.Hour).Unix())
	return s
}

func (s *testSite) do(method, path, ip string, f url.Values, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-For", ip)
	r.Header.Set("X-Forwarded-Proto", "https")
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func (s *testSite) signIn(email, pw string) int {
	return s.do("POST", base+"login", "9.9.9.9", url.Values{"email": {email}, "password": {pw}}, "").Code
}

func (s *testSite) people() string {
	w := s.do("GET", base+"people", "1.1.1.1", nil, "boss")
	if w.Code != 200 {
		s.t.Fatalf("people: %d", w.Code)
	}
	return w.Body.String()
}

func (s *testSite) admin(do, email, role string) *httptest.ResponseRecorder {
	return s.do("POST", base+"people", "1.1.1.1", url.Values{"do": {do}, "email": {email}, "role": {role}}, "boss")
}

func register(name, email, about string) url.Values {
	return url.Values{"name": {name}, "email": {email}, "about": {about}}
}

func TestRegisterVetInvite(t *testing.T) {
	s := newTestSite(t, nil, []byte("0123456789abcdef0123456789abcdef"), true)

	// The sign-in page offers registering and a forgotten password.
	login := s.do("GET", base+"login", "1.1.1.1", nil, "").Body.String()
	if !strings.Contains(login, base+"register") || !strings.Contains(login, base+"forgot") {
		t.Fatal(login)
	}

	// Lee registers; the admins hear of it.
	w := s.do("POST", base+"register", "5.5.5.5", register("Lee Smith", " Lee@Example.org ", "Grade 4 parent"), "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Thank you") {
		t.Fatalf("registering: %d %s", w.Code, w.Body)
	}
	waitFor(t, "the admins told", func() bool { return s.mail.count() == 1 })
	if to, subject, body := s.mail.last(); to != "boss@example.org" || !strings.Contains(subject, "Lee Smith") || !strings.Contains(body, "Grade 4 parent") {
		t.Fatalf("the admins' email: %s %s %s", to, subject, body)
	}
	// Lee waits: listed for the admin, not among everyone, can't sign in.
	p := s.people()
	if !strings.Contains(p, "Waiting to be let in") || !strings.Contains(p, "lee@example.org") || !strings.Contains(p, "Grade 4 parent") {
		t.Fatal(p)
	}
	if strings.Contains(p[strings.Index(p, "Everyone"):], "lee@example.org") {
		t.Fatal("Lee is among everyone while waiting")
	}
	if s.signIn("lee@example.org", "") != http.StatusUnauthorized {
		t.Fatal("a waiting registration signed in")
	}

	// Registering again, or as someone with an account, looks the same, and
	// changes nothing; a robot filling the hidden field is dropped.
	for _, f := range []url.Values{register("Lee again", "lee@example.org", "x"), register("Boss", "boss@example.org", "")} {
		if w := s.do("POST", base+"register", "6.6.6.6", f, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Thank you") {
			t.Fatalf("again: %d", w.Code)
		}
	}
	if x, _ := s.a.getAccount("boss@example.org"); x.Role != roleAdmin {
		t.Fatal("registering changed an account")
	}
	bot := register("Bot", "bot@example.org", "")
	bot.Set("website", "http://spam.example")
	s.do("POST", base+"register", "7.7.7.7", bot, "")
	if _, ok := s.a.getAccount("bot@example.org"); ok {
		t.Fatal("a robot registered")
	}
	if w := s.do("POST", base+"register", "7.7.7.8", register("<b>x</b>", "x@example.org", ""), ""); w.Code != http.StatusBadRequest {
		t.Fatalf("a name with tags: %d", w.Code)
	}

	// Let in as a client: Lee gets the link.
	if w := s.admin("letin", "lee@example.org", "client"); w.Code != 200 || !strings.Contains(w.Body.String(), "is on its way to") {
		t.Fatalf("letting in: %d %s", w.Code, w.Body)
	}
	to, _, body := s.mail.last()
	link := linkRE.FindString(body)
	if to != "lee@example.org" || !strings.HasPrefix(link, "https://school.example.org/app/welcome?") {
		t.Fatalf("the invite: %s %s", to, body)
	}
	if !strings.Contains(s.people(), "hasn't chosen a password yet") {
		t.Fatal("not shown as waiting for a password")
	}
	u, _ := url.Parse(link)
	if w := s.do("GET", u.RequestURI(), "8.8.8.8", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "lee@example.org") ||
		w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("the link: %d %s", w.Code, w.Body)
	}
	// Asking again soon sends nothing, and the link is the same.
	n := s.mail.count()
	s.admin("invite", "lee@example.org", "")
	if s.mail.count() != n {
		t.Fatal("a second email within minutes")
	}
	// A wrong token, or another person's, doesn't work.
	q := u.Query()
	if w := s.do("GET", base+"welcome?"+url.Values{"e": {"boss@example.org"}, "t": {q.Get("t")}}.Encode(), "8.8.8.8", nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("the token for someone else: %d", w.Code)
	}

	// Lee chooses a password: signed in at once.
	f := url.Values{"e": {q.Get("e")}, "t": {q.Get("t")}, "new": {"lees password 1"}, "again": {"lees password 2"}}
	if w := s.do("POST", base+"welcome", "8.8.8.8", f, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("different passwords: %d", w.Code)
	}
	f.Set("again", "lees password 1")
	w = s.do("POST", base+"welcome", "8.8.8.8", f, "")
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) == 0 {
		t.Fatalf("choosing a password: %d %s", w.Code, w.Body)
	}
	me := s.do("GET", base+"me", "8.8.8.8", nil, w.Result().Cookies()[0].Value)
	if strings.TrimSpace(me.Body.String()) != "lee@example.org" {
		t.Fatalf("signed in as %q", me.Body)
	}
	// The link works once.
	if w := s.do("GET", u.RequestURI(), "8.8.8.8", nil, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "has been used") {
		t.Fatalf("the link again: %d", w.Code)
	}
	if s.signIn("lee@example.org", "lees password 1") != http.StatusSeeOther {
		t.Fatal("signing in with the new password")
	}

	// Turning down deletes a registration.
	s.do("POST", base+"register", "5.5.5.6", register("Kai", "kai@example.org", ""), "")
	if s.admin("turndown", "kai@example.org", "").Code != http.StatusSeeOther {
		t.Fatal("turning down")
	}
	if _, ok := s.a.getAccount("kai@example.org"); ok {
		t.Fatal("still there after turning down")
	}
	// Only waiting registrations can be let in or turned down.
	if s.admin("turndown", "lee@example.org", "").Code != http.StatusBadRequest || s.admin("letin", "lee@example.org", "admin").Code != http.StatusBadRequest {
		t.Fatal("let in or turned down someone not waiting")
	}

	// A forgotten password: the same answer for everyone, and a link only for
	// someone with an account; the old password works until a new one is set.
	before := s.mail.count()
	for _, who := range []string{"lee@example.org", "nobody@example.org"} {
		w := s.do("POST", base+"forgot", "4.4.4.4", url.Values{"email": {who}}, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "If there's an account") {
			t.Fatalf("forgot %s: %d", who, w.Code)
		}
	}
	if s.mail.count() != before+1 {
		t.Fatalf("%d emails for forgotten passwords", s.mail.count()-before)
	}
	_, subject, body := s.mail.last()
	if !strings.Contains(subject, "password") || linkRE.FindString(body) == "" {
		t.Fatal(subject, body)
	}
	if s.signIn("lee@example.org", "lees password 1") != http.StatusSeeOther {
		t.Fatal("asking for a link changed the password")
	}
}

func TestInviteWithoutMail(t *testing.T) {
	s := newTestSite(t, nil, []byte("0123456789abcdef0123456789abcdef"), false)
	if strings.Contains(s.do("GET", base+"login", "1.1.1.1", nil, "").Body.String(), base+"forgot") {
		t.Fatal("offers a forgotten password with no email")
	}
	if w := s.do("GET", base+"forgot", "1.1.1.1", nil, ""); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("forgot with no email: %d", w.Code)
	}
	s.do("POST", base+"register", "5.5.5.5", register("Lee", "lee@example.org", ""), "")
	w := s.admin("letin", "lee@example.org", "staff")
	link := linkRE.FindString(html.UnescapeString(w.Body.String()))
	if !strings.Contains(w.Body.String(), "Pass this link on") || link == "" {
		t.Fatalf("no link to pass on: %s", w.Body)
	}
	if x, _ := s.a.getAccount("lee@example.org"); x.Role != roleStaff {
		t.Fatalf("let in as %q", x.Role)
	}
	// Adding someone still shows a temporary password.
	if w := s.admin("add", "sam@example.org", "client"); !strings.Contains(w.Body.String(), "temporary password") {
		t.Fatal("no temporary password")
	}
}

func TestRegisteringOff(t *testing.T) {
	s := newTestSite(t, nil, []byte("0123456789abcdef0123456789abcdef"), true)
	s.a.registering = false
	s.h = s.a.routes()
	if w := s.do("POST", base+"register", "5.5.5.5", register("Lee", "lee@example.org", ""), ""); w.Code == 200 {
		t.Fatal("registered with registering off")
	}
	if strings.Contains(s.do("GET", base+"login", "1.1.1.1", nil, "").Body.String(), base+"register") {
		t.Fatal("offers registering")
	}
}

// A registration on one machine is let in on the other, and the link works
// on either: they share the key links are made with.
func TestRegisterAcrossMachines(t *testing.T) {
	secret, _ := linkSecret([]byte("the key both machines share 1234"), "")
	da, db := machine(t, "desktop"), machine(t, "box")
	a := newTestSite(t, da, secret, true)
	b := newTestSite(t, db, secret, true)
	a.do("POST", base+"register", "5.5.5.5", register("Lee", "lee@example.org", "from the desktop"), "")
	meet(t, da, db)
	if !strings.Contains(b.people(), "from the desktop") {
		t.Fatal("the registration isn't on the other machine")
	}
	if w := b.admin("letin", "lee@example.org", "client"); w.Code != 200 {
		t.Fatalf("letting in on the box: %d", w.Code)
	}
	_, _, body := b.mail.last()
	u, _ := url.Parse(linkRE.FindString(body))
	meet(t, da, db)
	// Lee's link opens on the desktop, and the password reaches the box.
	f := url.Values{"e": {u.Query().Get("e")}, "t": {u.Query().Get("t")}, "new": {"lees password 1"}, "again": {"lees password 1"}}
	if w := a.do("POST", base+"welcome", "8.8.8.8", f, ""); w.Code != http.StatusSeeOther {
		t.Fatalf("the box's link on the desktop: %d %s", w.Code, w.Body)
	}
	meet(t, da, db)
	if b.signIn("lee@example.org", "lees password 1") != http.StatusSeeOther {
		t.Fatal("the password didn't reach the box")
	}
	if w := b.do("POST", base+"welcome", "8.8.8.8", f, ""); w.Code != http.StatusNotFound {
		t.Fatalf("the used link on the box: %d", w.Code)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 100 {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited for %s", what)
}
