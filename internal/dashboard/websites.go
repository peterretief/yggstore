package dashboard

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/websites"
)

// Websites: a member publishes a folder as the website of a name the group
// gave them (anna.example.org), from the browser. The web nodes serve it;
// they take it only from the node that holds the name (see site.Web).

const maxSiteUpload = 500 << 20

type websiteState struct {
	Sites []websiteInfo `json:"sites"`
	// Folder is the outbox's websites/ folder, and Folders the sites in it
	// with why any isn't published (see the websites package).
	Folder  string            `json:"folder,omitempty"`
	Folders []websites.Status `json:"folders,omitempty"`
	SiteURL string            `json:"site_url,omitempty"` // where this box shows its own site over Yggdrasil
	Default string            `json:"default,omitempty"`  // the site it shows there
	Error   string            `json:"error,omitempty"`
}

type websiteInfo struct {
	Site     string        `json:"site"`
	Name     bool          `json:"name,omitempty"` // one of this node's names
	Versions int           `json:"versions"`
	Current  *site.Version `json:"current,omitempty"`
	Previous bool          `json:"previous,omitempty"` // an earlier version to go back to
	Contact  bool          `json:"contact,omitempty"`
	Forward  *forwardInfo  `json:"forward,omitempty"` // where its contact messages go too
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
	if d.cfg.OwnSites != nil {
		out.SiteURL, out.Default = d.cfg.SiteURL, d.DefaultSite()
	}
	d.mu.Lock()
	ws := d.websites
	d.mu.Unlock()
	if ws != nil {
		out.Folder, out.Folders = ws.Dir(), ws.Status()
	}
	names, all := d.mySites(pub)
	for _, s := range all {
		info := websiteInfo{Site: s, Name: names[s], Served: []string{}, Contact: pub.Contact(s) != ""}
		if f := d.loadForward(s); f != nil {
			info.Forward = &forwardInfo{To: f.To, Confirmed: f.Confirmed}
		}
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

// handleSitePublish publishes a new version of a site from an upload:
// files as multipart parts named "f:PATH", or a zip in a part named "zip"
// (as Download gives). With ?edit=1 the upload changes the version being
// served instead: its files are replaced or added, and the paths in a part
// named "delete" (a JSON list) are removed.
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
	edit := r.URL.Query().Get("edit") == "1"
	if edit {
		if d.cfg.OwnSites == nil {
			http.Error(w, "editing is not set up for this dashboard", http.StatusServiceUnavailable)
			return
		}
		cur, err := d.cfg.OwnSites.Folder(r.Context(), name)
		if err != nil {
			http.Error(w, "fetching the current version: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := copyTree(cur, tmp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := receiveFolder(http.MaxBytesReader(w, r.Body, maxSiteUpload), r, tmp); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	root := tmp
	if !edit {
		root = siteRoot(tmp)
	}
	if n := countFiles(root); n == 0 {
		http.Error(w, "the upload has no files", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		http.Error(w, "there is no index.html at the top of the site, so it would have no home page", http.StatusBadRequest)
		return
	}
	online, err := d.onlinePeers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	v, reused, err := pub.Publish(ctx, name, root, online)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if d.cfg.OwnSites != nil {
		d.cfg.OwnSites.Keep(name, v.ID, root)
	}
	d.event("ok", fmt.Sprintf("website %s: version %s published (%d files)", name, v.ID[:8], v.Files))
	writeJSONResp(w, map[string]any{"version": v, "reused": reused})
}

// siteRoot is where a site's index.html is: the top of the upload, or the
// one folder in it (a folder chosen one level too high, or a zip that holds
// the site's folder).
func siteRoot(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return dir
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) == 1 && entries[0].IsDir() {
		return siteRoot(filepath.Join(dir, entries[0].Name()))
	}
	return dir
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if e.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		return writeFile(filepath.Join(dst, rel), in)
	})
}

// writeFile writes r to path, replacing what is there.
func writeFile(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// sitePath turns a path from the browser into one under the site, or "".
func sitePath(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if skipFile(rel) {
		return ""
	}
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return ""
	}
	return rel
}

// receiveFolder writes the upload's files under dir.
func receiveFolder(body io.Reader, r *http.Request, dir string) error {
	r.Body = io.NopCloser(body)
	mr, err := r.MultipartReader()
	if err != nil {
		return err
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the upload: %w", err)
		}
		err = receivePart(part, dir)
		part.Close()
		if err != nil {
			return err
		}
	}
}

func receivePart(part *multipart.Part, dir string) error {
	switch form := part.FormName(); {
	case form == "zip":
		return unzipInto(part, dir)
	case form == "delete":
		var paths []string
		if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&paths); err != nil {
			return fmt.Errorf("the list of files to delete: %w", err)
		}
		for _, p := range paths {
			if rel := sitePath(p); rel != "" {
				os.RemoveAll(filepath.Join(dir, filepath.FromSlash(rel)))
			}
		}
		return nil
	case strings.HasPrefix(form, "f:"):
		rel := sitePath(strings.TrimPrefix(form, "f:"))
		if rel == "" {
			return nil
		}
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(rel)), part); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil
}

