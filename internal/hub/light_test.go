package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

const (
	webYgg = "201:9::1" // the group's web node
	rpHost = "my.example.org"
)

// phone is a fake authenticator with one ES256 passkey.
type phone struct {
	rp    string // rpHost if ""
	key   *ecdsa.PrivateKey
	id    []byte
	user  []byte
	count uint32
}

func (p *phone) authData(withCred bool) []byte {
	rp := p.rp
	if rp == "" {
		rp = rpHost
	}
	h := sha256.Sum256([]byte(rp))
	flags := byte(0x05)
	if withCred {
		flags |= 0x40
	}
	b := append(h[:], flags)
	b = binary.BigEndian.AppendUint32(b, p.count)
	if withCred {
		b = append(b, make([]byte, 16)...)
		b = binary.BigEndian.AppendUint16(b, uint16(len(p.id)))
		b = append(b, p.id...)
		b = append(b, 0xa0)
	}
	return b
}

func clientJSON(typ, challenge string) []byte { return clientJSONAt(typ, challenge, rpHost) }

func clientJSONAt(typ, challenge, host string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": challenge, "origin": "https://" + host})
	return b
}

// browser talks to the relay as a browser at host (rpHost if "") would,
// keeping its cookies.
type browser struct {
	t       *testing.T
	relay   string
	host    string
	cookies map[string]string
}

func (b *browser) do(method, path, body string) *http.Response {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.relay+path, strings.NewReader(body))
	req.Host = rpHost
	if b.host != "" {
		req.Host = b.host
	}
	req.Header.Set("X-Yggstore", "1")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range b.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return resp
}

func text(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	return string(b)
}

// lightGroup is a box and a web node in front of it, as the group has them.
type lightGroup struct {
	u         *Users
	statePath string
	store     *light.Store
	box       *httptest.Server
	relay     string
}

func newLightGroup(t *testing.T, names ...peers.PersonName) *lightGroup {
	u, _, dir := setupUsers(t, 0)
	g := &lightGroup{u: u, statePath: filepath.Join(dir, "hub.json")}
	list := []peers.Peer{
		{Name: "desktop", Addr: "[" + webYgg + "]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[" + boxYgg + "]:7400", PeopleNames: names},
	}
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, list)
	u.cfg.PeersPath = peersPath
	u.cfg.WebNodes = func(context.Context) []string { return []string{"[" + webYgg + "]:7400"} }
	// The box, reached over Yggdrasil from the web node.
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()
	g.box = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr, r.Host = "["+webYgg+"]:51000", "["+boxYgg+"]:7490"
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.box.Close)
	g.store = &light.Store{Dir: filepath.Join(dir, "web-light"), Peers: func() []peers.Peer { return list }, Log: t.Logf}
	relay := httptest.NewServer(&light.Relay{Peers: func() []peers.Peer { return list }, Signin: g.store,
		BoxURL: func(string) string { return g.box.URL }, Log: t.Logf})
	t.Cleanup(relay.Close)
	g.relay = relay.URL
	return g
}

// push sends the web node the box's people, as pushLight does.
func (g *lightGroup) push(t *testing.T) {
	st, err := Load(g.statePath)
	if err != nil {
		t.Fatal(err)
	}
	g.store.SetPeople("box", st.LightPeople().People)
}

func (g *lightGroup) browser(t *testing.T, host string) *browser {
	return &browser{t: t, relay: g.relay, host: host, cookies: map[string]string{}}
}

