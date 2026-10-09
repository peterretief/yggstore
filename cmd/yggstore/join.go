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
	fmt.Printf("Joining %s, invited by %s.\n", inv.Group, inv.From)

	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
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
		if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "400") {
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

	fmt.Printf("\nYou're in: this node is %q (%s), giving %g GB.\n", resp.Node, addr, *quotaGB)
	fmt.Printf("The group has %d nodes; %s is in your contacts.\n", len(resp.Peers), inv.From)
	if *service {
		if err := installServices(inv.Group, node, dash); err != nil {
			return fmt.Errorf("joined, but the services could not be set up: %w\nStart them by hand:\n  %s\n  %s",
				err, shellJoin(node), shellJoin(dash))
		}
		fmt.Printf("The node and your dashboard are running: open http://127.0.0.1:7480\n")
		fmt.Printf("To open it from your other devices, see yggstore devices.\n")
		fmt.Printf("Drop files into %s to store them.\n", *outfiles)
		return nil
	}
	fmt.Printf("\nStart your node (keep it running so the group can use your space):\n  %s\n", shellJoin(node))
	fmt.Printf("Start your dashboard, then open http://127.0.0.1:7480:\n  %s\n", shellJoin(dash))
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

// installServices writes systemd user units for the node and dashboard and
// starts them.
func installServices(group string, node, dash []string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found")
	}
	unitDir := filepath.Join(homeDir(), ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	units := map[string]string{
		"yggstore-node.service":      unit("yggstore node ("+group+")", node),
		"yggstore-dashboard.service": unit("yggstore dashboard on http://127.0.0.1:7480", dash),
	}
	for name, body := range units {
		path := filepath.Join(unitDir, name)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; not overwriting it", path)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", "yggstore-node", "yggstore-dashboard"}} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

func unit(desc string, cmd []string) string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target

[Service]
ExecStart=%s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, desc, shellJoin(cmd))
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
