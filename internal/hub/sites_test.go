package hub

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/websites"
)

func TestSyncthingFile(t *testing.T) {
	for rel, want := range map[string]bool{"index.html": false, ".stfolder": true, "img/.stversions": true,
		".syncthing.big.jpg.tmp": true, "~syncthing~x.tmp": true, ".well-known": false} {
		if SyncthingFile(rel, false) != want {
			t.Errorf("SyncthingFile(%q) = %v", rel, !want)
		}
	}
}

// TestOldSitesMoveIntoWebsites: a site kept the old way, in a Syncthing
// folder of its own, moves into websites/ without Syncthing's files, and a
// forced publish asked for is passed on to the watcher.
func TestOldSitesMoveIntoWebsites(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	statePath := filepath.Join(dir, "hub.json")
	old := filepath.Join(dir, "sites", "shop.example.org")
	os.MkdirAll(filepath.Join(old, ".stfolder"), 0o755)
	os.WriteFile(filepath.Join(old, "index.html"), []byte("shop"), 0o644)
	Update(ctx, statePath, func(s *HubState) error {
		s.Sites = []Site{{Name: "shop.example.org", Folder: "yggstore-site-shop.example.org", Path: old, Force: true}}
		return nil
	})
	w := websites.New(websites.Config{Dir: filepath.Join(dir, "websites"),
		Pub: &site.Publisher{Dir: filepath.Join(dir, "pub"), Client: client.New()}, Client: client.New()})
	h := &Websites{W: w, StatePath: statePath, Logf: t.Logf}
	h.moveOldSites()

	moved := filepath.Join(dir, "websites", "shop.example.org")
	if b, err := os.ReadFile(filepath.Join(moved, "index.html")); err != nil || string(b) != "shop" {
		t.Fatalf("site not moved: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(moved, ".stfolder")); !os.IsNotExist(err) {
		t.Fatal("Syncthing's marker came along")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the old folder is still there")
	}

	h.sync(ctx)
	st, _ := Load(statePath)
	if len(st.Sites) != 1 || st.Sites[0].Force || st.Sites[0].Folder == "" {
		t.Fatalf("sites %+v; want the force passed on, the old folder kept until Users drops it", st.Sites)
	}
}
