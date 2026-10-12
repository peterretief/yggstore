package hub

import (
	"net"
	"strings"
)

// OnLAN is whether an address (host or host:port) is on the network this
// machine is on: loopback, private or link-local, or inside a network one
// of its LAN interfaces is on. The last covers LANs with public addresses, such
// as most home IPv6, and follows the machine to whatever network it is
// plugged into. Yggdrasil's interface doesn't count: its network is all of
// Yggdrasil.
func OnLAN(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host, _, _ = strings.Cut(strings.Trim(host, "[]"), "%") // fe80::1%eth0
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	if yggdrasil(ip) {
		return false
	}
	for _, n := range localNets() {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Virtual is whether an interface isn't the local network but a
// container's, a bridge, a VPN or Yggdrasil's own.
func Virtual(name string) bool {
	for _, v := range []string{"docker", "br-", "veth", "virbr", "tun", "tap", "tailscale", "wg", "zt", "lo"} {
		if strings.HasPrefix(name, v) {
			return true
		}
	}
	return false
}

// localNets are the networks this machine's LAN interfaces are on (see
// Virtual). A variable, for tests.
var localNets = func() []*net.IPNet {
	var out []*net.IPNet
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || Virtual(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !yggdrasil(n.IP) {
				if ones, _ := n.Mask.Size(); ones > 0 {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// yggdrasil is whether ip is in Yggdrasil's range, 200::/7.
func yggdrasil(ip net.IP) bool {
	return ip.To4() == nil && len(ip) == net.IPv6len && ip[0]&0xfe == 0x02
}

// LANAddrs are this machine's IPv4 addresses on its LAN interfaces, for
// devices to reach it by.
func LANAddrs() []string {
	var out []string
	for _, n := range localNets() {
		if ip := n.IP.To4(); ip != nil && !ip.IsLoopback() {
			out = append(out, ip.String())
		}
	}
	return out
}
