package light

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

// JoinPath is an invite link's page on the box (hub.JoinPath).
const JoinPath = "/files/join"

// HubPort is where a box's hub answers.
const HubPort = "7490"

// Relay serves my. and the group's domain on a web node: it passes each
// request to the person's box over Yggdrasil and the answer back, keeping
// nothing. Which box comes from the invite link the first time, from the
// passkey when signing in, and after that from a cookie.
type Relay struct {
	Peers func() []peers.Peer
	// Transport reaches boxes over Yggdrasil (http.DefaultTransport if nil).
	Transport http.RoundTripper
	// BoxURL is where the box at ip answers (tests change it).
	BoxURL func(ip string) string
	Log    func(format string, args ...any)

	proxy *httputil.ReverseProxy
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
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			client := pr.In.Header.Get("CF-Connecting-IP") // the browser, as Cloudflare saw it
			if client == "" {
				client, _, _ = net.SplitHostPort(pr.In.RemoteAddr)
			}
			pr.Out.Header.Set("X-Forwarded-For", client)
			pr.Out.Header.Set(RelayHeader, "1")
		},
		Transport:     rl.Transport,
		FlushInterval: -1, // pass downloads and uploads on as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rl.logf("light: %s: %v", r.Context().Value(targetKey{}), err)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Your box isn't answering</title><body style="font-family:system-ui,sans-serif;max-width:36em;margin:3em auto;padding:0 1em;line-height:1.5">
<h1>Your box isn't answering</h1><p>It may be off, or its internet may be down. Your files are safe: try again in a while, or ask the box's owner.</p></body>`)
		},
	}
}

func (rl *Relay) logf(format string, args ...any) {
	if rl.Log != nil {
		rl.Log(format, args...)
	}
}

// box is the address of the box called name, if the group has one.
func (rl *Relay) box(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	for _, p := range rl.Peers() {
		if strings.EqualFold(p.Name, name) {
			return p.IP(), true
		}
	}
	return "", false
}

func setBox(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: BoxCookie, Value: name, Path: "/", MaxAge: 400 * 24 * 3600,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// PersonHost reports whether host is the name of someone on a box (such
// as kath.example.org), whose /files their box serves through the relay.
func PersonHost(list []peers.Peer, host string) (box peers.Peer, ok bool) {
	p, pn, ok := peers.NameHolder(list, hostOnly(host))
	return p, ok && pn.Person != ""
}

func (rl *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rl.init()
	r.Header.Del(RelayHeader) // only this relay sets it
	if box, ok := PersonHost(rl.Peers(), r.Host); ok {
		// Someone's own address: their box, whoever asks.
		if p := r.URL.Path; p != "/files" && !strings.HasPrefix(p, "/files/") {
			http.NotFound(w, r)
			return
		}
		rl.forward(w, r, box.IP())
		return
	}
	name := ""
	if c, err := r.Cookie(BoxCookie); err == nil {
		name = c.Value
	}
	switch p := r.URL.Path; {
	case p == "/" || p == "":
		if _, ok := rl.box(name); ok {
			http.Redirect(w, r, "/files", http.StatusFound)
			return
		}
		ServeSignIn(w, http.StatusOK)
		return
	case p == JoinPath && r.URL.Query().Get("box") != "":
		name = r.URL.Query().Get("box")
		if _, ok := rl.box(name); !ok {
			http.Error(w, "This invite link names a box the group doesn't have. Ask the box's owner for a new one.", http.StatusNotFound)
			return
		}
		setBox(w, name)
	case p == Base+"/challenge" && name == "":
		// Not yet known which box: any box takes a challenge made here.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]string{"challenge": passkey.NewChallenge(time.Now()), "rp": strings.ToLower(hostOnly(r.Host))})
		return
	case p == Base+"/signin" && r.Method == http.MethodPost:
		// The passkey says which box it's for.
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req struct {
			UserHandle string `json:"user_handle"`
		}
		json.Unmarshal(body, &req)
		if uh, err := passkey.B64.DecodeString(req.UserHandle); err == nil && BoxOf(uh) != "" {
			if _, ok := rl.box(BoxOf(uh)); ok {
				name = BoxOf(uh)
				setBox(w, name)
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	case p != "/files" && !strings.HasPrefix(p, "/files/"):
		http.NotFound(w, r)
		return
	}
	ip, ok := rl.box(name)
	if !ok {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	rl.forward(w, r, ip)
}

// forward passes r to the box at ip.
func (rl *Relay) forward(w http.ResponseWriter, r *http.Request, ip string) {
	u := "http://" + net.JoinHostPort(ip, HubPort)
	if rl.BoxURL != nil {
		u = rl.BoxURL(ip)
	}
	target, err := url.Parse(u)
	if err != nil {
		http.Error(w, html.EscapeString(err.Error()), http.StatusInternalServerError)
		return
	}
	rl.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, target)))
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}
