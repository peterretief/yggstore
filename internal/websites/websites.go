// Package websites watches a websites/ folder that holds a node's own
// websites, one folder per domain (websites/example.org/), and publishes a
// site to the group whenever its folder changes.
//
// A site published from this machine, or a name the group gave this node,
// is this node's: its folder is filled with the version being served when
// it is missing, and every change is published, unless it looks like a
// mistake (no index.html, shrunk to less than half, too big). A folder for
// a new domain is published only once the domain is on Cloudflare, which
// puts the group's sites online; until then nothing happens and a file
// beside the folder, "DOMAIN - NOT PUBLISHED.txt", says why and what to do.
// A domain another node publishes is left alone.
package websites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// NoteSuffix ends the name of the file that says why a site isn't published.
const NoteSuffix = " - NOT PUBLISHED.txt"

// MovedSuffix ends the name of the file that says where a site moved to.
const MovedSuffix = " - MOVED.txt"

// ErrNoToken is what Route returns when there is no Cloudflare API token:
// routes are then added by hand.
var ErrNoToken = errors.New("no Cloudflare API token")

type Config struct {
	Dir string // the websites/ folder
	// StatePath keeps which version each folder holds, so a restart
	// knows a folder from an edit in it. "": kept in memory only.
	StatePath string
	Pub       *site.Publisher
	Client    client.Client // fetches the version being served, to fill a missing folder
	Online    func(ctx context.Context) ([]peers.Peer, error)
	// Mine is the names the group gave this node; they are published
	// without the Cloudflare check. nil: none.
	Mine func() map[string]bool
	// Served is the sites the group's web nodes serve now. nil: none known.
	Served func() map[string]bool
	// Moved says where a site published from here moved to, and when
	// (see package sitemove). nil: none moved.
	Moved func(name string) (to string, at time.Time, ok bool)
	// Cloudflare reports whether domain is on Cloudflare, and the zone it
	// is (or would be) in. nil: by the domain's nameservers (OnCloudflare).
	Cloudflare func(ctx context.Context, domain string) (bool, string, error)
	// Route adds the tunnel route and DNS record for a new site and
	// reports what it added. nil, or ErrNoToken: there is no API token,
	// and routes are added by hand in Cloudflare's dashboard.
	Route func(ctx context.Context, domain string) ([]string, error)
	// Answers reports whether https://domain/ is online. nil: never
	// checked.
	Answers func(ctx context.Context, domain string) bool
	// Service is where the web nodes serve, for the instructions
	// (default localhost:8480); TokenPath where an API token would go.
	Service, TokenPath string
	// MaxBytes is the most a site may hold (0 = no limit).
	MaxBytes int64
	// Quiet is how long a folder must be unchanged before it is
	// published, Gap the least time between two publishes of a site, and
	// Recheck how often Cloudflare and https://domain/ are checked again.
	Quiet, Gap, Recheck time.Duration
	Interval            time.Duration
	Logf                func(format string, args ...any)
	Now                 func() time.Time
}

// Watcher publishes the sites in a websites/ folder.
type Watcher struct {
	cfg   Config
	mu    sync.Mutex
	sites map[string]*entry
	ready atomic.Bool
}

type entry struct {
	last, seen scan
	matches    string // the version the folder holds, as last published or filled
	seenAt     time.Time
	tried      time.Time // last publish, or attempt
	checked    time.Time // last Cloudflare check
	onCF       bool
	zone       string
	online     time.Time // last check of https://domain/
	answered   bool
	routeErr   error // why the route couldn't be added, last time
	force      bool  // publish it as it is, past the checks
	status     Status
}

// Status is one site as the watcher has it, for a dashboard.
type Status struct {
	Site      string `json:"site"`
	Folder    string `json:"folder"`
	Published int64  `json:"published,omitempty"` // when this watcher last published it
	Version   string `json:"version,omitempty"`
	Problem   string `json:"problem,omitempty"` // why it isn't published (or online), as in its note
}

