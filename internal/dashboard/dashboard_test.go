package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestFileStatusFollowsNodes(t *testing.T) {
	dir := t.TempDir()
	var list []peers.Peer
	var srvs []*httptest.Server
	for i := range 6 {
		h := server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		srvs = append(srvs, srv)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	peersPath := filepath.Join(dir, "peers.json")
	b, _ := json.Marshal(list)
	os.WriteFile(peersPath, b, 0o600)

	data := make([]byte, 200_000)
	rand.Read(data)
	src := filepath.Join(dir, "f.bin")
	os.WriteFile(src, data, 0o600)
	c := client.New()
	m, chal, err := files.Put(context.Background(), c, src, list, files.PutOptions{ChunkSize: 64 << 10, Challenges: 2})
	if err != nil {
		t.Fatal(err)
	}
	files.WriteJSON(src+files.StubExt, m)
	files.WriteJSON(src+files.StubExt+files.ChallengesExt, chal)

	d := New(Config{PeersPath: peersPath, StubDir: dir, Client: c})
	status := func() string {
		d.poll(context.Background())
		if len(d.state.Files) != 1 {
			t.Fatalf("files = %d", len(d.state.Files))
		}
		return d.state.Files[0].Status
	}
	if s := status(); s != "healthy" {
		t.Fatalf("status %s, want healthy", s)
	}
	if n := d.state.Peers[0].Info.ShardCount; n == 0 {
		t.Fatal("shard count not reported")
	}
	srvs[0].Close()
	srvs[1].Close()
	if s := status(); s != "degraded" {
		t.Fatalf("status %s, want degraded", s)
	}
	srvs[2].Close()
	if s := status(); s != "lost" {
		t.Fatalf("status %s, want lost", s)
	}
	var sawDown bool
	for _, e := range d.events {
		if e.Kind == "down" {
			sawDown = true
		}
	}
	if !sawDown {
		t.Fatal("no down event recorded")
	}
}
