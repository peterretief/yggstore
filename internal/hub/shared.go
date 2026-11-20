package hub

import (
	"path/filepath"
	"strings"
	"time"
)

// Shared mailboxes: every box has support, sales and accounts (the hub's
// -shared-mail), whose local addresses (support@local.4 on localbox-004) take
// mail from inside the group. Anyone on the box's own network opens them
// from its info page (http://mail.local/info) with no password; nobody
// opens them from anywhere else (webmail.go).

// DefaultShared are the shared mailboxes a box has unless told otherwise.
const DefaultShared = "support,sales,accounts"

// SharedNames reads a comma-separated list of mailbox names.
func SharedNames(list string) []string {
	var out []string
	for _, n := range strings.Split(list, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" && !unsafeName.MatchString(n) && strings.Trim(n, ".-") == n {
			out = append(out, n)
		}
	}
	return out
}

// EnsureShared adds the shared mailboxes the box hasn't got, and gives
// back the ones it added and the ones it couldn't because a person there
// has the name already.
func (s *HubState) EnsureShared(names []string, dir string, now time.Time) (added, taken []string) {
	for _, n := range names {
		if i := s.person(n); i >= 0 {
			if !s.People[i].Shared {
				taken = append(taken, n)
			}
			continue
		}
		s.People = append(s.People, Person{Name: n, Folder: "yggstore-p-" + shortHash(n), Path: filepath.Join(dir, n),
			Added: now.Unix(), Shared: true})
		added = append(added, n)
	}
	return added, taken
}

// SharedPeople are the box's shared mailboxes that are on.
func (s HubState) SharedPeople() []Person {
	var out []Person
	for _, p := range s.People {
		if p.Shared && p.Active() {
			out = append(out, p)
		}
	}
	return out
}
