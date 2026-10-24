package site

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Sites' data on the web nodes (data.go), as handlers use it and the web
// nodes share it. A handler calls two functions the web node gives it,
// yggstore.db_query and yggstore.db_record, each with a JSON request in its
// memory and room for the answer. Entries go straight to the other web
// nodes over Yggdrasil (/v1/site-data/), not on a topic every member can
// read, and each web node asks the others now and then for what it missed.

const (
	dataPath      = "/v1/site-data/"
	maxQueryRows  = 1000
	pullEvery     = time.Minute
	shareTimeout  = time.Second // within the handler's own time
	pullBatch     = 500
	webNodesFresh = 5 * time.Minute
)

// dataSites keeps the sites' data open.
type dataSites struct {
	mu    sync.Mutex
	open  map[string]*siteData
	sql   map[string]siteSQL // by the version's folder
	pulls map[string]int64   // "site peer" → the peer's got time reached

	webMu      sync.Mutex
	web        map[string]bool // the web nodes' IP addresses
	webAddrs   []string        // their node API addresses
	webAt      time.Time
	refreshing bool
}

// others are the web nodes' node API addresses, kept for webNodesFresh:
// finding them asks every node in the group, too slow for each write. A
// stale list is used while a fresh one is fetched.
func (w *Web) others() []string {
	if w.Others == nil {
		return nil
	}
	w.data.webMu.Lock()
	fresh := time.Since(w.data.webAt) < webNodesFresh
	have := w.data.webAt != (time.Time{})
	if !fresh && !w.data.refreshing {
		w.data.refreshing = true
		done := make(chan struct{})
		go func() {
			defer close(done)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			list := w.Others(ctx)
			ips := map[string]bool{}
			for _, a := range list {
				if h, _, err := net.SplitHostPort(a); err == nil {
					if p := net.ParseIP(h); p != nil {
						h = p.String()
					}
					ips[h] = true
				}
			}
			w.data.webMu.Lock()
			w.data.webAddrs, w.data.web, w.data.webAt, w.data.refreshing = list, ips, time.Now(), false
			w.data.webMu.Unlock()
		}()
		if !have {
			w.data.webMu.Unlock()
			<-done // the first time, there's nothing to go on
			w.data.webMu.Lock()
		}
	}
	defer w.data.webMu.Unlock()
	return append([]string(nil), w.data.webAddrs...)
}

// siteDB is a site's data, brought up to date with the SQL of the version
// being served at root.
func (w *Web) siteDB(site, root string) (*siteData, error) {
	w.data.mu.Lock()
	if w.data.open == nil {
		w.data.open, w.data.sql, w.data.pulls = map[string]*siteData{}, map[string]siteSQL{}, map[string]int64{}
	}
	d := w.data.open[site]
	s, ok := w.data.sql[root]
	w.data.mu.Unlock()
	if !ok {
		var err error
		if s, err = readSiteSQL(root); err != nil {
			return nil, err
		}
		w.data.mu.Lock()
		w.data.sql[root] = s
		w.data.mu.Unlock()
	}
	if d == nil {
		var err error
		if d, err = w.siteLog(site); err != nil {
			return nil, err
		}
	}
	return d, d.replay(s)
}

// warm compiles a version's handlers and brings the site's data up to its
// SQL, before the first request needs them.
func (w *Web) warm(site, root string) {
	w.code.warm(root)
	if _, err := os.Stat(filepath.Join(root, "api", "schema.sql")); err == nil {
		if _, err := w.siteDB(site, root); err != nil {
			w.logf("web: %s: its data: %v", site, err)
		}
	}
}

func (w *Web) self() string {
	if w.Self != "" {
		return w.Self
	}
	return "web"
}

// The handler's side: a request for this call, kept in its context.
type callKey struct{}

type call struct {
	w          *Web
	site, root string
	ctx        context.Context
}

// hostModule gives handlers yggstore.db_query and yggstore.db_record.
func hostModule(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder("yggstore").
		NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, req, n, out, room uint32) int32 {
		return answer(ctx, m, req, n, out, room, (*call).query)
	}).Export("db_query").
		NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, req, n, out, room uint32) int32 {
		return answer(ctx, m, req, n, out, room, (*call).record)
	}).Export("db_record").
		Instantiate(ctx)
	return err
}

