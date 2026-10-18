package light

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

// JoinPath is an invite link's page on the box (hub.JoinPath).
const JoinPath = "/files/join"

// HubPort is where a box's hub answers.
const HubPort = "7490"

// Relay serves my. and the group's domain, and people's own addresses, on
// a web node. It signs people in itself (Store), so signing in works while
// their box is off, and passes each signed-in request to the person's box
// over Yggdrasil, naming them, and the answer back. Invites go to the box,
// which holds them: which box comes from the person's own address, or for
// my. from the invite link and then a cookie.
type Relay struct {
	Peers func() []peers.Peer
	// Signin keeps the group's people's keys and the sessions.
	Signin *Store
	// Transport reaches boxes over Yggdrasil (http.DefaultTransport if nil).
	Transport http.RoundTripper
	// BoxURL is where the box at ip answers (tests change it).
	BoxURL func(ip string) string
	Log    func(format string, args ...any)

	proxy *httputil.ReverseProxy
	mu    sync.Mutex
	down  map[string]bool      // boxes that didn't answer, by address, until they do
	heard map[string]time.Time // when each box last answered
}

// QuietFor is how long a box may go unheard before the relay checks it's
// there (within CheckWait) before passing a request on, so that a box
// that's off is found at once, not after a long wait for it.
var QuietFor, CheckWait = time.Minute, 4 * time.Second

func (rl *Relay) answered(ip string) {
	rl.mu.Lock()
	if rl.heard == nil {
		rl.heard = map[string]time.Time{}
	}
	rl.heard[ip] = time.Now()
	rl.mu.Unlock()
}

// there is whether the box at ip answers: at once if it has lately.
func (rl *Relay) there(ctx context.Context, ip string) bool {
	rl.mu.Lock()
	quiet := time.Since(rl.heard[ip]) > QuietFor
	rl.mu.Unlock()
	if !quiet {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, CheckWait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rl.boxURL(ip)+Base+"/ping", nil)
	resp, err := rl.client().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	rl.answered(ip)
	return true
}

// ProbeEvery is how often the relay asks a box that didn't answer whether
// it's back. Until then, its people's requests don't wait for it.
var ProbeEvery = 10 * time.Second

type ipKey struct{}

func (rl *Relay) isDown(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.down[ip]
}

// markDown notes that the box at ip didn't answer, and asks it every
// ProbeEvery until it does.
func (rl *Relay) markDown(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.down[ip] {
		return
	}
	if rl.down == nil {
		rl.down = map[string]bool{}
	}
	rl.down[ip] = true
	go func() {
		c := rl.client()
		c.Timeout = 5 * time.Second
		for {
			time.Sleep(ProbeEvery)
			if resp, err := c.Get(rl.boxURL(ip) + Base + "/ping"); err == nil {
				resp.Body.Close()
				rl.answered(ip)
				rl.mu.Lock()
				delete(rl.down, ip)
				rl.mu.Unlock()
				rl.logf("light: the box at %s answers again", ip)
				return
			}
		}
	}()
}

// offline answers for a box that's off: a browser opening a page with the
// key half goes to the recovery copy's page; anything else is told the box
// isn't answering (the files page then reloads, and so goes there too).
func (rl *Relay) offline(w http.ResponseWriter, r *http.Request) {
	nav := r.Method == http.MethodGet && (r.URL.Path == "/files" || r.Header.Get("Sec-Fetch-Mode") == "navigate")
	if c, err := r.Cookie(CopyCookie); err == nil && validCopy(c.Value) && nav {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, WorkPath+"?off=1#"+c.Value, http.StatusFound)
		return
	}
	boxOff(w)
}

type targetKey struct{}

func (rl *Relay) init() {
	if rl.proxy != nil {
		return
	}
	rl.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := pr.In.Context().Value(targetKey{}).(*url.URL)
			pr.SetURL(target)
			pr.Out.Host = target.Host
			rl.headers(pr.Out, pr.In)
		},
		Transport:     rl.Transport,
		FlushInterval: -1, // pass downloads and uploads on as they come
		ModifyResponse: func(resp *http.Response) error {
			rl.answered(resp.Request.Context().Value(ipKey{}).(string))
			// The box's key half goes to the browser, which keeps it.
			v := resp.Header.Get(CopyHeader)
			resp.Header.Del(CopyHeader)
			if validCopy(v) {
				if c, err := resp.Request.Cookie(CopyCookie); err != nil || c.Value != v {
					resp.Header.Add("Set-Cookie", copyCookie(v, copyCookieFor).String())
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rl.logf("light: %s: %v", r.Context().Value(targetKey{}), err)
			if r.Context().Err() == nil { // not the browser giving up
				rl.markDown(r.Context().Value(ipKey{}).(string))
			}
			rl.offline(w, r)
		},
	}
}

