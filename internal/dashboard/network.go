package dashboard

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// The network page (/network): every node in the group with its addresses
// (Yggdrasil, and the LAN ones its machine reports), what each machine
// answers on, the links to open, and the group's public names and whether
// they answer. It is worked out afresh as it's looked at: the member list,
// the nodes' info from the last poll, and probes kept for a minute or so.

//go:embed network.html
var networkHTML []byte

// netPorts are the ports a machine is checked for (see the page's legend).
var netPorts = []int{7400, 7401, 7480, 7481, 7490, 8480, 22000, 22}

const (
	portsFresh = time.Minute
	sitesFresh = 3 * time.Minute
)

type netNode struct {
	Name     string   `json:"name"`
	Machine  string   `json:"machine"`
	IP       string   `json:"ip"`
	Port     string   `json:"port"`
	Owner    string   `json:"owner,omitempty"`
	Roles    []string `json:"roles"`
	Up       bool     `json:"up"`
	RTTms    float64  `json:"rtt_ms,omitempty"`
	Error    string   `json:"error,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	Version  string   `json:"version,omitempty"`
	LAN      []string `json:"lan"`
	Used     int64    `json:"used"`
	Quota    int64    `json:"quota"`
	Shards   int      `json:"shards"`
	Names    []string `json:"names"`
	// Dashboard is the node's dashboard, if its machine answers there.
	Dashboard string `json:"dashboard,omitempty"`
	Test      bool   `json:"test,omitempty"`
	Self      bool   `json:"self,omitempty"`
}

type netMachine struct {
	IP    string `json:"ip"`
	Name  string `json:"name"`
	Open  []int  `json:"open"`
	Hub   string `json:"hub,omitempty"` // where its people open their files
	Known bool   `json:"known"`         // probed yet
	// YggName is the machine's DNS name for its Yggdrasil address, and
	// InDNS whether it points there yet (see peers.YggNames).
	YggName string `json:"ygg_name,omitempty"`
	InDNS   bool   `json:"in_dns,omitempty"`
}

type netName struct {
	Name    string `json:"name"`
	Mail    string `json:"mail"`
	Node    string `json:"node"`
	Person  string `json:"person,omitempty"`
	Status  int    `json:"status"` // HTTP status from https://name/, 0 if it didn't answer
	Problem string `json:"problem,omitempty"`
	Checked bool   `json:"checked"`
}

type netState struct {
	Domain   string       `json:"domain,omitempty"`
	MX       []string     `json:"mx"`
	Nodes    []netNode    `json:"nodes"`
	Machines []netMachine `json:"machines"`
	Names    []netName    `json:"names"`
	At       int64        `json:"at"`
}

// netProbe keeps what the probes found, refreshed in the background so the
// page never waits on a machine that's off.
type netProbe struct {
	mu      sync.Mutex
	ports   map[string][]int // per IP
	portsAt time.Time
	sites   map[string]netName // per name: Status and Problem
	sitesAt time.Time
	inDNS   map[string]bool // per IP: its name points at it
	mx      []string
	busy    bool
}

func (d *Dashboard) handleNetworkPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(networkHTML)
}

func (d *Dashboard) handleNetwork(w http.ResponseWriter, r *http.Request) {
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.mu.Lock()
	polled := append([]PeerState{}, d.state.Peers...)
	d.mu.Unlock()
	byAddr := map[string]PeerState{}
	for _, p := range polled {
		byAddr[p.Addr] = p
	}
	inMain, test := map[string]bool{}, map[string]bool{}
	for _, p := range list {
		inMain[p.Addr] = true
	}
	if d.cfg.TestPeersPath != "" { // the test cluster's nodes, other than this one
		tlist, _ := peers.Load(d.cfg.TestPeersPath)
		for _, p := range tlist {
			if p.IP() != d.cfg.SelfID {
				test[p.Addr] = true
			}
		}
	}
	all := append([]peers.Peer{}, list...)
	for _, p := range polled { // test nodes the dashboard watches that aren't members
		if p.Test && !inMain[p.Addr] {
			all = append(all, peers.Peer{Name: p.Name, Addr: p.Addr})
		}
	}

	yggNames := peers.YggNames(list)
	out := netState{Domain: peers.GroupDomain(list), MX: []string{}, Nodes: []netNode{}, Machines: []netMachine{}, Names: []netName{}, At: time.Now().Unix()}
	var ips []string
	machineOf := map[string]string{}
	for _, p := range all {
		ip, port := p.IP(), ""
		if _, pt, err := net.SplitHostPort(p.Addr); err == nil {
			port = pt
		}
		n := netNode{Name: p.Name, Machine: p.Machine(), IP: ip, Port: port, Owner: p.Owner, Roles: roles(p), LAN: []string{},
			Names: append([]string{}, p.Names...), Test: !inMain[p.Addr] || test[p.Addr], Self: ip == d.cfg.SelfID && port == "7400"}
		for _, pn := range p.PeopleNames {
			n.Names = append(n.Names, pn.Name)
		}
		if ps, ok := byAddr[p.Addr]; ok {
			n.Up, n.RTTms, n.Error, n.Test = ps.Up, ps.RTTms, ps.Error, n.Test || ps.Test
			if i := ps.Info; i != nil {
				n.Hostname, n.Version, n.Used, n.Quota, n.Shards = i.Hostname, i.Version, i.UsedBytes, i.QuotaBytes, i.ShardCount
				if i.LAN != nil {
					n.LAN = i.LAN
				}
				if len(i.Web) > 0 && string(i.Web) != "null" {
					n.Roles = append(n.Roles, "web node")
				}
				if i.MailOut {
					n.Roles = append(n.Roles, "mail relay")
				}
			}
		}
		out.Nodes = append(out.Nodes, n)
		if _, seen := machineOf[ip]; !seen && ip != "" {
			machineOf[ip] = p.Name // the first node at the address: test containers have their own
			ips = append(ips, ip)
		}
		for _, name := range p.Names {
			out.Names = append(out.Names, netName{Name: name, Mail: peers.MailAddress(name), Node: p.Name})
		}
		for _, pn := range p.PeopleNames {
			out.Names = append(out.Names, netName{Name: pn.Name, Mail: peers.MailAddress(pn.Name), Node: p.Name, Person: pn.Person})
		}
	}
	if out.Domain != "" {
		out.Names = append(out.Names, netName{Name: out.Domain, Node: "the group"})
	}

	ports, sites, mx, inDNS := d.netProbes(ips, out.Names, out.Domain, yggNames)
	out.MX = mx
	host := func(ip string) string { // the name to link to, once DNS has it
		if inDNS[ip] {
			return yggNames[ip]
		}
		return ip
	}
	for _, ip := range ips {
		open, known := ports[ip]
		m := netMachine{IP: ip, Name: machineOf[ip], Open: open, Known: known, YggName: yggNames[ip], InDNS: inDNS[ip]}
		if m.Open == nil {
			m.Open = []int{}
		}
		if hasPort(open, 7490) {
			m.Hub = peers.HostURL(host(ip), "7490") + "/files"
		}
		out.Machines = append(out.Machines, m)
	}
	for i, n := range out.Nodes {
		pt, err := strconv.Atoi(n.Port)
		if err != nil || pt < 7400 || pt > 7409 {
			continue
		}
		if dp := 7480 + pt - 7400; hasPort(ports[n.IP], dp) {
			out.Nodes[i].Dashboard = peers.HostURL(host(n.IP), strconv.Itoa(dp))
		}
	}
	for i, nm := range out.Names {
		if s, ok := sites[nm.Name]; ok {
			out.Names[i].Status, out.Names[i].Problem, out.Names[i].Checked = s.Status, s.Problem, true
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResp(w, out)
}

func roles(p peers.Peer) []string {
	r := []string{}
	if p.Admin {
		r = append(r, "admin")
	}
	if p.Gateway {
		r = append(r, "S3 gateway")
	}
	if p.Host != "" {
		r = append(r, "on "+p.Host)
	}
	if p.Slow {
		r = append(r, "slow")
	}
	if p.Leaving {
		r = append(r, "leaving")
	}
	return r
}

func hasPort(open []int, p int) bool {
	for _, o := range open {
		if o == p {
			return true
		}
	}
	return false
}

// netProbes gives what the last probes found, and starts new ones in the
// background once those are old.
func (d *Dashboard) netProbes(ips []string, names []netName, domain string, yggNames map[string]string) (map[string][]int, map[string]netName, []string, map[string]bool) {
	pr := &d.netProbe
	pr.mu.Lock()
	defer pr.mu.Unlock()
	stalePorts, staleSites := time.Since(pr.portsAt) > portsFresh, time.Since(pr.sitesAt) > sitesFresh
	if (stalePorts || staleSites) && !pr.busy {
		pr.busy = true
		go pr.refresh(ips, names, domain, yggNames, stalePorts, staleSites)
	}
	ports, sites := map[string][]int{}, map[string]netName{}
	for k, v := range pr.ports {
		ports[k] = v
	}
	for k, v := range pr.sites {
		sites[k] = v
	}
	mx := append([]string{}, pr.mx...)
	inDNS := map[string]bool{}
	for k, v := range pr.inDNS {
		inDNS[k] = v
	}
	return ports, sites, mx, inDNS
}

func (pr *netProbe) refresh(ips []string, names []netName, domain string, yggNames map[string]string, doPorts, doSites bool) {
	defer func() {
		pr.mu.Lock()
		pr.busy = false
		pr.mu.Unlock()
	}()
	var wg sync.WaitGroup
	if doPorts {
		var mu sync.Mutex
		ports := map[string][]int{}
		for _, ip := range ips {
			for _, p := range netPorts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(p)), 3*time.Second)
					mu.Lock()
					defer mu.Unlock()
					if _, ok := ports[ip]; !ok {
						ports[ip] = []int{}
					}
					if err == nil {
						c.Close()
						ports[ip] = append(ports[ip], p)
					}
				}()
			}
		}
		inDNS := map[string]bool{}
		for ip, name := range yggNames {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				got, _ := net.DefaultResolver.LookupIP(ctx, "ip6", name)
				mu.Lock()
				defer mu.Unlock()
				for _, g := range got {
					if g.Equal(net.ParseIP(ip)) {
						inDNS[ip] = true
					}
				}
			}()
		}
		wg.Wait()
		for _, v := range ports {
			sort.Ints(v)
		}
		pr.mu.Lock()
		pr.ports, pr.portsAt, pr.inDNS = ports, time.Now(), inDNS
		pr.mu.Unlock()
	}
	if doSites {
		var mu sync.Mutex
		sites := map[string]netName{}
		hc := &http.Client{Timeout: 10 * time.Second}
		for _, nm := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := netName{}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+nm.Name+"/", nil)
				if resp, err := hc.Do(req); err != nil {
					s.Problem = shortErr(err)
				} else {
					io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
					resp.Body.Close()
					s.Status = resp.StatusCode
				}
				mu.Lock()
				sites[nm.Name] = s
				mu.Unlock()
			}()
		}
		var mx []string
		if domain != "" {
			if recs, err := net.LookupMX(domain); err == nil {
				for _, r := range recs {
					mx = append(mx, strings.TrimSuffix(r.Host, "."))
				}
			}
		}
		wg.Wait()
		pr.mu.Lock()
		pr.sites, pr.sitesAt, pr.mx = sites, time.Now(), mx
		pr.mu.Unlock()
	}
}

// addYggNames puts the machines' names in DNS (see peers.YggNames), if this
// admin dashboard has a Cloudflare API token; without one, the network page
// lists the records to add by hand.
func (d *Dashboard) addYggNames(ctx context.Context, list []peers.Peer) {
	token, err := os.ReadFile(d.cfg.CloudflareToken)
	if d.cfg.CloudflareToken == "" || err != nil || len(strings.TrimSpace(string(token))) == 0 {
		return
	}
	cf := &site.Cloudflare{APIToken: strings.TrimSpace(string(token))}
	test := map[string]bool{} // the test cluster's machines stay out of DNS
	if d.cfg.TestPeersPath != "" {
		tlist, _ := peers.Load(d.cfg.TestPeersPath)
		for _, p := range tlist {
			test[p.IP()] = true
		}
	}
	for ip, name := range peers.YggNames(list) {
		if test[ip] || d.names.Points(name, ip) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		added, err := cf.AAAA(cctx, name, ip)
		cancel()
		switch {
		case err != nil:
			d.event("warn", fmt.Sprintf("names: %s: %v", name, err))
		case added:
			d.event("ok", fmt.Sprintf("names: %s now points at %s", name, ip))
		}
	}
}
