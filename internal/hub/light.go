package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/workhere"
)

// Light clients (see docs/web-access.md and package light): people on the
// box who use it from any browser, through the group's web nodes, signing
// in with a passkey. The box keeps their passkeys (the public half) and
// their sessions, and serves them the same files page as over Yggdrasil.

// Passkey is one of a person's passkeys.
type Passkey struct {
	passkey.Key
	RP    string `json:"rp,omitempty"`   // the address it signs in at
	Kind  string `json:"kind,omitempty"` // "" for a passkey, KindBrowser for a key kept by a browser
	Name  string `json:"name"`           // the device, as they named it
	Added int64  `json:"added"`
	Used  int64  `json:"used,omitempty"`
	Count uint32 `json:"count,omitempty"`
	// A browser key also needs a PIN, which the box checks (pinHash), so
	// having the device isn't enough.
	PIN      string `json:"pin,omitempty"`
	PINFails int    `json:"pin_fails,omitempty"`
}

// Session is a browser signed in through the web.
type Session struct {
	Hash    string `json:"hash"` // of the cookie's token
	Person  string `json:"person"`
	Device  string `json:"device"`         // the passkey's name
	Host    string `json:"host,omitempty"` // the address it was started at
	Started int64  `json:"started"`
	Used    int64  `json:"used"`
	Short   bool   `json:"short,omitempty"` // signed in with a browser key: lasts ShortSessionFor
}

// KindBrowser marks a key a browser keeps (see passkey.VerifyBrowser), for
// a device that can't make a passkey, such as a phone without a screen
// lock.
const KindBrowser = "browser"

// SessionFor is how long a session lasts unused.
const SessionFor = 30 * 24 * time.Hour

// ShortSessionFor is how long a session started with a browser key lasts
// unused: the PIN is asked again after that.
const ShortSessionFor = time.Hour

// MaxPINFails wrong PINs in a row lock a browser key.
const MaxPINFails = 5

// For is how long the session lasts unused.
func (ss Session) For() time.Duration {
	if ss.Short {
		return ShortSessionFor
	}
	return SessionFor
}

// WebAccess is whether the person may sign in through the web.
func (p Person) WebAccess() bool { return p.Active() && !p.WebOff }

// CheckInvite is the person an invite token is for, without using it.
func (s HubState) CheckInvite(token string, now time.Time) (Person, error) {
	h := tokenHash(token)
	for _, in := range s.Invites {
		if in.Hash == h && now.Unix() < in.Expires {
			if p, ok := s.PersonNamed(in.Person); ok && p.Active() {
				return p, nil
			}
		}
	}
	return Person{}, errBadInvite
}

// UseInviteWeb uses an invite to give its person a passkey.
func (s *HubState) UseInviteWeb(token string, k Passkey, now time.Time) (Person, error) {
	p, err := s.CheckInvite(token, now)
	if err != nil {
		return Person{}, err
	}
	if !p.WebAccess() {
		return Person{}, errors.New("The box's owner has turned off opening your files through the web. Ask them.")
	}
	if _, _, ok := s.PasskeyOf(k.ID); ok {
		return Person{}, errors.New("This passkey is in use already.")
	}
	i := s.person(p.Name)
	k.Added = now.Unix()
	s.People[i].Passkeys = append(s.People[i].Passkeys, k)
	s.DropInvite(p.Name)
	return s.People[i], nil
}

// PasskeyOf finds the passkey with credential ID id: whose, and where.
func (s HubState) PasskeyOf(id string) (person, key int, ok bool) {
	for i, p := range s.People {
		for j, k := range p.Passkeys {
			if k.ID == id {
				return i, j, true
			}
		}
	}
	return 0, 0, false
}

func sessionHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// NewSession signs person in, on the device the passkey device names. It
// gives the cookie's token, which only the browser keeps.
func (s *HubState) NewSession(person, device, host string, short bool, now time.Time) string {
	b := make([]byte, 32)
	rand.Read(b)
	token := passkey.B64.EncodeToString(b)
	s.PruneSessions(now)
	s.Sessions = append(s.Sessions, Session{Hash: sessionHash(token), Person: person, Device: device, Host: host, Started: now.Unix(), Used: now.Unix(), Short: short})
	return token
}

