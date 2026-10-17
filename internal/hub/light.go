package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/workhere"
)

// Light clients (see docs/web-access.md and package light): people on the
// box who use it from any browser, through the group's web nodes, signing
// in with a passkey or a browser key and PIN. The box makes their keys with
// an invite and keeps them (the public half), and sends them to every web
// node (pushLight), which signs people in and keeps the sessions. A
// request a web node relays names the person it signed in, and the box
// serves them the same files page as over Yggdrasil.

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

// KindBrowser marks a key a browser keeps (see light.KindBrowser).
const KindBrowser = light.KindBrowser

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

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// serveLight serves a request relayed from a web node: an invite's use,
// a web node's word of a key's use, and the files page of the person the
// web node signed in.
func (f *filesPage) serveLight(w http.ResponseWriter, r *http.Request) {
	rp, only, ok := f.rpFor(r)
	if !ok {
		http.Error(w, "this box doesn't answer light clients at that address", http.StatusForbidden)
		return
	}
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
	case light.Base + "/begin":
		var req light.SignInRequest
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
	case light.Base + "/register":
		f.register(w, r, rp, only, now)
		return
	}
	st, err := Load(f.u.cfg.StatePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p, ok := st.PersonNamed(r.Header.Get(light.PersonHeader))
	if !ok || !p.WebAccess() || (only != "" && !strings.EqualFold(p.Name, only)) {
		if r.Method == http.MethodGet && r.URL.Path == FilesPath && r.URL.RawQuery == "" {
			light.ServeSignIn(w, http.StatusOK)
			return
		}
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case light.SeenPath:
		f.seenKey(w, r, p, now)
	case FilesPath + "/devices":
		f.lightDevices(w, r, p)
	default:
		if r.Method == http.MethodGet {
			if v := f.u.copyLink(p.Name); v != "" {
				w.Header().Set(light.CopyHeader, v)
			}
		}
		f.serveAs(w, r, p)
	}
}