func New(cfg Config) *Watcher {
	if cfg.Quiet <= 0 {
		cfg.Quiet = 15 * time.Second
	}
	if cfg.Gap <= 0 {
		cfg.Gap = time.Minute
	}
	if cfg.Recheck <= 0 {
		cfg.Recheck = 5 * time.Minute
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Service == "" {
		cfg.Service = "localhost:8480"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Cloudflare == nil {
		cfg.Cloudflare = OnCloudflare
	}
	return &Watcher{cfg: cfg, sites: map[string]*entry{}}
}

func (w *Watcher) Dir() string { return w.cfg.Dir }

// Ready is true once the watcher has looked at the folder once, so the
// folders of this node's sites hold the versions being served.
func (w *Watcher) Ready() bool { return w.ready.Load() }

// Force publishes a site's folder as it is on the next look, even if it
// looks like a mistake.
func (w *Watcher) Force(folder string) {
	e := w.entry(folder)
	w.mu.Lock()
	e.force = true
	w.mu.Unlock()
}

func (w *Watcher) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.cfg.Dir, 0o755); err != nil {
		return err
	}
	w.load()
	w.cfg.Logf("websites: watching %s (a folder per domain, such as %s)", w.cfg.Dir, filepath.Join(w.cfg.Dir, "example.org"))
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		w.Step(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Status lists the sites in the folder, by name.
func (w *Watcher) Status() []Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []Status{}
	for _, e := range w.sites {
		out = append(out, e.status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site < out[j].Site })
	return out
}

// Step fills missing folders of this node's sites, then looks at every
// folder once.
func (w *Watcher) Step(ctx context.Context) {
	mine := map[string]bool{}
	if w.cfg.Mine != nil {
		mine = w.cfg.Mine()
	}
	published := map[string]bool{}
	for _, s := range w.cfg.Pub.Sites() {
		published[s] = true
	}
	for s := range published {
		if _, err := os.Stat(filepath.Join(w.cfg.Dir, s)); os.IsNotExist(err) {
			w.fill(ctx, s)
		}
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return
	}
	present := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "~") || ctx.Err() != nil {
			continue
		}
		folder := e.Name()
		present[folder] = true
		name := site.Normalise(folder)
		if err := site.ValidName(name); err != nil {
			w.note(folder, folder, fmt.Sprintf("%q isn't a domain name, so it can't be a website. Name the folder after the site's domain, such as example.org or shop.example.org.", folder))
			continue
		}
		path := filepath.Join(w.cfg.Dir, folder)
		switch {
		case published[name] || mine[name]:
			w.publish(ctx, folder, name, path, !mine[name])
		case w.cfg.Moved != nil && w.movedNote(folder, name):
		case w.cfg.Served != nil && w.cfg.Served()[name]:
			w.note(folder, name, fmt.Sprintf("%s is already published by another node in the group, so this one leaves it alone and nothing in this folder is published.\n\nTo change that site, edit it on the node that publishes it. To make a new site, rename this folder after a domain of your own.", name))
		default:
			w.newSite(ctx, folder, name, path)
		}
	}
	w.mu.Lock()
	gone := false
	for f := range w.sites {
		if !present[f] {
			delete(w.sites, f)
			os.Remove(w.notePath(f))
			gone = true
		}
	}
	if gone {
		w.save()
	}
	w.mu.Unlock()
	w.ready.Store(true)
}

// fill fills a missing folder with the version being served, so whoever
// edits it starts from the live site.
func (w *Watcher) fill(ctx context.Context, name string) {
	m, err := w.cfg.Pub.Current(name)
	if err != nil {
		return
	}
	tmp := filepath.Join(w.cfg.Dir, ".filling-"+name)
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	root, err := files.Restore(ctx, w.cfg.Client, m, tmp, nil)
	if err == nil {
		err = os.Rename(root, filepath.Join(w.cfg.Dir, name))
	}
	if err != nil {
		w.cfg.Logf("websites: %s: fetching the version being served: %v (will try again)", name, err)
		return
	}
	e := w.entry(name)
	w.mu.Lock()
	e.last, e.matches = scanSite(filepath.Join(w.cfg.Dir, name)), m.FileID
	w.save()
	w.mu.Unlock()
	w.cfg.Logf("websites: %s: its folder holds the version being served (%s)", name, short(m.FileID))
}

// catchUp brings a folder up to the version being served when that was
// published some other way (the dashboard's Edit, yggstore site publish)
// and the folder hasn't changed since, so the next edit in the folder
// doesn't undo it. It reports whether it did.
func (w *Watcher) catchUp(ctx context.Context, folder, name, path string, sc scan) bool {
	e := w.entry(folder)
	w.mu.Lock()
	matches, last := e.matches, e.last
	w.mu.Unlock()
	if matches == "" || sc != last {
		return false
	}
	m, err := w.cfg.Pub.Current(name)
	if err != nil || m.FileID == matches {
		return false
	}
	tmp := filepath.Join(w.cfg.Dir, ".filling-"+folder)
	old := filepath.Join(w.cfg.Dir, ".replaced-"+folder)
	os.RemoveAll(tmp)
	os.RemoveAll(old)
	defer os.RemoveAll(tmp)
	root, err := files.Restore(ctx, w.cfg.Client, m, tmp, nil)
	if err == nil {
		if err = os.Rename(path, old); err == nil {
			if err = os.Rename(root, path); err != nil {
				os.Rename(old, path)
			}
		}
	}
	os.RemoveAll(old)
	if err != nil {
		w.cfg.Logf("websites: %s: fetching the version published elsewhere: %v (will try again)", name, err)
		return true
	}
	sc = scanSite(path)
	w.mu.Lock()
	e.last, e.seen, e.matches = sc, sc, m.FileID
	w.save()
	w.mu.Unlock()
	w.cfg.Logf("websites: %s: its folder now holds version %s, published from the dashboard or the command line", name, short(m.FileID))
	return true
}

