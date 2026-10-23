package light

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

// The group's login (docs/login.md): my. is an OpenID Connect provider
// for the group's own addresses only. Someone signed in at my. is signed
// in, without another sign-in, at any address under the group's domain
// that asks: a person's files at their own address, the group's pages and,
// later, its apps. Every web node runs it and signs with its own key, and
// codes are shared between them like sessions, so it works with any one
// machine off.

// Where the login answers, on my.
const (
	LoginPath     = "/login"
	authorizePath = LoginPath + "/authorize"
	tokenPath     = LoginPath + "/token"
	userinfoPath  = LoginPath + "/userinfo"
	keysPath      = LoginPath + "/keys"
	adoptPath     = LoginPath + "/adopt"
	signoutPath   = LoginPath + "/signout"
	discoveryPath = "/.well-known/openid-configuration"
	// backPath is where my. sends someone back to at their own address.
	backPath = "/files/login"
	// loginCookie keeps, at their own address, the state and PKCE
	// verifier of a login under way.
	loginCookie = "ygg_login"
)

// CodeFor is how long a code works; TokenFor how long the tokens it gives.
var CodeFor, TokenFor = time.Minute, time.Hour

// Ticket is a one-time code: an authorization code for an app, or the
// adopt ticket that signs someone in at my. after they signed in at their
// own address.
type Ticket struct {
	Hash      string `json:"hash"` // of the code, which only the browser sees
	Kind      string `json:"kind"` // "code" or "adopt"
	Session   string `json:"session"`
	Client    string `json:"client,omitempty"`
	Redirect  string `json:"redirect"`
	Challenge string `json:"challenge,omitempty"` // PKCE
	Nonce     string `json:"nonce,omitempty"`
	Exp       int64  `json:"exp"`
}

// JWK is a signing key's public half, as the login publishes it.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

func (k JWK) public() (*ecdsa.PublicKey, bool) {
	x, err1 := passkey.B64.DecodeString(k.X)
	y, err2 := passkey.B64.DecodeString(k.Y)
	if err1 != nil || err2 != nil || k.Kty != "EC" || k.Crv != "P-256" {
		return nil, false
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, false
	}
	return pub, true
}

type signer struct {
	key *ecdsa.PrivateKey
	jwk JWK
}

// loginSigner is this web node's signing key, made the first time.
func (s *Store) loginSigner() (*signer, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.signer != nil {
		return s.signer, nil
	}
	path := filepath.Join(s.Dir, "login-key.pem")
	var key *ecdsa.PrivateKey
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: not a key", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); !ok {
			return nil, fmt.Errorf("%s: not an ECDSA key", path)
		}
	} else {
		var err error
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			return nil, err
		}
		if err := atomicfile.Replace(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
	}
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	h := sha256.Sum256(spki)
	s.signer = &signer{key: key, jwk: JWK{Kty: "EC", Crv: "P-256", Kid: hex.EncodeToString(h[:8]), Use: "sig", Alg: "ES256",
		X: passkey.B64.EncodeToString(key.PublicKey.X.FillBytes(make([]byte, 32))),
		Y: passkey.B64.EncodeToString(key.PublicKey.Y.FillBytes(make([]byte, 32)))}}
	return s.signer, nil
}

// loginKeys are the public keys of every web node's login: this one's and
// the others', asked for now and then and kept, so a token one of them
// signed checks out while it's off.
func (s *Store) loginKeys(ctx context.Context) []JWK {
	var out []JWK
	if sg, err := s.loginSigner(); err == nil {
		out = append(out, sg.jwk)
	}
	s.keyMu.Lock()
	ask := time.Since(s.keysAt) > 10*time.Minute
	if ask {
		s.keysAt = time.Now()
	}
	s.keyMu.Unlock()
	if ask {
		// Finding the other web nodes takes a while: wait only if none of
		// their keys is known yet.
		s.lock()
		none := len(s.st.LoginKeys) == 0
		s.mu.Unlock()
		if none {
			s.askKeys()
		} else {
			go s.askKeys()
		}
	}
	s.lock()
	for _, k := range s.st.LoginKeys {
		if len(out) == 0 || k.Kid != out[0].Kid {
			out = append(out, k)
		}
	}
	s.mu.Unlock()
	return out
}

