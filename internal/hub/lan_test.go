package hub

import (
	"net"
	"testing"
)

func TestOnLAN(t *testing.T) {
	saved := localNets
	defer func() { localNets = saved }()
	_, home6, _ := net.ParseCIDR("2a02:1810:4d00:1::/64") // a home's public IPv6 prefix
	_, odd4, _ := net.ParseCIDR("100.70.0.0/24")          // a LAN on non-private IPv4
	localNets = func() []*net.IPNet { return []*net.IPNet{home6, odd4} }
	for addr, want := range map[string]bool{
		"192.168.1.5:22000":            true,
		"10.20.30.40:22000":            true,
		"172.20.0.9:22000":             true,
		"[fd12::5]:22000":              true,
		"[fe80::1%eth0]:22000":         true,
		"[fe80::1]:22000":              true,
		"127.0.0.1:1":                  true,
		"[2a02:1810:4d00:1::77]:22000": true, // same IPv6 network as the box
		"100.70.0.12:22000":            true,
		"[2a02:1810:4d00:2::77]:22000": false, // the next network over
		"8.8.8.8:22000":                false,
		"[201:abcd::1]:22000":          false, // Yggdrasil
		"not an address":               false,
	} {
		if got := OnLAN(addr); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}
