package site

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

// bus stands in for the nodes' messaging: what is published is received.
type bus struct {
	mu   sync.Mutex
	msgs []msg.Stored
	from string // sender of the next publish
	wake chan struct{}
}

func newBus() *bus { return &bus{from: "200::1", wake: make(chan struct{})} }

func (b *bus) Publish(_ context.Context, topic, typ, body string) error {
	b.mu.Lock()
	n := int64(len(b.msgs) + 1)
	b.msgs = append(b.msgs, msg.Stored{N: n, FromName: b.from, Message: msg.Message{
		From: b.from, Topic: topic, Type: typ, Body: body, Time: time.Now().UnixMilli() + n}})
	close(b.wake)
	b.wake = make(chan struct{})
	b.mu.Unlock()
	return nil
}

func (b *bus) Subscribe(string) error { return nil }

func (b *bus) Wait(ctx context.Context, after int64, topic string) []msg.Stored {
	for {
		b.mu.Lock()
		var out []msg.Stored
		for _, m := range b.msgs {
			if m.N > after && m.Topic == topic {
				out = append(out, m)
			}
		}
		wake := b.wake
		b.mu.Unlock()
		if len(out) > 0 {
			return out
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return nil
		}
	}
}

func storageNodes(t *testing.T, n int) []peers.Peer {
	var list []peers.Peer
	for i := range n {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	return list
}

func writeSite(t *testing.T, dir string, files map[string]string) {
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func get(t *testing.T, h http.Handler, method, host, path string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, "http://"+host+path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

// eventually waits for the web node to serve want at host/.
func eventually(t *testing.T, w *Web, host, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := get(t, w, http.MethodGet, host, "/")
		if code == http.StatusOK && strings.Contains(body, want) {
			return
		}
		if want == "" && code == http.StatusNotFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s/ is %d %q, want %q", host, code, body, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPublishServeUpdateRollbackRemove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes := storageNodes(t, 6)
	b := newBus()
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	webDir := t.TempDir()
	web := &Web{Dir: webDir, Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf}
	go web.Run(ctx)

	src := t.TempDir()
	writeSite(t, src, map[string]string{
		"index.html":       "<h1>one</h1>",
		"about/index.html": "about us",
		"404.html":         "lost?",
		"img/logo.svg":     "<svg/>",
		"empty/.keep":      "",
	})
	v1, _, err := pub.Publish(ctx, "example.org", src, nodes)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "example.org", "one")

	if code, body := get(t, web, http.MethodGet, "example.org", "/about/"); code != 200 || body != "about us" {
		t.Fatalf("/about/: %d %q", code, body)
	}
	r := httptest.NewRecorder()
	web.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "http://example.org/", nil))
	if ct := r.Header().Get("Content-Type"); ct != "text/html" {
		t.Fatalf("page served as %q, want text/html with no charset, so the page's own meta tag counts", ct)
	}
	if code, body := get(t, web, http.MethodGet, "example.org", "/nope"); code != 404 || body != "lost?" {
		t.Fatalf("missing page: %d %q, want the site's 404.html", code, body)
	}
	if code, _ := get(t, web, http.MethodGet, "example.org", "/empty/"); code != 404 {
		t.Fatalf("folder without index.html: %d, want 404 (no listing)", code)
	}
	if code, body := get(t, web, http.MethodGet, "WWW.Example.org:443", "/"); code != 200 || !strings.Contains(body, "one") {
		t.Fatalf("www and port: %d %q", code, body)
	}
	if code, _ := get(t, web, http.MethodPost, "example.org", "/"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", code)
	}
	if code, _ := get(t, web, http.MethodGet, "other.org", "/"); code != 404 {
		t.Fatalf("unknown site: %d", code)
	}
	if code, _ := get(t, web, http.MethodGet, "example.org", "/../../../etc/passwd"); code != 404 {
		t.Fatalf("path outside the site: %d", code)
	}

	// An update serves the new version.
	writeSite(t, src, map[string]string{"index.html": "<h1>two</h1>"})
	v2, _, err := pub.Publish(ctx, "example.org", src, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if v2.ID == v1.ID {
		t.Fatal("changed site made no new version")
	}
	eventually(t, web, "example.org", "two")

	// Publishing unchanged content makes no new version.
	v3, _, err := pub.Publish(ctx, "example.org", src, nodes)
	if err != nil || v3.ID != v2.ID {
		t.Fatalf("unchanged publish: %v, version %s, want %s", err, v3.ID, v2.ID)
	}

	// Rollback goes back to the version before.
	if _, err := pub.Rollback(ctx, "example.org", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "example.org", "one")
	vs, _ := pub.Versions("example.org")
	if len(vs) != 2 || !vs[1].Current {
		t.Fatalf("versions after rollback: %+v", vs)
	}

	// Another member can't change someone else's site.
	b.from = "200::9"
	typ, body, _ := encode(Announcement{Site: "example.org"})
	b.Publish(ctx, Topic, typ, body)
	time.Sleep(100 * time.Millisecond)
	eventually(t, web, "example.org", "one")
	b.from = "200::1"

	// Nor take over its www. name, which is served as the site itself.
	b.from = "200::9"
	srcW := t.TempDir()
	writeSite(t, srcW, map[string]string{"index.html": "hijacked"})
	pubW := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	if _, _, err := pubW.Publish(ctx, "www.example.org", srcW, nodes); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	eventually(t, web, "www.example.org", "one")
	b.from = "200::1"

	// A restarted web node serves what it has before any message.
	cancel()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	web2 := &Web{Dir: webDir, Msgs: newBus(), Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf}
	go web2.Run(ctx2)
	eventually(t, web2, "example.org", "one")
	cancel2()

	// Removing it stops serving and deletes it.
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	web3 := &Web{Dir: webDir, Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf}
	go web3.Run(ctx3)
	if err := pub.Remove(ctx3, "example.org"); err != nil {
		t.Fatal(err)
	}
	eventually(t, web3, "example.org", "")
	if _, err := os.Stat(filepath.Join(webDir, "sites", "example.org")); !os.IsNotExist(err) {
		t.Fatal("site's copy kept after removal")
	}
	if sites := pub.Sites(); len(sites) != 0 {
		t.Fatalf("publisher still lists %v", sites)
	}
}

func TestValidName(t *testing.T) {
	for name, ok := range map[string]bool{
		"example.org": true, "www.example.co.za": true, "mta-sts.example.org": true,
		"localhost": false, "Example.org": false, "-bad.org": false, "a..b.org": false, "../x.org": false,
	} {
		if (ValidName(name) == nil) != ok {
			t.Errorf("%s: want ok=%v", name, ok)
		}
	}
}

// Announcements come from other members, so a web node checks them: a file
// ID that could leave its folder, or parts somewhere other than the group's
// nodes, are ignored.
func TestWebIgnoresUnsafeAnnouncements(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes := storageNodes(t, 6)
	b := newBus()
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	src := t.TempDir()
	writeSite(t, src, map[string]string{"index.html": "real"})
	if _, _, err := pub.Publish(ctx, "example.org", src, nodes); err != nil {
		t.Fatal(err)
	}
	stub := func(edit func(m map[string]any)) string {
		vs, _ := pub.Versions("example.org")
		raw, _ := os.ReadFile(vs[0].stub)
		var m map[string]any
		json.Unmarshal(raw, &m)
		edit(m)
		body, _ := json.Marshal(map[string]any{"site": "evil.org", "stub": m})
		return string(body)
	}
	b.Publish(ctx, Topic, typePublish, stub(func(m map[string]any) { m["file_id"] = "../../../escaped" }))
	b.Publish(ctx, Topic, typePublish, stub(func(m map[string]any) {
		for _, ch := range m["chunks"].([]any) {
			for _, s := range ch.(map[string]any)["shards"].([]any) {
				s.(map[string]any)["peer"] = "192.0.2.1:80"
			}
		}
	}))

	webDir := filepath.Join(t.TempDir(), "web")
	web := &Web{Dir: webDir, Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf}
	go web.Run(ctx)
	eventually(t, web, "example.org", "real")
	time.Sleep(200 * time.Millisecond)
	for _, s := range web.Status() {
		if s.Site == "evil.org" {
			t.Fatalf("unsafe announcement accepted: %+v", s)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(webDir), "escaped")); err == nil {
		t.Fatal("a file ID wrote outside the web node's folder")
	}
}
