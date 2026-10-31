package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSiteInitAndPush(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755)
	if err := cmdSiteInit(dir, "", "", "", ""); err == nil {
		t.Fatal("init without a name and box")
	}
	var got struct {
		path, preview string
		files         []string
	}
	box := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		got.path, got.preview = r.URL.Query().Get("site"), r.URL.Query().Get("preview")
		if r.Header.Get("X-Yggstore") != "1" {
			http.Error(rw, "no header", http.StatusForbidden)
			return
		}
		f, _, err := r.FormFile("zip")
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(f)
		zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		got.files = nil
		for _, zf := range zr.File {
			got.files = append(got.files, zf.Name)
		}
		sort.Strings(got.files)
		rw.Write([]byte(`{"preview_link":"https://shop.example.org/_yggstore/preview?key=k"}`))
	}))
	defer box.Close()

	if err := cmdSiteInit(dir, "Shop.Example.org", box.URL, "mkdir -p dist && echo built > dist/index.html", "dist"); err != nil {
		t.Fatal(err)
	}
	hook, err := os.ReadFile(filepath.Join(dir, ".git", "hooks", "pre-push"))
	if err != nil || !strings.Contains(string(hook), "yggstore site push -preview") {
		t.Fatalf("hook: %v %s", err, hook)
	}
	if ignore, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); !strings.Contains(string(ignore), ".yggstore-dev/") {
		t.Fatalf(".gitignore: %q", ignore)
	}
	// Run from a folder below the project: yggstore.json is found above.
	sub := filepath.Join(dir, "src")
	os.MkdirAll(sub, 0o755)
	os.MkdirAll(filepath.Join(dir, "dist", ".yggstore-dev"), 0o755)
	os.WriteFile(filepath.Join(dir, "dist", ".yggstore-dev", "x"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "dist", ".env"), []byte("SECRET=1"), 0o644)
	os.MkdirAll(filepath.Join(dir, "dist", "img"), 0o755)
	os.WriteFile(filepath.Join(dir, "dist", "img", "a.png"), []byte("png"), 0o644)
	if err := cmdSitePush(context.Background(), sub, true); err != nil {
		t.Fatal(err)
	}
	if got.path != "shop.example.org" || got.preview != "1" || strings.Join(got.files, ",") != "img/a.png,index.html" {
		t.Fatalf("the box got %+v", got)
	}

	// A failing build sends nothing.
	os.WriteFile(filepath.Join(dir, "yggstore.json"), []byte(`{"site":"shop.example.org","box":"`+box.URL+`","build":"exit 3","output":"dist"}`), 0o644)
	got.files = nil
	if err := cmdSitePush(context.Background(), dir, false); err == nil || got.files != nil {
		t.Fatalf("a failed build: %v, sent %v", err, got.files)
	}
}
