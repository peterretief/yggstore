package peers

import (
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
