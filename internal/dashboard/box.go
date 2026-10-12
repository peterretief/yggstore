package dashboard

import (
	"net"
	"os"

	"github.com/peterretief/yggstore/internal/hub"
)

// BoxState is what the dashboard knows about the machine it runs on, for
// the "My box" panel: what its owner needs to find it and see it is well.
type BoxState struct {
	Hostname  string   `json:"hostname,omitempty"`
	LAN       []string `json:"lan,omitempty"` // its addresses on the local network
	DiskFree  int64    `json:"disk_free,omitempty"`
	DiskTotal int64    `json:"disk_total,omitempty"`
	Version   string   `json:"version,omitempty"`
}

func (d *Dashboard) boxState() *BoxState {
	b := &BoxState{Version: d.cfg.Version}
	b.Hostname, _ = os.Hostname()
	b.DiskFree, b.DiskTotal = diskSpace(d.cfg.StubDir)
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || hub.Virtual(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
				b.LAN = append(b.LAN, n.IP.String())
			}
		}
	}
	return b
}
