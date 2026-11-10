package member

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
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

const (
	rpHost = "my.example.org"
	webIP  = "201:9::1" // the group's web node, as the member's node sees it
)

// phone is a fake authenticator with one ES256 passkey.
type phone struct {
	key   *ecdsa.PrivateKey
	id    []byte
	user  []byte
	count uint32
}

func (p *phone) authData(withCred bool) []byte {
	h := sha256.Sum256([]byte(rpHost))
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

func clientJSON(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": challenge, "origin": "https://" + rpHost})
	return b
}

// browser talks to the web node as a browser at my. would.
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

// group is a member's node, and a web node in front of it, as the group
// has them.
type group struct {
	login *Login
	store *light.Store
	relay string
}

func newGroup(t *testing.T) *group {
	dir := t.TempDir()
	g := &group{}
	var list []peers.Peer
	// The member's node API: the web node's requests come from webIP, the
	// member's own (the dashboard, the command line) from the node itself.
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := webIP
		if r.Header.Get("X-Test-Own") == "1" {
			caller = g.login.Self
		}
		if !g.login.Serve(w, r, caller) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(member.Close)
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(member.URL, "http://"))
	list = []peers.Peer{
		{Name: "web", Addr: "[" + webIP + "]:7400", Admin: true, Domain: "example.org"},
		{Name: "desktop", Addr: strings.TrimPrefix(member.URL, "http://"), Owner: "peter", Names: []string{"peter.example.org"}},
	}
	g.store = &light.Store{Dir: filepath.Join(dir, "web"), Peers: func() []peers.Peer { return list }, Log: t.Logf}
	// The web node's node API, where members send their keys.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.store.Serve(w, r, host) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	g.login = &Login{Path: filepath.Join(dir, "member-login.json"), Self: host, Peers: func() []peers.Peer { return list },
		Client: api.Client(), Log: t.Logf,
		WebNodes:  func(context.Context) []string { return []string{strings.TrimPrefix(api.URL, "http://")} },
		IsWebNode: func(_ context.Context, caller string) bool { return caller == webIP },
		IsBox:     func() bool { return false }}
	relay := httptest.NewServer(&light.Relay{Peers: func() []peers.Peer { return list }, Signin: g.store, Log: t.Logf})
	t.Cleanup(relay.Close)
	g.relay = relay.URL
	return g
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("waited too long for " + what)
}

