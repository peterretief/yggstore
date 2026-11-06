package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
)

// Messages is the node's messaging, as a web node uses it.
type Messages interface {
	Subscribe(topic string) error
	Wait(ctx context.Context, after int64, topic string) []msg.Stored
}

// Web serves the group's sites from copies kept in Dir.
type Web struct {
	mailMissed atomic.Int64 // mail that reached this node with no -mail-in
	Dir        string
	Msgs       Messages
	Peers      func() []peers.Peer
	Client     client.Client
	Log        func(string, ...any)
	// Mail, if set, takes the mail Worker's posts (see package mail).
	// Work, if set, is the page a person's recovery link opens, on any
	// site served here (WorkPath; see package workhere).
	Work http.Handler
	Mail http.Handler
	// Light, if set, serves my. and the group's domain: light clients'
	// requests, relayed to their boxes (see package light).
	Light http.Handler
	// Deliver, if set, takes sites' contact forms: it stores a sealed
	// message for a member's node to collect (mail.Node.Take).
	Deliver func(ctx context.Context, node string, sealed []byte) (string, error)
	// Apply, if set, passes an application to join (ApplyPath; an
	// Application as JSON) to the group's admins.
	Apply func(ctx context.Context, body string) error
	// Who, if set, says whose an access token from the group's login is
	// (its claims), for uploads to sites (see UploadPath).
	Who func(ctx context.Context, token string) (map[string]any, bool)
	// NoCode: this web node doesn't run sites' handlers (see ApiPath); the
	// other web nodes do.
	NoCode bool
	// Self names this web node in the entries it takes for sites' data
	// (its node name); Others are the group's web nodes' node API
	// addresses, which share them (see datastore.go), reached with
	// DataClient.
	Self       string
	Others     func(ctx context.Context) []string
	DataClient *http.Client

	code  handlers
	data  dataSites
	mu    sync.Mutex
	st    webState
	roots map[string]string // site → folder being served
	limit limiter
}

type webState struct {
	After int64                `json:"after"` // last message handled
	Sites map[string]*webEntry `json:"sites"`
	// CodeAllowed are the sites the group's admin let run code though
	// another node publishes them (CodeAllowance), with when.
	CodeAllowed map[string]int64 `json:"code_allowed,omitempty"`
}

type webEntry struct {
	Owner     string             `json:"owner"`     // node that published it first
	Announced int64              `json:"announced"` // time of the latest announcement, ms
	Serving   string             `json:"serving,omitempty"`
	Want      *manifest.Manifest `json:"want,omitempty"` // announced, not fetched yet
	Error     string             `json:"error,omitempty"`
	Contact   string             `json:"contact,omitempty"`   // the contact form's sharing code; "" if off
	Uploaders []string           `json:"uploaders,omitempty"` // who may add photos and files
	Code      bool               `json:"code,omitempty"`      // run its handlers (see ApiPath)
	Local     []string           `json:"local,omitempty"`     // paths for the publisher's machine (localapp.go)
	// Preview is the version shown with the preview key (its hash is
	// PreviewKey); PreviewWant one announced and not fetched yet.
	Preview     string             `json:"preview,omitempty"`
	PreviewWant *manifest.Manifest `json:"preview_want,omitempty"`
	PreviewKey  string             `json:"preview_key,omitempty"`
}

// NodeStatus is what a web node reports in its info.
type NodeStatus struct {
	Tunnel string       `json:"tunnel,omitempty"` // its connector, if it runs one
	Sites  []SiteStatus `json:"sites"`
	// MailOff: it has the tunnel but takes no mail, so mail reaching it is
	// delayed (MailMissed: how often since it started). MailRefused is how
	// many addresses it said are no one's in the last day.
	MailOff     bool  `json:"mail_off,omitempty"`
	MailMissed  int64 `json:"mail_missed,omitempty"`
	MailRefused int   `json:"mail_refused,omitempty"`
}

// SiteStatus is one site as a web node has it.
type SiteStatus struct {
	Site    string `json:"site"`
	Serving string `json:"serving,omitempty"` // version ID
	Waiting string `json:"waiting,omitempty"` // newer version not fetched yet
	Error   string `json:"error,omitempty"`
	Owner   string `json:"owner,omitempty"` // the node that publishes it
	Contact bool   `json:"contact,omitempty"`
	Code    bool   `json:"code,omitempty"`
}

func (w *Web) statePath() string { return filepath.Join(w.Dir, "web.json") }