// askKeys asks the other web nodes for their signing keys, and keeps them.
func (s *Store) askKeys() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, n := range s.others(ctx) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n+"/v1/light/login-key", nil)
		if err != nil {
			continue
		}
		resp, err := s.Client.Do(req)
		if err != nil {
			continue
		}
		var k JWK
		if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&k) == nil && k.Kid != "" {
			if _, ok := k.public(); ok {
				s.lock()
				if s.st.LoginKeys[k.Kid] != k {
					s.st.LoginKeys[k.Kid] = k
					s.saveLocked()
				}
				s.mu.Unlock()
			}
		}
		resp.Body.Close()
	}
}

// newTicket makes a code for t, here and on the other web nodes.
func (s *Store) newTicket(t Ticket, now time.Time) string {
	b := make([]byte, 32)
	rand.Read(b)
	code := passkey.B64.EncodeToString(b)
	t.Hash, t.Exp = tokenHash(code), now.Add(CodeFor).Unix()
	s.keepTicket(t)
	go s.tellOthers(http.MethodPut, "/v1/light/ticket", t)
	return code
}

func (s *Store) keepTicket(t Ticket) {
	s.lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	kept := s.st.Tickets[:0]
	for _, x := range s.st.Tickets {
		if x.Exp > now && x.Hash != t.Hash {
			kept = append(kept, x)
		}
	}
	s.st.Tickets = kept
	if !s.goneLocked(t.Hash) {
		s.st.Tickets = append(s.st.Tickets, t)
	}
	s.saveLocked()
}

func (s *Store) dropTicket(hash string) (Ticket, bool) {
	s.lock()
	defer s.mu.Unlock()
	if !s.goneLocked(hash) {
		s.forgetLocked(hash, CodeFor)
		s.saveLocked()
	}
	for i, t := range s.st.Tickets {
		if t.Hash == hash {
			s.st.Tickets = append(s.st.Tickets[:i], s.st.Tickets[i+1:]...)
			s.saveLocked()
			return t, true
		}
	}
	return Ticket{}, false
}

// takeTicket uses up a code, wherever it was made: it works once.
func (s *Store) takeTicket(ctx context.Context, code, kind string, now time.Time) (Ticket, bool) {
	if code == "" {
		return Ticket{}, false
	}
	h := tokenHash(code)
	t, ok := s.dropTicket(h)
	if ok {
		go s.tellOthers(http.MethodDelete, "/v1/light/ticket/"+h, nil)
	} else {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for _, n := range s.others(ctx) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+n+"/v1/light/ticket/"+h, nil)
			if err != nil {
				continue
			}
			resp, err := s.Client.Do(req)
			if err != nil {
				continue
			}
			var got Ticket
			if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&got) == nil && got.Hash == h {
				t, ok = got, true
			}
			resp.Body.Close()
		}
	}
	if !ok || t.Kind != kind || now.Unix() > t.Exp {
		return Ticket{}, false
	}
	return t, true
}

// setVia notes that the session hash came from the one at my., via.
func (s *Store) setVia(hash, via string) {
	s.lock()
	var ss Session
	for i := range s.st.Sessions {
		if s.st.Sessions[i].Hash == hash {
			s.st.Sessions[i].Via = via
			ss = s.st.Sessions[i]
		}
	}
	s.saveLocked()
	s.mu.Unlock()
	if ss.Hash != "" {
		go s.tellOthers(http.MethodPut, "/v1/light/session", ss)
	}
}

// issuer is the login's address, https://my.DOMAIN, or "" if the group
// has no domain.
func (rl *Relay) issuer() string {
	d := peers.GroupDomain(rl.Peers())
	if d == "" {
		return ""
	}
	return "https://" + Host + "." + d
}

var hostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// groupAddress is whether host is one of the group's addresses, other
// than my.: only those may use the login (apps the admin adds come later).
func (rl *Relay) groupAddress(host string) bool {
	d := peers.GroupDomain(rl.Peers())
	return d != "" && hostName.MatchString(host) && host != Host+"."+d && (host == d || strings.HasSuffix(host, "."+d))
}

// backTo is redirect with params added, for sending the browser back.
func backTo(redirect string, params url.Values) string {
	u, _ := url.Parse(redirect)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func loginError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Can't sign in there</title><body style="font-family:system-ui,sans-serif;max-width:36em;margin:3em auto;padding:0 1em;line-height:1.5">
<h1>Can't sign in there</h1><p>`+html.EscapeString(msg)+`</p></body>`)
}