// newSite publishes a folder for a domain nobody publishes yet, once the
// domain is on Cloudflare.
func (w *Watcher) newSite(ctx context.Context, folder, name, path string) {
	e := w.entry(folder)
	now := w.cfg.Now()
	w.mu.Lock()
	due := e.checked.IsZero() || now.Sub(e.checked) >= w.cfg.Recheck
	w.mu.Unlock()
	if due {
		ok, zone, err := w.cfg.Cloudflare(ctx, name)
		w.mu.Lock()
		e.checked, e.onCF, e.zone = now, ok && err == nil, zone
		w.mu.Unlock()
		if err != nil {
			w.cfg.Logf("websites: %s: checking Cloudflare: %v", name, err)
		}
	}
	w.mu.Lock()
	onCF, zone := e.onCF, e.zone
	w.mu.Unlock()
	if !onCF {
		w.note(folder, name, notOnCloudflare(name, zone, now))
		return
	}
	w.publish(ctx, folder, name, path, true)
}

// publish publishes a folder once it has settled, if nothing about it looks
// like a mistake. A site this node owns by name is online already; others
// get their route (or instructions for it) once published.
func (w *Watcher) publish(ctx context.Context, folder, name, path string, route bool) {
	e := w.entry(folder)
	sc := scanSite(path)
	if w.catchUp(ctx, folder, name, path, sc) {
		return
	}
	now := w.cfg.Now()
	w.mu.Lock()
	if e.seen != sc {
		e.seen, e.seenAt = sc, now // changed: wait for it to settle
		w.mu.Unlock()
		return
	}
	settled := !sc.temp && now.Sub(e.seenAt) >= w.cfg.Quiet
	force := e.force
	changed := e.last != sc || force
	early := now.Sub(e.tried) < w.cfg.Gap && !force
	w.mu.Unlock()
	if !settled {
		return
	}
	if changed {
		if problem := w.check(name, sc); problem != "" && !force {
			w.note(folder, name, problem)
			return
		}
		if early {
			return
		}
		w.mu.Lock()
		e.tried = now
		w.mu.Unlock()
		online, err := w.cfg.Online(ctx)
		var v site.Version
		if err == nil {
			v, _, err = w.cfg.Pub.Publish(ctx, name, path, online)
		}
		if err != nil {
			w.note(folder, name, fmt.Sprintf("The latest change to %s couldn't be published: %v.\nThis node tries again in a minute; the version being served stays online meanwhile.", name, err))
			return
		}
		w.mu.Lock()
		e.last, e.matches, e.force = sc, v.ID, false
		e.status.Published, e.status.Version = now.Unix(), short(v.ID)
		w.save()
		e.online = time.Time{} // check it's online again
		w.mu.Unlock()
		w.cfg.Logf("websites: %s published: version %s, %d files, %d bytes", name, short(v.ID), v.Files, v.Bytes)
	}
	if !route || w.cfg.Answers == nil {
		w.clear(folder, name, "published")
		return
	}
	w.mu.Lock()
	due := e.online.IsZero() || now.Sub(e.online) >= w.cfg.Recheck
	answered := e.answered
	w.mu.Unlock()
	if due {
		routeErr := ErrNoToken
		if !w.cfg.Answers(ctx, name) && w.cfg.Route != nil {
			added, err := w.cfg.Route(ctx, name)
			for _, a := range added {
				w.cfg.Logf("websites: %s: added %s", name, a)
			}
			if err != nil && !errors.Is(err, ErrNoToken) {
				w.cfg.Logf("websites: %s: adding its route in Cloudflare: %v", name, err)
			}
			routeErr = err
		}
		answered = w.cfg.Answers(ctx, name)
		w.mu.Lock()
		e.online, e.answered, e.routeErr = now, answered, routeErr
		w.mu.Unlock()
	}
	if answered {
		w.clear(folder, name, "online at https://"+name+"/")
		return
	}
	w.mu.Lock()
	routeErr := e.routeErr
	w.mu.Unlock()
	w.note(folder, name, w.notOnline(name, routeErr))
}

