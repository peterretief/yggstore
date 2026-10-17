package light

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

// signedIn gives a store anna on box, and a session for her at host.
func signedIn(t *testing.T, s *Store, host string) string {
	s.SetPeople("box", []Person{{Name: "anna", Web: true, Keys: []Key{{Key: passkey.Key{ID: "k1"}, Name: "phone"}}}})
	f, ok := s.key("k1")
	if !ok {
		t.Fatal("no key")
	}
	token, _ := s.newSession(f, host, time.Now())
	return token
}

func TestRelay(t *testing.T) {
	var got *http.Request
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r; w.Write([]byte("from the box")) }))
	defer box.Close()
	list := []peers.Peer{{Name: "box", Addr: "[201::4]:7400"}}
	store := &Store{Dir: t.TempDir(), Peers: func() []peers.Peer { return list }}
	rl := &Relay{Peers: func() []peers.Peer { return list }, Signin: store, BoxURL: func(ip string) string {
		if ip != "201::4" {
			t.Errorf("relayed to %s", ip)
		}
		return box.URL
	}}
	do := func(method, target, session string, hdr map[string]string) *httptest.ResponseRecorder {
		got = nil
		r := httptest.NewRequest(method, target, nil)
		r.Host = "my.example.org"
		if session != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		rl.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/files/join?box=nowhere&t=x", "", nil); w.Code != 404 {
		t.Errorf("an invite for an unknown box: %d", w.Code)
	}
	if w := do("GET", "/files/join?box=box&t=x", "", nil); w.Code != 200 || got == nil {
		t.Errorf("an invite didn't reach the box: %d", w.Code)
	}
	if w := do("GET", "/files", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Sign in") || got != nil {
		t.Errorf("files signed out: %d", w.Code)
	}
	if w := do("POST", "/files?op=list", "", map[string]string{"X-Yggstore": "1"}); w.Code != http.StatusUnauthorized || got != nil {
		t.Errorf("a request signed out: %d", w.Code)
	}
	if w := do("GET", "/", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Sign in") {
		t.Errorf("home signed out: %d", w.Code)
	}
	token := signedIn(t, store, "my.example.org")
	if w := do("GET", "/etc/passwd", token, nil); w.Code != 404 || got != nil {
		t.Errorf("a path outside /files: %d", w.Code)
	}
	if w := do("GET", "/", token, nil); w.Code != http.StatusFound {
		t.Errorf("home signed in: %d", w.Code)
	}
	w := do("GET", "/files?op=x", token, map[string]string{RelayHeader: "0", PersonHeader: "ben", "CF-Connecting-IP": "198.51.100.7", "X-Forwarded-Host": "evil.example"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "from the box") {
		t.Fatalf("relayed: %d %s", w.Code, w.Body)
	}
	if got.Header.Get(RelayHeader) != "1" || got.Header.Get(PersonHeader) != "anna" || got.Header.Get("X-Forwarded-Host") != "my.example.org" ||
		got.Header.Get("X-Forwarded-For") != "198.51.100.7" || got.URL.RequestURI() != "/files?op=x" {
		t.Fatalf("the box got %s with %v", got.URL, got.Header)
	}
	// A browser's own word for who it is goes nowhere.
	do("POST", "/files/join?box=box", "", map[string]string{"X-Yggstore": "1", PersonHeader: "anna"})
	if got == nil || got.Header.Get(PersonHeader) != "" {
		t.Fatalf("a browser named the person: %v", got)
	}
	// The session is for my. only.
	r := httptest.NewRequest("GET", "/files?op=x", nil)
	r.Host = "other.example.org"
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	got = nil
	rl.ServeHTTP(httptest.NewRecorder(), r)
	if got != nil {
		t.Fatal("a session for my. worked elsewhere")
	}
	box.Close()
	if w := do("GET", "/files", token, nil); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "isn't answering") {
		t.Fatalf("box off: %d %s", w.Code, w.Body)
	}
	// Signed out, here and on the others.
	if w := do("POST", Base+"/signout", token, map[string]string{"X-Yggstore": "1"}); w.Code != http.StatusNoContent {
		t.Fatalf("sign out: %d", w.Code)
	}
	if w := do("GET", "/", token, nil); w.Code != 200 {
		t.Fatalf("home after signing out: %d", w.Code)
	}
}

