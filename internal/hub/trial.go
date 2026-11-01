package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	netmail "net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

// Accounts people make themselves (docs/trial.md): on the box marked
// "trial" in the group's list, anyone may sign up through a web node with
// a name, an email address and a password. The box keeps the sign-up
// until the link emailed to them is opened, then makes them a person with
// TrialQuota, and its dashboard asks the group for their name. They may
// leave whenever they like, and an account unused for TrialIdle goes.
const (
	TrialQuota  = 500 << 20
	TrialMax    = 50 // accounts and sign-ups waiting, together
	TrialPerDay = 20
	SignupFor   = 48 * time.Hour   // how long a link works
	SignupEvery = 10 * time.Minute // the same link goes at most this often
	TrialIdle   = 60 * 24 * time.Hour
	TrialWarn   = 7 * 24 * time.Hour // the email goes this long before
	// LeaveFor is how long the box waits for a leaving person's files to
	// be deleted from the group before it removes the rest anyway; leases
	// let the nodes drop what's left.
	LeaveFor = 7 * 24 * time.Hour
)

// Trial is what the box keeps of an account the person made.
type Trial struct {
	Email string `json:"email"` // the address they confirmed
	Label string `json:"label"` // the name asked of the group: Label.DOMAIN
	Asked bool   `json:"asked,omitempty"`
	Seen  int64  `json:"seen,omitempty"` // when they last used it
	// Warned is when they were told the account goes soon.
	Warned int64 `json:"warned,omitempty"`
	// Leaving is when they left, or the account was found unused: their
	// stored files are being deleted from the group. Gone is when the
	// person was removed after that.
	Leaving int64 `json:"leaving,omitempty"`
	Gone    int64 `json:"gone,omitempty"`
}

// Signup waits for the link to be opened.
type Signup struct {
	Label string `json:"label"`
	Email string `json:"email"`
	Hash  string `json:"hash"` // their password's
	Token string `json:"token"`
	Made  int64  `json:"made"`
	Sent  int64  `json:"sent"`
}

