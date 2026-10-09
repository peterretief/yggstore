package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// Websites: a member publishes a folder as the website of a name the group
// gave them (anna.example.org), from the browser. The web nodes serve it;
// they take it only from the node that holds the name (see site.Web).

const maxSiteUpload = 500 << 20

type websiteState struct {
	Sites []websiteInfo `json:"sites"`
	Error string        `json:"error,omitempty"`
}

type websiteInfo struct {
	Site     string        `json:"site"`
	Name     bool          `json:"name,omitempty"` // one of this node's names
	Versions int           `json:"versions"`
	Current  *site.Version `json:"current,omitempty"`
	Previous bool          `json:"previous,omitempty"` // an earlier version to go back to
	Contact  bool          `json:"contact,omitempty"`
	Served   []string      `json:"served"`            // web nodes serving the current version
	Waiting  []string      `json:"waiting,omitempty"` // web nodes still fetching it, or failing to
}

// msgAnnouncer publishes through this machine's node.
type msgAnnouncer struct{ d *Dashboard }

func (a msgAnnouncer) Publish(ctx context.Context, topic, typ, body string) error {
	l, err := a.d.msgClient()
	if err != nil {
		return err
	}
	_, err = l.Publish(ctx, topic, typ, body)
	return err
}

func (d *Dashboard) publisher() (*site.Publisher, error) {
	if d.cfg.SitesDir == "" {
		return nil, errors.New("websites are not set up for this dashboard")
	}
	return &site.Publisher{Dir: d.cfg.SitesDir, Client: d.cfg.Client, Announce: msgAnnouncer{d}, Log: d.logf}, nil
}

func (d *Dashboard) logf(format string, args ...any) { d.event("info", fmt.Sprintf(format, args...)) }

// mySites is what this node may publish here: its names, and sites already
// published from this machine.
func (d *Dashboard) mySites(pub *site.Publisher) (names map[string]bool, all []string) {
	names = map[string]bool{}
	if list, err := peers.Load(d.cfg.PeersPath); err == nil {
		if own, ok := selfEntry(list, d.cfg.SelfID); ok {
			for _, n := range own.Names {
				names[n] = true
			}
		}
	}
	seen := map[string]bool{}
	for n := range names {
		seen[n] = true
		all = append(all, n)
	}
	for _, s := range pub.Sites() {
		if !seen[s] {
			all = append(all, s)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if names[all[i]] != names[all[j]] {
			return names[all[i]]
		}
		return all[i] < all[j]
	})
	return names, all
}

func (d *Dashboard) handleSites(w http.ResponseWriter, r *http.Request) {
	out := websiteState{Sites: []websiteInfo{}}
	pub, err := d.publisher()
	if err != nil {
		out.Error = err.Error()
		writeJSONResp(w, out)
		return
	}
	// What each web node serves, from the info the poll last got.
	type served struct{ node, serving, waiting string }
	bySite := map[string][]served{}
	d.mu.Lock()
	for _, p := range d.state.Peers {
		if p.Test || !p.Up || p.Info == nil || len(p.Info.Web) == 0 {
			continue
		}
		var st site.NodeStatus
		if json.Unmarshal(p.Info.Web, &st) != nil {
			continue
		}
		for _, s := range st.Sites {
			bySite[s.Site] = append(bySite[s.Site], served{p.Name, s.Serving, s.Waiting})
		}
	}
	d.mu.Unlock()
	names, all := d.mySites(pub)
	for _, s := range all {
		info := websiteInfo{Site: s, Name: names[s], Served: []string{}, Contact: pub.Contact(s) != ""}
		vs, _ := pub.Versions(s)
		info.Versions = len(vs)
		for i, v := range vs {
			if v.Current {
				info.Current = &vs[i]
				info.Previous = i+1 < len(vs)
			}
		}
		if info.Current != nil {
			for _, n := range bySite[s] {
				if n.serving == info.Current.ID {
					info.Served = append(info.Served, n.node)
				} else {
					info.Waiting = append(info.Waiting, n.node)
				}
			}
		}
		out.Sites = append(out.Sites, info)
	}
	writeJSONResp(w, out)
}

