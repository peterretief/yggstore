package workhere

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

// Deleting, moving, renaming and new folders on the page show at once,
// on every web node, and the place holding the working copy makes them.
func TestChangesAreMade(t *testing.T) {
	ctx := context.Background()
	var nodes []peers.Peer
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		nodes = append(nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	out := t.TempDir()
	for _, f := range []string{"Docs/report.txt", "Docs/old.txt", "Photos/a.jpg"} {
		m, _, _, err := files.PutReader(ctx, client.New(), strings.NewReader("content of "+f), filepath.Base(f), nodes, files.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(out, "files", filepath.Dir(f)), 0o755)
		files.WriteJSON(filepath.Join(out, "files", f+files.StubExt), m)
	}
	os.MkdirAll(filepath.Join(out, "files", "Empty"), 0o755)
	k := recovery.New(recovery.Config{Dir: filepath.Join(t.TempDir(), "recovery"), Client: client.New(),
		Sources: []recovery.Source{{Name: "outbox", Path: out, Keep: recovery.OutboxKeep}},
		Online:  func(context.Context) ([]peers.Peer, error) { return nodes, nil }})
	if _, err := k.SaveNow(ctx); err != nil {
		t.Fatal(err)
	}
	ks := k.Keys()
	rec, _ := json.Marshal(Record{ID: ks.ID, GroupHalf: ks.GroupHalf, Sealed: ks.Sealed, Name: "anna"})
	online := func(context.Context) ([]peers.Peer, error) { return nodes, nil }
	// Two web nodes: changes are made on one, and the other's page shows them.
	s := &Store{Dir: t.TempDir(), Online: online}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Serve(w, r, "200::1") {
			http.NotFound(w, r)
		}
	}))
	defer web.Close()
	webAddr := strings.TrimPrefix(web.URL, "http://")
	s2 := &Store{Dir: t.TempDir(), Online: online, Others: func(context.Context) []string { return []string{webAddr} }}
	for _, st := range []*Store{s, s2} {
		st.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/work/"+ks.ID, strings.NewReader(string(rec))), "200::1")
	}

	change := func(link string, ch Change) (int, string) {
		w := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"id": ks.ID, "link": link, "op": ch.Op, "path": ch.Path, "to": ch.To})
		r := httptest.NewRequest(http.MethodPost, PagePath+"?op=change", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	names := func(st *Store, dir string) string {
		w := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"id": ks.ID, "link": ks.LinkHalf, "path": dir})
		r := httptest.NewRequest(http.MethodPost, PagePath+"?op=list", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		st.ServeHTTP(w, r)
		if w.Code != 200 {
			return "error " + w.Body.String()
		}
		var l Listing
		json.Unmarshal(w.Body.Bytes(), &l)
		var got []string
		for _, e := range l.Entries {
			n := e.Name
			if e.Folder {
				n += "/"
			}
			if e.Note != "" {
				n += " (" + e.Note + ")"
			}
			got = append(got, n)
		}
		return strings.Join(got, ", ")
	}

	if got := names(s, ""); got != "Docs/, Empty/, Photos/" {
		t.Fatalf("top before: %s", got)
	}
	for _, c := range []Change{
		{Op: OpFolder, Path: "Projects"},
		{Op: OpMove, Path: "Docs/report.txt", To: "Projects/report.txt"},
		{Op: OpMove, Path: "Docs/old.txt", To: "Docs/older.txt"},
		{Op: OpDelete, Path: "Photos"},
		{Op: OpMove, Path: "Empty", To: "Projects/Empty"},
	} {
		if code, body := change(ks.LinkHalf, c); code != 200 {
			t.Fatalf("%+v: %d %s", c, code, body)
		}
	}
	for _, c := range []Change{
		{Op: OpMove, Path: "Projects", To: "Projects/inside"},
		{Op: OpDelete, Path: "Photos"},
		{Op: OpFolder, Path: "Kept whole/x"},
		{Op: OpFolder, Path: "Docs"},
		{Op: OpMove, Path: "Docs/older.txt", To: "Projects/report.txt"},
		{Op: OpMove, Path: "Docs/older.txt", To: "Nowhere/older.txt"},
		{Op: OpFolder, Path: ".hidden"},
		{Op: OpDelete, Path: "Kept whole"},
	} {
		if code, _ := change(ks.LinkHalf, c); code != http.StatusBadRequest {
			t.Errorf("%+v taken: %d", c, code)
		}
	}
	if code, _ := change(strings.Repeat("A", 43), Change{Op: OpDelete, Path: "Docs"}); code != http.StatusForbidden {
		t.Fatalf("a change with a wrong link: %d", code)
	}

	want := map[string]string{
		"":         "Docs/, Projects/",
		"Docs":     "older.txt (moving…)",
		"Projects": "Empty/ (moving…), report.txt (moving…)",
	}
	for _, st := range []*Store{s, s2} {
		for dir, w := range want {
			if got := names(st, dir); got != w {
				t.Errorf("before it's made, %q shows %q, want %q", dir, got, w)
			}
		}
	}

	n, err := Collect(ctx, client.New(), []string{webAddr}, ks, out, nil)
	if err != nil || n != 5 {
		t.Fatalf("made %d: %v", n, err)
	}
	var have []string
	filepath.WalkDir(out, func(p string, e os.DirEntry, _ error) error {
		rel, _ := filepath.Rel(out, p)
		if !strings.HasPrefix(rel, ".yggstore") && rel != "." {
			if e.IsDir() {
				rel += "/"
			}
			have = append(have, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(have)
	if got := strings.Join(have, " "); !strings.Contains(got, "files/Projects/report.txt.ystub") ||
		!strings.Contains(got, "files/Docs/older.txt.ystub") || !strings.Contains(got, "files/Projects/Empty/") ||
		strings.Contains(got, "files/Photos") || strings.Contains(got, "files/Empty") || !strings.Contains(got, "a.jpg.ystub") {
		t.Fatalf("outbox after: %s", got)
	}
	if b, _ := os.ReadDir(filepath.Join(out, "delete")); len(b) != 1 {
		t.Fatalf("delete/ holds %d", len(b))
	}

	// Made, and not in a copy yet: still shown, as it now is.
	want["Docs"], want["Projects"] = "older.txt", "Empty/, report.txt"
	for dir, w := range want {
		if got := names(s, dir); got != w {
			t.Errorf("after it's made, %q shows %q, want %q", dir, got, w)
		}
	}
	// The moved file downloads from its new place.
	form := url.Values{"id": {ks.ID}, "link": {ks.LinkHalf}, "path": {"Projects/report.txt"}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, PagePath+"?op=get", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(w, r)
	if w.Body.String() != "content of Docs/report.txt" {
		t.Fatalf("download after the move: %d %q", w.Code, w.Body)
	}
	// Nothing is made twice.
	if n, err := Collect(ctx, client.New(), []string{webAddr}, ks, out, nil); n != 0 || err != nil {
		t.Fatalf("made %d again: %v", n, err)
	}
}

// The folders shown beside a person's files are never changed themselves,
// whatever a queue holds.
func TestTopFoldersStay(t *testing.T) {
	out := t.TempDir()
	os.MkdirAll(filepath.Join(out, "whole"), 0o755)
	os.WriteFile(filepath.Join(out, "whole", "Photos.ystub"), []byte("{}"), 0o644)
	for _, p := range []string{"Kept whole", "/Kept whole/", "Sent to you"} {
		plain, _ := json.Marshal(Change{Op: OpDelete, Path: p})
		if did, err := apply(context.Background(), client.New(), Waiting{Op: OpDelete}, plain, out); did != "" || err != nil {
			t.Fatalf("delete %q: %q %v", p, did, err)
		}
	}
	if !exists(filepath.Join(out, "whole", "Photos.ystub")) {
		t.Fatal("Kept whole was emptied")
	}
}
