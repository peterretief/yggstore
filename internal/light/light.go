// Package light lets people on a box (light clients) use it from any
// browser, with nothing to install (see docs/web-access.md). The group's
// web nodes serve my. and the group's domain and relay each request to the
// person's box over Yggdrasil (Relay); the box signs people in with
// passkeys and keeps their sessions (package hub).
package light

import (
	_ "embed"
	"net/http"
	"strings"
)

const (
	// Host is the label the group's web address goes under: my.example.org.
	Host = "my"
	// RelayHeader marks a request a web node relays. A box takes it only
	// from the group's web nodes.
	RelayHeader = "X-Yggstore-Relay"
	// BoxCookie says which box a browser's requests go to.
	BoxCookie = "ygg_box"
	// SessionCookie is the box's session.
	SessionCookie = "ygg_session"
	// Base is where signing in happens, on the box's files page.
	Base = "/files/light"
	// CopyHeader is how a box gives the relay the person's recovery ID
	// and the browser's half of its key ("ID.half"), on their files page.
	// The relay keeps it in CopyCookie, in the browser, so it can open
	// their recovery copy while the box is off.
	CopyHeader = "X-Yggstore-Copy"
	CopyCookie = "ygg_copy"
	// WorkPath is the recovery copy's page (package workhere's PagePath).
	WorkPath = "/_yggstore/work"
)

var (
	//go:embed signin.html
	signinHTML []byte
	//go:embed join.html
	joinHTML []byte
)

const csp = "default-src 'self'; script-src 'unsafe-inline' 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"

// ServeJoin serves an invite link's page, which makes a passkey with the
// invite (the token is in the page's address).
func ServeJoin(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.Write(joinHTML)
}

// ServeSignIn serves the page that signs someone in with a passkey. Both a
// box and a web node serve it: a web node when the browser doesn't yet say
// which box it's for, as the passkey will.
func ServeSignIn(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", csp)
	w.WriteHeader(status)
	w.Write(signinHTML)
}

// UserID is a passkey's user handle: the box and the person, so whichever
// web node receives a sign-in knows where to send it.
func UserID(box, person string) []byte { return []byte(box + "\x00" + person) }

// BoxOf is the box a user handle names.
func BoxOf(userID []byte) string {
	box, _, ok := strings.Cut(string(userID), "\x00")
	if !ok {
		return ""
	}
	return box
}
