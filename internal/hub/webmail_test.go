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

	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

func TestWebmail(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	temp, tempPW := password.NewTemp(time.Now())
	kath := password.Chosen("kath's password 1", time.Now())
	Update(ctx, statePath, func(s *HubState) error {
		s.AddPerson("Kath", filepath.Join(dir, "users"))
		s.AddPerson("Ewart", filepath.Join(dir, "users"))
		s.People[0].Password = &kath
		s.People[1].Password = &tempPW
		return nil
	})
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, []peers.Peer{{Name: "admin", Addr: "[200::1]:7400", Admin: true, Domain: "group.example"}, {Name: "localbox-004", Addr: "[200::2]:7400"}})
	ps := NewPeopleSites(PeopleSitesConfig{StatePath: statePath, PeersPath: peersPath, Self: "200::2", Dir: filepath.Join(dir, "people"), Logf: t.Logf})
	u := &Users{cfg: UsersConfig{StatePath: statePath, PeersPath: peersPath, People: ps}}
	f := &filesPage{u: u, self: "200::2"}

	do := func(method, path, remote, host, cookie string, form url.Values) *httptest.ResponseRecorder {
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		r := httptest.NewRequest(method, path, body)
		r.RemoteAddr, r.Host = remote, host
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: webmailCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		if !f.ServeWebmail(w, r) {
			t.Fatalf("%s %s wasn't webmail's", method, path)
		}
		return w
	}
	const lan, box = "192.168.1.20:5555", "localbox-004.local"
	if w := do("GET", "/webmail", "41.1.2.3:5555", box, "", nil); w.Code != 403 {
		t.Fatalf("from the internet: %d", w.Code)
	}
	if w := do("GET", "/webmail", lan, "evil.example.com", "", nil); w.Code != 403 {
		t.Fatalf("under another site's name: %d", w.Code)
	}
	if w := do("GET", "/mail", lan, box, "", nil); w.Code != 302 || w.Header().Get("Location") != "/webmail" {
		t.Fatalf("/mail: %d", w.Code)
	}
	if w := do("GET", "/webmail", lan, box, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Your name on the box") || !strings.Contains(w.Body.String(), "my.group.example/mail") {
		t.Fatalf("sign-in page: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/webmail/signin", lan, box, "", url.Values{"name": {"Kath"}, "password": {"wrong"}}); w.Code != 403 {
		t.Fatalf("a wrong password: %d", w.Code)
	}
	w := do("POST", "/webmail/signin", lan, box, "", url.Values{"name": {"kath@localbox-004.group.internal"}, "password": {"kath's password 1"}})
	if w.Code != 303 {
		t.Fatalf("sign in: %d %s", w.Code, w.Body)
	}
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == webmailCookie {
			cookie = c.Value
		}
	}
	if w := do("GET", "/webmail", "[fe80::1%eth0]:5555", box, cookie, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "<title>Your mail</title>") {
		t.Fatalf("the mail page: %d", w.Code)
	}
	w = do("GET", "/webmail?op=status", lan, "192.168.1.2", cookie, nil)
	var st sitePageState
	if json.Unmarshal(w.Body.Bytes(), &st); st.Address != "kath@localbox-004.group.internal" || !st.Local {
		t.Fatalf("status: %s", w.Body)
	}
	if w := do("POST", "/webmail?op=mkfolder", lan, box, cookie, nil); w.Code != 403 {
		t.Fatalf("a post without the header: %d", w.Code)
	}
	// A new password elsewhere ends the session.
	Update(ctx, statePath, func(s *HubState) error { p := password.Chosen("another password 1", time.Now()); s.People[0].Password = &p; return nil })
	if w := do("GET", "/webmail", lan, box, cookie, nil); !strings.Contains(w.Body.String(), "Your name on the box") {
		t.Fatal("the session outlived the password")
	}
	// A temporary password is changed first.
	w = do("POST", "/webmail/signin", lan, box, "", url.Values{"name": {"ewart"}, "password": {temp}})
	if !strings.Contains(w.Body.String(), "Choose your password") {
		t.Fatalf("temporary: %d %s", w.Code, w.Body)
	}
	w = do("POST", "/webmail/signin", lan, box, "", url.Values{"name": {"ewart"}, "password": {temp}, "new": {"ewart's new one"}, "again": {"ewart's new one"}})
	if w.Code != 303 {
		t.Fatalf("chosen: %d %s", w.Code, w.Body)
	}
	if st, _ := Load(statePath); st.People[1].Password.Temp || !st.People[1].Password.Matches("ewart's new one") {
		t.Fatal("the new password isn't kept")
	}
	if f.ServeWebmail(httptest.NewRecorder(), httptest.NewRequest("GET", "/files", nil)) {
		t.Fatal("took /files")
	}
	os.Remove(filepath.Join(dir, "webmail-sessions.json"))
}