func (w *Web) load() {
	w.st = webState{Sites: map[string]*webEntry{}}
	if b, err := os.ReadFile(w.statePath()); err == nil {
		json.Unmarshal(b, &w.st)
		if w.st.Sites == nil {
			w.st.Sites = map[string]*webEntry{}
		}
	}
	w.roots = map[string]string{}
	for name, e := range w.st.Sites {
		if e.Serving != "" {
			w.roots[name] = filepath.Join(w.Dir, "sites", name, e.Serving)
		}
	}
}

func (w *Web) save() {
	b, _ := json.MarshalIndent(w.st, "", "  ")
	if err := atomicfile.Replace(w.statePath(), append(b, '\n'), 0o600); err != nil {
		w.logf("web: %v", err)
	}
}

// Run follows the sites topic and keeps the copies current. Sites already
// fetched are served from the start, so a node cut off from the group
// keeps serving what it has.
func (w *Web) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(w.Dir, "sites"), 0o755); err != nil {
		return err
	}
	w.code.dir = w.Dir // compiled handlers are kept in Dir/wasm-cache
	go w.others()      // find the other web nodes before the first write needs them
	go w.pullLoop(ctx)
	w.mu.Lock()
	w.load()
	w.mu.Unlock()
	if err := w.Msgs.Subscribe(Topic); err != nil {
		return err
	}
	go w.retry(ctx)
	w.fetchWanted(ctx)
	for ctx.Err() == nil {
		w.mu.Lock()
		after := w.st.After
		w.mu.Unlock()
		// Handle a batch (say, the history a node catches up on) before
		// fetching, so only each site's latest version is fetched.
		for _, s := range w.Msgs.Wait(ctx, after, Topic) {
			w.handle(s)
		}
		w.fetchWanted(ctx)
	}
	return nil
}

// retry fetches versions that failed (say, too many nodes were down).
func (w *Web) retry(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		w.fetchWanted(ctx)
	}
}

func (w *Web) handle(s msg.Stored) {
	w.mu.Lock()
	w.st.After = max(w.st.After, s.N)
	if s.Topic == Topic && s.Type == typeOwner {
		w.changeOwner(s)
		w.save()
		w.mu.Unlock()
		return
	}
	if s.Topic == Topic && s.Type == typeAllow {
		w.allowCode(s)
		w.save()
		w.mu.Unlock()
		return
	}
	a, ok := decode(s)
	if !ok {
		w.save()
		w.mu.Unlock()
		return
	}
	e := w.st.Sites[a.Site]
	list := w.Peers()
	keeper, given, under := peers.SiteKeeper(list, a.Site)
	switch {
	case (under || given) && !(given && keeper.IP() == s.From) && !isAdmin(list, s.From):
		w.logf("web: ignored %s from %s: names the group gives are published by the node it gives them to", a.Site, s.FromName)
	case !under && !given && e != nil && !w.mayChange(s.From, e.Owner):
		w.logf("web: ignored a change to %s from %s, who didn't publish it", a.Site, s.FromName)
	case !under && !given && e == nil && a.Stub != nil && !w.mayClaim(a.Site, s.From):
		w.logf("web: ignored %s from %s: www. of a site belongs to that site's publisher", a.Site, s.FromName)
	case e != nil && s.Time <= e.Announced:
		// an older announcement, arriving late
	case a.Stub == nil:
		if e != nil {
			delete(w.st.Sites, a.Site)
			delete(w.roots, a.Site)
			os.RemoveAll(filepath.Join(w.Dir, "sites", a.Site))
			w.logf("web: %s removed", a.Site)
		}
	case !w.shardsAtMembers(*a.Stub):
		w.logf("web: ignored %s from %s: its parts aren't on the group's nodes", a.Site, s.FromName)
	default:
		if e == nil {
			e = &webEntry{Owner: s.From}
			w.st.Sites[a.Site] = e
		}
		if given && keeper.IP() == s.From {
			e.Owner = s.From // its contact form goes to the name's node
		}
		e.Announced = s.Time
		if a.Code && !e.Code && e.Serving != "" && !w.NoCode {
			go w.warm(a.Site, filepath.Join(w.Dir, "sites", a.Site, e.Serving)) // code just turned on
		}
		e.Contact, e.Uploaders, e.Code, e.Local = a.Contact, a.Uploaders, a.Code, a.Local
		switch {
		case a.Preview == nil || a.PreviewKey == "":
			e.Preview, e.PreviewWant, e.PreviewKey = "", nil, ""
		case a.Preview.FileID != e.Preview:
			e.PreviewWant, e.PreviewKey = a.Preview, a.PreviewKey
		default:
			e.PreviewWant, e.PreviewKey = nil, a.PreviewKey
		}
		if a.Stub.FileID == e.Serving {
			e.Want = nil
		} else {
			e.Want, e.Error = a.Stub, ""
		}
	}
	w.save()
	w.mu.Unlock()
}

