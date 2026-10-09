package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	d := New(Config{PeersPath: peersPath, StubDir: dir, SelfID: "200::5", Listen: "127.0.0.1:7480", Identity: me, Client: client.New(),
		MsgAPI: api.Listener.Addr().String(), MsgTokenPath: tokenPath, SitesDir: filepath.Join(dir, "sites")})
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
}
