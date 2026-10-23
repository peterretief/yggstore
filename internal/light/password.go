package light

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

// Signing in with a password (see docs/web-access.md): someone's group
// address, such as kath@group.example (or name@box, without one), and the
// password the box told the web nodes the hash of. A temporary one must be
// swapped for their own first, which their box keeps.

// Fails are someone's wrong passwords in a row, and until when they must
// wait before trying again.
type Fails struct {
	N     int   `json:"n"`
	Until int64 `json:"until,omitempty"` // unix
}

// MaxFails wrong passwords in a row make someone wait PauseFor, and
// TriesPer sign-ins from one address in TriesIn are all it may try.
const (
	MaxFails = 5
	TriesPer = 20
)

var (
	PauseFor = 15 * time.Minute
	TriesIn  = 10 * time.Minute
)

// PasswordRequest is what the sign-in page sends.
type PasswordRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	New      string `json:"new"` // their own, in place of a temporary one
}

var errWrongPassword = errors.New("That address and password don't match.")

// byLogin finds whose a login is: a group address, or name@box.
func (s *Store) byLogin(list []peers.Peer, login string) (found, bool) {
	login = strings.ToLower(strings.TrimSpace(login))
	s.lock()
	defer s.mu.Unlock()
	for box, ps := range s.st.Boxes {
		var node string
		for _, p := range list {
			if p.Name == box {
				node = p.IP()
			}
		}
		for _, p := range ps {
			if p.Password == nil {
				continue
			}
			if strings.EqualFold(login, p.Name+"@"+box) {
				return found{Box: box, Person: p}, true
			}
			if pn, ok := peers.PersonNameOf(list, node, p.Name); ok && strings.EqualFold(login, peers.MailAddress(pn.Name)) {
				return found{Box: box, Person: p}, true
			}
		}
	}
	return found{}, false
}

func failsKey(box, person string) string { return box + "/" + strings.ToLower(person) }

func (s *Store) fails(who string) Fails {
	s.lock()
	defer s.mu.Unlock()
	return s.st.Fails[who]
}

// setFails notes someone's wrong passwords (none after a right one), and
// tells the other web nodes.
func (s *Store) setFails(who string, f Fails, tell bool) {
	s.lock()
	if f.N == 0 {
		delete(s.st.Fails, who)
	} else if old := s.st.Fails[who]; f.N >= old.N || f.Until > old.Until {
		s.st.Fails[who] = f
	}
	s.saveLocked()
	s.mu.Unlock()
	if tell {
		go s.tellOthers(http.MethodPost, "/v1/light/fails", map[string]any{"who": who, "n": f.N, "until": f.Until})
	}
}

// setPassword keeps the password someone chose, before their box's own
// word of it arrives.
func (s *Store) setPassword(box, person string, pw password.Password) {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.st.Boxes[box] {
		if p := &s.st.Boxes[box][i]; strings.EqualFold(p.Name, person) {
			p.Password = &pw
		}
	}
	s.saveLocked()
}

// tooMany is whether the address ip has tried too many passwords lately.
func (rl *Relay) tooMany(ip string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.tries == nil {
		rl.tries = map[string][]time.Time{}
	}
	kept := rl.tries[ip][:0]
	for _, t := range rl.tries[ip] {
		if now.Sub(t) < TriesIn {
			kept = append(kept, t)
		}
	}
	if len(kept) >= TriesPer {
		rl.tries[ip] = kept
		return true
	}
	rl.tries[ip] = append(kept, now)
	return false
}

// passwordSignIn checks a password and starts a session. A temporary
// password needs their own chosen with it (428 asks the page for one),
// which the box must take first.
func (rl *Relay) passwordSignIn(w http.ResponseWriter, r *http.Request, host, only string) {
	if rl.Signin == nil {
		http.Error(w, "this web node doesn't sign people in", http.StatusServiceUnavailable)
		return
	}
	var req PasswordRequest
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	now := time.Now()
	ip := r.Header.Get("CF-Connecting-IP")
	if ip == "" {
		ip = hostOnly(r.RemoteAddr)
	}
	if rl.tooMany(ip, now) {
		http.Error(w, "Too many tries from here. Wait a few minutes, then try again.", http.StatusTooManyRequests)
		return
	}
	f, ok := rl.Signin.byLogin(rl.Peers(), req.Login)
	if !ok || (only != "" && !strings.EqualFold(f.Person.Name, only)) {
		password.Matches("pbkdf2-sha256$00$00", req.Password) // as long as a real check
		http.Error(w, errWrongPassword.Error(), http.StatusForbidden)
		return
	}
	who := failsKey(f.Box, f.Person.Name)
	if fl := rl.Signin.fails(who); fl.Until > now.Unix() {
		http.Error(w, fmt.Sprintf("Too many wrong passwords. Try again after %s.", time.Unix(fl.Until, 0).Format("15:04")), http.StatusTooManyRequests)
		return
	}
	pw := f.Person.Password
	if !pw.Matches(req.Password) {
		fl := rl.Signin.fails(who)
		fl.N++
		if fl.N >= MaxFails {
			fl.Until = now.Add(PauseFor).Unix()
		}
		rl.Signin.setFails(who, fl, true)
		rl.logf("light: wrong password for %s on %s (from %s)", f.Person.Name, f.Box, ip)
		http.Error(w, errWrongPassword.Error(), http.StatusForbidden)
		return
	}
	if !f.Person.Web {
		http.Error(w, "Opening your files through the web is turned off for now. Ask the box's owner.", http.StatusForbidden)
		return
	}
	if !pw.Live(now) {
		http.Error(w, "This temporary password has run out. Ask the group's admin for a new one.", http.StatusForbidden)
		return
	}
	if rl.Signin.fails(who).N > 0 {
		rl.Signin.setFails(who, Fails{}, true)
	}
	if pw.Temp {
		if req.New == "" {
			http.Error(w, "Choose your own password now.", http.StatusPreconditionRequired)
			return
		}
		if err := password.OK(req.New); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		chosen := password.Chosen(req.New, now)
		if err := rl.tellBox(f.Box, host, f.Person.Name, Seen{Key: pw.ID(), Password: &chosen}); err != nil {
			rl.logf("light: %s's new password: %v", f.Person.Name, err)
			http.Error(w, "Your box has to be on to keep your new password, and it isn't answering. Try again in a while.", http.StatusServiceUnavailable)
			return
		}
		rl.Signin.setPassword(f.Box, f.Person.Name, chosen)
		pw = &chosen
		rl.logf("light: %s on %s chose their password", f.Person.Name, f.Box)
	}
	token, ss := rl.Signin.startSession(Session{Box: f.Box, Person: f.Person.Name, Key: pw.ID(), Device: "password",
		Host: host, Started: now.Unix()}, now)
	rl.logf("light: %s on %s signed in at %s with their password", f.Person.Name, f.Box, host)
	http.SetCookie(w, sessionCookie(token, int(ss.For().Seconds())))
	answer := map[string]string{"person": f.Person.Name, "home": "/files/mail"}
	if !own(rl.Peers(), host) {
		setBox(w, f.Box)
	} else if next := rl.adoptLink(ss, host); next != "" {
		answer["next"] = next
	}
	jsonOK(w, answer)
}