// MailMissed is how often mail reached this node with no -mail-in.
func (w *Web) MailMissed() int64 { return w.mailMissed.Load() }

// allowCode takes an admin's CodeAllowance. Called with w.mu held.
func (w *Web) allowCode(s msg.Stored) {
	var c CodeAllowance
	if json.Unmarshal([]byte(s.Body), &c) != nil || ValidName(c.Site) != nil {
		return
	}
	if !isAdmin(w.Peers(), s.From) {
		w.logf("web: ignored code allowed for %s from %s, who isn't the group's admin", c.Site, s.FromName)
		return
	}
	if w.st.CodeAllowed == nil {
		w.st.CodeAllowed = map[string]int64{}
	}
	if c.On {
		w.st.CodeAllowed[c.Site] = s.Time
		if e := w.st.Sites[c.Site]; e != nil && e.Code && e.Serving != "" && !w.NoCode {
			go w.warm(c.Site, filepath.Join(w.Dir, "sites", c.Site, e.Serving))
		}
		w.logf("web: %s may run code (the admin allowed it)", c.Site)
	} else {
		delete(w.st.CodeAllowed, c.Site)
		w.logf("web: %s may no longer run code (the admin stopped it)", c.Site)
	}
}

// codeAllowed is whether a site's handlers may run: the admin publishes
// it, or allowed it.
func (w *Web) codeAllowed(site, owner string) bool {
	if isAdmin(w.Peers(), owner) {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.st.CodeAllowed[site]
	return ok
}

// changeOwner takes an admin's OwnerChange: the site's next versions come
// from the new owner. The version being served stays until it announces
// one. Called with w.mu held.
func (w *Web) changeOwner(s msg.Stored) {
	var c OwnerChange
	if json.Unmarshal([]byte(s.Body), &c) != nil || ValidName(c.Site) != nil || net.ParseIP(c.Owner) == nil {
		return
	}
	list := w.Peers()
	_, _, under := peers.SiteKeeper(list, c.Site)
	switch e := w.st.Sites[c.Site]; {
	case !isAdmin(list, s.From):
		w.logf("web: ignored a new owner for %s from %s, who isn't an admin", c.Site, s.FromName)
	case under:
		w.logf("web: ignored a new owner for %s: names under %s move with the member list", c.Site, peers.GroupDomain(list))
	case e == nil:
		w.logf("web: ignored a new owner for %s: it isn't served here", c.Site)
	default:
		e.Owner = c.Owner
		w.logf("web: %s is published from %s now", c.Site, c.Owner)
	}
}

// mayChange: the node that first published a site may change it, and so
// may admin nodes.
func (w *Web) mayChange(from, owner string) bool {
	return from == owner || isAdmin(w.Peers(), from)
}

func isAdmin(list []peers.Peer, ip string) bool {
	for _, p := range list {
		if p.IP() == ip && p.Admin {
			return true
		}
	}
	return false
}

// servable: a site under the group's domain is served only while someone
// holds its name (or its publisher is an admin), so a name taken back
// stops being served at once. A name given to another node is a site on
// the move: its version is served until the new holder announces its own.
func servable(list []peers.Peer, site, owner string) bool {
	_, given, under := peers.SiteKeeper(list, site)
	return !under || given || isAdmin(list, owner)
}

// mayClaim: www.NAME is served as NAME until it is a site of its own, so
// only NAME's publisher (or an admin) may make it one.
func (w *Web) mayClaim(site, from string) bool {
	parent, ok := strings.CutPrefix(site, "www.")
	if !ok {
		return true
	}
	if p := w.st.Sites[parent]; p != nil {
		return w.mayChange(from, p.Owner)
	}
	return true
}

// shardsAtMembers: a site's parts must be on overlay addresses or members'
// nodes, so an announcement can't send the web node to other addresses.
func (w *Web) shardsAtMembers(m manifest.Manifest) bool {
	known := map[string]bool{}
	for _, p := range w.Peers() {
		known[p.Addr] = true
	}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if !peers.IsOverlay(ref.Peer) && !known[ref.Peer] {
				return false
			}
		}
	}
	return true
}

var fetchMu sync.Mutex // one fetch at a time

