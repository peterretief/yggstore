package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

// Webmail on the box's own network: someone on the box opens
// http://BOX.local/webmail (or the box's address there) on a phone or a
// laptop on the site's Wi-Fi, signs in with their name and their box
// password, and reads and writes their mail, with no internet and nothing
// joined to Yggdrasil. Passkeys need a secure (https) address, which a box
// on a local network hasn't got, so it's the password. Only the local
// network gets it (OnLAN), and its sessions stay on the box.

const (
	WebmailPath   = "/webmail"
	webmailCookie = "box_webmail"
	webmailFor    = 30 * 24 * time.Hour
)

type webmailSession struct {
	Name string `json:"name"`
	Key  string `json:"key"` // the password it started with: a new one ends it
	Ends int64  `json:"ends"`
}

type webmailSessions struct {
	mu    sync.Mutex
	path  string
	list  map[string]webmailSession // by the token's hash
	tries map[string][]time.Time    // sign-ins, by address
}

func (s *webmailSessions) load() {
	if s.list != nil {
		return
	}
	s.list, s.tries = map[string]webmailSession{}, map[string][]time.Time{}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.list)
	}
}

func (s *webmailSessions) save() {
	b, _ := json.Marshal(s.list)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.path)
	}
}

func hashToken(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }

// tooMany says whether an address has tried to sign in too often lately.
func (s *webmailSessions) tooMany(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	keep := s.tries[ip][:0]
	for _, t := range s.tries[ip] {
		if now.Sub(t) < 15*time.Minute {
			keep = append(keep, t)
		}
	}
	s.tries[ip] = append(keep, now)
	return len(keep) >= 10
}

func (f *filesPage) webmailState() *webmailSessions {
	f.nameMu.Lock()
	defer f.nameMu.Unlock()
	if f.wm == nil {
		f.wm = &webmailSessions{path: filepath.Join(filepath.Dir(f.u.cfg.StatePath), "webmail-sessions.json")}
	}
	return f.wm
}

// webmailPerson is who a request's cookie says, if they may still read
// their mail: on the box, active, with the password the session began with.
func (f *filesPage) webmailPerson(r *http.Request, now time.Time) (Person, bool) {
	c, err := r.Cookie(webmailCookie)
	if err != nil {
		return Person{}, false
	}
	s := f.webmailState()
	s.mu.Lock()
	s.load()
	ss, ok := s.list[hashToken(c.Value)]
	s.mu.Unlock()
	if !ok || now.Unix() > ss.Ends {
		return Person{}, false
	}
	st, err := Load(f.u.cfg.StatePath)
	if err != nil {
		return Person{}, false
	}
	p, ok := st.PersonNamedOK(ss.Name)
	if !ok || !p.Active() || p.Password == nil || p.Password.ID() != ss.Key {
		return Person{}, false
	}
	return p, true
}

// localHost is a Host a browser on the local network uses for the box: an
// address, a .local name, or a bare name (which stops DNS rebinding from
// elsewhere).
func localHost(h string) bool {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	return net.ParseIP(h) != nil || strings.HasSuffix(h, ".local") || (h != "" && !strings.Contains(h, "."))
}

// ServeWebmail answers /webmail (and /mail) for the local network; it
// reports whether the request was its to answer.
func (f *filesPage) ServeWebmail(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if p != WebmailPath && p != "/mail" && !strings.HasPrefix(p, WebmailPath+"/") {
		return false
	}
	if !OnLAN(r.RemoteAddr) || !localHost(r.Host) {
		http.Error(w, "webmail opens on the box's own network", http.StatusForbidden)
		return true
	}
	if f.u.cfg.People == nil {
		http.NotFound(w, r)
		return true
	}
	if p == "/mail" {
		http.Redirect(w, r, WebmailPath, http.StatusFound)
		return true
	}
	now := time.Now()
	switch p {
	case WebmailPath + "/signin":
		if r.Method == http.MethodPost {
			f.webmailSignIn(w, r, now)
			return true
		}
	case WebmailPath + "/signout":
		if r.Method == http.MethodPost {
			if c, err := r.Cookie(webmailCookie); err == nil {
				s := f.webmailState()
				s.mu.Lock()
				s.load()
				delete(s.list, hashToken(c.Value))
				s.save()
				s.mu.Unlock()
			}
			http.SetCookie(w, &http.Cookie{Name: webmailCookie, Path: "/", MaxAge: -1, HttpOnly: true})
			http.Redirect(w, r, WebmailPath, http.StatusSeeOther)
			return true
		}
	}
	person, ok := f.webmailPerson(r, now)
	if !ok {
		webmailSignInPage(w, http.StatusOK, "", "", f.awayAt())
		return true
	}
	if p != WebmailPath {
		http.NotFound(w, r)
		return true
	}
	// Other web pages open in the browser can't add this header.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Yggstore") != "1" {
		http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
		return true
	}
	r.URL.Path = MailPath // the same page and answers as on the web
	f.site(w, r, person)
	return true
}

