package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/contacts"
	"github.com/peterretief/yggstore/internal/invite"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
)

// cmdJoin sets this machine up as a member of a group, from an invite.
func cmdJoin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	name := fs.String("name", hostname(), "name for this node, as the group sees it")
	me := fs.String("me", userName(), "your name, as the group sees it")
	quotaGB := fs.Float64("quota", 20, "space to give the group, in GB")
	port := fs.Int("port", 7400, "port for this node")
	dir := fs.String("dir", yggstoreHome(), "where to keep this node's files")
	outfiles := fs.String("outfiles", filepath.Join(homeDir(), "yggstore"), "your outbox folder (for the dashboard)")
	customers := fs.Bool("customers", false, "also hold paying customers' files (earns you credit; you can change this later)")
	service := fs.Bool("service", false, "also install and start user services for the node and dashboard (Linux)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: yggstore join [-name NODE] [-me NAME] [-quota GB] INVITE")
	}
	inv, err := invite.Decode(fs.Arg(0))
	if err != nil {
		return err
	}
	// Everything that would stop the join is checked before the group is
	// asked, so a refused join leaves the machine, and the group, as they were.
	nodeUnit, dashUnit, dashListen := nodeServices(*dir, *name, *port)
	if err := alreadyJoined(*dir); err != nil {
		return err
	}
	if *service {
		if err := unitsFree(nodeUnit, dashUnit); err != nil {
			return err
		}
	}
	fmt.Printf("Joining %s, invited by %s.\n", inv.Group, inv.From)

	t, err := transport.ByName("ygg")
	if err != nil {
		return err
	}
	// Without Yggdrasil on this machine, the node runs its own, linked
	// through the peers in the invite.
	var builtin []string
	ip, err := t.LocalIP()
	if err != nil {
		yggPeers := strings.Join(inv.YggPeers, ",")
		if yggPeers == "" {
			yggPeers = defaultYggPeers
		}
		if err := os.MkdirAll(*dir, 0o700); err != nil {
			return err
		}
		yggKey := filepath.Join(*dir, "ygg.key")
		node, err := startBuiltin(yggKey, yggPeers, "", true, *port)
		if err != nil {
			return err
		}
		defer node.Close()
		ip = node.Addr()
		builtin = []string{"-transport", "builtin", "-ygg-key", yggKey, "-ygg-peers", yggPeers}
		fmt.Println("This machine has no Yggdrasil running, so the node will use its own, built in.")
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(*port))
	if builtin == nil {
		// A node of another group may already answer on this port.
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("something on this machine already uses port %d (another yggstore node?); join with another -port, and its own -dir", *port)
		}
		l.Close()
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(*dir, "sharing.key")
	id, err := share.LoadOrCreate(keyPath)
	if err != nil {
		return err
	}

	c := client.New()
	jctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := c.Join(jctx, inv.Admin.Addr, invite.Request{Token: inv.Token, Node: *name, Person: *me, Addr: addr, SharingCode: id.Code()})
	if err != nil {
		// A refusal starts with its HTTP status; "400" alone also matches port 7400.
		if msg := err.Error(); strings.HasPrefix(msg, "403 ") || strings.HasPrefix(msg, "400 ") {
			return fmt.Errorf("the group did not accept this machine: %v", err)
		}
		if builtin != nil {
			return fmt.Errorf("could not reach the group (%s at %s) through Yggdrasil peers %s: %v\n"+
				"Check this machine is online and can reach them, or ask %s for an invite with other peers.", inv.Admin.Name, inv.Admin.Addr, builtin[5], err, inv.From)
		}
		return fmt.Errorf("could not reach the group (%s at %s): %v\n\n%s", inv.Admin.Name, inv.Admin.Addr, err, yggHelp(inv))
	}

	peersPath := filepath.Join(*dir, "peers.json")
	if err := peers.Write(peersPath, resp.Peers); err != nil {
		return err
	}
	contactsPath := filepath.Join(*dir, "contacts.json")
	if inv.SharingCode != "" {
		if err := contacts.Add(contactsPath, contacts.Contact{Name: inv.From, Code: inv.SharingCode}, id.Code()); err != nil && !errors.Is(err, contacts.ErrExists) {
			fmt.Printf("note: could not add %s as a contact: %v\n", inv.From, err)
		}
	}

	exe, _ := os.Executable()
	node := []string{exe, "serve", "-peers", peersPath, "-data", filepath.Join(*dir, "shards"), "-port", strconv.Itoa(*port),
		"-name", resp.Node, "-quota", strconv.FormatFloat(*quotaGB, 'f', -1, 64),
		"-sharing-key", keyPath, "-contacts", contactsPath, "-invites", filepath.Join(*dir, "invites.json")}
	node = append(node, builtin...)
	if *customers {
		node = append(node, "-customers")
	}
	dash := []string{exe, "dashboard", "-peers", peersPath, "-outfiles", *outfiles,
		"-sharing-key", keyPath, "-contacts", contactsPath, "-me", *me, "-remote"}
	if dashListen != defaultDashListen {
		// A second node's dashboard: the first one keeps the usual port and
		// the mail bridge.
		dash = append(dash, "-listen", dashListen, "-imap", "", "-smtp", "")
	}

	fmt.Printf("\nYou're in: this node is %q (%s), giving %g GB.\n", resp.Node, addr, *quotaGB)
	fmt.Printf("The group has %d nodes; %s is in your contacts.\n", len(resp.Peers), inv.From)
	if *service {
		if err := installUnits(map[string]string{
			nodeUnit: unit("yggstore node ("+inv.Group+")", node),
			dashUnit: unit("yggstore dashboard on http://"+dashListen, dash),
		}); err != nil {
			return fmt.Errorf("joined, but the services could not be set up: %w\nStart them by hand:\n  %s\n  %s",
				err, shellJoin(node), shellJoin(dash))
		}
		fmt.Printf("The node and your dashboard are running: open http://%s\n", dashListen)
		fmt.Printf("To open it from your other devices, see yggstore devices.\n")
		fmt.Printf("Drop files into %s to store them.\n", *outfiles)
		return nil
	}
	fmt.Printf("\nStart your node (keep it running so the group can use your space):\n  %s\n", shellJoin(node))
	fmt.Printf("Start your dashboard, then open http://%s:\n  %s\n", dashListen, shellJoin(dash))
	fmt.Printf("Or run join again with -service to have both started for you.\n")
	return nil
}

