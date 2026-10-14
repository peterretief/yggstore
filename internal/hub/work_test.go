package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
	"github.com/peterretief/yggstore/internal/workcopy"
)

// desktop is a person's computer in a test: an outbox, a home with a
// sharing key, and recovery copies of them on test nodes.
type desktop struct {
	out, home string
	keeper    *recovery.Keeper
	nodes     []peers.Peer
}

// copyOf makes a recovery copy of a small outbox and home on test nodes.
func copyOf(t *testing.T) json.RawMessage {
	t.Helper()
	d := newDesktop(t)
	m, err := d.keeper.SaveNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := manifest.Marshal(m)
	return b
}

func newDesktop(t *testing.T) *desktop {
	t.Helper()
	var nodes []peers.Peer
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		nodes = append(nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	out, home := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(out, "files", "Clients"), 0o755)
	os.WriteFile(filepath.Join(out, "files", "Clients", "quote.docx.ystub"), []byte(`{"a":1}`), 0o644)
	if _, err := share.LoadOrCreate(filepath.Join(home, "sharing.key")); err != nil {
		t.Fatal(err)
	}
	k := recovery.New(recovery.Config{Dir: filepath.Join(home, "recovery"), Client: client.New(),
		Sources: []recovery.Source{{Name: "outbox", Path: out, Keep: recovery.OutboxKeep}, {Name: "home", Path: home, Keep: recovery.HomeKeep}},
		Online:  func(context.Context) ([]peers.Peer, error) { return nodes, nil }})
	return &desktop{out: out, home: home, keeper: k, nodes: nodes}
}

const (
	laptopID = "LAPTOPA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"
	clientPC = "CLIENTP-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB"
)

func TestWorkHere(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	u.cfg.Client = client.New()
	ctx := context.Background()
	stub := copyOf(t)

	resp, err := u.WorkHere(ctx, WorkRequest{ID: "rec1", Name: "anna", Stub: stub, Device: laptopID})
	if err != nil || resp.BoxID != "HUBHUBH-AAAAAAA" {
		t.Fatalf("%+v, %v", resp, err)
	}
	st, _ := Load(filepath.Join(dir, "hub.json"))
	p, ok := st.PersonNamed("anna")
	if !ok || p.RecoveryID != "rec1" {
		t.Fatalf("people %+v", st.People)
	}
	if b, err := os.ReadFile(filepath.Join(p.Path, "files", "Clients", "quote.docx.ystub")); err != nil || string(b) != `{"a":1}` {
		t.Fatalf("stub not unpacked: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(p.Path, ".yggstore", "home", "sharing.key")); err != nil {
		t.Fatal("sharing key not unpacked into .yggstore/home")
	}
	u.step(ctx)
	if got := strings.Join(folderDevices(fake, p.Folder), " "); got != "HUBHUBH-AAAAAAA "+laptopID {
		t.Fatalf("shared with %s", got)
	}

	// Another device: refused while the laptop has it.
	if _, err := u.WorkHere(ctx, WorkRequest{ID: "rec1", Name: "anna", Stub: stub, Device: clientPC}); !errors.Is(err, ErrInUse) {
		t.Fatalf("second device: %v", err)
	}
	// Move it here: the laptop goes, the client's PC gets it.
	if _, err := u.WorkHere(ctx, WorkRequest{ID: "rec1", Name: "anna", Stub: stub, Device: clientPC, Move: true}); err != nil {
		t.Fatal(err)
	}
	u.step(ctx)
	if got := strings.Join(folderDevices(fake, p.Folder), " "); got != "HUBHUBH-AAAAAAA "+clientPC {
		t.Fatalf("after moving, shared with %s", got)
	}
	if _, err := u.WorkHere(ctx, WorkRequest{ID: "rec1", Stub: stub, Device: "not an id"}); err == nil {
		t.Fatal("took a bad device ID")
	}
}

// bus is the group's messaging, shared by the places in a test.
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

// The desktop holds the working copy; Work here on the box asks for it,
// the desktop saves a final copy (with what changed since the last one),
// and the box unpacks that and holds it from then on.
func TestWorkHereTakesOverFromTheDesktop(t *testing.T) {
	u, _, _ := setupUsers(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDesktop(t)
	b := &bus{}
	u.cfg.Client, u.cfg.Bus = client.New(), func() (workcopy.Bus, error) { return b, nil }
	u.cfg.Online = func(context.Context) ([]peers.Peer, error) { return d.nodes, nil }
	handOverWait = 20 * time.Second

	id, _ := share.Load(filepath.Join(d.home, "sharing.key"))
	home := workcopy.New(workcopy.Config{Bus: func() (workcopy.Bus, error) { return b, nil }, ID: func() *share.Identity { return id },
		Place: func() string { return "desktop" }, SaveNow: d.keeper.SaveNow, Interval: 50 * time.Millisecond})
	home.Step(ctx) // claims it
	first, _ := d.keeper.SaveNow(ctx)
	stub, _ := manifest.Marshal(first)
	// Something new on the desktop after the last copy.
	os.WriteFile(filepath.Join(d.out, "files", "Clients", "late.docx.ystub"), []byte(`{"late":1}`), 0o644)
	go home.Run(ctx)

	if _, err := u.WorkHere(ctx, WorkRequest{ID: "rec2", Name: "anna", Stub: stub, Device: laptopID}); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(u.cfg.StatePath)
	p, _ := st.PersonNamed("anna")
	if _, err := os.Stat(filepath.Join(p.Path, "files", "Clients", "late.docx.ystub")); err != nil {
		t.Fatal("the box didn't get the desktop's final copy")
	}
	if !u.Holding("anna") {
		t.Fatalf("the box doesn't hold it: %+v", u.places["anna"].tracker.State())
	}
	home.Step(ctx)
	if home.Holding() {
		t.Fatalf("the desktop still holds it: %+v", home.State())
	}
}
