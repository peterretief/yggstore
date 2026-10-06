package mesh

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

// fakeYgg answers admin requests like Yggdrasil does. Links added by URI
// come up at once if the URI is in reachable, as the given address.
type fakeYgg struct {
	mu        sync.Mutex
	self      Self
	links     map[string]Link
	reachable map[string]string // uri → address of the machine there
	calls     []string
}

func (f *fakeYgg) serve(t *testing.T) Admin {
	path := filepath.Join(t.TempDir(), "ygg.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return Admin{Endpoint: "unix://" + path}
}

func (f *fakeYgg) handle(c net.Conn) {
	defer c.Close()
	var req struct {
		Request   string            `json:"request"`
		Arguments map[string]string `json:"arguments"`
	}
	if json.NewDecoder(c).Decode(&req) != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.Request+" "+req.Arguments["uri"])
	reply := map[string]any{"status": "success"}
	uri := req.Arguments["uri"]
	switch req.Request {
	case "getself":
		reply["response"] = f.self
	case "getpeers":
		list := []Link{}
		for _, l := range f.links {
			list = append(list, l)
		}
		reply["response"] = map[string]any{"peers": list}
	case "addpeer":
		if _, ok := f.links[uri]; ok {
			reply = map[string]any{"status": "error", "error": "peer is already configured"}
			break
		}
		l := Link{URI: uri, LastError: "dial tcp: connection refused"}
		if addr, ok := f.reachable[uri]; ok {
			l = Link{URI: uri, Up: true, Address: addr}
		}
		f.links[uri] = l
	case "removepeer":
		delete(f.links, uri)
	}
	json.NewEncoder(c).Encode(reply)
}

func (f *fakeYgg) has(uri string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.links[uri]
	return ok
}

func TestMeshLinksMembers(t *testing.T) {
	f := &fakeYgg{
		self: Self{Key: "aa", Address: "200::1"},
		links: map[string]Link{
			// configured by hand, and already linked to pi by multicast
			"tls://public.example:443": {URI: "tls://public.example:443", Up: true, Address: "300::9"},
			"":                         {Up: true, Inbound: true, Address: "200::3"},
		},
		reachable: map[string]string{"wss://ygg.mail.example:443": "200::2"},
	}
	admin := f.serve(t)
	list := []peers.Peer{
		{Name: "desktop", Addr: "[200::1]:7400", YggListen: []string{"tls://198.51.100.1:14415"}},
		{Name: "mail", Addr: "[200::2]:7400", YggListen: []string{"wss://ygg.mail.example:443"}},
		{Name: "pi", Addr: "[200::3]:7400", YggListen: []string{"tls://192.0.2.3:14415"}},
		{Name: "far", Addr: "[200::4]:7400", YggListen: []string{"tls://192.0.2.4:14415"}},
	}
	var mu sync.Mutex
	current := list
	m := New(admin, "200::1", func() []peers.Peer { mu.Lock(); defer mu.Unlock(); return current }, filepath.Join(t.TempDir(), "mesh.json"), t.Logf)
	ctx := context.Background()
	linkWait = 0

	m.Sync(ctx)
	st := m.Status()
	if !st.On || st.Key != "aa" {
		t.Fatalf("status %+v", st)
	}
	if f.has("tls://198.51.100.1:14415") {
		t.Fatal("linked to itself")
	}
	if f.has("tls://192.0.2.3:14415") {
		t.Fatal("added a link to pi, which is linked already")
	}
	if !f.has("wss://ygg.mail.example:443") || !f.has("tls://192.0.2.4:14415") {
		t.Fatal("missing links were not added")
	}

	st = m.Status() // the new link to mail is reported at once
	if len(st.Direct) != 2 || st.Direct[0] != "mail" || st.Direct[1] != "pi" {
		t.Fatalf("direct %v, want [mail pi]", st.Direct)
	}
	if st.Trying["far"] == "" {
		t.Fatalf("trying %v, want far with its error", st.Trying)
	}

	// far leaves the list: its link goes, links set by hand stay.
	mu.Lock()
	current = list[:3]
	mu.Unlock()
	m.Sync(ctx)
	if f.has("tls://192.0.2.4:14415") {
		t.Fatal("link to a removed member kept")
	}
	if !f.has("tls://public.example:443") {
		t.Fatal("removed a link this node didn't add")
	}

	// Yggdrasil restarts and forgets the links: they come back.
	f.mu.Lock()
	delete(f.links, "wss://ygg.mail.example:443")
	f.mu.Unlock()
	m.Sync(ctx)
	if !f.has("wss://ygg.mail.example:443") {
		t.Fatal("link not restored after a restart")
	}
}

func TestMeshWithoutAccess(t *testing.T) {
	m := New(Admin{Endpoint: "unix://" + filepath.Join(t.TempDir(), "none.sock")}, "200::1",
		func() []peers.Peer { return nil }, filepath.Join(t.TempDir(), "mesh.json"), t.Logf)
	m.Sync(context.Background())
	if st := m.Status(); st.On || st.Error == "" {
		t.Fatalf("status %+v, want off with a reason", st)
	}
}

func TestValidListen(t *testing.T) {
	for uri, ok := range map[string]bool{
		"tls://203.0.113.5:14415":   true,
		"wss://ygg.example.org:443": true,
		"quic://[2001:db8::1]:9001": true,
		"wss://ygg.example.org":     false,
		"tls://127.0.0.1:14415":     false,
		"tls://localhost:14415":     false,
		"http://example.org:80":     false,
	} {
		if err := peers.ValidListen(uri); (err == nil) != ok {
			t.Errorf("%s: %v", uri, err)
		}
	}
}
