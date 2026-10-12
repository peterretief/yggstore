package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/syncthing"
	"github.com/peterretief/yggstore/internal/transport"
)

const hubUsage = `yggstore hub: a box such as a Raspberry Pi that the devices on its LAN
sync their files with, and a page showing how it is

  yggstore hub [-listen :7490] [-dir DIR] [-pair-limit 5] [-bin DIR] [-service]
  yggstore hub off | on          stop or start taking new devices
  yggstore hub pause | resume    stop or start syncing for every device
  yggstore hub users             the devices paired
  yggstore hub remove NAME       unpair a device (its files stay on the hub)
  yggstore hub sites             the websites kept here
  yggstore hub site add DOMAIN   keep a website here: synced with the devices,
                                 published whenever it changes
  yggstore hub site publish DOMAIN   publish it now, even if it looks broken
  yggstore hub site remove DOMAIN    stop syncing and publishing it

Devices pair through Syncthing, which must run on this box as the same user:
a device on the LAN that adds this box is accepted, and shares a folder
here, DIR/users/NAME. Files put in it are stored in the group and replaced
by stubs, on the device too. Only devices on the local network are accepted.
Each website is a folder of its own, DIR/sites/DOMAIN, shared with every
paired device; publish it from this box first (yggstore site publish).

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
	dir := fs.String("dir", hubDir(), "where the devices' folders (users/) and websites (sites/) are kept")
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
		if err := installUnits(map[string]string{"yggstore-hub.service": unit("yggstore hub on "+*listen, cmd)}); err != nil {
			return fmt.Errorf("the service could not be set up: %w\nStart it by hand:\n  %s", err, shellJoin(cmd))
		}
		fmt.Printf("The hub is running on %s.\n", *listen)
		return nil
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
		cfg.Users = hub.NewUsers(hub.UsersConfig{ST: syncthing.New(base, key), Dir: filepath.Join(*dir, "users"), StatePath: *statePath,
			Limit: *limit, Logf: log.Printf,
			Watch: func(ctx context.Context, u hub.User) {
				w := outbox.New(outbox.Config{Dir: u.Path, PeersPath: *peersPath, Client: wc, Identity: id,
					Event: func(kind, msg string) { log.Printf("%s: %s", u.Name, msg) }})
				if err := w.Run(ctx); err != nil && ctx.Err() == nil {
					log.Printf("%s: stopped storing files: %v", u.Name, err)
				}
			}})
		go cfg.Users.Run(ctx)

		pub := &site.Publisher{Dir: filepath.Join(yggstoreHome(), "sites"), Client: wc, Log: log.Printf, Skip: hub.SyncthingFile}
		sites := hub.NewSites(hub.SitesConfig{StatePath: *statePath, Pub: pub, Client: wc, MaxBytes: *siteMax << 20,
			Quiet: 15 * time.Second, Gap: time.Minute, Logf: log.Printf,
			Online: func(ctx context.Context) ([]peers.Peer, error) {
				token, err := msg.ReadToken(*msgToken)
				if err != nil {
					return nil, fmt.Errorf("this box's node's messaging token: %w", err)
				}
				pub.Announce = localAnnouncer{msg.Local{Addr: *msgAPI, Token: token}}
				return onlinePeers(ctx, *peersPath)
			}})
		go sites.Run(ctx)
	}

	h := hub.New(cfg)
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
		fmt.Printf("%-20s %-12s %-17s %s\n", "NAME", "PAIRED", "FROM", "FOLDER")
		for _, u := range st.Users {
			name := u.Name
			if u.Remove {
				name += " (removing)"
			}
			fmt.Printf("%-20s %-12s %-17s %s\n", name, time.Unix(u.Added, 0).Format("2006-01-02"), u.Address, u.Path)
		}
		return nil
	case cmd == "sites" && fs.NArg() == 1:
		st, err := hub.Load(*statePath)
		if err != nil {
			return err
		}
		if len(st.Sites) == 0 {
			fmt.Println("No websites yet (yggstore hub site add DOMAIN).")
			return nil
		}
		for _, si := range st.Sites {
			state := "not published from the hub yet"
			if si.Published > 0 && si.Version != "" {
				state = "published " + time.Unix(si.Published, 0).Format("2 Jan 15:04") + ", version " + si.Version
			}
			if si.Remove {
				state = "being removed"
			}
			fmt.Printf("%-30s %s\n  folder %s\n", si.Name, state, si.Path)
			if si.Problem != "" {
				fmt.Printf("  latest change not published: %s\n", si.Problem)
			}
		}
		return nil
	case cmd == "site" && fs.NArg() == 3:
		name := site.Normalise(fs.Arg(2))
		if err := site.ValidName(name); err != nil {
			return err
		}
		find := func(s *hub.HubState) (*hub.Site, error) {
			for i := range s.Sites {
				if s.Sites[i].Name == name {
					return &s.Sites[i], nil
				}
			}
			return nil, fmt.Errorf("no website %s here (see yggstore hub sites)", name)
		}
		switch fs.Arg(1) {
		case "add":
			path := filepath.Join(*dir, "sites", name)
			return set(func(s *hub.HubState) error {
				if si, _ := find(s); si != nil && !si.Remove {
					return fmt.Errorf("%s is kept here already, in %s", name, si.Path)
				}
				s.Sites = append(s.Sites, hub.Site{Name: name, Folder: hub.SiteFolderID(name), Path: path, Added: time.Now().Unix()})
				return nil
			}, fmt.Sprintf("%s is kept in %s. The hub fills it with the version being served, if this box published one,\n"+
				"shares it with every paired device, and publishes it whenever it changes.", name, path))
		case "publish":
			return set(func(s *hub.HubState) error {
				si, err := find(s)
				if err == nil {
					si.Force = true
				}
				return err
			}, name+" will be published as it is within a few seconds.")
		case "remove":
			return set(func(s *hub.HubState) error {
				si, err := find(s)
				if err == nil {
					si.Remove = true
				}
				return err
			}, name+" will no longer be synced or published from the hub. Its folder stays, and the site stays up\n"+
				"(yggstore site remove takes it off the web).")
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
