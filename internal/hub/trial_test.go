package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/peers"
)

// trialGroup is a trial box and a web node in front of it that "sends"
// email into sent.
type trialGroup struct {
	*lightGroup
	mu   sync.Mutex
	sent []string // To, then the text
}

func newTrialGroup(t *testing.T) *trialGroup {
	u, _, dir := setupUsers(t, 0)
	g := &trialGroup{lightGroup: &lightGroup{u: u, statePath: filepath.Join(dir, "hub.json")}}
	list := []peers.Peer{
		{Name: "desktop", Addr: "[" + webYgg + "]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[" + boxYgg + "]:7400", Trial: true},
	}
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, list)
	u.cfg.PeersPath, u.cfg.SelfAddr = peersPath, "["+boxYgg+"]:7400"
	u.cfg.WebNodes = func(context.Context) []string { return []string{"[" + webYgg + "]:7400"} }
	h := (&Hub{cfg: Config{Files: u.FilesHandler(boxYgg)}}).Handler()
	g.box = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr, r.Host = "["+webYgg+"]:51000", "["+boxYgg+"]:7490"
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.box.Close)
	g.store = &light.Store{Dir: filepath.Join(dir, "web-light"), Peers: func() []peers.Peer { return list }, Log: t.Logf}
	email := func(_ context.Context, to, subject, text string) error {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.sent = append(g.sent, to, text)
		return nil
	}
	g.store.Email = email
	relay := httptest.NewServer(&light.Relay{Peers: func() []peers.Peer { return list }, Signin: g.store,
		BoxURL: func(string) string { return g.box.URL }, Log: t.Logf, Email: email})
	t.Cleanup(relay.Close)
	g.relay = relay.URL
	return g
}

var linkToken = regexp.MustCompile(`https://my\.example\.org/signup#([0-9a-f]+)`)

func (g *trialGroup) lastLink(t *testing.T) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) == 0 {
		t.Fatal("no email was sent")
	}
	m := linkToken.FindStringSubmatch(g.sent[len(g.sent)-1])
	if m == nil {
		t.Fatalf("no link in %q", g.sent[len(g.sent)-1])
	}
	return m[1]
}