// unzipInto unpacks a zip under dir.
func unzipInto(r io.Reader, dir string) error {
	f, err := os.CreateTemp(dir, ".zip-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	size, err := io.Copy(f, r)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("that is not a zip file: %w", err)
	}
	var total uint64
	for _, zf := range zr.File {
		rel := sitePath(zf.Name)
		if rel == "" || zf.FileInfo().IsDir() || strings.HasPrefix(path.Base(rel), "__MACOSX") || strings.HasPrefix(rel, "__MACOSX/") {
			continue
		}
		if total += zf.UncompressedSize64; total > 2*maxSiteUpload {
			return errors.New("the zip unpacks to more than 1 GB")
		}
		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		err = writeFile(filepath.Join(dir, filepath.FromSlash(rel)), io.LimitReader(rc, int64(2*maxSiteUpload)))
		rc.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil
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
		To   string `json:"to"`   // forward: the address
		Code string `json:"code"` // forward-confirm: the code sent to it
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
		if err = pub.SetContact(ctx, name, code); err == nil {
			err = d.contactLink(ctx, pub, name, req.On)
		}
	case "forward":
		err = d.sendForwardCode(ctx, name, req.To)
	case "forward-confirm":
		err = d.confirmForward(name, req.Code, time.Now())
	case "forward-off":
		if err = d.saveForward(name, nil); err == nil {
			d.event("info", "website "+name+": its contact form's messages stay here only")
		}
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

// DefaultSite is the site this box shows at its own address: its first
// name that has a website, or else the first site published from here.
func (d *Dashboard) DefaultSite() string {
	pub, err := d.publisher()
	if err != nil {
		return ""
	}
	_, all := d.mySites(pub)
	for _, s := range all {
		if _, err := pub.Current(s); err == nil {
			return s
		}
	}
	return ""
}

// handleSiteDownload gives the current version of a site as a zip, to
// change and publish again.
func (d *Dashboard) handleSiteDownload(w http.ResponseWriter, r *http.Request) {
	pub, err := d.publisher()
	if err != nil || d.cfg.OwnSites == nil {
		http.Error(w, "downloads are not set up for this dashboard", http.StatusServiceUnavailable)
		return
	}
	name := site.Normalise(r.URL.Query().Get("site"))
	if err := d.mayPublish(pub, name); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	m, err := pub.Current(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	root, err := d.cfg.OwnSites.Folder(r.Context(), name)
	if err != nil {
		http.Error(w, "fetching it from the group: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.zip"`, name, m.FileID[:8]))
	zw := zip.NewWriter(w)
	filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := e.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name, hdr.Method = name+"/"+filepath.ToSlash(rel), zip.Deflate
		zf, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(zf, f)
		return err
	})
	zw.Close()
}

type siteFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// handleSiteFiles lists the files of the version being served, or with
// ?path= gives one of them, to edit.
func (d *Dashboard) handleSiteFiles(w http.ResponseWriter, r *http.Request) {
	pub, err := d.publisher()
	if err != nil || d.cfg.OwnSites == nil {
		http.Error(w, "editing is not set up for this dashboard", http.StatusServiceUnavailable)
		return
	}
	name := site.Normalise(r.URL.Query().Get("site"))
	if err := d.mayPublish(pub, name); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	root, err := d.cfg.OwnSites.Folder(r.Context(), name)
	if err != nil {
		http.Error(w, "fetching it from the group: "+err.Error(), http.StatusBadGateway)
		return
	}
	if p := r.URL.Query().Get("path"); p != "" {
		rel := sitePath(p)
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if rel == "" || err != nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.Copy(w, io.LimitReader(f, 4<<20))
		return
	}
	out := []siteFile{}
	filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			rel, _ := filepath.Rel(root, p)
			info, _ := e.Info()
			sf := siteFile{Path: filepath.ToSlash(rel)}
			if info != nil {
				sf.Size = info.Size()
			}
			out = append(out, sf)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSONResp(w, out)
}

// contactLink publishes a new version of the site with the contact link on
// its home page (on) or taken off it, when that changes the page.
func (d *Dashboard) contactLink(ctx context.Context, pub *site.Publisher, name string, on bool) error {
	if d.cfg.OwnSites == nil {
		return nil
	}
	if !d.siteMu.TryLock() {
		return errors.New("the form is switched, but another website is being published, so the link on the home page was not changed; try again when it is done")
	}
	defer d.siteMu.Unlock()
	cur, err := d.cfg.OwnSites.Folder(ctx, name)
	if err != nil {
		return fmt.Errorf("the form is switched, but fetching the site to change its home page failed: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(cur, "index.html"))
	if err != nil {
		return nil
	}
	page := site.WithoutContactLink(string(b))
	if on {
		page = site.WithContactLink(page)
	}
	if page == string(b) {
		return nil
	}
	tmp, err := os.MkdirTemp(d.cfg.SitesDir, ".upload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(cur, tmp); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "index.html"), []byte(page), 0o644); err != nil {
		return err
	}
	online, err := d.onlinePeers()
	if err != nil {
		return err
	}
	v, _, err := pub.Publish(ctx, name, tmp, online)
	if err != nil {
		return err
	}
	d.cfg.OwnSites.Keep(name, v.ID, tmp)
	verb := "added to"
	if !on {
		verb = "taken off"
	}
	d.event("ok", fmt.Sprintf("website %s: contact link %s the home page (version %s)", name, verb, v.ID[:8]))
	return nil
}

// runWebsites publishes the sites in the outbox's websites/ folder (see the
// websites package) until ctx ends.
func (d *Dashboard) runWebsites(ctx context.Context) {
	pub, err := d.publisher()
	if err != nil || d.cfg.Outbox == nil {
		return
	}
	pub.Skip = websites.Skip
	service := d.cfg.SiteService
	if service == "" {
		service = "http://localhost:8480"
	}
	w := websites.New(websites.Config{
		Dir:       filepath.Join(d.cfg.Outbox.Dir(), outbox.WebsitesDir),
		StatePath: filepath.Join(d.cfg.Outbox.Dir(), ".yggstore", "websites.json"),
		Pub:       pub, Client: d.cfg.Client,
		Online:    func(context.Context) ([]peers.Peer, error) { return d.onlinePeers() },
		Mine:      func() map[string]bool { names, _ := d.mySites(pub); return names },
		Served:    d.servedSites,
		Answers:   site.Answers,
		Service:   strings.TrimPrefix(service, "http://"),
		TokenPath: d.cfg.CloudflareToken,
		MaxBytes:  maxSiteUpload,
		Route: func(ctx context.Context, domain string) ([]string, error) {
			api, err := os.ReadFile(d.cfg.CloudflareToken)
			if err != nil || d.cfg.CloudflareToken == "" {
				return nil, websites.ErrNoToken
			}
			tun, err := os.ReadFile(d.cfg.TunnelToken)
			if err != nil {
				return nil, fmt.Errorf("can't read the tunnel token: %w", err)
			}
			added, wwwErr, err := site.AddRoutes(ctx, string(api), string(tun), domain, service)
			if wwwErr != nil {
				d.event("warn", fmt.Sprintf("websites: %s: www.%s: %v", domain, domain, wwwErr))
			}
			return added, err
		},
		Logf: func(format string, args ...any) { d.event("info", fmt.Sprintf(format, args...)) },
	})
	d.mu.Lock()
	d.websites = w
	d.mu.Unlock()
	if err := w.Run(ctx); err != nil {
		d.event("bad", "websites: "+err.Error())
	}
}

// servedSites is the sites the group's web nodes serve, from the info the
// poll last got.
func (d *Dashboard) servedSites() map[string]bool {
	out := map[string]bool{}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.state.Peers {
		if p.Test || !p.Up || p.Info == nil || len(p.Info.Web) == 0 {
			continue
		}
		var st site.NodeStatus
		if json.Unmarshal(p.Info.Web, &st) != nil {
			continue
		}
		for _, s := range st.Sites {
			out[s.Site] = true
		}
	}
	return out
}