// register makes a passkey or browser key with an invite. The web node
// that passed it on signs the person in with it.
func (f *filesPage) register(w http.ResponseWriter, r *http.Request, rp, only string, now time.Time) {
	var req light.SignInRequest
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	dec := func(s string) []byte { b, _ := passkey.B64.DecodeString(s); return b }
	origin := "https://" + rp
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
	var made Passkey
	var who string
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		var k passkey.Key
		var err error
		if browser {
			if len(req.ID) < 16 || len(req.ID) > 64 {
				return errors.New("the browser key has no proper ID")
			}
			if err := passkey.PINOK(req.PIN); err != nil {
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
		device := strings.TrimSpace(req.Name)
		if len(device) > 60 {
			device = device[:60]
		}
		if device == "" {
			device = "a browser"
		}
		if p, err := s.CheckInvite(req.T, now); err == nil && only != "" && !strings.EqualFold(p.Name, only) {
			return errors.New("This invite is for someone else, not for " + rp + ".")
		}
		made = Passkey{Key: k, RP: rp, Name: device}
		if browser {
			made.Kind, made.PIN = KindBrowser, passkey.PINHash(req.PIN)
		}
		p, err := s.UseInviteWeb(req.T, made, now)
		if err != nil {
			return err
		}
		made.Added = now.Unix()
		who = p.Name
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	f.u.lightChanged()
	verb := "made a passkey"
	if browser {
		verb = "made a browser key"
	}
	f.u.cfg.Logf("hub: %s %s through the web (%s, from %s)", who, verb, made.Name, r.Header.Get("X-Forwarded-For"))
	jsonOK(w, light.Registered{Person: who, Key: made.light()})
}

// seenKey takes a web node's word of one of p's keys: used to sign in,
// its wrong PINs in a row, or a PIN chosen for a key made before PINs.
func (f *filesPage) seenKey(w http.ResponseWriter, r *http.Request, p Person, now time.Time) {
	var seen light.Seen
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&seen) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		i := s.person(p.Name)
		for j := range s.People[i].Passkeys {
			k := &s.People[i].Passkeys[j]
			if k.ID != seen.Key {
				continue
			}
			if seen.PIN != "" {
				if k.Kind != KindBrowser || k.PIN != "" {
					return errors.New("this key has a PIN, or needs none")
				}
				k.PIN = seen.PIN
			}
			if seen.Used {
				k.Used = now.Unix()
			}
			k.PINFails = seen.PINFails
			return nil
		}
		return errors.New("no such key")
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	f.u.lightChanged()
	w.WriteHeader(http.StatusNoContent)
}

// lightDevices serves the person's devices page. The web node answers
// the list and signing out (light.Relay); forgetting a key is the box's.
func (f *filesPage) lightDevices(w http.ResponseWriter, r *http.Request, p Person) {
	switch op := r.URL.Query().Get("op"); {
	case op == "" && r.Method == http.MethodGet:
		f.lightDevicesPage(w)
	case op == "forget" && r.Method == http.MethodPost:
		arg := r.URL.Query().Get("id")
		_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
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
		f.u.lightChanged()
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

// light is the key as the web nodes are told it.
func (k Passkey) light() light.Key {
	return light.Key{Key: k.Key, RP: k.RP, Kind: k.Kind, Name: k.Name, PIN: k.PIN, PINFails: k.PINFails, Added: k.Added, Used: k.Used}
}

// LightPeople is what the web nodes are told of the box's people: everyone
// with a key, and whether they may sign in now.
func (s HubState) LightPeople() light.People {
	out := light.People{People: []light.Person{}}
	for _, p := range s.People {
		if len(p.Passkeys) == 0 {
			continue
		}
		lp := light.Person{Name: p.Name, Web: p.WebAccess(), Recovery: p.RecoveryID}
		for _, k := range p.Passkeys {
			lp.Keys = append(lp.Keys, k.light())
		}
		out.People = append(out.People, lp)
	}
	return out
}

// lightChanged sends the web nodes the box's people soon.
func (u *Users) lightChanged() {
	select {
	case u.lightKick <- struct{}{}:
	default:
	}
	select {
	case u.copiesKick <- struct{}{}: // a new key: start their copies now
	default:
	}
}

// LightEvery is how often a box sends the web nodes its people unasked.
const LightEvery = 10 * time.Minute

// pushLight sends every web node the box's people (see package light)
// when they change (the hub's state file, whoever changed it), and every
// LightEvery, so a web node that was off catches up.
func (u *Users) pushLight(ctx context.Context) {
	if u.cfg.WebNodes == nil {
		return
	}
	var lastMod time.Time
	var lastSent []byte
	sent := map[string]time.Time{} // per web node, when it last took them
	var listed time.Time
	failed := false
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		changed := false
		if fi, err := os.Stat(u.cfg.StatePath); err == nil && !fi.ModTime().Equal(lastMod) {
			if st, err := Load(u.cfg.StatePath); err == nil {
				lastMod = fi.ModTime()
				b, _ := json.Marshal(st.LightPeople())
				if !bytes.Equal(b, lastSent) {
					lastSent, changed = b, true
				}
			}
		}
		// Finding the web nodes asks every node, so only when there's
		// something to send.
		if lastSent != nil && (changed || failed || time.Since(listed) > LightEvery) {
			var ps light.People
			json.Unmarshal(lastSent, &ps)
			listed, failed = time.Now(), false
			for _, n := range u.cfg.WebNodes(ctx) {
				if !changed && time.Since(sent[n]) < LightEvery {
					continue
				}
				pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				err := light.SendPeople(pctx, u.cfg.Client.HTTP, n, ps)
				cancel()
				if err != nil {
					u.cfg.Logf("hub: sending %s the box's people: %v", n, err)
					delete(sent, n)
					failed = true
					continue
				}
				sent[n] = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-u.lightKick:
		}
	}
}
