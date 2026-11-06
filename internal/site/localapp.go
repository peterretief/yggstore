package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
)

// Local apps (docs/local-apps.md): a site's pages are published to the
// group, and paths it names in LocalConfig go, over Yggdrasil, to a program
// on the publisher's own machine, whose data never leaves it. The web node
// passes such a request to the publisher's node (Web.localApp); the node's
// LocalRelay takes it only from the group's web nodes and hands it to the
// program. Only the paths are announced: where the program listens stays
// on the machine, in LocalApps.
const (
	LocalConfig   = ".yggstore.json" // in the site's folder; never published
	localBase     = "/v1/local/"
	maxLocalPaths = 5
	maxLocalBody  = 10 << 20
	localTimeout  = 30 * time.Second
	OfflinePage   = "_offline.html"
)

// visitor headers a request can't bring with it: only the web node sets them.
var localHeaders = []string{"X-Yggstore-Site", "X-Yggstore-Visitor"}

// localDown marks the relay's own refusals, so the web node shows its
// offline page for them but passes on anything the program answers.
const localDown = "X-Yggstore-Local-Down"

// ReadLocalConfig reads the site's paths and where each goes: {} if the
// folder has no LocalConfig.
func ReadLocalConfig(folder string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(folder, LocalConfig))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var c struct {
		Local map[string]string `json:"local"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", LocalConfig, err)
	}
	if len(c.Local) > maxLocalPaths {
		return nil, fmt.Errorf("%s: at most %d paths", LocalConfig, maxLocalPaths)
	}
	for p, to := range c.Local {
		if err := validLocal(p, to); err != nil {
			return nil, fmt.Errorf("%s: %w", LocalConfig, err)
		}
	}
	return c.Local, nil
}

func validLocal(p, to string) error {
	if !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, "/") || p == "/" || path.Clean(p)+"/" != p || strings.HasPrefix(p, "/_yggstore/") {
		return fmt.Errorf("%q isn't a path like /app/", p)
	}
	u, err := url.Parse(to)
	if err != nil || u.Scheme != "http" || u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%q isn't an address like http://127.0.0.1:3000", to)
	}
	// Only this machine: a site's folder must not send requests elsewhere.
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s isn't on this machine", to)
	}
	return nil
}

// LocalApps is the file, on the publisher's machine, of where each site's
// local paths go. The dashboard writes it when publishing; the node reads it.
type LocalApps struct{ Path string }

func (l LocalApps) load() map[string]map[string]string {
	out := map[string]map[string]string{}
	if b, err := os.ReadFile(l.Path); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

// Set records site's paths; none removes it.
func (l LocalApps) Set(site string, paths map[string]string) error {
	if l.Path == "" {
		return nil
	}
	all := l.load()
	if len(paths) == 0 {
		if _, ok := all[site]; !ok {
			return nil
		}
		delete(all, site)
	} else {
		all[site] = paths
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	return atomicfile.Replace(l.Path, append(b, '\n'), 0o600)
}

// cleanLocal is a request's path as local paths are matched against it.
func cleanLocal(p string) string {
	c := path.Clean("/" + p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

// localPrefix is the path among paths that p falls under, longest first.
func localPrefix(paths []string, p string) string {
	best := ""
	for _, x := range paths {
		if (strings.HasPrefix(p, x) || p+"/" == x) && len(x) > len(best) {
			best = x
		}
	}
	return best
}

// localApp passes a request to the site's publisher's node.
func (w *Web) localApp(rw http.ResponseWriter, r *http.Request, site, owner, root string) {
	var addr string
	for _, p := range w.Peers() {
		if p.IP() == owner && addr == "" {
			addr = p.Addr
		}
	}
	if addr == "" {
		localOffline(rw, root, site)
		return
	}
	ip := visitor(r)
	r.Body = http.MaxBytesReader(rw, r.Body, maxLocalBody)
	ctx, cancel := context.WithTimeout(r.Context(), localTimeout)
	defer cancel()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", addr
			pr.Out.URL.Path = localBase + site + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = addr
			for _, h := range localHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Set("X-Yggstore-Site", site)
			pr.Out.Header.Set("X-Yggstore-Visitor", ip)
		},
		Transport: w.Client.HTTP.Transport,
		ModifyResponse: func(res *http.Response) error {
			if res.Header.Get(localDown) != "" {
				return fmt.Errorf("the node says %s", res.Status)
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			w.logf("web: %s's local app: %v", site, err)
			localOffline(rw, root, site)
		},
	}
	proxy.ServeHTTP(rw, r.WithContext(ctx))
}

// localOffline answers when the publisher's machine doesn't: the site's
// own OfflinePage, or a short one.
func localOffline(rw http.ResponseWriter, root, site string) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Retry-After", "60")
	rw.WriteHeader(http.StatusServiceUnavailable)
	if root != "" {
		if b, err := os.ReadFile(filepath.Join(root, OfflinePage)); err == nil {
			rw.Write(b)
			return
		}
	}
	fmt.Fprintf(rw, "<!doctype html><title>Back soon</title><h1>Back soon</h1><p>This part of %s isn't available just now. The rest of the site is.</p>", html.EscapeString(site))
}

// LocalRelay is the node's side: it takes requests from the group's web
// nodes and hands them to the program on this machine.
type LocalRelay struct {
	Apps LocalApps
	// IsWebNode says whether a member node is one of the group's web
	// nodes, and Self is this node's ID: a web node may be the publisher too.
	IsWebNode func(ctx context.Context, caller string) bool
	Self      string
	Log       func(format string, args ...any)

	mu   sync.Mutex
	seen map[string]localAnswer // asking is slow, so answers are kept a while
}

type localAnswer struct {
	yes   bool
	until time.Time
}

func (l *LocalRelay) webNode(ctx context.Context, caller string) bool {
	if caller == l.Self {
		return true
	}
	if l.IsWebNode == nil {
		return false
	}
	l.mu.Lock()
	a, ok := l.seen[caller]
	l.mu.Unlock()
	if ok && time.Now().Before(a.until) {
		return a.yes
	}
	yes := l.IsWebNode(ctx, caller)
	keep := 30 * time.Second
	if yes {
		keep = 5 * time.Minute
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]localAnswer{}
	}
	l.seen[caller] = localAnswer{yes, time.Now().Add(keep)}
	return yes
}

// Serve answers localBase for a member node (caller); it reports whether
// the request was its to answer.
func (l *LocalRelay) Serve(w http.ResponseWriter, r *http.Request, caller string) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, localBase)
	if !ok {
		return false
	}
	down := func(msg string, code int) {
		w.Header().Set(localDown, "1")
		http.Error(w, msg, code)
	}
	if !l.webNode(r.Context(), caller) {
		down("only the group's web nodes", http.StatusForbidden)
		return true
	}
	site, p, _ := strings.Cut(rest, "/")
	p = cleanLocal(p)
	apps := l.Apps.load()[site]
	var paths []string
	for x := range apps {
		paths = append(paths, x)
	}
	prefix := localPrefix(paths, p)
	if prefix == "" {
		down("no such local path", http.StatusNotFound)
		return true
	}
	target, err := url.Parse(apps[prefix])
	if err != nil {
		down("bad local app", http.StatusBadGateway)
		return true
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = p, ""
			pr.Out.Host = site
			pr.Out.Header.Set("X-Forwarded-For", pr.In.Header.Get("X-Yggstore-Visitor"))
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", site)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if l.Log != nil {
				l.Log("local app for %s: %v", site, err)
			}
			down("the local app isn't answering", http.StatusBadGateway)
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del(localDown)
			return nil
		},
	}
	proxy.ServeHTTP(w, r)
	return true
}

// devLocal passes a request under one of the folder's local paths straight
// to its program, as the relay would (but not as https, which it isn't).
func (d *Dev) devLocal(rw http.ResponseWriter, r *http.Request) bool {
	apps, err := ReadLocalConfig(d.Folder)
	if err != nil {
		d.Log("%v", err)
		return false
	}
	var paths []string
	for x := range apps {
		paths = append(paths, x)
	}
	p := cleanLocal(r.URL.Path)
	prefix := localPrefix(paths, p)
	if prefix == "" {
		return false
	}
	target, err := url.Parse(apps[prefix])
	if err != nil {
		return false
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = p, ""
			pr.Out.Host = d.Name
			pr.Out.Header.Set("X-Forwarded-For", "127.0.0.1")
			pr.Out.Header.Set("X-Forwarded-Host", d.Name)
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			d.Log("%s isn't answering: is your program running? %v", apps[prefix], err)
			d.mu.Lock()
			root := d.built
			d.mu.Unlock()
			localOffline(rw, root, d.Name)
		},
	}
	proxy.ServeHTTP(rw, r)
	return true
}
