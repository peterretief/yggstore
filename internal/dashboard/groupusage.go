package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/peers"
)

// The usage page (admin dashboards only): what each machine stores
// against what it gives the group, what each person on each box uses
// against their limit, and who has which website and email address.
// Machines come from the dashboard's own poll of every node; people only
// their box knows, so each box sends its admins a summary every hour.

//go:embed groupusage.html
var groupUsageHTML []byte

const (
	typeBoxUsage = "box-usage" // box → admins: {"people": [...]}
	usageEvery   = time.Hour
	nearFull     = 0.9
)

type personUsage struct {
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	Files     int    `json:"files"`
	Quota     int64  `json:"quota,omitempty"`
	Suspended bool   `json:"suspended,omitempty"`
}

type boxUsage struct {
	Box    string        `json:"box"` // its node's name
	At     int64         `json:"at"`  // unix ms, when it was sent
	People []personUsage `json:"people"`
}

// usageReports keeps the latest summary from each box, by node ID.
type usageReports struct {
	mu   sync.Mutex
	path string
	last time.Time // when this box last sent its own
	by   map[string]boxUsage
}

func (d *Dashboard) usageReports() *usageReports {
	d.usageOnce.Do(func() {
		u := &usageReports{by: map[string]boxUsage{}}
		if d.cfg.NamesPath != "" {
			u.path = filepath.Join(filepath.Dir(d.cfg.NamesPath), "usage.json")
			if b, err := os.ReadFile(u.path); err == nil {
				json.Unmarshal(b, &u.by)
			}
		}
		d.usageR = u
	})
	return d.usageR
}

func (u *usageReports) put(node string, b boxUsage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if old, ok := u.by[node]; ok && old.At > b.At {
		return
	}
	u.by[node] = b
	if u.path != "" {
		if data, err := json.Marshal(u.by); err == nil {
			os.WriteFile(u.path, data, 0o600)
		}
	}
}

// boxSummary is what this box's people use now.
func (d *Dashboard) boxSummary(list []peers.Peer) (boxUsage, bool) {
	if !d.onBox() {
		return boxUsage{}, false
	}
	st, err := hub.Load(d.cfg.HubState)
	if err != nil {
		return boxUsage{}, false
	}
	own, _ := selfEntry(list, d.cfg.SelfID)
	b := boxUsage{Box: own.Machine(), At: time.Now().UnixMilli(), People: []personUsage{}}
	for _, p := range st.People {
		if p.Removed {
			continue
		}
		bytes, files := d.usage.get(p)
		b.People = append(b.People, personUsage{Name: p.Name, Bytes: bytes, Files: files, Quota: p.Quota, Suspended: p.Suspended})
	}
	return b, true
}

// sendUsage sends this box's summary to the group's admins, once an hour.
// An admin's own box keeps it straight away.
func (d *Dashboard) sendUsage(ctx context.Context, list []peers.Peer) {
	u := d.usageReports()
	u.mu.Lock()
	due := time.Since(u.last) >= usageEvery
	u.mu.Unlock()
	if !due {
		return
	}
	b, ok := d.boxSummary(list)
	if !ok {
		return
	}
	u.mu.Lock()
	u.last = time.Now()
	u.mu.Unlock()
	body, _ := json.Marshal(b)
	lc, lerr := d.msgClient()
	for _, p := range list {
		switch {
		case !p.Admin:
		case p.IP() == d.cfg.SelfID:
			u.put(d.cfg.SelfID, b)
		case lerr == nil:
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			lc.Send(sctx, p.IP(), typeBoxUsage, string(body))
			cancel()
		}
	}
}

// takeUsage keeps a box's summary, on an admin dashboard.
func (d *Dashboard) takeUsage(list []peers.Peer, from, fromName, body string) {
	if !isAdmin(list, d.cfg.SelfID) {
		return
	}
	var b boxUsage
	if json.Unmarshal([]byte(body), &b) != nil || len(b.People) > 1000 {
		return
	}
	if b.Box == "" {
		b.Box = fromName
	}
	d.usageReports().put(from, b)
}

