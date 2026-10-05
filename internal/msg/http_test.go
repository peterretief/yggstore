package msg

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
)

// Two engines talking real HTTP. Each test server stands in for one node
// and tells its engine who is calling, as the node server does.
func TestOverHTTP(t *testing.T) {
	var a, b *Engine
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Serve(w, r, "200::2") {
			http.NotFound(w, r)
		}
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !b.Serve(w, r, "200::1") {
			http.NotFound(w, r)
		}
	}))
	defer srvB.Close()
	old := httptest.NewServer(http.NotFoundHandler()) // a node on older yggstore
	defer old.Close()
	list := []peers.Peer{
		{Name: "a", Addr: "[200::1]:7400"}, {Name: "b", Addr: "[200::2]:7400"}, {Name: "old", Addr: "[200::3]:7400"},
	}
	addrs := map[string]string{"[200::1]:7400": srvA.Listener.Addr().String(), "[200::2]:7400": srvB.Listener.Addr().String(),
		"[200::3]:7400": old.Listener.Addr().String()}
	// Route the group's addresses to the test servers.
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addrs[addr])
	}}
	c := Client{HTTP: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
	var err error
	if a, err = Open(t.TempDir(), "200::1", func() []peers.Peer { return list }, c, t.Logf); err != nil {
		t.Fatal(err)
	}
	if b, err = Open(t.TempDir(), "200::2", func() []peers.Peer { return list }, c, t.Logf); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)

	b.Subscribe("news")
	deadline := time.Now().Add(5 * time.Second)
	for len(a.Status().Subscribers["news"]) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscription never reached a")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.Publish("news", "text/plain", "over http")
	a.Send("b", "", "direct over http")
	for len(b.Messages(0, "", 0)) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("b got %v", bodies(b.Messages(0, "", 0)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := b.Messages(0, "news", 0); got[0].From != "200::1" || got[0].FromName != "a" {
		t.Fatalf("got %+v", got[0])
	}
	if err := c.Announce(ctx, "[200::3]:7400", nil); err != ErrNoMessaging {
		t.Fatalf("old node: %v", err)
	}
}

func TestLocalAPI(t *testing.T) {
	g := newGroup(t, 2)
	token := strings.Repeat("ab", 24)
	srv := httptest.NewServer(g.nodes[1].LocalHandler(token))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	l := Local{Addr: addr, Token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := (Local{Addr: addr, Token: "wrong" + token[5:]}).Subscribe(ctx, "x"); err == nil {
		t.Fatal("wrong token accepted")
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/status", nil)
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-loopback Host allowed: %v %v", resp.StatusCode, err)
	}

	got := make(chan Stored, 4)
	go l.Stream(ctx, -1, "", func(s Stored) error { got <- s; return nil })
	time.Sleep(100 * time.Millisecond)
	if _, err := l.Send(ctx, "node1", "", "from the local api"); err != nil {
		t.Fatal(err)
	}
	g.eventually("node1 gets it", func() bool { return len(g.nodes[0].Messages(0, "", 0)) == 1 })
	g.nodes[0].Send("node2", "", "reply")
	select {
	case s := <-got:
		if s.Body != "reply" || s.FromName != "node1" {
			t.Fatalf("streamed %+v", s)
		}
	case <-ctx.Done():
		t.Fatal("stream got nothing")
	}
	if list, err := l.Latest(ctx, "direct", 5); err != nil || len(list) != 1 {
		t.Fatalf("latest: %v %v", list, err)
	}
}

var dialer net.Dialer
