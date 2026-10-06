// Package transport hides which network a node runs on. chunk storage only
// needs a local address to bind to and a way to tell who is calling.
package transport

import (
	"fmt"
	"net"
	"strings"

	"github.com/peterretief/yggstore/internal/yggnet"
)

// Transport is the seam between storage and the network (plan Phase 1).
type Transport interface {
	Name() string
	// LocalIP is the address the shard server binds to; it doubles as node ID.
	LocalIP() (net.IP, error)
	// Identify returns the caller's node ID from the connection's remote address.
	Identify(remoteAddr string) (string, error)
}

// Yggdrasil addresses live in 200::/7 and are derived from the node's public
// key, so a source address on the overlay cannot be forged by another node.
func IsYggdrasil(ip net.IP) bool { return yggnet.IsYggdrasil(ip) }

// Yggdrasil is the system daemon's TUN device, or a node's built-in
// Yggdrasil (package yggnet); callers are told apart by address either way.
type Yggdrasil struct{}

func (Yggdrasil) Name() string { return "yggdrasil" }

// LocalIP is the address of the node on this machine: one running the
// built-in Yggdrasil if there is one, else the system daemon's.
func (Yggdrasil) LocalIP() (net.IP, error) {
	if ip, ok := yggnet.LocalNode(); ok {
		return ip, nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if ok && IsYggdrasil(ipnet.IP) {
			return ipnet.IP, nil
		}
	}
	return nil, fmt.Errorf("no Yggdrasil (200::/7) address found: start the node (yggstore serve), or the Yggdrasil daemon")
}

func (Yggdrasil) Identify(remoteAddr string) (string, error) {
	ip, err := hostIP(remoteAddr)
	if err != nil {
		return "", err
	}
	if !IsYggdrasil(ip) {
		return "", fmt.Errorf("caller %s is not on the Yggdrasil overlay", ip)
	}
	return ip.String(), nil
}

// Loopback runs several nodes on one machine for tests and demos.
type Loopback struct{}

func (Loopback) Name() string             { return "loopback" }
func (Loopback) LocalIP() (net.IP, error) { return net.IPv6loopback, nil }
func (Loopback) Identify(remoteAddr string) (string, error) {
	ip, err := hostIP(remoteAddr)
	if err != nil {
		return "", err
	}
	if !ip.IsLoopback() {
		return "", fmt.Errorf("caller %s is not loopback", ip)
	}
	return ip.String(), nil
}

func ByName(name string) (Transport, error) {
	switch strings.ToLower(name) {
	case "", "yggdrasil", "ygg", "builtin", "embedded":
		return Yggdrasil{}, nil
	case "loopback":
		return Loopback{}, nil
	}
	return nil, fmt.Errorf("unknown transport %q", name)
}

func hostIP(remoteAddr string) (net.IP, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return nil, fmt.Errorf("invalid remote address %q", remoteAddr)
	}
	return ip, nil
}
