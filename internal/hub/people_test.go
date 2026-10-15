package hub

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The admin adds Anna and sends her an invite link; she opens it on her
// laptop, from anywhere, and her files open there. The link works once.
func TestInviteLetsTheFirstDeviceIn(t *testing.T) {
	u, _, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	var token string
	if _, err := Update(ctx, statePath, func(s *HubState) error {
		p, err := s.AddPerson("anna", u.cfg.Dir)
		if err != nil {
			return err
		}
		token, err = s.Invite(p.Name, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()

	// Opening the link only offers; a link preview doesn't use it up.
	rec := filesReq(h, "GET", JoinPath+"?t="+token, annaYgg, "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Let this device in") {
		t.Fatalf("the invite page: %d %s", rec.Code, rec.Body)
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	rec = filesReq(h, "POST", JoinPath, annaYgg, url.Values{"t": {token}, "name": {"laptop"}}.Encode(), form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("using the invite: %d %s", rec.Code, rec.Body)
	}
	if got := listOf(t, h, annaYgg); got.Path != "" {
		t.Fatalf("Anna's files: %+v", got)
	}
	// Once only.
	rec = filesReq(h, "POST", JoinPath, annaYgg2, url.Values{"t": {token}}.Encode(), form)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("the invite used twice: %d", rec.Code)
	}
	if st, _ := Load(statePath); len(st.Invites) != 0 {
		t.Fatalf("invites left: %+v", st.Invites)
	}
	// An expired one doesn't work either.
	Update(ctx, statePath, func(s *HubState) error {
		token, _ = s.Invite("anna", time.Now().Add(-InviteFor-time.Hour))
		return nil
	})
	if rec := filesReq(h, "POST", JoinPath, annaYgg2, url.Values{"t": {token}}.Encode(), form); rec.Code != http.StatusForbidden {
		t.Fatalf("an expired invite: %d", rec.Code)
	}
}

// Limits: how many devices, and how much stored; suspending shuts the
// person out, and removing them for good, their files staying.
func TestLimitsSuspendRemove(t *testing.T) {
	u, _, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	var anna Person
	Update(ctx, statePath, func(s *HubState) error {
		anna, _ = s.AddPerson("anna", u.cfg.Dir)
		s.LetIn("anna", annaYgg, "laptop")
		return s.Limit("anna", 10, 1)
	})
	if _, err := Update(ctx, statePath, func(s *HubState) error { _, err := s.LetIn("anna", annaYgg2, "phone"); return err }); err == nil {
		t.Fatal("a second device let in, with a limit of one")
	}
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()
	put := func(body string) int {
		return filesReq(h, "POST", "/files?op=put", annaYgg, body, map[string]string{"X-Yggstore": "1", "X-Name": "a.txt", "X-Dir": ""}).Code
	}
	if code := put("12345"); code != 200 {
		t.Fatalf("a file within the quota: %d", code)
	}
	if code := put("123456"); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a file over the quota: %d", code)
	}
	if b, n := Usage(anna); b != 5 || n != 1 {
		t.Fatalf("usage %d bytes, %d files", b, n)
	}

	Update(ctx, statePath, func(s *HubState) error { return s.Suspend("anna", true) })
	if rec := filesReq(h, "POST", "/files?op=list", annaYgg, `{}`, map[string]string{"X-Yggstore": "1"}); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "closed for now") {
		t.Fatalf("suspended: %d %s", rec.Code, rec.Body)
	}
	if _, err := Update(ctx, statePath, func(s *HubState) error { _, err := s.Invite("anna", time.Now()); return err }); err == nil {
		t.Fatal("an invite for someone suspended")
	}
	Update(ctx, statePath, func(s *HubState) error { return s.Suspend("anna", false) })
	if code := filesReq(h, "POST", "/files?op=list", annaYgg, `{}`, map[string]string{"X-Yggstore": "1"}).Code; code != 200 {
		t.Fatalf("let back in: %d", code)
	}

	Update(ctx, statePath, func(s *HubState) error { return s.RemovePerson("anna") })
	if code := filesReq(h, "POST", "/files?op=list", annaYgg, `{}`, map[string]string{"X-Yggstore": "1"}).Code; code != http.StatusForbidden {
		t.Fatalf("removed: %d", code)
	}
	if _, err := os.Stat(filepath.Join(anna.Path, "files", "a.txt")); err != nil {
		t.Fatal("her files went with her")
	}
	// Added again, she's back with her files.
	if _, err := Update(ctx, statePath, func(s *HubState) error { _, err := s.AddPerson("anna", u.cfg.Dir); return err }); err != nil {
		t.Fatal(err)
	}
	if st, _ := Load(statePath); len(st.People) != 1 || !st.People[0].Active() {
		t.Fatalf("back: %+v", st.People)
	}
}

// Someone who only opens their files in the browser has them stored, until
// they're suspended.
func TestBrowserOnlyPeopleAreStored(t *testing.T) {
	u, _, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	var mu sync.Mutex
	watching := map[string]bool{}
	u.cfg.Watch = func(ctx context.Context, p Person) {
		mu.Lock()
		watching[p.Name] = true
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		watching[p.Name] = false
		mu.Unlock()
	}
	is := func() bool {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return watching["anna"]
	}
	Update(ctx, statePath, func(s *HubState) error { s.AddPerson("anna", u.cfg.Dir); return nil })
	u.step(ctx)
	if is() {
		t.Fatal("stored with no device at all")
	}
	Update(ctx, statePath, func(s *HubState) error { _, err := s.LetIn("anna", annaYgg, "laptop"); return err })
	u.step(ctx)
	if !is() {
		t.Fatal("a browser-only person's folder isn't stored")
	}
	Update(ctx, statePath, func(s *HubState) error { return s.Suspend("anna", true) })
	u.step(ctx)
	if is() {
		t.Fatal("still stored while suspended")
	}
}
