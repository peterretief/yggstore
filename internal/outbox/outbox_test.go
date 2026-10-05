package outbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
)

func setup(t *testing.T, nodes int) (*Watcher, string, *[]string) {
	t.Helper()
	var list []peers.Peer
	for i := range nodes {
		h := server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	peersPath := filepath.Join(t.TempDir(), "peers.json")
	b, _ := json.Marshal(list)
	os.WriteFile(peersPath, b, 0o600)

	dir := t.TempDir()
	var events []string
	w := New(Config{Dir: dir, PeersPath: peersPath, Client: client.New(), ChunkSize: 64 << 10, Challenges: 2,
		Event: func(kind, msg string) { events = append(events, kind+": "+msg) }})
	for _, d := range []string{RestoreDir, RestoredDir, DeleteDir, WholeDir, filepath.Join(privateDir, "restored-stubs")} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	return w, dir, &events
}

// settle backdates mtimes so items count as finished copying, then polls twice.
func settle(t *testing.T, w *Watcher, paths ...string) {
	t.Helper()
	old := time.Now().Add(-time.Minute)
	for _, p := range paths {
		filepath.WalkDir(p, func(q string, _ fs.DirEntry, _ error) error { return os.Chtimes(q, old, old) })
	}
	w.poll(context.Background())
	w.poll(context.Background())
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestFileAndFolderRoundTrip(t *testing.T) {
	w, dir, events := setup(t, 4)

	fileData := randBytes(300_000)
	os.WriteFile(filepath.Join(dir, "photo.jpg"), fileData, 0o644)
	// A folder dropped into whole/ is stored as one item.
	folder := filepath.Join(dir, WholeDir, "Holiday")
	os.MkdirAll(filepath.Join(folder, "day2"), 0o755)
	a, b := randBytes(70_000), randBytes(5)
	os.WriteFile(filepath.Join(folder, "a.txt"), a, 0o644)
	os.WriteFile(filepath.Join(folder, "day2", "b.bin"), b, 0o600)

	settle(t, w, filepath.Join(dir, "photo.jpg"), folder)

	for _, name := range []string{"photo.jpg", filepath.Join(WholeDir, "Holiday")} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still present after store (events: %v)", name, *events)
		}
		stub := filepath.Join(dir, name+files.StubExt)
		if _, err := files.ReadStub(stub); err != nil {
			t.Fatalf("stub %s: %v", name, err)
		}
		if _, err := os.Stat(files.ChallengesPath(stub)); err != nil {
			t.Fatalf("challenges for %s: %v", name, err)
		}
	}
	m, _ := files.ReadStub(filepath.Join(dir, WholeDir, "Holiday"+files.StubExt))
	if m.Kind != files.KindFolder || m.FileCount != 2 || m.ContentBytes != int64(len(a)+len(b)) {
		t.Fatalf("folder manifest kind=%q count=%d bytes=%d", m.Kind, m.FileCount, m.ContentBytes)
	}

	// Restore by dropping copies of the stubs into restore/.
	for _, name := range []string{"photo.jpg", filepath.Join(WholeDir, "Holiday")} {
		data, _ := os.ReadFile(filepath.Join(dir, name+files.StubExt))
		os.WriteFile(filepath.Join(dir, RestoreDir, filepath.Base(name)+files.StubExt), data, 0o600)
	}
	settle(t, w, filepath.Join(dir, RestoreDir))

	restored := filepath.Join(dir, RestoredDir)
	if got, _ := os.ReadFile(filepath.Join(restored, "photo.jpg")); !bytes.Equal(got, fileData) {
		t.Fatalf("restored file differs (events: %v)", *events)
	}
	if got, _ := os.ReadFile(filepath.Join(restored, "Holiday", "a.txt")); !bytes.Equal(got, a) {
		t.Fatal("restored folder a.txt differs")
	}
	if got, _ := os.ReadFile(filepath.Join(restored, "Holiday", "day2", "b.bin")); !bytes.Equal(got, b) {
		t.Fatal("restored folder day2/b.bin differs")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, RestoreDir)); len(left) != 0 {
		t.Fatalf("restore/ not emptied: %d entries", len(left))
	}

	// Restoring again must not overwrite the first copy.
	if p, err := w.RestoreStub(context.Background(), filepath.Join(dir, "photo.jpg"+files.StubExt)); err != nil || filepath.Base(p) != "photo (2).jpg" {
		t.Fatalf("second restore = %q, %v", p, err)
	}
}

