package site

import (
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
)

// Keeper is a site owner's copy of its sites' data. The node that
// publishes a site with code (a box, for its people's sites) asks the web
// nodes now and then which of their sites are its own, and pulls those
// sites' entries into its own log, so the data outlives the web nodes.
// It gives the entries back to any web node that asks: a new one, or one
// that lost its disk, catches up from it as from another web node. It
// takes no entries of its own and replays nothing; the web nodes do that.
//
// Only a node that isn't a web node keeps a copy (a web node has the
// entries already), and of several nodes on one machine, only the one at
// the lowest address (KeeperAddr).
type Keeper struct {
	Dir      string                             // its copies: DIR/SITE/log.db
	WebNodes func(ctx context.Context) []string // their node API addresses
	Client   *http.Client
	Log      func(format string, args ...any)

	mu     sync.Mutex
	open   map[string]*siteData
	pulls  map[string]int64 // "site addr" → the web node's got time reached
	web    map[string]bool  // the web nodes' IP addresses, for Serve
	addrs  []string         // and their node API addresses,
	listed time.Time        // found then: finding them asks every node
}

// KeeperAddr is the node API address of the node that keeps a site's data
// for its owner, the node at IP: of the group's nodes there, the one at the
// lowest address.
func KeeperAddr(list []peers.Peer, ip string) string {
	var addrs []string
	for _, p := range list {
		if ipOf(p.Addr) == ipOf(ip) {
			addrs = append(addrs, p.Addr)
		}
	}
	sort.Strings(addrs)
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0]
}

// SameAddr is whether a and b are the same node API address, however the
// IP address is spelled.
func SameAddr(a, b string) bool {
	_, pa, _ := net.SplitHostPort(a)
	_, pb, _ := net.SplitHostPort(b)
	return ipOf(a) == ipOf(b) && pa == pb
}

// ipOf is the IP address in addr (host:port, or an address alone), in one
// spelling.
func ipOf(addr string) string {
	h := addr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		h = host
	}
	if p := net.ParseIP(strings.Trim(h, "[]")); p != nil {
		return p.String()
	}
	return h
}

func (k *Keeper) logf(format string, args ...any) {
	if k.Log != nil {
		k.Log(format, args...)
	}
}

// Run keeps the copies up to date until ctx ends.
func (k *Keeper) Run(ctx context.Context) {
	for {
		k.Step(ctx)
		select {
		case <-ctx.Done():
			k.close()
			return
		case <-time.After(pullEvery):
		}
	}
}

// Step asks each web node for this node's sites and their new entries.
func (k *Keeper) Step(ctx context.Context) {
	k.mu.Lock()
	addrs, fresh := k.addrs, time.Since(k.listed) < webNodesFresh
	k.mu.Unlock()
	if !fresh {
		addrs = k.WebNodes(ctx)
		web := map[string]bool{}
		for _, a := range addrs {
			web[ipOf(a)] = true
		}
		k.mu.Lock()
		k.web, k.addrs, k.listed = web, addrs, time.Now()
		k.mu.Unlock()
	}
	k.mu.Lock()
	if k.pulls == nil {
		k.pulls = map[string]int64{}
	}
	k.mu.Unlock()
	for _, addr := range addrs {
		for _, site := range k.owned(ctx, addr) {
			key := site + " " + addr
			k.mu.Lock()
			got := k.pulls[key]
			k.mu.Unlock()
			got = pullEntries(ctx, k.client(), addr, site, got, func(entries []Entry) error {
				d, err := k.log(site)
				if err != nil {
					return err
				}
				_, err = d.add(entries, time.Now())
				return err
			}, k.logf)
			k.mu.Lock()
			k.pulls[key] = got
			k.mu.Unlock()
		}
	}
}

// owned are the sites with code a web node serves for this node.
func (k *Keeper) owned(ctx context.Context, addr string) []string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+dataPath+"owned", nil)
	resp, err := k.client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Sites []string `json:"sites"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil {
		return nil
	}
	var sites []string
	for _, s := range out.Sites {
		if ValidName(s) == nil {
			sites = append(sites, s)
		}
	}
	return sites
}

func (k *Keeper) client() *http.Client {
	if k.Client != nil {
		return k.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// log is a site's copy, opened once.
func (k *Keeper) log(site string) (*siteData, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.open == nil {
		k.open = map[string]*siteData{}
	}
	if k.pulls == nil {
		k.pulls = map[string]int64{}
	}
	if d := k.open[site]; d != nil {
		return d, nil
	}
	d, err := openSiteData(filepath.Join(k.Dir, site))
	if err != nil {
		return nil, err
	}
	k.open[site] = d
	return d, nil
}

func (k *Keeper) close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, d := range k.open {
		d.close()
	}
	k.open = nil
}

// Sites are the sites this node keeps a copy of.
func (k *Keeper) Sites() []string {
	entries, _ := os.ReadDir(k.Dir)
	var out []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(k.Dir, e.Name(), "log.db")); e.IsDir() && err == nil && ValidName(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// Serve gives the web nodes the entries this node keeps
// (GET /v1/site-data/entries?site=SITE&got=T), as a web node does.
func (k *Keeper) Serve(rw http.ResponseWriter, r *http.Request, caller string) bool {
	path, ok := strings.CutPrefix(r.URL.Path, dataPath)
	if !ok {
		return false
	}
	k.mu.Lock()
	web := k.web[ipOf(caller)]
	k.mu.Unlock()
	if !web {
		http.Error(rw, "only the group's web nodes", http.StatusForbidden)
		return true
	}
	site := r.URL.Query().Get("site")
	if path != "entries" || r.Method != http.MethodGet {
		http.NotFound(rw, r)
		return true
	}
	if ValidName(site) != nil {
		http.Error(rw, "bad site", http.StatusBadRequest)
		return true
	}
	got, _ := strconv.ParseInt(r.URL.Query().Get("got"), 10, 64)
	out := struct {
		Entries []Entry `json:"entries"`
		Got     int64   `json:"got"`
	}{Entries: []Entry{}, Got: got}
	if _, err := os.Stat(filepath.Join(k.Dir, site, "log.db")); err == nil {
		d, err := k.log(site)
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
	return true
}

// pullEntries asks the node at addr for a site's entries after got, in
// batches, hands them to take, and gives the got time reached.
func pullEntries(ctx context.Context, c *http.Client, addr, site string, got int64, take func([]Entry) error, logf func(string, ...any)) int64 {
	for range 100 {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, "http://"+addr+dataPath+"entries?site="+site+"&got="+strconv.FormatInt(got, 10), nil)
		resp, err := c.Do(req)
		if err != nil {
			cancel()
			return got
		}
		var in struct {
			Entries []Entry `json:"entries"`
			Got     int64   `json:"got"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&in)
		resp.Body.Close()
		cancel()
		if err != nil || resp.StatusCode != http.StatusOK {
			return got
		}
		if len(in.Entries) > 0 {
			if err := take(in.Entries); err != nil {
				logf("site data: %s: entries from %s: %v", site, addr, err)
				return got
			}
		}
		got = in.Got
		if len(in.Entries) < pullBatch {
			return got
		}
	}
	return got
}
