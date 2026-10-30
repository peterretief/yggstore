package site

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

// buildModule builds the handler in testdata/DIR for WASI.
func buildModule(t *testing.T, dir string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a WebAssembly module")
	}
	srcs, _ := filepath.Glob(filepath.Join("testdata", dir, "*.go"))
	out := filepath.Join(t.TempDir(), dir+".wasm")
	args := []string{"build", "-ldflags=-s -w", "-o", out}
	for _, s := range srcs {
		args = append(args, filepath.Base(s))
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join("testdata", dir)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", dir, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// apiCall sends a request to a site's handler and decodes its JSON reply.
func apiCall(t *testing.T, h http.Handler, method, path, body string, into any) int {
	t.Helper()
	rec := request(h, method, "shop.example", path, body)
	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("%s %s: %d %q: %v", method, path, rec.Code, rec.Body, err)
		}
	}
	return rec.Code
}

// A web node's node API for the others, which can be cut off.
type nodeAPI struct {
	srv *httptest.Server
	cut atomic.Bool
}

func serveNodeAPI(t *testing.T, w **Web) *nodeAPI {
	n := &nodeAPI{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if n.cut.Load() {
			http.Error(rw, "cut off", http.StatusServiceUnavailable)
			return
		}
		if !(*w).Serve(rw, r, "127.0.0.1") {
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// Two web nodes serve a shop whose handler keeps members and bookings.
// Entries go from one to the other at once; cut off from each other, both
// take bookings for the last places, and once they meet again both have
// the same bookings, the same ones turned down.
func TestSiteDataHandlers(t *testing.T) {
	wasm := buildModule(t, "dbhandler")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := storageNodes(t, 6)
	nodes := append(append([]peers.Peer{}, storage...), peers.Peer{Name: "admin", Addr: "[200::1]:7400", Admin: true})
	b := newBus()
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	var a, z *Web
	apiA, apiZ := serveNodeAPI(t, &a), serveNodeAPI(t, &z)
	addr := func(n *nodeAPI) string { return strings.TrimPrefix(n.srv.URL, "http://") }
	// An older web node, without sites' data: it doesn't count against
	// "confirmed".
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	mk := func(self string, other *nodeAPI) *Web {
		return &Web{Dir: t.TempDir(), Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf,
			Self: self, Others: func(context.Context) []string { return []string{addr(other), strings.TrimPrefix(old.URL, "http://")} }}
	}
	a, z = mk("web-a", apiZ), mk("web-z", apiA)
	background(t, func() { a.Run(ctx) })
	background(t, func() { z.Run(ctx) })

	src := t.TempDir()
	writeSite(t, src, map[string]string{"index.html": "<h1>shop</h1>", "api/shop.wasm": string(wasm),
		"api/schema.sql": testSchema, "api/entries.sql": testEntries})
	if _, _, err := pub.Publish(ctx, "shop.example", src, storage); err != nil {
		t.Fatal(err)
	}
	pub.SetCode(ctx, "shop.example", true)
	for _, w := range []*Web{a, z} {
		eventually(t, w, "shop.example", "shop")
		wait := 20 * time.Second
		if handlerTime != HandlerTime {
			wait = 5 * time.Minute // the race detector: compiling alone takes most of a minute
		}
		for deadline := time.Now().Add(wait); ; {
			rec := request(w, "GET", "shop.example", "/api/shop/members", "")
			if rec.Code == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the handler never answered: %d %s", rec.Code, rec.Body)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Anna joins at a; z has her at once.
	var res Recorded
	if code := apiCall(t, a, "POST", "/api/shop/join", `{"email":"Anna@Example.org","name":"Anna"}`, &res); code != 200 || res.Status != EntryApplied || !res.Confirmed {
		t.Fatalf("join: %d %+v", code, res)
	}
	var members []map[string]any
	apiCall(t, z, "GET", "/api/shop/members", "", &members)
	if len(members) != 1 || members[0]["email"] != "anna@example.org" {
		t.Fatalf("z's members: %v", members)
	}
	// Her email again, at z: turned down by the site's rule.
	if apiCall(t, z, "POST", "/api/shop/join", `{"email":"anna@example.org","name":"Anna 2"}`, &res); res.Status != EntryRefused || res.Confirmed {
		t.Fatalf("a second account: %+v", res)
	}
	var wrote map[string]bool
	if apiCall(t, a, "GET", "/api/shop/write", "", &wrote); wrote["wrote"] {
		t.Fatal("a query wrote to the data")
	}

	// Cut off from each other, both take bookings for the 2 places.
	apiA.cut.Store(true)
	apiZ.cut.Store(true)
	if apiCall(t, a, "POST", "/api/shop/book", `{"course":"intro","email":"ben@example.org"}`, &res); res.Status != EntryApplied || res.Confirmed {
		t.Fatalf("a booking while cut off: %+v", res)
	}
	time.Sleep(5 * time.Millisecond)
	apiCall(t, z, "POST", "/api/shop/book", `{"course":"intro","email":"cy@example.org"}`, &res)
	time.Sleep(5 * time.Millisecond)
	if apiCall(t, z, "POST", "/api/shop/book", `{"course":"intro","email":"dee@example.org"}`, &res); res.Status != EntryApplied || res.Confirmed {
		t.Fatalf("dee's booking while cut off is %+v: z alone has room for her", res)
	}
	dee := res.ID

	// They meet again and catch up.
	apiA.cut.Store(false)
	apiZ.cut.Store(false)
	a.pull(ctx)
	z.pull(ctx)
	var ba, bz []map[string]any
	apiCall(t, a, "GET", "/api/shop/bookings?course=intro", "", &ba)
	apiCall(t, z, "GET", "/api/shop/bookings?course=intro", "", &bz)
	ja, _ := json.Marshal(ba)
	jz, _ := json.Marshal(bz)
	if string(ja) != string(jz) || len(ba) != 2 {
		t.Fatalf("the web nodes differ: a %s, z %s", ja, jz)
	}
	if !strings.Contains(string(ja), "ben@") || !strings.Contains(string(ja), "cy@") {
		t.Fatalf("the first two in time should have the places: %s", ja)
	}
	for _, w := range []*Web{a, z} {
		d, _ := w.siteLog("shop.example")
		if st, why := d.status(dee); st != EntryRefused || !strings.Contains(why, "full") {
			t.Fatalf("%s: dee's booking after meeting: %s %q", w.Self, st, why)
		}
	}

	// A new member at z, after they met: confirmed again.
	if apiCall(t, z, "POST", "/api/shop/join", `{"email":"ed@example.org","name":"Ed"}`, &res); !res.Confirmed {
		t.Fatalf("after meeting again: %+v", res)
	}
	// Strangers can't send entries or read them.
	rec := httptest.NewRecorder()
	if !a.Serve(rec, httptest.NewRequest("GET", dataPath+"entries?site=shop.example", nil), "200::99") || rec.Code != http.StatusForbidden {
		t.Fatalf("a stranger asking for entries: %d", rec.Code)
	}
}
