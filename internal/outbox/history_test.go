package outbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
)

// shardsGone reports whether no shard of any of ms is on its node.
func shardsGone(t *testing.T, w *Watcher, ms ...manifest.Manifest) bool {
	t.Helper()
	for _, m := range ms {
		for _, ch := range m.Chunks {
			for _, ref := range ch.Shards {
				if ok, _ := w.cfg.Client.Has(context.Background(), ref.Peer, ref.Hash); ok {
					return false
				}
			}
		}
	}
	return true
}

func TestVersionsAreKeptAndRestorable(t *testing.T) {
	w, dir, events := setup(t, 4)
	p := filepath.Join(dir, "Docs", "plan.txt")
	os.MkdirAll(filepath.Dir(p), 0o755)
	v1 := randBytes(300_000) // 5 chunks of 64 KiB
	os.WriteFile(p, v1, 0o644)
	settle(t, w, p)
	stub := p + files.StubExt
	m1, err := files.ReadStub(stub)
	if err != nil {
		t.Fatalf("not stored: %v (events: %v)", err, *events)
	}

	// A changed copy dropped beside the stub becomes the new version.
	v2 := append([]byte(nil), v1...)
	copy(v2[70_000:], "edited")
	os.WriteFile(p, v2, 0o644)
	settle(t, w, p)
	m2, _ := files.ReadStub(stub)
	if m2.FileID == m1.FileID || m2.Lineage != m1.FileID || files.Reused(m2, m1) != len(m2.Chunks)-1 {
		t.Fatalf("second version: lineage %q, reused %d of %d (events: %v)", m2.Lineage, files.Reused(m2, m1), len(m2.Chunks), *events)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("original not replaced by its stub")
	}
	vs, err := w.Versions(stub)
	if err != nil || len(vs) != 2 || !vs[0].Current || vs[1].Current {
		t.Fatalf("versions = %+v, %v", vs, err)
	}

	// The old version restores under a dated name; the current one as is.
	old, err := w.VersionStub(stub, vs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.RestoreStub(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, v1) || !strings.HasPrefix(filepath.Base(got), "plan (") {
		t.Fatalf("old version restored as %s", got)
	}
	got, _ = w.RestoreStub(context.Background(), stub)
	if b, _ := os.ReadFile(got); !bytes.Equal(b, v2) {
		t.Fatal("current version differs")
	}
	if _, err := w.VersionStub(stub, "../../x.ystub"); err == nil {
		t.Fatal("version id with a path accepted")
	}

	// Deleting the item deletes every version's shards and the history.
	os.Rename(stub, filepath.Join(dir, DeleteDir, "plan.txt"+files.StubExt))
	settle(t, w, filepath.Join(dir, DeleteDir))
	if !shardsGone(t, w, m1, m2) {
		t.Fatalf("shards left after delete (events: %v)", *events)
	}
	if _, err := os.Stat(w.historyPath(m1.FileID)); !os.IsNotExist(err) {
		t.Fatal("history left after delete")
	}
	if last := (*events)[len(*events)-1]; !strings.Contains(last, "with its 1 older versions") {
		t.Fatalf("last event = %q", last)
	}
}

func TestKeptOriginalsGetVersionsWhenEdited(t *testing.T) {
	w, dir, events := setup(t, 4)
	w.cfg.Keep = true
	p := filepath.Join(dir, "notes.md")
	os.WriteFile(p, []byte("first"), 0o644)
	settle(t, w, p)
	w.poll(context.Background()) // unchanged: nothing happens
	m1, _ := files.ReadStub(p + files.StubExt)
	if w.VersionCount(m1) != 0 {
		t.Fatalf("unchanged file made a version (events: %v)", *events)
	}
	os.WriteFile(p, []byte("second, longer"), 0o644)
	settle(t, w, p)
	m2, _ := files.ReadStub(p + files.StubExt)
	if w.VersionCount(m2) != 1 || m2.PlaintextSize != len("second, longer") {
		t.Fatalf("edit not versioned (events: %v)", *events)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("kept original was removed")
	}
}

func TestOldVersionsArePruned(t *testing.T) {
	defer func(n int) { KeepVersions = n }(KeepVersions)
	KeepVersions = 2
	w, dir, events := setup(t, 4)
	p := filepath.Join(dir, "log.txt")
	var all []manifest.Manifest
	for i := range 5 {
		os.WriteFile(p, randBytes(1000+i), 0o644)
		settle(t, w, p)
		m, err := files.ReadStub(p + files.StubExt)
		if err != nil {
			t.Fatalf("version %d: %v (events: %v)", i, err, *events)
		}
		// Give each version its own second, so they sort.
		m.StoredAt = time.Now().Unix() - int64(100-i)
		files.WriteJSON(p+files.StubExt, m)
		all = append(all, m)
	}
	m, _ := files.ReadStub(p + files.StubExt)
	if n := w.VersionCount(m); n != KeepVersions {
		t.Fatalf("%d older versions kept, want %d (events: %v)", n, KeepVersions, *events)
	}
	if !shardsGone(t, w, all[0], all[1]) || shardsGone(t, w, all[2]) || shardsGone(t, w, all[4]) {
		t.Fatal("pruning deleted the wrong shards")
	}
}

func TestRestoreFolderAsOf(t *testing.T) {
	w, dir, events := setup(t, 4)
	folder := filepath.Join(dir, "Project")
	os.MkdirAll(filepath.Join(folder, "sub"), 0o755)
	a1, a2, b := randBytes(5000), randBytes(6000), randBytes(700)
	write := func(rel string, data []byte, at time.Time) {
		p := filepath.Join(folder, rel)
		os.WriteFile(p, data, 0o644)
		settle(t, w, p)
		m, err := files.ReadStub(p + files.StubExt)
		if err != nil {
			t.Fatalf("%s: %v (events: %v)", rel, err, *events)
		}
		m.StoredAt = at.Unix()
		files.WriteJSON(p+files.StubExt, m)
	}
	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	write("a.txt", a1, t1)
	write("sub/b.bin", b, t1.Add(24*time.Hour))
	write("a.txt", a2, t1.Add(48*time.Hour))
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("not in the folder"), 0o644)
	settle(t, w, filepath.Join(dir, "other.txt"))

	// As of day 2: the first a.txt, and b.bin.
	target, n, err := w.RestoreFolderAt(context.Background(), "Project", t1.Add(36*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("restore: %d items, %v (events: %v)", n, err, *events)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "a.txt")); !bytes.Equal(got, a1) {
		t.Fatal("a.txt is not the first version")
	}
	if got, _ := os.ReadFile(filepath.Join(target, "sub", "b.bin")); !bytes.Equal(got, b) {
		t.Fatal("sub/b.bin missing or wrong")
	}
	if _, err := os.Stat(filepath.Join(target, "other.txt")); err == nil {
		t.Fatal("an item outside the folder was restored")
	}
	// As of day 1, only a.txt existed.
	target, n, _ = w.RestoreFolderAt(context.Background(), "Project", t1.Add(time.Hour))
	if _, err := os.Stat(filepath.Join(target, "sub")); n != 1 || err == nil {
		t.Fatalf("as of day 1: %d items", n)
	}
	if _, _, err := w.RestoreFolderAt(context.Background(), "Project", t1.Add(-time.Hour)); err == nil {
		t.Fatal("restore before anything was stored succeeded")
	}
}