func tokenError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// serveLogin answers the login's own addresses on my.
func (rl *Relay) serveLogin(w http.ResponseWriter, r *http.Request, host string) bool {
	iss := rl.issuer()
	if iss == "" || rl.Signin == nil || "https://"+host != iss {
		return false
	}
	switch r.URL.Path {
	case discoveryPath:
		jsonOK(w, map[string]any{
			"issuer":                                iss,
			"authorization_endpoint":                iss + authorizePath,
			"token_endpoint":                        iss + tokenPath,
			"userinfo_endpoint":                     iss + userinfoPath,
			"jwks_uri":                              iss + keysPath,
			"end_session_endpoint":                  iss + signoutPath,
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"ES256"},
			"scopes_supported":                      []string{"openid", "profile", "email"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"code_challenge_methods_supported":      []string{"S256"},
			"claims_supported":                      []string{"sub", "name", "preferred_username", "email", "email_verified", "box"},
		})
	case keysPath:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		jsonOK(w, map[string]any{"keys": rl.Signin.loginKeys(r.Context())})
	case authorizePath:
		rl.authorize(w, r, host)
	case tokenPath:
		rl.token(w, r)
	case userinfoPath:
		rl.userinfo(w, r)
	case adoptPath:
		rl.adopt(w, r, host)
	case signoutPath:
		if c, err := r.Cookie(SessionCookie); err == nil {
			rl.Signin.end(tokenHash(c.Value), true)
		}
		http.SetCookie(w, sessionCookie("", -1))
		if u, err := url.Parse(r.URL.Query().Get("post_logout_redirect_uri")); err == nil && u.Scheme == "https" && rl.groupAddress(hostOnly(u.Host)) {
			http.Redirect(w, r, u.String(), http.StatusFound)
			return true
		}
		http.Redirect(w, r, "/", http.StatusFound)
	default:
		return false
	}
	return true
}

// authorize hands a signed-in browser back to a group address with a
// code, or signs it in first.
func (rl *Relay) authorize(w http.ResponseWriter, r *http.Request, host string) {
	q := r.URL.Query()
	client := strings.ToLower(q.Get("client_id"))
	redirect := q.Get("redirect_uri")
	u, err := url.Parse(redirect)
	if !rl.groupAddress(client) || err != nil || u.Scheme != "https" || !strings.EqualFold(hostOnly(u.Host), client) || u.User != nil || u.Fragment != "" {
		loginError(w, http.StatusBadRequest, "Only the group's own addresses can use its login, and this one isn't one of them.")
		return
	}
	back := func(params url.Values) {
		params.Set("iss", rl.issuer())
		if st := q.Get("state"); st != "" {
			params.Set("state", st)
		}
		http.Redirect(w, r, backTo(redirect, params), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		back(url.Values{"error": {"unsupported_response_type"}})
		return
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) < 43 {
		back(url.Values{"error": {"invalid_request"}, "error_description": {"PKCE with S256 is needed"}})
		return
	}
	ss, ok := rl.signedIn(w, r, host, "")
	if !ok {
		if strings.Contains(" "+q.Get("prompt")+" ", " none ") {
			back(url.Values{"error": {"login_required"}})
			return
		}
		ServeSignIn(w, http.StatusOK) // and once signed in, it comes back here
		return
	}
	code := rl.Signin.newTicket(Ticket{Kind: "code", Session: ss.Hash, Client: client, Redirect: redirect,
		Challenge: q.Get("code_challenge"), Nonce: q.Get("nonce")}, time.Now())
	back(url.Values{"code": {code}})
}

func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	h := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(passkey.B64.EncodeToString(h[:])), []byte(challenge)) == 1
}

var errBadCode = errors.New("the code is wrong, used already or too old")

// exchange uses up a code, giving the session at my. it stands for.
func (rl *Relay) exchange(ctx context.Context, code, client, redirect, verifier string) (Session, Ticket, error) {
	now := time.Now()
	t, ok := rl.Signin.takeTicket(ctx, code, "code", now)
	if !ok || !strings.EqualFold(t.Client, client) || t.Redirect != redirect {
		return Session{}, t, errBadCode
	}
	if !pkceOK(verifier, t.Challenge) {
		return Session{}, t, errors.New("the code verifier doesn't match")
	}
	ss, ok := rl.Signin.sessionByHash(ctx, t.Session, "", now)
	if !ok {
		return Session{}, t, errors.New("that sign-in has ended")
	}
	return ss, t, nil
}

