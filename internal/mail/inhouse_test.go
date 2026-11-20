package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
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

// Mail to an app's address on a machine goes to the app's inbox, marked as
// from a member when it came through the group's mail.
func TestMailForAnApp(t *testing.T) {
	var list []peers.Peer
	for i := range 6 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	apps := t.TempDir()
	app, err := share.LoadOrCreate(filepath.Join(apps, "leave", "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}
	owner := identity(t)
	list[0].Admin, list[0].Domain = true, "group.example"
	list = append(list,
		peers.Peer{Name: "web", Addr: "127.0.0.2:9"},
		peers.Peer{Name: "home", Addr: "127.0.0.3:9", Names: []string{"peter.group.example"}, Code: owner.Code(),
			PeopleNames: []peers.PersonName{{Name: "leave.group.example", Person: "app:leave", Code: app.Code()}}})
	members := func() []peers.Peer { return list }
	pb := &postbox{boxes: map[string][]msg.Stored{}, wake: make(chan struct{})}
	c := client.New()
	web := &Node{Dir: t.TempDir(), Self: "127.0.0.2", Msgs: postNode{pb, "127.0.0.2"}, Peers: members, Client: c}
	home := &Node{Dir: t.TempDir(), Box: t.TempDir(), Apps: apps, Self: "127.0.0.3", Msgs: postNode{pb, "127.0.0.3"}, Peers: members, Client: c}
	for _, n := range []*Node{web, home} {
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
	}
	r := &Relay{Peers: members, InHouse: web.DeliverInHouse,
		Deliver: func(ctx context.Context, from string, to []string, msg []byte) error {
			return errors.New("nothing goes out")
		}}
	raw, rcpts, _ := Draft{From: "Peter@group.example", To: "leave@group.example", Subject: "Leave", Text: "2 to 6 December"}.Compose()
	if err := r.send(context.Background(), "Peter@group.example", rcpts, raw); err != nil {
		t.Fatal(err)
	}
	// And one from outside, as the Worker brings it.
	sealed, _ := Seal(app.Code(), []byte("From: someone@example.net\r\nTo: leave@group.example\r\nSubject: Hi\r\n\r\nHello\r\n"))
	if _, err := web.Take(context.Background(), "127.0.0.3", sealed); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	defer running.Wait()
	defer cancel()
	for _, n := range []*Node{web, home} {
		running.Add(1)
		go func() { defer running.Done(); n.Run(ctx) }()
	}
	inbox := filepath.Join(apps, "leave", "inbox")
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, _ := filepath.Glob(filepath.Join(inbox, "*.sealed"))
		if len(got) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("in the app's inbox: %v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	metas, _ := filepath.Glob(filepath.Join(inbox, "*.json"))
	var marked []string
	for _, p := range metas {
		var m Meta
		b, _ := os.ReadFile(p)
		json.Unmarshal(b, &m)
		marked = append(marked, m.InGroup)
		data, _ := os.ReadFile(strings.TrimSuffix(p, ".json") + ".sealed")
		if _, err := Open(app, data); err != nil {
			t.Fatal("the app can't open its mail")
		}
	}
	sort.Strings(marked)
	if strings.Join(marked, ",") != ",peter@group.example" {
		t.Fatalf("marked: %q", marked)
	}
	if got, _ := (&Box{Dir: home.Box, ID: owner}).List(); len(got) != 0 {
		t.Fatalf("the app's mail went to the owner's mailbox: %v", got)
	}
}

// Internal addresses work only inside the group: mail to them or from them
// never goes out, even when it can't be delivered just now.
func TestInternalMail(t *testing.T) {
	anna := identity(t)
	list := []peers.Peer{
		{Name: "admin", Addr: "127.0.0.1:9", Admin: true, Domain: "group.example"},
		{Name: "home", Addr: "127.0.0.3:9", Names: []string{"anna.group.example"}, Code: anna.Code()},
		{Name: "peter", Addr: "127.0.0.4:9", Names: []string{"peter.group.example"}, Code: anna.Code()},
	}
	members := func() []peers.Peer { return list }
	if peers.InternalDomain(list) != "group.internal" {
		t.Fatal(peers.InternalDomain(list))
	}
	if p, _, ok := peers.MailHolder(list, "Anna@group.internal"); !ok || p.Name != "home" {
		t.Fatal("anna@group.internal isn't anna's")
	}
	r := &Relay{Peers: members}
	if !r.may("127.0.0.4", "peter@group.internal") || r.may("127.0.0.3", "peter@group.internal") {
		t.Fatal("who may send as an internal address")
	}
	var out []string
	var delivered []string
	up := true
	r.Deliver = func(ctx context.Context, from string, to []string, msg []byte) error {
		out = append(out, to...)
		return nil
	}
	r.InHouse = func(ctx context.Context, from, to string, msg []byte) (bool, error) {
		if _, _, ok := peers.MailHolder(list, to); !ok {
			return false, nil
		}
		if !up {
			return true, errors.New("unreachable")
		}
		delivered = append(delivered, to)
		return true, nil
	}
	ctx := context.Background()
	if err := r.send(ctx, "peter@group.internal", []string{"anna@group.internal", "anna@group.example"}, nil); err != nil || len(out) != 0 || len(delivered) != 2 {
		t.Fatalf("inside: %v, out %v, in %v", err, out, delivered)
	}
	if err := r.send(ctx, "peter@group.internal", []string{"someone@example.net"}, nil); err == nil || len(out) != 0 {
		t.Fatalf("internal mail went out: %v %v", err, out)
	}
	if err := r.send(ctx, "peter@group.example", []string{"nobody@group.internal"}, nil); err == nil || len(out) != 0 {
		t.Fatalf("to no one inside: %v %v", err, out)
	}
	up = false
	if err := r.send(ctx, "peter@group.example", []string{"anna@group.internal"}, nil); err == nil || len(out) != 0 {
		t.Fatalf("internal mail fell back to going out: %v %v", err, out)
	}
	if err := r.send(ctx, "peter@group.example", []string{"anna@group.example"}, nil); err != nil || strings.Join(out, ",") != "anna@group.example" {
		t.Fatalf("an ordinary address should still fall back: %v %v", err, out)
	}
	r.InHouse = nil
	if err := r.send(ctx, "peter@group.internal", []string{"anna@group.internal"}, nil); err == nil {
		t.Fatal("internal mail without in-house delivery")
	}
}

// Every person on a box has a local address: mail to it is sealed for them
// and goes to their box; only the box sends as it; it never leaves the group.
func TestLocalAddresses(t *testing.T) {
	kath := identity(t)
	list := []peers.Peer{
		{Name: "admin", Addr: "127.0.0.1:9", Admin: true, Domain: "group.example"},
		{Name: "localbox-004", Addr: "127.0.0.4:9"},
		{Name: "desk", Addr: "127.0.0.5:9", Names: []string{"peter.group.example"}, Code: kath.Code()},
	}
	members := func() []peers.Peer { return list }
	if a := peers.LocalAddress(list, "localbox-004", "Kath Smith"); a != "kath-smith@local.4" {
		t.Fatal(a)
	}
	if a := peers.LocalAddress(list, "desk", "Kath"); a != "kath@local.desk" {
		t.Fatal(a)
	}
	// The longer form that came before still reads.
	if p, local, ok := peers.LocalBox(list, "kath-smith@localbox-004.group.internal"); !ok || p.Name != "localbox-004" || local != "kath-smith" {
		t.Fatal("the old form doesn't read")
	}
	if !peers.IsInternal(list, "kath@local.99") {
		t.Fatal("a local address at no box would go out")
	}
	for _, bad := range []string{"kath@nobox.group.internal", "kath.x@localbox-004.group.internal", "kath@localbox-004.group.example", "kath@local.99", "kath@local.004"} {
		if _, _, ok := peers.LocalBox(list, bad); ok {
			t.Errorf("%s read as a local address", bad)
		}
	}
	var took []string
	n := &Node{Self: "127.0.0.2", Peers: members, LocalCode: func(box, local string) (string, bool) {
		if box == "localbox-004" && local == "kath-smith" {
			return kath.Code(), true
		}
		return "", false
	}}
	if !n.IsLocal("Kath-Smith@local.4") || n.IsLocal("tom@local.4") {
		t.Fatal("IsLocal")
	}
	p, code, ok := n.localHolder("kath-smith@local.4")
	if !ok || p.Name != "localbox-004" || code != kath.Code() {
		t.Fatalf("holder: %v %v", p, ok)
	}
	r := &Relay{Peers: members, Local: n.IsLocal,
		Deliver: func(ctx context.Context, from string, to []string, msg []byte) error { took = append(took, to...); return nil },
		InHouse: func(ctx context.Context, from, to string, msg []byte) (bool, error) {
			if _, _, ok := peers.MailHolder(list, to); ok || n.IsLocal(to) {
				took = append(took, "in:"+to)
				return true, nil
			}
			return false, nil
		}}
	ctx := context.Background()
	if err := r.send(ctx, "peter@group.example", []string{"kath-smith@local.4"}, nil); err != nil || strings.Join(took, ",") != "in:kath-smith@local.4" {
		t.Fatalf("to kath: %v %v", err, took)
	}
	took = nil
	if err := r.send(ctx, "kath-smith@local.4", []string{"someone@example.net"}, nil); err == nil || len(took) != 0 {
		t.Fatalf("kath wrote outside: %v %v", err, took)
	}
	if err := r.send(ctx, "peter@group.example", []string{"tom@local.4"}, nil); err == nil {
		t.Fatal("to nobody on the box")
	}
	if !r.may("127.0.0.4", "kath-smith@local.4") || r.may("127.0.0.5", "kath-smith@local.4") {
		t.Fatal("who may send as kath's local address")
	}
}
