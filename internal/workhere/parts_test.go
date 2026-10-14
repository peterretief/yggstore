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
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

// A big file comes in parts, possibly to different web nodes, and is
// collected as one.
func TestUploadInParts(t *testing.T) {
	ctx := context.Background()
	var nodes []peers.Peer
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		nodes = append(nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	out := t.TempDir()
	os.MkdirAll(filepath.Join(out, "files"), 0o755)
	k := recovery.New(recovery.Config{Dir: filepath.Join(t.TempDir(), "recovery"), Client: client.New(),
		Sources: []recovery.Source{{Name: "outbox", Path: out, Keep: recovery.OutboxKeep}},
		Online:  func(context.Context) ([]peers.Peer, error) { return nodes, nil }})
	if _, err := k.SaveNow(ctx); err != nil {
		t.Fatal(err)
	}
	ks := k.Keys()
	online := func(context.Context) ([]peers.Peer, error) { return nodes, nil }
	// Two web nodes with the person's record; the parts go to both.
	var stores []*Store
	for range 2 {
		s := &Store{Dir: t.TempDir(), Online: online}
		b, _ := json.Marshal(Record{ID: ks.ID, GroupHalf: ks.GroupHalf, Sealed: ks.Sealed, Name: "anna"})
		s.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/work/"+ks.ID, strings.NewReader(string(b))), "200::1")
		stores = append(stores, s)
	}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stores[1].Serve(w, r, "200::1") {
			http.NotFound(w, r)
		}
	}))
	defer web.Close()

	part := func(s *Store, upload, body string) []byte {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, PagePath+"?op=put", strings.NewReader(body))
		r.Header.Set("X-Id", ks.ID)
		r.Header.Set("X-Link", ks.LinkHalf)
		r.Header.Set("X-Dir", "Films")
		r.Header.Set("X-Name", url.PathEscape("big film.mkv"))
		r.Header.Set("X-Upload", upload)
		s.ServeHTTP(w, r)
		var got struct{ Part []byte }
		if json.Unmarshal(w.Body.Bytes(), &got); w.Code != 200 || got.Part == nil {
			t.Fatalf("part: %d %s", w.Code, w.Body)
		}
		return got.Part
	}
	finish := func(s *Store, op, upload string, parts [][]byte) int {
		b, _ := json.Marshal(map[string]any{"id": ks.ID, "link": ks.LinkHalf, "upload": upload, "dir": "Films", "name": "big film.mkv", "parts": parts})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, PagePath+"?op="+op, strings.NewReader(string(b))))
		return w.Code
	}
	const up, other = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	p1 := part(stores[0], up, strings.Repeat("a", 5000))
	p2 := part(stores[1], up, strings.Repeat("b", 3000))
	stray := part(stores[0], other, "from another upload")

	if code := finish(stores[0], "parts", up, [][]byte{p1, stray}); code != http.StatusBadRequest {
		t.Fatalf("joined a part from another upload: %d", code)
	}
	w := httptest.NewRecorder()
	b, _ := json.Marshal(map[string]any{"id": ks.ID, "link": strings.Repeat("A", 43), "upload": up, "dir": "Films", "name": "x", "parts": [][]byte{p1}})
	stores[0].ServeHTTP(w, httptest.NewRequest(http.MethodPost, PagePath+"?op=parts", strings.NewReader(string(b))))
	if w.Code != http.StatusForbidden {
		t.Fatalf("parts with a wrong link: %d", w.Code)
	}
	if code := finish(stores[1], "parts", up, [][]byte{p1, p2}); code != 200 {
		t.Fatalf("joining: %d", code)
	}

	// Giving up deletes what was stored.
	key, _ := recovery.Key(ks.LinkHalf, ks.GroupHalf)
	stub, _ := recovery.Open(key, stray)
	sm, _ := manifest.Unmarshal(stub)
	if code := finish(stores[0], "parts-drop", other, [][]byte{stray}); code != 200 {
		t.Fatalf("dropping: %d", code)
	}
	if err := files.Get(ctx, client.New(), sm, new(strings.Builder), func(string, ...any) {}); err == nil {
		t.Fatal("a dropped part can still be fetched")
	}

	n, err := Collect(ctx, client.New(), []string{strings.TrimPrefix(web.URL, "http://")}, ks, out, nil)
	if err != nil || n != 1 {
		t.Fatalf("collected %d: %v", n, err)
	}
	got, _ := os.ReadFile(filepath.Join(out, "files", "Films", "big film.mkv"))
	if string(got) != strings.Repeat("a", 5000)+strings.Repeat("b", 3000) {
		t.Fatalf("collected %d bytes", len(got))
	}
}