// check is why a folder shouldn't be published as it is: it would break or
// empty the site, most likely by mistake.
func (w *Watcher) check(name string, sc scan) string {
	keep := "\nThe version being served (if there is one) stays online. Once the folder is fixed, it is published by itself."
	if !sc.index {
		return fmt.Sprintf("%s isn't published: its folder has no index.html, the site's home page.%s", name, keep)
	}
	if w.cfg.MaxBytes > 0 && sc.bytes > w.cfg.MaxBytes {
		return fmt.Sprintf("%s isn't published: its folder holds %.1f MB, more than the %.1f MB a site may.%s",
			name, float64(sc.bytes)/(1<<20), float64(w.cfg.MaxBytes)/(1<<20), keep)
	}
	if vs, _ := w.cfg.Pub.Versions(name); len(vs) > 0 {
		for _, v := range vs {
			if v.Current && v.Bytes > 0 && sc.bytes < v.Bytes/2 {
				return fmt.Sprintf("%s isn't published: its folder shrank from %d to %d bytes since the version being served, which looks like a mistake. To publish it anyway, use the dashboard's Websites page or: yggstore site publish \"%s\" -name %s%s",
					name, v.Bytes, sc.bytes, filepath.Join(w.cfg.Dir, name), name, keep)
			}
		}
	}
	return ""
}

func notOnCloudflare(name, zone string, at time.Time) string {
	if zone == "" {
		zone = name
	}
	return fmt.Sprintf(`%s is not online yet.

Its folder is ready, but the domain %s isn't set up on Cloudflare, which
puts the group's websites online. Nothing is published until it is.

To set it up (or ask the group's admin to):
1. Log in to Cloudflare (dash.cloudflare.com) and add %s as a domain.
   The Free plan is enough.
2. Where the domain was bought, change its nameservers to the two that
   Cloudflare gives you.
3. Wait. It can take from a few minutes to a day. This node checks every
   few minutes and publishes the site as soon as the domain is on
   Cloudflare. This file then goes away.

Last checked: %s
`, name, zone, zone, at.Format("2 Jan 2006 15:04"))
}

// notOnline says how to put a published site online: by hand, or why
// adding its route didn't work (routeErr).
func (w *Watcher) notOnline(name string, routeErr error) string {
	how := fmt.Sprintf(`To put it online, in Cloudflare's dashboard (Zero Trust, Networks, Tunnels,
the group's tunnel, Public hostnames) add %s with service HTTP and
URL %s.`, name, w.cfg.Service)
	if routeErr != nil && !errors.Is(routeErr, ErrNoToken) {
		how = fmt.Sprintf("This node tried to add its route in Cloudflare, which didn't work:\n%v\n\n", routeErr) + how
	} else if routeErr == nil {
		how = "Its route was added in Cloudflare; it can take a few minutes to answer."
	} else if w.cfg.TokenPath != "" {
		how += fmt.Sprintf("\nOr save a Cloudflare API token as %s on this node\n(see docs/sites.md), and it is added by itself.", w.cfg.TokenPath)
	}
	return fmt.Sprintf("%s is published, but isn't online at https://%s/ yet.\n\n%s\n\nThis file goes away once the site answers.\n", name, name, how)
}

func (w *Watcher) entry(folder string) *entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.sites[folder]
	if e == nil {
		e = &entry{status: Status{Site: site.Normalise(folder), Folder: folder}}
		w.sites[folder] = e
	}
	return e
}

// movedNote writes where a site moved to beside its folder, if it moved
// away from here: nothing in the folder is published any more.
func (w *Watcher) movedNote(folder, name string) bool {
	to, at, ok := w.cfg.Moved(name)
	if !ok {
		return false
	}
	text := fmt.Sprintf("%s moved to %s on %s, and is published from there now.\n\nNothing in this folder is published any more: to change the site, edit it there. You can delete this folder.\n",
		name, to, at.Format("2 January 2006"))
	e := w.entry(folder)
	w.mu.Lock()
	e.status.Problem = text
	w.mu.Unlock()
	path := filepath.Join(w.cfg.Dir, folder+MovedSuffix)
	if old, err := os.ReadFile(path); err != nil || string(old) != text {
		if err := os.WriteFile(path, []byte(text), 0o644); err == nil {
			w.cfg.Logf("websites: %s moved to %s; its folder here is no longer published", name, to)
		}
	}
	os.Remove(w.notePath(folder))
	return true
}

func (w *Watcher) notePath(folder string) string {
	return filepath.Join(w.cfg.Dir, folder+NoteSuffix)
}

