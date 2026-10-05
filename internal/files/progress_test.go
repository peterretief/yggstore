package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

func TestProgressCountsEveryByte(t *testing.T) {
	var list []peers.Peer
	for i := range 6 {
		h := server.Handler(localstore.New(t.TempDir()), server.Options{Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	data := make([]byte, 3<<20+123)
	rand.Read(data)
	c := client.New()

	var up atomic.Int64
	ctx := WithProgress(context.Background(), func(n int) { up.Add(int64(n)) })
	m, _, _, err := PutReader(ctx, c, bytes.NewReader(data), "p.bin", list, PutOptions{ChunkSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if up.Load() != int64(len(data)) {
		t.Fatalf("upload progress %d, want %d", up.Load(), len(data))
	}

	var down atomic.Int64
	var out bytes.Buffer
	if err := Get(WithProgress(context.Background(), func(n int) { down.Add(int64(n)) }), c, m, &out, nil); err != nil {
		t.Fatal(err)
	}
	if down.Load() != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("download progress %d, want %d", down.Load(), len(data))
	}
}