// mayPublish: a name of this node's, or a site published from here before.
func (d *Dashboard) mayPublish(pub *site.Publisher, name string) error {
	names, all := d.mySites(pub)
	if names[name] {
		return nil
	}
	for _, s := range all {
		if s == name {
			return nil
		}
	}
	return fmt.Errorf("%s is not one of this box's names; ask for a name under Your name first", name)
}

// handleSitePublish takes a folder as multipart parts named "f:PATH" and
// publishes it as a new version of the site.
func (d *Dashboard) handleSitePublish(w http.ResponseWriter, r *http.Request) {
	pub, err := d.publisher()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	name := site.Normalise(r.URL.Query().Get("site"))
	if err := d.mayPublish(pub, name); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if !d.siteMu.TryLock() {
		http.Error(w, "another website is being published; try again when it is done", http.StatusConflict)
		return
	}
	defer d.siteMu.Unlock()
	if err := os.MkdirAll(d.cfg.SitesDir, 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp, err := os.MkdirTemp(d.cfg.SitesDir, ".upload-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)
	n, err := receiveFolder(http.MaxBytesReader(w, r.Body, maxSiteUpload), r, tmp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if n == 0 {
		http.Error(w, "the folder has no files", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(tmp, "index.html")); err != nil {
		http.Error(w, "the folder has no index.html at its top, so the site would have no home page", http.StatusBadRequest)
		return
	}
	online, err := d.onlinePeers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	v, reused, err := pub.Publish(ctx, name, tmp, online)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	d.event("ok", fmt.Sprintf("website %s: version %s published (%d files)", name, v.ID[:8], v.Files))
	writeJSONResp(w, map[string]any{"version": v, "reused": reused})
}

// receiveFolder writes the upload's files under dir and counts them.
func receiveFolder(body io.Reader, r *http.Request, dir string) (int, error) {
	r.Body = io.NopCloser(body)
	mr, err := r.MultipartReader()
	if err != nil {
		return 0, err
	}
	n := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("reading the upload: %w", err)
		}
		rel, ok := strings.CutPrefix(part.FormName(), "f:")
		if !ok {
			part.Close()
			continue
		}
		if skipFile(rel) {
			part.Close()
			continue
		}
		rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
		if rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)) {
			part.Close()
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return n, err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return n, fmt.Errorf("%s: %w", rel, err)
		}
		_, err = io.Copy(f, part)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		part.Close()
		if err != nil {
			return n, fmt.Errorf("%s: %w", rel, err)
		}
		n++
	}
}

// skipFile leaves out what folders carry but sites don't serve.
func skipFile(rel string) bool {
	for _, c := range strings.Split(rel, "/") {
		switch c {
		case "..", ".git", ".DS_Store", "Thumbs.db", "desktop.ini":
			return true
		}
	}
	return false
}

// onlinePeers is the group's nodes that answered the last poll.
func (d *Dashboard) onlinePeers() ([]peers.Peer, error) {
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return nil, err
	}
	up := map[string]bool{}
	d.mu.Lock()
	for _, p := range d.state.Peers {
		if p.Up && !p.Test {
			up[p.Addr] = true
		}
	}
	d.mu.Unlock()
	var out []peers.Peer
	for _, p := range list {
		if up[p.Addr] && !p.Gateway {
			out = append(out, p)
		}
	}
	return out, nil
}

func (d *Dashboard) handleSiteAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Site string `json:"site"`
		On   bool   `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pub, err := d.publisher()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	name := site.Normalise(req.Site)
	if err := d.mayPublish(pub, name); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	switch r.PathValue("action") {
	case "rollback":
		var v site.Version
		if v, err = pub.Rollback(ctx, name, ""); err == nil {
			d.event("info", fmt.Sprintf("website %s: back to version %s", name, v.ID[:8]))
		}
	case "contact":
		code := ""
		if req.On {
			if d.cfg.Identity == nil {
				err = errors.New("this box has no sharing key for the form's messages")
				break
			}
			code = d.cfg.Identity.Code()
		}
		err = pub.SetContact(ctx, name, code)
	case "remove":
		if err = pub.Remove(ctx, name); err == nil {
			d.event("info", "website "+name+" taken down")
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
