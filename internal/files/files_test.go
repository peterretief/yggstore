package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

type node struct {
	srv  *httptest.Server
	dir  string
	peer peers.Peer
}

func startNodes(t *testing.T, n int) []*node {
	t.Helper()
	var nodes []*node
	for i := range n {
		dir := t.TempDir()
		h := server.Handler(localstore.New(dir), server.Options{
			Name: "n", Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "http://")
		nodes = append(nodes, &node{srv: srv, dir: dir, peer: peers.Peer{Name: string(rune('a' + i)), Addr: addr}})
	}
	return nodes
}

func putFile(t *testing.T, nodes []*node, data []byte) (Challenges, func() []byte, manifest.Manifest) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var list []peers.Peer
	for _, n := range nodes {
		list = append(list, n.peer)
	}
	c := client.New()
	m, chal, err := Put(context.Background(), c, path, list, PutOptions{ChunkSize: 1 << 20, Challenges: 3})
	if err != nil {
		t.Fatal(err)
	}
	get := func() []byte {
		var buf bytes.Buffer
		if err := Get(context.Background(), c, m, &buf, nil); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	return chal, get, m
}

func TestRoundTripSurvivesTwoNodesDown(t *testing.T) {
	nodes := startNodes(t, 6)
	data := make([]byte, 3<<20+12345)
	rand.Read(data)
	_, get, m := putFile(t, nodes, data)
	if len(m.Chunks) != 4 {
		t.Fatalf("chunks = %d, want 4", len(m.Chunks))
	}
	for _, ch := range m.Chunks {
		seen := map[string]bool{}
		for _, ref := range ch.Shards {
			if seen[ref.Peer] {
				t.Fatalf("two shards of one chunk on %s", ref.Peer)
			}
			seen[ref.Peer] = true
		}
	}
	nodes[0].srv.Close()
	nodes[3].srv.Close()
	if !bytes.Equal(get(), data) {
		t.Fatal("restored data differs")
	}
}

func TestThreeNodesDownFails(t *testing.T) {
	nodes := startNodes(t, 6)
	data := make([]byte, 100_000)
	rand.Read(data)
	_, _, m := putFile(t, nodes, data)
	for _, i := range []int{0, 1, 2} {
		nodes[i].srv.Close()
	}
	err := Get(context.Background(), client.New(), m, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "need 4") {
		t.Fatalf("want not-enough-shards error, got %v", err)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	nodes := startNodes(t, 6)
	data := make([]byte, 50_000)
	rand.Read(data)
	chal, _, m := putFile(t, nodes, data)

	results := Verify(context.Background(), client.New(), m, &chal)
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("clean shard failed: %v", r.Err)
		}
	}
	// Corrupt the shard on whichever node holds shard 0.
	ref := m.Chunks[0].Shards[0]
	for _, n := range nodes {
		if n.peer.Addr == ref.Peer {
			p := filepath.Join(n.dir, ref.Hash)
			b, _ := os.ReadFile(p)
			b[0] ^= 0xff
			os.Chmod(p, 0o600)
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	failed := 0
	for _, r := range Verify(context.Background(), client.New(), m, &chal) {
		if r.Err != nil {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
}

func TestUnlistedCallerDenied(t *testing.T) {
	h := server.Handler(localstore.New(t.TempDir()), server.Options{
		Transport: transport.Loopback{}, Allowed: map[string]bool{"::1": true}})
	srv := httptest.NewServer(h) // client connects from 127.0.0.1, not on the list
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestYggdrasilTransportRejectsNonOverlay(t *testing.T) {
	y := transport.Yggdrasil{}
	if _, err := y.Identify("127.0.0.1:1234"); err == nil {
		t.Fatal("loopback accepted as overlay caller")
	}
	id, err := y.Identify("[200:1234:5678::1]:7400")
	if err != nil || id != "200:1234:5678::1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := y.Identify("[fd00::1]:1"); err == nil {
		t.Fatal("non-200::/7 IPv6 accepted")
	}
}

func TestSafeRelRejectsEscapes(t *testing.T) {
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../x", "..", "."} {
		if _, err := safeRel(bad); err == nil {
			t.Errorf("safeRel(%q) accepted", bad)
		}
	}
	for _, good := range []string{"a", "a/b/c.txt", "dir/", "a/./b"} {
		if _, err := safeRel(good); err != nil {
			t.Errorf("safeRel(%q) rejected: %v", good, err)
		}
	}
}
