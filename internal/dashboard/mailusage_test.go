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
)

func writeMail(t *testing.T, dir, id string, meta string) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, id+".sealed"), []byte("0123456789"), 0o600)
	if meta != "" {
		os.WriteFile(filepath.Join(dir, id+".json"), []byte(meta), 0o600)
	}
}

func TestCountMail(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	writeMail(t, dir, "a1", `{"received":`+itoa(ms(time.Hour))+`}`)                           // new, unread
	writeMail(t, dir, "a2", `{"received":`+itoa(ms(time.Hour))+`,"flags":["\\Seen"]}`)        // read
	writeMail(t, dir, "a3", `{"received":`+itoa(ms(30*24*time.Hour))+`,"folder":"Archives"}`) // old, filed
	writeMail(t, dir, "a4", `{"received":`+itoa(ms(2*time.Hour))+`,"sent":true}`)             // sent
	os.WriteFile(filepath.Join(dir, "folders.json"), []byte(`{}`), 0o600)
	m, ok := countMail(dir, now)
	if !ok || m.Total != 4 || m.Inbox != 2 || m.Unread != 1 || m.InWeek != 2 || m.OutWeek != 1 || m.Bytes != 40 ||
		m.LastIn != ms(time.Hour) || m.LastOut != ms(2*time.Hour) {
		t.Fatalf("%+v", m)
	}
	if _, ok := countMail(filepath.Join(dir, "none"), now); ok {
		t.Fatal("no mailbox counted as one")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A box's people's mail counts reach the admin's email page, with their
// addresses; a member's dashboard doesn't show it.
func TestGroupMail(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "hub.json")
	os.WriteFile(state, []byte(`{"people":[{"name":"anna","folder":"p-anna"},{"name":"ben","folder":"p-ben"}]}`), 0o600)
	writeMail(t, filepath.Join(dir, "people", "p-anna", "mail"), "m1", `{"received":`+itoa(time.Now().UnixMilli())+`}`)
	ctx, admin, member, do, _, listOf := namePair(t, func(c *Config) { c.HubState, c.HubPeople = state, filepath.Join(dir, "users") })
	list := listOf(admin)
	list[1].PeopleNames = []peers.PersonName{{Name: "anna.example.org", Person: "anna", Code: "ys1anna"}}
	for _, d := range []*Dashboard{admin, member} {
		peers.Write(d.cfg.PeersPath, list)
	}
	member.sendUsage(ctx, listOf(member))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		admin.syncNames(ctx, listOf(admin))
		u := admin.usageReports()
		u.mu.Lock()
		_, ok := u.by["200::2"]
		u.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the box's mail counts never reached the admin")
		}
	}
	get := func(d *Dashboard) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/group-mail", nil)
		r.Host = "127.0.0.1:7480"
		return do(d, r)
	}
	if w := get(member); w.Code != http.StatusForbidden {
		t.Fatalf("a member's dashboard showed the group's email: %d", w.Code)
	}
	var g groupMailState
	json.Unmarshal(get(admin).Body.Bytes(), &g)
	who := map[string]mailboxShown{}
	for _, m := range g.Mailboxes {
		who[m.Person] = m
	}
	anna, ben := who["anna"], who["ben"]
	if anna.Machine != "pi" || anna.Unread != 1 || anna.InWeek != 1 || len(anna.Addresses) == 0 || anna.Addresses[0] != "anna@example.org" {
		t.Errorf("anna: %+v", anna)
	}
	if ben.Person != "ben" || ben.Total != 0 {
		t.Errorf("ben, with no mail yet, is still shown: %+v", ben)
	}
	var pi *mailMachine
	for i := range g.Machines {
		if g.Machines[i].Machine == "pi" {
			pi = &g.Machines[i]
		}
	}
	if pi == nil || !pi.Reports || pi.InWeek != 1 {
		t.Errorf("machines: %+v", g.Machines)
	}
}
