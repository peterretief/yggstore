package hub

import (
	"context"
	"errors"
	"strings"

	"github.com/peterretief/yggstore/internal/site"
)

// A person's site's versions (see docs/site-code.md, stage 0): going back
// to an earlier one, and deciding about a change held back because it
// looked like a mistake. Each is on their website page, and the box's
// admin can ask for it on the command line (Person.SiteAsk).

// Versions are person's site's versions, newest first.
func (ps *PeopleSites) Versions(p Person) ([]site.Version, error) {
	r := ps.site(p)
	if r == nil {
		return nil, errors.New("you have no website yet")
	}
	return r.pub.Versions(r.name)
}

// GoBack serves an earlier version of person's site again: id (or a
// prefix of it), or with "" the one before the version being served. Their
// site's folder then holds it too; what was in the folder is kept on the
// box (DIR/FOLDER/replaced).
func (ps *PeopleSites) GoBack(ctx context.Context, p Person, id string) (site.Version, error) {
	r := ps.site(p)
	if r == nil {
		return site.Version{}, errors.New("you have no website yet")
	}
	v, err := r.pub.Rollback(ctx, r.name, id)
	if err != nil {
		return v, err
	}
	r.w.Refill(r.name)
	ps.cfg.Logf("%s: %s went back to version %s", p.Name, r.name, v.ID[:min(8, len(v.ID))])
	return v, nil
}

// PublishAnyway publishes person's site's folder as it is, past the checks
// that held a change back.
func (ps *PeopleSites) PublishAnyway(p Person) error {
	r := ps.site(p)
	if r == nil {
		return errors.New("you have no website yet")
	}
	r.w.Force(r.name)
	return nil
}

// KeepServed drops a change held back: the folder holds the version being
// served again, and what was in it is kept on the box.
func (ps *PeopleSites) KeepServed(p Person) error {
	r := ps.site(p)
	if r == nil {
		return errors.New("you have no website yet")
	}
	r.w.Refill(r.name)
	return nil
}

// siteAsk does what the box's admin asked for person's site on the command
// line: "back" or "back:ID", "anyway", or "keep".
func (ps *PeopleSites) siteAsk(ctx context.Context, p Person, ask string) error {
	switch what, id, _ := strings.Cut(ask, ":"); what {
	case "back":
		_, err := ps.GoBack(ctx, p, id)
		return err
	case "anyway":
		return ps.PublishAnyway(p)
	case "keep":
		return ps.KeepServed(p)
	}
	return errors.New("unknown request " + ask)
}
