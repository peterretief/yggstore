package dashboard

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

// pairNet delivers straight into the other node's engine.
type pairNet struct {
	from    string
	engines map[string]*msg.Engine
}

func (n pairNet) Deliver(ctx context.Context, addr string, m msg.Message) error {
	host, _, _ := net.SplitHostPort(addr)
	return n.engines[host].Receive(n.from, m)
}
func (pairNet) Announce(context.Context, string, []string) error { return nil }
func (pairNet) History(context.Context, string, string, uint64) ([]msg.Message, error) {
	return nil, nil
}

// namePair is an admin dashboard (200::1) and a member's (200::2), with
// messaging between them; extra changes the member's Config.
func namePair(t *testing.T, extra func(*Config)) (ctx context.Context, admin, member *Dashboard,
	do func(*Dashboard, *http.Request) *httptest.ResponseRecorder, names func(*Dashboard) namesState, listOf func(*Dashboard) []peers.Peer) {
	dir := t.TempDir()
	list := []peers.Peer{
		{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org"},
		{Name: "pi", Addr: "[200::2]:7400"},
		{Name: "pi-usb1", Addr: "[200::2]:7401", Host: "pi"}, // a second node on the machine
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	engines := map[string]*msg.Engine{}
	token := strings.Repeat("cd", 24)
	tokenPath := filepath.Join(dir, "msg.token")
	os.WriteFile(tokenPath, []byte(token), 0o600)
	dash := map[string]*Dashboard{}
	for _, ip := range []string{"200::1", "200::2"} {
		sub := filepath.Join(dir, ip)
		os.MkdirAll(sub, 0o700)
		peersPath := filepath.Join(sub, "peers.json")
		peers.Write(peersPath, list)
		e, err := msg.Open(filepath.Join(sub, "msg"), ip, func() []peers.Peer { return list }, pairNet{ip, engines}, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		engines[ip] = e
		go e.Run(ctx)
		api := httptest.NewServer(e.LocalHandler(token))
		t.Cleanup(api.Close)
		id, _ := share.LoadOrCreate(filepath.Join(sub, "me.key"))
		cfg := Config{PeersPath: peersPath, StubDir: sub, SelfID: ip, Listen: "127.0.0.1:7480", Identity: id,
			MsgAPI: api.Listener.Addr().String(), MsgTokenPath: tokenPath, MailDir: filepath.Join(sub, "mail"),
			NamesPath: filepath.Join(sub, "names.json")}
		if ip == "200::2" && extra != nil {
			extra(&cfg)
		}
		dash[ip] = New(cfg)
	}
	admin, member = dash["200::1"], dash["200::2"]
	do = func(d *Dashboard, r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, r)
		return w
	}
	names = func(d *Dashboard) namesState {
		r := httptest.NewRequest(http.MethodGet, "/api/names", nil)
		r.Host = "127.0.0.1:7480"
		var st namesState
		json.Unmarshal(do(d, r).Body.Bytes(), &st)
		return st
	}
	listOf = func(d *Dashboard) []peers.Peer {
		l, err := peers.Load(d.cfg.PeersPath)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	return
}

func TestNameClaimApproved(t *testing.T) {
	ctx, admin, member, do, names, listOf := namePair(t, nil)

	for label, want := range map[string]int{"www": http.StatusBadRequest, "-x": http.StatusBadRequest} {
		if w := do(member, post("/api/names/claim", strings.NewReader(`{"label":"`+label+`"}`))); w.Code != want {
			t.Fatalf("%s: %d %s", label, w.Code, w.Body)
		}
	}
	if w := do(member, post("/api/names/claim", strings.NewReader(`{"label":"Anna"}`))); w.Code != http.StatusNoContent {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	if st := names(member); len(st.Claims) != 1 || st.Claims[0].Status != "asked" || st.Domain != "example.org" {
		t.Fatalf("member sees %+v", st)
	}
	var req nameClaim
	for deadline := time.Now().Add(5 * time.Second); req.Name == ""; {
		admin.syncNames(ctx, listOf(admin))
		if st := names(admin); len(st.Requests) == 1 {
			req = st.Requests[0]
		} else if time.Now().After(deadline) {
			t.Fatalf("the request never reached the admin: %+v", st)
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if req.Name != "anna.example.org" || req.Node != "200::2" || req.Code != member.cfg.Identity.Code() {
		t.Fatalf("request %+v", req)
	}
	// The member can't approve its own request.
	if w := do(member, post("/api/names/decide", strings.NewReader(`{"name":"anna.example.org","node":"200::2","approve":true}`))); w.Code != http.StatusForbidden {
		t.Fatalf("member decided: %d", w.Code)
	}
	if w := do(admin, post("/api/names/decide", strings.NewReader(`{"name":"anna.example.org","node":"200::2","approve":true}`))); w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	got := listOf(admin)
	if p, ok := peers.MailOwner(got, "anna@example.org"); !ok || p.Name != "pi" || p.Code != member.cfg.Identity.Code() {
		t.Fatalf("list after approving: %+v", got)
	}

	// The list reaches the member (the admin's poll pushes it).
	peers.Write(member.cfg.PeersPath, got)
	member.syncNames(ctx, got)
	if st := names(member); len(st.Mine) != 1 || len(st.Claims) != 0 {
		t.Fatalf("member after approval: %+v", st)
	}
	box := &mail.Box{Dir: member.cfg.MailDir}
	if a := box.Address(); a != "anna@example.org" {
		t.Fatalf("mailbox address %q", a)
	}
	if w := do(admin, post("/api/names/claim", strings.NewReader(`{"label":"anna"}`))); w.Code != http.StatusConflict {
		t.Fatalf("claimed a taken name: %d", w.Code)
	}

	// The admin's own claim is given at once, reserved or not.
	if w := do(admin, post("/api/names/claim", strings.NewReader(`{"label":"www"}`))); w.Code != http.StatusNoContent {
		t.Fatalf("admin claim: %d %s", w.Code, w.Body)
	}
	if p, ok := peers.NameOwner(listOf(admin), "www.example.org"); !ok || p.Name != "desktop" || !strings.HasPrefix(p.Code, "ys1") {
		t.Fatalf("admin's own name: %+v %v", p, ok)
	}
	if w := do(admin, post("/api/names/remove", strings.NewReader(`{"name":"anna.example.org"}`))); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if _, ok := peers.NameOwner(listOf(admin), "anna.example.org"); ok {
		t.Fatal("anna's name is still given")
	}
}

func TestSetMe(t *testing.T) {
	dir := t.TempDir()
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Listen: "127.0.0.1:7480", Name: "localbox-002",
		MePath: filepath.Join(dir, "me.name")})
	if d.me() != "localbox-002" {
		t.Fatalf("default %q", d.me())
	}
	for body, want := range map[string]int{`{"name":"  Kath   Smith "}`: http.StatusNoContent, `{"name":"x\" <evil@x>"}`: http.StatusBadRequest, `{"name":""}`: http.StatusBadRequest} {
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, post("/api/me", strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if d.me() != "Kath Smith" {
		t.Fatalf("set %q", d.me())
	}
}

// A box asks for a name for someone on it; the admin approves it onto the
// box's entry with the person's own code, and the box gives it back.
func TestPersonName(t *testing.T) {
	hubDir := t.TempDir()
	statePath := filepath.Join(hubDir, "hub.json")
	ctx, admin, member, do, names, listOf := namePair(t, func(c *Config) { c.HubState, c.HubPeople = statePath, filepath.Join(hubDir, "users") })
	if _, err := hub.Update(ctx, statePath, func(s *hub.HubState) error { _, err := s.AddPerson("Anna", filepath.Join(hubDir, "users")); return err }); err != nil {
		t.Fatal(err)
	}
	if w := do(member, post("/api/box/people/name", strings.NewReader(`{"name":"Anna","label":"anna"}`))); w.Code != http.StatusOK {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	if st := names(member); len(st.Claims) != 0 {
		t.Fatalf("a person's claim shows as the owner's own: %+v", st.Claims)
	}
	var req nameClaim
	for deadline := time.Now().Add(5 * time.Second); req.Name == ""; {
		admin.syncNames(ctx, listOf(admin))
		if st := names(admin); len(st.Requests) == 1 {
			req = st.Requests[0]
		} else if time.Now().After(deadline) {
			t.Fatalf("the request never reached the admin: %+v", st)
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	st, _ := hub.Load(statePath)
	anna, _ := st.PersonNamed("Anna")
	key, _ := hub.PersonKey(statePath, anna)
	if req.Person != "Anna" || req.Code != key.Code() || req.Code == member.cfg.Identity.Code() {
		t.Fatalf("request %+v", req)
	}
	if w := do(admin, post("/api/names/decide", strings.NewReader(`{"name":"anna.example.org","node":"200::2","approve":true}`))); w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	got := listOf(admin)
	if p, holder, ok := peers.MailHolder(got, "anna@example.org"); !ok || p.Name != "pi" || holder.Code != key.Code() || len(p.Names) != 0 {
		t.Fatalf("list after approving: %+v", got)
	}
	if st := names(admin); len(st.Given) != 1 || st.Given[0].Person != "Anna" {
		t.Fatalf("given: %+v", st.Given)
	}

	peers.Write(member.cfg.PeersPath, got)
	member.syncNames(ctx, got)
	r := httptest.NewRequest(http.MethodGet, "/api/box/people", nil)
	r.Host = "127.0.0.1:7480"
	var people struct{ People []boxPerson }
	json.Unmarshal(do(member, r).Body.Bytes(), &people)
	if len(people.People) != 1 || people.People[0].Site != "anna.example.org" || people.People[0].Asked != "" {
		t.Fatalf("box sees %+v", people.People)
	}
	if w := do(member, post("/api/box/people/name", strings.NewReader(`{"name":"Anna","label":"anna2"}`))); w.Code != http.StatusBadRequest {
		t.Fatalf("a second name for Anna: %d", w.Code)
	}

	// Given back (the hub does it when she's removed).
	lc, err := member.msgClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Send(ctx, "200::1", typeNameReturn, `{"name":"anna.example.org"}`); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		admin.syncNames(ctx, listOf(admin))
		if _, ok := peers.NameOwner(listOf(admin), "anna.example.org"); !ok {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the name was never taken back")
		}
	}
}
