package site

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// A site's handlers (see docs/site-code.md, stage 1): a request for
// /api/NAME, or /api/NAME/anything, runs api/NAME.wasm from the version
// being served, a WebAssembly module built for WASI. The request goes in as
// CGI has it, in environment variables and on stdin, and the reply comes
// out on stdout: headers, a blank line, the body. In Go that is any
// http.Handler served with net/http/cgi. Each request gets a fresh instance
// with no files, no network and no other site, stopped after HandlerTime
// with at most HandlerMemory. Modules are compiled once per version and
// kept compiled in Dir/wasm-cache across restarts.

// ApiPath is where a site's handlers answer.
const ApiPath = "/api/"

const (
	HandlerTime     = 2 * time.Second
	HandlerMemory   = 64 << 20 // bytes
	handlerMaxBody  = 64 << 10 // request
	handlerMaxReply = 1 << 20  // reply
	handlersKept    = 32       // compiled modules kept in memory
	compileTime     = time.Minute
	apiPerIP        = 120   // requests per visitor address a minute
	apiPerSite      = 20000 // requests per site a day, per web node
)

// handlerTime is HandlerTime, longer only in tests built with -race,
// where everything runs many times slower.
var handlerTime = HandlerTime

var handlerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// handlers runs sites' modules.
type handlers struct {
	dir string

	once sync.Once
	rt   wazero.Runtime
	err  error

	mu   sync.Mutex
	mods map[string]*compiled // by the module's path, which names its version
}

type compiled struct {
	mod  wazero.CompiledModule
	used time.Time
	err  error
	done chan struct{}
}

func (h *handlers) runtime() (wazero.Runtime, error) {
	h.once.Do(func() {
		ctx := context.Background()
		cfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(HandlerMemory / 65536).WithCloseOnContextDone(true)
		if h.dir != "" {
			if cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(h.dir, "wasm-cache")); err == nil {
				cfg = cfg.WithCompilationCache(cache)
			}
		}
		h.rt = wazero.NewRuntimeWithConfig(ctx, cfg)
		if _, h.err = wasi_snapshot_preview1.Instantiate(ctx, h.rt); h.err == nil {
			h.err = hostModule(ctx, h.rt) // the site's data: datastore.go
		}
		h.mods = map[string]*compiled{}
	})
	return h.rt, h.err
}

// module is the compiled module at path, compiling it the first time;
// requests that come meanwhile wait for the one compile.
func (h *handlers) module(ctx context.Context, path string) (wazero.CompiledModule, error) {
	rt, err := h.runtime()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	c := h.mods[path]
	if c == nil {
		c = &compiled{done: make(chan struct{})}
		h.mods[path] = c
		h.forgetOldLocked(ctx)
		h.mu.Unlock()
		bin, err := os.ReadFile(path)
		if err == nil {
			c.mod, err = rt.CompileModule(context.Background(), bin)
		}
		c.err = err
		close(c.done)
		if err != nil {
			h.mu.Lock()
			delete(h.mods, path)
			h.mu.Unlock()
		}
	} else {
		h.mu.Unlock()
	}
	select {
	case <-c.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	h.mu.Lock()
	c.used = time.Now()
	h.mu.Unlock()
	return c.mod, c.err
}

// forgetOldLocked keeps at most handlersKept modules in memory, dropping
// the least used; they stay compiled on disk. h.mu is held.
func (h *handlers) forgetOldLocked(ctx context.Context) {
	for len(h.mods) > handlersKept {
		var oldest string
		var at time.Time
		for p, c := range h.mods {
			select {
			case <-c.done:
			default:
				continue // still compiling
			}
			if oldest == "" || c.used.Before(at) {
				oldest, at = p, c.used
			}
		}
		if oldest == "" {
			return
		}
		if m := h.mods[oldest].mod; m != nil {
			m.Close(ctx)
		}
		delete(h.mods, oldest)
	}
}

// warm compiles a version's handlers before the first request needs them.
func (h *handlers) warm(root string) {
	paths, _ := filepath.Glob(filepath.Join(root, "api", "*.wasm"))
	for _, p := range paths {
		if handlerName.MatchString(strings.TrimSuffix(filepath.Base(p), ".wasm")) {
			h.module(context.Background(), p)
		}
	}
}

// apiName is the handler a path names: /api/register/x is "register".
func apiName(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, ApiPath)
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	return name, handlerName.MatchString(name)
}

