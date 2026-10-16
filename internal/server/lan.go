package server

import (
	"net"
	"strings"
)

// virtual are interface name prefixes that aren't the machine's own local
// network: containers, VPNs and Yggdrasil itself.
var virtual = []string{"docker", "br-", "veth", "virbr", "tun", "tap", "tailscale", "wg", "zt", "utun", "lxc", "cni", "flannel"}

// LANAddrs is the machine's addresses on its local network: IPv4, and the
// IPv6 link-local address with its interface (which stays put when DHCP
// moves the IPv4 one), from interfaces that are up.
func LANAddrs() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, ll []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isVirtual(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			switch ip := n.IP; {
			case ip.To4() != nil:
				v4 = append(v4, ip.String())
			case ip.IsLinkLocalUnicast():
				ll = append(ll, ip.String()+"%"+ifc.Name)
			}
		}
	}
	return append(v4, ll...)
}

func isVirtual(name string) bool {
	for _, p := range virtual {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
