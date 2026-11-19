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
// webmail (webmail.go). Avahi already gives the box BOX.local; the hub runs
// avahi-publish (from avahi-utils) to say NAME.local is the box's address
// on its network too, and starts it again when that address changes. -R
// leaves out the reverse entry, which Avahi would refuse as a second name
// for the box's own address. If another box on the network has the name
// already, Avahi keeps it theirs and this one says so once.

// avahiHosts held the name before avahi-publish did; the line written then
// is taken out again.
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

// withoutOurLine is a hosts file without the line the hub once wrote.
func withoutOurLine(old string) string {
	var out []string
	skip := false
	for _, l := range strings.Split(strings.TrimRight(old, "\n"), "\n") {
		if skip {
			skip = false
			continue
		}
		if l == localMark {
			skip = true
			continue
		}
		out = append(out, l)
	}
	return strings.TrimLeft(strings.Join(out, "\n"), "\n") + "\n"
}

// KeepLocalName keeps NAME.local pointing at the box until ctx ends.
func KeepLocalName(ctx context.Context, name string, logf func(string, ...any)) {
	if name == "" {
		return
	}
	if old, err := os.ReadFile(avahiHosts); err == nil && strings.Contains(string(old), localMark) {
		if os.WriteFile(avahiHosts, []byte(withoutOurLine(string(old))), 0o644) == nil {
			exec.Command("avahi-daemon", "--reload").Run()
		}
	}
	bin, err := exec.LookPath("avahi-publish")
	if err != nil {
		logf("hub: no %s.local: avahi-publish isn't here (apt install avahi-utils)", name)
		return
	}
	said := ""
	for ctx.Err() == nil {
		addr := lanAddress()
		if addr == "" {
			sleepCtx(ctx, time.Minute)
			continue
		}
		pctx, stop := context.WithCancel(ctx)
		cmd := exec.CommandContext(pctx, bin, "-a", "-R", name+".local", addr)
		out := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			stop()
			logf("hub: %s.local: %v", name, err)
			sleepCtx(ctx, 5*time.Minute)
			continue
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		if said != addr {
			logf("hub: %s.local is %s on this network", name, addr)
			said = addr
		}
		// Until the address changes, or Avahi stops it (another box has
		// the name, say).
		for running := true; running; {
			select {
			case <-ctx.Done():
				running = false
			case <-done:
				running = false
				if msg := strings.TrimSpace(out.String()); msg != "" && said != "!"+msg {
					logf("hub: %s.local: %s", name, msg)
					said = "!" + msg
				}
				sleepCtx(ctx, time.Minute)
			case <-time.After(30 * time.Second):
				running = lanAddress() == addr
			}
		}
		stop()
		<-done
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