// token is the token endpoint: a code and its verifier for an ID token.
func (rl *Relay) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, "unsupported_grant_type", "only authorization_code")
		return
	}
	client := r.PostForm.Get("client_id")
	if user, _, ok := r.BasicAuth(); ok && client == "" {
		client = user
	}
	ss, t, err := rl.exchange(r.Context(), r.PostForm.Get("code"), client, r.PostForm.Get("redirect_uri"), r.PostForm.Get("code_verifier"))
	if err != nil {
		tokenError(w, "invalid_grant", err.Error())
		return
	}
	claims := rl.claims(ss, strings.ToLower(client), t.Nonce)
	id, err1 := rl.sign(claims)
	claims["use"] = "access"
	delete(claims, "nonce")
	access, err2 := rl.sign(claims)
	if err := errors.Join(err1, err2); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(TokenFor.Seconds()), "id_token": id})
}

// email is the person's address in the group, from their own name
// (kath.example.org gives kath@example.org), if they have one.
func (rl *Relay) email(box, person string) string {
	d := peers.GroupDomain(rl.Peers())
	for _, p := range rl.Peers() {
		if !strings.EqualFold(p.Name, box) {
			continue
		}
		for _, pn := range p.PeopleNames {
			if strings.EqualFold(pn.Person, person) && !pn.Site {
				if label, ok := strings.CutSuffix(pn.Name, "."+d); ok && d != "" && !strings.Contains(label, ".") {
					return label + "@" + d
				}
			}
		}
	}
	return ""
}

// claims say who ss is, to client.
func (rl *Relay) claims(ss Session, client, nonce string) map[string]any {
	now := time.Now()
	sid := sha256.Sum256([]byte("sid " + ss.Hash))
	c := map[string]any{
		"iss": rl.issuer(), "aud": client, "sub": strings.ToLower(ss.Box + "/" + ss.Person),
		"iat": now.Unix(), "exp": now.Add(TokenFor).Unix(), "auth_time": ss.Started,
		"name": ss.Person, "preferred_username": ss.Person, "box": ss.Box, "sid": hex.EncodeToString(sid[:16]),
	}
	if e := rl.email(ss.Box, ss.Person); e != "" {
		c["email"], c["email_verified"] = e, true
	}
	if nonce != "" {
		c["nonce"] = nonce
	}
	return c
}

// sign makes a JWT of claims, signed with this web node's key.
func (rl *Relay) sign(claims map[string]any) (string, error) {
	sg, err := rl.Signin.loginSigner()
	if err != nil {
		return "", err
	}
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": sg.jwk.Kid})
	body, _ := json.Marshal(claims)
	msg := passkey.B64.EncodeToString(head) + "." + passkey.B64.EncodeToString(body)
	h := sha256.Sum256([]byte(msg))
	rr, ss, err := ecdsa.Sign(rand.Reader, sg.key, h[:])
	if err != nil {
		return "", err
	}
	sig := append(rr.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	return msg + "." + passkey.B64.EncodeToString(sig), nil
}

// verify checks a JWT one of the web nodes signed, giving its claims.
func (rl *Relay) verify(ctx context.Context, token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	var head struct{ Alg, Kid string }
	hb, err := passkey.B64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &head) != nil || head.Alg != "ES256" {
		return nil, false
	}
	sig, err := passkey.B64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, false
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, k := range rl.Signin.loginKeys(ctx) {
		pub, ok := k.public()
		if !ok || k.Kid != head.Kid || !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			continue
		}
		bb, err := passkey.B64.DecodeString(parts[1])
		var c map[string]any
		if err != nil || json.NewDecoder(bytes.NewReader(bb)).Decode(&c) != nil {
			return nil, false
		}
		if exp, _ := c["exp"].(float64); int64(exp) < time.Now().Unix() || c["iss"] != rl.issuer() {
			return nil, false
		}
		return c, true
	}
	return nil, false
}

// userinfo says who an access token is for.
func (rl *Relay) userinfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, valid := rl.verify(r.Context(), strings.TrimSpace(tok))
	if !ok || !valid || c["use"] != "access" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	out := map[string]any{}
	for _, k := range []string{"sub", "name", "preferred_username", "email", "email_verified", "box"} {
		if v, ok := c[k]; ok {
			out[k] = v
		}
	}
	jsonOK(w, out)
}

