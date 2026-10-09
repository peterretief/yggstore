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

func TestNameClaimApproved(t *testing.T) {
	dir := t.TempDir()
	list := []peers.Peer{
		{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org"},
		{Name: "pi", Addr: "[200::2]:7400"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		defer api.Close()
		id, _ := share.LoadOrCreate(filepath.Join(sub, "me.key"))
		dash[ip] = New(Config{PeersPath: peersPath, StubDir: sub, SelfID: ip, Listen: "127.0.0.1:7480", Identity: id,
			MsgAPI: api.Listener.Addr().String(), MsgTokenPath: tokenPath, MailDir: filepath.Join(sub, "mail"),
			NamesPath: filepath.Join(sub, "names.json")})
	}
	admin, member := dash["200::1"], dash["200::2"]
	do := func(d *Dashboard, r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, r)
		return w
	}
	names := func(d *Dashboard) namesState {
		r := httptest.NewRequest(http.MethodGet, "/api/names", nil)
		r.Host = "127.0.0.1:7480"
		var st namesState
		json.Unmarshal(do(d, r).Body.Bytes(), &st)
		return st
	}
	listOf := func(d *Dashboard) []peers.Peer {
		l, err := peers.Load(d.cfg.PeersPath)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

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
