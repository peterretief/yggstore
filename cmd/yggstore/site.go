package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
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
                                                defaults to the folder's name); with a
                                                Cloudflare API token it also adds the
                                                site's tunnel route and DNS (and www.)
  yggstore site list                            sites published from this machine
  yggstore site versions DOMAIN                 a site's versions, newest first
  yggstore site rollback DOMAIN [VERSION]       serve an earlier version (default: the one before)
  yggstore site announce                        tell web nodes about every site again
  yggstore site remove DOMAIN -yes              stop serving it and delete every version
  yggstore site status                          which web nodes serve which version

Publishing needs this machine's node running (it announces through its
messaging). Web nodes are nodes started with -web. See docs/sites.md.
Options: -peers FILE, -api 127.0.0.1:7401, -token ~/.yggstore/msg.token,
-cloudflare-token ~/.yggstore/cloudflare-api.token,
-tunnel-token ~/.yggstore/group-sites.token, -no-route, -no-wait.
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
	cfToken := fs.String("cloudflare-token", filepath.Join(yggstoreHome(), "cloudflare-api.token"), "Cloudflare API token file, for adding routes")
	tunToken := fs.String("tunnel-token", filepath.Join(yggstoreHome(), "group-sites.token"), "the web nodes' tunnel token file")
	service := fs.String("service", "http://localhost:8480", "where the web nodes serve, as the tunnel reaches them")
	noRoute := fs.Bool("no-route", false, "don't add Cloudflare routes")
	noWait := fs.Bool("no-wait", false, "don't wait for the web nodes and the public address")
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
		fmt.Println(").")
		if *noWait {
			fmt.Println("Web nodes switch to it within seconds; check with: yggstore site status")
		} else {
			waitServed(ctx, *peersPath, domain, v.ID)
		}
		routed := false
		if !*noRoute {
			routed = addRoutes(ctx, domain, *cfToken, *tunToken, *service)
		}
		if routed && !*noWait {
			waitPublic(ctx, domain)
		}
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

// waitServed waits until every web node serves version id of domain.
func waitServed(ctx context.Context, peersPath, domain, id string) {
	list, err := peers.Load(peersPath)
	if err != nil {
		return
	}
	c := client.New()
	deadline := time.Now().Add(90 * time.Second)
	for {
		var have, lacking []string
		for _, p := range list {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			info, err := c.Info(pctx, p.Addr)
			cancel()
			if err != nil || len(info.Web) == 0 {
				continue
			}
			var st site.NodeStatus
			if json.Unmarshal(info.Web, &st) != nil {
				json.Unmarshal(info.Web, &st.Sites)
			}
			ok := false
			for _, s := range st.Sites {
				ok = ok || (s.Site == domain && s.Serving == id)
			}
			if ok {
				have = append(have, p.Name)
			} else {
				lacking = append(lacking, p.Name)
			}
		}
		if len(lacking) == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			switch {
			case len(have)+len(lacking) == 0:
				fmt.Println("No web nodes answer. Start a node with -web 127.0.0.1:8480 to make it one.")
			case len(lacking) == 0:
				fmt.Printf("Served by %s.\n", strings.Join(have, ", "))
			default:
				fmt.Printf("Served by %s; not yet by %s (see yggstore site status).\n", orNone(strings.Join(have, ", ")), strings.Join(lacking, ", "))
			}
			return
		}
		time.Sleep(time.Second)
	}
}

// addRoutes makes domain (and www.domain, for a bare domain) reach the web
// nodes through Cloudflare. It reports whether the routes are in place.
func addRoutes(ctx context.Context, domain, cfTokenPath, tunTokenPath, service string) bool {
	manual := func(why string) bool {
		fmt.Printf("\n%s. To put it online, add a published application route for %s\n"+
			"(service HTTP, URL %s) to the web nodes' tunnel in the Cloudflare dashboard,\n"+
			"or save an API token as %s and publish again (see docs/sites.md).\n",
			why, domain, strings.TrimPrefix(service, "http://"), cfTokenPath)
		return false
	}
	apiTok, err := os.ReadFile(cfTokenPath)
	if err != nil {
		return manual("No Cloudflare API token")
	}
	tunTok, err := os.ReadFile(tunTokenPath)
	if err != nil {
		return manual("Can't read the tunnel token " + tunTokenPath)
	}
	account, tunnel, err := site.TunnelFromToken(string(tunTok))
	if err != nil {
		return manual(tunTokenPath + " doesn't hold a tunnel token")
	}
	cf := &site.Cloudflare{APIToken: strings.TrimSpace(string(apiTok)), Account: account, Tunnel: tunnel}
	hosts := []string{domain}
	if _, zone, err := cf.Zone(ctx, domain); err == nil && zone == domain {
		hosts = append(hosts, "www."+domain)
	}
	ok := true
	for _, h := range hosts {
		added, err := cf.Route(ctx, h, service)
		if err != nil {
			fmt.Printf("\nCloudflare: %v\n", err)
			ok = ok && h != domain // a www that can't be added doesn't stop the site
			continue
		}
		for _, a := range added {
			fmt.Printf("Added %s.\n", a)
		}
	}
	return ok
}

// waitPublic waits for https://domain/ to answer through Cloudflare.
func waitPublic(ctx context.Context, domain string) {
	c := http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(2 * time.Minute)
	last := ""
	for time.Now().Before(deadline) && ctx.Err() == nil {
		resp, err := c.Get("https://" + domain + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fmt.Printf("Online: https://%s/\n", domain)
				return
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		time.Sleep(3 * time.Second)
	}
	fmt.Printf("https://%s/ doesn't answer yet (%s); new DNS names can take a few minutes.\n", domain, last)
}

func short(id string) string {
	if id == "" {
		return "nothing yet"
	}
	return id[:min(8, len(id))]
}
