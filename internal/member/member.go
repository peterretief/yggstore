// Package member signs a member in to the group login (docs/login.md, step
// 2) with a passkey, as a box does for its people (package hub): the
// member's own node keeps their passkeys and invite, sends the web nodes
// the public halves, and answers the invite and its pages on its node API,
// under light.MemberBase, where the web nodes pass them.
package member

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

// InviteFor is how long an invite works, unused.
const InviteFor = 7 * 24 * time.Hour

// SendEvery is how often the node sends the web nodes its member unasked,
// so a web node that was off catches up.
const SendEvery = 10 * time.Minute

// Login is a member's side of the group login, on their own node.
type Login struct {
	// Path is the state file: the member's passkeys and invite.
	Path string
	// Self is this node's address (its node ID).
	Self  string
	Peers func() []peers.Peer
	// WebNodes are the group's web nodes (node API addresses), and
	// IsWebNode whether a caller is one.
	WebNodes  func(ctx context.Context) []string
	IsWebNode func(ctx context.Context, caller string) bool
	Client    *http.Client
	Log       func(format string, args ...any)
	// IsBox is whether this machine is a box, whose hub answers on
	// light.HubPort (by default, it's asked). A box's owner signs in as a
	// person on it: were the node to say it's a member's, the web nodes
	// would send its people's requests here instead of to the hub.
	IsBox func() bool

	challenges passkey.Challenges
	mu         sync.Mutex
	kick       chan struct{}
}

type state struct {
	Keys   []light.Key `json:"keys"`
	Invite *invite     `json:"invite,omitempty"`
}

type invite struct {
	Hash    string `json:"hash"`
	Token   string `json:"token"` // the same link until it's used
	Expires int64  `json:"expires"`
}

func hash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (l *Login) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log(format, args...)
	}
}

