package workhere

import (
	"archive/tar"
	"bytes"
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

// A file uploaded on the page waits on the web node, shows as arriving,
// and is collected into the outbox by the place holding the working copy;
// then the web node's copy goes.
func TestUploadIsCollected(t *testing.T) {
	ctx := context.Background()
	var nodes []peers.Peer
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		nodes = append(nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	out := t.TempDir()
	os.MkdirAll(filepath.Join(out, "files", "Docs"), 0o755)
	os.WriteFile(filepath.Join(out, "files", "Docs", "old.txt.ystub"), []byte(`{}`), 0o644)
	k := recovery.New(recovery.Config{Dir: filepath.Join(t.TempDir(), "recovery"), Client: client.New(),
		Sources: []recovery.Source{{Name: "outbox", Path: out, Keep: recovery.OutboxKeep}},
		Online:  func(context.Context) ([]peers.Peer, error) { return nodes, nil }})
	if _, err := k.SaveNow(ctx); err != nil {
		t.Fatal(err)
	}
	ks := k.Keys()
	s := &Store{Dir: t.TempDir(), Online: func(context.Context) ([]peers.Peer, error) { return nodes, nil }}
	b, _ := json.Marshal(Record{ID: ks.ID, GroupHalf: ks.GroupHalf, Sealed: ks.Sealed, Name: "anna"})
	s.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/work/"+ks.ID, strings.NewReader(string(b))), "200::1")
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Serve(w, r, "200::1") {
			http.NotFound(w, r)
		}
	}))
	defer web.Close()

	up := func(link, name string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, PagePath+"?op=put", strings.NewReader("minutes of the meeting"))
		r.Header.Set("X-Id", ks.ID)
		r.Header.Set("X-Link", link)
		r.Header.Set("X-Dir", "Docs")
		r.Header.Set("X-Name", url.PathEscape(name))
		s.ServeHTTP(w, r)
		return w.Code
	}
	if code := up(strings.Repeat("A", 43), "minutes.txt"); code != http.StatusForbidden {
		t.Fatalf("upload with a wrong link: %d", code)
	}
	if code := up(ks.LinkHalf, "../../escape.txt"); code != 200 {
		t.Fatalf("a name with a path is taken as its base: %d", code)
	}
	if code := up(ks.LinkHalf, "minutes ü.txt"); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	// A folder's file, its folders made when it's collected.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, PagePath+"?op=put", strings.NewReader("day one"))
	r.Header.Set("X-Id", ks.ID)
	r.Header.Set("X-Link", ks.LinkHalf)
	r.Header.Set("X-Dir", url.PathEscape("Trip/day 1"))
	r.Header.Set("X-Name", "notes.txt")
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("upload into new folders: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, PagePath+"?op=put", strings.NewReader("x"))
	r.Header.Set("X-Id", ks.ID)
	r.Header.Set("X-Link", ks.LinkHalf)
	r.Header.Set("X-Dir", url.PathEscape("Trip/.git"))
	r.Header.Set("X-Name", "config")
	s.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("upload into a hidden folder: %d", w.Code)
	}
	if p := s.inbox(ks.ID); len(p) != 3 {
		t.Fatalf("pending %v", p)
	}

	n, err := Collect(ctx, client.New(), []string{strings.TrimPrefix(web.URL, "http://")}, ks, out, nil)
	if err != nil || n != 3 {
		t.Fatalf("collected %d: %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "files", "Docs", "minutes ü.txt")); string(b) != "minutes of the meeting" {
		t.Fatalf("collected file: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "files", "Trip", "day 1", "notes.txt")); string(b) != "day one" {
		t.Fatalf("a folder's file collected: %q", b)
	}
	if _, err := os.Stat(filepath.Join(out, "files", "Docs", "escape.txt")); err != nil {
		t.Fatal("the name with a path wasn't kept in the folder it was uploaded to")
	}
	if p := s.inbox(ks.ID); len(p) != 0 {
		t.Fatalf("still pending after collecting: %v", p)
	}
}

// A folder kept whole downloads inside a folder of its own name.
func TestInFolder(t *testing.T) {
	var src, dst bytes.Buffer
	tw := tar.NewWriter(&src)
	tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "docs/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "docs/a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "../b.txt", Typeflag: tar.TypeLink, Linkname: "docs/a.txt"})
	tw.Close()
	if err := inFolder(&dst, &src, "Photos"); err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := tar.NewReader(&dst)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		n := h.Name
		if h.Linkname != "" {
			n += " -> " + h.Linkname
		}
		got = append(got, n)
	}
	if s := strings.Join(got, ", "); s != "Photos/, Photos/docs/, Photos/docs/a.txt, Photos/b.txt -> Photos/docs/a.txt" {
		t.Fatal(s)
	}
}