// answer reads a call's JSON request from the handler's memory and writes
// the JSON answer there: its length, or minus the length when there's no
// room for it (the handler makes room and calls again).
func answer(ctx context.Context, m api.Module, req, n, out, room uint32, f func(*call, []byte) any) int32 {
	c, _ := ctx.Value(callKey{}).(*call)
	var resp any = map[string]string{"error": "this site has no data here"}
	if b, ok := m.Memory().Read(req, n); ok && c != nil {
		resp = f(c, append([]byte(nil), b...))
	}
	b, _ := json.Marshal(resp)
	if uint32(len(b)) > room {
		return -int32(len(b))
	}
	m.Memory().Write(out, b)
	return int32(len(b))
}

func (c *call) query(req []byte) any {
	var q struct {
		SQL  string `json:"sql"`
		Args []any  `json:"args"`
	}
	if err := json.Unmarshal(req, &q); err != nil {
		return map[string]string{"error": "bad query"}
	}
	d, err := c.w.siteDB(c.site, c.root)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	rows, err := d.query(q.SQL, q.Args, maxQueryRows)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	return rows
}

// Recorded is what a handler hears after recording an entry: whether its
// rules took it, and whether every web node answering has it (confirmed),
// or only this one so far (held).
type Recorded struct {
	ID        string `json:"id,omitempty"`
	Status    string `json:"status,omitempty"` // applied or refused
	Error     string `json:"error,omitempty"`
	Confirmed bool   `json:"confirmed"`
}

func (c *call) record(req []byte) any {
	var r struct {
		Kind   string         `json:"kind"`
		Fields map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return Recorded{Error: "bad entry"}
	}
	d, err := c.w.siteDB(c.site, c.root)
	if err != nil {
		return Recorded{Error: err.Error()}
	}
	e, err := d.record(c.w.self(), r.Kind, r.Fields, time.Now())
	if err != nil {
		return Recorded{Error: err.Error()}
	}
	if _, err := c.w.siteDB(c.site, c.root); err != nil {
		return Recorded{ID: e.ID, Error: err.Error()}
	}
	status, why := d.status(e.ID)
	out := Recorded{ID: e.ID, Status: status, Error: why}
	if status == EntryApplied {
		out.Confirmed = c.w.share(c.ctx, c.site, []Entry{e})
		if out.Confirmed {
			// Another web node may have had an earlier entry that wins.
			if _, err := c.w.siteDB(c.site, c.root); err == nil {
				out.Status, out.Error = d.status(e.ID)
				out.Confirmed = out.Status == EntryApplied
			}
		}
	}
	return out
}

// share sends entries to the other web nodes, and says whether every one
// that keeps sites' data has them now. One that doesn't (an older
// yggstore: it answers 404) doesn't count; one that is down does.
func (w *Web) share(ctx context.Context, site string, entries []Entry) bool {
	if w.Others == nil {
		return true // the only web node
	}
	body, _ := json.Marshal(map[string]any{"site": site, "entries": entries})
	ctx, cancel := context.WithTimeout(ctx, shareTimeout)
	defer cancel()
	all := true
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, addr := range w.others() {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if !w.postEntries(ctx, addr, body) {
				mu.Lock()
				all = false
				mu.Unlock()
			}
		}(addr)
	}
	wg.Wait()
	return all
}

func (w *Web) postEntries(ctx context.Context, addr string, body []byte) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+dataPath+"entries", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.dataClient().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound
}

