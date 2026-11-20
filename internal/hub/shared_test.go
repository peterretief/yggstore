package hub

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestEnsureShared(t *testing.T) {
	var s HubState
	s.AddPerson("Sales", t.TempDir()) // a person has the name already
	added, taken := s.EnsureShared(SharedNames(DefaultShared), "users", time.Now())
	if strings.Join(added, ",") != "support,accounts" || strings.Join(taken, ",") != "sales" {
		t.Fatalf("added %v, taken %v", added, taken)
	}
	if added, taken = s.EnsureShared(SharedNames(DefaultShared), "users", time.Now()); added != nil || len(taken) != 1 {
		t.Fatalf("again: added %v, taken %v", added, taken)
	}
	if n := len(s.SharedPeople()); n != 2 {
		t.Fatalf("%d shared", n)
	}
	// Removed by the box's admin: it stays off.
	s.People[s.person("support")].Removed = true
	if added, _ = s.EnsureShared([]string{"support"}, "users", time.Now()); added != nil || len(s.SharedPeople()) != 1 {
		t.Fatal("a removed shared mailbox came back")
	}
	if got := SharedNames(" Support, ,sa/les,accounts "); strings.Join(got, ",") != "support,accounts" {
		t.Fatalf("names: %v", got)
	}
}

// Anyone on the box's network opens a shared mailbox from the info page,
// with no password; nobody does from anywhere else, or through a proxy,
// and a person's own mailbox isn't opened that way.
func TestSharedMailboxes(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	Update(context.Background(), statePath, func(s *HubState) error {
		s.AddPerson("Kath", filepath.Join(dir, "users"))
		s.EnsureShared(SharedNames(DefaultShared), filepath.Join(dir, "users"), time.Now())
		return nil
	})
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, []peers.Peer{{Name: "admin", Addr: "[200::1]:7400", Admin: true, Domain: "group.example"}, {Name: "localbox-004", Addr: "[200::2]:7400"}})
	ps := NewPeopleSites(PeopleSitesConfig{StatePath: statePath, PeersPath: peersPath, Self: "200::2", Dir: filepath.Join(dir, "people"), Logf: t.Logf})
	u := &Users{cfg: UsersConfig{StatePath: statePath, PeersPath: peersPath, People: ps}}
	f := &filesPage{u: u, self: "200::2"}
	do := func(path, remote string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr, r.Host = remote, "mail.local"
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		if !f.ServeWebmail(w, r) {
			t.Fatalf("%s wasn't webmail's", path)
		}
		return w
	}
	const lan = "192.168.1.20:5555"
	w := do("/info", lan)
	for _, want := range []string{"Mail on localbox-004", "support@local.4", "/webmail/shared/sales", "/webmail/shared/accounts", "Open your own mail"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("info page lacks %q: %s", want, w.Body)
		}
	}
	if strings.Contains(w.Body.String(), "kath") {
		t.Fatal("the info page lists a person's own mailbox")
	}
	if w := do("/webmail/shared/support", lan); w.Code != 200 || !strings.Contains(w.Body.String(), "<title>Your mail</title>") {
		t.Fatalf("support: %d", w.Code)
	}
	w = do("/webmail/shared/support?op=status", lan)
	var st sitePageState
	if json.Unmarshal(w.Body.Bytes(), &st); st.Person != "support" || st.Address != "support@local.4" {
		t.Fatalf("status: %s", w.Body)
	}
	for _, c := range []struct {
		path, remote string
		hdr          []string
		code         int
	}{
		{"/webmail/shared/support", "41.1.2.3:5555", nil, 403},                                      // the internet
		{"/info", "41.1.2.3:5555", nil, 403},                                                        //
		{"/webmail/shared/support", "127.0.0.1:5555", []string{"X-Forwarded-For", "41.1.2.3"}, 403}, // a proxy on the box
		{"/webmail/shared/support", lan, []string{"Cf-Connecting-Ip", "41.1.2.3"}, 403},             // a tunnel
		{"/webmail/shared/kath", lan, nil, 404},                                                     // a person's own
		{"/webmail/shared/nobody", lan, nil, 404},
		{"/webmail/shared/support?op=pushkey", lan, nil, 403},
	} {
		if w := do(c.path, c.remote, c.hdr...); w.Code != c.code {
			t.Errorf("%s from %s %v: %d, not %d", c.path, c.remote, c.hdr, w.Code, c.code)
		}
	}
	// A shared mailbox has no password to sign in with.
	if st, _ := Load(statePath); st.People[1].Password != nil {
		t.Fatal("a shared mailbox has a password")
	}
}
