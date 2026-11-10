package light

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

// A member's own node sends its member with People.Member: the web node
// then reaches it on its node API, and the login gives the member's
// address from the node's own name.
func TestMemberNode(t *testing.T) {
	list := []peers.Peer{
		{Name: "web", Addr: "[201:9::1]:7400", Admin: true, Domain: "example.org"},
		{Name: "desktop", Addr: "[201:9::2]:7400", Owner: "peter", Names: []string{"peter.example.org"}},
		{Name: "box", Addr: "[201:9::3]:7400"},
	}
	s := &Store{Dir: filepath.Join(t.TempDir(), "web"), Peers: func() []peers.Peer { return list }}
	rl := &Relay{Peers: func() []peers.Peer { return list }, Signin: s}
	if got := rl.boxURL("201:9::2"); got != "http://[201:9::2]:7490" {
		t.Fatalf("before it says it's a member: %s", got)
	}
	if rl.email("desktop", "peter") != "" {
		t.Fatal("an address for a node that isn't a member's")
	}
	put := func(caller, body string) int {
		w := httptest.NewRecorder()
		s.Serve(w, httptest.NewRequest("PUT", "/v1/light/people", strings.NewReader(body)), caller)
		return w.Code
	}
	if c := put("201:9::2", `{"member":true,"people":[{"name":"peter","web":true,"keys":[]}]}`); c != http.StatusNoContent {
		t.Fatalf("sending its member: %d", c)
	}
	if got := rl.boxURL("201:9::2"); got != "http://[201:9::2]:7400"+MemberBase {
		t.Fatalf("a member's node: %s", got)
	}
	if got := rl.boxURL("201:9::3"); got != "http://[201:9::3]:7490" {
		t.Fatalf("a box: %s", got)
	}
	if e := rl.email("desktop", "peter"); e != "peter@example.org" {
		t.Fatalf("the member's address: %q", e)
	}
	// A box saying nothing of it stays a box.
	put("201:9::3", `{"people":[]}`)
	if s.IsMember("box") {
		t.Fatal("a box became a member's node")
	}
}