func TestRelayPersonHost(t *testing.T) {
	var got *http.Request
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r }))
	defer box.Close()
	list := []peers.Peer{{Name: "box", Addr: "[201::4]:7400", PeopleNames: []peers.PersonName{
		{Name: "kath.example.org", Person: "Kath", Code: "ys1x"}, {Name: "anna.example.org", Person: "anna", Code: "ys1y"}}}}
	store := &Store{Dir: t.TempDir(), Peers: func() []peers.Peer { return list }}
	rl := &Relay{Peers: func() []peers.Peer { return list }, Signin: store, BoxURL: func(ip string) string {
		if ip != "201::4" {
			t.Errorf("relayed to %s", ip)
		}
		return box.URL
	}}
	do := func(host, target, session string) int {
		got = nil
		r := httptest.NewRequest("GET", target, nil)
		r.Host = host
		if session != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
		}
		w := httptest.NewRecorder()
		rl.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("kath.example.org", "/files/join?t=x", ""); c != 200 || got == nil || got.Header.Get("X-Forwarded-Host") != "kath.example.org" {
		t.Fatalf("kath's invite: %d %v", c, got)
	}
	if c := do("Kath.Example.org", "/files", ""); c != 200 || got != nil {
		t.Fatalf("kath's files signed out: %d", c)
	}
	if c := do("kath.example.org", "/", ""); c != 404 || got != nil {
		t.Fatalf("kath's website went to the box: %d", c)
	}
	// anna's session at anna's address is no good at kath's.
	token := signedIn(t, store, "anna.example.org")
	if c := do("anna.example.org", "/files", token); c != 200 || got == nil || got.Header.Get(PersonHeader) != "anna" {
		t.Fatalf("anna's files: %d %v", c, got)
	}
	if c := do("kath.example.org", "/files", token); got != nil {
		t.Fatalf("anna's session at kath's address: %d", c)
	}
}

