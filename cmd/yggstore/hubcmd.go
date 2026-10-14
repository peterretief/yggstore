package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/syncthing"
	"github.com/peterretief/yggstore/internal/transport"
	"github.com/peterretief/yggstore/internal/websites"
	"github.com/peterretief/yggstore/internal/workcopy"
)

const hubUsage = `yggstore hub: a box such as a Raspberry Pi that the devices on its LAN
sync their files with, and a page showing how it is

  yggstore hub [-listen :7490] [-dir DIR] [-pair-limit 5] [-bin DIR] [-service]
  yggstore hub off | on          stop or start taking new devices
  yggstore hub pause | resume    stop or start syncing for every device
  yggstore hub users             the devices paired, and whose they are
  yggstore hub people            the people, their devices and folders
  yggstore hub link DEVICE PERSON  make a device sync that person's folder
  yggstore hub teams             the team folders, and who is in each
  yggstore hub team add NAME PERSON...    a team folder for these people
  yggstore hub team join|leave NAME PERSON...
  yggstore hub team remove NAME  stop syncing a team folder (its files stay)
  yggstore hub conflicts         files two devices changed at once
  yggstore hub remove NAME       unpair a device (its files stay on the hub)
  yggstore hub sites             the websites kept here, in DIR/websites/
  yggstore hub site publish DOMAIN   publish it now, even if it looks broken

Devices pair through Syncthing, which must run on this box as the same user:
a device on the LAN that adds this box is accepted, and shares a folder
here, DIR/users/NAME. Files put in it are stored in the group and replaced
by stubs, on the device too. Only devices on the local network are accepted.
The box's websites are in DIR/websites/, a folder per domain, shared with
every paired device: a change there is published. A new site's folder is
published once its domain is on Cloudflare (see docs/sites.md).

The page (port 7490) shows this box's node and Syncthing ID, and puts a node
on the computer that opens it, with an invite. See docs/hub.md.
`

