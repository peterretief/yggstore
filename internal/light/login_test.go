package light

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

// Two web nodes, each with a relay and a store that shares with the other.
type loginGroup struct {
	t      *testing.T
	relays [2]*Relay
	stores [2]*Store
	boxGot *http.Request
}

func newLoginGroup(t *testing.T) *loginGroup {
	g := &loginGroup{t: t}
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.boxGot = r; w.Write([]byte("from the box")) }))
	t.Cleanup(box.Close)
	list := []peers.Peer{
		{Name: "admin", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[201::4]:7400", PeopleNames: []peers.PersonName{{Name: "anna.example.org", Person: "anna"}}},
	}
	listFn := func() []peers.Peer { return list }
	var api [2]string
	for i := range g.stores {
		s := &Store{Dir: t.TempDir(), Peers: listFn, Client: http.DefaultClient}
		g.stores[i] = s
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.Serve(w, r, "127.0.0.1") {
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		api[i] = strings.TrimPrefix(srv.URL, "http://")
		g.relays[i] = &Relay{Peers: listFn, Signin: s, BoxURL: func(string) string { return box.URL }}
	}
	for i := range g.stores {
		other := api[1-i]
		g.stores[i].Others = func(context.Context) []string { return []string{other} }
	}
	people := []Person{
		{Name: "anna", Web: true, Keys: []Key{{Key: passkey.Key{ID: "k-anna"}, Name: "phone"}}},
		{Name: "ben", Web: true, Keys: []Key{{Key: passkey.Key{ID: "k-ben"}, Name: "laptop"}}},
	}
	for _, s := range g.stores {
		s.SetPeople("box", people)
	}
	return g
}

// do sends a request to web node n, with the cookies given.
func (g *loginGroup) do(n int, method, target string, cookies map[string]string, form url.Values) *httptest.ResponseRecorder {
	u, _ := url.Parse(target)
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, u.RequestURI(), strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, u.RequestURI(), nil)
	}
	r.Host = u.Host
	for k, v := range cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	g.relays[n].ServeHTTP(w, r)
	return w
}

func cookie(w *httptest.ResponseRecorder, name string) (string, bool) {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c.Value, c.MaxAge >= 0
		}
	}
	return "", false
}

// session signs person in at host on web node n with their key.
func (g *loginGroup) session(n int, person, host string) string {
	f, ok := g.stores[n].key("k-" + person)
	if !ok {
		g.t.Fatal("no key")
	}
	token, _ := g.stores[n].newSession(f, host, time.Now())
	return token
}

// Someone signed in at my. opens their own address and is signed in there
// too, whichever web node each step reaches; someone else isn't.
func TestLoginAtOwnAddress(t *testing.T) {
	g := newLoginGroup(t)
	my := g.session(0, "anna", "my.example.org")

	open := func(myCookie string) (string, *httptest.ResponseRecorder) {
		// Not signed in at anna.example.org: off to my., asking quietly.
		w := g.do(0, "GET", "https://anna.example.org/files", nil, nil)
		loc := w.Header().Get("Location")
		if w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://my.example.org/login/authorize?") || !strings.Contains(loc, "prompt=none") {
			t.Fatalf("files signed out: %d %s", w.Code, loc)
		}
		login, _ := cookie(w, loginCookie)
		// my. (the other web node) sends it back.
		cs := map[string]string{}
		if myCookie != "" {
			cs[SessionCookie] = myCookie
		}
		w = g.do(1, "GET", loc, cs, nil)
		back := w.Header().Get("Location")
		if w.Code != http.StatusFound || !strings.HasPrefix(back, "https://anna.example.org/files/login?") {
			t.Fatalf("my. answered %d %s", w.Code, back)
		}
		return back, g.do(0, "GET", back, map[string]string{loginCookie: login}, nil)
	}

	back, w := open(my)
	own, ok := cookie(w, SessionCookie)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/files" || !ok || own == "" {
		t.Fatalf("back at anna.example.org: %d %s", w.Code, w.Body)
	}
	g.do(0, "GET", "https://anna.example.org/files?op=list", map[string]string{SessionCookie: own}, nil)
	if g.boxGot == nil || g.boxGot.Header.Get(PersonHeader) != "anna" {
		t.Fatal("signed in at her address, her box doesn't hear it's her")
	}
	// A code works once.
	if w := g.do(1, "GET", back, map[string]string{loginCookie: "x"}, nil); w.Code != http.StatusOK || strings.Contains(w.Header().Get("Set-Cookie"), SessionCookie) {
		t.Fatalf("a code used again: %d", w.Code)
	}

	// Ben signed in at my. isn't let in at anna's address.
	_, w = open(g.session(1, "ben", "my.example.org"))
	if _, ok := cookie(w, SessionCookie); ok || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sign in") {
		t.Fatalf("someone else at her address: %d", w.Code)
	}
	// Nobody signed in at my.: her address's own sign-in page.
	back, w = open("")
	if !strings.Contains(back, "error=login_required") || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sign in") {
		t.Fatalf("signed out everywhere: %s %d", back, w.Code)
	}

	// Her key is my.'s: from her address's sign-in page, to my.'s.
	w = g.do(0, "GET", "https://anna.example.org/files/login/start", nil, nil)
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://my.example.org/login/authorize?") || strings.Contains(loc, "prompt=") {
		t.Fatalf("to sign in at my.: %d %s", w.Code, loc)
	}

	// Signing out at my. signs her out at her address too, on both nodes.
	g.do(1, "GET", "https://my.example.org/login/signout", map[string]string{SessionCookie: my}, nil)
	time.Sleep(200 * time.Millisecond) // the other node hears
	for n := range g.relays {
		if w := g.do(n, "GET", "https://anna.example.org/files?op=list", map[string]string{SessionCookie: own}, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("node %d: still signed in at her address after signing out at my.: %d", n, w.Code)
		}
	}
}