func TestMemberPasskey(t *testing.T) {
	g := newGroup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Before an invite, the node sends the web nodes nothing, and answers
	// them nothing.
	go g.login.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	if g.store.IsMember("desktop") {
		t.Fatal("sent the web nodes a member before any invite")
	}

	link, err := g.login.Invite(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	token := u.Query().Get("t")
	if u.Host != rpHost || u.Path != light.JoinPath || u.Query().Get("box") != "desktop" || token == "" {
		t.Fatalf("invite link %s", link)
	}
	if again, _ := g.login.Invite(time.Now()); again != link {
		t.Fatal("asked again, the invite changed")
	}
	waitFor(t, func() bool { return g.store.IsMember("desktop") }, "the web node to hear of the member")

	// The invite, on the phone, through the web node.
	b := &browser{t: t, relay: g.relay, cookies: map[string]string{}}
	if r := b.do("GET", light.JoinPath+"?box=desktop&t="+token, ""); r.StatusCode != 200 || !strings.Contains(text(r), "Make a passkey") {
		t.Fatalf("invite page: %d", r.StatusCode)
	}
	r := b.do("POST", light.Base+"/begin", `{"t":"`+token+`"}`)
	var a struct {
		Challenge, RP string
		UserID        string `json:"user_id"`
	}
	json.NewDecoder(r.Body).Decode(&a)
	r.Body.Close()
	if r.StatusCode != 200 || a.RP != rpHost {
		t.Fatalf("begin: %d %+v", r.StatusCode, a)
	}
	if r := b.do("POST", light.Base+"/begin", `{"t":"wrong"}`); r.StatusCode != http.StatusForbidden {
		t.Fatalf("begin with a wrong invite: %d", r.StatusCode)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ph := &phone{key: key, id: []byte("peter-phone-1")}
	ph.user, _ = passkey.B64.DecodeString(a.UserID)
	spki, _ := x509.MarshalPKIXPublicKey(&ph.key.PublicKey)
	reg, _ := json.Marshal(map[string]any{"t": token, "name": "my phone", "challenge": a.Challenge, "id": passkey.B64.EncodeToString(ph.id),
		"client_data": passkey.B64.EncodeToString(clientJSON("webauthn.create", a.Challenge)),
		"auth_data":   passkey.B64.EncodeToString(ph.authData(true)), "spki": passkey.B64.EncodeToString(spki), "alg": passkey.ES256})
	if r := b.do("POST", light.Base+"/register", string(reg)); r.StatusCode != 200 || b.cookies[light.SessionCookie] == "" {
		t.Fatalf("register: %d %s", r.StatusCode, text(r))
	}
	if light.BoxOf(ph.user) != "desktop" {
		t.Fatalf("user handle %q", ph.user)
	}
	if r := b.do("POST", light.Base+"/register", string(reg)); r.StatusCode == 200 {
		t.Fatal("the invite worked twice")
	}

	// Signed in: the member's page, from their node.
	if r := b.do("GET", "/files", ""); r.StatusCode != 200 || !strings.Contains(text(r), "signed in to the group") {
		t.Fatalf("the member's page: %d", r.StatusCode)
	}
	if r := b.do("GET", "/files/devices?op=list", ""); !strings.Contains(text(r), `"my phone"`) {
		t.Fatal("devices list")
	}

	// A new browser signs in with the passkey, at the web node.
	b2 := &browser{t: t, relay: g.relay, cookies: map[string]string{}}
	r = b2.do("GET", light.Base+"/challenge", "")
	var ch struct{ Challenge string }
	json.NewDecoder(r.Body).Decode(&ch)
	r.Body.Close()
	ph.count++
	cj, auth := clientJSON("webauthn.get", ch.Challenge), ph.authData(false)
	hc := sha256.Sum256(cj)
	digest := sha256.Sum256(append(append([]byte{}, auth...), hc[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, ph.key, digest[:])
	body, _ := json.Marshal(map[string]string{"challenge": ch.Challenge, "id": passkey.B64.EncodeToString(ph.id),
		"client_data": passkey.B64.EncodeToString(cj), "auth_data": passkey.B64.EncodeToString(auth),
		"signature": passkey.B64.EncodeToString(sig), "user_handle": passkey.B64.EncodeToString(ph.user)})
	if r := b2.do("POST", light.Base+"/signin", string(body)); r.StatusCode != 200 || b2.cookies[light.SessionCookie] == "" {
		t.Fatalf("sign in: %d %s", r.StatusCode, text(r))
	}
	// The member's node hears it was used, and the web nodes get the key.
	waitFor(t, func() bool {
		keys, _ := g.login.Keys()
		return len(keys) == 1 && keys[0].Used != 0
	}, "the key's use")

	// The last passkey can't be forgotten; the member's own calls are only
	// from their own machine.
	if r := b.do("POST", "/files/devices?op=forget&id="+url.QueryEscape(passkey.B64.EncodeToString(ph.id)), ""); r.StatusCode != http.StatusConflict {
		t.Fatalf("forgot the last passkey: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("POST", "http://"+g.login.Peers()[1].Addr+light.MemberBase+"/invite", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a web node made an invite: %d", resp.StatusCode)
	}
	req.Header.Set("X-Test-Own", "1")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Fatalf("the member's own invite: %d", resp.StatusCode)
	}
}

// A box's owner signs in as a person on it: the box's node never says
// it's a member's, which would send its people's requests astray.
func TestNotOnABox(t *testing.T) {
	g := newGroup(t)
	g.login.IsBox = func() bool { return true }
	if _, err := g.login.Invite(time.Now()); err == nil || !strings.Contains(err.Error(), "box") {
		t.Fatalf("an invite on a box: %v", err)
	}
}

func TestOnlyWebNodes(t *testing.T) {
	g := newGroup(t)
	g.login.Invite(time.Now())
	g.login.IsWebNode = func(context.Context, string) bool { return false }
	w := httptest.NewRecorder()
	g.login.Serve(w, httptest.NewRequest("GET", light.MemberBase+light.JoinPath, nil), "300::1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("a member who isn't a web node: %d", w.Code)
	}
	// Not ours: other paths go on to the node's other handlers.
	if g.login.Serve(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/members-other", nil), webIP) {
		t.Fatal("took a path that isn't its own")
	}
}