// PruneSessions drops sessions unused for their time, and those of people
// who may no longer sign in.
func (s *HubState) PruneSessions(now time.Time) {
	kept := s.Sessions[:0]
	for _, ss := range s.Sessions {
		p, ok := s.PersonNamed(ss.Person)
		if ok && p.WebAccess() && now.Sub(time.Unix(ss.Used, 0)) < ss.For() {
			kept = append(kept, ss)
		}
	}
	s.Sessions = kept
}

// SessionOf is the person signed in with token, at host.
func (s HubState) SessionOf(token, host string, now time.Time) (Person, Session, bool) {
	if token == "" {
		return Person{}, Session{}, false
	}
	h := sessionHash(token)
	for _, ss := range s.Sessions {
		if ss.Hash != h || now.Sub(time.Unix(ss.Used, 0)) >= ss.For() || !strings.EqualFold(ss.Host, host) {
			continue
		}
		if p, ok := s.PersonNamed(ss.Person); ok && p.WebAccess() {
			return p, ss, true
		}
	}
	return Person{}, Session{}, false
}

// EndSession signs a session out.
func (s *HubState) EndSession(hash string) {
	kept := s.Sessions[:0]
	for _, ss := range s.Sessions {
		if ss.Hash != hash {
			kept = append(kept, ss)
		}
	}
	s.Sessions = kept
}

// SessionsOf are person's sessions.
func (s HubState) SessionsOf(person string) []Session {
	out := []Session{}
	for _, ss := range s.Sessions {
		if strings.EqualFold(ss.Person, person) {
			out = append(out, ss)
		}
	}
	return out
}

// relayed reports whether r came through one of the group's web nodes.
func (f *filesPage) relayed(ctx context.Context, r *http.Request, remote string) bool {
	if r.Header.Get(light.RelayHeader) != "1" || f.u.cfg.WebNodes == nil {
		return false
	}
	f.nameMu.Lock()
	stale := time.Since(f.webAt) > 5*time.Minute
	f.nameMu.Unlock()
	if stale {
		ips := map[string]bool{}
		for _, a := range f.u.cfg.WebNodes(ctx) {
			if h, _, err := net.SplitHostPort(a); err == nil {
				if ip := net.ParseIP(h); ip != nil {
					ips[ip.String()] = true
				}
			}
		}
		f.nameMu.Lock()
		f.web, f.webAt = ips, time.Now()
		f.nameMu.Unlock()
	}
	f.nameMu.Lock()
	defer f.nameMu.Unlock()
	return f.web[remote]
}

// rpFor is the address a relayed request came to, which its passkeys are
// for: my. and the group's domain, for anyone on the box; or the name of
// someone on this box (kath.example.org), for them alone (only).
func (f *filesPage) rpFor(r *http.Request) (rp, only string, ok bool) {
	list, err := peers.Load(f.u.cfg.PeersPath)
	domain := peers.GroupDomain(list)
	if err != nil || domain == "" {
		return "", "", false
	}
	host := strings.ToLower(strings.TrimSuffix(r.Header.Get("X-Forwarded-Host"), "."))
	if host == light.Host+"."+domain {
		return host, "", true
	}
	if p, pn, found := peers.NameHolder(list, host); found && pn.Person != "" && p.IP() == f.self {
		return host, pn.Person, true
	}
	return "", "", false
}

// boxName is the name the group's list gives this box: the first node at
// its address.
func (f *filesPage) boxName() string {
	list, _ := peers.Load(f.u.cfg.PeersPath)
	for _, p := range list {
		if p.IP() == f.self {
			return p.Name
		}
	}
	return ""
}

func sessionCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: light.SessionCookie, Value: token, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

type passkeyReq struct {
	Kind       string `json:"kind"` // "" or KindBrowser
	T          string `json:"t"`
	Name       string `json:"name"`
	Challenge  string `json:"challenge"`
	ID         string `json:"id"`
	ClientData string `json:"client_data"`
	AuthData   string `json:"auth_data"`
	SPKI       string `json:"spki"`
	Alg        int    `json:"alg"`
	Signature  string `json:"signature"`
	UserHandle string `json:"user_handle"`
	PIN        string `json:"pin"`     // a browser key's PIN
	NewPIN     string `json:"new_pin"` // one chosen for a browser key that has none yet
}