func TestContainersOnTooFewMachinesKeepOriginal(t *testing.T) {
	w, dir, events := setup(t, 6)
	// Six nodes, but five are containers on one machine: two machines in all.
	list, _ := peers.Load(w.cfg.PeersPath)
	for i := 1; i < len(list); i++ {
		list[i].Host = "box"
	}
	b, _ := json.Marshal(list)
	os.WriteFile(w.cfg.PeersPath, b, 0o600)

	p := filepath.Join(dir, "doc.pdf")
	os.WriteFile(p, randBytes(1000), 0o644)
	settle(t, w, p)
	if _, err := os.Stat(p + files.StubExt); !os.IsNotExist(err) {
		t.Fatal("stored on two machines, where losing one could lose the file")
	}
	if len(*events) == 0 || !strings.Contains((*events)[len(*events)-1], "only 2 machines online") {
		t.Fatalf("expected a too-few-machines event, got %v", *events)
	}
}

func TestTooFewNodesKeepsOriginal(t *testing.T) {
	w, dir, events := setup(t, 2)
	p := filepath.Join(dir, "doc.pdf")
	os.WriteFile(p, randBytes(1000), 0o644)
	settle(t, w, p)
	if _, err := os.Stat(p); err != nil {
		t.Fatal("original removed although too few nodes were online")
	}
	if _, err := os.Stat(p + files.StubExt); !os.IsNotExist(err) {
		t.Fatal("stub written although too few nodes were online")
	}
	if len(*events) == 0 || !strings.Contains((*events)[len(*events)-1], "only 2 machines online") {
		t.Fatalf("expected a too-few-nodes event, got %v", *events)
	}
}

func TestUnstableItemIsLeftAlone(t *testing.T) {
	w, dir, _ := setup(t, 4)
	p := filepath.Join(dir, "growing.log")
	os.WriteFile(p, []byte("x"), 0o644)
	w.poll(context.Background())
	os.WriteFile(p, []byte("xy"), 0o644) // still being written
	w.poll(context.Background())
	if _, err := os.Stat(p + files.StubExt); !os.IsNotExist(err) {
		t.Fatal("stored a file that was still changing")
	}
}

func TestDeleteFolderRemovesItem(t *testing.T) {
	w, dir, events := setup(t, 4)
	os.MkdirAll(filepath.Join(dir, DeleteDir), 0o755)
	p := filepath.Join(dir, "old.mov")
	os.WriteFile(p, randBytes(200_000), 0o644)
	settle(t, w, p)
	stub := p + files.StubExt
	m, err := files.ReadStub(stub)
	if err != nil {
		t.Fatalf("not stored: %v (events: %v)", err, *events)
	}

	// Moving the stub into delete/ leaves its hidden challenges file behind;
	// that must be cleaned up too.
	dropped := filepath.Join(dir, DeleteDir, filepath.Base(stub))
	if err := os.Rename(stub, dropped); err != nil {
		t.Fatal(err)
	}
	settle(t, w, dropped)

	for _, q := range []string{dropped, files.ChallengesPath(stub)} {
		if _, err := os.Stat(q); !os.IsNotExist(err) {
			t.Fatalf("%s still present after delete (events: %v)", q, *events)
		}
	}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if ok, _ := w.cfg.Client.Has(context.Background(), ref.Peer, ref.Hash); ok {
				t.Fatalf("shard %s still on %s", ref.Hash[:12], ref.Peer)
			}
		}
	}
	if last := (*events)[len(*events)-1]; !strings.Contains(last, "deleted old.mov") {
		t.Fatalf("last event = %q", last)
	}
}

func TestFoldersAreStoredFileByFile(t *testing.T) {
	w, dir, events := setup(t, 4)
	movies := filepath.Join(dir, "Movies", "2026")
	os.MkdirAll(movies, 0o755)
	x, y := randBytes(90_000), randBytes(3)
	os.WriteFile(filepath.Join(movies, "x.mkv"), x, 0o644)
	os.WriteFile(filepath.Join(dir, "Movies", "y.txt"), y, 0o644)
	os.WriteFile(filepath.Join(movies, "half.mkv.part"), y, 0o644) // still downloading
	settle(t, w, filepath.Join(dir, "Movies"))

	for _, rel := range []string{"Movies/2026/x.mkv", "Movies/y.txt"} {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still present after store (events: %v)", rel, *events)
		}
		if _, err := files.ReadStub(p + files.StubExt); err != nil {
			t.Fatalf("no stub beside %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(movies, "half.mkv.part")); err != nil {
		t.Fatal("a half-downloaded file was taken")
	}

	// Restore one file with the dashboard's call: the rest stay stubs.
	got, err := w.RestoreStub(context.Background(), filepath.Join(movies, "x.mkv"+files.StubExt))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, x) {
		t.Fatal("restored x.mkv differs")
	}

	// Putting it back next to its own stub is refused with a warning, once.
	os.Rename(got, filepath.Join(movies, "x.mkv"))
	settle(t, w, movies)
	w.poll(context.Background())
	warned := 0
	for _, e := range *events {
		if strings.Contains(e, "x.mkv is not stored") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("got %d warnings about x.mkv beside its stub, want 1 (events: %v)", warned, *events)
	}

	// Deleting a stub from inside the tree removes that item only.
	os.Remove(filepath.Join(movies, "x.mkv"))
	stub := filepath.Join(dir, "Movies", "y.txt"+files.StubExt)
	m, _ := files.ReadStub(stub)
	os.Rename(stub, filepath.Join(dir, DeleteDir, "y.txt"+files.StubExt))
	settle(t, w, filepath.Join(dir, DeleteDir))
	if _, err := os.Stat(files.ChallengesPath(stub)); !os.IsNotExist(err) {
		t.Fatal("challenges of the deleted item left behind in its folder")
	}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if ok, _ := w.cfg.Client.Has(context.Background(), ref.Peer, ref.Hash); ok {
				t.Fatal("a shard of the deleted item is still stored")
			}
		}
	}
	if _, err := os.Stat(filepath.Join(movies, "x.mkv"+files.StubExt)); err != nil {
		t.Fatal("deleting y.txt removed x.mkv's stub")
	}
}

