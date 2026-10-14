package recovery

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
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

type world struct {
	k         *Keeper
	out, home string
	now       time.Time
	events    []string
	nodes     []peers.Peer
}

func put(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *world {
	t.Helper()
	x := &world{out: t.TempDir(), home: t.TempDir(), now: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		x.nodes = append(x.nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	put(t, filepath.Join(x.out, "files", "Photos", "beach.jpg.ystub"), `{"stub":1}`)
	put(t, filepath.Join(x.out, "files", "Photos", ".beach.jpg.ystub.challenges.json"), `{}`)
	put(t, filepath.Join(x.out, "files", "not-stored-yet.txt"), "plain text")
	put(t, filepath.Join(x.out, "restored", "beach.jpg"), "a restored photo")
	put(t, filepath.Join(x.out, "websites", "example.org", "index.html"), "site")
	put(t, filepath.Join(x.out, ".yggstore", "history", "abc", "1.ystub"), `{"old":1}`)
	put(t, filepath.Join(x.out, ".yggstore", "activity.log"), "busy")
	put(t, filepath.Join(x.home, "sharing.key"), "secret identity")
	put(t, filepath.Join(x.home, "mail", "m1.ystub"), `{"mail":1}`)
	put(t, filepath.Join(x.home, "shards", "aa", "bb"), "a node's shard")
	put(t, filepath.Join(x.home, "mail-out.json"), `{"password":"relay"}`)
	peersPath := filepath.Join(t.TempDir(), "peers.json")
	b, _ := json.Marshal(x.nodes)
	os.WriteFile(peersPath, b, 0o600)

	x.k = New(Config{Dir: filepath.Join(x.home, "recovery"), Client: client.New(), Keep: 2,
		Sources: []Source{{Name: "outbox", Path: x.out, Keep: OutboxKeep}, {Name: "home", Path: x.home, Keep: HomeKeep}, {Name: "peers.json", Path: peersPath}},
		Online:  func(context.Context) ([]peers.Peer, error) { return x.nodes, nil },
		Event:   func(kind, msg string) { x.events = append(x.events, kind+": "+msg) },
		Now:     func() time.Time { return x.now },
	})
	if err := x.k.load(); err != nil {
		t.Fatal(err)
	}
	return x
}

// restored lists what the newest copy holds, and returns the folder.
func (x *world) restored(t *testing.T) (string, []string) {
	t.Helper()
	m, err := x.k.Latest()
	if err != nil {
		t.Fatalf("%v (events %v)", err, x.events)
	}
	dir, err := files.Restore(context.Background(), client.New(), m, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	filepath.WalkDir(dir, func(p string, e os.DirEntry, _ error) error {
		if !e.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(got)
	return dir, got
}

func TestCopyHoldsWhatIsNeededAndNothingElse(t *testing.T) {
	x := setup(t)
	x.k.Step(context.Background())
	dir, got := x.restored(t)
	want := []string{"home/mail/m1.ystub", "home/sharing.key", "outbox/.yggstore/history/abc/1.ystub",
		"outbox/files/Photos/.beach.jpg.ystub.challenges.json", "outbox/files/Photos/beach.jpg.ystub", "peers.json"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("copy holds\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "home", "sharing.key")); string(b) != "secret identity" {
		t.Fatalf("sharing key %q", b)
	}
}

func TestRefreshedAfterChangesSettle(t *testing.T) {
	x := setup(t)
	ctx := context.Background()
	x.k.Step(ctx)
	if n := x.k.Status().Copies; n != 1 {
		t.Fatalf("%d copies at first", n)
	}
	// The activity log changing makes no new copy.
	put(t, filepath.Join(x.out, ".yggstore", "activity.log"), "busier")
	x.now = x.now.Add(10 * time.Minute)
	x.k.Step(ctx)
	if n := x.k.Status().Copies; n != 1 {
		t.Fatal("a new copy for the activity log")
	}

	// A new stub: not straight away, but once nothing has changed for a while.
	put(t, filepath.Join(x.out, "files", "film.mkv.ystub"), `{"film":1}`)
	x.k.Step(ctx)
	if n := x.k.Status().Copies; n != 1 {
		t.Fatal("copied before things settled")
	}
	x.now = x.now.Add(3 * time.Minute)
	x.k.Step(ctx)
	if n := x.k.Status().Copies; n != 2 {
		t.Fatalf("%d copies after a change settled (events %v)", n, x.events)
	}
	if _, got := x.restored(t); !strings.Contains(strings.Join(got, " "), "outbox/files/film.mkv.ystub") {
		t.Fatalf("new copy lacks the new stub: %v", got)
	}

	// Beyond Keep, the oldest goes, from the group too.
	first := x.k.st.Copies[0]
	m, _ := files.ReadStub(filepath.Join(x.k.cfg.Dir, first.Stub))
	put(t, filepath.Join(x.out, "files", "more.ystub"), `{}`)
	x.k.Step(ctx)
	x.now = x.now.Add(3 * time.Minute)
	x.k.Step(ctx)
	if n := x.k.Status().Copies; n != 2 {
		t.Fatalf("%d copies kept, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(x.k.cfg.Dir, first.Stub)); !os.IsNotExist(err) {
		t.Fatal("oldest copy's stub kept")
	}
	if err := files.Get(ctx, client.New(), m, new(strings.Builder), nil); err == nil {
		t.Fatal("oldest copy still readable in the group")
	}
}

func TestSealedWithBothHalves(t *testing.T) {
	x := setup(t)
	x.k.Step(context.Background())
	sealed, err := os.ReadFile(x.k.SealedPath())
	if err != nil {
		t.Fatal(err)
	}
	st := x.k.st
	key, err := Key(st.LinkHalf, st.GroupHalf)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Open(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	json.Unmarshal(plain, &m)
	latest, _ := x.k.Latest()
	if m.FileID != latest.FileID {
		t.Fatal("sealed stub isn't the newest copy's")
	}
	// Either half alone opens nothing.
	other, _ := Key(st.LinkHalf, randomText(32))
	if _, err := Open(other, sealed); err == nil {
		t.Fatal("opened with the link's half and a wrong group half")
	}
}

func TestOff(t *testing.T) {
	x := setup(t)
	if err := x.k.Set(false, ""); err != nil {
		t.Fatal(err)
	}
	x.k.Step(context.Background())
	if s := x.k.Status(); s.On || s.Copies != 0 {
		t.Fatalf("status %+v with recovery off", s)
	}
	if err := x.k.Set(true, "not an address"); err == nil {
		t.Fatal("took a bad address")
	}
	if err := x.k.Set(true, "anna@example.org"); err != nil || x.k.Status().Email != "anna@example.org" {
		t.Fatalf("set: %v", err)
	}
}
