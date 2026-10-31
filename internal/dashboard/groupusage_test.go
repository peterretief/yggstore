package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
)

func TestGroupUsage(t *testing.T) {
	_, admin, member, do, _, listOf := namePair(t, nil)
	list := listOf(admin)
	list[1].PeopleNames = []peers.PersonName{{Name: "anna.example.org", Person: "anna", Code: "ys1anna"}}
	for _, d := range []*Dashboard{admin, member} {
		peers.Write(d.cfg.PeersPath, list)
	}
	admin.mu.Lock()
	admin.state.Peers = []PeerState{
		{Name: "desktop", Addr: "[200::1]:7400", Up: true, Info: &server.Info{UsedBytes: 95, QuotaBytes: 100}},
		{Name: "pi", Addr: "[200::2]:7400", Up: true, Info: &server.Info{UsedBytes: 10, QuotaBytes: 100}},
		{Name: "pi-usb1", Addr: "[200::2]:7401", Up: true, Info: &server.Info{UsedBytes: 10, QuotaBytes: 100}},
	}
	admin.mu.Unlock()
	admin.takeUsage(list, "200::2", "pi", `{"box":"pi","at":1,"people":[{"name":"anna","bytes":50,"quota":100},{"name":"ben","bytes":120,"quota":100}]}`)

	get := func(d *Dashboard) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
		r.Host = "127.0.0.1:7480"
		return do(d, r)
	}
	if w := get(member); w.Code != http.StatusForbidden {
		t.Fatalf("a member's dashboard showed the group's usage: %d", w.Code)
	}
	var u usageState
	json.Unmarshal(get(admin).Body.Bytes(), &u)
	machines := map[string]machineUsage{}
	for _, m := range u.Machines {
		machines[m.Machine] = m
	}
	if m := machines["desktop"]; m.Status != "near" {
		t.Errorf("desktop at 95%%: %+v", m)
	}
	if m := machines["pi"]; m.Used != 20 || m.Quota != 200 || m.Status != "ok" {
		t.Errorf("pi's two nodes add up: %+v", m)
	}
	if len(u.People) != 2 || u.People[0].Name != "ben" || u.People[0].Status != "over" || u.People[1].Status != "ok" {
		t.Errorf("people, over first: %+v", u.People)
	}
	if len(u.Names) != 1 || u.Names[0].Email != "anna@example.org" || u.Names[0].Who != "anna on pi" {
		t.Errorf("names: %+v", u.Names)
	}
}

// A box sends its people's usage to the admin over messaging.
func TestBoxSendsUsage(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "hub.json")
	os.WriteFile(state, []byte(`{"people":[{"name":"anna","quota":1000}]}`), 0o600)
	ctx, admin, member, _, _, listOf := namePair(t, func(c *Config) { c.HubState, c.HubPeople = state, filepath.Join(dir, "users") })
	member.sendUsage(ctx, listOf(member))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		admin.syncNames(ctx, listOf(admin))
		u := admin.usageReports()
		u.mu.Lock()
		b, ok := u.by["200::2"]
		u.mu.Unlock()
		if ok {
			if b.Box != "pi" || len(b.People) != 1 || b.People[0].Name != "anna" || b.People[0].Quota != 1000 {
				t.Fatalf("report: %+v", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the box's usage never reached the admin")
		}
	}
	// Not again within the hour.
	before := member.usageReports().last
	member.sendUsage(ctx, listOf(member))
	if member.usageReports().last != before {
		t.Fatal("sent twice in an hour")
	}
}
