package peers

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Names: the admin gives members names under the group's domain. A name
// such as anna.example.org is both a mailbox (anna@example.org) and a
// website (anna.example.org), and belongs to the node whose entry lists it.
// Only an admin's push changes the list, so only the admin gives names.
//
// A box's node can also hold names for the people who use the box without
// a node of their own (PeopleNames): the node serves their site and takes
// their mail, but both are sealed for the person's own sharing code, so
// only they read it.
//
// A name can also be given as a site only (a node's Sites, or a PersonName
// with Site set): it is a website and nothing else. It takes no mail and
// nobody signs in at it, though its contact form's messages may be
// forwarded from its address. Each person has at most one name that isn't
// a site, their address, and any number of sites.

// PersonName is a name given to one person on a box.
type PersonName struct {
	Name   string `json:"name"`   // anna.example.org
	Person string `json:"person"` // as the box calls them
	Code   string `json:"code"`   // their sharing code
	Site   bool   `json:"site,omitempty"`
}

var (
	label     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidLabel checks the part a member chooses: lower-case letters, digits
// and hyphens, up to 32, not starting or ending with a hyphen.
func ValidLabel(l string) error {
	if !label.MatchString(l) {
		return errors.New("use lower-case letters, digits and hyphens (up to 32), not starting or ending with a hyphen")
	}
	return nil
}

// ValidDomain checks a domain name such as example.org.
func ValidDomain(d string) error {
	parts := strings.Split(d, ".")
	if len(parts) < 2 {
		return fmt.Errorf("%q is not a domain like example.org", d)
	}
	for _, p := range parts {
		if !hostLabel.MatchString(p) {
			return fmt.Errorf("%q is not a domain like example.org", d)
		}
	}
	return nil
}

func validNames(p Peer, owners map[string]bool) error {
	if p.Domain != "" {
		if !p.Admin {
			return errors.New("only an admin's entry sets the group's domain")
		}
		if err := ValidDomain(p.Domain); err != nil {
			return fmt.Errorf("domain: %w", err)
		}
	}
	for _, n := range p.Names {
		l, dom, ok := strings.Cut(n, ".")
		if !ok || ValidLabel(l) != nil || ValidDomain(dom) != nil {
			return fmt.Errorf("name %q is not like anna.example.org", n)
		}
		if owners[n] {
			return fmt.Errorf("name %s given twice", n)
		}
		owners[n] = true
	}
	for _, n := range p.Sites {
		l, dom, ok := strings.Cut(n, ".")
		if !ok || ValidLabel(l) != nil || ValidDomain(dom) != nil {
			return fmt.Errorf("site %q is not like shop.example.org", n)
		}
		if owners[n] {
			return fmt.Errorf("name %s given twice", n)
		}
		owners[n] = true
	}
	if len(p.Names)+len(p.Sites) > 0 && !strings.HasPrefix(p.Code, "ys1") {
		return errors.New(`a node with names needs its owner's sharing code ("code": "ys1…")`)
	}
	people := map[string]bool{}
	for _, pn := range p.PeopleNames {
		l, dom, ok := strings.Cut(pn.Name, ".")
		if !ok || ValidLabel(l) != nil || ValidDomain(dom) != nil {
			return fmt.Errorf("name %q is not like anna.example.org", pn.Name)
		}
		if owners[pn.Name] {
			return fmt.Errorf("name %s given twice", pn.Name)
		}
		owners[pn.Name] = true
		if pn.Person == "" {
			return fmt.Errorf("name %s: say which person on the box it is for", pn.Name)
		}
		if !pn.Site {
			if people[strings.ToLower(pn.Person)] {
				return fmt.Errorf("name %s: each person on a box has one address (any more are sites)", pn.Name)
			}
			people[strings.ToLower(pn.Person)] = true
		}
		if !strings.HasPrefix(pn.Code, "ys1") {
			return fmt.Errorf(`name %s needs its person's sharing code ("code": "ys1…")`, pn.Name)
		}
	}
	return nil
}

// GroupDomain is the domain the group's names are under: the first admin's
// "domain", or "" if none is set.
func GroupDomain(list []Peer) string {
	for _, p := range list {
		if p.Admin && p.Domain != "" {
			return p.Domain
		}
	}
	return ""
}

// NameOwner is the peer that holds name (anna.example.org): the node it
// was given to, or the box of the person it was given to.
func NameOwner(list []Peer, name string) (Peer, bool) {
	p, _, ok := NameHolder(list, name)
	return p, ok
}

// NameHolder is the peer that holds name and the sharing code its mail and
// contact forms are sealed for: the person's own, for a name given to
// someone on a box (person is then their name on the box).
func NameHolder(list []Peer, name string) (p Peer, person PersonName, ok bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, p := range list {
		for _, n := range p.Names {
			if n == name {
				return p, PersonName{Name: n, Code: p.Code}, true
			}
		}
		for _, n := range p.Sites {
			if n == name {
				return p, PersonName{Name: n, Code: p.Code, Site: true}, true
			}
		}
		for _, pn := range p.PeopleNames {
			if pn.Name == name {
				return p, pn, true
			}
		}
	}
	return Peer{}, PersonName{}, false
}

// PersonNameOf is the address node's entry gives person, if any: their
// one name that isn't a site.
func PersonNameOf(list []Peer, node, person string) (PersonName, bool) {
	for _, pn := range PersonNamesOf(list, node, person) {
		if !pn.Site {
			return pn, true
		}
	}
	return PersonName{}, false
}

// PersonNamesOf is every name node's entry gives person: their address
// first, if they have one, then their sites.
func PersonNamesOf(list []Peer, node, person string) []PersonName {
	var addr, sites []PersonName
	for _, p := range list {
		if p.IP() != node {
			continue
		}
		for _, pn := range p.PeopleNames {
			if !strings.EqualFold(pn.Person, person) {
				continue
			}
			if pn.Site {
				sites = append(sites, pn)
			} else {
				addr = append(addr, pn)
			}
		}
	}
	return append(addr, sites...)
}

// MailOwner is the peer that may send as an address (anna@example.org):
// the holder of the name, a site's included, so a site's contact form
// messages can be forwarded from it.
func MailOwner(list []Peer, addr string) (Peer, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	if !ok || ValidLabel(local) != nil {
		return Peer{}, false
	}
	p, _, ok := NameHolder(list, local+"."+domain)
	return p, ok
}

// MailHolder is the peer that takes an address's mail, with the code it
// is sealed for. A site takes no mail.
func MailHolder(list []Peer, addr string) (Peer, PersonName, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	if !ok || ValidLabel(local) != nil {
		return Peer{}, PersonName{}, false
	}
	p, pn, ok := NameHolder(list, local+"."+domain)
	if !ok || pn.Site {
		return Peer{}, PersonName{}, false
	}
	return p, pn, true
}

// MailAddress is the address a name receives mail at.
func MailAddress(name string) string {
	l, dom, _ := strings.Cut(name, ".")
	return l + "@" + dom
}

// SiteKeeper says who may publish a site: under the group's domain (the
// domain itself, its names, and www. of either) only the node the list
// gives the name to, and admins. under is false for other domains, which
// go to whoever published them first.
func SiteKeeper(list []Peer, site string) (owner Peer, given, under bool) {
	domain := GroupDomain(list)
	site = strings.ToLower(strings.TrimSuffix(site, "."))
	if domain == "" || (site != domain && !strings.HasSuffix(site, "."+domain)) {
		return Peer{}, false, false
	}
	if p, ok := NameOwner(list, site); ok {
		return p, true, true
	}
	if p, ok := NameOwner(list, strings.TrimPrefix(site, "www.")); ok {
		return p, true, true
	}
	return Peer{}, false, true
}
