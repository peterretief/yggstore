package site

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

func TestPreview(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes := storageNodes(t, 6)
	b := newBus()
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	web := &Web{Dir: t.TempDir(), Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf}
	background(t, func() { web.Run(ctx) })

	src := t.TempDir()
	writeSite(t, src, map[string]string{"index.html": "<html><body><h1>live</h1></body></html>", "about/index.html": "about live"})
	if _, err := pub.PublishPreview(ctx, "example.org", src, nodes); err == nil {
		t.Fatal("a preview before the site was ever published")
	}
	if _, _, err := pub.Publish(ctx, "example.org", src, nodes); err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "example.org", "live")

	writeSite(t, src, map[string]string{"index.html": "<html><body><h1>new look</h1></body></html>", "about/index.html": "about new"})
	link, err := pub.PublishPreview(ctx, "example.org", src, nodes)
	if err != nil || !strings.HasPrefix(link, "https://example.org"+PreviewPath+"?key=") {
		t.Fatalf("preview: %q %v", link, err)
	}
	u, _ := url.Parse(link)
	key := u.Query().Get("key")

	// Everyone else still sees the live site.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		web.mu.Lock()
		e := web.st.Sites["example.org"]
		ready := e != nil && e.Preview != ""
		web.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the web node never fetched the preview")
		}
	}
	if _, body := get(t, web, http.MethodGet, "example.org", "/"); !strings.Contains(body, "live") {
		t.Fatalf("visitors see %q", body)
	}

	// The link sets the cookie; a wrong key doesn't.
	open := func(q string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		web.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "http://example.org"+PreviewPath+"?"+q, nil))
		return r
	}
	if r := open("key=wrong"); r.Code != http.StatusNotFound || len(r.Result().Cookies()) != 0 {
		t.Fatalf("wrong key: %d %v", r.Code, r.Result().Cookies())
	}
	r := open("key=" + url.QueryEscape(key))
	cookies := r.Result().Cookies()
	if r.Code != http.StatusSeeOther || len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("opening the preview: %d %+v", r.Code, cookies)
	}
	withCookie := func(path string) (int, string, string) {
		req := httptest.NewRequest(http.MethodGet, "http://example.org"+path, nil)
		req.AddCookie(cookies[0])
		rec := httptest.NewRecorder()
		web.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control")
	}
	code, body, cache := withCookie("/")
	if code != 200 || !strings.Contains(body, "new look") || !strings.Contains(body, "Preview: not live yet") || cache != "no-store" {
		t.Fatalf("preview home: %d %q %q", code, body, cache)
	}
	if _, body, _ := withCookie("/about/"); !strings.HasPrefix(body, "about new") {
		t.Fatalf("preview about: %q", body)
	}
	// The web node keeps the preview when it cleans up old versions.
	if _, body, _ := withCookie("/"); !strings.Contains(body, "new look") {
		t.Fatal("preview gone")
	}

	// A Markdown site's source is never served.
	for _, p := range []string{"/" + SourceDir + "/index.md", "/" + SourceDir, "/x/../" + SourceDir + "/a"} {
		if code, _ := get(t, web, http.MethodGet, "example.org", p); code != http.StatusNotFound {
			t.Errorf("%s: %d", p, code)
		}
	}

	// Going live serves it to everyone; the link stops working.
	if _, err := pub.GoLive(ctx, "example.org"); err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "example.org", "new look")
	if r := open("key=" + url.QueryEscape(key)); r.Code != http.StatusNotFound {
		t.Fatalf("the old link after going live: %d", r.Code)
	}
	if _, body, _ := withCookie("/"); strings.Contains(body, "Preview: not live yet") {
		t.Fatal("still shown as a preview after going live")
	}

	// A dropped preview: visitors keep the live site, and the cookie
	// shows the live site too.
	writeSite(t, src, map[string]string{"index.html": "<html><body><h1>draft</h1></body></html>"})
	if _, err := pub.PublishPreview(ctx, "example.org", src, nodes); err != nil {
		t.Fatal(err)
	}
	if err := pub.DropPreview(ctx, "example.org"); err != nil {
		t.Fatal(err)
	}
	if pub.PreviewLink("example.org") != "" {
		t.Fatal("a link for a dropped preview")
	}
	if _, body := get(t, web, http.MethodGet, "example.org", "/"); !strings.Contains(body, "new look") {
		t.Fatalf("after dropping: %q", body)
	}
}
