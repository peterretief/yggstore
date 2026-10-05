package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestParseNodeEntry(t *testing.T) {
	out := "node ID: 200:aa::1\npeers.json entry: {\"name\": \"laptop\", \"addr\": \"[200:aa::1]:7400\", \"admin\": true}\n"
	p, err := parseNodeEntry(out)
	if err != nil || p.Name != "laptop" || p.Addr != "[200:aa::1]:7400" || p.Admin {
		t.Fatalf("got %+v, %v; want laptop at [200:aa::1]:7400, not admin", p, err)
	}
	for _, bad := range []string{"", "hello", `{"name": "x", "addr": "10.0.0.1:7400"}`, `{"name": "a b", "addr": "[200::1]:7400"}`, `{"name": "x", "addr": "[fd00::1]:7400"}`} {
		if _, err := parseNodeEntry(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// TestAddNodeReachesEveryNode: a node added on the dashboard is sent to the
// nodes, which then accept the new one.
func TestAddNodeReachesEveryNode(t *testing.T) {
	dir := t.TempDir()
	// The loopback transport sees every caller, this dashboard included, as 127.0.0.1.
	nodeBase := filepath.Join(dir, "node-peers.json")
	admin := peers.Peer{Name: "desktop", Addr: "127.0.0.1:1", Admin: true}
	peers.Write(nodeBase, []peers.Peer{admin})
	live, err := peers.NewLive(nodeBase, filepath.Join(dir, "node-pushed.json"), "200::9")
	if err != nil {
		t.Fatal(err)
	}
	node := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{Transport: transport.Loopback{}, Peers: live}))
	defer node.Close()

	mine := filepath.Join(dir, "peers.json")
	peers.Write(mine, []peers.Peer{admin, {Name: "pi", Addr: strings.TrimPrefix(node.URL, "http://")}})
	d := New(Config{PeersPath: mine, StubDir: dir, SelfID: "127.0.0.1", Client: client.New()})
	d.poll(context.Background())
	if want, _ := peers.Load(mine); live.Hash() != peers.Hash(want) {
		t.Fatal("node was not sent the list")
	}

	h := d.Handler()
	req := post("/api/nodes", strings.NewReader(`{"text": "peers.json entry: {\"name\": \"laptop\", \"addr\": \"[200:aa::1]:7400\"}", "host": "home", "slow": true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("add node: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, post("/api/nodes", strings.NewReader(`{"text": "{\"name\": \"laptop\", \"addr\": \"[200:aa::2]:7400\"}"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name: %d, want 409", rec.Code)
	}

	d.pushed = map[string]time.Time{} // skip the once-a-minute wait
	d.poll(context.Background())
	if !live.Allowed("200:aa::1") {
		t.Fatal("node does not accept the added node")
	}
	for _, p := range live.List() {
		if p.Name == "laptop" && (!p.Slow || p.Host != "home") {
			t.Fatalf("added node lost its options: %+v", p)
		}
	}
}

// post is a request as the dashboard page sends it.
func post(target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, body)
	r.Host = "127.0.0.1:7480"
	r.Header.Set("X-Yggstore", "1")
	return r
}

func TestGuardKeepsOtherPagesOut(t *testing.T) {
	d := New(Config{PeersPath: filepath.Join(t.TempDir(), "peers.json"), StubDir: t.TempDir(), Listen: "127.0.0.1:7480"})
	h := d.Handler()
	cases := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"page from another site, no header", func() *http.Request {
			r := post("/api/delete?stub=x", nil)
			r.Header.Del("X-Yggstore")
			return r
		}(), http.StatusForbidden},
		{"DNS rebinding: another host name", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/state", nil)
			r.Host = "evil.example:7480"
			return r
		}(), http.StatusForbidden},
		{"the dashboard page itself", post("/api/delete?stub=x", nil), http.StatusNotFound}, // unknown stub, but let in
		{"reading state on localhost", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/state", nil)
			r.Host = "localhost:7480"
			return r
		}(), http.StatusOK},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, c.req)
		if rec.Code != c.status {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.status)
		}
	}
}

// A node that took the list but reports another one runs older software:
// it is marked so, and not sent the list again every minute.
func TestOldNodeNotPushedForever(t *testing.T) {
	d := New(Config{PeersPath: filepath.Join(t.TempDir(), "peers.json"), StubDir: t.TempDir(), Listen: "127.0.0.1:7480"})
	list := []peers.Peer{{Name: "admin", Addr: "[200::1]:7400", Admin: true}, {Name: "old", Addr: "[200::2]:7400"}}
	states := []PeerState{{Name: "old", Addr: list[1].Addr, Info: &server.Info{PeersHash: "0123456789abcdef"}}}
	d.took[list[1].Addr] = peers.Hash(list)
	d.syncLists(context.Background(), list, states, true)
	if states[0].List != "old software" {
		t.Fatalf("list state %q, want old software", states[0].List)
	}
	if !d.pushed[list[1].Addr].IsZero() {
		t.Fatal("the list was sent again")
	}
}
