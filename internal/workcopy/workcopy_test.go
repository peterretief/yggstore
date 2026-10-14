package workcopy

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
)

// bus is the group's messaging, shared by every place in a test.
type bus struct {
	mu   sync.Mutex
	msgs []msg.Stored
}

func (b *bus) Subscribe(context.Context, string) error { return nil }

func (b *bus) Messages(_ context.Context, after int64, topic string, limit int) ([]msg.Stored, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []msg.Stored
	for _, s := range b.msgs {
		if s.N > after && s.Topic == topic && len(out) < limit {
			out = append(out, s)
		}
	}
	return out, nil
}

func (b *bus) Publish(_ context.Context, topic, typ, body string) (msg.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := msg.Message{Topic: topic, Type: typ, Body: body}
	b.msgs = append(b.msgs, msg.Stored{N: int64(len(b.msgs) + 1), Message: m})
	return m, nil
}

var nodes []peers.Peer

// stub stores a small item named name on test nodes, and returns its stub.
func stub(name string) manifest.Manifest {
	m, _, _, err := files.PutReader(context.Background(), client.New(), strings.NewReader("copy "+name), name, nodes, files.PutOptions{})
	if err != nil {
		panic(err)
	}
	return m
}

func TestMain(m *testing.M) {
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(os.TempDir()+"/workcopy-test-"+strconv.Itoa(os.Getpid())+"-"+strconv.Itoa(i)), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		defer srv.Close()
		nodes = append(nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	code := m.Run()
	for i := range 4 {
		os.RemoveAll(os.TempDir() + "/workcopy-test-" + strconv.Itoa(os.Getpid()) + "-" + strconv.Itoa(i))
	}
	os.Exit(code)
}

type place struct {
	t     *Tracker
	saved int
}

func newPlace(t *testing.T, b *bus, id *share.Identity, name string, clock *time.Time) *place {
	p := &place{}
	p.t = New(Config{
		Bus:   func() (Bus, error) { return b, nil },
		ID:    func() *share.Identity { return id },
		Place: func() string { return name },
		SaveNow: func(context.Context) (manifest.Manifest, error) {
			p.saved++
			return stub(name + "-final"), nil
		},
		Now: func() time.Time { *clock = clock.Add(time.Second); return *clock },
	})
	return p
}

func TestHandOver(t *testing.T) {
	id, err := share.LoadOrCreate(filepath.Join(t.TempDir(), "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}
	b := &bus{}
	clock := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	home := newPlace(t, b, id, "home", &clock)
	laptop := newPlace(t, b, id, "laptop", &clock)
	ctx := context.Background()

	// The first place to look claims it.
	home.t.Step(ctx)
	laptop.t.Step(ctx)
	if !home.t.Holding() || laptop.t.Holding() {
		t.Fatalf("home %+v, laptop %+v", home.t.State(), laptop.t.State())
	}
	if st := laptop.t.State(); st.Holder != "home" {
		t.Fatalf("laptop sees %+v", st)
	}

	// Copies are announced, so a place can take over from the latest.
	home.t.Copied(ctx, stub("copy1"))
	laptop.t.Step(ctx)
	if m, ok := laptop.t.Latest(); !ok || m.FileName != "copy1" {
		t.Fatalf("latest %v %v", m.FileName, ok)
	}

	// The laptop asks; home saves a final copy and hands over.
	if err := laptop.t.Ask(ctx); err != nil {
		t.Fatal(err)
	}
	home.t.Step(ctx)
	if home.saved != 1 || home.t.Holding() {
		t.Fatalf("home saved %d, holding %v: %+v", home.saved, home.t.Holding(), home.t.State())
	}
	home.t.Step(ctx)
	if home.saved != 1 {
		t.Fatal("handed over twice")
	}
	laptop.t.Step(ctx)
	m, ok := laptop.t.HandedHere()
	if !ok || m.FileName != "home-final" {
		t.Fatalf("handed here: %v %v", m.FileName, ok)
	}
	if laptop.t.Holding() {
		t.Fatal("laptop holds it before unpacking")
	}
	laptop.t.Claim(ctx)
	home.t.Step(ctx)
	if !laptop.t.Holding() || home.t.Holding() {
		t.Fatalf("after claim: laptop %+v, home %+v", laptop.t.State(), home.t.State())
	}
	if st := home.t.State(); st.Holder != "laptop" {
		t.Fatalf("home sees %+v", st)
	}
}

// Notes not sealed with the person's key are ignored: another member
// can't move someone's working copy.
func TestForeignNotesIgnored(t *testing.T) {
	me, _ := share.LoadOrCreate(filepath.Join(t.TempDir(), "a.key"))
	other, _ := share.LoadOrCreate(filepath.Join(t.TempDir(), "b.key"))
	b := &bus{}
	clock := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	home := newPlace(t, b, me, "home", &clock)
	ctx := context.Background()
	home.t.Step(ctx)
	// Another person writes a holding note on my topic, sealed with their key.
	body, _ := seal(other, Note{Kind: Holding, Place: "intruder", At: clock.Add(time.Hour).UnixMilli()})
	b.Publish(ctx, Topic(me.Code()), "application/json", body)
	home.t.Step(ctx)
	if st := home.t.State(); !st.Mine || st.Holder != "home" {
		t.Fatalf("moved by someone else's note: %+v", st)
	}
}