// Two web nodes share sessions and wrong PINs, and take a box's people
// only from that box.
func TestStoreShared(t *testing.T) {
	list := []peers.Peer{{Name: "web-a", Addr: "[201::a]:7400"}, {Name: "web-b", Addr: "[201::b]:7400"}, {Name: "box", Addr: "[201::4]:7400"}}
	var a, b *Store
	serve := func(s **Store, caller string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !(*s).Serve(w, r, caller) {
				http.NotFound(w, r)
			}
		}))
	}
	// Each node's server sees the other as the caller.
	toA, toB := serve(&a, "201::b"), serve(&b, "201::a")
	defer toA.Close()
	defer toB.Close()
	addr := func(u string) string { return strings.TrimPrefix(u, "http://") }
	mk := func(other string) *Store {
		return &Store{Dir: t.TempDir(), Peers: func() []peers.Peer { return list }, Client: http.DefaultClient,
			Others: func(context.Context) []string { return []string{other} }}
	}
	a, b = mk(addr(toB.URL)), mk(addr(toA.URL))
	// isWebNode compares addresses: the test servers aren't at 201::a/b.
	a.web, a.webAt = map[string]bool{"201::b": true}, time.Now()
	b.web, b.webAt = map[string]bool{"201::a": true}, time.Now()

	ppl := []Person{{Name: "anna", Web: true, Keys: []Key{{Key: passkey.Key{ID: "k1"}, Name: "phone"}}}}
	a.SetPeople("box", ppl)
	b.SetPeople("box", ppl)
	token := signedIn(t, a, "my.example.org")
	ctx := context.Background()
	now := time.Now()
	if _, ok := b.session(ctx, token, "my.example.org", now); !ok {
		t.Fatal("web-b doesn't know a session web-a started")
	}
	a.end(tokenHash(token), true)
	waitUntil(t, func() bool {
		b.lock()
		defer b.mu.Unlock()
		return len(b.st.Sessions) == 0
	}, "web-b to hear of the sign-out")
	if _, ok := b.session(ctx, token, "my.example.org", now); ok {
		t.Fatal("a signed-out session still works on web-b")
	}
	a.setFails("k1", 3, true)
	waitUntil(t, func() bool { f, _ := b.key("k1"); return f.Fails == 3 }, "web-b to hear of wrong PINs")

	// Only the box sends its people, and only web nodes share sessions.
	w := httptest.NewRecorder()
	a.Serve(w, httptest.NewRequest("PUT", "/v1/light/people", strings.NewReader(`{"people":[]}`)), "201::99")
	if w.Code != http.StatusForbidden {
		t.Fatalf("people from a stranger: %d", w.Code)
	}
	w = httptest.NewRecorder()
	a.Serve(w, httptest.NewRequest("PUT", "/v1/light/people", strings.NewReader(`{"people":[]}`)), "201::4")
	if f, ok := a.key("k1"); w.Code != http.StatusNoContent || ok {
		t.Fatalf("people from the box: %d, still has %+v", w.Code, f)
	}
	w = httptest.NewRecorder()
	a.Serve(w, httptest.NewRequest("PUT", "/v1/light/session", strings.NewReader(`{}`)), "201::4")
	if w.Code != http.StatusForbidden {
		t.Fatalf("a session from a box: %d", w.Code)
	}
}

func waitUntil(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %s", what)
}

// The box gives the browser its half of the recovery copy's key while it's
// on; with the box off, the files page opens the copy with it.
func TestRelayBoxOff(t *testing.T) {
	const half = "yKTBM4Go0vXeL9v1bj9wAg.0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(CopyHeader, half)
		w.Write([]byte("from the box"))
	}))
	list := []peers.Peer{{Name: "box", Addr: "[201::4]:7400"}}
	store := &Store{Dir: t.TempDir(), Peers: func() []peers.Peer { return list }}
	rl := &Relay{Peers: func() []peers.Peer { return list }, Signin: store, BoxURL: func(string) string { return box.URL }}
	token := signedIn(t, store, "my.example.org")
	do := func(method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Host = "my.example.org"
		r.Header.Set("X-Yggstore", "1")
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		rl.ServeHTTP(w, r)
		return w
	}
	w := do("GET", "/files")
	var kept *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == CopyCookie {
			kept = c
		}
	}
	if kept == nil || kept.Value != half || !kept.HttpOnly || !kept.Secure || w.Header().Get(CopyHeader) != "" {
		t.Fatalf("the browser was given %+v, header %q", kept, w.Header().Get(CopyHeader))
	}
	if w := do("GET", "/files", kept); len(w.Result().Cookies()) != 0 {
		t.Fatal("a browser that has the half was given it again")
	}

	box.Close()
	if w := do("GET", "/files", kept); w.Code != http.StatusFound || w.Header().Get("Location") != WorkPath+"?off=1#"+half {
		t.Fatalf("box off, with the half: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := do("GET", "/files"); w.Code != http.StatusBadGateway {
		t.Fatalf("box off, without it: %d", w.Code)
	}
	if w := do("POST", "/files?op=list", kept); w.Code != http.StatusBadGateway {
		t.Fatalf("box off, an action: %d", w.Code)
	}
	// Signed out, the browser forgets it.
	w = do("POST", Base+"/signout", kept)
	for _, c := range w.Result().Cookies() {
		if c.Name == CopyCookie && c.MaxAge < 0 {
			return
		}
	}
	t.Fatal("signing out left the half in the browser")
}
