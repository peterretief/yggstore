package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
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
	Dir    string
	Msgs   Messages
	Peers  func() []peers.Peer
	Client client.Client
	Log    func(string, ...any)

	mu    sync.Mutex
	st    webState
	roots map[string]string // site → folder being served
}

type webState struct {
	After int64                `json:"after"` // last message handled
	Sites map[string]*webEntry `json:"sites"`
}

type webEntry struct {
	Owner     string             `json:"owner"`     // node that published it first
	Announced int64              `json:"announced"` // time of the latest announcement, ms
	Serving   string             `json:"serving,omitempty"`
	Want      *manifest.Manifest `json:"want,omitempty"` // announced, not fetched yet
	Error     string             `json:"error,omitempty"`
}

// SiteStatus is one site as a web node has it.
type SiteStatus struct {
	Site    string `json:"site"`
	Serving string `json:"serving,omitempty"` // version ID
	Waiting string `json:"waiting,omitempty"` // newer version not fetched yet
	Error   string `json:"error,omitempty"`
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
		for _, s := range w.Msgs.Wait(ctx, after, Topic) {
			w.handle(ctx, s)
		}
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

func (w *Web) handle(ctx context.Context, s msg.Stored) {
	w.mu.Lock()
	w.st.After = max(w.st.After, s.N)
	a, ok := decode(s)
	if !ok {
		w.save()
		w.mu.Unlock()
		return
	}
	e := w.st.Sites[a.Site]
	switch {
	case e != nil && !w.mayChange(s.From, e.Owner):
		w.logf("web: ignored a change to %s from %s, who didn't publish it", a.Site, s.FromName)
	case e != nil && s.Time <= e.Announced:
		// an older announcement, arriving late
	case a.Stub == nil:
		if e != nil {
			delete(w.st.Sites, a.Site)
			delete(w.roots, a.Site)
			os.RemoveAll(filepath.Join(w.Dir, "sites", a.Site))
			w.logf("web: %s removed", a.Site)
		}
	default:
		if e == nil {
			e = &webEntry{Owner: s.From}
			w.st.Sites[a.Site] = e
		}
		e.Announced = s.Time
		if a.Stub.FileID == e.Serving {
			e.Want = nil
		} else {
			e.Want, e.Error = a.Stub, ""
		}
	}
	w.save()
	w.mu.Unlock()
	w.fetchWanted(ctx)
}

// mayChange: the node that first published a site may change it, and so
// may admin nodes.
func (w *Web) mayChange(from, owner string) bool {
	if from == owner {
		return true
	}
	for _, p := range w.Peers() {
		if p.IP() == from && p.Admin {
			return true
		}
	}
	return false
}

var fetchMu sync.Mutex // one fetch at a time

func (w *Web) fetchWanted(ctx context.Context) {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	w.mu.Lock()
	todo := map[string]manifest.Manifest{}
	for name, e := range w.st.Sites {
		if e.Want != nil {
			todo[name] = *e.Want
		}
	}
	w.mu.Unlock()
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
			w.cleanup(name, old, m.FileID)
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

// cleanup keeps the version being served and the one before it, for a
// quick rollback.
func (w *Web) cleanup(name, previous, current string) {
	dir := filepath.Join(w.Dir, "sites", name)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := e.Name(); n != previous && n != current {
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
		s := SiteStatus{Site: name, Serving: e.Serving, Error: e.Error}
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
	w.mu.Lock()
	root, ok := w.roots[host]
	if !ok {
		root, ok = w.roots[strings.TrimPrefix(host, "www.")]
	}
	w.mu.Unlock()
	if !ok {
		http.Error(rw, "There is no site called "+host+" here.", http.StatusNotFound)
		return
	}
	h := rw.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=300")
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
