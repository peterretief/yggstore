package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

const meshUsage = `yggstore mesh: Yggdrasil links between the group's machines

  yggstore mesh                      which members each node is linked to, and the
                                     AllowedPublicKeys line for machines that listen
  yggstore mesh set NODE URI [URI...]  where other members can link to NODE, e.g.
                                     tls://203.0.113.5:14415 or wss://ygg.example.org:443
  yggstore mesh set NODE             stop listing addresses for NODE

Run set on the admin machine; its dashboard sends the changed list to every
node. All take -peers. See docs/mesh.md.
`

func cmdMesh(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "set" {
		return meshSet(args[1:])
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
		}
		if err == nil && info.Mesh != nil && info.Mesh.Key != "" {
			keys = append(keys, info.Mesh.Key)
		}
		fmt.Printf("%-12s %-22s %s\n", p.Name, direct, strings.Join(notes, "; "))
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
