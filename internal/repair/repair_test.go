package repair

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

type group struct {
	t      *testing.T
	srvs   []*httptest.Server
	stores []localstore.Store
	list   []peers.Peer
	c      client.Client
	dir    string
	state  string
}

func newGroup(t *testing.T, n int) *group {
	g := &group{t: t, c: client.New(), dir: t.TempDir()}
	g.state = filepath.Join(g.dir, "repair.json")
	for i := range n {
		store := localstore.New(t.TempDir())
		g.stores = append(g.stores, store)
		h := server.Handler(store, server.Options{Name: "n", Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		g.srvs = append(g.srvs, srv)
		g.list = append(g.list, peers.Peer{Name: fmt.Sprint("n", i), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	return g
}

func (g *group) put(name string, data []byte) (string, manifest.Manifest) {
	m, _, _, err := files.PutReader(context.Background(), g.c, bytes.NewReader(data), name, g.list, files.PutOptions{ChunkSize: 64 << 10})
	if err != nil {
		g.t.Fatal(err)
	}
	stub := filepath.Join(g.dir, name+files.StubExt)
	if err := files.WriteJSON(stub, m); err != nil {
		g.t.Fatal(err)
	}
	return stub, m
}

func (g *group) repairer(grace time.Duration) *Repairer {
	return &Repairer{Client: g.c, State: g.state, Grace: grace,
		Peers: func() ([]peers.Peer, error) { return g.list, nil },
		Stubs: func() []string { s, _ := filepath.Glob(filepath.Join(g.dir, "*"+files.StubExt)); return s },
		Log:   func(kind, msg string) { g.t.Log(kind, msg) }}
}

// downSince pretends node i went down at t.
func (g *group) downSince(i int, t time.Time) {
	b, _ := json.Marshal(state{DownSince: map[string]int64{g.list[i].Addr: t.Unix()}})
	os.WriteFile(g.state, b, 0o600)
}

func (g *group) get(m manifest.Manifest) []byte {
	var buf bytes.Buffer
	if err := files.Get(context.Background(), g.c, m, &buf, nil); err != nil {
		g.t.Fatal(err)
	}
	return buf.Bytes()
}

func holds(m manifest.Manifest, addr string) int {
	n := 0
	for _, ch := range m.Chunks {
		for _, s := range ch.Shards {
			if s.Peer == addr {
				n++
			}
		}
	}
	return n
}

func TestRepairAfterGrace(t *testing.T) {
	g := newGroup(t, 7)
	data := make([]byte, 300<<10)
	rand.Read(data)
	stub, m := g.put("a.bin", data)
	gone := g.list[0].Addr
	lost := holds(m, gone)
	if lost == 0 {
		t.Fatal("node 0 holds nothing; nothing to test")
	}
	g.srvs[0].Close()

	// Down only just now: nothing moves yet.
	rep, err := g.repairer(time.Hour).Pass(context.Background())
	if err != nil || rep.Rebuilt != 0 || rep.Waiting != lost {
		t.Fatalf("within the grace period: %+v %v", rep, err)
	}

	g.downSince(0, time.Now().Add(-2*time.Hour))
	rep, err = g.repairer(time.Hour).Pass(context.Background())
	if err != nil || rep.Rebuilt != lost || rep.Rewrote != 1 {
		t.Fatalf("after the grace period: %+v %v", rep, err)
	}
	fixed, err := files.ReadStub(stub)
	if err != nil {
		t.Fatal(err)
	}
	if holds(fixed, gone) != 0 {
		t.Fatal("the stub still names the gone node")
	}
	for ci, ch := range fixed.Chunks {
		seen := map[string]bool{}
		for si, s := range ch.Shards {
			if seen[s.Peer] {
				t.Fatalf("chunk %d: two shards on %s", ci, s.Peer)
			}
			seen[s.Peer] = true
			if s.Hash != m.Chunks[ci].Shards[si].Hash {
				t.Fatalf("chunk %d shard %d: hash changed", ci, si)
			}
		}
	}
	// Whole again: it survives two more nodes going.
	g.srvs[1].Close()
	g.srvs[2].Close()
	if !bytes.Equal(g.get(fixed), data) {
		t.Fatal("rebuilt data differs")
	}
}

func TestRepairMissingShardOnLiveNode(t *testing.T) {
	g := newGroup(t, 7)
	data := make([]byte, 100<<10)
	rand.Read(data)
	stub, m := g.put("b.bin", data)
	ref := m.Chunks[0].Shards[0]
	if err := g.c.Delete(context.Background(), ref.Peer, ref.Hash); err != nil {
		t.Fatal(err)
	}
	rep, err := g.repairer(time.Hour).Pass(context.Background())
	if err != nil || rep.Rebuilt != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	fixed, _ := files.ReadStub(stub)
	if has, err := g.c.Has(context.Background(), fixed.Chunks[0].Shards[0].Peer, ref.Hash); err != nil || !has {
		t.Fatalf("the rebuilt shard isn't there: %v %v", has, err)
	}
}

func TestNoRepairWhenMostAreDown(t *testing.T) {
	g := newGroup(t, 7)
	g.put("c.bin", make([]byte, 10<<10))
	for i := range 4 {
		g.srvs[i].Close()
	}
	b, _ := json.Marshal(state{DownSince: map[string]int64{g.list[0].Addr: 1, g.list[1].Addr: 1, g.list[2].Addr: 1, g.list[3].Addr: 1}})
	os.WriteFile(g.state, b, 0o600)
	rep, _ := g.repairer(time.Hour).Pass(context.Background())
	if rep.Rebuilt != 0 || len(rep.Problems) == 0 {
		t.Fatalf("repaired with most of the group unreachable: %+v", rep)
	}
}

func TestLeftTheListIsGone(t *testing.T) {
	g := newGroup(t, 7)
	_, m := g.put("d.bin", make([]byte, 50<<10))
	left := g.list[3].Addr
	n := holds(m, left)
	g.list = append(g.list[:3], g.list[4:]...) // still running, but no longer a member
	rep, err := g.repairer(DefaultGrace).Pass(context.Background())
	if err != nil || rep.Rebuilt != n {
		t.Fatalf("%+v %v (want %d rebuilt)", rep, err, n)
	}
}

func TestSharedItemsAreLeftAlone(t *testing.T) {
	g := newGroup(t, 7)
	stub, m := g.put("e.bin", make([]byte, 10<<10))
	m.SharedBy = &manifest.Sender{Name: "someone"}
	files.WriteJSON(stub, m)
	g.srvs[0].Close()
	g.downSince(0, time.Unix(1, 0))
	if rep, _ := g.repairer(time.Hour).Pass(context.Background()); rep.Items != 0 || rep.Rebuilt != 0 {
		t.Fatalf("touched a share: %+v", rep)
	}
}

// An item stored while most machines were down is spread over them once
// they are back.
func TestSpreadWhenMachinesReturn(t *testing.T) {
	g := newGroup(t, 7)
	all := g.list
	g.list = all[:2] // only two machines up: 2+2, two shards on each
	stub, m := g.put("f.bin", make([]byte, 200<<10))
	if m.DataShards != 2 || m.ParityShards != 2 {
		t.Fatalf("stored as %d+%d with two machines, want 2+2", m.DataShards, m.ParityShards)
	}
	g.list = all
	rep, err := g.repairer(time.Hour).Pass(context.Background())
	if err != nil || rep.Spread != 2*len(m.Chunks) || rep.Rewrote != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	spread, _ := files.ReadStub(stub)
	for ci, ch := range spread.Chunks {
		seen := map[string]bool{}
		for _, s := range ch.Shards {
			if seen[s.Peer] {
				t.Fatalf("chunk %d: still two shards on %s", ci, s.Peer)
			}
			seen[s.Peer] = true
		}
	}
	// Now any two machines can go.
	g.srvs[0].Close()
	g.srvs[1].Close()
	if got := g.get(spread); len(got) != 200<<10 {
		t.Fatalf("read %d bytes", len(got))
	}
	if rep, _ := g.repairer(time.Hour).Pass(context.Background()); rep.Spread != 0 {
		t.Fatalf("spread again: %+v", rep)
	}
}

func TestOneMachineIsRefused(t *testing.T) {
	g := newGroup(t, 3)
	for i := range g.list {
		g.list[i].Host = "box"
	}
	_, _, _, err := files.PutReader(context.Background(), g.c, bytes.NewReader([]byte("x")), "g", g.list, files.PutOptions{})
	if !errors.Is(err, files.ErrTooFewMachines) {
		t.Fatalf("stored on one machine: %v", err)
	}
}
