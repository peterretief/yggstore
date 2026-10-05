package server_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestPushPeers(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "peers.json")
	// The loopback transport sees every caller as 127.0.0.1.
	peers.Write(base, []peers.Peer{{Name: "me", Addr: "127.0.0.1:1"}})
	live, err := peers.NewLive(base, filepath.Join(dir, "pushed.json"), "200::9")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{Transport: transport.Loopback{}, Peers: live}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	c := client.New()

	next := []peers.Peer{{Name: "me", Addr: "127.0.0.1:1", Admin: true}, {Name: "new", Addr: "[200::3]:7400"}}
	if err := c.PushPeers(context.Background(), addr, next); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("non-admin push: %v, want 403", err)
	}

	peers.Write(base, []peers.Peer{{Name: "me", Addr: "127.0.0.1:1", Admin: true}})
	live.Refresh()
	if err := c.PushPeers(context.Background(), addr, next); err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if info.PeersHash != peers.Hash(next) || !live.Allowed("200::3") {
		t.Fatalf("node holds list %s, want %s", info.PeersHash, peers.Hash(next))
	}
}
