package site

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
)

// Own serves the sites published from this machine at its own address (its
// Yggdrasil address, say), so the group can see them without Cloudflare
// and without a web node. A request naming one of those sites gets it;
// any other gets Default, the box's own site. It keeps a copy of each
// site's current version in Dir, fetched from the group when first asked.
type Own struct {
	Pub     *Publisher
	Client  client.Client
	Dir     string
	Default func() string // the site a bare address shows

	mu sync.Mutex
}

// Folder is the current version of a site, fetched if need be.
func (o *Own) Folder(ctx context.Context, name string) (string, error) {
	m, err := o.Pub.Current(name)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(o.Dir, name)
	final := filepath.Join(dir, m.FileID)
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}
	incoming := filepath.Join(dir, ".incoming")
	os.RemoveAll(incoming)
	fctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	got, err := files.Restore(fctx, o.Client, m, incoming, nil)
	if err != nil {
		os.RemoveAll(incoming)
		return "", err
	}
	defer os.RemoveAll(incoming)
	if err := os.Rename(got, final); err != nil {
		return "", err
	}
	// Only the current version is kept here.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != m.FileID {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return final, nil
}

func (o *Own) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	host := Normalise(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	name := ""
	for _, s := range o.Pub.Sites() {
		if s == host || "www."+s == host {
			name = s
		}
	}
	if name == "" && o.Default != nil {
		name = o.Default()
	}
	if name == "" {
		http.Error(rw, "No website has been published from this box yet.", http.StatusNotFound)
		return
	}
	root, err := o.Folder(r.Context(), name)
	if err != nil {
		http.Error(rw, "This box can't show "+name+" just now: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	rw.Header().Set("Cache-Control", "no-cache") // so a new version shows at once
	serveStatic(rw, r, root)
}

// Sites is what Own can serve, its default first.
func (o *Own) Sites() []string {
	var out []string
	def := ""
	if o.Default != nil {
		def = o.Default()
	}
	if def != "" {
		out = append(out, def)
	}
	for _, s := range o.Pub.Sites() {
		if s != def && !strings.HasPrefix(s, ".") {
			out = append(out, s)
		}
	}
	return out
}

// Keep takes folder as the copy of version id of a site, as just published
// from here, so it isn't fetched back from the group.
func (o *Own) Keep(name, id, folder string) {
	dir := filepath.Join(o.Dir, name)
	o.mu.Lock()
	defer o.mu.Unlock()
	if os.MkdirAll(dir, 0o755) != nil || os.Rename(folder, filepath.Join(dir, id)) != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != id {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}
