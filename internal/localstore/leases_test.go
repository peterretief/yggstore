package localstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/lease"
)

func holding(t *testing.T, s Store, hash string) Holding {
	t.Helper()
	held, err := s.Holdings()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		if h.Hash == hash {
			return h
		}
	}
	t.Fatalf("%s not held", hash[:8])
	return Holding{}
}

func TestLeases(t *testing.T) {
	s := New(t.TempDir())
	if held, err := s.Holdings(); err != nil || len(held) != 0 {
		t.Fatalf("empty store: %v %v", held, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "leases")); !os.IsNotExist(err) {
		t.Fatal("reporting made the lease folder")
	}
	if _, err := s.PutLeased([]byte("x"), "alice", "NOT-A-LEASE"); err == nil {
		t.Fatal("took a bad lease ID")
	}

	a, b := lease.Token([]byte("key a")), lease.Token([]byte("key b"))
	hash, err := s.PutLeased([]byte("shared shard"), "alice", lease.ID(a))
	if err != nil {
		t.Fatal(err)
	}
	// The same shard stored for another item adds its lease.
	if _, err := s.PutLeased([]byte("shared shard"), "alice", lease.ID(b)); err != nil {
		t.Fatal(err)
	}
	if h := holding(t, s, hash); h.Leases != 2 || h.Writer != "alice" {
		t.Fatalf("%+v", h)
	}

	// Make everything old, then renew one of the two leases: the shard is
	// wanted while either is.
	old := time.Now().AddDate(-1, 0, 0)
	recs, _ := filepath.Glob(filepath.Join(s.Dir(), "leases", "*"))
	for _, r := range recs {
		os.Chtimes(r, old, old)
	}
	if h := holding(t, s, hash); h.Renewed.After(old.Add(time.Minute)) {
		t.Fatalf("renewed %v, want %v", h.Renewed, old)
	}
	stranger := lease.Token([]byte("not stored here"))
	known, err := s.Renew("bob", []string{b, stranger, "junk"})
	if err != nil || known != 1 {
		t.Fatalf("renew: %d %v", known, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "leases", lease.ID(stranger))); !os.IsNotExist(err) {
		t.Fatal("renewing a lease not held here made a record of it")
	}
	if h := holding(t, s, hash); time.Since(h.Renewed) > time.Minute {
		t.Fatalf("renewed %v", h.Renewed)
	}
	// An ID isn't a token: knowing the lease ID doesn't renew it.
	if known, _ := s.Renew("bob", []string{lease.ID(a)}); known != 0 {
		t.Fatal("renewed with the lease ID instead of the token")
	}

	// A shard from before leases is wanted while its writer renews.
	plain, _ := s.PutOwned([]byte("older shard"), "carol")
	os.Chtimes(filepath.Join(s.Dir(), plain), old, old)
	os.Chtimes(filepath.Join(s.Dir(), "leases", ".since"), old, old)
	if h := holding(t, s, plain); h.Leases != 0 || h.Renewed.After(old.Add(time.Minute)) {
		t.Fatalf("%+v", h)
	}
	s.Renew("carol", nil)
	if h := holding(t, s, plain); time.Since(h.Renewed) > time.Minute {
		t.Fatalf("writer renewed, shard still at %v", h.Renewed)
	}

	// Deleting a shard takes its leases with it.
	if err := s.DeleteOwned(hash, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), hash+leasesExt)); !os.IsNotExist(err) {
		t.Fatal("lease list left behind")
	}
}

// Nothing counts as unrenewed from before the store kept track.
func TestLeasesSince(t *testing.T) {
	s := New(t.TempDir())
	hash, _ := s.PutOwned([]byte("shard"), "dave")
	old := time.Now().AddDate(-2, 0, 0)
	os.Chtimes(filepath.Join(s.Dir(), hash), old, old)
	s.Renew("someone else", nil) // starts keeping track
	if h := holding(t, s, hash); time.Since(h.Renewed) > time.Minute {
		t.Fatalf("counted unrenewed from %v, before tracking began", h.Renewed)
	}
}

// Records of leases whose shards are all gone are pruned.
func TestPruneLeases(t *testing.T) {
	s := New(t.TempDir())
	a, b := lease.Token([]byte("a")), lease.Token([]byte("b"))
	ha, _ := s.PutLeased([]byte("shard a"), "w", lease.ID(a))
	s.PutLeased([]byte("shard b"), "w", lease.ID(b))
	s.DeleteOwned(ha, "w")
	if err := s.PruneLeases(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "leases", lease.ID(a))); !os.IsNotExist(err) {
		t.Error("record of a lease with no shards left kept")
	}
	if known, _ := s.Renew("w", []string{a, b}); known != 1 {
		t.Errorf("%d leases known after pruning, want 1", known)
	}
}
