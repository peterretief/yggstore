package localstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/peterretief/yggstore/internal/localstore"
)

func TestSweepReportsCorruptionAndPreservesOwnership(t *testing.T) {
	dir := t.TempDir()
	store := localstore.New(dir)
	good, err := store.PutOwned([]byte("healthy"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	bad, err := store.PutOwned([]byte("damaged"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bad), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".temporary"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	var reported []string
	checked, failed, err := store.Sweep(context.Background(), func(hash string, issue error) {
		if issue == nil {
			t.Fatal("missing issue")
		}
		reported = append(reported, hash)
	})
	if err != nil || checked != 2 || failed != 1 || len(reported) != 1 || reported[0] != bad {
		t.Fatalf("checked=%d failed=%d reported=%v err=%v", checked, failed, reported, err)
	}
	if _, err := store.Get(good); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, bad))
	if err != nil || string(data) != "corrupt" {
		t.Fatalf("corrupt shard altered: %q %v", data, err)
	}
	if err := store.DeleteOwned(bad, "other"); !errors.Is(err, localstore.ErrNotOwner) {
		t.Fatal(err)
	}
	if err := store.DeleteOwned(bad, "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestSweepCancellationAndMissingStore(t *testing.T) {
	store := localstore.New(filepath.Join(t.TempDir(), "missing"))
	if checked, failed, err := store.Sweep(context.Background(), nil); err != nil || checked != 0 || failed != 0 {
		t.Fatalf("%d %d %v", checked, failed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Sweep(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSweepRejectsNonRegularShard(t *testing.T) {
	dir := t.TempDir()
	store := localstore.New(dir)
	hash, err := store.Put([]byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, hash)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(dir, hash)); err != nil {
		t.Fatal(err)
	}
	if checked, failed, err := store.Sweep(context.Background(), nil); checked != 1 || failed != 1 || err != nil {
		t.Fatalf("%d %d %v", checked, failed, err)
	}
}

func TestUsageByWriter(t *testing.T) {
	store := localstore.New(t.TempDir())
	store.PutOwned([]byte("aaaa"), "alice")
	store.PutOwned([]byte("bb"), "alice")
	store.PutOwned([]byte("ccc"), "bob")
	store.PutOwned([]byte("aaaa"), "bob") // already Alice's: stays hers
	got, err := store.UsageByWriter()
	if err != nil || got["alice"] != 6 || got["bob"] != 3 || len(got) != 2 {
		t.Fatalf("usage %v, %v; want alice 6, bob 3", got, err)
	}
}
