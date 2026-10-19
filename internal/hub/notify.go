package hub

import (
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"time"
)

// The group's admin hears how connecting goes on a box: when someone
// fetches the connect script, when their device pairs or is turned away,
// and when a script was fetched but no device paired after it.

// TypeBoxNote is a note from a box to the group's admins, for their
// Messages page.
const TypeBoxNote = "box-note"

// Fetch is a connect script someone fetched from the box.
type Fetch struct {
	IP     string `json:"ip"`
	Script string `json:"script"`
	Time   int64  `json:"time"`
	// Done: a device paired since, or the admin was told none did.
	Done bool `json:"done,omitempty"`
}

const (
	// notPairedAfter is how long after fetching the script the admin hears
	// that no device paired.
	notPairedAfter = 15 * time.Minute
	keepFetches    = 7 * 24 * time.Hour
)

// Fetched notes that someone fetched a connect script, and tells the admin.
func (u *Users) Fetched(remoteAddr, script string) {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		ip = remoteAddr
	}
	now := time.Now()
	again := false
	Update(context.Background(), u.cfg.StatePath, func(s *HubState) error {
		for _, f := range s.Fetches {
			again = again || (f.IP == ip && !f.Done && now.Sub(time.Unix(f.Time, 0)) < time.Hour)
		}
		s.Fetches = append(s.Fetches, Fetch{IP: ip, Script: script, Time: now.Unix()})
		s.Fetches = slices.DeleteFunc(s.Fetches, func(f Fetch) bool { return now.Sub(time.Unix(f.Time, 0)) > keepFetches })
		return nil
	})
	u.cfg.Logf("hub: %s fetched %s", ip, script)
	if !again {
		u.tell(fmt.Sprintf("Someone at %s on %s's network is running %s, to connect a computer to the box.", ip, boxName(), script))
	}
}

// paired closes the fetches before a device paired: whoever fetched the
// script got through, even if their device answers from another address.
func (s *HubState) paired(at time.Time) {
	for i := range s.Fetches {
		if s.Fetches[i].Time <= at.Unix() {
			s.Fetches[i].Done = true
		}
	}
}

// checkFetches tells the admin about scripts fetched a while ago that no
// device paired after.
func (u *Users) checkFetches(ctx context.Context) {
	var stuck []Fetch
	now := time.Now()
	Update(ctx, u.cfg.StatePath, func(s *HubState) error {
		stuck = nil
		for i, f := range s.Fetches {
			if !f.Done && now.Sub(time.Unix(f.Time, 0)) > notPairedAfter {
				s.Fetches[i].Done = true
				stuck = append(stuck, f)
			}
		}
		return nil
	})
	for _, f := range stuck {
		u.tell(fmt.Sprintf("Someone at %s fetched %s from %s at %s, but no computer has connected since. They may be stuck: the script's window says why.",
			f.IP, f.Script, boxName(), time.Unix(f.Time, 0).Format("15:04")))
	}
}

// tell sends the group's admins a note, in the background.
func (u *Users) tell(text string) {
	if u.cfg.Tell == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := u.cfg.Tell(ctx, text); err != nil {
			u.cfg.Logf("hub: couldn't tell the group's admin %q: %v", text, err)
		}
	}()
}

// tellOnce tells the admin something about a device once while the hub
// runs (a device waiting for the day's limit is seen every round).
func (u *Users) tellOnce(key, text string) {
	u.mu.Lock()
	if u.told == nil {
		u.told = map[string]bool{}
	}
	seen := u.told[key]
	u.told[key] = true
	u.mu.Unlock()
	if !seen {
		u.tell(text)
	}
}

func boxName() string {
	host, _ := os.Hostname()
	if host == "" {
		return "the box"
	}
	return host
}
