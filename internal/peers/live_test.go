package peers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveFollowsPushesAndHandEdits(t *testing.T) {
	dir := t.TempDir()
	base, pushed := filepath.Join(dir, "peers.json"), filepath.Join(dir, "peers.pushed.json")
	admin := Peer{Name: "desktop", Addr: "[200::1]:7400", Admin: true}
	if err := Write(base, []Peer{admin, {Name: "pi", Addr: "[200::2]:7400"}}); err != nil {
		t.Fatal(err)
	}
	l, err := NewLive(base, pushed, "200::9")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Allowed("200::2") || !l.Allowed("200::9") || l.Allowed("200::3") {
		t.Fatal("allow-list wrong after start")
	}

	if err := l.Replace("200::2", []Peer{{Name: "pi", Addr: "[200::2]:7400", Admin: true}}); err != ErrNotAdmin {
		t.Fatalf("a non-admin replaced the list: %v", err)
	}
	if err := l.Replace("200::1", []Peer{{Name: "desktop", Addr: "[200::1]:7400"}}); err == nil {
		t.Fatal("the admin removed itself as admin")
	}
	added := []Peer{admin, {Name: "pi", Addr: "[200::2]:7400"}, {Name: "new", Addr: "[200::3]:7400"}}
	if err := l.Replace("200::1", added); err != nil {
		t.Fatal(err)
	}
	if !l.Allowed("200::3") || l.Hash() != Hash(added) {
		t.Fatal("pushed list not in use")
	}
	if got, err := Load(base); err != nil || Hash(got) != Hash(added) {
		t.Fatalf("the list the node started with did not follow the push: %v", err)
	}

	// A restart picks the pushed list, since it is newer.
	if l2, _ := NewLive(base, pushed, "200::9"); !l2.Allowed("200::3") {
		t.Fatal("pushed list lost on restart")
	}

	// A later hand edit of the base file wins over the push.
	later := time.Now().Add(time.Minute)
	Write(base, []Peer{admin})
	os.Chtimes(base, later, later)
	if err := l.Refresh(); err != nil {
		t.Fatal(err)
	}
	if l.Allowed("200::3") || l.Allowed("200::2") {
		t.Fatal("hand edit did not override the pushed list")
	}

	// A broken file keeps the list in use.
	os.WriteFile(base, []byte("not json"), 0o644)
	os.Chtimes(base, later.Add(time.Minute), later.Add(time.Minute))
	if err := l.Refresh(); err == nil || !l.Allowed("200::1") {
		t.Fatal("broken file was not reported, or emptied the list")
	}
}

func TestFollow(t *testing.T) {
	dir := t.TempDir()
	base, pushed := filepath.Join(dir, "peers.json"), filepath.Join(dir, "peers.pushed.json")
	one := []Peer{{Name: "a", Addr: "[201::1]:7400"}}
	two := append(one, Peer{Name: "b", Addr: "[201::2]:7400", Leaving: true})
	if err := Write(base, one); err != nil {
		t.Fatal(err)
	}
	if ok, err := Follow(base, pushed); ok || err != nil {
		t.Fatalf("no push: copied %v, %v", ok, err)
	}
	if err := Write(pushed, two); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(pushed, future, future)
	if ok, err := Follow(base, pushed); !ok || err != nil {
		t.Fatalf("newer push: copied %v, %v", ok, err)
	}
	got, err := Load(base)
	if err != nil || len(got) != 2 || !got[1].Leaving {
		t.Fatalf("base after follow: %+v, %v", got, err)
	}
	os.Chtimes(pushed, future.Add(time.Minute), future.Add(time.Minute))
	if ok, _ := Follow(base, pushed); ok {
		t.Fatal("copied a push with the same list")
	}
}

func TestOnlineLeavesOutLeaving(t *testing.T) {
	list := []Peer{{Name: "a", Addr: "[201::1]:7400"}, {Name: "b", Addr: "[201::2]:7400", Leaving: true}}
	answer := func(context.Context, Peer) error { return nil }
	if got := Online(context.Background(), list, answer); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("Online: %+v", got)
	}
	if got := Probe(context.Background(), list, answer); len(got) != 2 {
		t.Fatalf("Probe: %+v", got)
	}
}

// A second write in the same clock tick, which leaves the file's time as
// it was, is still picked up.
func TestRefreshSameTick(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "peers.json")
	tick := time.Now()
	Write(base, []Peer{{Name: "me", Addr: "127.0.0.1:1"}})
	os.Chtimes(base, tick, tick)
	l, err := NewLive(base, filepath.Join(dir, "pushed.json"), "200::9")
	if err != nil {
		t.Fatal(err)
	}
	Write(base, []Peer{{Name: "me", Addr: "127.0.0.1:1", Admin: true}})
	os.Chtimes(base, tick, tick)
	l.Refresh()
	if !l.IsAdmin("127.0.0.1") {
		t.Fatal("the second write in the same tick was missed")
	}
}

func TestReplaceWithReadOnlyBase(t *testing.T) {
	etc, data := t.TempDir(), t.TempDir()
	base, pushed := filepath.Join(etc, "peers.json"), filepath.Join(data, "peers.pushed.json")
	admin := Peer{Name: "desktop", Addr: "[200::1]:7400", Admin: true}
	if err := Write(base, []Peer{admin}); err != nil {
		t.Fatal(err)
	}
	os.Chmod(etc, 0o555)
	defer os.Chmod(etc, 0o755)
	l, err := NewLive(base, pushed, "200::9")
	if err != nil {
		t.Fatal(err)
	}
	added := []Peer{admin, {Name: "new", Addr: "[200::3]:7400"}}
	if err := l.Replace("200::1", added); err != nil {
		t.Fatalf("a base the node can't write stopped the push: %v", err)
	}
	if !l.Allowed("200::3") {
		t.Fatal("pushed list not in use")
	}
}
