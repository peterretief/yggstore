package hub

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A friendly name on the box's own network, such as mail.local, for its
// webmail (webmail.go): Avahi, which already gives the box BOX.local, also
// answers for the names in /etc/avahi/hosts, so the hub keeps a line there
// with the box's address on its network, changes it when that address
// changes, and has Avahi read it again. Only the line it wrote is touched.

const (
	avahiHosts = "/etc/avahi/hosts"
	localMark  = "# yggstore hub: webmail on this network"
)

// lanAddress is the box's address on its own network: the one it would
// send from towards the internet (no packet goes).
func lanAddress() string {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	a, _ := c.LocalAddr().(*net.UDPAddr)
	if a == nil || !(a.IP.IsPrivate() || a.IP.IsLinkLocalUnicast()) {
		return ""
	}
	return a.IP.String()
}

// hostsWith is the hosts file with our line for name at addr ("" removes it).
func hostsWith(old, name, addr string) string {
	var out []string
	skip := false
	for _, l := range strings.Split(strings.TrimRight(old, "\n"), "\n") {
		if skip {
			skip = false
			continue
		}
		if l == localMark {
			skip = true // our name is the next line
			continue
		}
		if l != "" || len(out) > 0 {
			out = append(out, l)
		}
	}
	if addr != "" {
		out = append(out, localMark, addr+" "+name)
	}
	return strings.Join(out, "\n") + "\n"
}

// KeepLocalName keeps NAME.local pointing at the box, every minute, until
// ctx ends. It does nothing on a machine without Avahi.
func KeepLocalName(ctx context.Context, name string, logf func(string, ...any)) {
	if name == "" {
		return
	}
	if _, err := os.Stat("/etc/avahi"); err != nil {
		logf("hub: no Avahi here, so no %s.local", name)
		return
	}
	said := ""
	for {
		if addr := lanAddress(); addr != "" && addr != said {
			old, _ := os.ReadFile(avahiHosts)
			next := hostsWith(string(old), name+".local", addr)
			if next != string(old) {
				if err := os.WriteFile(avahiHosts, []byte(next), 0o644); err != nil {
					logf("hub: %s.local: %v", name, err)
				} else if out, err := exec.CommandContext(ctx, "avahi-daemon", "--reload").CombinedOutput(); err != nil {
					logf("hub: %s.local: avahi-daemon --reload: %v %s", name, err, out)
				} else {
					logf("hub: %s.local is %s on this network", name, addr)
				}
			}
			said = addr
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}