// Signing in at her own address signs her in at my. too, as one browser.
func TestLoginAdopt(t *testing.T) {
	g := newLoginGroup(t)
	f, _ := g.stores[0].key("k-anna")
	token, ss := g.stores[0].newSession(f, "anna.example.org", time.Now())
	next := g.relays[0].adoptLink(ss, "anna.example.org")
	if !strings.HasPrefix(next, "https://my.example.org/login/adopt?t=") {
		t.Fatalf("next: %q", next)
	}
	w := g.do(1, "GET", next, nil, nil)
	_, ok := cookie(w, SessionCookie)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://anna.example.org/files" || !ok {
		t.Fatalf("adopt: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := g.do(1, "GET", next, nil, nil); w.Header().Get("Location") != "/" {
		t.Fatal("an adopt ticket worked twice")
	}
	time.Sleep(200 * time.Millisecond)
	w = g.do(0, "GET", "https://anna.example.org/files/devices?op=list", map[string]string{SessionCookie: token}, nil)
	var d struct {
		Sessions []struct {
			Me     bool     `json:"me"`
			Places []string `json:"places"`
		} `json:"sessions"`
	}
	json.Unmarshal(w.Body.Bytes(), &d)
	if len(d.Sessions) != 1 || !d.Sessions[0].Me || strings.Join(d.Sessions[0].Places, " ") != "my.example.org anna.example.org" {
		t.Fatalf("her devices: %d %s", w.Code, w.Body)
	}
}

