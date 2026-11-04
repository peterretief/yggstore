package light

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

// Accounts people make themselves (docs/trial.md). At my., the web node
// serves the sign-up page and passes sign-ups to the group's trial box
// (peers.TrialBox), which keeps each until its link is opened; the web
// node emails the link. Opening it has the box make the person, and the
// web node signs them in. Files they share by link download from
// my./s/BOX/TOKEN, without signing in.
const (
	SignupPath = "/signup"
	SharePath  = "/s/"
	// SignupsPerIP is how many sign-ups one internet address may send a day.
	SignupsPerIP = 3
)

//go:embed signup.html
var signupHTML []byte

// TrialEmail is an email a web node that sends mail sends for another:
// Kind "warn", from the trial box, tells someone their account closes at
// Until unless they sign in (with Login); Kind "confirm", from a web node
// that can't send mail itself, carries a sign-up's Link for the account
// Login (label.DOMAIN).
type TrialEmail struct {
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Login string `json:"login"`
	Until int64  `json:"until,omitempty"`
	Link  string `json:"link,omitempty"`
}

var signupLink = regexp.MustCompile(`^https://[a-z0-9.-]+/signup#[0-9a-f]{32,64}$`)

// confirmText is the email that carries a sign-up's link.
func confirmText(account, link string) string {
	return fmt.Sprintf("Hello,\n\nOpen this link to finish making your account, %s:\n\n%s\n\n"+
		"It works for two days. If you didn't ask for an account, ignore this email: nothing is made without the link.\n",
		account, link)
}

const confirmSubject = "Finish making your account"

func (rl *Relay) myHost(host string) bool {
	d := peers.GroupDomain(rl.Peers())
	return d != "" && host == Host+"."+d
}

// serveTrial answers sign-ups, their links and shared files at my.; it
// reports whether the request was one of those.
func (rl *Relay) serveTrial(w http.ResponseWriter, r *http.Request, host string) bool {
	if !rl.myHost(host) {
		return false
	}
	p := r.URL.Path
	switch {
	case p == SignupPath && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", csp)
		w.Write(signupHTML)
	case p == Base+"/signup" || p == Base+"/confirm":
		if r.Method != http.MethodPost || r.Header.Get("X-Yggstore") != "1" {
			http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
			return true
		}
		box, ok := peers.TrialBox(rl.Peers())
		if !ok {
			http.Error(w, "This group isn't taking accounts just now.", http.StatusNotFound)
			return true
		}
		if p == Base+"/signup" {
			rl.signup(w, r, host, box)
		} else {
			rl.confirm(w, r, host, box)
		}
	case strings.HasPrefix(p, SharePath):
		rl.shared(w, r, strings.TrimPrefix(p, SharePath))
	default:
		return false
	}
	return true
}

// toBox posts body to the box's path, as from the browser of r, and gives
// back the box's answer.
func (rl *Relay) toBox(r *http.Request, box peers.Peer, path string, body []byte) (int, []byte, error) {
	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, rl.boxURL(box.IP())+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("X-Yggstore", "1")
	rl.headers(out, r)
	resp, err := rl.client().Do(out)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, answer, nil
}

