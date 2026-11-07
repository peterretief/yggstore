package site

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
)

// Dev serves a site's folder on the developer's own machine exactly as the
// group's web nodes would serve it once published (yggstore site dev):
// Markdown pages built, notices, /api/ handlers with the site's data, the
// site's 404 page. It rebuilds when the folder changes, and pages reload
// themselves. The site's data is kept in the folder's .yggstore-dev/,
// which isn't published, so it lasts from one run to the next.
type Dev struct {
	Folder string
	Name   string // the site's domain, for {{site}} and the pages' links
	Log    func(string, ...any)
	// Local, if set, is used for the site's local paths in place of its
	// LocalConfig: a development copy of the program, not the live one.
	Local map[string]string

	web     *Web
	mu      sync.Mutex
	version int64 // changes on every build, for the pages to reload
	built   string
	seen    devScan
}

// DevDir is where a folder's local web node keeps its builds and data.
const DevDir = ".yggstore-dev"

// devReloadPath is what pages ask to learn of a new build.
const devReloadPath = "/_yggstore/dev"

type devScan struct {
	files  int
	bytes  int64
	newest int64
}

func scanFolder(dir string) devScan {
	var sc devScan
	filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		if n := e.Name(); n == DevDir || n == "node_modules" || n == ".git" {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			sc.files++
			sc.bytes += info.Size()
			sc.newest = max(sc.newest, info.ModTime().UnixNano())
		}
		return nil
	})
	return sc
}

// Start builds the folder once and sets up the local web node.
func (d *Dev) Start() error {
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	state := filepath.Join(d.Folder, DevDir)
	if err := os.MkdirAll(filepath.Join(state, "sites", d.Name), 0o700); err != nil {
		return err
	}
	// The developer's machine is its own group: its only web node, and the
	// admin, so the site's code runs with no one to allow it.
	self := peers.Peer{Name: "dev", Addr: "[::1]:7400", Admin: true}
	d.web = &Web{Dir: state, Peers: func() []peers.Peer { return []peers.Peer{self} }, Log: d.Log, Self: "dev"}
	d.web.st = webState{Sites: map[string]*webEntry{}}
	d.web.roots = map[string]string{}
	return d.build()
}

// build builds the folder into a new version and serves it.
func (d *Dev) build() error {
	sc := scanFolder(d.Folder)
	d.mu.Lock()
	n := d.version + 1
	d.mu.Unlock()
	dir := filepath.Join(d.Folder, DevDir, "sites", d.Name, "build-"+strconv.FormatInt(n, 10))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var err error
	if HasMarkdown(d.Folder) {
		var pages int
		pages, _, err = BuildMarkdown(d.Folder, dir, d.Name, false)
		if err == nil {
			d.Log("built %d pages from Markdown", pages)
		}
	} else {
		err = copyTree(d.Folder, dir)
	}
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	d.mu.Lock()
	old := d.built
	d.version, d.built, d.seen = n, dir, sc
	d.mu.Unlock()
	d.web.mu.Lock()
	d.web.st.Sites[d.Name] = &webEntry{Owner: "::1", Serving: filepath.Base(dir), Code: true}
	d.web.roots[d.Name] = dir
	d.web.mu.Unlock()
	if old != "" {
		os.RemoveAll(old)
	}
	return nil
}

// copyTree copies a plain folder (no Markdown) as publishing would take it.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e os.DirEntry, err error) error {
		if err != nil || p == src {
			return err
		}
		if n := e.Name(); n == DevDir || n == "node_modules" || n == ".git" || n == LocalConfig {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() || !e.Type().IsRegular() {
			return nil
		}
		return copyFile(p, filepath.Join(dst, must(filepath.Rel(src, p))))
	})
}

// Watch rebuilds when the folder changes, until ctx ends.
func (d *Dev) Watch(ctx context.Context) {
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sc := scanFolder(d.Folder)
		d.mu.Lock()
		changed := sc != d.seen
		d.mu.Unlock()
		if !changed {
			continue
		}
		time.Sleep(300 * time.Millisecond) // let a save finish
		if err := d.build(); err != nil {
			d.Log("couldn't build: %v (fix it and save again)", err)
			d.mu.Lock()
			d.seen = sc
			d.mu.Unlock()
			continue
		}
		d.Log("rebuilt: the pages reload")
	}
}

// devReloader goes at the end of every page: it reloads the page after a
// new build.
const devReloader = `<script>(()=>{let v=null;setInterval(async()=>{try{const r=await fetch("` + devReloadPath +
	`",{cache:"no-store"});const n=(await r.json()).version;if(v!==null&&n!==v)location.reload();v=n}catch(e){}},1000)})()</script>`

// ServeHTTP serves the site as a web node would, whatever host it's asked
// for (localhost), with the reloader on its pages. The contact form's
// messages are printed instead of sent.
func (d *Dev) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case devReloadPath:
		d.mu.Lock()
		v := d.version
		d.mu.Unlock()
		rw.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(rw).Encode(map[string]int64{"version": v})
		return
	case ContactPath:
		d.devContact(rw, r)
		return
	}
	if d.devLocal(rw, r) {
		return
	}
	r.Host = d.Name
	rec := &devRecorder{h: http.Header{}, buf: &bytes.Buffer{}}
	d.web.ServeHTTP(rec, r)
	for k, v := range rec.h {
		rw.Header()[k] = v
	}
	body := rec.buf.Bytes()
	if strings.HasPrefix(rec.h.Get("Content-Type"), "text/html") {
		page := string(body)
		if i := strings.LastIndex(strings.ToLower(page), "</body>"); i >= 0 {
			page = page[:i] + devReloader + page[i:]
		} else {
			page += devReloader
		}
		body = []byte(page)
		rw.Header().Del("Content-Length")
	}
	rw.Header().Set("Cache-Control", "no-store")
	code := rec.code
	if code == 0 {
		code = http.StatusOK
	}
	rw.WriteHeader(code)
	rw.Write(body)
}

// devContact prints what the contact form sent, and answers as the web
// nodes do.
func (d *Dev) devContact(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "Send the form with POST.", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 64<<10)
	if err := r.ParseMultipartForm(1 << 20); err != nil && err != http.ErrNotMultipart {
		r.ParseForm()
	}
	var lines []string
	for k, vs := range r.PostForm {
		if strings.HasPrefix(k, "_") {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", k, strings.Join(vs, ", ")))
	}
	d.Log("the contact form sent (on the group it would go to the site's owner):\n%s", strings.Join(lines, "\n"))
	if next := r.PostForm.Get("_next"); next != "" && strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		http.Redirect(rw, r, next, http.StatusSeeOther)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	io.WriteString(rw, `{"ok":true}`)
}

type devRecorder struct {
	h    http.Header
	code int
	buf  *bytes.Buffer
}

func (r *devRecorder) Header() http.Header         { return r.h }
func (r *devRecorder) WriteHeader(code int)        { r.code = code }
func (r *devRecorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