// adopt signs a browser in at my. after it signed in at the person's own
// address, so the rest of the group knows them too.
func (rl *Relay) adopt(w http.ResponseWriter, r *http.Request, host string) {
	now := time.Now()
	t, ok := rl.Signin.takeTicket(r.Context(), r.URL.Query().Get("t"), "adopt", now)
	if !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	own, ok := rl.Signin.sessionByHash(r.Context(), t.Session, "", now)
	if !ok {
		http.Redirect(w, r, t.Redirect, http.StatusFound)
		return
	}
	if mine, ok := rl.signedIn(w, r, host, ""); ok {
		// Already signed in here: as them, the two go together; as someone
		// else, that stays.
		if mine.Box == own.Box && strings.EqualFold(mine.Person, own.Person) {
			rl.Signin.setVia(own.Hash, mine.Hash)
		}
		http.Redirect(w, r, t.Redirect, http.StatusFound)
		return
	}
	token, ss := rl.Signin.startSession(Session{Box: own.Box, Person: own.Person, Key: own.Key, Device: own.Device, Host: host,
		Started: now.Unix()}, now)
	rl.Signin.setVia(own.Hash, ss.Hash)
	http.SetCookie(w, sessionCookie(token, int(ss.For().Seconds())))
	setBox(w, own.Box)
	http.Redirect(w, r, t.Redirect, http.StatusFound)
}

// adoptLink is where a browser just signed in at its person's own address
// goes next, to be signed in at my. as well; "" if there's no my.
func (rl *Relay) adoptLink(ss Session, host string) string {
	iss := rl.issuer()
	if iss == "" || "https://"+host == iss {
		return ""
	}
	t := rl.Signin.newTicket(Ticket{Kind: "adopt", Session: ss.Hash, Redirect: "https://" + host + "/files"}, time.Now())
	return iss + adoptPath + "?t=" + url.QueryEscape(t)
}

// startLogin sends a browser that isn't signed in at someone's own
// address to my. Quietly, without a sign-in page: if it's signed in there
// as that person, it comes straight back signed in; if not, it comes back
// to the address's own sign-in page. Not quietly (their key is my.'s), my.
// asks them to sign in.
func (rl *Relay) startLogin(w http.ResponseWriter, r *http.Request, host string, quiet bool) bool {
	iss := rl.issuer()
	if iss == "" || !rl.groupAddress(host) {
		return false
	}
	b := make([]byte, 48)
	rand.Read(b)
	state, verifier := passkey.B64.EncodeToString(b[:16]), passkey.B64.EncodeToString(b[16:])
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: state + "." + verifier, Path: backPath, MaxAge: 600,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	h := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {host}, "redirect_uri": {"https://" + host + backPath}, "response_type": {"code"}, "scope": {"openid"},
		"state": {state}, "code_challenge": {passkey.B64.EncodeToString(h[:])}, "code_challenge_method": {"S256"}}
	if quiet {
		q.Set("prompt", "none")
	}
	http.Redirect(w, r, iss+authorizePath+"?"+q.Encode(), http.StatusFound)
	return true
}

// loginBack takes the browser back from my. at someone's own address: signed
// in, if my. knew them as that person; to the sign-in page if not.
func (rl *Relay) loginBack(w http.ResponseWriter, r *http.Request, host, only string, box peers.Peer) {
	q := r.URL.Query()
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: "", Path: backPath, MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	c, err := r.Cookie(loginCookie)
	state, verifier, _ := strings.Cut(func() string {
		if err != nil {
			return ""
		}
		return c.Value
	}(), ".")
	if state == "" || q.Get("state") != state || q.Get("code") == "" {
		ServeSignIn(w, http.StatusOK)
		return
	}
	my, _, err := rl.exchange(r.Context(), q.Get("code"), host, "https://"+host+backPath, verifier)
	if err != nil || my.Box != box.Name || !strings.EqualFold(my.Person, only) {
		ServeSignIn(w, http.StatusOK) // someone else is signed in at my.
		return
	}
	now := time.Now()
	token, ss := rl.Signin.startSession(Session{Box: my.Box, Person: my.Person, Key: my.Key, Device: my.Device, Host: host,
		Started: now.Unix(), Via: my.Hash}, now)
	rl.logf("light: %s on %s signed in at %s through the login", ss.Person, ss.Box, host)
	http.SetCookie(w, sessionCookie(token, int(ss.For().Seconds())))
	http.Redirect(w, r, "/files", http.StatusFound)
}
