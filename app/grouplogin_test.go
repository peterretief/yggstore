package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLogin is the group's login as an app sees it: it hands out a code for
// whoever it's told is signed in, and an ID token for the code.
type fakeLogin struct {
	srv     *httptest.Server
	key     *ecdsa.PrivateKey
	who     map[string]any // the claims for the next sign-in
	aud     string         // set: tokens for another site
	codes   map[string]url.Values
	signKey *ecdsa.PrivateKey // set: tokens signed by someone else
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newFakeLogin(t *testing.T) *fakeLogin {
	f := &fakeLogin{codes: map[string]url.Values{}}
	f.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iss := f.srv.URL
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "authorization_endpoint": iss + "/login/authorize",
				"token_endpoint": iss + "/login/token", "jwks_uri": iss + "/login/keys"})
		case "/login/keys":
			pad := func(n *big.Int) []byte { b := make([]byte, 32); n.FillBytes(b); return b }
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "EC", "crv": "P-256", "kid": "k1", "alg": "ES256", "use": "sig",
				"x": b64(pad(f.key.X)), "y": b64(pad(f.key.Y))}}})
		case "/login/authorize":
			q := r.URL.Query()
			code := randomText()
			f.codes[code] = q
			http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), http.StatusFound)
		case "/login/token":
			r.ParseForm()
			q, ok := f.codes[r.PostFormValue("code")]
			delete(f.codes, r.PostFormValue("code"))
			sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
			if !ok || b64(sum[:]) != q.Get("code_challenge") || r.PostFormValue("client_id") != q.Get("client_id") {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			c := map[string]any{"iss": iss, "aud": q.Get("client_id"), "exp": time.Now().Add(time.Hour).Unix(), "nonce": q.Get("nonce")}
			if f.aud != "" {
				c["aud"] = f.aud
			}
			for k, v := range f.who {
				c[k] = v
			}
			head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "k1", "typ": "JWT"})
			body, _ := json.Marshal(c)
			signing := b64(head) + "." + b64(body)
			h := sha256.Sum256([]byte(signing))
			k := f.key
			if f.signKey != nil {
				k = f.signKey
			}
			rr, ss, _ := ecdsa.Sign(rand.Reader, k, h[:])
			sig := make([]byte, 64)
			rr.FillBytes(sig[:32])
			ss.FillBytes(sig[32:])
			json.NewEncoder(w).Encode(map[string]string{"id_token": signing + "." + b64(sig), "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// signInWithGroup goes through the group sign-in as a browser would, and
// says who the app then takes it to be.
func signInWithGroup(t *testing.T, s *TestSite, f *fakeLogin) (int, string) {
	t.Helper()
	w := s.Do("", "GET", groupLoginPath, nil, "")
	if w.Code != http.StatusFound {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == groupCookie {
			cookie = c
		}
	}
	// The login: straight back, with a code.
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	r := httptest.NewRequest("GET", back.Path+"?"+back.RawQuery, nil)
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		return rec.Code, rec.Body.String()
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName && c.Value != "" {
			r := httptest.NewRequest("GET", "/app/me", nil)
			r.AddCookie(c)
			me := httptest.NewRecorder()
			s.h.ServeHTTP(me, r)
			return rec.Code, me.Body.String()
		}
	}
	return rec.Code, ""
}

func TestGroupLogin(t *testing.T) {
	f := newFakeLogin(t)
	s := NewTestSite(t, "testdata/signs.sql", map[string][2]string{"boss": {"boss@example.org", roleAdmin}, "sam": {"sam@example.org", roleStaff}})
	s.App.site, s.App.secret = "notes.example.org", []byte("secret")
	s.App.group = &groupLogin{issuer: f.srv.URL, role: roleClient, client: f.srv.Client()}
	s.h = s.App.routes()

	if page := s.Get("", "/app/login"); !strings.Contains(page, "Sign in with the group") {
		t.Fatalf("no button:\n%s", page)
	}
	// Someone new in the group comes in as a client.
	f.who = map[string]any{"email": "Kath@Example.org", "email_verified": true, "sub": "box/kath"}
	if code, me := signInWithGroup(t, s, f); code != http.StatusSeeOther || !strings.Contains(me, "kath@example.org") {
		t.Fatalf("kath: %d %s", code, me)
	}
	if role := s.App.accountRole("kath@example.org"); role != roleClient {
		t.Fatalf("kath is %q", role)
	}
	// Staff with an account keep their role.
	f.who = map[string]any{"email": "sam@example.org", "email_verified": true}
	if code, _ := signInWithGroup(t, s, f); code != http.StatusSeeOther || s.App.accountRole("sam@example.org") != roleStaff {
		t.Fatalf("sam: %d %s", code, s.App.accountRole("sam@example.org"))
	}
	// Someone on a box with no name in the group comes in as their local
	// address there, as a client.
	f.who = map[string]any{"sub": "localbox-004/Kath Smith", "name": "Kath Smith"}
	code, me := signInWithGroup(t, s, f)
	if code != http.StatusSeeOther || !strings.Contains(me, "kath-smith@local.4") {
		t.Fatalf("unnamed: %d %s", code, me)
	}
	if s.App.accountRole("kath-smith@local.4") != roleClient {
		t.Fatal("not a client")
	}
	// Nothing to tell who they are: refused.
	f.who = map[string]any{"name": "nobody"}
	if code, _ := signInWithGroup(t, s, f); code != http.StatusForbidden {
		t.Fatalf("no one: %d", code)
	}
	// A token for another site, or not signed by the login, gets nowhere.
	f.who = map[string]any{"email": "eve@example.org", "email_verified": true}
	f.aud = "other.example.org"
	if code, _ := signInWithGroup(t, s, f); code != http.StatusForbidden {
		t.Fatalf("another site's token: %d", code)
	}
	f.aud = ""
	f.signKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if code, _ := signInWithGroup(t, s, f); code != http.StatusForbidden {
		t.Fatalf("a forged token: %d", code)
	}
	if s.App.accountRole("eve@example.org") != "" {
		t.Fatal("eve got an account")
	}
	// A code back without the cookie that started it is refused.
	f.signKey = nil
	if w := s.Do("", "GET", groupBackPath+"?code=x&state=y", nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("no cookie: %d", w.Code)
	}
	// A group sign-in account has no password to sign in with.
	if w := s.Post("", "/app/login", url.Values{"email": {"kath@example.org"}, "password": {""}}); w.Code == http.StatusSeeOther {
		t.Fatal("signed in with no password")
	}
}

func TestGroupRules(t *testing.T) {
	dir := t.TempDir()
	design := filepath.Join(dir, "d.sql")
	writeFile(t, design, `CREATE TABLE faults (
  -- allow: group:sign_technicians read, group:sign_technicians write
  -- step: new -> group:sign_technicians fixes -> fixed
  -- tell: new to group:sign_technicians
  id TEXT PRIMARY KEY,
  what TEXT,
  status TEXT NOT NULL DEFAULT 'new'
);`)
	if p := DesignProblems(t, design); len(p) > 0 {
		t.Fatal(p)
	}
	s := NewTestSite(t, design, map[string][2]string{"boss": {"boss@example.org", roleAdmin},
		"tess": {"tess@example.org", roleClient}, "sam": {"sam@example.org", roleStaff}})
	bag := &mailbag{}
	s.App.data.notify.send = bag.sendFrom
	s.DB().Exec(`INSERT INTO faults (id, what) VALUES ('f1', 'Flicker')`)
	if w := s.Do("tess", "GET", tableLink("faults")+"f1", nil, ""); w.Code != 404 {
		t.Fatalf("tess before the group: %d", w.Code)
	}
	s.Post("boss", "/app/groups", url.Values{"do": {"new"}, "name": {"Sign technicians"}})
	var gid string
	s.DB().QueryRow(`SELECT id FROM _groups`).Scan(&gid)
	s.Post("boss", "/app/groups", url.Values{"do": {"add"}, "group": {gid}, "email": {"tess@example.org"}})
	s.Post("boss", "/app/groups", url.Values{"do": {"add"}, "group": {gid}, "email": {"boss@example.org"}})
	if page := s.Get("tess", tableLink("faults")+"f1"); !strings.Contains(page, `value="fixes">Fix</button>`) {
		t.Fatalf("tess in the group:\n%s", page)
	}
	if w := s.Do("sam", "GET", tableLink("faults")+"f1", nil, ""); w.Code != 404 {
		t.Fatalf("staff outside the group: %d", w.Code)
	}
	if w := s.Post("tess", tableLink("faults")+"new", url.Values{"what": {"Dark"}}); w.Code != 303 {
		t.Fatalf("tess adds: %d", w.Code)
	}
	waitFor(t, "the group told", func() bool { return bag.count() == 1 })
	if to, _, _ := bag.last(); to != "boss@example.org" {
		t.Fatalf("told %q", to)
	}
	if w := s.Post("tess", tableLink("faults")+"f1/step", url.Values{"action": {"fixes"}}); w.Code != 303 {
		t.Fatalf("tess fixes: %d", w.Code)
	}
}

// Groups travel between machines, so a rule naming one means the same on both.
func TestGroupsTravel(t *testing.T) {
	a, b := machine(t, "a"), machine(t, "b")
	if err := a.sync.write(entryGroup, "", "g1", map[string]any{"name": "Sign technicians"}); err != nil {
		t.Fatal(err)
	}
	if err := a.sync.write(entryMember, "", "g1/tess@example.org", map[string]any{"group": "g1", "name": "tess@example.org"}); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	if got := groupsOf(b.db, "tess@example.org"); len(got) != 1 || got[0] != "sign_technicians" {
		t.Fatalf("b: %v", got)
	}
	b.sync.write(entryMember, "", "g1/tess@example.org", map[string]any{"group": "g1", "name": "tess@example.org", "removed": true})
	meet(t, a, b)
	if got := groupsOf(a.db, "tess@example.org"); len(got) != 0 {
		t.Fatalf("a after removing: %v", got)
	}
}