// SignupRequest is what a web node passes on.
type SignupRequest struct {
	Label    string `json:"label"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// SignupAnswer is the box's: the link's token, for the web node to email.
type SignupAnswer struct {
	Label string `json:"label"`
	Email string `json:"email"`
	Token string `json:"token"`
}

// Confirmed is the box's answer to a link opened: who the person is now,
// for the web node to sign them in.
type Confirmed struct {
	Person   string            `json:"person"`
	Password password.Password `json:"password"`
}

var errNoSignup = errors.New("This link has been used, or it's more than two days old. Sign up again to get a new one.")

func (p Person) trial() bool     { return p.Trial != nil }
func (p Person) leaving() bool   { return p.Trial != nil && p.Trial.Leaving != 0 }
func (p Person) trialLive() bool { return p.trial() && p.Active() && !p.leaving() }

func (s *HubState) pruneSignups(now time.Time) {
	s.Signups = slices.DeleteFunc(s.Signups, func(su Signup) bool { return now.Sub(time.Unix(su.Made, 0)) > SignupFor })
}

// SignUp keeps a sign-up, or gives the one waiting for that address again
// (with this name and password) once SignupEvery has passed.
func (s *HubState) SignUp(list []peers.Peer, req SignupRequest, now time.Time) (Signup, error) {
	label := strings.ToLower(strings.TrimSpace(req.Label))
	domain := peers.GroupDomain(list)
	a, err := netmail.ParseAddress(strings.TrimSpace(req.Email))
	switch {
	case domain == "":
		return Signup{}, errors.New("this group has no domain, so it can't take accounts")
	case peers.ValidLabel(label) != nil:
		return Signup{}, peers.ValidLabel(label)
	case peers.Reserved[label]:
		return Signup{}, fmt.Errorf("%s is kept for the group: choose another name", label)
	case err != nil || a.Name != "" || !strings.Contains(a.Address, "."):
		return Signup{}, errors.New("That isn't an email address.")
	case strings.HasSuffix(strings.ToLower(a.Address), "@"+domain):
		return Signup{}, errors.New("Give an address outside " + domain + ", one you read now.")
	}
	if err := password.OK(req.Password); err != nil {
		return Signup{}, err
	}
	s.pruneSignups(now)
	mine := -1
	for i, su := range s.Signups {
		if strings.EqualFold(su.Email, a.Address) {
			mine = i
		}
	}
	if mine >= 0 {
		if sent := time.Unix(s.Signups[mine].Sent, 0); now.Sub(sent) < SignupEvery {
			return Signup{}, fmt.Errorf("The link went to %s at %s. Email can take a few minutes: look in your spam or junk folder too. You can have it sent again from %s.",
				a.Address, sent.Format("15:04"), sent.Add(SignupEvery).Format("15:04"))
		}
	}
	taken := s.person(label) >= 0
	if _, ok := peers.NameOwner(list, label+"."+domain); ok {
		taken = true
	}
	waiting, today := 0, 0
	for i, su := range s.Signups {
		if i != mine && su.Label == label {
			taken = true
		}
		if i != mine {
			waiting++
		}
		if now.Sub(time.Unix(su.Made, 0)) < 24*time.Hour {
			today++
		}
	}
	for _, p := range s.People {
		if p.trial() && strings.EqualFold(p.Trial.Email, a.Address) && !p.Removed {
			return Signup{}, fmt.Errorf("%s has an account already: sign in with it.", a.Address)
		}
		if p.trial() && !p.Removed {
			waiting++
			if now.Sub(time.Unix(p.Added, 0)) < 24*time.Hour {
				today++
			}
		}
	}
	switch {
	case taken:
		return Signup{}, fmt.Errorf("%s.%s is taken: choose another name", label, domain)
	case waiting >= TrialMax:
		return Signup{}, errors.New("The trial is full just now. Try again in a few days.")
	case mine < 0 && today >= TrialPerDay:
		return Signup{}, errors.New("Enough accounts were made today. Try again tomorrow.")
	}
	if mine < 0 {
		b := make([]byte, 24)
		rand.Read(b)
		s.Signups = append(s.Signups, Signup{Email: a.Address, Token: hex.EncodeToString(b), Made: now.Unix()})
		mine = len(s.Signups) - 1
	}
	su := &s.Signups[mine]
	su.Label, su.Hash, su.Sent = label, password.Hash(req.Password), now.Unix()
	return *su, nil
}

// Confirm makes the person a sign-up's link is for, with a folder in dir.
func (s *HubState) Confirm(token, dir string, now time.Time) (Person, error) {
	s.pruneSignups(now)
	i := slices.IndexFunc(s.Signups, func(su Signup) bool { return token != "" && su.Token == token })
	if i < 0 {
		return Person{}, errNoSignup
	}
	su := s.Signups[i]
	if s.person(su.Label) >= 0 {
		return Person{}, fmt.Errorf("Someone took %s while you were away. Sign up again with another name.", su.Label)
	}
	p, err := s.AddPerson(su.Label, dir)
	if err != nil {
		return Person{}, err
	}
	j := s.person(p.Name)
	s.People[j].Quota = TrialQuota
	s.People[j].Password = &password.Password{Hash: su.Hash, Set: now.Unix()}
	s.People[j].Trial = &Trial{Email: su.Email, Label: su.Label, Seen: now.Unix()}
	s.Signups = slices.Delete(s.Signups, i, i+1)
	return s.People[j], nil
}

// Leave starts taking a trial account away: nobody can sign in to it, and
// the hub deletes its files (trialStep).
func (s *HubState) Leave(person string, now time.Time) error {
	i := s.person(person)
	if i < 0 || !s.People[i].trial() {
		return errors.New("only an account you made yourself can be closed here: ask the box's owner")
	}
	p := &s.People[i]
	if p.Trial.Leaving == 0 {
		p.Trial.Leaving = now.Unix()
	}
	p.Password, p.Passkeys, p.WebOff = nil, nil, true
	s.Shares = slices.DeleteFunc(s.Shares, func(sh Share) bool { return strings.EqualFold(sh.Person, p.Name) })
	return nil
}

// trialStep looks after the box's trial accounts: warns of, and closes,
// those unused for long, and finishes closing those leaving.
func (u *Users) trialStep(ctx context.Context, now time.Time) {
	st, err := Load(u.cfg.StatePath)
	if err != nil {
		return
	}
	for _, p := range st.People {
		if !p.trial() || p.Trial.Gone != 0 && now.Sub(time.Unix(p.Trial.Gone, 0)) < time.Minute {
			continue
		}
		switch {
		case p.trialLive():
			last := time.Unix(max(p.Added, p.Trial.Seen), 0)
			if now.Sub(last) > TrialIdle {
				u.cfg.Logf("hub: %s's account wasn't used for %d days: closing it", p.Name, int(TrialIdle.Hours()/24))
				u.update(ctx, func(s *HubState) error { return s.Leave(p.Name, now) })
			} else if now.Sub(last) > TrialIdle-TrialWarn && p.Trial.Warned == 0 {
				u.warnTrial(ctx, p, last.Add(TrialIdle), now)
			}
		case p.leaving() && !p.Removed:
			left := deleteStubs(p)
			if left > 0 && now.Sub(time.Unix(p.Trial.Leaving, 0)) < LeaveFor {
				continue
			}
			u.update(ctx, func(s *HubState) error {
				if err := s.RemovePerson(p.Name); err != nil {
					return err
				}
				s.People[s.person(p.Name)].Trial.Gone = now.Unix()
				return nil
			})
			u.cfg.Logf("hub: %s's account is closed: their files are deleted from the group", p.Name)
		case p.Removed && p.Trial.Gone != 0:
			// The watcher has stopped: the folder goes, and once the
			// group has their name back, so does the person.
			if _, err := os.Stat(p.Path); err == nil && strings.HasPrefix(p.Path, u.cfg.Dir+string(filepath.Separator)) {
				os.RemoveAll(p.Path)
			}
			list, _ := peers.Load(u.cfg.PeersPath)
			if _, named := peers.PersonNameOf(list, u.selfIP(), p.Name); named && now.Sub(time.Unix(p.Trial.Gone, 0)) < LeaveFor {
				continue
			}
			u.update(ctx, func(s *HubState) error {
				s.People = slices.DeleteFunc(s.People, func(x Person) bool { return strings.EqualFold(x.Name, p.Name) && x.Removed })
				return nil
			})
		}
	}
	if n := len(st.Signups); n > 0 {
		u.update(ctx, func(s *HubState) error { s.pruneSignups(now); return nil })
	}
}

func (u *Users) update(ctx context.Context, f func(*HubState) error) {
	if _, err := Update(ctx, u.cfg.StatePath, f); err != nil {
		u.cfg.Logf("hub: %v", err)
		return
	}
	u.lightChanged()
}

// warnTrial asks the web nodes that send mail to tell p their account
// closes at until, unless they sign in.
func (u *Users) warnTrial(ctx context.Context, p Person, until, now time.Time) {
	if u.cfg.MailNodes == nil {
		return
	}
	list, _ := peers.Load(u.cfg.PeersPath)
	login := LoginOf(list, u.selfIP(), p.Name)
	for _, n := range u.cfg.MailNodes(ctx) {
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := light.SendTrialEmail(sctx, u.cfg.Client.HTTP, n, light.TrialEmail{To: p.Trial.Email, Kind: "warn", Login: login, Until: until.Unix()})
		cancel()
		if err != nil {
			u.cfg.Logf("hub: warning %s through %s: %v", p.Name, n, err)
			continue
		}
		u.update(ctx, func(s *HubState) error {
			if i := s.person(p.Name); i >= 0 && s.People[i].Trial != nil {
				s.People[i].Trial.Warned = now.Unix()
			}
			return nil
		})
		return
	}
}

// deleteStubs puts every stub in p's folder into its delete/, where the
// watcher deletes each from the group, and says how many are left there
// or elsewhere.
func deleteStubs(p Person) int {
	del := filepath.Join(p.Path, outbox.DeleteDir)
	os.MkdirAll(del, 0o700)
	left := 0
	filepath.WalkDir(p.Path, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() {
			if path != p.Path && strings.HasPrefix(e.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(e.Name(), files.StubExt) {
			return nil
		}
		left++
		if filepath.Dir(path) == del {
			return nil
		}
		rel, _ := filepath.Rel(p.Path, path)
		to := filepath.Join(del, strings.ReplaceAll(filepath.ToSlash(rel), "/", "_"))
		if _, err := os.Stat(to); err == nil {
			to = strings.TrimSuffix(to, files.StubExt) + fmt.Sprintf("-%d", time.Now().UnixNano()) + files.StubExt
		}
		os.Rename(path, to)
		return nil
	})
	return left
}

// trialSeen notes that p used their account, at most hourly.
func (f *filesPage) trialSeen(ctx context.Context, p Person, now time.Time) {
	if !p.trialLive() || now.Sub(time.Unix(p.Trial.Seen, 0)) < time.Hour {
		return
	}
	Update(ctx, f.u.cfg.StatePath, func(s *HubState) error {
		if i := s.person(p.Name); i >= 0 && s.People[i].Trial != nil {
			s.People[i].Trial.Seen, s.People[i].Trial.Warned = now.Unix(), 0
		}
		return nil
	})
}

// signup and confirm answer a web node at my. (light.Relay).
func (f *filesPage) signup(w http.ResponseWriter, r *http.Request, now time.Time) {
	list, _ := peers.Load(f.u.cfg.PeersPath)
	if tb, ok := peers.TrialBox(list); !ok || tb.IP() != f.self {
		http.Error(w, "this box doesn't take accounts", http.StatusNotFound)
		return
	}
	var req SignupRequest
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var su Signup
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		var err error
		su, err = s.SignUp(list, req, now)
		return err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	f.u.cfg.Logf("hub: %s signed up for an account (from %s)", su.Label, r.Header.Get("X-Forwarded-For"))
	jsonOK(w, SignupAnswer{Label: su.Label, Email: su.Email, Token: su.Token})
}

func (f *filesPage) confirm(w http.ResponseWriter, r *http.Request, now time.Time) {
	var req struct {
		T string `json:"t"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var p Person
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		var err error
		p, err = s.Confirm(req.T, f.u.cfg.Dir, now)
		return err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	f.u.lightChanged()
	f.u.cfg.Logf("hub: %s confirmed their account (%s)", p.Name, p.Trial.Email)
	jsonOK(w, Confirmed{Person: p.Name, Password: *p.Password})
}

// leave closes the signed-in person's own account.
func (f *filesPage) leave(w http.ResponseWriter, r *http.Request, p Person, now time.Time) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if _, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error { return s.Leave(p.Name, now) }); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	f.u.lightChanged()
	f.u.cfg.Logf("hub: %s closed their account", p.Name)
	w.WriteHeader(http.StatusNoContent)
}
