package light

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

func TestSignupsPerAddress(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"}, {Name: "box", Addr: "[200::2]:7400", Trial: true}}
	ok := true
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok {
			http.Error(w, "anna.example.org is taken", http.StatusConflict)
			return
		}
		jsonOK(w, map[string]string{"label": "anna", "email": "anna@example.net", "token": "abc"})
	}))
	t.Cleanup(box.Close)
	sent := 0
	rl := &Relay{Peers: func() []peers.Peer { return list }, BoxURL: func(string) string { return box.URL }, Log: t.Logf,
		Email: func(context.Context, string, string, string) error { sent++; return nil }}
	post := func() int {
		r := httptest.NewRequest(http.MethodPost, Base+"/signup", strings.NewReader(`{"label":"anna","email":"anna@example.net","password":"long enough pw"}`))
		r.Host = "my.example.org"
		r.Header.Set("X-Yggstore", "1")
		r.Header.Set("CF-Connecting-IP", "192.0.2.7")
		w := httptest.NewRecorder()
		rl.ServeHTTP(w, r)
		return w.Code
	}
	// Refused sign-ups don't count.
	ok = false
	for range 5 {
		if c := post(); c != http.StatusConflict {
			t.Fatalf("refused: %d", c)
		}
	}
	ok = true
	for i := range SignupsPerIP {
		if c := post(); c != http.StatusOK {
			t.Fatalf("sign-up %d: %d", i+1, c)
		}
	}
	if c := post(); c != http.StatusTooManyRequests || sent != SignupsPerIP {
		t.Fatalf("one too many: %d, %d sent", c, sent)
	}
}

func TestTrialEmailOnlyFromTheTrialBox(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"}, {Name: "box", Addr: "[200::2]:7400", Trial: true},
		{Name: "other", Addr: "[200::3]:7400"}}
	var to, text string
	s := &Store{Dir: filepath.Join(t.TempDir(), "light"), Peers: func() []peers.Peer { return list }, Log: t.Logf,
		Email: func(_ context.Context, a, _, b string) error { to, text = a, b; return nil }}
	ask := func(caller string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/light/trial-email", strings.NewReader(`{"to":"cara@example.net","kind":"warn","login":"cara@box","until":1900000000}`))
		w := httptest.NewRecorder()
		s.Serve(w, r, caller)
		return w.Code
	}
	if c := ask("200::3"); c != http.StatusForbidden || to != "" {
		t.Fatalf("another member asked: %d", c)
	}
	if c := ask("200::2"); c != http.StatusNoContent || to != "cara@example.net" || !strings.Contains(text, "https://my.example.org") {
		t.Fatalf("the trial box asked: %d %q %q", c, to, text)
	}
}

func TestSignupLinkThroughAnotherWebNode(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"}, {Name: "box", Addr: "[200::2]:7400", Trial: true},
		{Name: "web2", Addr: "[201::5]:7400"}, {Name: "other", Addr: "[200::3]:7400"}}
	// The web node that sends mail.
	var to, text string
	sender := &Store{Dir: filepath.Join(t.TempDir(), "light"), Peers: func() []peers.Peer { return list }, Log: t.Logf,
		Others: func(context.Context) []string { return []string{"[201::5]:7400"} },
		Email:  func(_ context.Context, a, _, b string) error { to, text = a, b; return nil }}
	ask := func(caller, body string) int {
		w := httptest.NewRecorder()
		sender.Serve(w, httptest.NewRequest(http.MethodPost, "/v1/light/trial-email", strings.NewReader(body)), caller)
		return w.Code
	}
	link := "https://my.example.org/signup#" + strings.Repeat("ab", 24)
	good := `{"to":"anna@example.net","kind":"confirm","login":"anna.example.org","link":"` + link + `"}`
	if c := ask("200::3", good); c != http.StatusForbidden || to != "" {
		t.Fatalf("a member that isn't a web node asked: %d", c)
	}
	for _, bad := range []string{
		`{"to":"anna@example.net","kind":"confirm","login":"anna.example.org","link":"https://evil.example/signup#` + strings.Repeat("ab", 24) + `"}`,
		`{"to":"anna@example.net","kind":"confirm","login":"anna.example.org","link":"https://my.example.org/files#x"}`,
	} {
		if c := ask("201::5", bad); c != http.StatusBadRequest || to != "" {
			t.Fatalf("not a sign-up link: %d", c)
		}
	}
	if c := ask("201::5", good); c != http.StatusNoContent || to != "anna@example.net" || !strings.Contains(text, link) {
		t.Fatalf("a web node asked: %d %q", c, text)
	}

	// A web node without mail passes the link on.
	mailer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sender.Serve(w, r, "201::5") }))
	t.Cleanup(mailer.Close)
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]string{"label": "bob", "email": "bob@example.net", "token": strings.Repeat("cd", 24)})
	}))
	t.Cleanup(box.Close)
	rl := &Relay{Peers: func() []peers.Peer { return list }, BoxURL: func(string) string { return box.URL }, Log: t.Logf,
		Mailers: func(context.Context) []string { return []string{strings.TrimPrefix(mailer.URL, "http://")} }}
	r := httptest.NewRequest(http.MethodPost, Base+"/signup", strings.NewReader(`{"label":"bob","email":"bob@example.net","password":"long enough pw"}`))
	r.Host = "my.example.org"
	r.Header.Set("X-Yggstore", "1")
	w := httptest.NewRecorder()
	rl.ServeHTTP(w, r)
	if w.Code != http.StatusOK || to != "bob@example.net" || !strings.Contains(text, "bob.example.org") {
		t.Fatalf("sign-up through another web node: %d %s, sent to %q", w.Code, w.Body, to)
	}
}

func TestSignupsPaused(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"}, {Name: "box", Addr: "[200::2]:7400", Trial: true, TrialPaused: true}}
	asked := false
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked = true }))
	t.Cleanup(box.Close)
	rl := &Relay{Peers: func() []peers.Peer { return list }, BoxURL: func(string) string { return box.URL }, Log: t.Logf,
		Email: func(context.Context, string, string, string) error { return nil }}
	r := httptest.NewRequest(http.MethodPost, Base+"/signup", strings.NewReader(`{"label":"anna","email":"anna@example.net","password":"long enough pw"}`))
	r.Host = "my.example.org"
	r.Header.Set("X-Yggstore", "1")
	w := httptest.NewRecorder()
	rl.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "paused") || asked {
		t.Fatalf("paused: %d %q, box asked %v", w.Code, w.Body, asked)
	}
}

func TestSignInAtOwnDomain(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[200::2]:7400", PeopleNames: []peers.PersonName{
			{Name: "tommy.example.org", Person: "tommy", Code: "ys1t"},
			{Name: "acme.com", Person: "tommy", Code: "ys1t", Site: true}}}}
	s := &Store{Dir: filepath.Join(t.TempDir(), "light"), Peers: func() []peers.Peer { return list }, Log: t.Logf}
	pw := password.Chosen("long enough pw", time.Now())
	s.SetPeople("box", []Person{{Name: "tommy", Web: true, Password: &pw}, {Name: "kath", Web: true, Password: &pw}})
	for _, login := range []string{"tommy@example.org", "tommy@acme.com", "Info@ACME.com"} {
		if f, ok := s.byLogin(list, login); !ok || f.Person.Name != "tommy" {
			t.Errorf("%s: %v %+v", login, ok, f)
		}
	}
	for _, login := range []string{"kath@other.com", "tommy@other.com"} {
		if f, ok := s.byLogin(list, login); ok {
			t.Errorf("%s signed in as %s", login, f.Person.Name)
		}
	}
}