func (g *lightGroup) invite(t *testing.T, who string) string {
	var token string
	_, err := Update(context.Background(), g.statePath, func(s *HubState) error {
		if _, ok := s.PersonNamed(who); !ok {
			if _, err := s.AddPerson(who, g.u.cfg.Dir); err != nil {
				return err
			}
		}
		var err error
		token, err = s.Invite(who, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type beginAnswer struct {
	Challenge, RP string
	UserID        string `json:"user_id"`
}

func begin(t *testing.T, b *browser, path, token string) (beginAnswer, int) {
	if path != "" {
		b.do("GET", path, "")
	}
	r := b.do("POST", light.Base+"/begin", `{"t":"`+token+`"}`)
	var a beginAnswer
	if r.StatusCode == 200 {
		json.NewDecoder(r.Body).Decode(&a)
	}
	r.Body.Close()
	return a, r.StatusCode
}

// makePasskey makes ph's passkey with an invite.
func makePasskey(b *browser, ph *phone, token string, a beginAnswer) *http.Response {
	host := b.host
	if host == "" {
		host = rpHost
	}
	ph.user, _ = passkey.B64.DecodeString(a.UserID)
	spki, _ := x509.MarshalPKIXPublicKey(&ph.key.PublicKey)
	reg, _ := json.Marshal(map[string]any{"t": token, "name": "phone", "challenge": a.Challenge, "id": passkey.B64.EncodeToString(ph.id),
		"client_data": passkey.B64.EncodeToString(clientJSONAt("webauthn.create", a.Challenge, host)),
		"auth_data":   passkey.B64.EncodeToString(ph.authData(true)), "spki": passkey.B64.EncodeToString(spki), "alg": passkey.ES256})
	return b.do("POST", light.Base+"/register", string(reg))
}

// signInPasskey signs in with ph's passkey at b's host.
func signInPasskey(b *browser, ph *phone) *http.Response {
	host := b.host
	if host == "" {
		host = rpHost
	}
	r := b.do("GET", light.Base+"/challenge", "")
	var ch struct{ Challenge string }
	json.NewDecoder(r.Body).Decode(&ch)
	r.Body.Close()
	return signInWith(b, ph, ch.Challenge, host)
}

func signInWith(b *browser, ph *phone, challenge, host string) *http.Response {
	ph.count++
	cj, auth := clientJSONAt("webauthn.get", challenge, host), ph.authData(false)
	hc := sha256.Sum256(cj)
	digest := sha256.Sum256(append(append([]byte{}, auth...), hc[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, ph.key, digest[:])
	body, _ := json.Marshal(map[string]string{"challenge": challenge, "id": passkey.B64.EncodeToString(ph.id),
		"client_data": passkey.B64.EncodeToString(cj), "auth_data": passkey.B64.EncodeToString(auth),
		"signature": passkey.B64.EncodeToString(sig), "user_handle": passkey.B64.EncodeToString(ph.user)})
	return b.do("POST", light.Base+"/signin", string(body))
}

func TestLightClient(t *testing.T) {
	g := newLightGroup(t)
	token := g.invite(t, "anna")
	st, _ := Load(g.statePath)
	anna, _ := st.PersonNamed("anna")
	os.MkdirAll(filepath.Join(anna.Path, "files"), 0o755)
	os.WriteFile(filepath.Join(anna.Path, "files", "hers.txt"), []byte("hers"), 0o644)

	// The invite, in a fresh browser.
	b := g.browser(t, "")
	if r := b.do("GET", "/files/join?box=box&t="+token, ""); r.StatusCode != 200 || !strings.Contains(text(r), "Make a passkey") || b.cookies[light.BoxCookie] != "box" {
		t.Fatalf("invite page: %d, cookies %v", r.StatusCode, b.cookies)
	}
	a, code := begin(t, b, "", token)
	if code != 200 || a.RP != rpHost {
		t.Fatalf("begin: %d %+v", code, a)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ph := &phone{key: key, id: []byte("anna-phone-1")}
	if r := makePasskey(b, ph, token, a); r.StatusCode != 200 || b.cookies[light.SessionCookie] == "" {
		t.Fatalf("register: %d %s", r.StatusCode, text(r))
	}
	if light.BoxOf(ph.user) != "box" {
		t.Fatalf("user handle %q", ph.user)
	}
	if r := makePasskey(b, ph, token, a); r.StatusCode == 200 {
		t.Fatal("the invite worked twice")
	}

	// Signed in: Anna's own files.
	r := b.do("POST", "/files?op=list", `{"path":""}`)
	if body := text(r); r.StatusCode != 200 || !strings.Contains(body, "hers.txt") {
		t.Fatalf("list: %d %s", r.StatusCode, body)
	}
	if r := b.do("GET", "/files/devices?op=list", ""); !strings.Contains(text(r), `"phone"`) {
		t.Fatal("devices list")
	}

	// A new browser signs in with the passkey: the web node checks it.
	b2 := g.browser(t, "")
	if r := b2.do("GET", "/", ""); r.StatusCode != 200 || !strings.Contains(text(r), "Sign in") {
		t.Fatal("no sign-in page for a new browser")
	}
	r = b2.do("GET", light.Base+"/challenge", "")
	var ch struct{ Challenge, RP string }
	json.NewDecoder(r.Body).Decode(&ch)
	r.Body.Close()
	if ch.RP != rpHost {
		t.Fatalf("challenge %+v", ch)
	}
	if r := signInWith(b2, ph, ch.Challenge, rpHost); r.StatusCode != 200 || b2.cookies[light.SessionCookie] == "" {
		t.Fatalf("sign in: %d %s %v", r.StatusCode, text(r), b2.cookies)
	}
	if r := b2.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != 200 {
		t.Fatalf("list after signing in: %d", r.StatusCode)
	}
	if r := signInWith(g.browser(t, ""), ph, ch.Challenge, rpHost); r.StatusCode == 200 {
		t.Fatal("a used challenge signed in again")
	}

	// With the box off, signing in still works; the files wait for it.
	on := g.box.Config.Handler
	g.box.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	})
	b4 := g.browser(t, "")
	if r := signInPasskey(b4, ph); r.StatusCode != 200 {
		t.Fatalf("sign in with the box off: %d %s", r.StatusCode, text(r))
	}
	if r := b4.do("GET", "/files", ""); r.StatusCode != http.StatusBadGateway {
		t.Fatalf("files with the box off: %d", r.StatusCode)
	}
	if r := b4.do("GET", "/files/devices?op=list", ""); r.StatusCode != 200 || !strings.Contains(text(r), `"phone"`) {
		t.Fatal("the devices list with the box off")
	}
	g.box.Config.Handler = on

	// Signed out, or never in: the sign-in page, and nothing else.
	b2.do("POST", light.Base+"/signout", "")
	if r := b2.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list after signing out: %d", r.StatusCode)
	}
	if r := b2.do("GET", "/files", ""); !strings.Contains(text(r), "Sign in") {
		t.Fatal("no sign-in page after signing out")
	}

	// Web access off: once the box tells the web node, the session ends.
	Update(context.Background(), g.statePath, func(s *HubState) error { s.People[s.person("anna")].WebOff = true; return nil })
	g.push(t)
	if r := b.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list with web access off: %d", r.StatusCode)
	}
	if r := signInPasskey(g.browser(t, ""), ph); r.StatusCode == 200 {
		t.Fatal("signed in with web access off")
	}

	// Only web nodes may name the person: the headers from anywhere else
	// count for nothing.
	Update(context.Background(), g.statePath, func(s *HubState) error { s.People[s.person("anna")].WebOff = false; return nil })
	h := (&Hub{cfg: Config{Files: g.u.FilesHandler(boxYgg)}}).Handler()
	rec := filesReq(h, "POST", "/files?op=list", benYgg, `{}`, map[string]string{"X-Yggstore": "1", light.RelayHeader: "1",
		"X-Forwarded-Host": rpHost, light.PersonHeader: "anna"})
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "hers") {
		t.Fatalf("a relay header from a member's device: %d %s", rec.Code, rec.Body)
	}
	// And a browser can't name them through the web node.
	b5 := g.browser(t, "")
	b5.cookies[light.BoxCookie] = "box"
	req, _ := http.NewRequest("POST", g.relay+"/files?op=list", strings.NewReader(`{"path":""}`))
	req.Host = rpHost
	req.Header.Set("X-Yggstore", "1")
	req.Header.Set(light.PersonHeader, "anna")
	req.AddCookie(&http.Cookie{Name: light.BoxCookie, Value: "box"})
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a browser naming the person: %v %v", resp, err)
	}
}

// The box sends the web nodes its people when they change.
func TestPushLight(t *testing.T) {
	u, _, dir := setupUsers(t, 0)
	got := make(chan light.People, 8)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ps light.People
		if r.Method != http.MethodPut || r.URL.Path != "/v1/light/people" || json.NewDecoder(r.Body).Decode(&ps) != nil {
			t.Errorf("%s %s", r.Method, r.URL)
		}
		got <- ps
		w.WriteHeader(http.StatusNoContent)
	}))
	defer node.Close()
	u.cfg.WebNodes = func(context.Context) []string { return []string{strings.TrimPrefix(node.URL, "http://")} }
	u.cfg.Client.HTTP = http.DefaultClient
	statePath := filepath.Join(dir, "hub.json")
	Update(context.Background(), statePath, func(s *HubState) error {
		s.AddPerson("anna", u.cfg.Dir)
		s.People[s.person("anna")].Passkeys = []Passkey{{Key: passkey.Key{ID: "k1"}, Name: "phone", Kind: KindBrowser, PIN: "pbkdf2-sha256$x$y"}}
		s.AddPerson("ben", u.cfg.Dir) // no key: not sent
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.pushLight(ctx)
	wait := func() light.People {
		select {
		case ps := <-got:
			return ps
		case <-time.After(5 * time.Second):
			t.Fatal("the web node wasn't sent the box's people")
		}
		return light.People{}
	}
	if ps := wait(); len(ps.People) != 1 || ps.People[0].Name != "anna" || !ps.People[0].Web || ps.People[0].Keys[0].PIN == "" {
		t.Fatalf("sent %+v", ps)
	}
	time.Sleep(10 * time.Millisecond) // a new modification time
	Update(context.Background(), statePath, func(s *HubState) error { return s.Suspend("anna", true) })
	u.lightChanged()
	if ps := wait(); len(ps.People) != 1 || ps.People[0].Web {
		t.Fatalf("after suspending: %+v", ps)
	}
}

// Someone who only signs in on the web still has their files stored.
func TestLightClientStored(t *testing.T) {
	u, _, dir := setupUsers(t, 0)
	watched := make(chan string, 4)
	u.cfg.Watch = func(ctx context.Context, p Person) { watched <- p.Name; <-ctx.Done() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Update(ctx, filepath.Join(dir, "hub.json"), func(s *HubState) error {
		s.AddPerson("anna", u.cfg.Dir)
		s.People[s.person("anna")].Passkeys = []Passkey{{Name: "phone"}}
		return nil
	})
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case who := <-watched:
		if who != "anna" {
			t.Fatalf("watching %s", who)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a web-only person's files aren't stored")
	}
}

// Someone with a name signs in at their own address, and only they can.
func TestLightOwnAddress(t *testing.T) {
	g := newLightGroup(t, peers.PersonName{Name: "anna.example.org", Person: "anna", Code: "ys1annaannaanna"})
	tokens := map[string]string{"anna": g.invite(t, "anna"), "ben": g.invite(t, "ben")}
	at := func(host string) *browser { return g.browser(t, host) }
	if _, code := begin(t, at("anna.example.org"), "", tokens["ben"]); code != http.StatusForbidden {
		t.Fatalf("ben's invite at anna's address: %d", code)
	}
	if _, code := begin(t, at("evil.example.org"), "", tokens["anna"]); code == 200 {
		t.Fatalf("an address that isn't anyone's: %d", code)
	}
	b := at("anna.example.org")
	a, code := begin(t, b, "", tokens["anna"])
	if code != 200 || a.RP != "anna.example.org" {
		t.Fatalf("anna's invite at her address: %d %+v", code, a)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ph := &phone{rp: "anna.example.org", key: key, id: []byte("anna-key")}
	if r := makePasskey(b, ph, tokens["anna"], a); r.StatusCode != 200 {
		t.Fatalf("register at her address: %d %s", r.StatusCode, text(r))
	}
	if r := b.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != 200 {
		t.Fatalf("her files at her address: %d %s", r.StatusCode, text(r))
	}
	// The same session at the shared address counts for nothing.
	shared := at(rpHost)
	shared.cookies = b.cookies
	if r := shared.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("her session at the shared address: %d", r.StatusCode)
	}
	// And her passkey doesn't sign in there either.
	if r := signInWith(at(rpHost), ph, passkey.NewChallenge(time.Now()), rpHost); r.StatusCode == 200 {
		t.Fatal("her passkey signed in at the shared address")
	}
	// Signing in again at her address works.
	if r := signInPasskey(at("anna.example.org"), ph); r.StatusCode != 200 {
		t.Fatalf("signing in at her address: %d %s", r.StatusCode, text(r))
	}
}

// A device that can't make a passkey keeps a key in the browser instead,
// with a PIN the web node checks and counts.
func TestLightBrowserKey(t *testing.T) {
	g := newLightGroup(t)
	token := g.invite(t, "kath")
	b := g.browser(t, "")
	a, _ := begin(t, b, "/files/join?box=box&t="+token, token)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sign := func(ch string) string {
		hh := sha256.Sum256(passkey.BrowserMessage("https://"+rpHost, ch))
		r, s, _ := ecdsa.Sign(rand.Reader, key, hh[:])
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return passkey.B64.EncodeToString(sig)
	}
	id := "browser-key-id-0001"
	register := func(pin string) *http.Response {
		reg, _ := json.Marshal(map[string]any{"kind": KindBrowser, "t": token, "name": "kath's phone", "challenge": a.Challenge, "id": id,
			"spki": passkey.B64.EncodeToString(spki), "signature": sign(a.Challenge), "pin": pin})
		return b.do("POST", light.Base+"/register", string(reg))
	}
	if r := register("111111"); r.StatusCode == 200 {
		t.Fatal("registered with a PIN that's too easy")
	}
	a, _ = begin(t, b, "", token)
	if r := register("482913"); r.StatusCode != 200 || b.cookies[light.SessionCookie] == "" {
		t.Fatalf("register a browser key: %d %s", r.StatusCode, text(r))
	}
	st, _ := Load(g.statePath)
	if p, _ := st.PersonNamed("kath"); len(p.Passkeys) != 1 || p.Passkeys[0].Kind != KindBrowser || !passkey.PINMatches(p.Passkeys[0].PIN, "482913") {
		t.Fatalf("kept %+v", p.Passkeys)
	}
	if r := b.do("GET", "/files/devices?op=list", ""); strings.Contains(text(r), "pbkdf2") {
		t.Fatal("the devices list shows the PIN's hash")
	}
	g.push(t)

	pin := "482913"
	b2 := g.browser(t, "")
	signin := func(kind, sig string, extra map[string]string) *http.Response {
		ch := passkey.NewChallenge(time.Now())
		if sig == "" {
			sig = sign(ch)
		}
		body := map[string]string{"kind": kind, "challenge": ch, "id": id, "signature": sig, "pin": pin}
		for k, v := range extra {
			body[k] = v
		}
		bb, _ := json.Marshal(body)
		delete(b2.cookies, light.SessionCookie)
		return b2.do("POST", light.Base+"/signin", string(bb))
	}
	if r := signin("", "", nil); r.StatusCode == 200 {
		t.Fatal("a browser key signed in as a passkey")
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ch := passkey.NewChallenge(time.Now())
	hh := sha256.Sum256(passkey.BrowserMessage("https://"+rpHost, ch))
	r1, s1, _ := ecdsa.Sign(rand.Reader, other, hh[:])
	bad := make([]byte, 64)
	r1.FillBytes(bad[:32])
	s1.FillBytes(bad[32:])
	if r := signin(KindBrowser, passkey.B64.EncodeToString(bad), nil); r.StatusCode == 200 {
		t.Fatal("another key signed in")
	}
	// Wrong PINs are counted, and the box hears of them; a right one
	// starts again.
	pin = "000001"
	for i := 0; i < passkey.MaxPINFails-1; i++ {
		if r := signin(KindBrowser, "", nil); r.StatusCode != 403 || b2.cookies[light.SessionCookie] != "" {
			t.Fatalf("wrong PIN %d: %d %s", i, r.StatusCode, text(r))
		}
	}
	waitFor(t, func() bool {
		st, _ := Load(g.statePath)
		p, _ := st.PersonNamed("kath")
		return p.Passkeys[0].PINFails == passkey.MaxPINFails-1
	}, "the box to hear of the wrong PINs")
	pin = "482913"
	if r := signin(KindBrowser, "", nil); r.StatusCode != 200 || b2.cookies[light.SessionCookie] == "" {
		t.Fatalf("sign in with the browser key: %d %s", r.StatusCode, text(r))
	}
	if r := b2.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != 200 {
		t.Fatalf("files after signing in: %d", r.StatusCode)
	}
	if r := b2.do("GET", "/files/devices?op=list", ""); !strings.Contains(text(r), `"short":true`) {
		t.Fatal("a browser key's session isn't short")
	}
	// MaxPINFails wrong PINs in a row lock the key, even to the right PIN,
	// and even once the box has said it again.
	pin = "000001"
	for i := 0; i < passkey.MaxPINFails; i++ {
		signin(KindBrowser, "", nil)
	}
	waitFor(t, func() bool {
		st, _ := Load(g.statePath)
		p, _ := st.PersonNamed("kath")
		return p.Passkeys[0].PINFails == passkey.MaxPINFails
	}, "the box to hear of the lock")
	g.push(t)
	pin = "482913"
	if r := signin(KindBrowser, "", nil); r.StatusCode != 403 || !strings.Contains(text(r), "locked") {
		t.Fatalf("a locked key: %d", r.StatusCode)
	}
	// A key made before PINs: the web node asks for one to be chosen, and
	// the box keeps it.
	Update(context.Background(), g.statePath, func(s *HubState) error {
		k := &s.People[s.person("kath")].Passkeys[0]
		k.PIN, k.PINFails = "", 0
		return nil
	})
	g.push(t)
	if r := signin(KindBrowser, "", nil); r.StatusCode != http.StatusPreconditionRequired || b2.cookies[light.SessionCookie] != "" {
		t.Fatalf("no PIN yet: %d %s", r.StatusCode, text(r))
	}
	if r := signin(KindBrowser, "", map[string]string{"new_pin": "735102"}); r.StatusCode != 200 || b2.cookies[light.SessionCookie] == "" {
		t.Fatalf("choose a PIN: %d %s", r.StatusCode, text(r))
	}
	st, _ = Load(g.statePath)
	if p, _ := st.PersonNamed("kath"); !passkey.PINMatches(p.Passkeys[0].PIN, "735102") {
		t.Fatal("the box didn't keep the chosen PIN")
	}
	pin = "735102"
	if r := signin(KindBrowser, "", nil); r.StatusCode != 200 {
		t.Fatalf("sign in with the chosen PIN: %d %s", r.StatusCode, text(r))
	}
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %s", what)
}
