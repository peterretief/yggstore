package hub

import (
	"net"
	"strings"
)

// OnLAN is whether an address (host or host:port) is on the network this
// machine is on: loopback, private or link-local, or inside a network one
// of its interfaces is on. The last covers LANs with public addresses, such
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

// localNets are the networks this machine's interfaces are on, except
// Yggdrasil's. A variable, for tests.
var localNets = func() []*net.IPNet {
	var out []*net.IPNet
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
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