func cmdHub(ctx context.Context, args []string) error {
	if hubControl(args) {
		return cmdHubControl(ctx, args)
	}
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	listen := fs.String("listen", ":7490", "address for the page (the LAN must reach it)")
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	name := fs.String("name", hostname(), "this box's node, by name, if its address isn't found")
	binDir := fs.String("bin", "", "folder of more yggstore builds to hand out (yggstore-PLATFORM)")
	dir := fs.String("dir", hubDir(), "where the devices' folders (users/) and websites (websites/) are kept")
	statePath := fs.String("state", hubState(), "the hub's list of devices and its switches")
	limit := fs.Int("pair-limit", 5, "devices that may pair in a day (0 = no limit)")
	noSync := fs.Bool("no-sync", false, "don't pair devices through Syncthing")
	stConfig := fs.String("syncthing-config", "", "Syncthing's config.xml, for its address and API key (default: found)")
	stURL := fs.String("syncthing-url", "", "Syncthing's address, if not the one in its config (as with --gui-address)")
	keyPath := fs.String("sharing-key", filepath.Join(yggstoreHome(), "sharing.key"), "the sharing key files are stored with")
	siteMax := fs.Int64("site-max", 500, "the most a website may hold, in MB (0 = no limit)")
	msgAPI := fs.String("msg-api", "127.0.0.1:7401", "this box's node's local messaging API, to announce websites")
	msgToken := fs.String("msg-token", filepath.Join(yggstoreHome(), "msg.token"), "the node's messaging token file")
	service := fs.Bool("service", false, "install and start a user service for the hub, with these flags (Linux)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, hubUsage); fs.PrintDefaults() }
	fs.Parse(args)

	if *service {
		exe, _ := os.Executable()
		cmd := []string{exe, "hub"}
		for _, a := range args {
			if a != "-service" && a != "--service" {
				cmd = append(cmd, a)
			}
		}
		var after []string
		if !*noSync {
			after = []string{"syncthing.service"} // as packaged for Debian and Raspberry Pi OS
			if asRoot() {
				after = []string{"syncthing@root.service"}
			}
		}
		if err := installUnits(map[string]string{"yggstore-hub.service": unit("yggstore hub on "+*listen, cmd, after...)}); err != nil {
			return fmt.Errorf("the service could not be set up: %w\nStart it by hand:\n  %s", err, shellJoin(cmd))
		}
		fmt.Printf("The hub is running on %s.\n", *listen)
		return nil
	}

	// The group is the one the node on this machine is in: use its member
	// list and messaging token, unless told otherwise. With no node yet, the
	// member list is where join will put it, so the hub finds it the moment
	// it exists.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if node, ok := hub.FindLocalNode(); ok {
		if !given["peers"] {
			*peersPath = node.Peers
		}
		if !given["msg-token"] && node.MsgToken != "" {
			*msgToken = node.MsgToken
		}
		log.Printf("hub: storing through this machine's node (member list %s)", *peersPath)
	} else if _, err := os.Stat(*peersPath); err != nil && !given["peers"] {
		*peersPath = filepath.Join(yggstoreHome(), "peers.json")
	}

	bins := hub.Own()
	if *binDir != "" {
		if err := hub.FromDir(bins, *binDir); err != nil {
			return err
		}
	}
	self := ""
	if t, err := transport.ByName("ygg"); err == nil {
		if ip, err := t.LocalIP(); err == nil {
			self = ip.String()
		}
	}
	c := client.New()
	c.HTTP.Timeout = 5 * time.Second
	cfg := hub.Config{PeersPath: *peersPath, SelfAddr: self, SelfName: *name, Client: c, Bins: bins, Version: versionString()}

	if !*noSync {
		base, key, err := syncthing.ReadConfig(*stConfig)
		if err != nil {
			return fmt.Errorf("%v\nStart Syncthing on this box first (it makes its config), or run the hub with -no-sync", err)
		}
		if *stURL != "" {
			base = *stURL
		}
		id, err := share.LoadOrCreate(*keyPath)
		if err != nil {
			return fmt.Errorf("sharing key: %w", err)
		}
		wc := client.New()
		wc.HTTP.Timeout = 2 * time.Minute // uploads and restores move whole files
		pub := &site.Publisher{Dir: filepath.Join(yggstoreHome(), "sites"), Client: wc, Log: log.Printf, Skip: websites.Skip}
		cfToken, tunToken := filepath.Join(yggstoreHome(), "cloudflare-api.token"), filepath.Join(yggstoreHome(), "group-sites.token")
		sites := websites.New(websites.Config{Dir: filepath.Join(*dir, "websites"), StatePath: filepath.Join(*dir, "websites.json"),
			Pub: pub, Client: wc, MaxBytes: *siteMax << 20, Logf: log.Printf, TokenPath: cfToken,
			Online: func(ctx context.Context) ([]peers.Peer, error) {
				token, err := msg.ReadToken(*msgToken)
				if err != nil {
					return nil, fmt.Errorf("this box's node's messaging token: %w", err)
				}
				pub.Announce = localAnnouncer{msg.Local{Addr: *msgAPI, Token: token}}
				return onlinePeers(ctx, *peersPath)
			},
			Mine:    func() map[string]bool { return givenNames(*peersPath, self) },
			Served:  servedSites(*peersPath),
			Answers: site.Answers,
			Route: func(ctx context.Context, domain string) ([]string, error) {
				api, err := os.ReadFile(cfToken)
				if err != nil {
					return nil, websites.ErrNoToken
				}
				tun, err := os.ReadFile(tunToken)
				if err != nil {
					return nil, fmt.Errorf("can't read the tunnel token: %w", err)
				}
				added, wwwErr, err := site.AddRoutes(ctx, string(api), string(tun), domain, "http://localhost:8480")
				if wwwErr != nil {
					log.Printf("websites: %s: www.%s: %v", domain, domain, wwwErr)
				}
				return added, err
			}})
		cfg.Users = hub.NewUsers(hub.UsersConfig{ST: syncthing.New(base, key), Dir: filepath.Join(*dir, "users"), StatePath: *statePath,
			Limit: *limit, Logf: log.Printf, Websites: sites.Dir(), WebsitesReady: sites.Ready, Client: wc,
			PeersPath: *peersPath, Online: func(ctx context.Context) ([]peers.Peer, error) { return onlinePeers(ctx, *peersPath) },
			Bus: func() (workcopy.Bus, error) {
				token, err := msg.ReadToken(*msgToken)
				if err != nil {
					return nil, fmt.Errorf("this box's node's messaging token: %w", err)
				}
				return msg.Local{Addr: *msgAPI, Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}, nil
			},
			WebNodes:  func(ctx context.Context) []string { return webNodes(ctx, wc, *peersPath) },
			MailNodes: func(ctx context.Context) []string { return mailNodes(ctx, wc, *peersPath) },
			LinkHost:  func(ctx context.Context) string { return linkHost(ctx, wc, *peersPath) },
			SelfAddr:  selfAddr(self),
			Watch: func(ctx context.Context, p hub.Person) {
				w := outbox.New(outbox.Config{Dir: p.Path, PeersPath: *peersPath, Client: wc, Identity: id,
					Event: func(kind, msg string) { log.Printf("%s: %s", p.Name, msg) }})
				w.SetHold(func() bool { return cfg.Users.Holding(p.Name) })
				if err := w.Run(ctx); err != nil && ctx.Err() == nil {
					log.Printf("%s: stopped storing files: %v", p.Name, err)
				}
			}})
		go cfg.Users.Run(ctx)
		cfg.Work = cfg.Users.WorkHandler(func(ip string) bool {
			list, err := peers.Load(*peersPath)
			return err == nil && peers.AllowedIPs(list)[ip]
		})
		// The team folders' stubs are the box's to keep: a copy of them is
		// kept in the group, so losing the box loses no team's files.
		teams := recovery.New(recovery.Config{Dir: filepath.Join(*dir, "recovery"), Client: wc,
			Sources: []recovery.Source{{Name: "teams", Path: filepath.Join(*dir, "teams"), Keep: recovery.FoldersKeep}},
			Online:  func(ctx context.Context) ([]peers.Peer, error) { return onlinePeers(ctx, *peersPath) },
			Event:   func(kind, msg string) { log.Printf("teams: %s", msg) }})
		go func() {
			if err := teams.Run(ctx); err != nil {
				log.Printf("teams: recovery copy: %v", err)
			}
		}()
		go (&hub.Websites{W: sites, StatePath: *statePath, Logf: log.Printf}).Run(ctx)
	}

	// Joining, when an admin's dashboard adds this box: the same as
	// running yggstore join -service with the invite here.
	cfg.Join = func(ctx context.Context, invite string) (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		out, err := exec.CommandContext(ctx, exe, "join", "-service", "-name", *name, invite).CombinedOutput()
		log.Printf("hub: joined a group, as an admin's dashboard asked:\n%s", out)
		if err != nil {
			return string(out), fmt.Errorf("joining failed: %v", err)
		}
		return string(out), nil
	}

	h := hub.New(cfg)
	_, portStr, _ := net.SplitHostPort(*listen)
	port, _ := strconv.Atoi(portStr)
	if pc, err := net.ListenPacket("udp4", net.JoinHostPort("", portStr)); err != nil {
		log.Printf("hub: dashboards on the LAN won't find this box: %v", err)
	} else {
		go h.Answer(ctx, pc, port)
	}
	srv := &http.Server{Addr: *listen, Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("hub on %s, handing out yggstore for %v (others from %s)", *listen, slices.Sorted(maps.Keys(bins)), hub.Releases)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// hubControl is whether args are a command for the hub (off, users, ...)
// rather than flags to run it; those take only -state and -dir.
func hubControl(args []string) bool {
	for i := 0; i < len(args); i++ {
		switch a := strings.TrimPrefix(args[i], "-"); {
		case a == "-state" || a == "state" || a == "-dir" || a == "dir":
			i++ // its value
		case strings.HasPrefix(a, "state=") || strings.HasPrefix(a, "-state=") || strings.HasPrefix(a, "dir=") || strings.HasPrefix(a, "-dir="):
		default:
			return !strings.HasPrefix(args[i], "-")
		}
	}
	return false
}

func hubState() string { return filepath.Join(yggstoreHome(), "hub.json") }

func hubDir() string { return filepath.Join(homeDir(), "yggstore-hub") }

// cmdHubControl changes the hub's switches and devices. The running hub
// picks the change up within a few seconds.
func cmdHubControl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	statePath := fs.String("state", hubState(), "the hub's list of devices and its switches")
	dir := fs.String("dir", hubDir(), "where the devices' folders and websites are kept")
	fs.Usage = func() { fmt.Fprint(os.Stderr, hubUsage) }
	fs.Parse(reorder(args))
	cmd := fs.Arg(0)
	set := func(f func(*hub.HubState) error, done string) error {
		if _, err := hub.Update(ctx, *statePath, f); err != nil {
			return err
		}
		fmt.Println(done)
		return nil
	}
	switch {
	case cmd == "off" && fs.NArg() == 1:
		return set(func(s *hub.HubState) error { s.PairingOff = true; return nil },
			"The hub no longer takes new devices. Paired devices keep syncing (yggstore hub pause stops them).")
	case cmd == "on" && fs.NArg() == 1:
		return set(func(s *hub.HubState) error { s.PairingOff = false; return nil },
			"The hub takes new devices on the local network again.")
	case cmd == "pause" && fs.NArg() == 1:
		return set(func(s *hub.HubState) error { s.Paused = true; return nil },
			"Syncing is paused for every device. Files already on the hub are still stored.")
	case cmd == "resume" && fs.NArg() == 1:
		return set(func(s *hub.HubState) error { s.Paused = false; return nil }, "Syncing again.")
	case cmd == "users" && fs.NArg() == 1:
		st, err := hub.Load(*statePath)
		if err != nil {
			return err
		}
		pairing := "taking new devices"
		if st.PairingOff {
			pairing = "not taking new devices"
		}
		if st.Paused {
			pairing += ", syncing paused"
		}
		fmt.Printf("The hub is %s.\n", pairing)
		if len(st.Users) == 0 {
			fmt.Println("No devices yet.")
			return nil
		}
		fmt.Printf("%-20s %-16s %-12s %s\n", "DEVICE", "PERSON", "PAIRED", "FROM")
		for _, u := range st.Users {
			name := u.Name
			if u.Remove {
				name += " (removing)"
			}
			fmt.Printf("%-20s %-16s %-12s %s\n", name, u.Person, time.Unix(u.Added, 0).Format("2006-01-02"), u.Address)
		}
		return nil
	case cmd == "people" && fs.NArg() == 1:
		st, err := hub.Load(*statePath)
		if err != nil {
			return err
		}
		if len(st.People) == 0 {
			fmt.Println("Nobody yet: a person is added when their first device pairs.")
			return nil
		}
		for _, p := range st.People {
			var devs []string
			for _, d := range st.DevicesOf(p.Name) {
				devs = append(devs, d.Name)
			}
			list := strings.Join(devs, ", ")
			if list == "" {
				list = "no devices: the files stay on the box, unshared"
			}
			fmt.Printf("%-20s %s\n  folder %s\n", p.Name, list, p.Path)
		}
		return nil
	case cmd == "teams" && fs.NArg() == 1:
		st, err := hub.Load(*statePath)
		if err != nil {
			return err
		}
		if len(st.Teams) == 0 {
			fmt.Println("No teams yet: yggstore hub team add NAME PERSON...")
			return nil
		}
		for _, t := range st.Teams {
			fmt.Printf("%-20s %s\n  folder %s\n", t.Name, strings.Join(t.Members, ", "), t.Path)
		}
		return nil
	case cmd == "conflicts" && fs.NArg() == 1:
		st, err := hub.Load(*statePath)
		if err != nil {
			return err
		}
		cs := st.Conflicts()
		if len(cs) == 0 {
			fmt.Println("No conflicts.")
			return nil
		}
		fmt.Println("Files two devices changed at once. Both versions are kept: open both, keep one, and delete the other.")
		for _, c := range cs {
			fmt.Printf("  %-20s %s\n", c.Folder, c.File)
		}
		return nil
	case cmd == "team" && fs.NArg() >= 3:
		name, people := fs.Arg(2), fs.Args()[3:]
		check := func(s *hub.HubState) error {
			for _, p := range people {
				if _, ok := s.PersonNamed(p); !ok {
					return fmt.Errorf("nobody called %q (see yggstore hub people)", p)
				}
			}
			return nil
		}
		switch fs.Arg(1) {
		case "add":
			if strings.Trim(name, ". ") == "" || strings.ContainsAny(name, "/\\") {
				return fmt.Errorf("%q can't be a team's name", name)
			}
			return set(func(s *hub.HubState) error {
				if _, _, ok := s.TeamNamed(name); ok {
					return fmt.Errorf("there is a team %s already", name)
				}
				if err := check(s); err != nil {
					return err
				}
				var members []string
				for _, p := range people {
					person, _ := s.PersonNamed(p)
					members = append(members, person.Name)
				}
				s.Teams = append(s.Teams, hub.Team{Name: name, Folder: hub.TeamFolderID(name), Path: filepath.Join(*dir, "teams", name),
					Members: members, Added: time.Now().Unix()})
				return nil
			}, fmt.Sprintf("Team %s will be shared with its members' devices within a few seconds.", name))
		case "join", "leave":
			join := fs.Arg(1) == "join"
			return set(func(s *hub.HubState) error {
				_, i, ok := s.TeamNamed(name)
				if !ok {
					return fmt.Errorf("no team %q (see yggstore hub teams)", name)
				}
				if err := check(s); err != nil {
					return err
				}
				for _, p := range people {
					person, _ := s.PersonNamed(p)
					has := slices.Contains(s.Teams[i].Members, person.Name)
					switch {
					case join && !has:
						s.Teams[i].Members = append(s.Teams[i].Members, person.Name)
					case !join && has:
						s.Teams[i].Members = slices.DeleteFunc(s.Teams[i].Members, func(m string) bool { return m == person.Name })
					}
				}
				return nil
			}, "Done: devices are added or dropped within a few seconds.")
		case "remove":
			return set(func(s *hub.HubState) error {
				_, i, ok := s.TeamNamed(name)
				if !ok {
					return fmt.Errorf("no team %q (see yggstore hub teams)", name)
				}
				s.Teams[i].Remove = true
				return nil
			}, fmt.Sprintf("Team %s will stop syncing within a few seconds; its files stay on the box.", name))
		}
	case cmd == "link" && fs.NArg() == 3:
		dev, person := fs.Arg(1), fs.Arg(2)
		return set(func(s *hub.HubState) error {
			p, ok := s.PersonNamed(person)
			if !ok {
				return fmt.Errorf("nobody called %q (see yggstore hub people)", person)
			}
			for i, u := range s.Users {
				if strings.EqualFold(u.Name, dev) || u.Device == dev {
					s.Users[i].Person = p.Name
					return nil
				}
			}
			return fmt.Errorf("no device %q (see yggstore hub users)", dev)
		}, fmt.Sprintf("%s will sync %s's folder within a few seconds. The folder it synced before stays on the box.", dev, person))
	case cmd == "sites" && fs.NArg() == 1:
		st, err := hub.Load(*statePath)
		if err != nil {
			return err
		}
		if len(st.Sites) == 0 {
			fmt.Printf("No websites yet. Put one in %s, a folder named after its domain.\n", filepath.Join(*dir, "websites"))
			return nil
		}
		for _, si := range st.Sites {
			state := "not published from the hub yet"
			if si.Published > 0 && si.Version != "" {
				state = "published " + time.Unix(si.Published, 0).Format("2 Jan 15:04") + ", version " + si.Version
			}
			fmt.Printf("%-30s %s\n  folder %s\n", si.Name, state, filepath.Join(*dir, "websites", si.Name))
			if si.Problem != "" {
				fmt.Printf("  not published: %s\n", strings.SplitN(si.Problem, "\n", 2)[0])
			}
		}
		return nil
	case cmd == "site" && fs.NArg() == 3:
		name := site.Normalise(fs.Arg(2))
		if err := site.ValidName(name); err != nil {
			return err
		}
		switch fs.Arg(1) {
		case "add":
			return fmt.Errorf("websites are folders now: put %s in %s (publish it from this box first if it is already online elsewhere)",
				name, filepath.Join(*dir, "websites", name))
		case "publish":
			return set(func(s *hub.HubState) error {
				for i := range s.Sites {
					if s.Sites[i].Name == name {
						s.Sites[i].Force = true
						return nil
					}
				}
				return fmt.Errorf("no website %s here (see yggstore hub sites)", name)
			}, name+" will be published as it is within a few seconds.")
		case "remove":
			return fmt.Errorf("a website stays while its folder is in %s; yggstore site remove %s takes it off the web",
				filepath.Join(*dir, "websites"), name)
		}
	case cmd == "remove" && fs.NArg() == 2:
		return set(func(s *hub.HubState) error {
			for i, u := range s.Users {
				if strings.EqualFold(u.Name, fs.Arg(1)) || u.Device == fs.Arg(1) {
					s.Users[i].Remove = true
					return nil
				}
			}
			return fmt.Errorf("no device %q (see yggstore hub users)", fs.Arg(1))
		}, fmt.Sprintf("%s will be unpaired within a few seconds; its files stay on the hub.", fs.Arg(1)))
	}
	fs.Usage()
	return errors.New("unknown hub command")
}

// givenNames is the names the group gave this node (its entry's Names), which
// it publishes without the Cloudflare check.
func givenNames(peersPath, self string) map[string]bool {
	out := map[string]bool{}
	list, err := peers.Load(peersPath)
	if err != nil {
		return out
	}
	for _, p := range list {
		if self != "" && p.IP() == self {
			for _, n := range p.Names {
				out[n] = true
			}
		}
	}
	return out
}

// servedSites returns what the group's web nodes serve, asking them at most
// once a minute.
func servedSites(peersPath string) func() map[string]bool {
	var mu sync.Mutex
	var at time.Time
	have := map[string]bool{}
	return func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < time.Minute {
			return have
		}
		at = time.Now()
		list, err := peers.Load(peersPath)
		if err != nil {
			return have
		}
		c := client.New()
		got := map[string]bool{}
		for _, p := range list {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			info, err := c.Info(ctx, p.Addr)
			cancel()
			if err != nil || len(info.Web) == 0 {
				continue
			}
			var st site.NodeStatus
			if json.Unmarshal(info.Web, &st) != nil {
				continue
			}
			for _, s := range st.Sites {
				got[s.Site] = true
			}
		}
		have = got
		return have
	}
}

