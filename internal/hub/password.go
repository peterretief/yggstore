package hub

import (
	"errors"
	"time"

	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

// Passwords (see docs/web-access.md): the group's admin, or the box's
// owner, sets a temporary password for someone and gives it to them
// directly. They sign in on the web with their group address and it, and
// choose their own at once. A new temporary one is how someone who forgot
// theirs gets back in.

// CanSignIn is whether p has a way to sign in on the web.
func (p Person) CanSignIn() bool { return len(p.Passkeys) > 0 || p.Password != nil }

// SetTempPassword gives person a new temporary password, ending any
// earlier one and the sessions it started. It returns the password, which
// isn't kept.
func (s *HubState) SetTempPassword(person string, now time.Time) (string, error) {
	i := s.person(person)
	if i < 0 {
		return "", errors.New("nobody called " + person + " on this box")
	}
	if p := s.People[i]; !p.Active() {
		return "", errors.New(p.Name + " is suspended or removed: turn them back on first")
	}
	pw, kept := password.NewTemp(now)
	s.People[i].Password = &kept
	return pw, nil
}

// ChoosePassword takes the password p chose, signed in with the one called
// was (password.Password.ID), as a web node passes it on.
func (p *Person) ChoosePassword(was string, chosen password.Password) error {
	if p.Password == nil || p.Password.ID() != was {
		return errors.New("that password was changed already: sign in again")
	}
	if chosen.Temp || chosen.Hash == "" {
		return errors.New("not a chosen password")
	}
	p.Password = &chosen
	return nil
}

// LoginOf is what person on the box at node signs in with: their group
// address, or person@box without one.
func LoginOf(list []peers.Peer, node, person string) string {
	if pn, ok := peers.PersonNameOf(list, node, person); ok {
		return peers.MailAddress(pn.Name)
	}
	for _, p := range list {
		if p.IP() == node {
			return person + "@" + p.Name
		}
	}
	return person
}

// SignInAt is where people sign in on the web: the group's login, or ""
// with no group domain.
func SignInAt(list []peers.Peer) string {
	if d := peers.GroupDomain(list); d != "" {
		return "https://" + light.Host + "." + d
	}
	return ""
}
