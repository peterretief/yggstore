package peers

import (
	"context"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Yggdrasil names: each machine in the group gets a DNS name for its
// Yggdrasil address under the group's domain, such as
// localbox-003.ygg.example.org, an AAAA record only devices on Yggdrasil can
// use. Links with it survive email and chat, which mangle a bare address
// in [brackets].

// YggZone is the label the machines' names go under.
const YggZone = "ygg"

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// YggNames are the machines' names, by Yggdrasil address: each takes the
// name of the first node at its address, if that makes a DNS label. None
// if the group has no domain.
func YggNames(list []Peer) map[string]string {
	out := map[string]string{}
	domain := GroupDomain(list)
	if domain == "" {
		return out
	}
	for _, p := range list {
		ip := p.IP()
		if _, seen := out[ip]; seen || !isYgg(ip) {
			continue
		}
		label := strings.ToLower(p.Name)
		if !dnsLabel.MatchString(label) {
			out[ip] = "" // first at the address, but no name
			continue
		}
		out[ip] = label + "." + YggZone + "." + domain
	}
	for ip, n := range out {
		if n == "" {
			delete(out, ip)
		}
	}
	return out
}

// isYgg reports whether ip is a Yggdrasil address (200::/7).
func isYgg(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && p.To4() == nil && p[0]&0xfe == 0x02
}

// YggName is the name of the machine at ip, or "".
func YggName(list []Peer, ip string) string {
	if parsed := net.ParseIP(ip); parsed != nil {
		ip = parsed.String()
	}
	return YggNames(list)[ip]
}

// NameCheck says whether names point at the addresses they should, asking
// DNS at most every few minutes for each.
type NameCheck struct {
	mu   sync.Mutex
	seen map[string]nameSeen
	// Lookup is how names are looked up; net.DefaultResolver's if nil.
	Lookup func(ctx context.Context, host string) ([]net.IP, error)
}

type nameSeen struct {
	ok bool
	at time.Time
}

// Points reports whether name resolves to ip. An answer is kept 10 minutes
// when it does, 1 minute when it doesn't, so a new record is found soon.
func (c *NameCheck) Points(name, ip string) bool {
	if name == "" {
		return false
	}
	key := name + " " + ip
	c.mu.Lock()
	if s, ok := c.seen[key]; ok && (time.Since(s.at) < time.Minute || (s.ok && time.Since(s.at) < 10*time.Minute)) {
		c.mu.Unlock()
		return s.ok
	}
	c.mu.Unlock()
	lookup := c.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip6", host)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, _ := lookup(ctx, name)
	ok := false
	for _, got := range ips {
		if got.Equal(net.ParseIP(ip)) {
			ok = true
		}
	}
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]nameSeen{}
	}
	c.seen[key] = nameSeen{ok: ok, at: time.Now()}
	c.mu.Unlock()
	return ok
}

// HostURL is http://host:port, with host in brackets if it's an IPv6
// address.
func HostURL(host, port string) string {
	return "http://" + net.JoinHostPort(host, port)
}
