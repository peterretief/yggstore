package dashboard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
)

// TestNetworkPage: the network page lists each node with what it reports
// about its machine, and which of the machine's ports answer.
func TestNetworkPage(t *testing.T) {
	dir := t.TempDir()
	node := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{Name: "pi", Transport: transport.Loopback{},
		Allowed: map[string]bool{"127.0.0.1": true}, Version: "v-test"}))
	defer node.Close()
	id, _ := share.LoadOrCreate(filepath.Join(dir, "me.key"))
	mine := filepath.Join(dir, "peers.json")
	peers.Write(mine, []peers.Peer{{Name: "desktop", Addr: "127.0.0.1:1", Admin: true, Domain: "example.org"},
		{Name: "pi", Addr: strings.TrimPrefix(node.URL, "http://"), Host: "home", Names: []string{"pi.example.org"}, Code: id.Code()}})
	d := New(Config{PeersPath: mine, StubDir: dir, SelfID: "127.0.0.1", Client: client.New()})
	d.poll(context.Background())

	get := func() netState {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/network", nil)
		req.Host = "localhost"
		d.Handler().ServeHTTP(rec, req)
		var st netState
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
		}
		return st
	}
	st := get()
	if len(st.Nodes) != 2 || st.Domain != "example.org" {
		t.Fatalf("got %+v", st)
	}
	pi := st.Nodes[1]
	if !pi.Up || pi.Version != "v-test" || pi.Hostname == "" || pi.Machine != "home" || len(pi.Names) != 1 {
		t.Fatalf("pi: %+v", pi)
	}
	if len(st.Names) != 2 || st.Names[0].Mail != "pi@example.org" {
		t.Fatalf("names: %+v", st.Names)
	}
	// The probes run in the background; once done, the machines are known.
	deadline := time.Now().Add(20 * time.Second)
	for !st.Machines[0].Known && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		st = get()
	}
	if len(st.Machines) != 1 || !st.Machines[0].Known {
		t.Fatalf("machines: %+v", st.Machines)
	}
}