type usageState struct {
	Machines []machineUsage `json:"machines"`
	People   []peopleUsage  `json:"people"`
	Names    []nameUsage    `json:"names"`
	Domain   string         `json:"domain"`
}

type machineUsage struct {
	Machine string `json:"machine"`
	Up      bool   `json:"up"`
	Used    int64  `json:"used"`
	Quota   int64  `json:"quota"`
	Status  string `json:"status"` // ok, near, over, down
}

type peopleUsage struct {
	Box string `json:"box"`
	personUsage
	Status string `json:"status"` // ok, near, over, shut-out
	At     int64  `json:"at"`     // when the box last said
}

type nameUsage struct {
	Website string `json:"website"`
	Email   string `json:"email"`
	Who     string `json:"who"` // a person on a box, or a node
	Given   int64  `json:"given,omitempty"`
}

func fill(used, quota int64) string {
	switch {
	case quota <= 0:
		return "ok"
	case used > quota:
		return "over"
	case float64(used) >= nearFull*float64(quota):
		return "near"
	}
	return "ok"
}

func (d *Dashboard) handleUsagePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(groupUsageHTML)
}

func (d *Dashboard) handleUsage(w http.ResponseWriter, r *http.Request) {
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil || !isAdmin(list, d.cfg.SelfID) {
		http.Error(w, "only an admin dashboard shows the group's usage", http.StatusForbidden)
		return
	}
	out := usageState{Machines: []machineUsage{}, People: []peopleUsage{}, Names: []nameUsage{}, Domain: peers.GroupDomain(list)}

	// Machines: a machine with a node per disk adds them up.
	d.mu.Lock()
	ps := append([]PeerState{}, d.state.Peers...)
	d.mu.Unlock()
	machineOf := map[string]string{}
	for _, p := range list {
		machineOf[p.Addr] = p.Machine()
	}
	byMachine := map[string]*machineUsage{}
	var order []string
	for _, p := range ps {
		if p.Test {
			continue
		}
		name := machineOf[p.Addr]
		if name == "" {
			name = p.Name
		}
		m := byMachine[name]
		if m == nil {
			m = &machineUsage{Machine: name}
			byMachine[name] = m
			order = append(order, name)
		}
		if p.Up && p.Info != nil {
			m.Up = true
			m.Used += p.Info.UsedBytes
			m.Quota += p.Info.QuotaBytes
		}
	}
	for _, name := range order {
		m := byMachine[name]
		m.Status = fill(m.Used, m.Quota)
		if !m.Up {
			m.Status = "down"
		}
		out.Machines = append(out.Machines, *m)
	}

	// People, from each box's last summary.
	u := d.usageReports()
	u.mu.Lock()
	for _, b := range u.by {
		for _, p := range b.People {
			pu := peopleUsage{Box: b.Box, personUsage: p, Status: fill(p.Bytes, p.Quota), At: b.At}
			if p.Suspended {
				pu.Status = "shut-out"
			}
			out.People = append(out.People, pu)
		}
	}
	u.mu.Unlock()
	rank := map[string]int{"over": 0, "near": 1, "shut-out": 2, "ok": 3}
	sort.Slice(out.People, func(i, j int) bool {
		a, b := out.People[i], out.People[j]
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] < rank[b.Status]
		}
		return a.Bytes > b.Bytes
	})

	// Websites and email addresses, and when they were given.
	given := map[string]int64{}
	d.namesMu.Lock()
	for _, c := range d.loadNames().Claims {
		if c.Status == "approved" {
			given[c.Name] = c.Time
		}
	}
	d.namesMu.Unlock()
	add := func(name, who string) {
		if out.Domain == "" || !strings.HasSuffix(name, "."+out.Domain) {
			return
		}
		out.Names = append(out.Names, nameUsage{Website: "https://" + name, Email: peers.MailAddress(name), Who: who, Given: given[name]})
	}
	for _, p := range list {
		for _, n := range p.Names {
			add(n, p.Machine())
		}
		for _, pn := range p.PeopleNames {
			add(pn.Name, pn.Person+" on "+p.Machine())
		}
	}
	sort.Slice(out.Names, func(i, j int) bool { return out.Names[i].Given > out.Names[j].Given })
	writeJSONResp(w, out)
}
