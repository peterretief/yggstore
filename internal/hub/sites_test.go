package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

func TestScanLeavesOutSyncthing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".well-known"), 0o755)
	os.WriteFile(filepath.Join(dir, ".well-known", "security.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".stversions"), 0o755)
	os.WriteFile(filepath.Join(dir, ".stversions", "old.html"), []byte("old old old"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".stfolder"), 0o755)
	os.WriteFile(filepath.Join(dir, ".stignore"), []byte(""), 0o644)
	sc := scanSite(dir)
	if sc.files != 2 || sc.bytes != 6 || !sc.index || sc.temp {
		t.Fatalf("scan %+v; want index.html and .well-known/security.txt only", sc)
	}
	os.WriteFile(filepath.Join(dir, ".syncthing.big.jpg.tmp"), []byte("half"), 0o644)
	if !scanSite(dir).temp {
		t.Fatal("a file Syncthing is writing wasn't noticed")
	}
}

// TestBrokenSiteNotPublished: no index.html, or too big, and the site isn't
// published, until it is forced.
func TestBrokenSiteNotPublished(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	sitePath := filepath.Join(dir, "site")
	os.MkdirAll(sitePath, 0o755)
	os.WriteFile(filepath.Join(sitePath, "about.html"), []byte(strings.Repeat("x", 2000)), 0o644)
	ctx := context.Background()
	Update(ctx, statePath, func(s *HubState) error {
		s.Sites = []Site{{Name: "shop.example.org", Path: sitePath, Seeded: true}}
		return nil
	})
	tried := 0
	s := NewSites(SitesConfig{StatePath: statePath, Pub: &site.Publisher{Dir: filepath.Join(dir, "pub"), Client: client.New()},
		MaxBytes: 1000, Gap: time.Hour, Logf: t.Logf,
		Online: func(context.Context) ([]peers.Peer, error) { tried++; return nil, errors.New("no group here") }})
	settle := func() {
		s.step(ctx)
		time.Sleep(time.Millisecond)
		s.step(ctx)
	}
	problem := func() string { st, _ := Load(statePath); return st.Sites[0].Problem }

	settle()
	if tried != 0 || !strings.Contains(problem(), "index.html") {
		t.Fatalf("tried %d, problem %q; want no publish for lack of index.html", tried, problem())
	}
	os.WriteFile(filepath.Join(sitePath, "index.html"), []byte("home"), 0o644)
	settle()
	if tried != 0 || !strings.Contains(problem(), "MB") {
		t.Fatalf("tried %d, problem %q; want no publish over the size cap", tried, problem())
	}
	Update(ctx, statePath, func(s *HubState) error { s.Sites[0].Force = true; return nil })
	settle()
	if tried != 1 || !strings.Contains(problem(), "no group here") {
		t.Fatalf("tried %d, problem %q; want a forced publish", tried, problem())
	}
}
