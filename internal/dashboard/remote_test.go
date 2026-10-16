package dashboard

import (
	"context"
	"encoding/json"
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

// A device not on the list can ask to be let in, and nothing else; the
// owner allows it from the dashboard, and then it opens the files page.
func TestDeviceAsks(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "devices.json")
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Listen: "127.0.0.1:7480",
		YggAddr: "200::1", Devices: devices.NewWatch(list)})
	h := d.Handler()
	do := func(method, target, body, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host, r.RemoteAddr = "[200::1]:7480", net.JoinHostPort(remote, "50000")
		if strings.HasPrefix(body, "name=") {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r.Header.Set("X-Yggstore", "1")
		}
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("200::1"), Port: 7480}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/files", "", "201::9"); w.Code != http.StatusForbidden {
		t.Fatalf("files page from a device not let in: %d", w.Code)
	}
	if w := do("POST", "/api/devices/add", `{"addr":"201::9","name":"me"}`, "201::9"); w.Code != http.StatusForbidden {
		t.Fatalf("a device adding itself: %d", w.Code)
	}
	if w := do("POST", "/ask", "name=Anna's phone", "201::9"); w.Code != http.StatusOK {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	w := do("GET", "/api/devices", "", "200::1")
	if !strings.Contains(w.Body.String(), `"addr":"201::9","name":"Anna's phone"`) {
		t.Fatalf("the ask isn't shown: %s", w.Body)
	}
	if w := do("POST", "/api/devices/add", `{"addr":"201::9","name":"Anna's phone"}`, "200::1"); w.Code != http.StatusNoContent {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/files", "", "201::9"); w.Code == http.StatusForbidden {
		t.Fatalf("files page from the device let in: %d", w.Code)
	}
	if w := do("POST", "/ask", "name=Anna's phone", "201::9"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/files" {
		t.Fatalf("asking again once let in: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/api/devices", "", "200::1"); strings.Contains(w.Body.String(), `"asks":[{`) {
		t.Fatalf("still asking once let in: %s", w.Body)
	}
}

func TestDevicePages(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "devices.json")
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Listen: "127.0.0.1:7480",
		YggAddr: "200::1", Devices: devices.NewWatch(list)})
	h := d.Handler()
	do := func(method, target, body, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host, r.RemoteAddr = "[200::1]:7480", net.JoinHostPort(remote, "50000")
		r.Header.Set("X-Yggstore", "1")
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("200::1"), Port: 7480}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// Let in from the dashboard: everything but Admin.
	if w := do("POST", "/api/devices/add", `{"addr":"201::9","name":"phone"}`, "200::1"); w.Code != http.StatusNoContent {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	for path, want := range map[string]int{"/": 200, "/mail": 200, "/storage": 200, "/admin": 403, "/network": 403,
		"/api/devices": 403, "/api/network": 403, "/api/box/people": 403} {
		if w := do("GET", path, "", "201::9"); w.Code != want {
			t.Errorf("%s from a new device: %d, want %d", path, w.Code, want)
		}
	}
	if w := do("POST", "/api/devices/pages", `{"addr":"201::9","pages":["admin","files","web","messages","mail"]}`, "201::9"); w.Code != http.StatusForbidden {
		t.Fatalf("a device giving itself Admin: %d", w.Code)
	}

	// Mail only.
	if w := do("POST", "/api/devices/pages", `{"addr":"201::9","pages":["mail"]}`, "200::1"); w.Code != http.StatusNoContent {
		t.Fatalf("set pages: %d %s", w.Code, w.Body)
	}
	for path, want := range map[string]int{"/mail": 200, "/storage": 403, "/files": 403, "/web": 403, "/messages": 403,
		"/api/msg": 403, "/api/names": 403, "/api/sites": 403} {
		if w := do("GET", path, "", "201::9"); w.Code != want {
			t.Errorf("%s from a mail-only device: %d, want %d", path, w.Code, want)
		}
	}
	if w := do("POST", "/api/delete?stub=x", "", "201::9"); w.Code != http.StatusForbidden {
		t.Errorf("delete from a mail-only device: %d", w.Code)
	}
	w := do("GET", "/api/state", "", "201::9")
	var st State
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || len(st.Pages) != 1 || st.Pages[0] != "mail" || st.Events == nil || len(st.Events) != 0 {
		t.Fatalf("state for a mail-only device: %v %+v", err, st)
	}

	// Admin, and taking it from the device in use.
	if w := do("POST", "/api/devices/pages", `{"addr":"201::9","pages":["admin"]}`, "200::1"); w.Code != http.StatusNoContent {
		t.Fatalf("set admin: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/api/devices", "", "201::9"); w.Code != 200 {
		t.Fatalf("devices from an admin device: %d", w.Code)
	}
	if w := do("POST", "/api/devices/pages", `{"addr":"201::9","pages":["mail"]}`, "201::9"); w.Code != http.StatusConflict {
		t.Fatalf("an admin device taking Admin from itself: %d", w.Code)
	}

	// From this machine, everything.
	if w := do("GET", "/admin", "", "200::1"); w.Code != 200 {
		t.Fatalf("admin from this machine: %d", w.Code)
	}
}

func TestPageOf(t *testing.T) {
	for path, want := range map[string]string{"/": "", "/api/state": "", "/ask": "", "/files": "files", "/api/restore-to": "files",
		"/api/mail/raw": "mail", "/api/msg/send": "messages", "/api/sites/publish": "web", "/api/box/people/add": "admin",
		"/api/something-new": "admin", "/mailbox": "", "/api/mailer": "admin"} {
		if got := pageOf(path); got != want {
			t.Errorf("pageOf(%s) = %q, want %q", path, got, want)
		}
	}
}

func TestYggNameHost(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "devices.json")
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Listen: "127.0.0.1:7480",
		YggAddr: "200::1", Devices: devices.NewWatch(list)})
	d.yggName = "desktop.ygg.example.org"
	d.names.Lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("200::1")}, nil }
	devices.Add(list, "201::9", "laptop")
	h := d.Handler()
	do := func(target, host, local, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r.Host, r.RemoteAddr = host, net.JoinHostPort(remote, "50000")
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(local), Port: 7480}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for host, want := range map[string]int{"desktop.ygg.example.org:7480": 200, "Desktop.YGG.example.org.:7480": 200,
		"other.ygg.example.org:7480": 403, "evil.example:7480": 403} {
		if w := do("/", host, "200::1", "201::9"); w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
	// From the LAN, sent on by name once DNS has it.
	if w := do("/mail", "box.local:7480", "192.168.0.5", "192.168.0.20"); w.Header().Get("Location") != "http://desktop.ygg.example.org:7480/mail" {
		t.Fatalf("sent to %q", w.Header().Get("Location"))
	}
}
