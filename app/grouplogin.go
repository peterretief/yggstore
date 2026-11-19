package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/light"
)

// Signing in with the group (yggstore's docs/login.md): anyone with a place
// in the group, a member or someone on a box, signs in here with their
// passkey at the group's login (my.DOMAIN), and needs no account of the
// app's own. The first time, they get an account with the app's group role
// (-group-role, client unless said), which an admin can change on People.
// Accounts with passwords go on working beside it.
//
// It is OpenID Connect's code flow with PKCE: the app is known to the login
// by its own address (-site), and checks the ID token's signature against
// the login's published keys.

const (
	groupLoginPath = base + "group-login"
	groupBackPath  = base + "group-login/back"
	groupCookie    = "group_login"
)

type groupLogin struct {
	issuer string // https://my.example.org
	role   string // what a newcomer's account is
	client *http.Client

	mu    sync.Mutex
	conf  *oidcConf
	keys  []light.JWK
	until time.Time // when to ask for the configuration and keys again
}

type oidcConf struct {
	Issuer    string `json:"issuer"`
	Authorize string `json:"authorization_endpoint"`
	Token     string `json:"token_endpoint"`
	Keys      string `json:"jwks_uri"`
}

func (g *groupLogin) get(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// config is the login's configuration and keys, asked for again hourly,
// or at once (fresh) when a token names a key not seen yet.
func (g *groupLogin) config(ctx context.Context, fresh bool) (*oidcConf, []light.JWK, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conf != nil && !fresh && time.Now().Before(g.until) {
		return g.conf, g.keys, nil
	}
	var c oidcConf
	if err := g.get(ctx, g.issuer+"/.well-known/openid-configuration", &c); err != nil {
		return nil, nil, err
	}
	if c.Issuer != g.issuer {
		return nil, nil, fmt.Errorf("the login calls itself %q, not %q", c.Issuer, g.issuer)
	}
	var ks struct{ Keys []light.JWK }
	if err := g.get(ctx, c.Keys, &ks); err != nil {
		return nil, nil, err
	}
	g.conf, g.keys, g.until = &c, ks.Keys, time.Now().Add(time.Hour)
	return g.conf, g.keys, nil
}

func randomText() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// clientID is the app's own address, which is what the login knows it by.
func (a *app) clientID() string { return strings.ToLower(a.site) }

func (a *app) groupBack() string { return "https://" + a.clientID() + groupBackPath }

// sealed is a short-lived value only this app could have made.
func (a *app) sealCookie(v string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte("group login\x00" + v))
	return v + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *app) openCookie(c string) (string, bool) {
	i := strings.LastIndexByte(c, '.')
	if i < 0 {
		return "", false
	}
	return c[:i], hmac.Equal([]byte(a.sealCookie(c[:i])), []byte(c))
}

// groupStart sends the browser to the group's login.
func (a *app) groupStart(w http.ResponseWriter, r *http.Request) {
	g := a.group
	conf, _, err := g.config(r.Context(), false)
	if err != nil {
		log.Printf("group login: %v", err)
		page(w, r, http.StatusBadGateway, "login", a.loginForm("The group's sign-in can't be reached just now. Try again in a little while."))
		return
	}
	state, nonce, verifier := randomText(), randomText(), randomText()
	ends := time.Now().Add(10 * time.Minute)
	http.SetCookie(w, &http.Cookie{Name: groupCookie, Path: base, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		Expires: ends, Value: a.sealCookie(state + "|" + nonce + "|" + verifier + "|" + strconv.FormatInt(ends.Unix(), 10))})
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {a.clientID()}, "redirect_uri": {a.groupBack()},
		"scope": {"openid email profile"}, "state": {state}, "nonce": {nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, conf.Authorize+"?"+q.Encode(), http.StatusFound)
}

var errGroupLogin = errors.New("that sign-in didn't work")

