package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

// copyOf makes a recovery copy of a small outbox and home on test nodes.
func copyOf(t *testing.T) json.RawMessage {
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
	os.WriteFile(filepath.Join(home, "sharing.key"), []byte("key"), 0o600)
	k := recovery.New(recovery.Config{Dir: t.TempDir(), Client: client.New(),
		Sources: []recovery.Source{{Name: "outbox", Path: out, Keep: recovery.OutboxKeep}, {Name: "home", Path: home, Keep: recovery.HomeKeep}},
		Online:  func(context.Context) ([]peers.Peer, error) { return nodes, nil }})
	m, err := k.SaveNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := manifest.Marshal(m)
	return b
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
	if b, _ := os.ReadFile(filepath.Join(p.Path, ".yggstore", "home", "sharing.key")); string(b) != "key" {
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