// serveLight serves a request relayed from a web node: signing in, and
// then the person's files page.
func (f *filesPage) serveLight(w http.ResponseWriter, r *http.Request) {
	rp, only, ok := f.rpFor(r)
	if !ok {
		http.Error(w, "this box doesn't answer light clients at that address", http.StatusForbidden)
		return
	}
	origin := "https://" + rp
	// Other web pages open in the browser can't add this header.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Yggstore") != "1" {
		http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
		return
	}
	now := time.Now()
	switch r.URL.Path {
	case JoinPath:
		light.ServeJoin(w)
		return
	case light.Base + "/challenge":
		jsonOK(w, map[string]string{"challenge": passkey.NewChallenge(now), "rp": rp})
		return
	case light.Base + "/begin":
		var req passkeyReq
		if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st, err := Load(f.u.cfg.StatePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p, err := st.CheckInvite(req.T, now)
		if err == nil && only != "" && !strings.EqualFold(p.Name, only) {
			err = errors.New("This invite is for someone else, not for " + rp + ".")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		uid := light.UserID(f.boxName(), p.Name)
		if len(uid) > 64 {
			http.Error(w, "this box's and person's names are too long for a passkey", http.StatusConflict)
			return
		}
		jsonOK(w, map[string]any{"challenge": passkey.NewChallenge(now), "rp": rp, "rp_name": "yggstore " + f.boxName(),
			"user_id": passkey.B64.EncodeToString(uid), "user_name": p.Name + " on " + f.boxName(), "algs": passkey.Algs})
		return
	case light.Base + "/register", light.Base + "/signin":
		f.passkeyIn(w, r, rp, only, origin, now)
		return
	case light.Base + "/signout":
		if c, err := r.Cookie(light.SessionCookie); err == nil {
			Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error { s.EndSession(sessionHash(c.Value)); return nil })
		}
		http.SetCookie(w, sessionCookie("", -1))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	st, err := Load(f.u.cfg.StatePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	token := ""
	if c, err := r.Cookie(light.SessionCookie); err == nil {
		token = c.Value
	}
	p, ss, ok := st.SessionOf(token, rp, now)
	if !ok {
		if r.Method == http.MethodGet && (r.URL.Path == FilesPath || r.URL.Path == FilesPath+"/") && r.URL.RawQuery == "" {
			light.ServeSignIn(w, http.StatusOK)
			return
		}
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if now.Sub(time.Unix(ss.Used, 0)) > ss.For()/24 { // a session lasts its time from its last use
		Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
			for i := range s.Sessions {
				if s.Sessions[i].Hash == ss.Hash {
					s.Sessions[i].Used = now.Unix()
				}
			}
			return nil
		})
		http.SetCookie(w, sessionCookie(token, int(ss.For().Seconds())))
	}
	if r.URL.Path == FilesPath+"/devices" {
		f.lightDevices(w, r, p, ss)
		return
	}
	f.serveAs(w, r, p)
}

// passkeyIn makes a passkey with an invite (register) or signs in with one,
// and starts a session.
func (f *filesPage) passkeyIn(w http.ResponseWriter, r *http.Request, rp, only, origin string, now time.Time) {
	var req passkeyReq
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	dec := func(s string) []byte { b, _ := passkey.B64.DecodeString(s); return b }
	clientData := dec(req.ClientData)
	browser := req.Kind == KindBrowser
	if !browser && passkey.ChallengeOf(clientData) != req.Challenge {
		http.Error(w, "the challenge doesn't match", http.StatusBadRequest)
		return
	}
	if err := f.challenges.Use(req.Challenge, now); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	register := strings.HasSuffix(r.URL.Path, "/register")
	var who, device, token string
	var refused error
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		if register {
			var k passkey.Key
			var err error
			if browser {
				if len(req.ID) < 16 || len(req.ID) > 64 {
					return errors.New("the browser key has no proper ID")
				}
				if err := pinOK(req.PIN); err != nil {
					return err
				}
				err = passkey.VerifyBrowser(dec(req.SPKI), origin, req.Challenge, dec(req.Signature))
				k = passkey.Key{ID: req.ID, SPKI: dec(req.SPKI), Alg: passkey.ES256}
			} else {
				k, err = passkey.Register(clientData, dec(req.AuthData), dec(req.SPKI), req.Alg, req.ID, req.Challenge, origin, rp)
			}
			if err != nil {
				return err
			}
			device = strings.TrimSpace(req.Name)
			if len(device) > 60 {
				device = device[:60]
			}
			if device == "" {
				device = "a browser"
			}
			if p, err := s.CheckInvite(req.T, now); err == nil && only != "" && !strings.EqualFold(p.Name, only) {
				return errors.New("This invite is for someone else, not for " + rp + ".")
			}
			kind := ""
			if browser {
				kind = KindBrowser
			}
			pin := ""
			if browser {
				pin = pinHash(req.PIN)
			}
			p, err := s.UseInviteWeb(req.T, Passkey{Key: k, RP: rp, Kind: kind, Name: device, PIN: pin}, now)
			if err != nil {
				return err
			}
			who = p.Name
		} else {
			i, j, ok := s.PasskeyOf(req.ID)
			if !ok {
				return errors.New("This passkey isn't one this box knows. Use the one you made with your invite, or ask the box's owner for a new invite.")
			}
			pk := &s.People[i].Passkeys[j]
			if browser != (pk.Kind == KindBrowser) {
				return errors.New("That key isn't the kind it says it is.")
			}
			var count uint32
			var err error
			if browser {
				if pk.RP != "" && !strings.EqualFold(pk.RP, rp) {
					return errors.New("This browser's key is for " + pk.RP + ".")
				}
				err = passkey.VerifyBrowser(pk.SPKI, origin, req.Challenge, dec(req.Signature))
			} else {
				count, err = pk.Verify(clientData, dec(req.AuthData), dec(req.Signature), req.Challenge, origin, rp)
			}
			if err != nil {
				return err
			}
			if count != 0 && count <= pk.Count {
				return errors.New("This passkey looks copied (its counter went back). Ask the box's owner.")
			}
			if only != "" && !strings.EqualFold(s.People[i].Name, only) {
				return errors.New("This passkey is someone else's, not for " + rp + ".")
			}
			if !s.People[i].WebAccess() {
				return errors.New("Opening your files through the web is turned off for now. Ask the box's owner.")
			}
			if browser {
				switch {
				case pk.PINFails >= MaxPINFails:
					return errPINLocked
				case pk.PIN == "" && req.NewPIN == "":
					return errNeedPIN
				case pk.PIN == "":
					if err := pinOK(req.NewPIN); err != nil {
						return err
					}
					pk.PIN = pinHash(req.NewPIN)
				case !pinMatches(pk.PIN, req.PIN):
					pk.PINFails++ // kept: this Update saves, and refused says no
					refused = errWrongPIN(MaxPINFails - pk.PINFails)
					return nil
				}
				pk.PINFails = 0
			}
			pk.Count, pk.Used = count, now.Unix()
			who, device = s.People[i].Name, pk.Name
		}
		token = s.NewSession(who, device, rp, browser, now)
		return nil
	})
	if err == nil {
		err = refused
	}
	if errors.Is(err, errNeedPIN) {
		http.Error(w, err.Error(), http.StatusPreconditionRequired) // the page asks for a new PIN
		return
	}
	if err != nil {
		f.u.cfg.Logf("hub: web sign-in refused (from %s): %v", r.Header.Get("X-Forwarded-For"), err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	verb := "signed in"
	if register {
		verb = "made a passkey"
		if browser {
			verb = "made a browser key"
		}
	}
	f.u.cfg.Logf("hub: %s %s through the web (%s, from %s)", who, verb, device, r.Header.Get("X-Forwarded-For"))
	life := SessionFor
	if browser {
		life = ShortSessionFor
	}
	http.SetCookie(w, sessionCookie(token, int(life.Seconds())))
	jsonOK(w, map[string]string{"person": who})
}

// lightDevices is the person's passkeys and signed-in browsers, as a light
// client sees them.
func (f *filesPage) lightDevices(w http.ResponseWriter, r *http.Request, p Person, me Session) {
	switch op := r.URL.Query().Get("op"); {
	case op == "" && r.Method == http.MethodGet:
		f.lightDevicesPage(w)
	case op == "list" && r.Method == http.MethodGet:
		st, err := Load(f.u.cfg.StatePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p, _ = st.PersonNamed(p.Name)
		type sess struct {
			Session
			Me bool `json:"me,omitempty"`
		}
		ss := []sess{}
		for _, s := range st.SessionsOf(p.Name) {
			ss = append(ss, sess{s, s.Hash == me.Hash})
		}
		keys := []Passkey{}
		for _, k := range p.Passkeys {
			k.PIN = "" // the browser has no business with it
			keys = append(keys, k)
		}
		jsonOK(w, map[string]any{"person": p.Name, "passkeys": keys, "sessions": ss})
	case (op == "signout" || op == "forget") && r.Method == http.MethodPost:
		arg := r.URL.Query().Get("id")
		_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
			if op == "signout" {
				for _, ss := range s.SessionsOf(p.Name) {
					if ss.Hash == arg {
						s.EndSession(arg)
					}
				}
				return nil
			}
			i := s.person(p.Name)
			kept := s.People[i].Passkeys[:0]
			for _, k := range s.People[i].Passkeys {
				if k.ID != arg {
					kept = append(kept, k)
				}
			}
			if len(kept) == 0 {
				return errors.New("this is your last passkey: keep it, or you can't sign in")
			}
			s.People[i].Passkeys = kept
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *filesPage) lightDevicesPage(w http.ResponseWriter) {
	page(w, "Your devices", `<h1>Your devices</h1>
<p>You sign in with a passkey. These are your passkeys, and the browsers signed in with them.</p>
<h2 style="font-size:1em">Passkeys</h2><ul id="keys"><li>…</li></ul>
<h2 style="font-size:1em">Signed in</h2><ul id="sessions"><li>…</li></ul>
<p><a href="`+FilesPath+`">Back to your files</a> · <a href="#" id="out">Sign out here</a></p>
<p style="color:#666;font-size:.9em">A new phone or computer: passkeys often reach your other devices through your Apple, Google or Microsoft account.
If not, ask the box's owner for a new invite and open it there.</p>
<script>
const esc = s => String(s).replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
const when = t => t ? new Date(t * 1000).toLocaleString() : "never";
const post = u => fetch(u, {method: "POST", headers: {"X-Yggstore": "1"}});
async function load() {
  const d = await (await fetch("?op=list", {cache: "no-store"})).json();
  document.getElementById("keys").innerHTML = d.passkeys.map(k => "<li><b>" + esc(k.name) + "</b>" + (k.kind === "browser" ? " (kept by that browser)" : "") + ", made " + when(k.added) + ", last used " + when(k.used) +
    (d.passkeys.length > 1 ? " <button data-forget='" + esc(k.id) + "'>Forget</button>" : "") + "</li>").join("");
  document.getElementById("sessions").innerHTML = d.sessions.map(s => "<li><b>" + esc(s.device) + "</b>" + (s.me ? " (this browser)" : "") +
    ", since " + when(s.started) + ", last used " + when(s.used) + " <button data-out='" + esc(s.hash) + "'>Sign out</button></li>").join("") || "<li>None</li>";
}
document.addEventListener("click", async ev => {
  const b = ev.target;
  if (b.dataset.forget && confirm("Forget this passkey? It won't sign in any more.")) { const r = await post("?op=forget&id=" + encodeURIComponent(b.dataset.forget)); if (!r.ok) alert(await r.text()); load(); }
  if (b.dataset.out) { await post("?op=signout&id=" + encodeURIComponent(b.dataset.out)); load(); }
  if (b.id === "out") { ev.preventDefault(); await post("`+light.Base+`/signout"); location.href = "`+FilesPath+`"; }
});
load();
</script>`)
}

// serveAs serves the files page to person p, however they got in.
func (f *filesPage) serveAs(w http.ResponseWriter, r *http.Request, p Person) {
	switch r.URL.Path {
	case FilesPath:
		name := p.Name
		live := &workhere.Live{Outbox: p.Path, Client: f.client, Holding: func() bool { return f.u.Holding(name) },
			Devices: FilesPath + "/devices", Room: func(size int64) error { return f.room(p, size) }}
		if f.u.cfg.People != nil {
			live.Site = SitePath
		}
		live.ServeHTTP(w, r)
	case SitePath:
		if f.u.cfg.People == nil {
			http.NotFound(w, r)
			return
		}
		f.site(w, r, p)
	default:
		http.NotFound(w, r)
	}
}