// An ordinary OpenID Connect app at a group address: discovery, a code,
// tokens from either web node, keys checking out, userinfo.
func TestLoginForApps(t *testing.T) {
	g := newLoginGroup(t)
	my := g.session(0, "anna", "my.example.org")
	cs := map[string]string{SessionCookie: my}

	w := g.do(0, "GET", "https://my.example.org/.well-known/openid-configuration", nil, nil)
	var conf map[string]any
	json.NewDecoder(w.Body).Decode(&conf)
	if conf["issuer"] != "https://my.example.org" || conf["token_endpoint"] != "https://my.example.org/login/token" {
		t.Fatalf("discovery: %v", conf)
	}
	if w := g.do(0, "GET", "https://anna.example.org/.well-known/openid-configuration", nil, nil); w.Code == http.StatusOK {
		t.Fatal("the login answered at someone's own address")
	}

	verifier := strings.Repeat("v", 50)
	h := sha256.Sum256([]byte(verifier))
	challenge := passkey.B64.EncodeToString(h[:])
	auth := func(client, redirect string) *httptest.ResponseRecorder {
		q := url.Values{"client_id": {client}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {"openid"},
			"state": {"s1"}, "nonce": {"n1"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
		return g.do(0, "GET", "https://my.example.org/login/authorize?"+q.Encode(), cs, nil)
	}
	for _, bad := range [][2]string{
		{"evil.example.net", "https://evil.example.net/cb"},
		{"wiki.example.org", "https://evil.example.net/cb"},
		{"wiki.example.org", "http://wiki.example.org/cb"},
		{"my.example.org", "https://my.example.org/cb"},
		{"example.org.evil.net", "https://example.org.evil.net/cb"},
	} {
		if w := auth(bad[0], bad[1]); w.Code != http.StatusBadRequest {
			t.Errorf("%s → %s: %d %s", bad[0], bad[1], w.Code, w.Header().Get("Location"))
		}
	}
	w = auth("wiki.example.org", "https://wiki.example.org/cb")
	loc, _ := url.Parse(w.Header().Get("Location"))
	code := loc.Query().Get("code")
	if w.Code != http.StatusFound || loc.Host != "wiki.example.org" || code == "" || loc.Query().Get("state") != "s1" {
		t.Fatalf("authorize: %d %s", w.Code, loc)
	}
	tok := func(n int, code, verifier string) (*httptest.ResponseRecorder, map[string]any) {
		w := g.do(n, "POST", "https://my.example.org/login/token", nil, url.Values{"grant_type": {"authorization_code"}, "code": {code},
			"redirect_uri": {"https://wiki.example.org/cb"}, "client_id": {"wiki.example.org"}, "code_verifier": {verifier}})
		var out map[string]any
		json.NewDecoder(w.Body).Decode(&out)
		return w, out
	}
	if w, _ := tok(1, code, strings.Repeat("x", 50)); w.Code != http.StatusBadRequest || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("a wrong verifier: %d, its error readable by a site's page: %q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}
	// A site's page asks from the browser, which may check first.
	if w := g.do(0, "OPTIONS", "https://my.example.org/login/token", nil, nil); w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}
	// That used the code up: a new one, taken at the other web node.
	w = auth("wiki.example.org", "https://wiki.example.org/cb")
	loc, _ = url.Parse(w.Header().Get("Location"))
	w, out := tok(1, loc.Query().Get("code"), verifier)
	if w.Code != http.StatusOK || out["id_token"] == nil {
		t.Fatalf("token: %d %v", w.Code, out)
	}
	// Node 1 signed it; node 0 checks it with the keys it publishes.
	claims, ok := g.relays[0].verify(t.Context(), out["id_token"].(string))
	if !ok || claims["sub"] != "box/anna" || claims["aud"] != "wiki.example.org" || claims["nonce"] != "n1" || claims["email"] != "anna@example.org" {
		t.Fatalf("id token: %v %v", ok, claims)
	}
	w = g.do(0, "GET", "https://my.example.org/login/keys", nil, nil)
	var keys struct{ Keys []JWK }
	json.NewDecoder(w.Body).Decode(&keys)
	if len(keys.Keys) != 2 {
		t.Fatalf("published keys: %s", w.Body)
	}
	if _, ok := tok(0, loc.Query().Get("code"), verifier); ok["id_token"] != nil {
		t.Fatal("a code worked twice")
	}

	r := httptest.NewRequest("GET", "/login/userinfo", nil)
	r.Host = "my.example.org"
	r.Header.Set("Authorization", "Bearer "+out["access_token"].(string))
	uw := httptest.NewRecorder()
	g.relays[0].ServeHTTP(uw, r)
	var who map[string]any
	json.NewDecoder(uw.Body).Decode(&who)
	if uw.Code != http.StatusOK || who["name"] != "anna" {
		t.Fatalf("userinfo: %d %v", uw.Code, who)
	}
	r.Header.Set("Authorization", "Bearer "+out["id_token"].(string))
	uw = httptest.NewRecorder()
	g.relays[0].ServeHTTP(uw, r)
	if uw.Code != http.StatusUnauthorized {
		t.Fatal("userinfo took an ID token as an access token")
	}

	// Not signed in at my.: the sign-in page, which comes back here.
	w = g.do(0, "GET", "https://my.example.org/login/authorize?client_id=wiki.example.org&redirect_uri=https%3A%2F%2Fwiki.example.org%2Fcb&response_type=code&code_challenge="+challenge+"&code_challenge_method=S256", nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sign in") {
		t.Fatalf("authorize signed out: %d", w.Code)
	}
}

// A box checks uploads to people's sites with the web nodes' public keys,
// so a member that isn't a web node may ask for one; nothing else.
func TestLoginKeyForMembers(t *testing.T) {
	g := newLoginGroup(t)
	ask := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		if !g.stores[0].Serve(w, r, "200::77") {
			t.Fatalf("%s: not answered", path)
		}
		return w
	}
	w := ask("/v1/light/login-key")
	var k JWK
	if w.Code != http.StatusOK || json.NewDecoder(w.Body).Decode(&k) != nil || k.Kid == "" {
		t.Fatalf("a box asking for the key: %d", w.Code)
	}
	if keys := LoginKeys(t.Context(), http.DefaultClient, nil); len(keys) != 0 {
		t.Fatal("keys from no web nodes")
	}
	if w := ask("/v1/light/ticket/x"); w.Code == http.StatusOK {
		t.Fatalf("a box got a web node's ticket: %d", w.Code)
	}
}
