package site

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
)

// LightPrefix is the label light clients' address takes under the group's
// domain (package light's Host, and a dot).
const LightPrefix = "my."

// WorkPath is where a recovery link opens (package workhere's PagePath).
const WorkPath = "/_yggstore/work"

// ServeHTTP serves the site named by the request's host. Requests for
// www.NAME are served NAME if there is no www.NAME site.
func (w *Web) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	host := Normalise(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if w.serveNode(rw, r, host) {
		return
	}
	s, ok := w.lookup(host)
	if !ok {
		http.Error(rw, "There is no site called "+host+" here.", http.StatusNotFound)
		return
	}
	w.serveSite(rw, r, s)
}

// serveNode answers what isn't any one site's: the health check, light
// clients, mail and recovery links. It says whether it answered.
func (w *Web) serveNode(rw http.ResponseWriter, r *http.Request, host string) bool {
	if host == HealthHost {
		rw.Write([]byte("ok\n"))
		return true
	}
	if w.Light != nil {
		if d := peers.GroupDomain(w.Peers()); d != "" && host == LightPrefix+d && !(w.Work != nil && r.URL.Path == WorkPath) {
			w.Light.ServeHTTP(rw, r)
			return true
		}
		// Someone on a box signs in at their own name: /files there is
		// their box's, the rest their website.
		if p := r.URL.Path; p == "/files" || strings.HasPrefix(p, "/files/") {
			if _, pn, ok := peers.NameHolder(w.Peers(), host); ok && pn.Person != "" && !pn.Site {
				w.Light.ServeHTTP(rw, r)
				return true
			}
		}
	}
	if r.URL.Path == mail.IngestPath || r.URL.Path == mail.MailboxPath {
		w.serveMail(rw, r)
		return true
	}
	if w.Work != nil && r.URL.Path == WorkPath {
		w.Work.ServeHTTP(rw, r)
		return true
	}
	return false
}

// serveMail passes the mail Worker's posts to Mail. A web node without
// -mail-in must not look like "no such mailbox": the Worker would bounce
// the mail for good. A failure makes the sending server try again, and
// reach a node that takes it.
func (w *Web) serveMail(rw http.ResponseWriter, r *http.Request) {
	if w.Mail == nil {
		w.mailMissed.Add(1)
		w.logf("web: mail reached this node, which has no -mail-in: the sender will retry")
		http.Error(rw, "this web node takes no mail: try another", http.StatusServiceUnavailable)
		return
	}
	w.Mail.ServeHTTP(rw, r)
}

// served is a site as one request sees it.
type served struct {
	name, root                          string
	owner, contact, preview, previewKey string
	uploaders, local, machines          []string
	code                                bool
}

// lookup finds the site served at host (or, failing that, at host without
// "www."), if it may be served.
func (w *Web) lookup(host string) (served, bool) {
	w.mu.Lock()
	name := host
	root, ok := w.roots[name]
	if !ok {
		name = strings.TrimPrefix(host, "www.")
		root, ok = w.roots[name]
	}
	s := served{name: name, root: root}
	if e := w.st.Sites[name]; ok && e != nil {
		s.owner, s.contact, s.uploaders, s.code, s.local, s.machines = e.Owner, e.Contact, e.Uploaders, e.Code, e.Local, e.Machines
		s.preview, s.previewKey = e.Preview, e.PreviewKey
	}
	w.mu.Unlock()
	if !ok || !servable(w.Peers(), name, s.owner) {
		return served{}, false
	}
	return s, true
}

// serveSite answers a request to site s: its forms, handlers and private
// part first, then its files.
func (w *Web) serveSite(rw http.ResponseWriter, r *http.Request, s served) {
	p := r.URL.Path
	switch {
	case p == PreviewPath:
		previewSwitch(rw, r, s.previewKey)
	case localPrefix(s.local, cleanLocal(p)) != "":
		w.localApp(rw, r, s.name, s.owner, s.machines, s.root)
	case isSource(p):
		http.Error(rw, "Not found.", http.StatusNotFound) // a Markdown site's source isn't served
	case p == ContactPath:
		w.contact(rw, r, s.name, s.owner, s.contact)
	case p == ApplyPath:
		w.apply(rw, r, s.name)
	case p == UploadPath:
		w.upload(rw, r, s.name, s.uploaders)
	case p == strings.TrimSuffix(ApiPath, "/") || strings.HasPrefix(p, ApiPath):
		w.api(rw, r, s.name, s.root, s.owner, s.code)
	case s.preview != "" && s.previewKey != "" && inPreview(r, s.previewKey):
		rw.Header().Set("Cache-Control", "no-store")
		servePreview(rw, r, w.versionDir(s.name, s.preview))
	default:
		rw.Header().Set("Cache-Control", "public, max-age=300")
		serveStatic(rw, r, s.root)
	}
}

// isSource: the path is in a Markdown site's source folder.
func isSource(urlPath string) bool {
	p := path.Clean("/" + urlPath)
	return p == "/"+SourceDir || strings.HasPrefix(p, "/"+SourceDir+"/")
}

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
