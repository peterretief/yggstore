package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	// Remove is set by "yggstore hub remove"; the hub then unshares the
	// folder and forgets the device, keeping the files.
	Remove bool `json:"remove,omitempty"`
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
	Users []User `json:"users"`
	Sites []Site `json:"sites,omitempty"`
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
	// Watch stores what arrives in a user's folder, until ctx ends.
	Watch func(ctx context.Context, u User)
	Logf  func(format string, args ...any)
}

type Users struct {
	cfg UsersConfig

	mu       sync.Mutex
	myID     string
	err      string // the last problem talking to Syncthing
	watchers map[string]context.CancelFunc
	paused   map[string]bool   // folders as last set in Syncthing
	shared   map[string]string // site folders: the devices last shared with, and paused
}

func NewUsers(cfg UsersConfig) *Users {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Users{cfg: cfg, watchers: map[string]context.CancelFunc{}, paused: map[string]bool{}, shared: map[string]string{}}
}

// Status is what the hub page shows of pairing.
type Status struct {
	DeviceID string       `json:"device_id,omitempty"` // the hub's Syncthing ID, to add on a device
	Pairing  bool         `json:"pairing"`
	Paused   bool         `json:"paused"`
	Users    int          `json:"users"`
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
	s.Sites = []SiteStatus{}
	for _, si := range st.Sites {
		if !si.Remove {
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

// setup finds this Syncthing's ID and keeps it to the LAN: no global
// discovery or relays, so only devices on the local network find the hub.
func (u *Users) setup(ctx context.Context) error {
	id, err := u.cfg.ST.MyID(ctx)
	if err == nil {
		err = u.cfg.ST.SetOptions(ctx, map[string]any{"globalAnnounceEnabled": false, "relaysEnabled": false,
			"natEnabled": false, "localAnnounceEnabled": true})
	}
	if err != nil {
		u.setErr(fmt.Errorf("Syncthing: %w", err))
		return err
	}
	u.mu.Lock()
	u.myID = id
	u.mu.Unlock()
	u.cfg.Logf("hub: Syncthing device %s, kept to the local network", id)
	return nil
}

// step does one round: removals, pause, new devices, watchers.
func (u *Users) step(ctx context.Context) error {
	st, err := Load(u.cfg.StatePath)
	if err != nil {
		return err
	}
	var removed []string
	for _, us := range st.Users {
		if !us.Remove {
			continue
		}
		u.stopWatch(us.Folder)
		if err := u.cfg.ST.RemoveFolder(ctx, us.Folder); err != nil && !notFound(err) {
			return err
		}
		if err := u.cfg.ST.RemoveDevice(ctx, us.Device); err != nil && !notFound(err) {
			return err
		}
		u.cfg.Logf("hub: removed %s; its files stay in %s", us.Name, us.Path)
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
	for _, us := range st.Users {
		if !have[us.Folder] { // lost from Syncthing's config: share it again
			if err := u.share(ctx, us, st.Paused); err != nil {
				return err
			}
		} else if p, ok := u.paused[us.Folder]; !ok || p != st.Paused {
			if err := u.cfg.ST.PauseFolder(ctx, us.Folder, st.Paused); err != nil {
				return err
			}
		}
		u.paused[us.Folder] = st.Paused
		u.startWatch(ctx, us)
	}

	if !st.PairingOff {
		if err := u.pair(ctx, st); err != nil {
			return err
		}
		if st, err = Load(u.cfg.StatePath); err != nil { // with the devices just paired
			return err
		}
	}
	return u.shareSites(ctx, st, have)
}

// shareSites keeps each site's folder shared with every paired device, and
// drops the folders of sites removed. Sites are shared once seeded (see
// Sites.seed), so nobody syncs an empty folder over the live site.
func (u *Users) shareSites(ctx context.Context, st HubState, have map[string]bool) error {
	devs := []string{u.myID}
	for _, us := range st.Users {
		if !us.Remove {
			devs = append(devs, us.Device)
		}
	}
	want := fmt.Sprint(devs, st.Paused)
	var removed []string
	for _, si := range st.Sites {
		switch {
		case si.Remove:
			if err := u.cfg.ST.RemoveFolder(ctx, si.Folder); err != nil && !notFound(err) {
				return err
			}
			delete(u.shared, si.Folder)
			removed = append(removed, si.Name)
			u.cfg.Logf("hub: site %s no longer synced; its files stay in %s", si.Name, si.Path)
		case !si.Seeded:
		case !have[si.Folder] || u.shared[si.Folder] != want:
			if err := u.cfg.ST.AddFolder(ctx, syncthing.Folder{ID: si.Folder, Label: si.Name + " (website)", Path: si.Path,
				Devices: devs, Paused: st.Paused, TrashDays: 0}); err != nil {
				return err
			}
			if !have[si.Folder] {
				u.cfg.Logf("hub: site %s shared with the paired devices", si.Name)
			}
			u.shared[si.Folder] = want
		}
	}
	if len(removed) > 0 {
		_, err := Update(ctx, u.cfg.StatePath, func(s *HubState) error {
			s.Sites = slices.DeleteFunc(s.Sites, func(x Site) bool { return x.Remove && slices.Contains(removed, x.Name) })
			return nil
		})
		return err
	}
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
	for id, p := range pending {
		if known(st.Users, id) {
			continue
		}
		if !onLAN(p.Address) {
			u.cfg.Logf("hub: not pairing %s (%s): it isn't on the local network (%s)", p.Name, short(id), p.Address)
			u.cfg.ST.DismissPending(ctx, id)
			continue
		}
		if u.cfg.Limit > 0 && today >= u.cfg.Limit {
			u.cfg.Logf("hub: not pairing %s (%s) yet: %d devices paired in the last day already", p.Name, short(id), today)
			continue
		}
		var us User
		if _, err := Update(ctx, u.cfg.StatePath, func(s *HubState) error {
			us = User{Name: uniqueName(s.Users, u.cfg.Dir, p.Name, id), Device: id, Folder: "yggstore-" + short(id),
				Added: time.Now().Unix(), Address: p.Address}
			us.Path = filepath.Join(u.cfg.Dir, us.Name)
			s.Users = append(s.Users, us)
			return nil
		}); err != nil {
			return err
		}
		if err := u.share(ctx, us, st.Paused); err != nil {
			return err
		}
		today++
		u.paused[us.Folder] = st.Paused
		u.startWatch(ctx, us)
		u.cfg.Logf("hub: paired %s (%s) from %s; its folder is %s", us.Name, short(id), p.Address, us.Path)
	}
	return nil
}

// ignore keeps the outbox's own files on the hub: its settings and history,
// and each stub's challenges.
const ignore = "// Kept on the hub by yggstore\n.yggstore\n.*.challenges.json\n"

// share makes the user's folder and shares it with their device. Files their
// device deletes or replaces are kept in .stversions on the hub, so a stub
// deleted by mistake isn't lost.
func (u *Users) share(ctx context.Context, us User, paused bool) error {
	if err := os.MkdirAll(us.Path, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(us.Path, ".stignore"), []byte(ignore), 0o644); err != nil {
		return err
	}
	if err := u.cfg.ST.AddDevice(ctx, us.Device, us.Name); err != nil {
		return err
	}
	host, _ := os.Hostname()
	return u.cfg.ST.AddFolder(ctx, syncthing.Folder{ID: us.Folder, Label: "yggstore (" + host + ")", Path: us.Path,
		Devices: []string{u.myID, us.Device}, Paused: paused, TrashDays: 0})
}

func (u *Users) startWatch(ctx context.Context, us User) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.watchers[us.Folder]; ok || u.cfg.Watch == nil {
		return
	}
	wctx, cancel := context.WithCancel(ctx)
	u.watchers[us.Folder] = cancel
	go u.cfg.Watch(wctx, us)
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

// onLAN is whether a device connected from the local network.
func onLAN(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
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

// uniqueName is a folder name for a device: its own name, made safe, and
// numbered if another user or folder has it.
func uniqueName(list []User, dir, name, id string) string {
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
		_, err := os.Stat(filepath.Join(dir, n))
		return err == nil
	}
	n := base
	for i := 2; taken(n); i++ {
		n = fmt.Sprintf("%s-%d", base, i)
	}
	return n
}