// groupBackHere is where the login sends the browser back, with a code.
func (a *app) groupBackHere(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string, err error) {
		if err != nil {
			log.Printf("group login: %v", err)
		}
		http.SetCookie(w, &http.Cookie{Name: groupCookie, Path: base, MaxAge: -1, HttpOnly: true, Secure: true})
		page(w, r, http.StatusForbidden, "login", a.loginForm(msg))
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		fail("You didn't sign in with the group. You can sign in here instead, or try again.", nil)
		return
	}
	c, err := r.Cookie(groupCookie)
	if err != nil {
		fail("That sign-in took too long, or came from another browser. Try again.", nil)
		return
	}
	v, ok := a.openCookie(c.Value)
	parts := strings.Split(v, "|")
	if !ok || len(parts) != 4 {
		fail("That sign-in didn't work. Try again.", errors.New("a changed cookie"))
		return
	}
	state, nonce, verifier := parts[0], parts[1], parts[2]
	ends, _ := strconv.ParseInt(parts[3], 10, 64)
	if time.Now().Unix() > ends || !hmac.Equal([]byte(state), []byte(q.Get("state"))) {
		fail("That sign-in took too long, or came from another browser. Try again.", nil)
		return
	}
	email, err := a.groupWho(r.Context(), q.Get("code"), verifier, nonce)
	if err != nil {
		fail("That sign-in didn't work. Try again.", err)
		return
	}
	if email == "" {
		fail("You're signed in with the group, but it didn't say who you are. Try again, or sign in here instead.", nil)
		return
	}
	switch role := a.accountRole(email); role {
	case "", roleApplied: // new here, or waiting to be let in: the group knows them
		x, _ := a.getAccount(email)
		x.Name, x.Role = email, a.group.role
		if err := a.putAccount(x); err != nil {
			fail("That sign-in didn't work. Try again.", err)
			return
		}
		log.Printf("group login: %s is new here, as %s", email, a.group.role)
	}
	http.SetCookie(w, &http.Cookie{Name: groupCookie, Path: base, MaxAge: -1, HttpOnly: true, Secure: true})
	if err := a.startSession(w, r, email); err != nil {
		fail("That sign-in didn't work. Try again.", err)
		return
	}
	http.Redirect(w, r, base, http.StatusSeeOther)
}

// groupWho swaps a code for an ID token, checks it, and gives the person's
// address in the group ("" if they have none).
func (a *app) groupWho(ctx context.Context, code, verifier, nonce string) (string, error) {
	g := a.group
	conf, keys, err := g.config(ctx, false)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.groupBack()},
		"client_id": {a.clientID()}, "code_verifier": {verifier}}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, conf.Token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK || tok.IDToken == "" {
		return "", fmt.Errorf("the token: %s %s %v", resp.Status, tok.Error, err)
	}
	claims, ok := light.VerifyToken(tok.IDToken, keys)
	if !ok { // a key newer than ours?
		if _, keys, err = g.config(ctx, true); err != nil {
			return "", err
		}
		if claims, ok = light.VerifyToken(tok.IDToken, keys); !ok {
			return "", errors.New("the ID token isn't signed by the login")
		}
	}
	str := func(k string) string { s, _ := claims[k].(string); return s }
	exp, _ := claims["exp"].(float64)
	audOK := str("aud") == a.clientID()
	if list, isList := claims["aud"].([]any); isList {
		for _, x := range list {
			audOK = audOK || x == a.clientID()
		}
	}
	switch {
	case str("iss") != g.issuer:
		return "", fmt.Errorf("the token is from %q", str("iss"))
	case !audOK:
		return "", errors.New("the token is for another site")
	case time.Now().Unix() > int64(exp):
		return "", errors.New("the token has ended")
	case !hmac.Equal([]byte(str("nonce")), []byte(nonce)):
		return "", errors.New("the token's nonce isn't ours")
	}
	if v, _ := claims["email_verified"].(bool); v && str("email") != "" {
		return cleanEmail(str("email")), nil
	}
	// Someone on a box with no name in the group yet: known by their box
	// and their name on it, at the group's internal domain, where no mail
	// goes (see noMailbox). Given a name later, they're a new account.
	return g.unnamed(str("sub")), nil
}

// unnamed is the account for "box/person", someone on a box with no name
// in the group: person.box@GROUP.internal ("" if sub isn't like that).
func (g *groupLogin) unnamed(sub string) string {
	box, person, ok := strings.Cut(strings.ToLower(sub), "/")
	label := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
				b.WriteByte('-')
			}
		}
		return strings.Trim(b.String(), "-")
	}
	box, person = label(box), label(person)
	host := strings.TrimPrefix(strings.TrimPrefix(g.issuer, "https://"), "http://")
	host, _, _ = strings.Cut(host, ":")
	group, _, _ := strings.Cut(strings.TrimPrefix(host, "my."), ".")
	if !ok || box == "" || person == "" || group == "" {
		return ""
	}
	return person + "." + box + "@" + group + ".internal"
}

// noMailbox says whether an account is someone known only by their box
// (unnamed): mail to it can't be delivered, so it's left out of emails
// rather than stop them going to everyone else.
func noMailbox(addr string) bool {
	local, domain, _ := strings.Cut(addr, "@")
	return strings.HasSuffix(domain, ".internal") && strings.Contains(local, ".")
}