func boxOff(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Your box isn't answering</title><body style="font-family:system-ui,sans-serif;max-width:36em;margin:3em auto;padding:0 1em;line-height:1.5">
<h1>Your box isn't answering</h1><p>It may be off, or its internet may be down. Your files are safe: try again in a while, or ask the box's owner.</p></body>`)
}

// headers sets what the box is told about a relayed request in.
func (rl *Relay) headers(out, in *http.Request) {
	out.Header.Set("X-Forwarded-Host", in.Host)
	out.Header.Set("X-Forwarded-Proto", "https")
	client := in.Header.Get("CF-Connecting-IP") // the browser, as Cloudflare saw it
	if client == "" {
		client, _, _ = net.SplitHostPort(in.RemoteAddr)
	}
	out.Header.Set("X-Forwarded-For", client)
	out.Header.Set(RelayHeader, "1")
	if who, ok := in.Context().Value(personKey{}).(string); ok {
		out.Header.Set(PersonHeader, who)
	} else {
		out.Header.Del(PersonHeader)
	}
}

type personKey struct{}

func (rl *Relay) logf(format string, args ...any) {
	if rl.Log != nil {
		rl.Log(format, args...)
	}
}

// box is the box called name, if the group has one.
func (rl *Relay) box(name string) (peers.Peer, bool) {
	if name == "" {
		return peers.Peer{}, false
	}
	for _, p := range rl.Peers() {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return peers.Peer{}, false
}

func (rl *Relay) boxURL(ip string) string {
	if rl.BoxURL != nil {
		return rl.BoxURL(ip)
	}
	return "http://" + net.JoinHostPort(ip, HubPort)
}

func setBox(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: BoxCookie, Value: name, Path: "/", MaxAge: 400 * 24 * 3600,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// copyCookieFor is how long a browser keeps the key half: as long as
// browsers allow, renewed whenever the box is on.
const copyCookieFor = 400 * 24 * 60 * 60

var copyValue = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}\.[A-Za-z0-9_-]{16,128}$`)

func validCopy(v string) bool { return copyValue.MatchString(v) }

