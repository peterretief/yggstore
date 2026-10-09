package dashboard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/devices"
)

func TestRemoteDevices(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "devices.json")
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Listen: "127.0.0.1:7480",
		YggAddr: "200::1", Devices: devices.NewWatch(list)})
	h := d.Handler()
	do := func(method, target, host, local, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Host, r.RemoteAddr = host, net.JoinHostPort(remote, "50000")
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(local), Port: 7480}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := do("GET", "/", "127.0.0.1:7480", "127.0.0.1", "127.0.0.1"); w.Code != http.StatusOK {
		t.Fatalf("on this machine: %d", w.Code)
	}

	// From the LAN, by IP or .local name: sent to the Yggdrasil address.
	w := do("GET", "/api/state?x=1", "box.local:7480", "192.168.0.5", "192.168.0.20")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "http://[200::1]:7480/api/state?x=1" {
		t.Fatalf("from the LAN: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if w := do("POST", "/api/verify", "box.local:7480", "192.168.0.5", "192.168.0.20"); w.Code != http.StatusForbidden {
		t.Fatalf("action from the LAN: %d, want refused", w.Code)
	}

	// Over Yggdrasil, a device not on the list is told its address.
	w = do("GET", "/", "[200::1]:7480", "200::1", "201::9")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "201::9") {
		t.Fatalf("unknown device: %d %q", w.Code, w.Body.String())
	}
	if w := do("GET", "/", "[200::1]:7480", "200::1", "200::1"); w.Code != http.StatusOK {
		t.Fatalf("this machine over Yggdrasil: %d", w.Code)
	}

	if _, err := devices.Add(list, "201:0::9", "laptop"); err != nil {
		t.Fatal(err)
	}
	if w := do("GET", "/", "[200::1]:7480", "200::1", "201::9"); w.Code != http.StatusOK {
		t.Fatalf("allowed device: %d", w.Code)
	}
	if w := do("GET", "/", "evil.example:7480", "200::1", "201::9"); w.Code != http.StatusForbidden {
		t.Fatalf("allowed device, another host name (DNS rebinding): %d", w.Code)
	}
	if w := do("POST", "/api/verify", "[200::1]:7480", "200::1", "201::9"); w.Code != http.StatusForbidden {
		t.Fatalf("action without the header: %d", w.Code)
	}

	if _, err := devices.Remove(list, "laptop"); err != nil {
		t.Fatal(err)
	}
	if w := do("GET", "/", "[200::1]:7480", "200::1", "201::9"); w.Code != http.StatusForbidden {
		t.Fatalf("removed device: %d", w.Code)
	}
}

func TestRemoteOff(t *testing.T) {
	dir := t.TempDir()
	h := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Listen: "127.0.0.1:7480"}).Handler()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host, r.RemoteAddr = "[200::1]:7480", "[201::9]:50000"
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("200::1"), Port: 7480}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("without -remote, from another machine: %d", w.Code)
	}
}
