package files

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestDeleteKeepsStubUntilEveryShardIsGone(t *testing.T) {
	var list []peers.Peer
	var stores []localstore.Store
	var down atomic.Bool // makes node 0 answer 503, as if offline
	for i := range 6 {
		st := localstore.New(t.TempDir())
		stores = append(stores, st)
		h := server.Handler(st, server.Options{Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		if i == 0 {
			inner := h
			h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if down.Load() {
					http.Error(w, "down", http.StatusServiceUnavailable)
					return
				}
				inner.ServeHTTP(w, r)
			})
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "doc.bin")
	data := make([]byte, 2<<20+77)
	rand.Read(data)
	os.WriteFile(path, data, 0o600)
	c := client.New()
	m, chal, err := Put(context.Background(), c, path, list, PutOptions{ChunkSize: 1 << 20, Challenges: 2})
	if err != nil {
		t.Fatal(err)
	}
	stub := path + StubExt
	WriteJSON(stub, m)
	WriteJSON(ChallengesPath(stub), chal)
	total := 0
	for _, ch := range m.Chunks {
		total += len(ch.Shards)
	}

	down.Store(true)
	_, res, err := DeleteStub(context.Background(), c, stub)
	if err == nil || len(res.Failed) == 0 || res.Deleted+len(res.Failed) != total {
		t.Fatalf("with a node down: err=%v deleted=%d failed=%d, want a partial failure", err, res.Deleted, len(res.Failed))
	}
	if _, err := os.Stat(stub); err != nil {
		t.Fatal("stub removed although some shards could not be deleted")
	}

	down.Store(false)
	if _, res, err = DeleteStub(context.Background(), c, stub); err != nil || res.Deleted != total {
		t.Fatalf("retry: err=%v deleted=%d, want all %d", err, res.Deleted, total)
	}
	for _, p := range []string{stub, ChallengesPath(stub)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still present after a complete delete", filepath.Base(p))
		}
	}
	for i, st := range stores {
		if n, _, _ := st.Stats(); n != 0 {
			t.Fatalf("node %d still holds %d shards", i, n)
		}
	}
}
