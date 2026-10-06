package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

const siteUsage = `yggstore site: static websites hosted on the group

  yggstore site publish FOLDER [-name DOMAIN]   store a new version and serve it (DOMAIN
                                                defaults to the folder's name)
  yggstore site list                            sites published from this machine
  yggstore site versions DOMAIN                 a site's versions, newest first
  yggstore site rollback DOMAIN [VERSION]       serve an earlier version (default: the one before)
  yggstore site announce                        tell web nodes about every site again
  yggstore site remove DOMAIN -yes              stop serving it and delete every version
  yggstore site status                          which web nodes serve which version

Publishing needs this machine's node running (it announces through its
messaging). Web nodes are nodes started with -web. See docs/sites.md.
Options: -peers FILE, -api 127.0.0.1:7401, -token ~/.yggstore/msg.token.
`

// localAnnouncer publishes through this machine's node.
type localAnnouncer struct{ l msg.Local }

func (a localAnnouncer) Publish(ctx context.Context, topic, typ, body string) error {
	_, err := a.l.Publish(ctx, topic, typ, body)
	return err
}

func cmdSite(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, siteUsage)
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("site "+sub, flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "the group's member list"+peersHelp)
	api := fs.String("api", "127.0.0.1:7401", "this node's local messaging API")
	tokenPath := fs.String("token", filepath.Join(yggstoreHome(), "msg.token"), "the node's messaging token file")
	name := fs.String("name", "", "the domain the site is served at")
	yes := fs.Bool("yes", false, "confirm removing a site")
	fs.Usage = func() { fmt.Fprint(os.Stderr, siteUsage) }
	fs.Parse(reorder(args[1:]))

	pub := &site.Publisher{Dir: filepath.Join(yggstoreHome(), "sites"), Client: client.New(), Log: log.Printf}
	announcer := func() error {
		token, err := msg.ReadToken(*tokenPath)
		if err != nil {
			return err
		}
		pub.Announce = localAnnouncer{msg.Local{Addr: *api, Token: token}}
		return nil
	}
	arg := func(i int) string { return fs.Arg(i) }

	switch sub {
	case "publish":
		if fs.NArg() != 1 {
			return errors.New("usage: yggstore site publish FOLDER [-name DOMAIN]")
		}
		folder := filepath.Clean(arg(0))
		domain := site.Normalise(*name)
		if domain == "" {
			domain = site.Normalise(filepath.Base(folder))
		}
		if err := site.ValidName(domain); err != nil {
			return fmt.Errorf("%w\nGive it with -name, e.g. -name example.org", err)
		}
		if err := announcer(); err != nil {
			return err
		}
		online, err := onlinePeers(ctx, *peersPath)
		if err != nil {
			return err
		}
		v, reused, err := pub.Publish(ctx, domain, folder, online)
		if err != nil {
			return err
		}
		fmt.Printf("%s: serving version %s (%d files, %s", domain, v.ID[:8], v.Files, size(v.Bytes))
		if reused > 0 {
			fmt.Printf(", %d unchanged parts reused", reused)
		}
		fmt.Println(").\nWeb nodes switch to it within seconds; check with: yggstore site status")
	case "list":
		for _, s := range pub.Sites() {
			vs, _ := pub.Versions(s)
			cur := "?"
			for _, v := range vs {
				if v.Current {
					cur = v.ID[:8] + " from " + time.Unix(v.StoredAt, 0).Format("2 Jan 2006 15:04")
				}
			}
			fmt.Printf("%-30s %d version(s), serving %s\n", s, len(vs), cur)
		}
	case "versions":
		vs, err := pub.Versions(site.Normalise(arg(0)))
		if err != nil {
			return err
		}
		for _, v := range vs {
			mark := " "
			if v.Current {
				mark = "*"
			}
			fmt.Printf("%s %s  %s  %d files, %s\n", mark, v.ID[:8], time.Unix(v.StoredAt, 0).Format("2 Jan 2006 15:04:05"), v.Files, size(v.Bytes))
		}
		fmt.Println("(* is being served)")
	case "rollback":
		if fs.NArg() < 1 {
			return errors.New("usage: yggstore site rollback DOMAIN [VERSION]")
		}
		if err := announcer(); err != nil {
			return err
		}
		v, err := pub.Rollback(ctx, site.Normalise(arg(0)), arg(1))
		if err != nil {
			return err
		}
		fmt.Printf("%s: serving version %s from %s again.\n", arg(0), v.ID[:8], time.Unix(v.StoredAt, 0).Format("2 Jan 2006 15:04"))
	case "announce":
		if err := announcer(); err != nil {
			return err
		}
		n, err := pub.AnnounceAll(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Announced %d site(s).\n", n)
	case "remove":
		if fs.NArg() != 1 || !*yes {
			return errors.New("usage: yggstore site remove DOMAIN -yes (this deletes every version)")
		}
		if err := announcer(); err != nil {
			return err
		}
		if err := pub.Remove(ctx, site.Normalise(arg(0))); err != nil {
			return err
		}
		fmt.Printf("%s removed: web nodes stop serving it, and its versions are deleted.\n", arg(0))
	case "status":
		return siteStatus(ctx, *peersPath)
	default:
		fmt.Fprint(os.Stderr, siteUsage)
		os.Exit(2)
	}
	return nil
}

// onlinePeers is the member list's nodes that answer, which uploads use.
func onlinePeers(ctx context.Context, path string) ([]peers.Peer, error) {
	list, err := peers.Load(path)
	if err != nil {
		return nil, err
	}
	c := client.New()
	return peers.Online(ctx, list, func(ctx context.Context, p peers.Peer) error {
		_, err := c.Info(ctx, p.Addr)
		return err
	}), nil
}

func siteStatus(ctx context.Context, path string) error {
	list, err := peers.Load(path)
	if err != nil {
		return err
	}
	c := client.New()
	bySite := map[string][]string{}
	webNodes := 0
	for _, p := range list {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := c.Info(pctx, p.Addr)
		cancel()
		if err != nil || len(info.Web) == 0 {
			continue
		}
		webNodes++
		var st site.NodeStatus
		if json.Unmarshal(info.Web, &st) != nil {
			// older nodes report just the list of sites
			json.Unmarshal(info.Web, &st.Sites)
		}
		tunnel := "no tunnel"
		if st.Tunnel != "" {
			tunnel = "tunnel " + st.Tunnel
		}
		fmt.Printf("%-12s %s\n", p.Name, tunnel)
		sites := st.Sites
		for _, s := range sites {
			line := p.Name + " "
			switch {
			case s.Waiting != "" && s.Error != "":
				line += short(s.Serving) + ", can't fetch " + short(s.Waiting) + ": " + s.Error
			case s.Waiting != "":
				line += short(s.Serving) + ", fetching " + short(s.Waiting)
			default:
				line += short(s.Serving)
			}
			bySite[s.Site] = append(bySite[s.Site], line)
		}
	}
	if webNodes == 0 {
		fmt.Println("No web nodes answer. Start a node with -web 127.0.0.1:8480 to make it one.")
		return nil
	}
	names := make([]string, 0, len(bySite))
	for n := range bySite {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("\n%d web node(s).\n", webNodes)
	for _, n := range names {
		fmt.Printf("%s\n  %s\n", n, strings.Join(bySite[n], "\n  "))
	}
	return nil
}

func short(id string) string {
	if id == "" {
		return "nothing yet"
	}
	return id[:min(8, len(id))]
}
