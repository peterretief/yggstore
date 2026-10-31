package site

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPullData(t *testing.T) {
	// A web node with 600 entries (two batches), and one that refuses.
	all := make([]Entry, 600)
	for i := range all {
		all[i] = Entry{ID: fmt.Sprintf("a/%d", i+1), Time: int64(1000 + i), Node: "a", Seq: int64(i + 1), Kind: "register", Fields: map[string]any{"n": i}}
	}
	giver := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("site") != "shop.example.org" {
			http.Error(rw, "no", http.StatusNotFound)
			return
		}
		got, _ := strconv.ParseInt(r.URL.Query().Get("got"), 10, 64)
		var out []Entry
		for _, e := range all {
			if e.Time > got && len(out) < pullBatch {
				out = append(out, e)
			}
		}
		last := got
		if len(out) > 0 {
			last = out[len(out)-1].Time
		}
		json.NewEncoder(rw).Encode(map[string]any{"entries": out, "got": last})
	}))
	defer giver.Close()
	refuser := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "only the group's web nodes", http.StatusForbidden)
	}))
	defer refuser.Close()
	addr := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

	folder := t.TempDir()
	old := filepath.Join(folder, DevDir, "data", "shop.example.org")
	os.MkdirAll(old, 0o700)
	os.WriteFile(filepath.Join(old, "marker"), []byte("test data"), 0o600)

	if _, _, err := PullData(context.Background(), http.DefaultClient, []string{addr(refuser)}, "shop.example.org", t.TempDir()); err == nil || !strings.Contains(err.Error(), "machine that publishes") {
		t.Fatalf("only refusals: %v", err)
	}
	n, from, err := PullData(context.Background(), http.DefaultClient, []string{addr(refuser), addr(giver)}, "shop.example.org", folder)
	if err != nil || n != 600 || from != 1 {
		t.Fatalf("pulled %d from %d: %v", n, from, err)
	}
	d, err := openSiteData(old)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if got, _, _ := d.since(0, 1000); len(got) != 600 {
		t.Fatalf("the dev log has %d entries", len(got))
	}
	if _, err := os.Stat(filepath.Join(old, "marker")); err == nil {
		t.Fatal("the old data was mixed in, not set aside")
	}
	aside, _ := filepath.Glob(old + ".before-pull-*")
	if len(aside) != 1 {
		t.Fatalf("set aside: %v", aside)
	}
}
