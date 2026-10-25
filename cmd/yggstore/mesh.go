package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mesh"
	"github.com/peterretief/yggstore/internal/peers"
)

const meshUsage = `yggstore mesh: Yggdrasil links between the group's machines

  yggstore mesh                      which members each node is linked to, and the
                                     AllowedPublicKeys line for machines that listen
  yggstore mesh set NODE URI [URI...]  where other members can link to NODE, e.g.
                                     tls://203.0.113.5:14415 or wss://ygg.example.org:443
  yggstore mesh set NODE             stop listing addresses for NODE
  yggstore mesh public URI [URI...]  public Yggdrasil peers every node links to, so
                                     it reaches the group from any network
  yggstore mesh public               stop listing public peers
  yggstore mesh allow -conf FILE     on a machine members link to: add each member's
                                     key to FILE's AllowedPublicKeys, drop those of
                                     members who left, and run -restart if it changed

Run set on the admin machine; its dashboard sends the changed list to every
node. All take -peers. See docs/mesh.md.
`

func cmdMesh(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "set" {
		return meshSet(args[1:])
	}
	if len(args) > 0 && args[0] == "public" {
		return meshPublic(args[1:])
	}
	if len(args) > 0 && args[0] == "allow" {
		return meshAllow(ctx, args[1:])
	}
	fs := flag.NewFlagSet("mesh", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	fs.Usage = func() { fmt.Fprint(os.Stderr, meshUsage) }
	fs.Parse(args)
	if fs.NArg() > 0 {
		fs.Usage()
		os.Exit(2)
	}
	list, err := peers.Load(*peersPath)
	if err != nil {
		return err
	}
	c := client.New()
	var keys []string
	fmt.Printf("%-12s %-22s %s\n", "NODE", "LINKED DIRECTLY TO", "NOTES")
	for _, p := range list {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := c.Info(pctx, p.Addr)
		cancel()
		notes := []string{}
		if len(p.YggListen) > 0 {
			notes = append(notes, "listens at "+strings.Join(p.YggListen, ", "))
		}
		direct := "?"
		switch {
		case err != nil:
			notes = append(notes, "down")
		case info.Mesh == nil:
			notes = append(notes, "older yggstore: update it")
		case !info.Mesh.On:
			notes = append(notes, "can't manage its links: "+info.Mesh.Error)
		default:
			direct = strings.Join(info.Mesh.Direct, ", ")
			if direct == "" {
				direct = "nobody"
			}
			names := make([]string, 0, len(info.Mesh.Trying))
			for n := range info.Mesh.Trying {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				notes = append(notes, fmt.Sprintf("can't reach %s (%s)", n, info.Mesh.Trying[n]))
			}
			if n := len(info.Mesh.Public); n > 0 {
				up := 0
				for _, why := range info.Mesh.Public {
					if why == "up" {
						up++
					}
				}
				notes = append(notes, fmt.Sprintf("%d of %d public peers up", up, n))
			}
		}
		if err == nil && info.Mesh != nil && info.Mesh.Key != "" {
			keys = append(keys, info.Mesh.Key)
		}
		fmt.Printf("%-12s %-22s %s\n", p.Name, direct, strings.Join(notes, "; "))
	}
	if public := peers.PublicPeers(list); len(public) > 0 {
		fmt.Printf("\nPublic peers every node links to: %s\n", strings.Join(public, ", "))
	}
	if len(keys) > 0 {
		b, _ := json.Marshal(keys)
		fmt.Printf("\nOn a machine that listens and limits who may link to it, put this in\nits yggdrasil.conf, so every member may:\n  AllowedPublicKeys: %s\n", b)
	}
	return nil
}

func meshSet(args []string) error {
	fs := flag.NewFlagSet("mesh set", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	fs.Parse(reorder(args))
	if fs.NArg() < 1 {
		return errors.New("usage: yggstore mesh set NODE [URI...]")
	}
	name, uris := fs.Arg(0), fs.Args()[1:]
	for _, u := range uris {
		if err := peers.ValidListen(u); err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
	}
	list, err := peers.Load(*peersPath)
	if err != nil {
		return err
	}
	found := false
	for i := range list {
		if list[i].Name == name {
			list[i].YggListen, found = uris, true
		}
	}
	if !found {
		return fmt.Errorf("no node called %q in %s", name, *peersPath)
	}
	if err := peers.Write(*peersPath, list); err != nil {
		return err
	}
	if len(uris) == 0 {
		fmt.Printf("%s: no addresses listed now.\n", name)
	} else {
		fmt.Printf("%s: other members will link to %s.\n", name, strings.Join(uris, ", "))
	}
	fmt.Println("The dashboard sends the changed list to every node within a minute.")
	return nil
}

func meshPublic(args []string) error {
	fs := flag.NewFlagSet("mesh public", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	fs.Parse(reorder(args))
	uris := fs.Args()
	for _, u := range uris {
		if err := peers.ValidListen(u); err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
	}
	list, err := peers.Load(*peersPath)
	if err != nil {
		return err
	}
	// The list goes on the first admin's entry; any kept on other entries
	// is dropped, so what's given here is the whole list.
	admin := -1
	for i := range list {
		if list[i].Admin && admin < 0 {
			admin = i
		}
		list[i].YggPeers = nil
	}
	if admin < 0 {
		return fmt.Errorf("no admin node in %s to keep the list on", *peersPath)
	}
	if len(uris) > 0 {
		list[admin].YggPeers = uris
	}
	if err := peers.Write(*peersPath, list); err != nil {
		return err
	}
	if len(uris) == 0 {
		fmt.Println("No public peers listed now; nodes close the links they opened to them.")
	} else {
		fmt.Printf("Every node will link to %s.\n", strings.Join(uris, ", "))
	}
	fmt.Println("The dashboard sends the changed list to every node within a minute.")
	return nil
}

// reorder puts flags before the other arguments, so -peers may come last.
func reorder(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, args[i])
	}
	return append(flags, rest...)
}

// meshAllow keeps a listening machine's AllowedPublicKeys in step with the
// member list. It asks every member for its key (over Yggdrasil, which a
// new member reaches through the public peers), so it's run once a minute
// by a timer, as root. It prints nothing when nothing changed.
func meshAllow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mesh allow", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	conf := fs.String("conf", "/etc/yggdrasil/yggdrasil.conf", "Yggdrasil's settings (plain JSON)")
	restart := fs.String("restart", "systemctl restart yggdrasil", "command run after the list changed")
	state := fs.String("state", "", "keys this added before (default: mesh-allowed.json beside the peer list)")
	fs.Parse(reorder(args))
	if *state == "" {
		*state = filepath.Join(filepath.Dir(*peersPath), "mesh-allowed.json")
	}
	list, err := peers.Load(*peersPath)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New(*peersPath + " lists nobody")
	}
	c := &mesh.AllowConf{Path: *conf}
	current, err := c.Keys()
	if err != nil {
		return err
	}
	var before []string
	if b, err := os.ReadFile(*state); err == nil {
		if err := json.Unmarshal(b, &before); err != nil {
			return fmt.Errorf("%s: %w", *state, err)
		}
	}

	found := make([]string, len(list))
	cl := client.New()
	var wg sync.WaitGroup
	for i, p := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if info, err := cl.Info(pctx, p.Addr); err == nil && info.Mesh != nil {
				found[i] = info.Mesh.Key
			}
		}()
	}
	wg.Wait()
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs { // this machine's own key needn't be allowed
			if ip, ok := a.(*net.IPNet); ok {
				for i, k := range found {
					if mesh.KeyAddress(k) == ip.IP.String() {
						found[i] = ""
					}
				}
			}
		}
	}

	next, added := mesh.Allowed(current, list, found, before)
	b, _ := json.MarshalIndent(added, "", "  ")
	if slices.Equal(next, current) {
		if !slices.Equal(added, before) {
			return atomicfile.Replace(*state, append(b, '\n'), 0o600)
		}
		return nil
	}
	whose := func(k string) string {
		for _, p := range list {
			if mesh.KeyAddress(k) == net.ParseIP(p.IP()).String() {
				return p.Name
			}
		}
		return "no member's"
	}
	in := map[string]bool{}
	for _, k := range next {
		in[k] = true
	}
	for _, k := range next {
		if !slices.Contains(current, k) {
			fmt.Printf("allowing %s (%s)\n", k, whose(k))
		}
	}
	for _, k := range current {
		if !in[k] {
			fmt.Printf("no longer allowing %s (%s)\n", k, whose(k))
		}
	}
	backup := *conf + ".bak-mesh-allow"
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		if old, err := os.ReadFile(*conf); err == nil {
			atomicfile.Replace(backup, old, 0o600)
		}
	}
	if err := c.SetKeys(next); err != nil {
		return err
	}
	if err := atomicfile.Replace(*state, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if *restart != "" {
		if out, err := exec.CommandContext(ctx, "sh", "-c", *restart).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v %s", *restart, err, out)
		}
		fmt.Printf("ran %s\n", *restart)
	}
	return nil
}
