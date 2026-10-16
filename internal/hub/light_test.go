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

// browser talks to the relay as a browser at my.example.org would, keeping
// its cookies.
type browser struct {
	t       *testing.T
	relay   string
	cookies map[string]string
}

func (b *browser) do(method, path, body string) *http.Response {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.relay+path, strings.NewReader(body))
	req.Host = rpHost
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

func TestLightClient(t *testing.T) {
	u, _, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	peersPath := filepath.Join(dir, "peers.json")
	list := []peers.Peer{
		{Name: "desktop", Addr: "[" + webYgg + "]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[" + boxYgg + "]:7400"},
	}
	peers.Write(peersPath, list)
	u.cfg.PeersPath = peersPath
	u.cfg.WebNodes = func(context.Context) []string { return []string{"[" + webYgg + "]:7400"} }
	var token string
	st, err := Update(ctx, statePath, func(s *HubState) error {
		p, err := s.AddPerson("anna", u.cfg.Dir)
		if err != nil {
			return err
		}
		token, err = s.Invite(p.Name, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	anna, _ := st.PersonNamed("anna")
	os.MkdirAll(filepath.Join(anna.Path, "files"), 0o755)
	os.WriteFile(filepath.Join(anna.Path, "files", "hers.txt"), []byte("hers"), 0o644)

	// The box, reached over Yggdrasil from the web node.
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr, r.Host = "["+webYgg+"]:51000", "["+boxYgg+"]:7490"
		h.ServeHTTP(w, r)
	}))
	defer box.Close()
	relay := httptest.NewServer(&light.Relay{Peers: func() []peers.Peer { return list }, BoxURL: func(string) string { return box.URL }, Log: t.Logf})
	defer relay.Close()

	// The invite, in a fresh browser.
	b := &browser{t: t, relay: relay.URL, cookies: map[string]string{}}
	if r := b.do("GET", "/files/join?box=box&t="+token, ""); r.StatusCode != 200 || !strings.Contains(text(r), "Make a passkey") || b.cookies[light.BoxCookie] != "box" {
		t.Fatalf("invite page: %d, cookies %v", r.StatusCode, b.cookies)
	}
	r := b.do("POST", light.Base+"/begin", `{"t":"`+token+`"}`)
	var begin struct {
		Challenge, RP string
		UserID        string `json:"user_id"`
	}
	json.NewDecoder(r.Body).Decode(&begin)
	if r.StatusCode != 200 || begin.RP != rpHost {
		t.Fatalf("begin: %d %+v", r.StatusCode, begin)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ph := &phone{key: key, id: []byte("anna-phone-1")}
	ph.user, _ = passkey.B64.DecodeString(begin.UserID)
	if light.BoxOf(ph.user) != "box" {
		t.Fatalf("user handle %q", ph.user)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	reg, _ := json.Marshal(map[string]any{"t": token, "name": "anna's phone", "challenge": begin.Challenge, "id": passkey.B64.EncodeToString(ph.id),
		"client_data": passkey.B64.EncodeToString(clientJSON("webauthn.create", begin.Challenge)),
		"auth_data":   passkey.B64.EncodeToString(ph.authData(true)), "spki": passkey.B64.EncodeToString(spki), "alg": passkey.ES256})
	if r := b.do("POST", light.Base+"/register", string(reg)); r.StatusCode != 200 || b.cookies[light.SessionCookie] == "" {
		t.Fatalf("register: %d %s", r.StatusCode, text(r))
	}
	if r := b.do("POST", light.Base+"/register", string(reg)); r.StatusCode == 200 {
		t.Fatal("the invite worked twice")
	}

	// Signed in: Anna's own files.
	r = b.do("POST", "/files?op=list", `{"path":""}`)
	if body := text(r); r.StatusCode != 200 || !strings.Contains(body, "hers.txt") {
		t.Fatalf("list: %d %s", r.StatusCode, body)
	}
	if r := b.do("GET", "/files/devices?op=list", ""); !strings.Contains(text(r), "anna's phone") {
		t.Fatal("devices list")
	}

	// A new browser signs in with the passkey: the relay learns the box
	// from it.
	b2 := &browser{t: t, relay: relay.URL, cookies: map[string]string{}}
	if r := b2.do("GET", "/", ""); r.StatusCode != 200 || !strings.Contains(text(r), "Sign in") {
		t.Fatal("no sign-in page for a new browser")
	}
	r = b2.do("GET", light.Base+"/challenge", "")
	var ch struct{ Challenge, RP string }
	json.NewDecoder(r.Body).Decode(&ch)
	if ch.RP != rpHost {
		t.Fatalf("challenge %+v", ch)
	}
	signIn := func(b *browser, challenge string) *http.Response {
		ph.count++
		cj, auth := clientJSON("webauthn.get", challenge), ph.authData(false)
		hc := sha256.Sum256(cj)
		digest := sha256.Sum256(append(append([]byte{}, auth...), hc[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		body, _ := json.Marshal(map[string]string{"challenge": challenge, "id": passkey.B64.EncodeToString(ph.id),
			"client_data": passkey.B64.EncodeToString(cj), "auth_data": passkey.B64.EncodeToString(auth),
			"signature": passkey.B64.EncodeToString(sig), "user_handle": passkey.B64.EncodeToString(ph.user)})
		return b.do("POST", light.Base+"/signin", string(body))
	}
	if r := signIn(b2, ch.Challenge); r.StatusCode != 200 || b2.cookies[light.BoxCookie] != "box" || b2.cookies[light.SessionCookie] == "" {
		t.Fatalf("sign in: %d %s %v", r.StatusCode, text(r), b2.cookies)
	}
	if r := b2.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != 200 {
		t.Fatalf("list after signing in: %d", r.StatusCode)
	}
	b3 := &browser{t: t, relay: relay.URL, cookies: map[string]string{}}
	if r := signIn(b3, ch.Challenge); r.StatusCode == 200 {
		t.Fatal("a used challenge signed in again")
	}

	// Signed out, or never in: the sign-in page, and nothing else.
	b2.do("POST", light.Base+"/signout", "")
	if r := b2.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list after signing out: %d", r.StatusCode)
	}
	if r := b2.do("GET", "/files", ""); !strings.Contains(text(r), "Sign in") {
		t.Fatal("no sign-in page after signing out")
	}

	// Web access off: the session no longer works.
	Update(ctx, statePath, func(s *HubState) error { s.People[s.person("anna")].WebOff = true; return nil })
	if r := b.do("POST", "/files?op=list", `{"path":""}`); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list with web access off: %d", r.StatusCode)
	}

	// Only web nodes may relay: the header from anywhere else counts for
	// nothing.
	rec := filesReq(h, "POST", "/files?op=list", benYgg, `{}`, map[string]string{"X-Yggstore": "1", light.RelayHeader: "1",
		"X-Forwarded-Host": rpHost, "Cookie": light.SessionCookie + "=" + b.cookies[light.SessionCookie]})
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "hers") {
		t.Fatalf("a relay header from a member's device: %d %s", rec.Code, rec.Body)
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
	u, _, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, []peers.Peer{
		{Name: "desktop", Addr: "[" + webYgg + "]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[" + boxYgg + "]:7400", PeopleNames: []peers.PersonName{{Name: "anna.example.org", Person: "anna", Code: "ys1annaannaanna"}}},
	})
	u.cfg.PeersPath = peersPath
	u.cfg.WebNodes = func(context.Context) []string { return []string{"[" + webYgg + "]:7400"} }
	tokens := map[string]string{}
	Update(ctx, statePath, func(s *HubState) error {
		for _, who := range []string{"anna", "ben"} {
			s.AddPerson(who, u.cfg.Dir)
			tokens[who], _ = s.Invite(who, time.Now())
		}
		return nil
	})
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()
	cookies := map[string]string{}
	do := func(host, path, body string) *httptest.ResponseRecorder {
		hdr := map[string]string{"X-Yggstore": "1", light.RelayHeader: "1", "X-Forwarded-Host": host}
		var c []string
		for k, v := range cookies {
			c = append(c, k+"="+v)
		}
		if len(c) > 0 {
			hdr["Cookie"] = strings.Join(c, "; ")
		}
		rec := filesReq(h, "POST", path, webYgg, body, hdr)
		for _, ck := range rec.Result().Cookies() {
			cookies[ck.Name] = ck.Value
		}
		return rec
	}
	begin := func(host, token string) (string, int) {
		rec := do(host, light.Base+"/begin", `{"t":"`+token+`"}`)
		var b struct{ Challenge string }
		json.Unmarshal(rec.Body.Bytes(), &b)
		return b.Challenge, rec.Code
	}
	if _, code := begin("anna.example.org", tokens["ben"]); code != http.StatusForbidden {
		t.Fatalf("ben's invite at anna's address: %d", code)
	}
	if _, code := begin("evil.example.org", tokens["anna"]); code != http.StatusForbidden {
		t.Fatalf("an address that isn't anyone's: %d", code)
	}
	ch, code := begin("anna.example.org", tokens["anna"])
	if code != 200 {
		t.Fatalf("anna's invite at her address: %d", code)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ph := &phone{rp: "anna.example.org", key: key, id: []byte("anna-key")}
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	reg, _ := json.Marshal(map[string]any{"t": tokens["anna"], "name": "phone", "challenge": ch, "id": passkey.B64.EncodeToString(ph.id),
		"client_data": passkey.B64.EncodeToString(clientJSONAt("webauthn.create", ch, "anna.example.org")),
		"auth_data":   passkey.B64.EncodeToString(ph.authData(true)), "spki": passkey.B64.EncodeToString(spki), "alg": passkey.ES256})
	if rec := do("anna.example.org", light.Base+"/register", string(reg)); rec.Code != 200 {
		t.Fatalf("register at her address: %d %s", rec.Code, rec.Body)
	}
	if rec := do("anna.example.org", "/files?op=list", `{"path":""}`); rec.Code != 200 {
		t.Fatalf("her files at her address: %d %s", rec.Code, rec.Body)
	}
	// The same session at the shared address counts for nothing.
	if rec := do(rpHost, "/files?op=list", `{"path":""}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("her session at the shared address: %d", rec.Code)
	}
	// And her passkey doesn't sign in there either.
	ch2 := passkey.NewChallenge(time.Now())
	ph.count++
	cj, auth := clientJSONAt("webauthn.get", ch2, rpHost), ph.authData(false)
	hc := sha256.Sum256(cj)
	digest := sha256.Sum256(append(append([]byte{}, auth...), hc[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	body, _ := json.Marshal(map[string]string{"challenge": ch2, "id": passkey.B64.EncodeToString(ph.id),
		"client_data": passkey.B64.EncodeToString(cj), "auth_data": passkey.B64.EncodeToString(auth), "signature": passkey.B64.EncodeToString(sig)})
	if rec := do(rpHost, light.Base+"/signin", string(body)); rec.Code == 200 {
		t.Fatal("her passkey signed in at the shared address")
	}
}
