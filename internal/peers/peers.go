// Package peers holds the static allow-list of nodes and probes which are up.
package peers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Peer is one storage node. Addr is host:port, e.g. "[200:1f12::1]:7400".
// A slow peer (say, a Pi on a USB stick) is given one shard per chunk while
// faster peers can take the rest. Host names the machine a peer runs on;
// peers sharing one (containers on the same box) fail together, so placement
// treats them as one. It defaults to the peer's name. An admin peer may send
// every node a new list (see Live). Owner names the person who runs the
// node, for accounting; it defaults to the machine. A gateway peer stores
// paying customers' files (see package gateway); nodes only take its shards
// if their owner has opted in. YggListen lists where other members can
// open a Yggdrasil link to the peer's machine (see package mesh). YggPeers,
// on an admin's entry, lists public Yggdrasil peers every node links to, so
// a node reaches the group from wherever it is plugged in. Domain, on an
// admin's entry, is the group's domain; Names are the names under it the
// admin gave the node's owner (see names.go), and Code the owner's sharing
// code, which their mail is sealed for.
type Peer struct {
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	Slow    bool   `json:"slow,omitempty"`
	Host    string `json:"host,omitempty"`
	Admin   bool   `json:"admin,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Gateway bool   `json:"gateway,omitempty"`

	YggListen []string `json:"ygg_listen,omitempty"`
	YggPeers  []string `json:"ygg_peers,omitempty"`

	Domain string   `json:"domain,omitempty"`
	Names  []string `json:"names,omitempty"`
	Code   string   `json:"code,omitempty"`
}

// Operator is the person the peer's space and uploads count towards.
func (p Peer) Operator() string {
	if p.Owner != "" {
		return p.Owner
	}
	return p.Machine()
}

// Machine is the failure domain the peer belongs to.
func (p Peer) Machine() string {
	if p.Host != "" {
		return p.Host
	}
	return p.Name
}

// Machines counts the distinct machines in list.
func Machines(list []Peer) int {
	seen := map[string]bool{}
	for _, p := range list {
		seen[p.Machine()] = true
	}
	return len(seen)
}

func (p Peer) IP() string {
	host, _, err := net.SplitHostPort(p.Addr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

func Load(path string) ([]Peer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []Peer
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := Validate(list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return list, nil
}

// Validate checks that every peer has a name and a host:port address, and
// that names and addresses are not repeated.
func Validate(list []Peer) error {
	names, addrs, owners := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, p := range list {
		if p.Name == "" {
			return fmt.Errorf("peer %d has no name", i)
		}
		if _, _, err := net.SplitHostPort(p.Addr); err != nil {
			return fmt.Errorf("peer %d (%s): addr must be host:port: %w", i, p.Name, err)
		}
		if names[p.Name] || addrs[p.Addr] {
			return fmt.Errorf("peer %d (%s): name or address listed twice", i, p.Name)
		}
		names[p.Name], addrs[p.Addr] = true, true
		for _, u := range p.YggListen {
			if err := ValidListen(u); err != nil {
				return fmt.Errorf("peer %d (%s): ygg_listen %q: %w", i, p.Name, u, err)
			}
		}
		for _, u := range p.YggPeers {
			if err := ValidListen(u); err != nil {
				return fmt.Errorf("peer %d (%s): ygg_peers %q: %w", i, p.Name, u, err)
			}
		}
		if err := validNames(p, owners); err != nil {
			return fmt.Errorf("peer %d (%s): %w", i, p.Name, err)
		}
	}
	return nil
}

// PublicPeers is the group's public Yggdrasil peers: the ygg_peers of its
// admins, each once, in list order. Other members' ygg_peers are ignored.
func PublicPeers(list []Peer) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range list {
		if !p.Admin {
			continue
		}
		for _, u := range p.YggPeers {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// ValidListen checks a Yggdrasil peering address that other members will
// connect to, such as "tls://203.0.113.5:14415" or "wss://ygg.example:443".
// It must name a port and a host others can reach, not this machine's own
// loopback.
func ValidListen(uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "tls", "tcp", "quic", "ws", "wss":
	default:
		return errors.New("use tls://, quic://, tcp://, ws:// or wss://")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || port == "" {
		return errors.New("give host:port, e.g. tls://203.0.113.5:14415 (wss:// needs :443 too)")
	}
	if host == "localhost" {
		return errors.New("localhost can't be reached from other machines")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return errors.New("a loopback or unspecified address can't be reached from other machines")
	}
	return nil
}

// Hash identifies a list's exact contents, so nodes can tell whether they
// hold the same one.
func Hash(list []Peer) string {
	b, _ := json.Marshal(list)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Write saves list to path atomically, one peer per line.
func Write(path string, list []Peer) error {
	var b strings.Builder
	b.WriteString("[\n")
	for i, p := range list {
		line, err := json.Marshal(p)
		if err != nil {
			return err
		}
		b.WriteString("  ")
		b.Write(line)
		if i < len(list)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IsOverlay reports whether addr is host:port with a Yggdrasil (200::/7)
// host.
func IsOverlay(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.To4() == nil && ip[0]&0xfe == 0x02
}

// AllowedIPs is the set of node IDs (overlay IPs) that may talk to a server.
func AllowedIPs(list []Peer) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, p := range list {
		if ip := p.IP(); ip != "" {
			out[ip] = true
		}
	}
	return out
}

// Online probes every peer in parallel and returns those that answer, in list order.
func Online(ctx context.Context, list []Peer, probe func(context.Context, Peer) error) []Peer {
	ok := make([]bool, len(list))
	var wg sync.WaitGroup
	for i, p := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			ok[i] = probe(pctx, p) == nil
		}()
	}
	wg.Wait()
	var up []Peer
	for i, p := range list {
		if ok[i] {
			up = append(up, p)
		}
	}
	return up
}
