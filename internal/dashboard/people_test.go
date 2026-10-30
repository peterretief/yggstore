package dashboard

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/hub"
)

// On a box, its admin adds someone and gets their invite link, sets their
// limits, and suspends them; on another machine there's no such section.
func TestPeopleOnTheBox(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "hub.json")
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, Listen: "127.0.0.1:7480",
		SelfID: "200::7", HubState: state, HubPeople: filepath.Join(dir, "users")})
	h := d.Handler()
	do := func(method, target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = "127.0.0.1:7480"
		r.Header.Set("X-Yggstore", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var out struct {
		On     bool
		People []boxPerson
	}
	json.Unmarshal(do("GET", "/api/box/people", "").Body.Bytes(), &out)
	if out.On {
		t.Fatal("a machine with no hub shows the box's people")
	}
	if w := do("POST", "/api/box/people/add", `{"name":"anna"}`); w.Code != http.StatusConflict {
		t.Fatalf("adding someone with no hub: %d", w.Code)
	}
	os.WriteFile(state, []byte("{}"), 0o600)

	w := do("POST", "/api/box/people/add", `{"name":"anna"}`)
	var added struct{ Password, Login string }
	json.Unmarshal(w.Body.Bytes(), &added)
	if w.Code != 200 || strings.Count(added.Password, "-") != 3 || added.Login == "" {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	w = do("POST", "/api/box/people/invite", `{"name":"anna"}`)
	var inv struct {
		YggLink string `json:"ygg_link"`
	}
	json.Unmarshal(w.Body.Bytes(), &inv)
	if w.Code != 200 || !strings.HasPrefix(inv.YggLink, "http://[200::7]:7490/files/join?t=") {
		t.Fatalf("invite: %d %s", w.Code, w.Body)
	}
	do("POST", "/api/box/people/invite-cancel", `{"name":"anna"}`)
	if w := do("POST", "/api/box/people/add", `{"name":"Anna"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("adding Anna twice: %d", w.Code)
	}
	if w := do("POST", "/api/box/people/limit", `{"name":"anna","quota":1073741824,"max_devices":2}`); w.Code != 200 {
		t.Fatalf("limit: %d %s", w.Code, w.Body)
	}
	// A password chosen on the box's own screen; never from elsewhere.
	if w := do("POST", "/api/box/people/set-password", `{"name":"anna","password":"short"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a short chosen password: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/api/box/people/set-password", strings.NewReader(`{"name":"anna","password":"tea at four on fridays"}`))
	r.Host, r.RemoteAddr = "[200::7]:7480", "[200::9]:5555"
	r.Header.Set("X-Yggstore", "1")
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("200::7"), Port: 7480}))
	r.SetPathValue("action", "set-password")
	rw := httptest.NewRecorder()
	if d.handlePeopleAction(rw, r); rw.Code != http.StatusForbidden {
		t.Fatalf("a chosen password from another device: %d %s", rw.Code, rw.Body)
	}
	if w := do("POST", "/api/box/people/set-password", `{"name":"anna","password":"tea at four on fridays"}`); w.Code != 200 {
		t.Fatalf("set-password: %d %s", w.Code, w.Body)
	}
	if st, _ := hub.Load(state); !st.People[0].Password.Matches("tea at four on fridays") || st.People[0].Password.Temp {
		t.Fatal("the chosen password isn't kept")
	}
	if w := do("POST", "/api/box/people/suspend", `{"name":"anna"}`); w.Code != 200 {
		t.Fatalf("suspend: %d %s", w.Code, w.Body)
	}
	json.Unmarshal(do("GET", "/api/box/people", "").Body.Bytes(), &out)
	if !out.On || len(out.People) != 1 {
		t.Fatalf("people: %+v", out)
	}
	if p := out.People[0]; p.Quota != 1<<30 || p.MaxDevices != 2 || !p.Suspended || p.InviteUntil != 0 || p.Password != "own" {
		t.Fatalf("Anna: %+v", p)
	}
	st, _ := hub.Load(state)
	if _, err := os.Stat(filepath.Join(st.People[0].Path, "files")); err != nil {
		t.Fatal("no folder made for Anna")
	}
}
