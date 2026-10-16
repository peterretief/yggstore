package peers

import (
	"context"
	"net"
	"testing"
)

func TestYggNames(t *testing.T) {
	list := []Peer{
		{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org"},
		{Name: "Box-4", Addr: "[201::4]:7400"},
		{Name: "box-4-usb1", Addr: "[201::4]:7401", Host: "Box-4"}, // the same machine
		{Name: "not a label", Addr: "[201::5]:7400"},
		{Name: "lan-only", Addr: "192.168.0.9:7400"},
	}
	got := YggNames(list)
	want := map[string]string{"200::1": "desktop.ygg.example.org", "201::4": "box-4.ygg.example.org"}
	if len(got) != len(want) {
		t.Fatalf("names %v", got)
	}
	for ip, n := range want {
		if got[ip] != n {
			t.Errorf("%s: %q, want %q", ip, got[ip], n)
		}
	}
	if n := YggName(list, "201:0::4"); n != "box-4.ygg.example.org" {
		t.Errorf("YggName: %q", n)
	}
	list[0].Domain = ""
	if n := YggNames(list); len(n) != 0 {
		t.Errorf("names without a domain: %v", n)
	}
}

func TestNameCheck(t *testing.T) {
	asked := 0
	c := &NameCheck{Lookup: func(_ context.Context, host string) ([]net.IP, error) {
		asked++
		if host == "box.ygg.example.org" {
			return []net.IP{net.ParseIP("201::4")}, nil
		}
		return nil, &net.DNSError{IsNotFound: true}
	}}
	if !c.Points("box.ygg.example.org", "201:0::4") || !c.Points("box.ygg.example.org", "201::4") {
		t.Fatal("a name in DNS isn't found")
	}
	if c.Points("box.ygg.example.org", "201::5") || c.Points("new.ygg.example.org", "201::4") || c.Points("", "201::4") {
		t.Fatal("a wrong or missing name passes")
	}
	before := asked
	c.Points("box.ygg.example.org", "201::4")
	if asked != before {
		t.Fatal("asked DNS again at once")
	}
}