// note writes why a site isn't published beside its folder, and logs it
// when it changes.
func (w *Watcher) note(folder, name, text string) {
	e := w.entry(folder)
	w.mu.Lock()
	news := firstLine(e.status.Problem) != firstLine(text)
	e.status.Problem = text
	w.mu.Unlock()
	if old, err := os.ReadFile(w.notePath(folder)); err == nil && string(old) == text {
		return
	}
	if err := os.WriteFile(w.notePath(folder), []byte(text), 0o644); err != nil {
		w.cfg.Logf("websites: %s: %v", name, err)
	}
	if news {
		w.cfg.Logf("websites: %s", firstLine(text))
	}
}

// clear removes a site's note, if it has one, once all is well.
func (w *Watcher) clear(folder, name, state string) {
	e := w.entry(folder)
	w.mu.Lock()
	had := e.status.Problem != ""
	e.status.Problem = ""
	w.mu.Unlock()
	if err := os.Remove(w.notePath(folder)); err == nil || had {
		w.cfg.Logf("websites: %s is %s", name, state)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// OnCloudflare reports whether a domain is on Cloudflare by its
// nameservers: the first of the domain and its parents that has
// nameservers is its zone, and Cloudflare's are *.ns.cloudflare.com.
func OnCloudflare(ctx context.Context, domain string) (bool, string, error) {
	labels := strings.Split(strings.TrimSuffix(domain, "."), ".")
	for i := 0; i+2 <= len(labels); i++ {
		zone := strings.Join(labels[i:], ".")
		ns, err := net.DefaultResolver.LookupNS(ctx, zone)
		if err != nil || len(ns) == 0 {
			continue
		}
		for _, n := range ns {
			if strings.HasSuffix(strings.TrimSuffix(strings.ToLower(n.Host), "."), ".ns.cloudflare.com") {
				return true, zone, nil
			}
		}
		return false, zone, nil
	}
	return false, "", nil
}

// saved is what StatePath keeps for a folder.
type saved struct {
	Matches string `json:"matches"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	Newest  int64  `json:"newest"`
	Index   bool   `json:"index"`
}

func (w *Watcher) load() {
	b, err := os.ReadFile(w.cfg.StatePath)
	if w.cfg.StatePath == "" || err != nil {
		return
	}
	var st map[string]saved
	if json.Unmarshal(b, &st) != nil {
		return
	}
	for folder, v := range st {
		e := w.entry(folder)
		w.mu.Lock()
		e.matches, e.last = v.Matches, scan{files: v.Files, bytes: v.Bytes, newest: v.Newest, index: v.Index}
		w.mu.Unlock()
	}
}

// save keeps which version each folder holds; call with w.mu held.
func (w *Watcher) save() {
	if w.cfg.StatePath == "" {
		return
	}
	st := map[string]saved{}
	for folder, e := range w.sites {
		if e.matches != "" {
			st[folder] = saved{e.matches, e.last.files, e.last.bytes, e.last.newest, e.last.index}
		}
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	os.MkdirAll(filepath.Dir(w.cfg.StatePath), 0o700)
	if err := atomicfile.Replace(w.cfg.StatePath, append(b, '\n'), 0o600); err != nil {
		w.cfg.Logf("websites: %v", err)
	}
}

// scan sums up a site's folder, leaving out hidden files and Syncthing's,
// to see when it changes.
type scan struct {
	files  int
	bytes  int64
	newest int64 // newest modification, unix nanoseconds
	index  bool  // has index.html
	temp   bool  // a file is being written
}

func scanSite(dir string) scan {
	var sc scan
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		name := e.Name()
		if strings.HasPrefix(name, ".syncthing.") || strings.HasPrefix(name, "~syncthing~") {
			sc.temp = true
		}
		if hidden(name) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if t := info.ModTime().UnixNano(); t > sc.newest {
			sc.newest = t
		}
		if info.Mode().IsRegular() {
			sc.files++
			sc.bytes += info.Size()
			if filepath.Dir(p) == dir && name == "index.html" {
				sc.index = true
			}
		}
		return nil
	})
	return sc
}

// Skip leaves out of a published site what a websites/ folder carries but a
// site doesn't serve: hidden files (Syncthing's among them, but not
// .well-known/) and the files operating systems leave behind.
func Skip(rel string, dir bool) bool {
	for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
		if hidden(c) || c == "Thumbs.db" || c == "desktop.ini" {
			return true
		}
	}
	return false
}

func hidden(name string) bool {
	return (strings.HasPrefix(name, ".") && name != ".well-known") || strings.HasPrefix(name, "~")
}