func yggHelp(inv invite.Invite) string {
	var b strings.Builder
	b.WriteString("This machine's Yggdrasil can't reach the group. Check it has peers (`sudo yggdrasilctl getPeers`);\n")
	b.WriteString("these are the group's (Peers: [...] in /etc/yggdrasil.conf or /etc/yggdrasil/yggdrasil.conf):\n")
	for _, p := range inv.YggPeers {
		fmt.Fprintf(&b, "  %s\n", p)
	}
	b.WriteString("Or stop Yggdrasil and run this again: the node then uses its own, built in.")
	return b.String()
}

const defaultDashListen = "127.0.0.1:7480"

// alreadyJoined refuses a second group in one folder: joining would
// overwrite the first group's member list, leaving that group without
// telling it. A machine is in one group; a second is a second node, with
// its own folder and port.
func alreadyJoined(dir string) error {
	list, err := peers.Load(filepath.Join(dir, "peers.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%s has a member list that can't be read (%v); move it away to join a group here", filepath.Join(dir, "peers.json"), err)
	}
	admin := "its admin"
	for _, p := range list {
		if p.Admin {
			admin = p.Name
			break
		}
	}
	return fmt.Errorf("this machine is already in a group (%d nodes, admin %s), kept in %s.\n"+
		"To join another group as well, run it as a second node with its own folder and port, e.g.\n"+
		"  yggstore join -dir %s-2 -port 7401 [-name NODE] INVITE", len(list), admin, dir, filepath.Clean(dir))
}

// nodeServices names the units and the dashboard address for a node kept in
// dir: the usual ones for the first node, and ones of its own, after its
// name and port, for a second.
func nodeServices(dir, name string, port int) (node, dash, dashListen string) {
	if filepath.Clean(dir) == filepath.Clean(yggstoreHome()) && port == 7400 {
		return "yggstore-node.service", "yggstore-dashboard.service", defaultDashListen
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	dashPort := 7480 + port - 7400
	switch {
	case port <= 7400:
		dashPort = 7481
	case dashPort > 65535:
		dashPort = port - 1
	}
	return "yggstore-node-" + safe + ".service", "yggstore-dashboard-" + safe + ".service",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(dashPort))
}

// unitsFree checks that none of the named units exists yet.
func unitsFree(names ...string) error {
	for _, n := range names {
		path := filepath.Join(unitDir(), n)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists, so a node is set up here already; join with its own -dir, -port and -name, or without -service", path)
		}
	}
	return nil
}

// unitDir is where installUnits writes units.
func unitDir() string {
	if asRoot() {
		return "/etc/systemd/system"
	}
	return filepath.Join(homeDir(), ".config", "systemd", "user")
}

// asRoot is whether services are the machine's own (run as root, from
// /etc/systemd/system, started at boot) rather than the user's. A variable,
// for tests.
var asRoot = func() bool { return os.Geteuid() == 0 }

// systemctl runs systemctl for the machine's services or the user's.
func systemctl(args ...string) error {
	if !asRoot() {
		args = append([]string{"--user"}, args...)
	}
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

// installUnits writes systemd units, named NAME.service, and starts them:
// the machine's own when run as root, else the user's.
func installUnits(units map[string]string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found")
	}
	unitDir := unitDir()
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	start := []string{"enable", "--now"}
	for name, body := range units {
		start = append(start, strings.TrimSuffix(name, ".service"))
		path := filepath.Join(unitDir, name)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; not overwriting it", path)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl(start...)
}

// unit is a service running cmd. HOME is set because systemd gives root's
// services none, and yggstore keeps its files under it. after names units
// it should start after, and with, if they exist.
func unit(desc string, cmd []string, after ...string) string {
	target := "default.target"
	if asRoot() {
		target = "multi-user.target"
	}
	deps := ""
	for _, a := range after {
		deps += "Wants=" + a + "\nAfter=" + a + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=%s
Wants=network-online.target
After=network-online.target
%s
[Service]
Environment=%s
ExecStart=%s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=%s
`, desc, deps, shellJoin([]string{"HOME=" + homeDir()}), shellJoin(cmd), target)
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"$\\") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

func homeDir() string {
	if d, err := os.UserHomeDir(); err == nil {
		return d
	}
	return "."
}