func TestReceiveSharedItem(t *testing.T) {
	alice, dir, _ := setup(t, 4) // the sender, storing on the shared nodes
	keys := t.TempDir()
	aliceID, _ := share.LoadOrCreate(filepath.Join(keys, "alice"))
	bobID, _ := share.LoadOrCreate(filepath.Join(keys, "bob"))

	data := randBytes(150_000)
	p := filepath.Join(dir, "plan.pdf")
	os.WriteFile(p, data, 0o644)
	settle(t, alice, p)
	stub, _ := os.ReadFile(p + files.StubExt)
	sealed, err := aliceID.Seal(bobID.Code(), stub, "Alice", "")
	if err != nil {
		t.Fatal(err)
	}

	// Bob's outbox uses the same nodes.
	bobDir := t.TempDir()
	var events []string
	bob := New(Config{Dir: bobDir, PeersPath: alice.cfg.PeersPath, Client: client.New(), Identity: bobID,
		Event: func(kind, msg string) { events = append(events, kind+": "+msg) }})
	for _, d := range []string{RestoreDir, RestoredDir, DeleteDir, WholeDir, ReceivedDir, filepath.Join(privateDir, "restored-stubs")} {
		os.MkdirAll(filepath.Join(bobDir, d), 0o755)
	}
	drop := filepath.Join(bobDir, "plan.pdf"+share.Ext)
	os.WriteFile(drop, sealed, 0o644)
	settle(t, bob, drop)

	got := filepath.Join(bobDir, ReceivedDir, "plan.pdf"+files.StubExt)
	m, err := files.ReadStub(got)
	if err != nil {
		t.Fatalf("no received stub (events: %v)", events)
	}
	if m.SharedBy == nil || m.SharedBy.Name != "Alice" || m.SharedBy.Code != aliceID.Code() {
		t.Fatalf("shared_by = %+v", m.SharedBy)
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Fatal(".ysend left behind after opening")
	}
	out, err := bob.RestoreStub(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, data) {
		t.Fatal("restored file differs")
	}

	// Removing it on Bob's side keeps Alice's copy.
	if err := bob.DeleteStub(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatal("received stub not removed")
	}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if ok, _ := alice.cfg.Client.Has(context.Background(), ref.Peer, ref.Hash); !ok {
				t.Fatal("removing a received item deleted the sender's shards")
			}
		}
	}

	// A .ysend for someone else, or one pointing at a non-overlay address, is refused.
	eveID, _ := share.LoadOrCreate(filepath.Join(keys, "eve"))
	forEve, _ := aliceID.Seal(eveID.Code(), stub, "Alice", "")
	other := filepath.Join(bobDir, "x"+share.Ext)
	os.WriteFile(other, forEve, 0o644)
	var bad manifest.Manifest
	json.Unmarshal(stub, &bad)
	bad.Chunks[0].Shards[0].Peer = "192.168.0.1:22"
	badStub, _ := json.Marshal(bad)
	sneaky, _ := aliceID.Seal(bobID.Code(), badStub, "Alice", "")
	sneakyPath := filepath.Join(bobDir, "y"+share.Ext)
	os.WriteFile(sneakyPath, sneaky, 0o644)
	settle(t, bob, other, sneakyPath)
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "someone else's sharing code") || !strings.Contains(joined, "non-Yggdrasil address") {
		t.Fatalf("refusals not reported: %v", events)
	}
	if _, err := os.Stat(filepath.Join(bobDir, ReceivedDir, "plan.pdf (2)"+files.StubExt)); !os.IsNotExist(err) {
		t.Fatal("a refused file was opened")
	}
}