// load reads the state; ok is false when the member hasn't started (no
// file), and the node then sends the web nodes nothing.
func (l *Login) load() (st state, ok bool, err error) {
	b, err := os.ReadFile(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	return st, true, json.Unmarshal(b, &st)
}

// update changes the state under the lock and saves it.
func (l *Login) update(f func(*state) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, _, err := l.load()
	if err != nil {
		return err
	}
	if err := f(&st); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := atomicfile.Replace(l.Path, b, 0o600); err != nil {
		return err
	}
	l.changed()
	return nil
}

func (l *Login) changed() {
	if l.kick == nil {
		return
	}
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// me is this node's entry in the group's list.
func (l *Login) me() (peers.Peer, bool) {
	for _, p := range l.Peers() {
		if p.IP() == l.Self {
			return p, true
		}
	}
	return peers.Peer{}, false
}

// Person is the member's name at the group login: their node's owner.
func (l *Login) Person() string {
	p, ok := l.me()
	if !ok || p.Owner == "" {
		return ""
	}
	return p.Owner
}

func (l *Login) isBox() bool {
	if l.IsBox != nil {
		return l.IsBox()
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(l.Self, light.HubPort), 2*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Invite is a link that makes a passkey for the group login, at my. on the
// group's domain; asked again before it's used, it's the same link.
func (l *Login) Invite(now time.Time) (string, error) {
	if l.isBox() {
		return "", errors.New("this machine is a box: sign in as a person on it (yggstore hub person invite)")
	}
	me, ok := l.me()
	domain := peers.GroupDomain(l.Peers())
	if !ok || domain == "" {
		return "", errors.New("this node isn't in the group's list, or the group has no domain")
	}
	if l.Person() == "" {
		return "", errors.New("this node has no owner in the group's list")
	}
	var token string
	err := l.update(func(st *state) error {
		if in := st.Invite; in != nil && now.Unix() < in.Expires && in.Token != "" {
			in.Expires = now.Add(InviteFor).Unix()
			token = in.Token
			return nil
		}
		b := make([]byte, 18)
		rand.Read(b)
		token = base64.RawURLEncoding.EncodeToString(b)
		st.Invite = &invite{Hash: hash(token), Token: token, Expires: now.Add(InviteFor).Unix()}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "https://" + light.Host + "." + domain + light.JoinPath + "?box=" + me.Name + "&t=" + token, nil
}

// Keys are the member's passkeys.
func (l *Login) Keys() ([]light.Key, error) {
	st, _, err := l.load()
	return st.Keys, err
}

// Forget drops a passkey; not the last.
func (l *Login) Forget(id string) error {
	return l.update(func(st *state) error {
		kept := st.Keys[:0]
		for _, k := range st.Keys {
			if k.ID != id {
				kept = append(kept, k)
			}
		}
		if len(kept) == len(st.Keys) {
			return errors.New("no such passkey")
		}
		if len(kept) == 0 {
			return errors.New("this is your last way in: keep it, or you can't sign in")
		}
		st.Keys = kept
		return nil
	})
}

// checkInvite is the invite a token names, if it still works.
func (st state) checkInvite(token string, now time.Time) error {
	in := st.Invite
	if token == "" || in == nil || in.Hash != hash(token) {
		return errors.New("This invite link isn't one this node made, or it was used already. Make a new one on your dashboard.")
	}
	if now.Unix() >= in.Expires {
		return errors.New("This invite link is more than a week old. Make a new one on your dashboard.")
	}
	return nil
}

// rp is the address a passkey is made and used at: my. on the group's
// domain, as the web node says.
func (l *Login) rp(r *http.Request) (string, bool) {
	domain := peers.GroupDomain(l.Peers())
	host := strings.ToLower(strings.TrimSuffix(r.Header.Get("X-Forwarded-Host"), "."))
	return host, domain != "" && host == light.Host+"."+domain
}

// Serve answers the node API under light.MemberBase: the member's own
// machine making an invite (POST …/invite), and the group's web nodes
// passing on the invite and the member's pages. It reports whether the
// request was its to answer.
func (l *Login) Serve(w http.ResponseWriter, r *http.Request, caller string) bool {
	p, ok := strings.CutPrefix(r.URL.Path, light.MemberBase)
	if !ok || (p != "" && !strings.HasPrefix(p, "/")) {
		return false
	}
	if p == "/invite" || p == "/keys" || p == "/forget" {
		if caller != l.Self {
			http.Error(w, "only this node's own machine", http.StatusForbidden)
			return true
		}
		l.own(w, r, p)
		return true
	}
	if l.IsWebNode == nil || !l.IsWebNode(r.Context(), caller) {
		http.Error(w, "only the group's web nodes", http.StatusForbidden)
		return true
	}
	if _, started, _ := l.load(); !started {
		http.Error(w, "this member hasn't set up the group login", http.StatusNotFound)
		return true
	}
	l.web(w, r, p)
	return true
}

// own answers the member's own machine (the dashboard, the command line).
func (l *Login) own(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case p == "/invite" && r.Method == http.MethodPost:
		link, err := l.Invite(time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		jsonOK(w, map[string]string{"link": link, "person": l.Person()})
	case p == "/keys" && r.Method == http.MethodGet:
		keys, err := l.Keys()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if keys == nil {
			keys = []light.Key{}
		}
		jsonOK(w, map[string]any{"person": l.Person(), "keys": keys})
	case p == "/forget" && r.Method == http.MethodPost:
		if err := l.Forget(r.URL.Query().Get("id")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// web answers what the web nodes pass on: what a box answers on HubPort.
func (l *Login) web(w http.ResponseWriter, r *http.Request, p string) {
	now := time.Now()
	rp, rpOK := l.rp(r)
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Yggstore") != "1" {
		http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
		return
	}
	switch p {
	case light.Base + "/ping":
		w.WriteHeader(http.StatusNoContent)
		return
	case light.JoinPath:
		light.ServeJoin(w)
		return
	case light.Base + "/begin":
		var req light.SignInRequest
		if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st, _, err := l.load()
		if err == nil {
			err = st.checkInvite(req.T, now)
		}
		if err == nil && !rpOK {
			err = errors.New("A member's passkey is made at the group's login, my. on the group's domain.")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		me, _ := l.me()
		uid := light.UserID(me.Name, l.Person())
		if len(uid) > 64 {
			http.Error(w, "this node's and member's names are too long for a passkey", http.StatusConflict)
			return
		}
		jsonOK(w, map[string]any{"challenge": passkey.NewChallenge(now), "rp": rp, "rp_name": "yggstore",
			"user_id": passkey.B64.EncodeToString(uid), "user_name": l.Person() + " (" + me.Name + ")", "algs": passkey.Algs})
		return
	case light.Base + "/register":
		if !rpOK {
			http.Error(w, "A member's passkey is made at the group's login, my. on the group's domain.", http.StatusForbidden)
			return
		}
		l.register(w, r, rp, now)
		return
	}
	// The rest is for the member, signed in: the web node names them.
	if who := r.Header.Get(light.PersonHeader); who == "" || !strings.EqualFold(who, l.Person()) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	switch {
	case p == light.SeenPath && r.Method == http.MethodPost:
		var seen light.Seen
		if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&seen) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if seen.Used {
			l.update(func(st *state) error {
				for i := range st.Keys {
					if st.Keys[i].ID == seen.Key {
						st.Keys[i].Used = now.Unix()
					}
				}
				return nil
			})
		}
		w.WriteHeader(http.StatusNoContent)
	case p == "/files/devices" && r.Method == http.MethodGet && r.URL.Query().Get("op") == "":
		devicesPage(w)
	case p == "/files/devices" && r.Method == http.MethodPost && r.URL.Query().Get("op") == "forget":
		if err := l.Forget(r.URL.Query().Get("id")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case (p == "/files" || p == "/files/") && r.Method == http.MethodGet:
		l.home(w)
	default:
		http.NotFound(w, r)
	}
}

// register makes a passkey with the invite, or (req.Link) uses the invite
// to sign the browser in once without one; the web node then signs the
// member in.
func (l *Login) register(w http.ResponseWriter, r *http.Request, rp string, now time.Time) {
	var req light.SignInRequest
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	dec := func(s string) []byte { b, _ := passkey.B64.DecodeString(s); return b }
	clientData := dec(req.ClientData)
	if !req.Link {
		if passkey.ChallengeOf(clientData) != req.Challenge {
			http.Error(w, "the challenge doesn't match", http.StatusBadRequest)
			return
		}
		if err := l.challenges.Use(req.Challenge, now); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	var made light.Key
	err := l.update(func(st *state) error {
		if err := st.checkInvite(req.T, now); err != nil {
			return err
		}
		var k passkey.Key
		if req.Link {
			id := make([]byte, 18)
			rand.Read(id)
			k.ID = passkey.B64.EncodeToString(id)
		} else {
			var err error
			if k, err = passkey.Register(clientData, dec(req.AuthData), dec(req.SPKI), req.Alg, req.ID, req.Challenge, "https://"+rp, rp); err != nil {
				return err
			}
		}
		device := strings.TrimSpace(req.Name)
		if len(device) > 60 {
			device = device[:60]
		}
		if device == "" {
			device = "a browser"
		}
		made = light.Key{Key: k, RP: rp, Name: device, Link: req.Link, Added: now.Unix()}
		st.Keys = append(st.Keys, made)
		st.Invite = nil // it works once
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	verb := "made a passkey"
	if made.Link {
		verb = "signed in with their invite link"
	}
	l.logf("member: %s %s for the group login (%s, from %s)", l.Person(), verb, made.Name, r.Header.Get("X-Forwarded-For"))
	jsonOK(w, light.Registered{Person: l.Person(), Key: made})
}

// Run sends the web nodes the member and their passkeys when they change,
// and every SendEvery, once the member has made an invite.
func (l *Login) Run(ctx context.Context) {
	l.mu.Lock()
	l.kick = make(chan struct{}, 1)
	l.mu.Unlock()
	var lastSent []byte
	sent := map[string]time.Time{} // per web node, when it last took them
	var listed time.Time
	failed := false
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		changed := false
		if st, started, err := l.load(); err == nil && started && l.Person() != "" && !l.isBox() {
			b, _ := json.Marshal(light.People{Member: true, People: []light.Person{{Name: l.Person(), Web: true, Keys: st.Keys}}})
			if !bytes.Equal(b, lastSent) {
				lastSent, changed = b, true
			}
		}
		// Finding the web nodes asks every node, so only when there's
		// something to send.
		if lastSent != nil && (changed || failed || time.Since(listed) > SendEvery) {
			var ps light.People
			json.Unmarshal(lastSent, &ps)
			listed, failed = time.Now(), false
			for _, n := range l.WebNodes(ctx) {
				if !changed && time.Since(sent[n]) < SendEvery {
					continue
				}
				pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				err := light.SendPeople(pctx, l.Client, n, ps)
				cancel()
				if err != nil {
					l.logf("member: sending %s the group login's keys: %v", n, err)
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
		case <-l.kick:
		}
	}
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func page(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%s</title><body style="font-family:system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em;line-height:1.5">%s</body></html>`,
		html.EscapeString(title), body)
}

// home is where a member lands once signed in: their files are on their
// own node, not on the web.
func (l *Login) home(w http.ResponseWriter) {
	page(w, "Signed in", `<h1>You're signed in to the group</h1>
<p>As `+html.EscapeString(l.Person())+`. The group's pages and apps that ask who you are now know.</p>
<p>Your files and dashboard stay on your own node, opened from your own devices.</p>
<p><a href="/files/devices">Your devices</a> · <a href="#" id="out">Sign out</a></p>
<script>
document.getElementById("out").onclick = async e => {
  e.preventDefault();
  await fetch("`+light.Base+`/signout", {method: "POST", headers: {"X-Yggstore": "1"}});
  location.href = "/";
};
</script>`)
}

func devicesPage(w http.ResponseWriter) {
	page(w, "Your devices", `<h1>Your devices</h1>
<p>These are your passkeys for the group login, and the browsers signed in with them.</p>
<h2 style="font-size:1em">Ways in</h2><ul id="keys"><li>…</li></ul>
<h2 style="font-size:1em">Signed in</h2><ul id="sessions"><li>…</li></ul>
<p><a href="/files">Back</a> · <a href="#" id="out">Sign out here</a></p>
<p style="color:#666;font-size:.9em">A new phone or computer: passkeys often reach your other devices through your Apple, Google or Microsoft account.
If not, make a new invite on your node's dashboard and open it there.</p>
<script>
const esc = s => String(s).replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
const when = t => t ? new Date(t * 1000).toLocaleString() : "never";
const post = u => fetch(u, {method: "POST", headers: {"X-Yggstore": "1"}});
async function load() {
  const d = await (await fetch("?op=list", {cache: "no-store"})).json();
  document.getElementById("keys").innerHTML = d.passkeys.map(k => "<li><b>" + esc(k.name) + "</b>" + (k.link ? " (invite link)" : "") + ", made " + when(k.added) + ", last used " + when(k.used) +
    (d.passkeys.length > 1 ? " <button data-forget='" + esc(k.id) + "'>Forget</button>" : "") + "</li>").join("");
  document.getElementById("sessions").innerHTML = d.sessions.map(s => "<li><b>" + esc(s.device) + "</b>" + (s.me ? " (this browser)" : "") +
    (s.places && s.places.length ? ", at " + s.places.map(esc).join(", ") : "") +
    ", since " + when(s.started) + ", last used " + when(s.used) + " <button data-out='" + esc(s.hash) + "'>Sign out</button></li>").join("") || "<li>None</li>";
}
document.addEventListener("click", async e => {
  const f = e.target.dataset.forget, o = e.target.dataset.out;
  if (f && confirm("Forget this passkey? It won't sign you in any more.")) { const r = await post("?op=forget&id=" + encodeURIComponent(f)); if (!r.ok) alert(await r.text()); load(); }
  if (o) { await post("?op=signout&id=" + encodeURIComponent(o)); load(); }
});
document.getElementById("out").onclick = async e => { e.preventDefault(); await post("`+light.Base+`/signout"); location.href = "/"; };
load();
</script>`)
}