// api answers a request for one of a site's handlers.
func (w *Web) api(rw http.ResponseWriter, r *http.Request, site, root, owner string, on bool) {
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	name, ok := apiName(r.URL.Path)
	path := filepath.Join(root, "api", name+".wasm")
	if _, err := os.Stat(path); !ok || err != nil || !on {
		http.Error(rw, "There is no such address on "+site+".", http.StatusNotFound)
		return
	}
	switch {
	case w.NoCode:
		http.Error(rw, "This web node doesn't run sites' code. Try again: another may.", http.StatusServiceUnavailable)
		return
	case !isAdmin(w.Peers(), owner):
		// The group's admin approves code for other publishers (not built
		// yet); until then only the admin's own sites run it.
		http.Error(rw, "Code on "+site+" waits for the group's admin to allow it.", http.StatusForbidden)
		return
	}
	ip := visitor(r)
	if !w.limit.allow("api ip "+ip, apiPerIP, time.Minute) || !w.limit.allow("api site "+site, apiPerSite, 24*time.Hour) {
		http.Error(rw, "Too many requests: try again in a minute.", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, handlerMaxBody+1))
	if err != nil {
		http.Error(rw, "The request didn't arrive whole.", http.StatusBadRequest)
		return
	}
	if len(body) > handlerMaxBody {
		http.Error(rw, "The request is too big.", http.StatusRequestEntityTooLarge)
		return
	}
	// Compiling (once per version, or after a restart without the cache)
	// isn't the handler's time.
	cctx, ccancel := context.WithTimeout(r.Context(), compileTime)
	defer ccancel()
	mod, err := w.code.module(cctx, path)
	if err != nil {
		w.logf("web: %s%s: compiling %s: %v", site, r.URL.Path, filepath.Base(path), err)
		http.Error(rw, "The site's code couldn't be started.", http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), handlerTime)
	defer cancel()
	ctx = context.WithValue(ctx, callKey{}, &call{w: w, site: site, root: root, ctx: ctx})
	out, err := w.code.run(ctx, mod, r, site, ip, body)
	if err != nil {
		w.logf("web: %s%s: %v", site, r.URL.Path, err)
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(rw, "The site's code took too long.", http.StatusGatewayTimeout)
		} else {
			http.Error(rw, "The site's code failed.", http.StatusBadGateway)
		}
		return
	}
	writeCGI(rw, out)
}

// run runs a module for one request and gives what it wrote.
func (h *handlers) run(ctx context.Context, mod wazero.CompiledModule, r *http.Request, site, ip string, body []byte) ([]byte, error) {
	out := &capped{max: handlerMaxReply}
	var stderr capped
	stderr.max = 4 << 10
	cfg := wazero.NewModuleConfig().WithName("").WithStdin(bytes.NewReader(body)).WithStdout(out).WithStderr(&stderr).
		WithSysWalltime().WithSysNanotime().WithRandSource(rand.Reader).WithArgs("handler")
	for k, v := range cgiEnv(r, site, ip, len(body)) {
		cfg = cfg.WithEnv(k, v)
	}
	m, err := h.rt.InstantiateModule(ctx, mod, cfg)
	if m != nil {
		m.Close(context.Background())
	}
	if ctx.Err() != nil {
		return nil, context.DeadlineExceeded
	}
	if err != nil && !strings.Contains(err.Error(), "exit_code(0)") {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	if out.over {
		return nil, errors.New("its reply is too big")
	}
	return out.Bytes(), nil
}

// cgiEnv is a request as CGI's environment variables.
func cgiEnv(r *http.Request, site, ip string, n int) map[string]string {
	env := map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_PROTOCOL":   "HTTP/1.1",
		"SERVER_SOFTWARE":   "yggstore",
		"SERVER_NAME":       site,
		"SERVER_PORT":       "443",
		"HTTPS":             "on",
		"REQUEST_METHOD":    r.Method,
		"REQUEST_URI":       r.URL.RequestURI(),
		"SCRIPT_NAME":       "",
		"PATH_INFO":         r.URL.Path,
		"QUERY_STRING":      r.URL.RawQuery,
		"REMOTE_ADDR":       ip,
		"CONTENT_LENGTH":    strconv.Itoa(n),
		"HTTP_HOST":         site,
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		env["CONTENT_TYPE"] = ct
	}
	for k, vs := range r.Header {
		switch k {
		case "Content-Type", "Content-Length", "Host", "Authorization", "Proxy-Authorization", "Cf-Connecting-Ip", "X-Forwarded-For":
			continue
		}
		env["HTTP_"+strings.ReplaceAll(strings.ToUpper(k), "-", "_")] = strings.Join(vs, ", ")
	}
	return env
}

// writeCGI sends a handler's CGI reply on.
func writeCGI(rw http.ResponseWriter, out []byte) {
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(out)))
	head, err := tp.ReadMIMEHeader()
	if err != nil && len(head) == 0 {
		http.Error(rw, "The site's code gave no reply.", http.StatusBadGateway)
		return
	}
	status := http.StatusOK
	if s := head.Get("Status"); s != "" {
		if n, err := strconv.Atoi(strings.Fields(s)[0]); err == nil && n >= 100 && n < 600 {
			status = n
		}
	} else if head.Get("Location") != "" {
		status = http.StatusFound
	}
	h := rw.Header()
	for k, vs := range head {
		switch k {
		case "Status", "Content-Length", "Connection", "Transfer-Encoding", "Strict-Transport-Security":
			continue
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	rw.WriteHeader(status)
	io.Copy(rw, tp.R)
}

// capped keeps at most max bytes, noting whether more came.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); len(p) > room {
		c.over = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}
