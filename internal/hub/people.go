package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/files"
)

// The box's admin looks after the people on it, from the box's dashboard
// (which opens only on the admin's own devices) or its command line: adds
// them, sends each an invite link that lets their first device open their
// files from anywhere over Yggdrasil, sees what they store and when their
// devices were last on, limits how much they store and how many devices
// they let in, suspends them, and removes them. A removed person's files
// stay on the box.

// Invite lets one device in as a person, once, within a week.
type Invite struct {
	Person  string `json:"person"`
	Hash    string `json:"hash"` // of the token, which only the link has
	Expires int64  `json:"expires"`
}

// InviteFor is how long an invite link works.
const InviteFor = 7 * 24 * time.Hour

// JoinPath is the invite link's page on the box.
const JoinPath = FilesPath + "/join"

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// person is the index of the person called name (ignoring case), or -1.
func (s *HubState) person(name string) int {
	for i, p := range s.People {
		if strings.EqualFold(p.Name, name) {
			return i
		}
	}
	return -1
}

// AddPerson adds someone, with an empty folder in dir, who opens it from
// their devices once let in (see Invite).
func (s *HubState) AddPerson(name, dir string) (Person, error) {
	name = strings.TrimSpace(name)
	if name == "" || unsafeName.MatchString(name) || strings.Trim(name, ".-") != name {
		return Person{}, fmt.Errorf("%q can't be a name here: use letters, digits, - and _", name)
	}
	if i := s.person(name); i >= 0 {
		if !s.People[i].Removed {
			return Person{}, fmt.Errorf("there is someone called %s already", s.People[i].Name)
		}
		// Back again: their files are where they were.
		s.People[i].Removed, s.People[i].Suspended = false, false
		return s.People[i], nil
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		return Person{}, fmt.Errorf("%s is taken by a folder on the box", filepath.Join(dir, name))
	}
	p := Person{Name: name, Folder: "yggstore-p-" + shortHash(name), Path: filepath.Join(dir, name), Added: time.Now().Unix()}
	if err := os.MkdirAll(filepath.Join(p.Path, "files"), 0o700); err != nil {
		return Person{}, err
	}
	s.People = append(s.People, p)
	return p, nil
}

// Invite makes a link token that lets one device in as person, replacing
// any earlier invite of theirs.
func (s *HubState) Invite(person string, now time.Time) (string, error) {
	i := s.person(person)
	if i < 0 {
		return "", fmt.Errorf("nobody called %q on this box", person)
	}
	if p := s.People[i]; p.Removed || p.Suspended {
		return "", fmt.Errorf("%s is %s: turn them back on first", p.Name, map[bool]string{true: "removed", false: "suspended"}[p.Removed])
	}
	b := make([]byte, 18)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	s.DropInvite(s.People[i].Name)
	s.Invites = append(s.Invites, Invite{Person: s.People[i].Name, Hash: tokenHash(token), Expires: now.Add(InviteFor).Unix()})
	return token, nil
}

// DropInvite drops person's invite, if any, and those expired.
func (s *HubState) DropInvite(person string) {
	kept := s.Invites[:0]
	for _, in := range s.Invites {
		if !strings.EqualFold(in.Person, person) && time.Now().Unix() < in.Expires {
			kept = append(kept, in)
		}
	}
	s.Invites = kept
}

// InviteOf is when person's invite stops working, if they have one.
func (s HubState) InviteOf(person string) (time.Time, bool) {
	for _, in := range s.Invites {
		if strings.EqualFold(in.Person, person) && time.Now().Unix() < in.Expires {
			return time.Unix(in.Expires, 0), true
		}
	}
	return time.Time{}, false
}

var errBadInvite = errors.New("This invite link doesn't work (any more): it works once, for a week. Ask the box's owner for a new one.")

// UseInvite lets the device at addr in with the invite token, once.
func (s *HubState) UseInvite(token, addr, name string, now time.Time) (Person, error) {
	h := tokenHash(token)
	for j, in := range s.Invites {
		if in.Hash != h {
			continue
		}
		if now.Unix() >= in.Expires {
			break
		}
		if other, ok := s.PersonAt(addr); ok && !strings.EqualFold(other.Name, in.Person) {
			return Person{}, fmt.Errorf("This device opens %s's files already. Use another device, or ask the box's owner.", other.Name)
		}
		if _, err := s.LetIn(in.Person, addr, name); err != nil {
			return Person{}, err
		}
		s.Invites = append(s.Invites[:j], s.Invites[j+1:]...)
		p, _ := s.PersonNamed(in.Person)
		return p, nil
	}
	return Person{}, errBadInvite
}

// Limit sets how much person may store (bytes) and how many devices they
// may let in; 0 is no limit, and a negative value leaves it as it is.
func (s *HubState) Limit(person string, quota int64, maxDevices int) error {
	i := s.person(person)
	if i < 0 {
		return fmt.Errorf("nobody called %q on this box", person)
	}
	if quota >= 0 {
		s.People[i].Quota = quota
	}
	if maxDevices >= 0 {
		s.People[i].MaxDevices = maxDevices
	}
	return nil
}

// Suspend shuts person out for now (on) or lets them back (off). Their
// devices stay on their list.
func (s *HubState) Suspend(person string, on bool) error {
	i := s.person(person)
	if i < 0 {
		return fmt.Errorf("nobody called %q on this box", person)
	}
	if on {
		s.DropInvite(s.People[i].Name)
	}
	s.People[i].Suspended = on
	return nil
}

// RemovePerson shuts person out for good: their devices, browser and
// Syncthing alike, and their invite go. Their files stay on the box.
func (s *HubState) RemovePerson(person string) error {
	i := s.person(person)
	if i < 0 {
		return fmt.Errorf("nobody called %q on this box", person)
	}
	p := &s.People[i]
	p.Removed, p.FilesDevices, p.Passkeys = true, nil, nil
	s.DropInvite(p.Name)
	for j := range s.Users {
		if s.Users[j].Person == p.Name {
			s.Users[j].Remove = true
		}
	}
	return nil
}

// Active is whether the person may use the box.
func (p Person) Active() bool { return !p.Suspended && !p.Removed }

// Usage is what a person stores: the bytes and the number of files, stored
// or waiting to be. Restored copies and the box's own files don't count.
func Usage(p Person) (bytes int64, n int) {
	filepath.WalkDir(p.Path, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := e.Name()
		if e.IsDir() {
			if path != p.Path && (strings.HasPrefix(name, ".") || (filepath.Dir(path) == p.Path && name == "restored")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, files.ChallengesExt) {
			return nil
		}
		if strings.HasSuffix(name, files.StubExt) {
			if m, err := files.ReadStub(path); err == nil {
				if m.Kind == files.KindFolder {
					bytes += m.ContentBytes
				} else {
					bytes += int64(m.PlaintextSize)
				}
				n++
			}
			return nil
		}
		if _, err := os.Stat(path + files.StubExt); err == nil {
			return nil // a new version of a stored file: counted when stored
		}
		if info, err := e.Info(); err == nil {
			bytes += info.Size()
			n++
		}
		return nil
	})
	return bytes, n
}