func (w *Web) fetchWanted(ctx context.Context) {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	w.mu.Lock()
	todo := map[string]manifest.Manifest{}
	previews := map[string]manifest.Manifest{}
	for name, e := range w.st.Sites {
		if e.Want != nil {
			todo[name] = *e.Want
		}
		if e.PreviewWant != nil {
			previews[name] = *e.PreviewWant
		}
	}
	w.mu.Unlock()
	for name, m := range previews {
		err := w.fetch(ctx, name, m)
		w.mu.Lock()
		if e := w.st.Sites[name]; e != nil && e.PreviewWant != nil && e.PreviewWant.FileID == m.FileID {
			if err != nil {
				w.logf("web: could not fetch the preview of %s (will retry): %v", name, err)
			} else {
				e.Preview, e.PreviewWant = m.FileID, nil
				w.logf("web: preview of %s is version %s", name, m.FileID[:8])
				w.save()
			}
		}
		w.mu.Unlock()
	}
	for name, m := range todo {
		err := w.fetch(ctx, name, m)
		w.mu.Lock()
		e := w.st.Sites[name]
		if e == nil || e.Want == nil || e.Want.FileID != m.FileID {
			w.mu.Unlock()
			continue // changed meanwhile
		}
		if err != nil {
			e.Error = err.Error()
			w.logf("web: could not fetch %s (will retry): %v", name, err)
		} else {
			old := e.Serving
			e.Serving, e.Want, e.Error = m.FileID, nil, ""
			w.roots[name] = filepath.Join(w.Dir, "sites", name, m.FileID)
			w.logf("web: now serving %s version %s (%d files)", name, m.FileID[:8], m.FileCount)
			if e.Code && !w.NoCode {
				go w.warm(name, w.roots[name]) // so the first request needn't wait
			}
			w.cleanup(name, old, m.FileID, e.Preview)
		}
		w.save()
		w.mu.Unlock()
	}
}

// fetch unpacks version m of a site beside the one being served.
func (w *Web) fetch(ctx context.Context, name string, m manifest.Manifest) error {
	if m.Kind != files.KindFolder {
		return errors.New("the announced item is not a folder")
	}
	dir := filepath.Join(w.Dir, "sites", name)
	final := filepath.Join(dir, m.FileID)
	if _, err := os.Stat(final); err == nil {
		return nil // fetched before (a rollback to it)
	}
	incoming := filepath.Join(dir, ".incoming")
	os.RemoveAll(incoming)
	fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	got, err := files.Restore(fctx, w.Client, m, incoming, nil)
	if err != nil {
		os.RemoveAll(incoming)
		return err
	}
	defer os.RemoveAll(incoming)
	return os.Rename(got, final)
}

// cleanup keeps the version being served, the one before it for a quick
// rollback, and the preview.
func (w *Web) cleanup(name, previous, current, preview string) {
	dir := filepath.Join(w.Dir, "sites", name)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := e.Name(); n != previous && n != current && (n != preview || preview == "") {
			os.RemoveAll(filepath.Join(dir, n))
		}
	}
}

// Status lists the sites this node has.
func (w *Web) Status() []SiteStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []SiteStatus
	for name, e := range w.st.Sites {
		s := SiteStatus{Site: name, Serving: e.Serving, Error: e.Error, Owner: e.Owner, Contact: e.Contact != "", Code: e.Code}
		if e.Want != nil {
			s.Waiting = e.Want.FileID
		}
		out = append(out, s)
	}
	return out
}

