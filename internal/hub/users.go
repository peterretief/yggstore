package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/filelock"
	"github.com/peterretief/yggstore/internal/syncthing"
)

// User is a device paired with the hub: its Syncthing device and the folder
// on the hub it syncs with, which an outbox watcher stores into the group.
type User struct {
	Name    string `json:"name"`
	Device  string `json:"device"` // Syncthing device ID
	Folder  string `json:"folder"` // Syncthing folder ID
	Path    string `json:"path"`
	Added   int64  `json:"added"`
	Address string `json:"address,omitempty"` // where it paired from
	// Remove is set by "yggstore hub remove"; the hub then forgets the
	// device. Its person's folder and files stay.
	Remove bool `json:"remove,omitempty"`
	// Person is whose device it is: the device syncs their folder.
	Person string `json:"person,omitempty"`
}

// Person is someone who uses the box, from one or more devices. They have
// one folder on the box, which every device of theirs syncs.
type Person struct {
	Name   string `json:"name"`
	Folder string `json:"folder"` // Syncthing folder ID
	Path   string `json:"path"`
	Added  int64  `json:"added"`
}

// Switches are what the hub's owner turns on and off.
type Switches struct {
	// Pairing off: new devices are not accepted. On by default.
	PairingOff bool `json:"pairing_off,omitempty"`
	// Paused: every user's folder stops syncing.
	Paused bool `json:"paused,omitempty"`
}

// HubState is kept in the hub's state file, shared with the hub commands.
type HubState struct {
	Switches
	Users  []User   `json:"users"` // devices
	People []Person `json:"people,omitempty"`
	Teams  []Team   `json:"teams,omitempty"`
	Sites  []Site   `json:"sites,omitempty"`
}

// Team is a folder several people work in, on this box: every device of
// every member syncs it. Unlike a person's folder it never moves to
// another place, so when one member works elsewhere the others carry on.
type Team struct {
	Name    string   `json:"name"`
	Folder  string   `json:"folder"` // Syncthing folder ID
	Path    string   `json:"path"`
	Members []string `json:"members"` // people's names
	Added   int64    `json:"added"`
	// Remove is set by "yggstore hub team remove"; the hub then stops
	// syncing the folder, keeping its files.
	Remove bool `json:"remove,omitempty"`
}

// TeamNamed finds a team by name, ignoring case.
func (s HubState) TeamNamed(name string) (Team, int, bool) {
	for i, t := range s.Teams {
		if strings.EqualFold(t.Name, name) {
			return t, i, true
		}
	}
	return Team{}, -1, false
}

// TeamFolderID is the Syncthing folder of a team called name.
func TeamFolderID(name string) string {
	return "yggstore-team-" + strings.ToLower(unsafeName.ReplaceAllString(name, "-"))
}

// DevicesOf lists the devices of a person that aren't being removed.
func (s HubState) DevicesOf(person string) []User {
	var out []User
	for _, us := range s.Users {
		if us.Person == person && !us.Remove {
			out = append(out, us)
		}
	}
	return out
}

