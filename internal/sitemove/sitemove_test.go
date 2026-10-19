package sitemove

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/transport"
)

func storageNodes(t *testing.T, n int) []peers.Peer {
	var list []peers.Peer
	for i := range n {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	return list
}

type noAnnounce struct{}

func (noAnnounce) Publish(context.Context, string, string, string) error { return nil }

// A site's versions go from one publisher to another, sealed for the new
// holder, and the old one lets go.
func TestHandOver(t *testing.T) {
	ctx := context.Background()
	nodes := storageNodes(t, 6)
	c := client.New()
	oldDir, newDir := t.TempDir(), t.TempDir()
	oldPub := &site.Publisher{Dir: oldDir, Client: c, Announce: noAnnounce{}}
	src := t.TempDir()
	for i, body := range []string{"<h1>one</h1>", "<h1>two</h1>"} {
		os.WriteFile(filepath.Join(src, "index.html"), []byte(body), 0o644)
		if _, _, err := oldPub.Publish(ctx, "shop.example.org", src, nodes); err != nil {
			t.Fatal(i, err)
		}
	}
	oldID, _ := share.LoadOrCreate(filepath.Join(t.TempDir(), "old.key"))
	newID, _ := share.LoadOrCreate(filepath.Join(t.TempDir(), "new.key"))
	if err := oldPub.SetContact(ctx, "shop.example.org", oldID.Code()); err != nil {
		t.Fatal(err)
	}

	b, err := Export(oldDir, "shop.example.org")
	if err != nil || len(b.Files) != 2 || !b.Contact {
		t.Fatalf("export: %v, %d files, contact %v", err, len(b.Files), b.Contact)
	}
	sealed, err := Pack(ctx, c, nodes, oldID, "old box", newID.Code(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(Body(Handover{Site: b.Site, Sealed: sealed})) > 64<<10 {
		t.Fatal("the handover doesn't fit in a message")
	}
	if _, _, err := Unpack(ctx, c, oldID, sealed); err == nil {
		t.Fatal("someone else opened the handover")
	}
	got, m, err := Unpack(ctx, c, newID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	Forget(ctx, c, m)
	if err := Import(newDir, got, newID.Code()); err != nil {
		t.Fatal(err)
	}
	if err := Import(newDir, got, newID.Code()); err == nil {
		t.Fatal("imported twice")
	}

	newPub := &site.Publisher{Dir: newDir, Client: c, Announce: noAnnounce{}}
	vs, err := newPub.Versions("shop.example.org")
	if err != nil || len(vs) != 2 || !vs[0].Current {
		t.Fatalf("versions at the new holder: %v %+v", err, vs)
	}
	if newPub.Contact("shop.example.org") != newID.Code() {
		t.Fatal("the contact form's messages aren't sealed for the new holder")
	}
	if _, err := newPub.Rollback(ctx, "shop.example.org", ""); err != nil {
		t.Fatalf("rolling back at the new holder: %v", err)
	}

	now := time.Now()
	if err := LetGo(oldDir, "shop.example.org", "Tim on localmail", now); err != nil {
		t.Fatal(err)
	}
	if s := oldPub.Sites(); len(s) != 0 {
		t.Fatalf("the old holder still publishes %v", s)
	}
	if to, _, ok := Moved(oldDir, "shop.example.org"); !ok || to != "Tim on localmail" {
		t.Fatalf("moved to %q %v", to, ok)
	}
	Prune(oldDir, now.Add(Keep/2))
	if ents, _ := os.ReadDir(filepath.Join(oldDir, MovedDir)); len(ents) != 2 {
		t.Fatalf("pruned too soon: %d", len(ents))
	}
	Prune(oldDir, now.Add(Keep+time.Hour))
	if ents, _ := os.ReadDir(filepath.Join(oldDir, MovedDir)); len(ents) != 1 {
		t.Fatalf("not pruned after %v: %d", Keep, len(ents))
	}
	var mv moved
	b2, _ := os.ReadFile(filepath.Join(oldDir, MovedDir, "shop.example.org.json"))
	if json.Unmarshal(b2, &mv) != nil || mv.To == "" {
		t.Fatal("the note of where it went is gone")
	}
}
