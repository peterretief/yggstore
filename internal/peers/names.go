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
	if len(p.Names) > 0 && !strings.HasPrefix(p.Code, "ys1") {
		return errors.New(`a node with names needs its owner's sharing code ("code": "ys1…")`)
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

// NameOwner is the peer that holds name (anna.example.org).
func NameOwner(list []Peer, name string) (Peer, bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, p := range list {
		for _, n := range p.Names {
			if n == name {
				return p, true
			}
		}
	}
	return Peer{}, false
}

// MailOwner is the peer whose name an address (anna@example.org) is.
func MailOwner(list []Peer, addr string) (Peer, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	if !ok || ValidLabel(local) != nil {
		return Peer{}, false
	}
	return NameOwner(list, local+"."+domain)
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
