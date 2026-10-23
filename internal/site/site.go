// Package site hosts static websites on the group. A site is a folder,
// stored as one item like any other, so every version is kept and an update
// costs only what changed. Publishing announces the version on the "sites"
// topic; web nodes fetch it, unpack it beside the one they serve, switch over
// at once, and serve it to visitors by domain name.
package site

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	netmail "net/mail"
	"regexp"
	"slices"
	"strings"

	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/share"
)

// Topic is where sites are announced.
const Topic = "sites"

const (
	typePublish = "site"
	typeRemove  = "site-remove"
	typeOwner   = "site-owner"
	typeAllow   = "site-allow-code"
)

// CodeAllowance, from an admin, lets a site's handlers run on the web
// nodes though another node publishes it (On), or stops them. The admin's
// own sites need none.
type CodeAllowance struct {
	Site string `json:"site"`
	On   bool   `json:"on"`
}

// EncodeAllowance is a code allowance as a message on Topic.
func EncodeAllowance(c CodeAllowance) (typ, body string, err error) {
	if err := ValidName(c.Site); err != nil {
		return "", "", err
	}
	b, err := json.Marshal(c)
	return typeAllow, string(b), err
}

// OwnerChange, from an admin, gives a site on a domain of its own to
// another node: web nodes take its next versions from that node, and send
// its contact form there. (A name under the group's domain moves with the
// member list instead; see docs/move-site.md.)
type OwnerChange struct {
	Site  string `json:"site"`
	Owner string `json:"owner"` // the node's overlay IP
}

// EncodeOwner is an owner change as a message on Topic.
func EncodeOwner(c OwnerChange) (typ, body string, err error) {
	if err := ValidName(c.Site); err != nil {
		return "", "", err
	}
	if net.ParseIP(c.Owner) == nil {
		return "", "", fmt.Errorf("%q isn't a node's address", c.Owner)
	}
	b, err := json.Marshal(c)
	return typeOwner, string(b), err
}

// Announcement is the body of a message on Topic.
type Announcement struct {
	Site string             `json:"site"`
	Stub *manifest.Manifest `json:"stub,omitempty"` // the version to serve; nil when removed
	// Contact, if set, is the publisher's sharing code: the site's contact
	// form is on, and its messages are sealed for that code and go to the
	// publishing node's mailbox (see ContactPath).
	Contact string `json:"contact,omitempty"`
	// Uploaders, if set, are the group addresses (anna@example.org) that may
	// add photos and files to the site (see UploadPath).
	Uploaders []string `json:"uploaders,omitempty"`
	// Code asks the web nodes to run the site's handlers: a request for
	// /api/NAME runs api/NAME.wasm (see ApiPath).
	Code bool `json:"code,omitempty"`
}

var domain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidName reports whether name is a domain a site can be published as,
// such as example.org or www.example.org.
func ValidName(name string) error {
	if !domain.MatchString(name) || len(name) > 253 {
		return fmt.Errorf("%q is not a domain name: a site is published under the domain it is served at, such as example.org", name)
	}
	return nil
}

// Normalise turns a typed domain into the form sites are kept under.
func Normalise(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func encode(a Announcement) (typ, body string, err error) {
	b, err := json.Marshal(a)
	if err != nil {
		return "", "", err
	}
	if len(b) > msg.MaxBody {
		return "", "", errors.New("the site has too many parts to announce in one message; split it into smaller sites")
	}
	typ = typePublish
	if a.Stub == nil {
		typ = typeRemove
	}
	return typ, string(b), nil
}

func decode(s msg.Stored) (Announcement, bool) {
	var a Announcement
	if s.Topic != Topic || (s.Type != typePublish && s.Type != typeRemove) {
		return a, false
	}
	if json.Unmarshal([]byte(s.Body), &a) != nil || ValidName(a.Site) != nil {
		return a, false
	}
	if s.Type == typeRemove {
		a.Stub = nil
		return a, true
	}
	if a.Stub == nil {
		return a, false
	}
	// The stub comes from another member: check it as a stub read from disk
	// is checked. (Web.handle checks where its shards are.)
	b, err := manifest.Marshal(*a.Stub)
	if err != nil {
		return a, false
	}
	m, err := manifest.Unmarshal(b)
	if err != nil {
		return a, false
	}
	a.Stub = &m
	if a.Contact != "" {
		if _, err := share.ParseCode(a.Contact); err != nil {
			a.Contact = ""
		}
	}
	a.Uploaders = CleanUploaders(a.Uploaders)
	return a, true
}

// MaxUploaders is how many addresses a site's upload list holds.
const MaxUploaders = 200

// CleanUploaders keeps a list's valid addresses, lower-cased, once each.
func CleanUploaders(list []string) []string {
	var out []string
	for _, a := range list {
		a = strings.ToLower(strings.TrimSpace(a))
		if p, err := netmail.ParseAddress(a); err != nil || p.Address != a || len(a) > 254 || slices.Contains(out, a) {
			continue
		}
		if out = append(out, a); len(out) == MaxUploaders {
			break
		}
	}
	return out
}
