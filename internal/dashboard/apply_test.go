package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/site"
)

func TestApplications(t *testing.T) {
	ctx, admin, member, do, _, listOf := namePair(t, nil)
	admin.cfg.InvitesPath = filepath.Join(t.TempDir(), "invites.json")
	admin.cfg.Client = client.Client{HTTP: &http.Client{Timeout: time.Second}} // no nodes answer here
	member.cfg.Client = admin.cfg.Client
	admin.state.AdminEntry = `{"name":"desktop","addr":"[200::1]:7400","admin":true}`

	body, _ := json.Marshal(site.Application{Name: "Anna", Email: "anna@example.net", Note: "hi", Site: "get.example.org", At: 1})
	admin.namesMu.Lock()
	admin.takeApplication(ctx, listOf(admin), string(body))
	admin.takeApplication(ctx, listOf(admin), string(body)) // the same again
	admin.namesMu.Unlock()
	member.namesMu.Lock()
	member.takeApplication(ctx, listOf(member), string(body)) // not an admin
	member.namesMu.Unlock()

	get := func(d *Dashboard) (int, []application) {
		r := httptest.NewRequest(http.MethodGet, "/api/applications", nil)
		r.Host = "127.0.0.1:7480"
		w := do(d, r)
		var out []application
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, apps := get(admin)
	if code != http.StatusOK || len(apps) != 1 || apps[0].Name != "Anna" || apps[0].Status != "waiting" {
		t.Fatalf("admin sees %d %+v", code, apps)
	}
	if code, _ := get(member); code != http.StatusForbidden {
		t.Fatalf("member sees applications: %d", code)
	}
	decide := `{"id":"` + apps[0].ID + `","approve":true}`
	if w := do(member, post("/api/applications/decide", strings.NewReader(decide))); w.Code != http.StatusForbidden {
		t.Fatalf("member decided: %d", w.Code)
	}
	// No mail is set up here, so it's approved with a warning.
	w := do(admin, post("/api/applications/decide", strings.NewReader(decide)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "warning") {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	if _, apps = get(admin); apps[0].Status != "approved" || apps[0].Error == "" || apps[0].Decided == 0 {
		t.Fatalf("after approving: %+v", apps[0])
	}
	if w := do(admin, post("/api/applications/decide", strings.NewReader(decide))); w.Code != http.StatusNotFound {
		t.Fatalf("decided twice: %d", w.Code)
	}
	// A new application from the same address waits again.
	admin.namesMu.Lock()
	admin.takeApplication(ctx, listOf(admin), string(body))
	admin.namesMu.Unlock()
	if _, apps = get(admin); len(apps) != 2 || apps[0].Status != "waiting" {
		t.Fatalf("a second application: %+v", apps)
	}
}

func TestTrialNoteTellsTheAdmin(t *testing.T) {
	ctx, admin, member, _, _, listOf := namePair(t, nil)
	admin.cfg.Client = client.Client{HTTP: &http.Client{Timeout: time.Second}}
	member.cfg.Client = admin.cfg.Client
	body := `{"person":"anna","name":"anna.example.org","email":"anna@example.net","box":"pi"}`
	admin.takeTrialNote(ctx, listOf(admin), body)
	member.takeTrialNote(ctx, listOf(member), body)
	has := func(d *Dashboard) bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, e := range d.events {
			if strings.Contains(e.Msg, "anna made an account on pi") {
				return true
			}
		}
		return false
	}
	if !has(admin) || has(member) {
		t.Fatalf("admin told %v, member told %v", has(admin), has(member))
	}
}
