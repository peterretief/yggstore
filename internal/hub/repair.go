package hub

import (
	"path/filepath"

	"github.com/peterretief/yggstore/internal/repair"
)

// The box looks after what it stores for its people and teams as a
// dashboard does for its owner (see package repair): it rebuilds shards on
// nodes gone and renews the leases. A person's files it repairs only while
// it holds their working copy, since repairing rewrites stubs; while
// they're worked on elsewhere, that place repairs them, and the box only
// renews their leases.

// Stubs lists the stubs the box looks after: mine to repair and renew,
// renew to only renew. Shares received are among them; repair leaves
// those to their senders.
func (u *Users) Stubs() (mine, renew []string) {
	st, err := Load(u.cfg.StatePath)
	if err != nil {
		return nil, nil
	}
	for _, p := range st.People {
		found := repair.Find(p.Path, filepath.Join(p.Path, ".yggstore", "history"), filepath.Join(homeOf(p), "recovery"))
		if u.Holding(p.Name) {
			mine = append(mine, found...)
		} else {
			renew = append(renew, found...)
		}
	}
	for _, t := range st.Teams {
		mine = append(mine, repair.Find(t.Path, filepath.Join(t.Path, ".yggstore", "history"))...)
	}
	return mine, renew
}
