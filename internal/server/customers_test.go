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

// A gateway can only store shards on nodes whose owner opted in.
func TestGatewayNeedsOptIn(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "peers.json")
	// The loopback transport sees every caller as 127.0.0.1: the gateway here.
	peers.Write(base, []peers.Peer{{Name: "gw", Addr: "127.0.0.1:1", Gateway: true}})
	live, err := peers.NewLive(base, filepath.Join(dir, "pushed.json"), "200::9")
	if err != nil {
		t.Fatal(err)
	}
	c := client.New()
	for _, optIn := range []bool{false, true} {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()),
			server.Options{Transport: transport.Loopback{}, Peers: live, Customers: optIn}))
		addr := strings.TrimPrefix(srv.URL, "http://")
		_, err := c.Put(context.Background(), addr, []byte("customer shard"))
		if optIn && err != nil {
			t.Fatalf("opted-in node refused: %v", err)
		}
		if !optIn && (err == nil || !strings.Contains(err.Error(), "403")) {
			t.Fatalf("node that didn't opt in: %v, want 403", err)
		}
		info, err := c.Info(context.Background(), addr)
		if err != nil || info.Customers != optIn {
			t.Fatalf("info.Customers = %v (%v), want %v", info.Customers, err, optIn)
		}
		srv.Close()
	}
}
