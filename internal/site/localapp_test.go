package site

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

func TestLocalConfig(t *testing.T) {
	dir := t.TempDir()
	if c, err := ReadLocalConfig(dir); err != nil || c != nil {
		t.Fatalf("no file: %v %v", c, err)
	}
	write := func(s string) { os.WriteFile(filepath.Join(dir, LocalConfig), []byte(s), 0o600) }
	write(`{"local":{"/app/":"http://127.0.0.1:3000"}}`)
	if c, err := ReadLocalConfig(dir); err != nil || c["/app/"] != "http://127.0.0.1:3000" {
		t.Fatalf("%v %v", c, err)
	}
	for _, bad := range []string{
		`{"local":{"/":"http://127.0.0.1:3000"}}`,
		`{"local":{"/app":"http://127.0.0.1:3000"}}`,
		`{"local":{"/a/../b/":"http://127.0.0.1:3000"}}`,
		`{"local":{"/_yggstore/x/":"http://127.0.0.1:3000"}}`,
		`{"local":{"/app/":"http://evil.example:3000"}}`,
		`{"local":{"/app/":"http://10.0.0.5:3000"}}`,
		`{"local":{"/app/":"https://127.0.0.1:3000"}}`,
		`{"local":{"/app/":"file:///etc/passwd"}}`,
	} {
		write(bad)
		if _, err := ReadLocalConfig(dir); err == nil {
			t.Errorf("took %s", bad)
		}
	}
}

func TestLocalAppThroughTheWebNode(t *testing.T) {
	// The program on the publisher's machine.
	var got *http.Request
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Set-Cookie", "s=1; Secure; HttpOnly")
		io.WriteString(w, "hello from "+r.URL.Path)
	}))
	t.Cleanup(app.Close)

	// The publisher's node: everything is from the web node at 200::9,
	// unless a test says otherwise.
	apps := LocalApps{Path: filepath.Join(t.TempDir(), "local-apps.json")}
	if err := apps.Set("peter.example.org", map[string]string{"/app/": app.URL}); err != nil {
		t.Fatal(err)
	}
	relay := &LocalRelay{Apps: apps, Self: "200::1", IsWebNode: func(_ context.Context, c string) bool { return c == "200::9" }}
	caller := "200::9"
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !relay.Serve(w, r, caller) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(node.Close)

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "index.html"), []byte("public page"), 0o644)
	list := []peers.Peer{{Name: "desktop", Addr: strings.TrimPrefix(node.URL, "http://"), Admin: true, Domain: "example.org",
		Names: []string{"peter.example.org"}, Code: "ys1a"}}
	owner := list[0].IP()
	web := &Web{Dir: t.TempDir(), Peers: func() []peers.Peer { return list }, Client: client.Client{HTTP: &http.Client{Transport: http.DefaultTransport}}, Log: t.Logf}
	web.st = webState{Sites: map[string]*webEntry{"peter.example.org": {Owner: owner, Serving: "v1", Local: []string{"/app/"}}}}
	web.roots = map[string]string{"peter.example.org": root}
	ask := func(path string, set func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://peter.example.org"+path, nil)
		r.RemoteAddr = "192.0.2.5:5555"
		if set != nil {
			set(r)
		}
		w := httptest.NewRecorder()
		web.ServeHTTP(w, r)
		return w
	}

	if w := ask("/", nil); !strings.Contains(w.Body.String(), "public page") {
		t.Fatalf("the public page: %d %s", w.Code, w.Body)
	}
	w := ask("/app/me?x=1", func(r *http.Request) {
		r.Header.Set("Cookie", "s=1")
		r.Header.Set("X-Yggstore-Site", "someone-else.example")
		r.Header.Set("X-Yggstore-Visitor", "6.6.6.6")
	})
	if w.Code != 200 || w.Body.String() != "hello from /app/me" || !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("through to the app: %d %q %v", w.Code, w.Body, w.Header())
	}
	if got.Host != "peter.example.org" || got.Header.Get("Cookie") != "s=1" || got.URL.RawQuery != "x=1" ||
		got.Header.Get("X-Forwarded-For") != "192.0.2.5" || got.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("what the app saw: host %s, %v", got.Host, got.Header)
	}
	// Paths the site didn't list never leave the web node.
	got = nil
	if ask("/app-secret", nil); got != nil {
		t.Fatal("a path that only starts like /app/ reached the app")
	}
	if ask("/app/../index.html", nil); got != nil {
		t.Fatal("a path that climbs out reached the app")
	}
	// A member that isn't a web node is refused.
	caller = "200::7"
	if res, err := http.Get(node.URL + localBase + "peter.example.org/app/me"); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("a member: %v %v", res, err)
	}
	caller = "200::9"
	// The program stopped: the offline page, and the site carries on.
	app.Close()
	if w := ask("/app/me", nil); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "Back soon") {
		t.Fatalf("the app down: %d %s", w.Code, w.Body)
	}
	// The machine off: the same, the site's own page if it has one.
	node.Close()
	if w := ask("/app/me", nil); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "Back soon") {
		t.Fatalf("the machine off: %d %s", w.Code, w.Body)
	}
	os.WriteFile(filepath.Join(root, OfflinePage), []byte("gone fishing"), 0o644)
	if w := ask("/app/me", nil); !strings.Contains(w.Body.String(), "gone fishing") {
		t.Fatalf("the site's own page: %s", w.Body)
	}
	if w := ask("/", nil); !strings.Contains(w.Body.String(), "public page") {
		t.Fatalf("the public page with the machine off: %d", w.Code)
	}
}

func TestLocalConfigOnlyForAMachinesOwnPublisher(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, LocalConfig), []byte(`{"local":{"/app/":"http://127.0.0.1:3000"}}`), 0o600)
	box := &Publisher{Dir: t.TempDir()}
	if err := box.readLocal("a.example.org", dir); err != nil {
		t.Fatal(err)
	}
	if s := box.readState("a.example.org"); len(s.Local) != 0 {
		t.Fatalf("a person's site got local paths: %v", s.Local)
	}
	own := &Publisher{Dir: t.TempDir(), LocalApps: LocalApps{Path: filepath.Join(t.TempDir(), "l.json")}}
	if err := own.readLocal("a.example.org", dir); err != nil {
		t.Fatal(err)
	}
	if s := own.readState("a.example.org"); len(s.Local) != 1 || s.Local[0] != "/app/" {
		t.Fatalf("%v", s.Local)
	}
	if own.LocalApps.load()["a.example.org"]["/app/"] != "http://127.0.0.1:3000" {
		t.Fatal("the target wasn't kept")
	}
}

func TestDevServesBothHalves(t *testing.T) {
	var proto string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Header.Get("X-Forwarded-Proto")
		io.WriteString(w, "private "+r.URL.Path)
	}))
	t.Cleanup(app.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>public</p>"), 0o644)
	os.WriteFile(filepath.Join(dir, LocalConfig), []byte(`{"local":{"/app/":"`+app.URL+`"}}`), 0o600)
	d := &Dev{Folder: dir, Name: "a.example.org", Log: t.Logf}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	ask := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		return w
	}
	if w := ask("/"); !strings.Contains(w.Body.String(), "public") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := ask("/app/me"); w.Body.String() != "private /app/me" || proto != "" {
		t.Fatalf("%d %s, proto %q", w.Code, w.Body, proto)
	}
	if w := ask("/" + LocalConfig); w.Code != http.StatusNotFound {
		t.Fatalf("the settings were served: %d %s", w.Code, w.Body)
	}
	app.Close()
	if w := ask("/app/me"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("the program stopped: %d", w.Code)
	}
}
