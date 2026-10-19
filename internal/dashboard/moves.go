package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/sitemove"
)

// Moving a website to another machine (see docs/move-site.md and package
// sitemove). The dashboard plays two parts: on the group's admin it starts
// moves and gives the names, and on any machine it hands over or takes
// the sites published from this machine itself (a box's people are its
// hub's).

//go:embed sites.html
var sitesHTML []byte

// siteHolder is this machine's own sites, for moves.
type siteHolder struct{ d *Dashboard }

func (h siteHolder) Handles(person string) bool { return person == "" }

func (h siteHolder) Publisher(string) (*site.Publisher, error) { return h.d.publisher() }

func (h siteHolder) Identity(string) (*share.Identity, error) {
	if h.d.cfg.Identity == nil {
		return nil, errors.New("this machine has no sharing key")
	}
	return h.d.cfg.Identity, nil
}

func (h siteHolder) Name(string) string { return "" }

func (h siteHolder) CanTake(string) error {
	if _, err := h.d.publisher(); err != nil {
		return err
	}
	if _, err := h.d.mailBox(); err != nil {
		return errors.New("this machine collects no mail, so the site's contact form would stop: " + err.Error())
	}
	return nil
}

// movesPath keeps the moves this machine has a part in.
func (d *Dashboard) movesPath() string {
	if d.cfg.NamesPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.cfg.NamesPath), "moves.json")
}

// mover is this machine's part in moves; nil if it has no messaging.
func (d *Dashboard) mover() *sitemove.Agent {
	d.moverOnce.Do(func() {
		if d.movesPath() == "" {
			return
		}
		d.moves = sitemove.NewAgent(sitemove.Config{
			Self: d.cfg.SelfID,
			Peers: func() []peers.Peer {
				list, _ := peers.Load(d.cfg.PeersPath)
				return list
			},
			Bus: func() (sitemove.Bus, error) {
				l, err := d.msgClient()
				return l, err
			},
			Client:    d.cfg.Client,
			Online:    func(context.Context) ([]peers.Peer, error) { return d.onlinePeers() },
			StatePath: d.movesPath(),
			Holder:    siteHolder{d},
			Give:      d.giveSite,
			Logf:      d.logf,
		})
	})
	return d.moves
}

func (d *Dashboard) runMoves(ctx context.Context) {
	if a := d.mover(); a != nil {
		a.Run(ctx)
	}
}

// giveSite gives a moved site's name to its new holder and sends the list
// out (admin dashboards).
func (d *Dashboard) giveSite(m sitemove.Move, r sitemove.Ready) error {
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return err
	}
	out, err := sitemove.Give(list, m, r)
	if err != nil {
		return err
	}
	if err := peers.Write(d.cfg.PeersPath, out); err != nil {
		return err
	}
	d.poke()
	return nil
}

// groupSite is a site the group's web nodes serve, for the admin's page.
type groupSite struct {
	Site    string   `json:"site"`
	Owner   string   `json:"owner"`            // the node publishing it
	Node    string   `json:"node"`             // its name
	Person  string   `json:"person,omitempty"` // someone on that node's box
	Given   bool     `json:"given"`            // the list gives its name to the owner
	Contact bool     `json:"contact"`
	Serving string   `json:"serving,omitempty"`
	Served  []string `json:"served"` // web nodes serving it
	Moving  bool     `json:"moving"`
}

type moveDest struct {
	Node string `json:"node"` // ID
	Name string `json:"name"`
	Self bool   `json:"self,omitempty"`
}

func (d *Dashboard) handleMoves(w http.ResponseWriter, r *http.Request) {
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if own, _ := selfEntry(list, d.cfg.SelfID); !own.Admin {
		http.Error(w, "only the group's admin moves websites", http.StatusForbidden)
		return
	}
	a := d.mover()
	out := struct {
		Sites []groupSite        `json:"sites"`
		Moves []sitemove.Pending `json:"moves"`
		Dests []moveDest         `json:"dests"`
		Names map[string]string  `json:"names"` // node ID → name
		At    int64              `json:"at"`
	}{Sites: d.groupSites(list), Moves: []sitemove.Pending{}, Dests: []moveDest{}, Names: map[string]string{}, At: time.Now().Unix()}
	if a != nil {
		for _, m := range a.Moves() {
			if m.Role == sitemove.RoleAdmin {
				out.Moves = append(out.Moves, m)
				for i := range out.Sites {
					if out.Sites[i].Site == m.Site && m.Status != sitemove.StatusDone && m.Status != sitemove.StatusRefused {
						out.Sites[i].Moving = true
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, p := range list {
		out.Names[p.IP()] = p.Name
		if seen[p.IP()] || p.Gateway || p.Leaving {
			continue
		}
		seen[p.IP()] = true
		out.Dests = append(out.Dests, moveDest{Node: p.IP(), Name: p.Name, Self: p.IP() == d.cfg.SelfID})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// groupSites is every site the web nodes report, with who publishes it.
func (d *Dashboard) groupSites(list []peers.Peer) []groupSite {
	type agg struct {
		groupSite
		owners map[string]int
	}
	by := map[string]*agg{}
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
			g := by[s.Site]
			if g == nil {
				g = &agg{groupSite: groupSite{Site: s.Site, Served: []string{}}, owners: map[string]int{}}
				by[s.Site] = g
			}
			if s.Serving != "" {
				g.Served = append(g.Served, p.Name)
				g.Serving = s.Serving
			}
			if s.Owner != "" {
				g.owners[s.Owner]++
			}
			g.Contact = g.Contact || s.Contact
		}
	}
	d.mu.Unlock()
	names := map[string]string{}
	for _, p := range list {
		if _, ok := names[p.IP()]; !ok {
			names[p.IP()] = p.Name
		}
	}
	out := []groupSite{}
	for _, g := range by {
		best := 0
		for o, n := range g.owners {
			if n > best || (n == best && o < g.Owner) {
				g.Owner, best = o, n
			}
		}
		g.Node = names[g.Owner]
		if holder, pn, ok := peers.NameHolder(list, g.Site); ok {
			g.Given = holder.IP() == g.Owner
			if g.Given {
				g.Person = pn.Person
			}
		}
		sort.Strings(g.Served)
		out = append(out, g.groupSite)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site < out[j].Site })
	return out
}

func (d *Dashboard) handleMoveStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Site   string `json:"site"`
		To     string `json:"to"`
		Person string `json:"person"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a := d.mover()
	if a == nil {
		http.Error(w, "this dashboard has no messaging", http.StatusServiceUnavailable)
		return
	}
	var from *groupSite
	for _, g := range d.groupSites(list) {
		if g.Site == req.Site {
			from = &g
		}
	}
	if from == nil || from.Owner == "" {
		http.Error(w, "no web node serves "+req.Site+" yet", http.StatusBadRequest)
		return
	}
	m := sitemove.Move{Site: req.Site, From: from.Owner, FromPerson: from.Person, To: req.To, ToPerson: strings.TrimSpace(req.Person)}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := a.Start(ctx, m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Dashboard) handleSitesPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(sitesHTML)
}

// movedAway is where a site published from here moved to, for the
// websites folder's note.
func (d *Dashboard) movedAway(name string) (string, time.Time, bool) {
	if d.cfg.SitesDir == "" {
		return "", time.Time{}, false
	}
	if _, err := os.Stat(d.cfg.SitesDir); err != nil {
		return "", time.Time{}, false
	}
	return sitemove.Moved(d.cfg.SitesDir, name)
}
