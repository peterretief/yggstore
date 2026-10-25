package mesh

import (
	"crypto/ed25519"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func newKey(t *testing.T) (key, addr string) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key = hex.EncodeToString(pub)
	return key, KeyAddress(key)
}

func member(name, addr string) peers.Peer {
	return peers.Peer{Name: name, Addr: net.JoinHostPort(addr, "7400")}
}

func TestAllowed(t *testing.T) {
	handKey, handAddr := newKey(t) // put in by hand, a member
	byHand, _ := newKey(t)         // put in by hand, no member's
	oldKey, _ := newKey(t)         // added earlier, its member has left
	keptKey, keptAddr := newKey(t) // added earlier, still a member
	newcomer, newAddr := newKey(t) // a member not allowed yet
	stranger, _ := newKey(t)       // reports a key that's no member's
	impostorKey, _ := newKey(t)    // a member reporting someone else's key
	members := []peers.Peer{member("a", handAddr), member("b", keptAddr), member("new", newAddr)}

	next, added := Allowed(
		[]string{handKey, byHand, oldKey, keptKey, handKey},
		members,
		[]string{handKey, keptKey, newcomer, stranger, impostorKey, "not-a-key"},
		[]string{oldKey, keptKey},
	)
	if want := []string{handKey, byHand, keptKey, newcomer}; !reflect.DeepEqual(next, want) {
		t.Fatalf("allowed %v\nwant %v", next, want)
	}
	if !reflect.DeepEqual(added, sorted(keptKey, newcomer)) {
		t.Fatalf("added %v", added)
	}

	// Again with nothing found (the members are down): nothing changes.
	again, _ := Allowed(next, members, nil, added)
	if !reflect.DeepEqual(again, next) {
		t.Fatalf("with nothing found: %v", again)
	}
}

func sorted(a, b string) []string {
	if a > b {
		a, b = b, a
	}
	return []string{a, b}
}

func TestAllowConf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yggdrasil.conf")
	os.WriteFile(path, []byte(`{"Listen": ["tls://[::]:14415"], "AllowedPublicKeys": ["aa"], "IfName": "auto"}`), 0o600)
	c := &AllowConf{Path: path}
	keys, err := c.Keys()
	if err != nil || !reflect.DeepEqual(keys, []string{"aa"}) {
		t.Fatalf("%v %v", keys, err)
	}
	if err := c.SetKeys([]string{"aa", "bb"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"bb"`, `"tls://[::]:14415"`, `"IfName": "auto"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s lost %s", b, want)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}

	os.WriteFile(path, []byte("{\n  # HJSON\n  AllowedPublicKeys: [aa]\n}"), 0o600)
	if _, err := c.Keys(); err == nil || !strings.Contains(err.Error(), "left alone") {
		t.Fatalf("HJSON: %v", err)
	}
	os.WriteFile(path, []byte(`{"AllowedPublicKeys": []}`), 0o600)
	if _, err := c.Keys(); err == nil || !strings.Contains(err.Error(), "anyone") {
		t.Fatalf("open to anyone: %v", err)
	}
}
