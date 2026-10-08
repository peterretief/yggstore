package repair

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
)

// backdate makes every lease on every node look last renewed at t.
func (g *group) backdate(t time.Time) {
	for _, s := range g.stores {
		recs, _ := filepath.Glob(filepath.Join(s.Dir(), "leases", "*"))
		for _, r := range recs {
			os.Chtimes(r, t, t)
		}
	}
}

// renewed is when the shards of m were last wanted, at the latest and the
// earliest, over every node.
func (g *group) renewed(m manifest.Manifest) (oldest time.Time, leases int) {
	want := map[string]bool{}
	for _, ch := range m.Chunks {
		for _, s := range ch.Shards {
			want[s.Hash] = true
		}
	}
	oldest = time.Now().Add(time.Hour)
	for _, s := range g.stores {
		held, err := s.Holdings()
		if err != nil {
			g.t.Fatal(err)
		}
		for _, h := range held {
			if want[h.Hash] {
				if h.Renewed.Before(oldest) {
					oldest = h.Renewed
				}
				leases = h.Leases
			}
		}
	}
	return oldest, leases
}

func TestRenewLeases(t *testing.T) {
	g := newGroup(t, 4)
	data := make([]byte, 100<<10)
	rand.Read(data)
	_, mine := g.put("mine", data)

	// A share received: its stub lives elsewhere, and isn't ours to repair,
	// but holding it keeps its shards wanted.
	received := filepath.Join(t.TempDir(), "received")
	os.MkdirAll(received, 0o700)
	theirs, _, _, err := files.PutReader(context.Background(), g.c, bytes.NewReader(data), "theirs", g.list, files.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	theirs.SharedBy = &manifest.Sender{Name: "a friend", Code: "x"}
	files.WriteJSON(filepath.Join(received, "theirs"+files.StubExt), theirs)

	// An item whose stub is gone: nobody renews it.
	lost, _, _, err := files.PutReader(context.Background(), g.c, bytes.NewReader(data), "lost", g.list, files.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, n := g.renewed(mine); n != 1 {
		t.Fatalf("shards stored under %d leases, want 1", n)
	}
	long := time.Now().AddDate(0, -4, 0)
	g.backdate(long)

	r := g.repairer(time.Hour)
	r.Received = func() []string { s, _ := filepath.Glob(filepath.Join(received, "*")); return s }
	rep, err := r.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Renewed != 4 {
		t.Fatalf("renewed on %d nodes, want 4 (%v)", rep.Renewed, rep.Problems)
	}
	day := time.Now().Add(-time.Hour)
	if at, _ := g.renewed(mine); at.Before(day) {
		t.Errorf("own item last renewed %v", at)
	}
	if at, _ := g.renewed(theirs); at.Before(day) {
		t.Errorf("received share last renewed %v", at)
	}
	if at, _ := g.renewed(lost); at.After(long.Add(time.Minute)) {
		t.Errorf("an item nobody holds was renewed (%v)", at)
	}

	// Once a day is enough.
	if rep, _ := r.Pass(context.Background()); rep.Renewed != 0 {
		t.Errorf("renewed again within the day on %d nodes", rep.Renewed)
	}

	// A wiped disk: the shards rebuilt there are under the item's lease.
	wiped := g.stores[0].Dir()
	os.RemoveAll(wiped)
	if rep, _ := r.Pass(context.Background()); rep.Rebuilt == 0 {
		t.Fatalf("nothing rebuilt: %+v", rep)
	}
	held, _ := localstore.New(wiped).Holdings()
	if len(held) == 0 {
		t.Fatal("nothing rebuilt on the wiped node")
	}
	for _, h := range held {
		if h.Leases != 1 {
			t.Errorf("rebuilt shard %s under %d leases", h.Hash[:8], h.Leases)
		}
	}
}

// RenewOnly renews without repairing.
func TestRenewOnly(t *testing.T) {
	g := newGroup(t, 4)
	_, m := g.put("item", []byte("some data to keep"))
	g.backdate(time.Now().AddDate(0, -4, 0))
	os.RemoveAll(g.stores[1].Dir())
	r := g.repairer(time.Hour)
	r.RenewOnly = true
	rep, err := r.Pass(context.Background())
	if err != nil || rep.Renewed != 4 || rep.Rebuilt != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if at, _ := g.renewed(m); at.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("last renewed %v", at)
	}
}