func (w *Web) dataClient() *http.Client {
	if w.DataClient != nil {
		return w.DataClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Serve answers the other web nodes on the node's API (/v1/site-data/…):
// entries they took, and what this one has that they missed. A site's
// owner may read its own sites' entries too, for its copy (see Keeper),
// and ask which they are.
func (w *Web) Serve(rw http.ResponseWriter, r *http.Request, caller string) bool {
	path, ok := strings.CutPrefix(r.URL.Path, dataPath)
	if !ok {
		return false
	}
	if path == "owned" && r.Method == http.MethodGet {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string][]string{"sites": w.ownedBy(caller)})
		return true
	}
	if !w.isWebNode(r.Context(), caller) && !(path == "entries" && r.Method == http.MethodGet && w.ownerOf(r.URL.Query().Get("site")) == ipOf(caller)) {
		http.Error(rw, "only the group's web nodes", http.StatusForbidden)
		return true
	}
	switch {
	case path == "entries" && r.Method == http.MethodPost:
		var in struct {
			Site    string  `json:"site"`
			Entries []Entry `json:"entries"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&in); err != nil || ValidName(in.Site) != nil || len(in.Entries) > pullBatch {
			http.Error(rw, "bad entries", http.StatusBadRequest)
			return true
		}
		if err := w.take(in.Site, in.Entries); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return true
		}
		rw.WriteHeader(http.StatusNoContent)
	case path == "entries" && r.Method == http.MethodGet:
		site := r.URL.Query().Get("site")
		got, _ := strconv.ParseInt(r.URL.Query().Get("got"), 10, 64)
		if ValidName(site) != nil {
			http.Error(rw, "bad site", http.StatusBadRequest)
			return true
		}
		out := struct {
			Entries []Entry `json:"entries"`
			Got     int64   `json:"got"`
		}{Entries: []Entry{}, Got: got}
		if _, err := os.Stat(filepath.Join(w.Dir, "data", site, "log.db")); err == nil {
			d, err := w.siteLog(site)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return true
			}
			if out.Entries, out.Got, err = d.since(got, pullBatch); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return true
			}
			if out.Entries == nil {
				out.Entries = []Entry{}
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(out)
	default:
		http.NotFound(rw, r)
	}
	return true
}

// ownerOf is the IP address of the node that publishes a site, or "".
func (w *Web) ownerOf(site string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.st.Sites[site]; e != nil && e.Owner != "" {
		return ipOf(e.Owner)
	}
	return ""
}

// ownedBy are the sites with code that the node at ip publishes.
func (w *Web) ownedBy(ip string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []string{}
	for name, e := range w.st.Sites {
		if e.Code && e.Owner != "" && ipOf(e.Owner) == ipOf(ip) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// siteLog is a site's data without replaying it (another web node's
// entries are taken whether or not this one serves the site's code yet).
func (w *Web) siteLog(site string) (*siteData, error) {
	w.data.mu.Lock()
	defer w.data.mu.Unlock()
	if w.data.open == nil {
		w.data.open, w.data.sql, w.data.pulls = map[string]*siteData{}, map[string]siteSQL{}, map[string]int64{}
	}
	if d := w.data.open[site]; d != nil {
		return d, nil
	}
	d, err := openSiteData(filepath.Join(w.Dir, "data", site))
	if err != nil {
		return nil, err
	}
	w.data.open[site] = d
	return d, nil
}

// take keeps entries another web node took; the next call replays them.
func (w *Web) take(site string, entries []Entry) error {
	d, err := w.siteLog(site)
	if err != nil {
		return err
	}
	_, err = d.add(entries, time.Now())
	return err
}

// pull asks the other web nodes for the entries of every site with code
// that this one missed, now and then: after it was off, say. A site whose
// owner isn't a web node is asked too, as it keeps a copy (see Keeper):
// a new web node gets the data from it when the others are gone.
func (w *Web) pull(ctx context.Context) {
	if w.Others == nil {
		return
	}
	others := w.others()
	web := map[string]bool{}
	for _, a := range others {
		web[ipOf(a)] = true
	}
	var list []peers.Peer
	if w.Peers != nil {
		list = w.Peers()
	}
	w.mu.Lock()
	from := map[string][]string{} // site → where to ask
	for name, e := range w.st.Sites {
		if !e.Code {
			continue
		}
		from[name] = others
		if owner := ipOf(e.Owner); e.Owner != "" && !web[owner] {
			if k := KeeperAddr(list, owner); k != "" {
				from[name] = append(append([]string(nil), others...), k)
			}
		}
	}
	w.mu.Unlock()
	for site, addrs := range from {
		for _, addr := range addrs {
			w.pullFrom(ctx, addr, site)
		}
	}
}

func (w *Web) pullFrom(ctx context.Context, addr, site string) {
	key := site + " " + addr
	w.data.mu.Lock()
	got := w.data.pulls[key]
	w.data.mu.Unlock()
	got = pullEntries(ctx, w.dataClient(), addr, site, got, func(entries []Entry) error {
		return w.take(site, entries)
	}, w.logf)
	w.data.mu.Lock()
	if w.data.pulls == nil {
		w.data.pulls = map[string]int64{}
	}
	w.data.pulls[key] = got
	w.data.mu.Unlock()
}

func (w *Web) pullLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pullEvery):
		}
		w.pull(ctx)
	}
}

// isWebNode is whether ip is one of the group's web nodes.
func (w *Web) isWebNode(ctx context.Context, ip string) bool {
	if w.Others == nil {
		return false
	}
	w.others()
	w.data.webMu.Lock()
	defer w.data.webMu.Unlock()
	return w.data.web[ip]
}
