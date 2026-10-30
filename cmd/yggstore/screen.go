package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

// yggstore screen is the box's own screen (docs/box-setup.md): what works
// and what doesn't, for whoever is in front of it, with Restart and Shut
// down. It runs on the first console in place of the login prompt, so it
// shows nothing secret and offers no shell.

const screenUsage = `yggstore screen: the box's status, on its own screen

  yggstore screen [-peers peers.json] [-name BOX] [-every 15s] [-once]

It shows the network, the internet, the group, this box's storage and its
services, and takes R (restart) and S (shut down). -once prints the status
one time and stops, without reading keys.
`

type screenLine struct {
	Label, Text string
	OK          bool
	Fix         string // what to do, when it isn't ok
}

type screenStatus struct {
	Box, Owner, Version string
	At                  time.Time
	Lines               []screenLine
	Setup               bool         // nobody is on the box yet: offer to set it up
	Have                []screenLine // the first person's website, email and drive
}

func cmdScreen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("screen", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, screenUsage) }
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	host, _ := os.Hostname()
	name := fs.String("name", host, "this box's name in the group's list")
	every := fs.Duration("every", 15*time.Second, "how often the status is checked")
	once := fs.Bool("once", false, "print the status once, and don't read keys")
	base := fs.String("dashboard", "http://127.0.0.1:7480", "the dashboard on this machine")
	fs.Parse(args)
	d := dash{strings.TrimSuffix(*base, "/")}

	if *once {
		fmt.Print(renderScreen(checkScreen(ctx, *peersPath, *name, d), ""))
		return nil
	}
	restore, err := rawTerminal(os.Stdin)
	if err != nil {
		return err
	}
	defer restore()
	fmt.Print("\033[?25l") // no cursor
	defer fmt.Print("\033[?25h\033[2J\033[H")

	keys := make(chan byte)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			b, err := r.ReadByte()
			if err != nil {
				close(keys)
				return
			}
			keys <- b
		}
	}()

	var st screenStatus
	asking := "" // "restart" or "shut down", while it waits for Y
	refresh := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refresh.C:
			st = checkScreen(ctx, *peersPath, *name, d)
			refresh.Reset(*every)
		case b, ok := <-keys:
			if !ok {
				return io.EOF
			}
			switch k := strings.ToLower(string(b)); {
			case asking != "" && k == "y":
				verb, doing := "reboot", "Restarting..."
				if asking == "shut down" {
					verb, doing = "poweroff", "Shutting down. Wait for the screen to go blank before unplugging it."
				}
				fmt.Print(renderScreen(st, "") + "\n  " + doing + "\n")
				if err := exec.Command("systemctl", verb).Run(); err != nil {
					asking = ""
					fmt.Print("\n  It didn't work: " + err.Error() + "\n")
					time.Sleep(3 * time.Second)
				}
				continue
			case asking != "":
				asking = ""
			case (k == "\r" || k == "\n") && st.Setup:
				err := runSetup(ctx, keyReader{keys}, *name, d, *peersPath)
				if errors.Is(err, io.EOF) {
					return err
				}
				st = checkScreen(ctx, *peersPath, *name, d)
			case k == "r":
				asking = "restart"
			case k == "s":
				asking = "shut down"
			}
		}
		fmt.Print(renderScreen(st, asking))
	}
}

// checkScreen finds out what works. Every check has a time limit, so a
// network that's down slows it by seconds, not minutes.
func checkScreen(ctx context.Context, peersPath, name string, d dash) screenStatus {
	st := screenStatus{Box: name, Version: versionString(), At: time.Now()}
	st.Lines = append(st.Lines, checkNetwork())
	st.Lines = append(st.Lines, checkInternet(ctx))

	list, err := peers.Load(peersPath)
	if err != nil {
		st.Lines = append(st.Lines, screenLine{Label: "Group", Text: "no group list", Fix: "this box hasn't joined a group"})
	} else {
		group, storage, owner := checkGroup(ctx, list, name)
		st.Owner = owner
		st.Lines = append(st.Lines, group, storage)
	}
	st.Lines = append(st.Lines, checkServices())
	if on, people, err := boxPeople(d); err == nil && on {
		if len(people) == 0 {
			st.Setup = true
		} else {
			list, _ := peers.Load(peersPath)
			st.Have = ownerLines(people[0], peers.GroupDomain(list))
		}
	}
	return st
}

// checkNetwork reports the interface the default route uses.
func checkNetwork() screenLine {
	l := screenLine{Label: "Network"}
	ifname := defaultRouteIface()
	if ifname == "" {
		l.Text, l.Fix = "not connected", "plug in the network cable, or join Wi-Fi"
		return l
	}
	kind := "cable"
	if _, err := os.Stat("/sys/class/net/" + ifname + "/wireless"); err == nil {
		kind = "Wi-Fi"
		if out, err := exec.Command("iwgetid", "-r", ifname).Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			kind = fmt.Sprintf("Wi-Fi %q", strings.TrimSpace(string(out)))
		}
	} else if s := readSys(ifname, "speed"); s != "" && s != "-1" {
		kind = "cable, " + mbps(s)
	}
	addr := ""
	if ifc, err := net.InterfaceByName(ifname); err == nil {
		if as, err := ifc.Addrs(); err == nil {
			for _, a := range as {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					addr = ipn.IP.String()
					break
				}
			}
		}
	}
	l.Text, l.OK = strings.TrimSpace(kind+"  "+addr), true
	return l
}

func mbps(s string) string {
	if s == "1000" {
		return "1 Gbps"
	}
	return s + " Mbps"
}

