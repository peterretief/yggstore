package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

// TestGetSkipsSlowAndDownNodes: with 6 nodes, one down and one very slow,
// every chunk still rebuilds from the 4 that answer quickly; with 3 down it
// fails instead of returning bad data.
func TestGetSkipsSlowAndDownNodes(t *testing.T) {
	var list []peers.Peer
	var down [6]atomic.Bool
	var slow atomic.Bool
	for i := range 6 {
		inner := server.Handler(localstore.New(t.TempDir()), server.Options{Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if down[i].Load() {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			if i == 1 && slow.Load() && r.Method == http.MethodGet {
				select {
				case <-time.After(5 * time.Second):
				case <-r.Context().Done():
					return
				}
			}
			inner.ServeHTTP(w, r)
		})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	data := make([]byte, 5<<20+9)
	rand.Read(data)
	c := client.New()
	m, _, _, err := PutReader(context.Background(), c, bytes.NewReader(data), "g.bin", list, PutOptions{ChunkSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}

	down[0].Store(true)
	slow.Store(true)
	start := time.Now()
	var out bytes.Buffer
	if err := Get(context.Background(), c, m, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("rebuilt data differs")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s: waited for the slow node", d)
	}

	down[2].Store(true)
	down[3].Store(true)
	if err := Get(context.Background(), c, m, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("rebuilt with only 3 of 6 nodes reachable")
	}
}
