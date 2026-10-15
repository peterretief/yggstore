package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/syncthing"
	"github.com/peterretief/yggstore/internal/workhere"
)

const (
	boxYgg   = "200:aaaa::1"
	annaYgg  = "201:a::1"
	annaYgg2 = "201:a::2"
	benYgg   = "202:b::1"
)

func filesReq(h http.Handler, method, target, from, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr, req.Host = "["+from+"]:51000", "["+boxYgg+"]:7490"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func listOf(t *testing.T, h http.Handler, from string) workhere.Listing {
	t.Helper()
	rec := filesReq(h, "POST", "/files?op=list", from, `{"path":""}`, map[string]string{"X-Yggstore": "1"})
	if rec.Code != 200 {
		t.Fatalf("list from %s: %d %s", from, rec.Code, rec.Body)
	}
	var l workhere.Listing
	json.Unmarshal(rec.Body.Bytes(), &l)
	return l
}

func names(l workhere.Listing) string {
	var out []string
	for _, e := range l.Entries {
		out = append(out, e.Name)
	}
	return strings.Join(out, " ")
}

// Two people on one box: each device opens its own person's folder, and
// nobody else's.
func TestFilesPagePerPerson(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	fake.pending["LAPTOPP-1"] = syncthing.PendingDevice{Name: "anna", Address: "192.168.1.5:22000"}
	fake.pending["PHONEEE-2"] = syncthing.PendingDevice{Name: "ben", Address: "192.168.1.6:22000"}
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(statePath)
	anna, _ := st.PersonNamed("anna")
	ben, _ := st.PersonNamed("ben")
	os.MkdirAll(filepath.Join(anna.Path, "files"), 0o755)
	os.MkdirAll(filepath.Join(ben.Path, "files"), 0o755)
	os.WriteFile(filepath.Join(anna.Path, "files", "anna.txt"), []byte("hers"), 0o644)
	os.WriteFile(filepath.Join(ben.Path, "files", "ben.txt"), []byte("his"), 0o644)
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()

	// Not let in yet: told how, nothing served.
	rec := filesReq(h, "POST", "/files?op=list", annaYgg, `{}`, map[string]string{"X-Yggstore": "1"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), annaYgg) {
		t.Fatalf("a device not let in: %d %s", rec.Code, rec.Body)
	}

	// The admin lets in Anna's laptop and Ben's phone.
	if _, err := Update(ctx, statePath, func(s *HubState) error {
		if _, err := s.LetIn("anna", annaYgg, "anna's laptop"); err != nil {
			return err
		}
		_, err := s.LetIn("ben", benYgg, "ben's phone")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := names(listOf(t, h, annaYgg)); got != "anna.txt" {
		t.Fatalf("Anna sees %q", got)
	}
	if got := names(listOf(t, h, benYgg)); got != "ben.txt" {
		t.Fatalf("Ben sees %q", got)
	}
	// Ben can't fetch Anna's file by its path either.
	rec = filesReq(h, "GET", "/files?op=get&path=anna.txt", benYgg, "", nil)
	if rec.Code == 200 && strings.Contains(rec.Body.String(), "hers") {
		t.Fatal("Ben downloaded Anna's file")
	}
	// A device can't be two people's.
	if _, err := Update(ctx, statePath, func(s *HubState) error { _, err := s.LetIn("ben", annaYgg, ""); return err }); err == nil {
		t.Fatal("Anna's laptop let in to Ben's files too")
	}

	// Changes need the header other web pages can't add.
	rec = filesReq(h, "POST", "/files?op=change", annaYgg, `{"op":"folder","path":"New"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a change with no header: %d", rec.Code)
	}
	// The page links to the person's devices.
	if rec := filesReq(h, "GET", "/files", annaYgg, "", nil); !strings.Contains(rec.Body.String(), `data-devices="/files/devices"`) {
		t.Fatal("the files page doesn't link Your devices")
	}
}

// A new device asks; a device of the same person already let in allows it.
func TestFilesDevicesAskAndAllow(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	fake.pending["LAPTOPP-1"] = syncthing.PendingDevice{Name: "anna", Address: "192.168.1.5:22000"}
	u.step(ctx)
	Update(ctx, statePath, func(s *HubState) error { _, err := s.LetIn("anna", annaYgg, "laptop"); return err })
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()

	// The phone asks, with a plain form (no header needed to ask).
	form := url.Values{"name": {"anna's phone"}, "who": {"Anna"}}.Encode()
	rec := filesReq(h, "POST", "/files/ask", annaYgg2, form, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Asked") {
		t.Fatalf("ask: %d %s", rec.Code, rec.Body)
	}
	rec = filesReq(h, "GET", "/files/devices?op=list", annaYgg, "", nil)
	var lst struct {
		Person  string
		Devices []struct{ Addr, Name string }
		Asks    []DeviceAsk
	}
	json.Unmarshal(rec.Body.Bytes(), &lst)
	if lst.Person != "anna" || len(lst.Devices) != 1 || len(lst.Asks) != 1 || lst.Asks[0].Name != "anna's phone" || lst.Asks[0].Who != "Anna" {
		t.Fatalf("Anna's devices: %s", rec.Body)
	}
	// Allowing needs the header.
	if rec := filesReq(h, "POST", "/files/devices?op=allow", annaYgg, `{"addr":"`+annaYgg2+`"}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("allow with no header: %d", rec.Code)
	}
	hdr := map[string]string{"X-Yggstore": "1"}
	if rec := filesReq(h, "POST", "/files/devices?op=allow", annaYgg, `{"addr":"`+annaYgg2+`"}`, hdr); rec.Code != http.StatusNoContent {
		t.Fatalf("allow: %d %s", rec.Code, rec.Body)
	}
	if got := filesReq(h, "POST", "/files?op=list", annaYgg2, `{}`, hdr); got.Code != 200 {
		t.Fatalf("the phone, let in: %d %s", got.Code, got.Body)
	}
	st, _ := Load(statePath)
	if p, _ := st.PersonAt(annaYgg2); p.Name != "anna" || len(st.Asks) != 0 {
		t.Fatalf("after allowing: %+v asks %+v", p, st.Asks)
	}
	if p, _ := st.PersonNamed("anna"); p.FilesDevices[1].Name != "anna's phone" {
		t.Fatalf("the phone's name: %+v", p.FilesDevices)
	}
	// Asking again once let in goes on to the files.
	if rec := filesReq(h, "POST", "/files/ask", annaYgg2, form, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("asking again: %d", rec.Code)
	}
	// The device in use can't remove itself; the other can.
	if rec := filesReq(h, "POST", "/files/devices?op=remove", annaYgg2, `{"addr":"`+annaYgg2+`"}`, hdr); rec.Code != http.StatusBadRequest {
		t.Fatalf("removing itself: %d", rec.Code)
	}
	if rec := filesReq(h, "POST", "/files/devices?op=remove", annaYgg, `{"addr":"`+annaYgg2+`"}`, hdr); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if rec := filesReq(h, "POST", "/files?op=list", annaYgg2, `{}`, hdr); rec.Code != http.StatusForbidden {
		t.Fatalf("the phone, removed: %d", rec.Code)
	}
}

// Over the LAN the page sends the browser on to the box's Yggdrasil
// address, and serves nothing; a request naming another host is refused.
func TestFilesPageOnlyOverYggdrasil(t *testing.T) {
	u, _, _ := setupUsers(t, 0)
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()
	rec := get(h, "/files", "192.168.1.5:5000", "box.local:7490")
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "http://["+boxYgg+"]:") {
		t.Fatalf("from the LAN: %d %v", rec.Code, rec.Header())
	}
	req := httptest.NewRequest("POST", "/files?op=list", nil)
	req.RemoteAddr, req.Host = "192.168.1.5:5000", "box.local:7490"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a POST from the LAN: %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/files", nil)
	req.RemoteAddr, req.Host = "["+annaYgg+"]:5000", "evil.example:7490"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("another host's name: %d", rec.Code)
	}
}

// Asks are kept for a day, ten at most.
func TestAsksLimited(t *testing.T) {
	var s HubState
	now := time.Now()
	for i := 0; i < maxAsks; i++ {
		if !s.Ask("201::"+string(rune('a'+i)), "d", "", now) {
			t.Fatalf("ask %d refused", i)
		}
	}
	if s.Ask("202::1", "one too many", "", now) {
		t.Fatal("an eleventh ask kept")
	}
	if !s.Ask("202::1", "a day later", "", now.Add(25*time.Hour)) || len(s.Asks) != 1 {
		t.Fatalf("after a day: %+v", s.Asks)
	}
}