func copyCookie(v string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: CopyCookie, Value: v, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

func sessionCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: token, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// PersonHost reports whether host is the name of someone on a box (such
// as kath.example.org), whose /files their box serves through the relay.
func PersonHost(list []peers.Peer, host string) (box peers.Peer, ok bool) {
	box, _, ok = personHost(list, host)
	return box, ok
}

func personHost(list []peers.Peer, host string) (peers.Peer, string, bool) {
	p, pn, ok := peers.NameHolder(list, hostOnly(host))
	return p, pn.Person, ok && pn.Person != ""
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func (rl *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rl.init()
	r.Header.Del(RelayHeader) // only this relay sets them
	r.Header.Del(PersonHeader)
	host := strings.ToLower(strings.TrimSuffix(hostOnly(r.Host), "."))
	p := r.URL.Path
	// Someone's own address: their box, and only them.
	own, only, isOwn := personHost(rl.Peers(), host)
	if !isOwn && rl.serveLogin(w, r, host) {
		return
	}
	if !isOwn && (p == "/" || p == "") {
		if _, ok := rl.signedIn(w, r, host, ""); ok {
			http.Redirect(w, r, "/files", http.StatusFound)
			return
		}
		ServeSignIn(w, http.StatusOK)
		return
	}
	if p != "/files" && !strings.HasPrefix(p, "/files/") {
		http.NotFound(w, r)
		return
	}
	// Other web pages open in the browser can't add this header.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Yggstore") != "1" {
		http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
		return
	}
	switch {
	case isOwn && p == backPath && r.Method == http.MethodGet:
		rl.loginBack(w, r, host, only, own)
		return
	case isOwn && p == backPath+"/start" && r.Method == http.MethodGet:
		if !rl.startLogin(w, r, host, false) {
			http.Redirect(w, r, "/files", http.StatusFound)
		}
		return
	case p == Base+"/challenge":
		jsonOK(w, map[string]string{"challenge": passkey.NewChallenge(time.Now()), "rp": host})
		return
	case p == Base+"/signin" && r.Method == http.MethodPost:
		rl.signIn(w, r, host, only)
		return
	case p == Base+"/signout":
		if c, err := r.Cookie(SessionCookie); err == nil && rl.Signin != nil {
			rl.Signin.end(tokenHash(c.Value), true)
		}
		http.SetCookie(w, sessionCookie("", -1))
		http.SetCookie(w, copyCookie("", -1)) // this browser no longer opens the copy
		w.WriteHeader(http.StatusNoContent)
		return
	case p == JoinPath || p == Base+"/begin" || p == Base+"/register":
		// Invites are the box's.
		box := own
		if !isOwn {
			name := r.URL.Query().Get("box")
			if p == JoinPath && name != "" {
				if _, ok := rl.box(name); !ok {
					http.Error(w, "This invite link names a box the group doesn't have. Ask the box's owner for a new one.", http.StatusNotFound)
					return
				}
				setBox(w, name)
			} else if c, err := r.Cookie(BoxCookie); err == nil {
				name = c.Value
			}
			var ok bool
			if box, ok = rl.box(name); !ok {
				http.Error(w, "Open the invite link again: this browser doesn't say which box it's for.", http.StatusBadRequest)
				return
			}
		}
		if p == Base+"/register" {
			rl.register(w, r, host, box)
			return
		}
		rl.forward(w, r, box.IP())
		return
	}
	ss, ok := rl.signedIn(w, r, host, only)
	if !ok {
		if r.Method == http.MethodGet && (p == "/files" || p == "/files/") && r.URL.RawQuery == "" {
			// At their own address, my. may know them already.
			if isOwn && rl.startLogin(w, r, host, true) {
				return
			}
			ServeSignIn(w, http.StatusOK)
			return
		}
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if p == "/files/devices" && rl.devices(w, r, ss) {
		return
	}
	box, ok := rl.box(ss.Box)
	if !ok {
		http.Error(w, "the group no longer has your box", http.StatusNotFound)
		return
	}
	rl.forward(w, r.WithContext(context.WithValue(r.Context(), personKey{}, ss.Person)), box.IP())
}

// signedIn is the session r's cookie holds at host, for only if set. It
// keeps a session in use going.
func (rl *Relay) signedIn(w http.ResponseWriter, r *http.Request, host, only string) (Session, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || rl.Signin == nil {
		return Session{}, false
	}
	now := time.Now()
	ss, ok := rl.Signin.session(r.Context(), c.Value, host, now)
	if !ok || (only != "" && !strings.EqualFold(ss.Person, only)) {
		return Session{}, false
	}
	if rl.Signin.touch(ss, now) {
		http.SetCookie(w, sessionCookie(c.Value, int(ss.For().Seconds())))
	}
	return ss, true
}

// SignInRequest is what the sign-in and invite pages send.
type SignInRequest struct {
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

// Registered is the box's answer to an invite used: the key it now has.
type Registered struct {
	Person string `json:"person"`
	Key    Key    `json:"key"`
}

// Seen is what a web node tells a box about a key after a sign-in: that it
// was used, its wrong PINs in a row, and a PIN chosen for it.
type Seen struct {
	Key      string `json:"key"`
	Used     bool   `json:"used,omitempty"`
	PINFails int    `json:"pin_fails"`
	PIN      string `json:"pin,omitempty"` // a PIN hash, for a key with none yet
}

// SeenPath is where a box hears of a key's use.
const SeenPath = Base + "/seen"

func dec(s string) []byte { b, _ := passkey.B64.DecodeString(s); return b }

// signIn checks a passkey or browser key (and its PIN) and starts a
// session.
func (rl *Relay) signIn(w http.ResponseWriter, r *http.Request, host, only string) {
	if rl.Signin == nil {
		http.Error(w, "this web node doesn't sign people in", http.StatusServiceUnavailable)
		return
	}
	var req SignInRequest
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	now := time.Now()
	origin := "https://" + host
	browser := req.Kind == KindBrowser
	clientData := dec(req.ClientData)
	if !browser && passkey.ChallengeOf(clientData) != req.Challenge {
		http.Error(w, "the challenge doesn't match", http.StatusBadRequest)
		return
	}
	if err := rl.Signin.challenges.Use(req.Challenge, now); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := rl.checkKey(req, host, only, origin)
	if err == nil && browser {
		err = rl.checkPIN(r.Context(), req, host, &f)
	}
	if errors.Is(err, passkey.ErrNeedPIN) {
		http.Error(w, err.Error(), http.StatusPreconditionRequired) // the page asks for a new PIN
		return
	}
	if err != nil {
		rl.logf("light: sign-in refused at %s (from %s): %v", host, r.Header.Get("CF-Connecting-IP"), err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	token, ss := rl.Signin.newSession(f, host, now)
	go rl.tellBox(f.Box, host, f.Person.Name, Seen{Key: f.Key.ID, Used: true})
	rl.logf("light: %s on %s signed in at %s (%s)", f.Person.Name, f.Box, host, f.Key.Name)
	http.SetCookie(w, sessionCookie(token, int(ss.For().Seconds())))
	answer := map[string]string{"person": f.Person.Name}
	if !own(rl.Peers(), host) {
		setBox(w, f.Box)
	} else if next := rl.adoptLink(ss, host); next != "" {
		answer["next"] = next // signed in at my. too
	}
	jsonOK(w, answer)
}

func own(list []peers.Peer, host string) bool { _, _, ok := personHost(list, host); return ok }

// checkKey finds the key a sign-in names and checks its signature.
func (rl *Relay) checkKey(req SignInRequest, host, only, origin string) (found, error) {
	f, ok := rl.Signin.key(req.ID)
	if !ok {
		return f, errNoKey
	}
	k := f.Key
	browser := req.Kind == KindBrowser
	if browser != (k.Kind == KindBrowser) {
		return f, errors.New("That key isn't the kind it says it is.")
	}
	if k.RP != "" && !strings.EqualFold(k.RP, host) {
		return f, fmt.Errorf("This key is for %s.", k.RP)
	}
	var err error
	if browser {
		err = passkey.VerifyBrowser(k.SPKI, origin, req.Challenge, dec(req.Signature))
	} else {
		// The device's counter isn't checked: the web nodes don't share one.
		_, err = k.Key.Verify(dec(req.ClientData), dec(req.AuthData), dec(req.Signature), req.Challenge, origin, host)
	}
	if err != nil {
		return f, err
	}
	if only != "" && !strings.EqualFold(f.Person.Name, only) {
		return f, fmt.Errorf("This key is someone else's, not for %s.", host)
	}
	if !f.Person.Web {
		return f, errors.New("Opening your files through the web is turned off for now. Ask the box's owner.")
	}
	return f, nil
}

// checkPIN checks a browser key's PIN, counting wrong ones. A key made
// before PINs gets the one chosen, if its box says yes.
func (rl *Relay) checkPIN(ctx context.Context, req SignInRequest, host string, f *found) error {
	k := f.Key
	switch {
	case f.Fails >= passkey.MaxPINFails:
		return passkey.ErrPINLocked
	case k.PIN == "" && req.NewPIN == "":
		return passkey.ErrNeedPIN
	case k.PIN == "":
		if err := passkey.PINOK(req.NewPIN); err != nil {
			return err
		}
		hash := passkey.PINHash(req.NewPIN)
		if err := rl.tellBox(f.Box, host, f.Person.Name, Seen{Key: k.ID, PIN: hash}); err != nil {
			rl.logf("light: %s's new PIN: %v", f.Person.Name, err)
			return errors.New("Your box has to be on to choose a PIN, and it isn't answering. Try again in a while.")
		}
		rl.Signin.setPIN(f.Box, k.ID, hash)
	case !passkey.PINMatches(k.PIN, req.PIN):
		n := f.Fails + 1
		rl.Signin.setFails(k.ID, n, true)
		go rl.tellBox(f.Box, host, f.Person.Name, Seen{Key: k.ID, PINFails: n})
		return passkey.WrongPIN(passkey.MaxPINFails - n)
	}
	if f.Fails > 0 {
		rl.Signin.setFails(k.ID, 0, true)
	}
	return nil
}

// tellBox sends a person's box what happened with one of their keys.
func (rl *Relay) tellBox(box, host, person string, seen Seen) error {
	b, ok := rl.box(box)
	if !ok {
		return errors.New("no such box")
	}
	body, _ := json.Marshal(seen)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rl.boxURL(b.IP())+SeenPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Yggstore", "1")
	req.Header.Set("X-Forwarded-Host", host)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set(RelayHeader, "1")
	req.Header.Set(PersonHeader, person)
	resp, err := rl.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (rl *Relay) client() *http.Client {
	if rl.Transport != nil {
		return &http.Client{Transport: rl.Transport}
	}
	return http.DefaultClient
}

// register passes an invite's use to the box, which makes the key, and
// signs the person in with it.
func (rl *Relay) register(w http.ResponseWriter, r *http.Request, host string, box peers.Peer) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, rl.boxURL(box.IP())+Base+"/register", bytes.NewReader(body))
	if err != nil {
		http.Error(w, html.EscapeString(err.Error()), http.StatusInternalServerError)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("X-Yggstore", "1")
	rl.headers(out, r)
	resp, err := rl.client().Do(out)
	if err != nil {
		rl.logf("light: %s: %v", box.Name, err)
		boxOff(w)
		return
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var reg Registered
	if resp.StatusCode != http.StatusOK || json.Unmarshal(answer, &reg) != nil || reg.Key.ID == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		w.Write(answer)
		return
	}
	if rl.Signin == nil {
		http.Error(w, "this web node doesn't sign people in", http.StatusServiceUnavailable)
		return
	}
	// The box sends every web node the new key too; this one needn't wait.
	f := found{Box: box.Name, Person: Person{Name: reg.Person, Web: true}, Key: reg.Key}
	rl.Signin.addKey(f)
	token, ss := rl.Signin.newSession(f, host, time.Now())
	rl.logf("light: %s on %s made a key at %s (%s)", reg.Person, box.Name, host, reg.Key.Name)
	http.SetCookie(w, sessionCookie(token, int(ss.For().Seconds())))
	reply := map[string]string{"person": reg.Person}
	if own(rl.Peers(), host) {
		if next := rl.adoptLink(ss, host); next != "" {
			reply["next"] = next
		}
	}
	jsonOK(w, reply)
}

// devices answers what the web node knows of the person's devices: the
// list (keys and sessions) and signing a session out. Forgetting a key is
// the box's.
func (rl *Relay) devices(w http.ResponseWriter, r *http.Request, me Session) bool {
	switch op := r.URL.Query().Get("op"); {
	case op == "list" && r.Method == http.MethodGet:
		f, _ := rl.Signin.key(me.Key)
		type sess struct {
			Session
			Me     bool     `json:"me,omitempty"`
			Places []string `json:"places"` // the addresses it's signed in at
		}
		// One per browser: a session at another address that came from
		// one at my. is a place of that one.
		all := rl.Signin.sessionsOf(me.Box, me.Person)
		at := map[string]int{}
		ss := []sess{}
		for _, s := range all {
			if s.Via == "" {
				at[s.Hash] = len(ss)
				ss = append(ss, sess{s, s.Hash == me.Hash, []string{s.Host}})
			}
		}
		for _, s := range all {
			if i, ok := at[s.Via]; ok {
				ss[i].Places = append(ss[i].Places, s.Host)
				ss[i].Me = ss[i].Me || s.Hash == me.Hash
			} else if s.Via != "" {
				ss = append(ss, sess{s, s.Hash == me.Hash, []string{s.Host}})
			}
		}
		keys := []Key{}
		for _, k := range f.Person.Keys {
			k.PIN = "" // the browser has no business with it
			keys = append(keys, k)
		}
		jsonOK(w, map[string]any{"person": me.Person, "passkeys": keys, "sessions": ss})
		return true
	case op == "signout" && r.Method == http.MethodPost:
		id := r.URL.Query().Get("id")
		for _, s := range rl.Signin.sessionsOf(me.Box, me.Person) {
			if s.Hash == id {
				rl.Signin.end(id, true)
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// forward passes r to the box at ip.
func (rl *Relay) forward(w http.ResponseWriter, r *http.Request, ip string) {
	target, err := url.Parse(rl.boxURL(ip))
	if err != nil {
		http.Error(w, html.EscapeString(err.Error()), http.StatusInternalServerError)
		return
	}
	if rl.isDown(ip) {
		rl.offline(w, r)
		return
	}
	if !rl.there(r.Context(), ip) {
		if r.Context().Err() == nil { // not the browser giving up
			rl.logf("light: the box at %s doesn't answer", ip)
			rl.markDown(ip)
		}
		rl.offline(w, r)
		return
	}
	ctx := context.WithValue(context.WithValue(r.Context(), targetKey{}, target), ipKey{}, ip)
	rl.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}
