package light

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestRelay(t *testing.T) {
	var got *http.Request
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r; w.Write([]byte("from the box")) }))
	defer box.Close()
	list := []peers.Peer{{Name: "box", Addr: "[201::4]:7400"}}
	rl := &Relay{Peers: func() []peers.Peer { return list }, BoxURL: func(ip string) string {
		if ip != "201::4" {
			t.Errorf("relayed to %s", ip)
		}
		return box.URL
	}}
	do := func(method, target, cookie string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Host = "my.example.org"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: BoxCookie, Value: cookie})
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		rl.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/files/join?box=nowhere&t=x", "", nil); w.Code != 404 {
		t.Errorf("an invite for an unknown box: %d", w.Code)
	}
	if w := do("GET", "/files", "", nil); w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Errorf("files without a box: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := do("GET", "/etc/passwd", "box", nil); w.Code != 404 {
		t.Errorf("a path outside /files: %d", w.Code)
	}
	if w := do("GET", "/", "box", nil); w.Code != http.StatusFound {
		t.Errorf("home with a box: %d", w.Code)
	}
	w := do("GET", "/files?op=x", "box", map[string]string{RelayHeader: "0", "CF-Connecting-IP": "198.51.100.7", "X-Forwarded-Host": "evil.example"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "from the box") {
		t.Fatalf("relayed: %d %s", w.Code, w.Body)
	}
	if got.Header.Get(RelayHeader) != "1" || got.Header.Get("X-Forwarded-Host") != "my.example.org" || got.Header.Get("X-Forwarded-For") != "198.51.100.7" ||
		got.URL.RequestURI() != "/files?op=x" {
		t.Fatalf("the box got %s with %v", got.URL, got.Header)
	}
	box.Close()
	if w := do("GET", "/files", "box", nil); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "isn't answering") {
		t.Fatalf("box off: %d %s", w.Code, w.Body)
	}
}

func TestRelayPersonHost(t *testing.T) {
	var got *http.Request
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r }))
	defer box.Close()
	list := []peers.Peer{{Name: "box", Addr: "[201::4]:7400", PeopleNames: []peers.PersonName{{Name: "kath.example.org", Person: "Kath", Code: "ys1x"}}}}
	rl := &Relay{Peers: func() []peers.Peer { return list }, BoxURL: func(ip string) string {
		if ip != "201::4" {
			t.Errorf("relayed to %s", ip)
		}
		return box.URL
	}}
	do := func(host, target string) int {
		got = nil
		r := httptest.NewRequest("GET", target, nil)
		r.Host = host
		w := httptest.NewRecorder()
		rl.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("kath.example.org", "/files/join?t=x"); c != 200 || got == nil || got.Header.Get("X-Forwarded-Host") != "kath.example.org" {
		t.Fatalf("kath's invite: %d %v", c, got)
	}
	if c := do("Kath.Example.org", "/files"); c != 200 || got == nil {
		t.Fatalf("kath's files: %d", c)
	}
	if c := do("kath.example.org", "/"); c != 404 || got != nil {
		t.Fatalf("kath's website went to the box: %d", c)
	}
}