// PersonNamed finds a person by name, ignoring case.
func (s HubState) PersonNamed(name string) (Person, bool) {
	for _, p := range s.People {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Person{}, false
}

// Update changes the state file under its lock.
func Update(ctx context.Context, path string, f func(*HubState) error) (HubState, error) {
	unlock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return HubState{}, err
	}
	defer unlock()
	st, err := Load(path)
	if err != nil {
		return st, err
	}
	if err := f(&st); err != nil {
		return st, err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return st, atomicfile.Replace(path, append(b, '\n'), 0o600)
}

// Load reads the state file; a missing one is a hub with nobody yet.
func Load(path string) (HubState, error) {
	var st HubState
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// UsersConfig is the hub's pairing side.
type UsersConfig struct {
	ST        syncthing.Client
	Dir       string // the users' folders are made in here
	StatePath string
	// Limit is how many devices may pair in a day.
	Limit int
	// Watch stores what arrives in a person's folder, until ctx ends.
	Watch func(ctx context.Context, p Person)
	// Websites is the hub's websites/ folder, shared with every paired
	// device once WebsitesReady (its sites' folders hold the versions
	// being served, so nobody syncs an empty folder over a live site).
	// "": none.
	Websites      string
	WebsitesReady func() bool
	Logf          func(format string, args ...any)
}

type Users struct {
	cfg UsersConfig

	mu       sync.Mutex
	myID     string
	err      string // the last problem talking to Syncthing
	watchers map[string]context.CancelFunc
	shared   map[string]string // per folder: the devices last shared with, and paused
}

func NewUsers(cfg UsersConfig) *Users {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Users{cfg: cfg, watchers: map[string]context.CancelFunc{}, shared: map[string]string{}}
}

// Status is what the hub page shows of pairing.
type Status struct {
	DeviceID string       `json:"device_id,omitempty"` // the hub's Syncthing ID, to add on a device
	Pairing  bool         `json:"pairing"`
	Paused   bool         `json:"paused"`
	Users    int          `json:"users"` // devices
	People   int          `json:"people"`
	Teams    []string     `json:"teams"`     // team folders' names
	Conflict int          `json:"conflicts"` // conflict copies waiting for someone to choose
	Problem  string       `json:"problem,omitempty"`
	Sites    []SiteStatus `json:"sites"`
}

// SiteStatus is a site as the hub page shows it (sites are public anyway).
type SiteStatus struct {
	Name      string `json:"name"`
	Published int64  `json:"published,omitempty"`
	Problem   string `json:"problem,omitempty"`
}

func (u *Users) Status() Status {
	st, err := Load(u.cfg.StatePath)
	u.mu.Lock()
	defer u.mu.Unlock()
	s := Status{DeviceID: u.myID, Pairing: !st.PairingOff, Paused: st.Paused, Problem: u.err}
	if err != nil {
		s.Problem = err.Error()
	}
	for _, us := range st.Users {
		if !us.Remove {
			s.Users++
		}
	}
	s.People = len(st.People)
	s.Teams = []string{}
	for _, t := range st.Teams {
		if !t.Remove {
			s.Teams = append(s.Teams, t.Name)
		}
	}
	s.Conflict = len(st.Conflicts())
	s.Sites = []SiteStatus{}
	for _, si := range st.Sites {
		if si.Folder == "" {
			s.Sites = append(s.Sites, SiteStatus{Name: si.Name, Published: si.Published, Problem: si.Problem})
		}
	}
	return s
}

// Run keeps Syncthing in line with the state file until ctx ends.
func (u *Users) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	setup := false
	for {
		if !setup {
			setup = u.setup(ctx) == nil
		}
		if setup {
			u.setErr(u.step(ctx))
		}
		select {
		case <-ctx.Done():
			u.mu.Lock()
			for _, cancel := range u.watchers {
				cancel()
			}
			u.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

func (u *Users) setErr(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	msg := ""
	if err != nil {
		msg = err.Error()
		if msg != u.err {
			u.cfg.Logf("hub: %v", err)
		}
	}
	u.err = msg
}

// setup finds this Syncthing's ID and lets paired devices reach the hub
// from anywhere: through global discovery and Syncthing's relays when they
// are away from the LAN. What they sync is encrypted end to end, so relays
// only pass it on. New devices still pair only from the LAN (see pair).
func (u *Users) setup(ctx context.Context) error {
	id, err := u.cfg.ST.MyID(ctx)
	if err == nil {
		err = u.cfg.ST.SetOptions(ctx, map[string]any{"globalAnnounceEnabled": true, "relaysEnabled": true,
			"natEnabled": true, "localAnnounceEnabled": true})
	}
	if err != nil {
		u.setErr(fmt.Errorf("Syncthing: %w", err))
		return err
	}
	u.mu.Lock()
	u.myID = id
	u.mu.Unlock()
	u.cfg.Logf("hub: Syncthing device %s; paired devices reach it from anywhere, new ones pair from the local network", id)
	return nil
}

// step does one round: removals, people's folders, new devices, watchers.
func (u *Users) step(ctx context.Context) error {
	st, err := u.migrate(ctx)
	if err != nil {
		return err
	}
	var removed []string
	for _, us := range st.Users {
		if !us.Remove {
			continue
		}
		if err := u.cfg.ST.RemoveDevice(ctx, us.Device); err != nil && !notFound(err) {
			return err
		}
		u.cfg.Logf("hub: removed %s; %s's files stay on the box", us.Name, us.Person)
		removed = append(removed, us.Device)
	}
	if len(removed) > 0 {
		if st, err = Update(ctx, u.cfg.StatePath, func(s *HubState) error {
			s.Users = slices.DeleteFunc(s.Users, func(x User) bool { return x.Remove && slices.Contains(removed, x.Device) })
			return nil
		}); err != nil {
			return err
		}
	}

	have, err := u.cfg.ST.FolderIDs(ctx)
	if err != nil {
		return err
	}
	if err := u.sharePeople(ctx, st, have); err != nil {
		return err
	}
	if err := u.shareTeams(ctx, st, have); err != nil {
		return err
	}

	if !st.PairingOff {
		if err := u.pair(ctx, st); err != nil {
			return err
		}
		if st, err = Load(u.cfg.StatePath); err != nil { // with the devices just paired
			return err
		}
		if err := u.sharePeople(ctx, st, have); err != nil {
			return err
		}
		if err := u.shareTeams(ctx, st, have); err != nil {
			return err
		}
	}
	return u.shareWebsites(ctx, st, have)
}

// shareTeams keeps each team's folder shared with every device of its
// members, and stored by a watcher. A removed team, or one whose members
// have no devices, stays on the box, unshared.
func (u *Users) shareTeams(ctx context.Context, st HubState, have map[string]bool) error {
	var gone []string
	for _, t := range st.Teams {
		var ids []string
		if !t.Remove {
			for _, m := range t.Members {
				for _, d := range st.DevicesOf(m) {
					if !slices.Contains(ids, d.Device) {
						ids = append(ids, d.Device)
					}
				}
			}
		}
		if len(ids) == 0 {
			if have[t.Folder] {
				if err := u.cfg.ST.RemoveFolder(ctx, t.Folder); err != nil && !notFound(err) {
					return err
				}
				delete(have, t.Folder)
			}
			u.stopWatch(t.Folder)
			delete(u.shared, t.Folder)
			if t.Remove {
				gone = append(gone, t.Name)
				u.cfg.Logf("hub: team %s removed; its files stay in %s", t.Name, t.Path)
			}
			continue
		}
		slices.Sort(ids)
		ids = append([]string{u.myID}, ids...)
		want := fmt.Sprint(ids, st.Paused)
		if !have[t.Folder] || u.shared[t.Folder] != want {
			if err := os.MkdirAll(t.Path, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(t.Path, ".stignore"), []byte(ignore), 0o644); err != nil {
				return err
			}
			host, _ := os.Hostname()
			if err := u.cfg.ST.AddFolder(ctx, syncthing.Folder{ID: t.Folder, Label: "team " + t.Name + " (" + host + ")", Path: t.Path,
				Devices: ids, Paused: st.Paused, TrashDays: 0}); err != nil {
				return err
			}
			if !have[t.Folder] {
				u.cfg.Logf("hub: team %s is shared with %s", t.Name, strings.Join(t.Members, ", "))
			}
			have[t.Folder] = true
			u.shared[t.Folder] = want
		}
		u.startWatch(ctx, Person{Name: "team " + t.Name, Folder: t.Folder, Path: t.Path})
	}
	if len(gone) > 0 {
		_, err := Update(ctx, u.cfg.StatePath, func(s *HubState) error {
			s.Teams = slices.DeleteFunc(s.Teams, func(t Team) bool { return t.Remove && slices.Contains(gone, t.Name) })
			return nil
		})
		return err
	}
	return nil
}

// Conflict is a file two devices changed at once: Syncthing kept both,
// naming the second NAME.sync-conflict-DATE-DEVICE.EXT, and both are stored.
type Conflict struct {
	Folder string `json:"folder"` // whose: a person's name, or "team NAME"
	File   string `json:"file"`   // its path in the folder
}

// Conflicts lists the conflict copies in people's and teams' folders.
func (s HubState) Conflicts() []Conflict {
	var out []Conflict
	look := func(owner, root string) {
		filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() && strings.HasPrefix(e.Name(), ".") && p != root {
				return filepath.SkipDir
			}
			if !e.IsDir() && strings.Contains(e.Name(), ".sync-conflict-") {
				rel, _ := filepath.Rel(root, p)
				out = append(out, Conflict{Folder: owner, File: filepath.ToSlash(rel)})
			}
			return nil
		})
	}
	for _, p := range s.People {
		look(p.Name, p.Path)
	}
	for _, t := range s.Teams {
		look("team "+t.Name, t.Path)
	}
	return out
}

// migrate makes a person of each device paired before people existed,
// keeping its folder, so nothing moves.
func (u *Users) migrate(ctx context.Context) (HubState, error) {
	st, err := Load(u.cfg.StatePath)
	if err != nil || !slices.ContainsFunc(st.Users, func(us User) bool { return us.Person == "" }) {
		return st, err
	}
	return Update(ctx, u.cfg.StatePath, func(s *HubState) error {
		for i, us := range s.Users {
			if us.Person != "" {
				continue
			}
			if j := slices.IndexFunc(s.People, func(p Person) bool { return p.Folder == us.Folder }); j >= 0 {
				s.Users[i].Person = s.People[j].Name
				continue
			}
			s.People = append(s.People, Person{Name: us.Name, Folder: us.Folder, Path: us.Path, Added: us.Added})
			s.Users[i].Person = us.Name
		}
		return nil
	})
}

// sharePeople keeps each person's folder shared with their devices, and
// stored by a watcher. A person with no devices left keeps their files on
// the box, unshared.
func (u *Users) sharePeople(ctx context.Context, st HubState, have map[string]bool) error {
	for _, p := range st.People {
		devs := st.DevicesOf(p.Name)
		if len(devs) == 0 {
			if have[p.Folder] {
				if err := u.cfg.ST.RemoveFolder(ctx, p.Folder); err != nil && !notFound(err) {
					return err
				}
				delete(have, p.Folder)
			}
			u.stopWatch(p.Folder)
			delete(u.shared, p.Folder)
			continue
		}
		ids := []string{u.myID}
		for _, d := range devs {
			ids = append(ids, d.Device)
		}
		want := fmt.Sprint(ids, st.Paused)
		if !have[p.Folder] || u.shared[p.Folder] != want {
			for _, d := range devs {
				if err := u.cfg.ST.AddDevice(ctx, d.Device, d.Name); err != nil {
					return err
				}
			}
			if err := u.share(ctx, p, ids, st.Paused); err != nil {
				return err
			}
			have[p.Folder] = true
			u.shared[p.Folder] = want
		}
		u.startWatch(ctx, p)
	}
	return nil
}

// shareWebsites keeps the websites folder shared with every paired device,
// once it is ready, and stops syncing the folders sites had of their own
// before websites/ (Websites moves their files over).
func (u *Users) shareWebsites(ctx context.Context, st HubState, have map[string]bool) error {
	var dropped []string
	for _, si := range st.Sites {
		if si.Folder == "" {
			continue
		}
		if err := u.cfg.ST.RemoveFolder(ctx, si.Folder); err != nil && !notFound(err) {
			return err
		}
		dropped = append(dropped, si.Name)
		u.cfg.Logf("hub: site %s is now synced in the websites folder, not a folder of its own", si.Name)
	}
	if len(dropped) > 0 {
		if _, err := Update(ctx, u.cfg.StatePath, func(s *HubState) error {
			for i := range s.Sites {
				if slices.Contains(dropped, s.Sites[i].Name) {
					s.Sites[i].Folder, s.Sites[i].Path = "", ""
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if u.cfg.Websites == "" || (u.cfg.WebsitesReady != nil && !u.cfg.WebsitesReady()) {
		return nil
	}
	devs := []string{u.myID}
	for _, us := range st.Users {
		if !us.Remove {
			devs = append(devs, us.Device)
		}
	}
	want := fmt.Sprint(devs, st.Paused)
	if have[WebsitesFolderID] && u.shared[WebsitesFolderID] == want {
		return nil
	}
	host, _ := os.Hostname()
	if err := u.cfg.ST.AddFolder(ctx, syncthing.Folder{ID: WebsitesFolderID, Label: "websites (" + host + ")", Path: u.cfg.Websites,
		Devices: devs, Paused: st.Paused, TrashDays: 0}); err != nil {
		return err
	}
	if !have[WebsitesFolderID] {
		u.cfg.Logf("hub: the websites folder is shared with the paired devices")
	}
	u.shared[WebsitesFolderID] = want
	return nil
}

// pair accepts devices that asked to connect from the local network, up to
// the day's limit.
func (u *Users) pair(ctx context.Context, st HubState) error {
	pending, err := u.cfg.ST.Pending(ctx)
	if err != nil || len(pending) == 0 {
		return err
	}
	today := 0
	for _, us := range st.Users {
		if time.Since(time.Unix(us.Added, 0)) < 24*time.Hour {
			today++
		}
	}
	// Those that asked first are paired first, while the day's limit lasts.
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if c := pending[a].Time.Compare(pending[b].Time); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	for _, id := range ids {
		p := pending[id]
		if known(st.Users, id) {
			continue
		}
		if !OnLAN(p.Address) {
			u.cfg.Logf("hub: not pairing %s (%s): it isn't on the local network (%s)", p.Name, short(id), p.Address)
			u.cfg.ST.DismissPending(ctx, id)
			continue
		}
		if u.cfg.Limit > 0 && today >= u.cfg.Limit {
			u.cfg.Logf("hub: not pairing %s (%s) yet: %d devices paired in the last day already", p.Name, short(id), today)
			continue
		}
		// A new device is a new person until it is linked to someone
		// (yggstore hub link).
		var us User
		var person Person
		if _, err := Update(ctx, u.cfg.StatePath, func(s *HubState) error {
			name := uniqueName(s.Users, s.People, u.cfg.Dir, p.Name, id)
			person = Person{Name: name, Folder: "yggstore-" + short(id), Path: filepath.Join(u.cfg.Dir, name), Added: time.Now().Unix()}
			us = User{Name: name, Device: id, Folder: person.Folder, Path: person.Path, Added: person.Added, Address: p.Address, Person: name}
			s.Users = append(s.Users, us)
			s.People = append(s.People, person)
			return nil
		}); err != nil {
			return err
		}
		today++
		u.cfg.Logf("hub: paired %s (%s) from %s; its folder is %s", us.Name, short(id), p.Address, person.Path)
	}
	return nil
}

// ignore keeps the outbox's own files on the hub: its settings and history,
// and each stub's challenges.
const ignore = "// Kept on the hub by yggstore\n.yggstore\n.*.challenges.json\n"

// share makes a person's folder and shares it with their devices. Files
// their devices delete or replace are kept in .stversions on the hub, so a
// stub deleted by mistake isn't lost.
func (u *Users) share(ctx context.Context, p Person, devices []string, paused bool) error {
	if err := os.MkdirAll(p.Path, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(p.Path, ".stignore"), []byte(ignore), 0o644); err != nil {
		return err
	}
	host, _ := os.Hostname()
	return u.cfg.ST.AddFolder(ctx, syncthing.Folder{ID: p.Folder, Label: "yggstore (" + host + ")", Path: p.Path,
		Devices: devices, Paused: paused, TrashDays: 0})
}

func (u *Users) startWatch(ctx context.Context, p Person) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.watchers[p.Folder]; ok || u.cfg.Watch == nil {
		return
	}
	wctx, cancel := context.WithCancel(ctx)
	u.watchers[p.Folder] = cancel
	go u.cfg.Watch(wctx, p)
}

func (u *Users) stopWatch(folder string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if cancel, ok := u.watchers[folder]; ok {
		cancel()
		delete(u.watchers, folder)
	}
}

func notFound(err error) bool { return strings.Contains(err.Error(), "404") }

func known(list []User, id string) bool {
	return slices.ContainsFunc(list, func(us User) bool { return us.Device == id })
}

// short is the first part of a device ID, as Syncthing shows it.
func short(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// uniqueName is a name for a new device and its person: its own name, made
// safe, and numbered if another device, person or folder has it.
func uniqueName(list []User, people []Person, dir, name, id string) string {
	base := strings.Trim(unsafeName.ReplaceAllString(name, "-"), "-.")
	if base == "" {
		base = "device-" + strings.ToLower(short(id))
	}
	taken := func(n string) bool {
		for _, us := range list {
			if strings.EqualFold(us.Name, n) {
				return true
			}
		}
		for _, p := range people {
			if strings.EqualFold(p.Name, n) {
				return true
			}
		}
		_, err := os.Stat(filepath.Join(dir, n))
		return err == nil
	}
	n := base
	for i := 2; taken(n); i++ {
		n = fmt.Sprintf("%s-%d", base, i)
	}
	return n
}
