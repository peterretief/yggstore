package mail

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

type downMsgs struct{}

func (downMsgs) Send(to, typ, body string) (msg.Message, error) {
	return msg.Message{}, errors.New("unreachable")
}
func (downMsgs) Wait(ctx context.Context, after int64, topic string) []msg.Stored {
	<-ctx.Done()
	return nil
}

// Mail from one member to another stays in the group; mail to anyone else
// goes out through the relay, and so does in-house mail that can't be
// delivered just now.
func TestInHouseMail(t *testing.T) {
	anna := identity(t)
	var list []peers.Peer
	for i := range 6 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	list[0].Admin, list[0].Domain = true, "group.example"
	list = append(list,
		peers.Peer{Name: "web", Addr: "127.0.0.2:9"},
		peers.Peer{Name: "home", Addr: "127.0.0.3:9", Names: []string{"anna.group.example"}, Code: anna.Code()},
		peers.Peer{Name: "peter", Addr: "127.0.0.4:9", Names: []string{"peter.group.example"}, Code: anna.Code()})
	members := func() []peers.Peer { return list }
	pb := &postbox{boxes: map[string][]msg.Stored{}, wake: make(chan struct{})}
	c := client.New()
	web := &Node{Dir: t.TempDir(), Self: "127.0.0.2", Msgs: postNode{pb, "127.0.0.2"}, Peers: members, Client: c}
	home := &Node{Dir: t.TempDir(), Box: t.TempDir(), Self: "127.0.0.3", Msgs: postNode{pb, "127.0.0.3"}, Peers: members, Client: c}
	for _, n := range []*Node{web, home} {
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var out []string
	r := &Relay{Peers: members, InHouse: web.DeliverInHouse,
		Deliver: func(ctx context.Context, from string, to []string, msg []byte) error {
			mu.Lock()
			defer mu.Unlock()
			out = append(out, to...)
			return nil
		}}
	raw, rcpts, err := Draft{From: "peter@group.example", To: "Anna@group.example, someone@example.net",
		Subject: "Lunch", Text: "At one?"}.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.send(context.Background(), "peter@group.example", rcpts, raw); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out, ",") != "someone@example.net" {
		t.Fatalf("sent out: %v", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	defer running.Wait()
	defer cancel()
	for _, n := range []*Node{web, home} {
		running.Add(1)
		go func() { defer running.Done(); n.Run(ctx) }()
	}
	box := &Box{Dir: home.Box, ID: anna}
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, _ := box.List()
		if len(got) == 1 {
			m, err := box.Read(got[0].ID)
			if err != nil || got[0].Subject != "Lunch" || !strings.Contains(m.Text, "At one?") {
				t.Fatalf("read: %+v %+v %v", got[0], m, err)
			}
			full, _ := box.Raw(got[0].ID)
			if !strings.HasPrefix(string(full), "Delivered-To: anna@group.example\r\nReturn-Path: <peter@group.example>\r\nReceived: by 127.0.0.2 (yggstore, in the group)") {
				t.Fatalf("headers: %.200q", full)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not in anna's mailbox: %v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A web node that can't reach anna's node sends it out instead.
	down := &Node{Dir: t.TempDir(), Self: "127.0.0.2", Msgs: downMsgs{}, Peers: members, Client: c}
	if err := down.Start(); err != nil {
		t.Fatal(err)
	}
	r.InHouse, out = down.DeliverInHouse, nil
	if err := r.send(context.Background(), "peter@group.example", []string{"anna@group.example", "nobody@group.example"}, raw); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out, ",") != "anna@group.example,nobody@group.example" {
		t.Fatalf("sent out: %v", out)
	}
	if len(down.st.Taken) != 0 {
		t.Fatalf("kept a copy it couldn't hand on: %v", down.st.Taken)
	}
}
