package hub

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/websites"
)

// MoveHolder is the box's people's sites, for moving a site to or from
// someone on the box (see package sitemove). The node's own sites are its
// dashboard's.
type MoveHolder struct{ PS *PeopleSites }

func (h MoveHolder) person(name string) (Person, error) {
	st, err := Load(h.PS.cfg.StatePath)
	if err != nil {
		return Person{}, err
	}
	p, ok := st.PersonNamed(name)
	if !ok {
		return Person{}, fmt.Errorf("nobody called %q is on this box", name)
	}
	return p, nil
}

func (h MoveHolder) Handles(person string) bool { return person != "" }

func (h MoveHolder) Publisher(name string) (*site.Publisher, error) {
	p, err := h.person(name)
	if err != nil {
		return nil, err
	}
	ann, err := h.PS.cfg.Announcer()
	if err != nil {
		return nil, err
	}
	return &site.Publisher{Dir: filepath.Join(h.PS.cfg.Dir, p.Folder, "sites"), Client: h.PS.cfg.Client, Announce: ann,
		Log: h.PS.cfg.Logf, Skip: websites.Skip}, nil
}

func (h MoveHolder) Identity(name string) (*share.Identity, error) {
	p, err := h.person(name)
	if err != nil {
		return nil, err
	}
	return PersonKey(h.PS.cfg.StatePath, p)
}

func (h MoveHolder) Name(name string) string {
	if p, err := h.person(name); err == nil {
		return p.Name
	}
	return name
}

func (h MoveHolder) CanTake(name string) error {
	p, err := h.person(name)
	if err != nil {
		return err
	}
	if !p.Active() {
		return fmt.Errorf("%s can't use the box now", p.Name)
	}
	if h.PS.cfg.NodeMail == "" {
		return errors.New("this box's node collects no mail, so the site's contact form would stop")
	}
	return nil
}