// ServeHTTP serves the site named by the request's host. Requests for
// www.NAME are served NAME if there is no www.NAME site.
func (w *Web) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	host := Normalise(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == HealthHost {
		rw.Write([]byte("ok\n"))
		return
	}
	if w.Light != nil {
		if d := peers.GroupDomain(w.Peers()); d != "" && host == LightPrefix+d && !(w.Work != nil && r.URL.Path == WorkPath) {
			w.Light.ServeHTTP(rw, r)
			return
		}
		// Someone on a box signs in at their own name: /files there is
		// their box's, the rest their website.
		if p := r.URL.Path; p == "/files" || strings.HasPrefix(p, "/files/") {
			if _, pn, ok := peers.NameHolder(w.Peers(), host); ok && pn.Person != "" && !pn.Site {
				w.Light.ServeHTTP(rw, r)
				return
			}
		}
	}
	if r.URL.Path == mail.IngestPath || r.URL.Path == mail.MailboxPath {
		// A web node without -mail-in must not look like "no such mailbox":
		// the Worker would bounce the mail for good. A failure makes the
		// sending server try again, and reach a node that takes it.
		if w.Mail == nil {
			w.mailMissed.Add(1)
			w.logf("web: mail reached this node, which has no -mail-in: the sender will retry")
			http.Error(rw, "this web node takes no mail: try another", http.StatusServiceUnavailable)
			return
		}
		w.Mail.ServeHTTP(rw, r)
		return
	}
	if w.Work != nil && r.URL.Path == WorkPath {
		w.Work.ServeHTTP(rw, r)
		return
	}
	w.mu.Lock()
	name := host
	root, ok := w.roots[name]
	if !ok {
		name = strings.TrimPrefix(host, "www.")
		root, ok = w.roots[name]
	}
	var owner, contact, preview, previewKey string
	var uploaders, local []string
	code := false
	if e := w.st.Sites[name]; ok && e != nil {
		owner, contact, uploaders, code, local = e.Owner, e.Contact, e.Uploaders, e.Code, e.Local
		preview, previewKey = e.Preview, e.PreviewKey
	}
	w.mu.Unlock()
	if ok && !servable(w.Peers(), name, owner) {
		ok = false
	}
	if !ok {
		http.Error(rw, "There is no site called "+host+" here.", http.StatusNotFound)
		return
	}
	if r.URL.Path == PreviewPath {
		previewSwitch(rw, r, previewKey)
		return
	}
	if localPrefix(local, cleanLocal(r.URL.Path)) != "" {
		w.localApp(rw, r, name, owner, root)
		return
	}
	if p := path.Clean("/" + r.URL.Path); p == "/"+SourceDir || strings.HasPrefix(p, "/"+SourceDir+"/") {
		http.Error(rw, "Not found.", http.StatusNotFound) // a Markdown site's source isn't served
		return
	}
	if r.URL.Path == ContactPath {
		w.contact(rw, r, name, owner, contact)
		return
	}
	if r.URL.Path == ApplyPath {
		w.apply(rw, r, name)
		return
	}
	if r.URL.Path == UploadPath {
		w.upload(rw, r, name, uploaders)
		return
	}
	if p := r.URL.Path; p == strings.TrimSuffix(ApiPath, "/") || strings.HasPrefix(p, ApiPath) {
		w.api(rw, r, name, root, owner, code)
		return
	}
	if preview != "" && previewKey != "" && inPreview(r, previewKey) {
		rw.Header().Set("Cache-Control", "no-store")
		servePreview(rw, r, filepath.Join(w.Dir, "sites", name, preview))
		return
	}
	rw.Header().Set("Cache-Control", "public, max-age=300")
	serveStatic(rw, r, root)
}

// LightPrefix is the label light clients' address takes under the group's
// domain (package light's Host, and a dot).
const LightPrefix = "my."

// WorkPath is where a recovery link opens (package workhere's PagePath).
const WorkPath = "/_yggstore/work"

// serveStatic serves a site's folder.
func serveStatic(rw http.ResponseWriter, r *http.Request, root string) {
	h := rw.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(rw, "This is a static site.", http.StatusMethodNotAllowed)
		return
	}
	// Show the site's own 404.html for missing pages, if it has one.
	clean := filepath.Join(root, filepath.FromSlash(filepath.Clean("/"+r.URL.Path)))
	if _, err := os.Stat(clean); errors.Is(err, os.ErrNotExist) {
		if b, err := os.ReadFile(filepath.Join(root, "404.html")); err == nil {
			h.Set("Content-Type", "text/html")
			rw.WriteHeader(http.StatusNotFound)
			rw.Write(b)
			return
		}
	}
	// Older pages declare their own encoding (often iso-8859-1) in a meta
	// tag; naming none here lets the browser follow it, as Apache does.
	if ext := strings.ToLower(filepath.Ext(r.URL.Path)); ext == ".html" || ext == ".htm" || strings.HasSuffix(r.URL.Path, "/") {
		h.Set("Content-Type", "text/html")
	}
	http.FileServer(noListing{http.Dir(root)}).ServeHTTP(rw, r)
}

// noListing hides folder listings: a folder without index.html is a 404.
type noListing struct{ fs http.FileSystem }

func (n noListing) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.IsDir() {
		idx, err := n.fs.Open(strings.TrimSuffix(name, "/") + "/index.html")
		if err != nil {
			f.Close()
			return nil, os.ErrNotExist
		}
		idx.Close()
	}
	return f, nil
}

func (w *Web) logf(format string, args ...any) {
	if w.Log != nil {
		w.Log(format, args...)
	}
}

// String describes the sites for logs.
func (s SiteStatus) String() string {
	return fmt.Sprintf("%s %s", s.Site, s.Serving)
}
