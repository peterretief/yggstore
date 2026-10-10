package dashboard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestPublishWebsite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	me, _ := share.LoadOrCreate(filepath.Join(dir, "me.key"))
	list := []peers.Peer{
		{Name: "admin", Addr: "[200::9]:7400", Admin: true, Domain: "example.org"},
		{Name: "anna", Addr: "[200::5]:7400", Names: []string{"anna.example.org"}, Code: me.Code()},
	}
	var states []PeerState
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		p := peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")}
		list = append(list, p)
		states = append(states, PeerState{Name: p.Name, Addr: p.Addr, Up: true})
	}
	peersPath := filepath.Join(dir, "peers.json")
	if err := peers.Write(peersPath, list); err != nil {
		t.Fatal(err)
	}
	e, err := msg.Open(filepath.Join(dir, "msg"), "200::5", func() []peers.Peer { return list }, pairNet{"200::5", nil}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	go e.Run(ctx)
	token := strings.Repeat("ab", 24)
	tokenPath := filepath.Join(dir, "msg.token")
	os.WriteFile(tokenPath, []byte(token), 0o600)
	api := httptest.NewServer(e.LocalHandler(token))
	defer api.Close()
	sitesDir := filepath.Join(dir, "sites")
	own := &site.Own{Pub: &site.Publisher{Dir: sitesDir, Client: client.New()}, Client: client.New(), Dir: filepath.Join(dir, "own-sites")}
	d := New(Config{PeersPath: peersPath, StubDir: dir, SelfID: "200::5", Listen: "127.0.0.1:7480", Identity: me, Client: client.New(),
		MsgAPI: api.Listener.Addr().String(), MsgTokenPath: tokenPath, SitesDir: sitesDir, OwnSites: own, SiteURL: "http://[200::5]:8480/"})
	own.Default = d.DefaultSite
	d.state.Peers = states
	l := msg.Local{Addr: api.Listener.Addr().String(), Token: token}
	l.Subscribe(ctx, site.Topic) // so its own announcements are kept

	upload := func(name string, files map[string]string) *httptest.ResponseRecorder {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		for p, body := range files {
			fw, _ := mw.CreateFormFile("f:"+p, filepath.Base(p))
			fw.Write([]byte(body))
		}
		mw.Close()
		r := post("/api/sites/publish?site="+name, &b)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, r)
		return w
	}
	if w := upload("someone.example.org", map[string]string{"index.html": "x"}); w.Code != http.StatusForbidden {
		t.Fatalf("published someone else's name: %d %s", w.Code, w.Body)
	}
	if w := upload("anna.example.org", map[string]string{"about.html": "x"}); w.Code != http.StatusBadRequest {
		t.Fatalf("published without index.html: %d %s", w.Code, w.Body)
	}
	w := upload("anna.example.org", map[string]string{"index.html": "<h1>Anna</h1>", "img/a.txt": "a", "../escape": "no", ".git/HEAD": "no"})
	if w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}

	// The announcement is on the sites topic, and holds the two files.
	got, err := l.Messages(ctx, 0, site.Topic, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("announcements %v %v", got, err)
	}
	var a site.Announcement
	json.Unmarshal([]byte(got[0].Body), &a)
	if a.Site != "anna.example.org" || a.Stub == nil || a.Stub.FileCount != 2 {
		t.Fatalf("announced %+v %d files", a, a.Stub.FileCount)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	r.Host = "127.0.0.1:7480"
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, r)
	var st websiteState
	json.Unmarshal(rec.Body.Bytes(), &st)
	if len(st.Sites) != 1 || !st.Sites[0].Name || st.Sites[0].Current == nil || st.Sites[0].Versions != 1 {
		t.Fatalf("sites %s", rec.Body)
	}

	if st.Default != "anna.example.org" || st.SiteURL == "" {
		t.Fatalf("own site %q at %q", st.Default, st.SiteURL)
	}

	// The box shows it at its bare address, from the copy kept when it was
	// published, and again after losing that copy (fetched from the group).
	for _, lose := range []bool{false, true} {
		if lose {
			os.RemoveAll(filepath.Join(dir, "own-sites"))
		}
		r := httptest.NewRequest(http.MethodGet, "http://[200::5]:8480/", nil)
		rec := httptest.NewRecorder()
		own.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Anna") {
			t.Fatalf("own site (copy lost %v): %d %q", lose, rec.Code, rec.Body)
		}
	}

	r = httptest.NewRequest(http.MethodGet, "/api/sites/download?site=anna.example.org", nil)
	r.Host = "127.0.0.1:7480"
	rec = httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, r)
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("download: %d %v", rec.Code, err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "anna.example.org/img/a.txt,anna.example.org/index.html" {
		t.Fatalf("zip holds %v", names)
	}

	// A zip as Download gives it (the site in a folder) replaces the site.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for p, body := range map[string]string{"anna-old/index.html": "<h1>From the zip</h1>", "anna-old/style.css": "h1{}", "__MACOSX/._index.html": "junk"} {
		f, _ := zw.Create(p)
		f.Write([]byte(body))
	}
	zw.Close()
	send := func(query string, parts map[string][]byte) *httptest.ResponseRecorder {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		for name, body := range parts {
			fw, _ := mw.CreateFormFile(name, "x")
			fw.Write(body)
		}
		mw.Close()
		r := post("/api/sites/publish?site=anna.example.org"+query, &b)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body)
		}
		return w
	}
	files := func() string {
		r := httptest.NewRequest(http.MethodGet, "/api/sites/files?site=anna.example.org", nil)
		r.Host = "127.0.0.1:7480"
		rec := httptest.NewRecorder()
		d.Handler().ServeHTTP(rec, r)
		var fs []siteFile
		json.Unmarshal(rec.Body.Bytes(), &fs)
		var out []string
		for _, f := range fs {
			out = append(out, f.Path)
		}
		return strings.Join(out, ",")
	}
	send("", map[string][]byte{"zip": zb.Bytes()})
	if got := files(); got != "index.html,style.css" {
		t.Fatalf("after the zip: %s", got)
	}
	// Edits change the version being served: a page changed, one added, one deleted.
	send("&edit=1", map[string][]byte{"f:index.html": []byte("<h1>Edited</h1>"), "f:img/logo.svg": []byte("<svg/>")})
	send("&edit=1", map[string][]byte{"delete": []byte(`["style.css"]`)})
	if got := files(); got != "img/logo.svg,index.html" {
		t.Fatalf("after editing: %s", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/sites/files?site=anna.example.org&path=index.html", nil)
	r.Host = "127.0.0.1:7480"
	rec = httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, r)
	if rec.Body.String() != "<h1>Edited</h1>" {
		t.Fatalf("index.html is %q", rec.Body)
	}
	if vs, _ := own.Pub.Versions("anna.example.org"); len(vs) != 4 {
		t.Fatalf("%d versions, want 4", len(vs))
	}

	// Turning the contact form on puts a link to it on the home page, and
	// turning it off takes the link away again.
	index := func() string {
		r := httptest.NewRequest(http.MethodGet, "/api/sites/files?site=anna.example.org&path=index.html", nil)
		r.Host = "127.0.0.1:7480"
		rec := httptest.NewRecorder()
		d.Handler().ServeHTTP(rec, r)
		return rec.Body.String()
	}
	contact := func(on bool) {
		rec := httptest.NewRecorder()
		d.Handler().ServeHTTP(rec, post("/api/sites/contact", strings.NewReader(fmt.Sprintf(`{"site":"anna.example.org","on":%v}`, on))))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("contact %v: %d %s", on, rec.Code, rec.Body)
		}
	}
	contact(true)
	if got := index(); !strings.HasPrefix(got, "<h1>Edited</h1>") || !strings.Contains(got, `href="/_yggstore/contact"`) {
		t.Fatalf("with the form on, index.html is %q", got)
	}
	contact(true)
	if got := index(); strings.Count(got, "/_yggstore/contact") != 1 {
		t.Fatalf("the link was added twice: %q", got)
	}
	contact(false)
	if got := index(); got != "<h1>Edited</h1>" {
		t.Fatalf("with the form off, index.html is %q", got)
	}
}

func TestContactLink(t *testing.T) {
	page := "<html><body>\n<h1>Hi</h1>\n</BODY></html>"
	with := withContactLink(page)
	if !strings.HasSuffix(with, contactLinkHTML+"</BODY></html>") {
		t.Fatalf("added: %q", with)
	}
	if got := withoutContactLink(with); got != page {
		t.Fatalf("removed: %q", got)
	}
	own := `<a href="/_yggstore/contact">Write to me</a>`
	if withContactLink(own) != own {
		t.Fatal("a page that links to the form already got a second link")
	}
}
