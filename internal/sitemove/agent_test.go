package sitemove

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
)

// net carries direct messages between the test's machines.
type net struct {
	mu    sync.Mutex
	inbox map[string][]msg.Stored
	n     int64
}

type bus struct {
	net  *net
	self string
}

func (b bus) Send(_ context.Context, to, typ, body string) (msg.Message, error) {
	b.net.mu.Lock()
	defer b.net.mu.Unlock()
	b.net.n++
	m := msg.Stored{N: b.net.n, Message: msg.Message{From: b.self, To: to, Type: typ, Body: body}}
	b.net.inbox[to] = append(b.net.inbox[to], m)
	return m.Message, nil
}

func (b bus) Messages(_ context.Context, after int64, _ string, _ int) ([]msg.Stored, error) {
	b.net.mu.Lock()
	defer b.net.mu.Unlock()
	var out []msg.Stored
	for _, m := range b.net.inbox[b.self] {
		if m.N > after {
			out = append(out, m)
		}
	}
	return out, nil
}

// holder keeps sites in folders by person, with a key each.
type holder struct {
	dir     string
	people  map[string]bool // who it handles
	noMail  bool
	keys    map[string]*share.Identity
	client  client.Client
	ann     site.Announcer
	keysDir string
}

func (h *holder) Handles(person string) bool { return h.people[strings.ToLower(person)] }

func (h *holder) Publisher(person string) (*site.Publisher, error) {
	return &site.Publisher{Dir: filepath.Join(h.dir, "p-"+strings.ToLower(person)), Client: h.client, Announce: h.ann}, nil
}

func (h *holder) Identity(person string) (*share.Identity, error) {
	return share.LoadOrCreate(filepath.Join(h.keysDir, "k-"+strings.ToLower(person)))
}

func (h *holder) Name(person string) string {
	if person == "" {
		return ""
	}
	return strings.ToUpper(person[:1]) + strings.ToLower(person[1:])
}

func (h *holder) CanTake(person string) error {
	if h.noMail {
		return errors.New("this box collects no mail")
	}
	return nil
}

type announced struct {
	mu   sync.Mutex
	from []string
}

type announcer struct {
	a    *announced
	self string
}

func (n announcer) Publish(context.Context, string, string, string) error {
	n.a.mu.Lock()
	n.a.from = append(n.a.from, n.self)
	n.a.mu.Unlock()
	return nil
}

func TestAgentsMoveASite(t *testing.T) {
	ctx := context.Background()
	nodes := storageNodes(t, 6)
	c := client.New()
	nt := &net{inbox: map[string][]msg.Stored{}}
	ann := &announced{}
	var mu sync.Mutex
	members := []peers.Peer{
		{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org", Code: "ys1x"},
		{Name: "localmail", Addr: "[200::2]:7400"},
	}
	list := func() []peers.Peer {
		mu.Lock()
		defer mu.Unlock()
		return append([]peers.Peer{}, members...)
	}
	online := func(context.Context) ([]peers.Peer, error) { return nodes, nil }

	desk := &holder{dir: t.TempDir(), keysDir: t.TempDir(), people: map[string]bool{"": true}, client: c, ann: announcer{ann, "200::1"}}
	box := &holder{dir: t.TempDir(), keysDir: t.TempDir(), people: map[string]bool{"tim": true}, noMail: true, client: c, ann: announcer{ann, "200::2"}}

	// The desktop publishes the site, with its contact form on.
	pub, _ := desk.Publisher("")
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "index.html"), []byte("<h1>shop</h1>"), 0o644)
	if _, _, err := pub.Publish(ctx, "shop.example.org", src, nodes); err != nil {
		t.Fatal(err)
	}
	deskID, _ := desk.Identity("")
	pub.SetContact(ctx, "shop.example.org", deskID.Code())

	desktop := NewAgent(Config{Self: "200::1", Peers: list, Client: c, Online: online, Holder: desk,
		StatePath: filepath.Join(t.TempDir(), "moves.json"), Logf: t.Logf,
		Bus: func() (Bus, error) { return bus{nt, "200::1"}, nil },
		Give: func(m Move, r Ready) error {
			mu.Lock()
			defer mu.Unlock()
			out, err := Give(members, m, r)
			if err == nil {
				members = out
			}
			return err
		}})
	localmail := NewAgent(Config{Self: "200::2", Peers: list, Client: c, Online: online, Holder: box,
		StatePath: filepath.Join(t.TempDir(), "moves.json"), Logf: t.Logf,
		Bus: func() (Bus, error) { return bus{nt, "200::2"}, nil }})
	steps := func() {
		for range 4 {
			desktop.Step(ctx)
			localmail.Step(ctx)
		}
	}
	move := Move{Site: "shop.example.org", From: "200::1", To: "200::2", ToPerson: "tim"} // as the admin typed it

	// A box that collects no mail can't take a site with a contact form.
	if err := desktop.Start(ctx, move); err != nil {
		t.Fatal(err)
	}
	steps()
	if m := desktop.Moves(); m[0].Status != StatusRefused || !strings.Contains(m[0].Reason, "no mail") {
		t.Fatalf("admin: %+v", m[0])
	}
	if s := pub.Sites(); len(s) != 1 {
		t.Fatal("a refused move let the site go")
	}

	box.noMail = false
	if err := desktop.Start(ctx, move); err != nil {
		t.Fatal(err)
	}
	if err := desktop.Start(ctx, move); err == nil {
		t.Fatal("started twice")
	}
	steps()

	var admin *Pending
	for _, m := range desktop.Moves() {
		if m.Role == RoleAdmin && m.Status != StatusRefused {
			admin = &m
		}
	}
	if admin == nil || admin.Status != StatusDone {
		t.Fatalf("admin's move: %+v", desktop.Moves())
	}
	l := list()
	if _, pn, ok := peers.NameHolder(l, "shop.example.org"); !ok || pn.Person != "Tim" || !pn.Site || l[1].PeopleNames[0].Code == "" {
		t.Fatalf("the name wasn't given to Tim: %+v", l)
	}
	// Tim's publisher has it, sealed for his key, and announced it.
	tpub, _ := box.Publisher("Tim")
	tid, _ := box.Identity("Tim")
	if vs, err := tpub.Versions("shop.example.org"); err != nil || len(vs) != 1 {
		t.Fatalf("Tim's versions: %v %v", vs, err)
	}
	if tpub.Contact("shop.example.org") != tid.Code() {
		t.Fatal("the contact form's messages aren't sealed for Tim")
	}
	ann.mu.Lock()
	last := ann.from[len(ann.from)-1]
	ann.mu.Unlock()
	if last != "200::2" {
		t.Fatalf("localmail didn't announce it: %v", ann.from)
	}
	// The desktop let it go, with a note of where it went.
	if s := pub.Sites(); len(s) != 0 {
		t.Fatalf("the desktop still publishes %v", s)
	}
	if to, _, ok := Moved(pub.Dir, "shop.example.org"); !ok || to != "Tim on localmail" {
		t.Fatalf("moved to %q", to)
	}

	// Someone who isn't an admin can't move it back.
	bus{nt, "200::2"}.Send(ctx, "200::2", TypeMove, Body(Move{Site: "shop.example.org", From: "200::2", FromPerson: "Tim", To: "200::1", Time: time.Now().UnixMilli()}))
	steps()
	for _, m := range localmail.Moves() {
		if m.Role == RoleOld {
			t.Fatalf("a move from a non-admin was taken: %+v", m)
		}
	}
}
