package workhere

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestBrowseAndDownload(t *testing.T) {
	ctx := context.Background()
	var nodes []peers.Peer
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		nodes = append(nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	// An outbox with a stored file in a folder.
	out, home := t.TempDir(), t.TempDir()
	m, _, _, err := files.PutReader(ctx, client.New(), strings.NewReader("the quarterly report"), "report.txt", nodes, files.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(out, "files", "Docs"), 0o755)
	files.WriteJSON(filepath.Join(out, "files", "Docs", "report.txt.ystub"), m)
	k := recovery.New(recovery.Config{Dir: filepath.Join(home, "recovery"), Client: client.New(),
		Sources: []recovery.Source{{Name: "outbox", Path: out, Keep: recovery.OutboxKeep}},
		Online:  func(context.Context) ([]peers.Peer, error) { return nodes, nil }})
	if _, err := k.SaveNow(ctx); err != nil {
		t.Fatal(err)
	}
	ks := k.Keys()
	s := &Store{Dir: t.TempDir()}
	b, _ := json.Marshal(Record{ID: ks.ID, GroupHalf: ks.GroupHalf, Sealed: ks.Sealed, Name: "anna"})
	s.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/work/"+ks.ID, strings.NewReader(string(b))), "200::1")

	list := func(link, dir string) (int, Listing) {
		w := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"id": ks.ID, "link": link, "path": dir})
		r := httptest.NewRequest(http.MethodPost, PagePath+"?op=list", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(w, r)
		var l Listing
		json.Unmarshal(w.Body.Bytes(), &l)
		return w.Code, l
	}
	if code, l := list(ks.LinkHalf, ""); code != 200 || len(l.Entries) != 1 || !l.Entries[0].Folder || l.Entries[0].Name != "Docs" {
		t.Fatalf("top: %d %+v", code, l)
	}
	if code, l := list(ks.LinkHalf, "Docs"); code != 200 || len(l.Entries) != 1 || l.Entries[0].Name != "report.txt" || l.Entries[0].Size != 20 {
		t.Fatalf("Docs: %d %+v", code, l)
	}
	if code, _ := list(strings.Repeat("A", 43), ""); code != http.StatusForbidden {
		t.Fatalf("wrong link: %d", code)
	}

	form := url.Values{"id": {ks.ID}, "link": {ks.LinkHalf}, "path": {"Docs/report.txt"}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, PagePath+"?op=get", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "the quarterly report" || !strings.Contains(w.Header().Get("Content-Disposition"), "report.txt") {
		t.Fatalf("download: %d %q %v", w.Code, w.Body, w.Header())
	}
	form.Set("path", "../../etc/passwd")
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, PagePath+"?op=get", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("a path outside: %d", w.Code)
	}
}