func TestTrialSignUpShareAndLeave(t *testing.T) {
	g := newTrialGroup(t)
	b := g.browser(t, "")
	if r := b.do("GET", light.SignupPath, ""); r.StatusCode != 200 || !strings.Contains(text(r), "Make my account") {
		t.Fatalf("the sign-up page: %d", r.StatusCode)
	}
	for _, c := range []struct{ body, want string }{
		{`{"label":"www","email":"a@example.net","password":"long enough pw"}`, "kept for the group"},
		{`{"label":"anna","email":"anna@example.org","password":"long enough pw"}`, "outside example.org"},
		{`{"label":"anna","email":"not an address","password":"long enough pw"}`, "isn't an email address"},
		{`{"label":"anna","email":"anna@example.net","password":"short"}`, "10"},
	} {
		if r := b.do("POST", light.Base+"/signup", c.body); r.StatusCode == 200 || !strings.Contains(text(r), c.want) {
			t.Errorf("%s: %d, want %q", c.body, r.StatusCode, c.want)
		}
	}
	if len(g.sent) != 0 {
		t.Fatalf("emailed a bad sign-up: %v", g.sent)
	}
	if r := b.do("POST", light.Base+"/signup", `{"label":"Anna","email":"anna@example.net","password":"long enough pw"}`); r.StatusCode != 200 {
		t.Fatalf("sign up: %d %s", r.StatusCode, text(r))
	}
	token := g.lastLink(t)
	if g.sent[0] != "anna@example.net" {
		t.Fatalf("emailed %q", g.sent[0])
	}
	// Again at once: told when it went, and nothing sent.
	if r := b.do("POST", light.Base+"/signup", `{"label":"anna","email":"anna@example.net","password":"long enough pw"}`); r.StatusCode != 409 || !strings.Contains(text(r), "went to") {
		t.Fatalf("again: %d", r.StatusCode)
	}
	// Someone else can't have the name while it waits.
	if r := g.browser(t, "").do("POST", light.Base+"/signup", `{"label":"anna","email":"bob@example.net","password":"long enough pw"}`); r.StatusCode != 409 || !strings.Contains(text(r), "taken") {
		t.Fatalf("the same name: %d", r.StatusCode)
	}

	// The link signs them in.
	if r := b.do("POST", light.Base+"/confirm", `{"t":"`+token+`"}`); r.StatusCode != 200 {
		t.Fatalf("confirm: %d %s", r.StatusCode, text(r))
	}
	if r := g.browser(t, "").do("POST", light.Base+"/confirm", `{"t":"`+token+`"}`); r.StatusCode != 409 {
		t.Fatalf("the link twice: %d", r.StatusCode)
	}
	st, _ := Load(g.statePath)
	p, ok := st.PersonNamed("anna")
	if !ok || p.Trial == nil || p.Quota != TrialQuota || len(st.Signups) != 0 || p.Trial.Email != "anna@example.net" {
		t.Fatalf("after confirming: %+v %+v", p, st.Signups)
	}
	if r := b.do("GET", "/files", ""); r.StatusCode != 200 || !strings.Contains(text(r), `data-leave="/files/leave"`) {
		t.Fatalf("their files page: %d", r.StatusCode)
	}

	// Signing in again with the address they signed up with.
	g.push(t)
	again := g.browser(t, "")
	if r := again.do("POST", light.Base+"/password", `{"login":"Anna@example.net","password":"long enough pw"}`); r.StatusCode != 200 {
		t.Fatalf("sign in by email: %d %s", r.StatusCode, text(r))
	}

	// Sharing a file by link.
	os.WriteFile(filepath.Join(p.Path, "files", "note.txt"), []byte("hello world"), 0o600)
	r := b.do("POST", "/files/share", `{"path":"note.txt"}`)
	var sh shareView
	json.NewDecoder(r.Body).Decode(&sh)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.HasPrefix(sh.URL, "https://my.example.org/s/box/") {
		t.Fatalf("share: %d %+v", r.StatusCode, sh)
	}
	if r := b.do("POST", "/files/share", `{"path":"nothere.txt"}`); r.StatusCode != 404 {
		t.Fatalf("shared a file that isn't there: %d", r.StatusCode)
	}
	anyone := g.browser(t, "")
	path := strings.TrimPrefix(sh.URL, "https://my.example.org")
	r = anyone.do("GET", path, "")
	if body := text(r); r.StatusCode != 200 || body != "hello world" || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") ||
		r.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("the link: %d %q %v", r.StatusCode, body, r.Header)
	}
	if r := anyone.do("GET", "/s/box/"+strings.Repeat("0", 36), ""); r.StatusCode != 404 {
		t.Fatalf("a made-up link: %d", r.StatusCode)
	}
	b.do("POST", "/files/share", `{"path":"note.txt","stop":true}`).Body.Close()
	if r := anyone.do("GET", path, ""); r.StatusCode != 404 {
		t.Fatalf("after stopping: %d", r.StatusCode)
	}

	// Leaving: nobody can sign in, and their stored files go to delete/.
	os.MkdirAll(filepath.Join(p.Path, "files", "sub"), 0o700)
	os.WriteFile(filepath.Join(p.Path, "files", "sub", "photo.jpg.ystub"), []byte("{}"), 0o600)
	if r := b.do("POST", "/files/leave", ""); r.StatusCode != 204 {
		t.Fatalf("leave: %d %s", r.StatusCode, text(r))
	}
	g.push(t)
	if r := b.do("GET", "/files?op=list", ""); r.StatusCode == 200 {
		t.Fatal("still signed in after leaving")
	}
	now := time.Now()
	g.u.trialStep(context.Background(), now)
	if _, err := os.Stat(filepath.Join(p.Path, "delete", "files_sub_photo.jpg.ystub")); err != nil {
		t.Fatalf("the stub isn't in delete/: %v", err)
	}
	if st, _ := Load(g.statePath); st.People[0].Removed {
		t.Fatal("removed before their files were deleted")
	}
	// The watcher deletes it from the group; then the person goes.
	os.Remove(filepath.Join(p.Path, "delete", "files_sub_photo.jpg.ystub"))
	g.u.trialStep(context.Background(), now)
	if st, _ := Load(g.statePath); !st.People[0].Removed || st.People[0].Trial.Gone == 0 {
		t.Fatalf("not removed: %+v", st.People[0])
	}
	g.u.trialStep(context.Background(), now.Add(2*time.Minute))
	st, _ = Load(g.statePath)
	if _, err := os.Stat(p.Path); err == nil || len(st.People) != 0 {
		t.Fatalf("their folder or entry is still there: %v %+v", err, st.People)
	}
}

