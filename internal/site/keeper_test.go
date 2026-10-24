package site

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
)

// A box publishes a site with code; the web node's entries reach the
// box's copy, and a new web node, with the old one gone, gets them back
// from the box.
func TestKeeper(t *testing.T) {
	ctx := context.Background()
	const site = "shop.example"
	addr := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	webNode := func(others func(context.Context) []string, list []peers.Peer) *Web {
		w := &Web{Dir: t.TempDir(), Log: t.Logf, Self: "web", Others: others, Peers: func() []peers.Peer { return list }}
		w.load()
		// The box, at 127.0.0.1 here, published it.
		w.st.Sites[site] = &webEntry{Owner: "127.0.0.1", Code: true}
		return w
	}
	none := func(context.Context) []string { return []string{} }

	a := webNode(none, nil)
	d, _ := a.siteLog(site)
	t.Cleanup(d.close)
	t0 := time.UnixMilli(1_800_000_000_000)
	e1, _ := d.record("web", "join", map[string]any{"email": "anna@example.org"}, t0)
	e2, _ := d.record("web", "join", map[string]any{"email": "ben@example.org"}, t0.Add(time.Second))
	apiA := serveNodeAPI(t, &a)

	k := &Keeper{Dir: t.TempDir(), WebNodes: func(context.Context) []string { return []string{addr(apiA.srv)} }}
	t.Cleanup(k.close)
	k.Step(ctx)
	if got := k.Sites(); len(got) != 1 || got[0] != site {
		t.Fatalf("the box keeps %v", got)
	}
	kd, _ := k.log(site)
	if have, _, _ := kd.since(0, 10); len(have) != 2 || have[0].ID != e1.ID || have[1].ID != e2.ID {
		t.Fatalf("the box's copy: %v", have)
	}
	// Only the web nodes may read it.
	rec := httptest.NewRecorder()
	k.Serve(rec, httptest.NewRequest("GET", dataPath+"entries?site="+site, nil), "200::99")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a stranger asking the box: %d", rec.Code)
	}
	// A web node won't give a site's entries to a node that doesn't own it.
	rec = httptest.NewRecorder()
	a.Serve(rec, httptest.NewRequest("GET", dataPath+"entries?site="+site, nil), "200::99")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a stranger asking the web node: %d", rec.Code)
	}

	// The web node is gone; a new one catches up from the box.
	apiA.srv.Close()
	box := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !k.Serve(rw, r, "127.0.0.1") {
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(box.Close)
	b := webNode(none, []peers.Peer{{Name: "box", Addr: addr(box)}})
	b.pull(ctx)
	bd, _ := b.siteLog(site)
	t.Cleanup(bd.close)
	if have, _, _ := bd.since(0, 10); len(have) != 2 {
		t.Fatalf("the new web node got %d entries from the box", len(have))
	}
}

func TestKeeperAddr(t *testing.T) {
	list := []peers.Peer{{Name: "box-usb1", Addr: "[201:2833::1]:7401"}, {Name: "box", Addr: "[201:2833::1]:7400"}, {Name: "other", Addr: "[200:1::2]:7400"}}
	if got := KeeperAddr(list, "201:2833:0:0::1"); got != "[201:2833::1]:7400" {
		t.Fatalf("keeper: %q", got)
	}
	if !SameAddr("[201:2833::1]:7400", "[201:2833:0::1]:7400") || SameAddr("[201:2833::1]:7400", "[201:2833::1]:7401") {
		t.Fatal("SameAddr")
	}
}
