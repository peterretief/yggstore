package site

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
)

func TestDev(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.md"), []byte("# Home\n\n[About](about.md)\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "about.md"), []byte("# About us\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "404.html"), []byte("<html><body>lost</body></html>"), 0o644)
	d := &Dev{Folder: dir, Name: "shop.example.org", Log: t.Logf}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8484"+path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := get("/"); code != 200 || !strings.Contains(body, `href="/about/"`) || !strings.Contains(body, devReloadPath) {
		t.Fatalf("home: %d %s", code, body)
	}
	if code, body := get("/about/"); code != 200 || !strings.Contains(body, "About us") {
		t.Fatalf("about: %d %s", code, body)
	}
	if code, body := get("/nope"); code != 404 || !strings.Contains(body, "lost") {
		t.Fatalf("missing: %d %s", code, body)
	}
	if code, _ := get("/" + DevDir + "/"); code != 404 {
		t.Fatalf("its own data served: %d", code)
	}
	_, before := get(devReloadPath)

	// A change is built again.
	os.WriteFile(filepath.Join(dir, "about.md"), []byte("# About us, again\n"), 0o644)
	if err := d.build(); err != nil {
		t.Fatal(err)
	}
	if _, after := get(devReloadPath); after == before {
		t.Fatal("no new build")
	}
	if _, body := get("/about/"); !strings.Contains(body, "again") {
		t.Fatalf("not rebuilt: %s", body)
	}

	// The contact form answers as the web nodes do.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8484"+ContactPath, strings.NewReader(url.Values{"message": {"hi"}, "_next": {"/thanks/"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/thanks/" {
		t.Fatalf("contact: %d %v", rec.Code, rec.Header())
	}
}

// A folder's local web node is never published.
func TestDevDirNotPublished(t *testing.T) {
	nodes := storageNodes(t, 6)
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: newBus(), Log: t.Logf}
	src := t.TempDir()
	writeSite(t, src, map[string]string{"index.html": "<h1>x</h1>", DevDir + "/data/x/log.db": "private", ".well-known/security.txt": "x"})
	if _, _, err := pub.Publish(context.Background(), "example.org", src, nodes); err != nil {
		t.Fatal(err)
	}
	m, _ := pub.Current("example.org")
	if m.FileCount != 2 {
		t.Fatalf("published %d files, want index.html and .well-known only", m.FileCount)
	}
}