func readSys(ifname, f string) string {
	b, err := os.ReadFile("/sys/class/net/" + ifname + "/" + f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// defaultRouteIface reads /proc/net/route for the default route's
// interface, so it needs no tools.
func defaultRouteIface() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

func checkInternet(ctx context.Context) screenLine {
	l := screenLine{Label: "Internet"}
	d := net.Dialer{Timeout: 4 * time.Second}
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
		if c, err := d.DialContext(ctx, "tcp", addr); err == nil {
			c.Close()
			l.Text, l.OK = "yes", true
			return l
		}
	}
	l.Text, l.Fix = "no", "the router is on, but the internet isn't: restart the router"
	if defaultRouteIface() == "" {
		l.Fix = "connect the network first"
	}
	return l
}

// checkGroup asks every node in the group's list for its info at once:
// how many answer, and this box's own nodes for storage.
func checkGroup(ctx context.Context, list []peers.Peer, name string) (group, storage screenLine, owner string) {
	group, storage = screenLine{Label: "Group"}, screenLine{Label: "Storage"}
	c := client.New()
	var mu sync.Mutex
	var wg sync.WaitGroup
	up, machines := map[string]bool{}, map[string]bool{}
	var used, quota int64
	mine, mineUp := 0, 0
	for _, p := range list {
		machines[p.Machine()] = true
		self := p.Machine() == name
		if self {
			mine++
			owner = p.Operator()
		}
		wg.Add(1)
		go func(p peers.Peer) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			info, err := c.Info(pctx, p.Addr)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if !self {
				up[p.Machine()] = true
			} else {
				mineUp++
				used += info.UsedBytes
				quota += info.QuotaBytes
			}
		}(p)
	}
	wg.Wait()

	others := len(machines)
	if mine > 0 {
		others--
	}
	group.Text = fmt.Sprintf("reached %d of %d other machines", len(up), others)
	switch {
	case others == 0:
		group.Text, group.OK = "no other machines yet", true
	case len(up) == 0:
		group.Fix = "none answer: check the internet line above"
	default:
		group.OK = true
	}

	switch {
	case mine == 0:
		storage.Text, storage.Fix = "this box isn't in the group's list", "ask the group's admin"
	case mineUp == 0:
		storage.Text, storage.Fix = "not answering", "restart the box (R)"
	default:
		storage.Text, storage.OK = fmt.Sprintf("%s used of %s", size(used), size(quota)), true
		if mineUp < mine {
			storage.Text += fmt.Sprintf(", %d of %d disks", mineUp, mine)
			storage.OK, storage.Fix = false, "a disk isn't answering: is it plugged in?"
		}
	}
	return group, storage, owner
}

// checkServices lists the box's own services that should be running.
// Timer-started ones that have finished are left out.
func checkServices() screenLine {
	l := screenLine{Label: "Services"}
	out, err := exec.Command("systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend",
		"yggstore*", "yggdrasil*", "syncthing*").Output()
	if err != nil {
		l.Text, l.Fix = "can't tell", "restart the box (R)"
		return l
	}
	running := 0
	var failed []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		switch {
		case f[2] == "failed":
			failed = append(failed, strings.TrimSuffix(f[0], ".service"))
		case f[3] == "running":
			running++
		}
	}
	if len(failed) > 0 {
		l.Text, l.Fix = "not running: "+strings.Join(failed, ", "), "restart the box (R); if it stays, tell the group's admin"
		return l
	}
	l.Text, l.OK = fmt.Sprintf("all %d running", running), true
	return l
}

// renderScreen draws the whole screen. It fits 64 columns, the narrowest
// a large console font gives on a small screen.
func renderScreen(st screenStatus, asking string) string {
	const green, red, dim, bold, off = "\033[32m", "\033[31m", "\033[2m", "\033[1m", "\033[0m"
	var b strings.Builder
	b.WriteString("\033[2J\033[H\n")
	head := "  " + bold + "LocalBox  " + st.Box + off
	if st.Owner != "" {
		head += "    " + st.Owner + "'s box"
	}
	b.WriteString(head + "\n\n")
	for _, l := range st.Lines {
		mark := green + "ok" + off
		if !l.OK {
			mark = red + "not ok" + off
		}
		fmt.Fprintf(&b, "  %-10s %-40s %s\n", l.Label, l.Text, mark)
		if !l.OK && l.Fix != "" {
			fmt.Fprintf(&b, "  %-10s %s%s%s\n", "", red, l.Fix, off)
		}
	}
	if len(st.Have) > 0 {
		b.WriteString("\n")
		for _, l := range st.Have {
			fmt.Fprintf(&b, "  %-10s %s\n", l.Label, l.Text)
		}
	}
	if st.Setup && asking == "" {
		b.WriteString("\n  " + bold + "Press Enter to set up this box." + off + "\n")
	}
	b.WriteString("\n")
	switch asking {
	case "":
		b.WriteString("  " + bold + "R" + off + " Restart    " + bold + "S" + off + " Shut down\n")
		b.WriteString("\n  " + dim + "Admin: Ctrl+Alt+F2, log in, then type  sudo yggstore admin" + off + "\n")
	default:
		b.WriteString("  " + bold + strings.ToUpper(asking[:1]) + asking[1:] + " the box?" + off +
			"  Press Y for yes, any other key for no.\n")
	}
	if !st.At.IsZero() {
		fmt.Fprintf(&b, "\n  %sChecked %s, yggstore %s%s\n", dim, st.At.Format("15:04:05"), st.Version, off)
	}
	return b.String()
}