// selfAddr is the box's node address for its recovery records: only its
// host matters there.
func selfAddr(ip string) string {
	if ip == "" {
		return ""
	}
	return net.JoinHostPort(ip, "7400")
}

// webNodes are the members that serve the group's websites and answer now.
func webNodes(ctx context.Context, c client.Client, peersPath string) []string {
	list, err := onlinePeers(ctx, peersPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range list {
		ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := c.Info(ictx, p.Addr)
		cancel()
		if err == nil && len(info.Web) > 0 {
			out = append(out, p.Addr)
		}
	}
	return out
}

// mailNodes are the web nodes that send the group's email.
func mailNodes(ctx context.Context, c client.Client, peersPath string) []string {
	var out []string
	for _, n := range webNodes(ctx, c, peersPath) {
		ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := c.Info(ictx, n)
		cancel()
		if err == nil && info.MailOut {
			out = append(out, n)
		}
	}
	return out
}

// linkHost is a site the group serves, for people's links: the admin's
// first name if it has one, else any site being served.
func linkHost(ctx context.Context, c client.Client, peersPath string) string {
	if list, err := peers.Load(peersPath); err == nil {
		for _, p := range list {
			if p.Admin && len(p.Names) > 0 {
				return p.Names[0]
			}
		}
	}
	for _, n := range webNodes(ctx, c, peersPath) {
		ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := c.Info(ictx, n)
		cancel()
		var st site.NodeStatus
		if err == nil && json.Unmarshal(info.Web, &st) == nil {
			for _, s := range st.Sites {
				if s.Serving != "" {
					return s.Site
				}
			}
		}
	}
	return ""
}