func TestTrialUnusedIsWarnedThenClosed(t *testing.T) {
	g := newTrialGroup(t)
	var warned []light.TrialEmail
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e light.TrialEmail
		json.NewDecoder(r.Body).Decode(&e)
		warned = append(warned, e)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(web.Close)
	g.u.cfg.MailNodes = func(context.Context) []string { return []string{strings.TrimPrefix(web.URL, "http://")} }
	g.u.cfg.Client = client.New()
	start := time.Now()
	Update(context.Background(), g.statePath, func(s *HubState) error {
		su, err := s.SignUp([]peers.Peer{{Name: "desktop", Admin: true, Domain: "example.org"}},
			SignupRequest{Label: "cara", Email: "cara@example.net", Password: "long enough pw"}, start)
		if err != nil {
			return err
		}
		_, err = s.Confirm(su.Token, g.u.cfg.Dir, start)
		s.People[0].Added = start.Unix()
		return err
	})
	g.u.trialStep(context.Background(), start.Add(50*24*time.Hour))
	if len(warned) != 0 {
		t.Fatal("warned too soon")
	}
	g.u.trialStep(context.Background(), start.Add(54*24*time.Hour))
	if len(warned) != 1 || warned[0].To != "cara@example.net" || warned[0].Kind != "warn" || warned[0].Login != "cara@box" {
		t.Fatalf("warning: %+v", warned)
	}
	g.u.trialStep(context.Background(), start.Add(55*24*time.Hour))
	if len(warned) != 1 {
		t.Fatal("warned twice")
	}
	g.u.trialStep(context.Background(), start.Add(61*24*time.Hour))
	if st, _ := Load(g.statePath); st.People[0].Trial.Leaving == 0 || st.People[0].Password != nil {
		t.Fatalf("not closed after 60 days: %+v", st.People[0])
	}
}

func TestTrialLimits(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Admin: true, Domain: "example.org", PeopleNames: []peers.PersonName{{Name: "dan.example.org"}}}}
	now := time.Now()
	var s HubState
	if _, err := s.SignUp(list, SignupRequest{Label: "dan", Email: "dan@example.net", Password: "long enough pw"}, now); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("a name the group has: %v", err)
	}
	for i := range TrialPerDay {
		if _, err := s.SignUp(list, SignupRequest{Label: "p" + string(rune('a'+i)), Email: string(rune('a'+i)) + "@example.net", Password: "long enough pw"}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SignUp(list, SignupRequest{Label: "late", Email: "late@example.net", Password: "long enough pw"}, now); err == nil || !strings.Contains(err.Error(), "tomorrow") {
		t.Fatalf("one too many today: %v", err)
	}
	// Two days on, the links have run out.
	if _, err := s.SignUp(list, SignupRequest{Label: "late", Email: "late@example.net", Password: "long enough pw"}, now.Add(49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(s.Signups) != 1 {
		t.Fatalf("old sign-ups kept: %d", len(s.Signups))
	}
}