func (f *filesPage) webmailSignIn(w http.ResponseWriter, r *http.Request, now time.Time) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	r.ParseForm()
	name, pw := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("password")
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s := f.webmailState()
	if s.tooMany(ip, now) {
		webmailSignInPage(w, http.StatusTooManyRequests, name, "Too many tries. Wait a quarter of an hour, then try again.", f.awayAt())
		return
	}
	st, err := Load(f.u.cfg.StatePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Their name on the box, or their local address.
	local, _, _ := strings.Cut(strings.ToLower(name), "@")
	var p Person
	found := false
	for _, q := range st.People {
		if strings.EqualFold(q.Name, name) || peers.LocalPart(q.Name) == peers.LocalPart(local) {
			p, found = q, true
			break
		}
	}
	if !found || !p.Active() || !p.Password.Live(now) || !p.Password.Matches(pw) {
		webmailSignInPage(w, http.StatusForbidden, name, "That name and password don't match. If you haven't got a password yet, ask the box's owner for one.", f.awayAt())
		return
	}
	// A temporary password is changed at the first sign-in, as on the web.
	if p.Password.Temp {
		chosen, again := r.PostFormValue("new"), r.PostFormValue("again")
		if chosen == "" {
			webmailChoosePage(w, name, pw, "")
			return
		}
		if chosen != again {
			webmailChoosePage(w, name, pw, "The two don't match.")
			return
		}
		if err := password.OK(chosen); err != nil {
			webmailChoosePage(w, name, pw, err.Error())
			return
		}
		next := password.Chosen(chosen, now)
		if _, err := Update(r.Context(), f.u.cfg.StatePath, func(st *HubState) error {
			if i := st.person(p.Name); i >= 0 {
				st.People[i].Password = &next
			}
			return nil
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.u.lightChanged() // the web nodes learn the new one too
		p.Password = &next
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.load()
	for k, v := range s.list {
		if now.Unix() > v.Ends {
			delete(s.list, k)
		}
	}
	s.list[hashToken(token)] = webmailSession{Name: p.Name, Key: p.Password.ID(), Ends: now.Add(webmailFor).Unix()}
	s.save()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: webmailCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: now.Add(webmailFor)})
	http.Redirect(w, r, WebmailPath, http.StatusSeeOther)
}

const webmailStyle = `<meta name="viewport" content="width=device-width, initial-scale=1"><style>
body{font:16px/1.5 system-ui,sans-serif;max-width:24rem;margin:3rem auto;padding:0 1rem;color:#1c1c1c;background:#f5f6f8}
label{display:block;margin:.8rem 0}input{display:block;width:100%;box-sizing:border-box;padding:.6rem;font:inherit;margin-top:.2rem}
button{margin-top:.6rem;padding:.6rem 1.2rem;font:inherit;background:#2b6fd6;color:#fff;border:0;border-radius:8px}
.err{color:#b3261e}.sub{color:#666;font-size:.9rem}
@media (prefers-color-scheme:dark){body{background:#151719;color:#e8e6e1}.sub{color:#9aa0a6}}</style>`

// awayAt is where the same mail opens away from the box's network.
func (f *filesPage) awayAt() string {
	if list, err := peers.Load(f.u.cfg.PeersPath); err == nil && peers.GroupDomain(list) != "" {
		return "my." + peers.GroupDomain(list) + "/mail"
	}
	return ""
}

func webmailSignInPage(w http.ResponseWriter, code int, name, msg, away string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	errText := ""
	if msg != "" {
		errText = `<p class="err">` + html.EscapeString(msg) + `</p>`
	}
	w.Write([]byte(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Your mail</title>` + webmailStyle + `
<h1>Your mail</h1>` + errText + `
<form method="post" action="` + WebmailPath + `/signin">
<label>Your name on the box <input name="name" autocomplete="username" required autofocus value="` + html.EscapeString(name) + `"></label>
<label>Your password <input name="password" type="password" autocomplete="current-password" required></label>
<button>Open your mail</button></form>
<p class="sub">This opens only on the box's own network.` + map[bool]string{true: " Away from it, sign in at " + html.EscapeString(away) + ".", false: ""}[away != ""] + `</p></html>`))
}

func webmailChoosePage(w http.ResponseWriter, name, pw, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	errText := ""
	if msg != "" {
		errText = `<p class="err">` + html.EscapeString(msg) + `</p>`
	}
	w.Write([]byte(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Choose your password</title>` + webmailStyle + `
<h1>Choose your password</h1><p>That was a temporary password. Choose your own, at least ` + "10" + ` characters.</p>` + errText + `
<form method="post" action="` + WebmailPath + `/signin">
<input type="hidden" name="name" value="` + html.EscapeString(name) + `"><input type="hidden" name="password" value="` + html.EscapeString(pw) + `">
<label>New password <input name="new" type="password" autocomplete="new-password" minlength="10" required autofocus></label>
<label>Again <input name="again" type="password" autocomplete="new-password" minlength="10" required></label>
<button>Save and open your mail</button></form></html>`))
}