func (rl *Relay) signup(w http.ResponseWriter, r *http.Request, host string, box peers.Peer) {
	var req struct {
		Label    string `json:"label"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Website  string `json:"website"` // a field people don't see
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if rl.Email == nil && rl.Mailers == nil {
		http.Error(w, "This web node can't send email, so it can't take accounts. Try again in a minute.", http.StatusServiceUnavailable)
		return
	}
	ip := visitorIP(r)
	if req.Website != "" {
		rl.logf("light: dropped a sign-up from %s that filled in the hidden field", ip)
		jsonOK(w, map[string]string{"email": req.Email})
		return
	}
	if !rl.signupsLeft(ip, time.Now(), false) {
		http.Error(w, "Too many accounts made from here today. Try again tomorrow.", http.StatusTooManyRequests)
		return
	}
	body, _ := json.Marshal(map[string]string{"label": req.Label, "email": req.Email, "password": req.Password})
	code, answer, err := rl.toBox(r, box, Base+"/signup", body)
	if err != nil {
		rl.logf("light: sign-up: %s: %v", box.Name, err)
		http.Error(w, "Accounts can't be made just now. Try again in a while.", http.StatusServiceUnavailable)
		return
	}
	var su struct{ Label, Email, Token string }
	if code != http.StatusOK || json.Unmarshal(answer, &su) != nil || su.Token == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		w.Write(answer)
		return
	}
	account := su.Label + "." + peers.GroupDomain(rl.Peers())
	link := "https://" + host + SignupPath + "#" + su.Token
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := rl.sendConfirm(ctx, su.Email, account, link); err != nil {
		rl.logf("light: emailing %s their sign-up link: %v", su.Label, err)
		http.Error(w, "The email couldn't be sent just now. Try again in 10 minutes.", http.StatusBadGateway)
		return
	}
	rl.signupsLeft(ip, time.Now(), true)
	rl.logf("light: %s signed up from %s: link emailed", su.Label, ip)
	jsonOK(w, map[string]string{"email": su.Email})
}

// sendConfirm emails a sign-up's link: itself, or through a web node that
// sends the group's mail.
func (rl *Relay) sendConfirm(ctx context.Context, to, account, link string) error {
	if rl.Email != nil {
		return rl.Email(ctx, to, confirmSubject, confirmText(account, link))
	}
	err := errors.New("no web node that sends mail is answering")
	for _, n := range rl.Mailers(ctx) {
		if err = SendTrialEmail(ctx, rl.client(), n, TrialEmail{To: to, Kind: "confirm", Login: account, Link: link}); err == nil {
			return nil
		}
		rl.logf("light: asking %s to send a sign-up link: %v", n, err)
	}
	return err
}

func (rl *Relay) confirm(w http.ResponseWriter, r *http.Request, host string, box peers.Peer) {
	if rl.Signin == nil {
		http.Error(w, "this web node doesn't sign people in", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code, answer, err := rl.toBox(r, box, Base+"/confirm", body)
	if err != nil {
		rl.logf("light: confirming a sign-up: %s: %v", box.Name, err)
		boxOff(w)
		return
	}
	var c struct {
		Person   string            `json:"person"`
		Password password.Password `json:"password"`
	}
	if code != http.StatusOK || json.Unmarshal(answer, &c) != nil || c.Person == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		w.Write(answer)
		return
	}
	// The box sends every web node its people too; this one needn't wait.
	now := time.Now()
	rl.Signin.addPerson(box.Name, Person{Name: c.Person, Web: true, Password: &c.Password})
	token, ss := rl.Signin.startSession(Session{Box: box.Name, Person: c.Person, Key: c.Password.ID(), Device: "password",
		Host: host, Started: now.Unix()}, now)
	http.SetCookie(w, sessionCookie(token, int(ss.For().Seconds())))
	setBox(w, box.Name)
	rl.logf("light: %s's account on %s is open", c.Person, box.Name)
	jsonOK(w, map[string]string{"person": c.Person})
}

// shared passes a share link (BOX/TOKEN) to its box, with nothing of the
// browser's own: anyone may open it.
func (rl *Relay) shared(w http.ResponseWriter, r *http.Request, rest string) {
	name, token, _ := strings.Cut(rest, "/")
	box, ok := rl.box(name)
	if !ok || token == "" || strings.Contains(token, "/") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	r.Header.Del("Cookie")
	r.Header.Del("Authorization")
	r.URL.Path, r.URL.RawPath, r.URL.RawQuery = "/files/shared/"+token, "", ""
	if rl.isDown(box.IP()) || !rl.there(r.Context(), box.IP()) {
		http.Error(w, "The box this file is on isn't answering just now. Try again later.", http.StatusServiceUnavailable)
		return
	}
	rl.forward(w, r, box.IP())
}

func visitorIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	return hostOnly(r.RemoteAddr)
}

// signupsLeft is whether ip may have another sign-up link emailed today;
// with note, one was.
func (rl *Relay) signupsLeft(ip string, now time.Time, note bool) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.signups == nil {
		rl.signups = map[string][]time.Time{}
	}
	var kept []time.Time
	for _, t := range rl.signups[ip] {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
		}
	}
	if note {
		kept = append(kept, now)
	}
	rl.signups[ip] = kept
	return len(kept) < SignupsPerIP
}

// addPerson keeps a person a box made just now, until the box's list
// arrives.
func (s *Store) addPerson(box string, p Person) {
	s.lock()
	defer s.mu.Unlock()
	for _, x := range s.st.Boxes[box] {
		if strings.EqualFold(x.Name, p.Name) {
			return
		}
	}
	if s.st.Boxes == nil {
		s.st.Boxes = map[string][]Person{}
	}
	s.st.Boxes[box] = append(s.st.Boxes[box], p)
	s.saveLocked()
}

// SendTrialEmail asks the web node at node to email one of the trial
// box's people.
func SendTrialEmail(ctx context.Context, c *http.Client, node string, e TrialEmail) error {
	b, _ := json.Marshal(e)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+node+"/v1/light/trial-email", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
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

// trialEmail sends what the trial box (a warning) or another web node (a
// sign-up's link) asks, always in the group's own words.
func (s *Store) trialEmail(w http.ResponseWriter, r *http.Request, caller string) {
	list := s.Peers()
	if s.Email == nil {
		http.Error(w, "this node sends no email", http.StatusNotFound)
		return
	}
	var e TrialEmail
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&e) != nil || e.To == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	domain := peers.GroupDomain(list)
	tb, hasTrial := peers.TrialBox(list)
	switch {
	case e.Kind == "confirm":
		if !s.isWebNode(r.Context(), caller) {
			http.Error(w, "only the group's web nodes send sign-up links", http.StatusForbidden)
			return
		}
		if !signupLink.MatchString(e.Link) || !strings.HasPrefix(e.Link, "https://"+Host+"."+domain+SignupPath+"#") ||
			!strings.HasSuffix(e.Login, "."+domain) || strings.ContainsAny(e.Login, " \r\n") {
			http.Error(w, "that isn't a sign-up link", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		if err := s.Email(ctx, e.To, confirmSubject, confirmText(e.Login, e.Link)); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.logf("light: sent %s's sign-up link for another web node", e.Login)
		w.WriteHeader(http.StatusNoContent)
		return
	case e.Kind != "warn":
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	case !hasTrial || tb.IP() != caller:
		http.Error(w, "only the group's trial box", http.StatusForbidden)
		return
	}
	text := fmt.Sprintf("Hello,\n\nYour account %s hasn't been used for a while, so it closes on %s, with your files and website.\n\n"+
		"To keep it, sign in before then at https://%s.%s\n\nIf you don't need it any more, there's nothing to do.\n",
		e.Login, time.Unix(e.Until, 0).Format("2 January 2006"), Host, domain)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.Email(ctx, e.To, "Your account closes soon", text); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.logf("light: told %s their account closes soon", e.Login)
	w.WriteHeader(http.StatusNoContent)
}
