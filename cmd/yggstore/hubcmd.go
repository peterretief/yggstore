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
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/syncthing"
	"github.com/peterretief/yggstore/internal/transport"
)

const hubUsage = `yggstore hub: a box such as a Raspberry Pi that the devices on its LAN
sync their files with, and a page showing how it is

  yggstore hub [-listen :7490] [-users DIR] [-pair-limit 5] [-bin DIR] [-service]
  yggstore hub off | on          stop or start taking new devices
  yggstore hub pause | resume    stop or start syncing for every device
  yggstore hub users             the devices paired
  yggstore hub remove NAME       unpair a device (its files stay on the hub)

Devices pair through Syncthing, which must run on this box as the same user:
a device on the LAN that adds this box is accepted, and shares a folder
here, DIR/NAME. Files put in it are stored in the group and replaced by
stubs, on the device too. Only devices on the local network are accepted.

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
	usersDir := fs.String("users", filepath.Join(homeDir(), "yggstore-hub"), "where paired devices' folders are kept")
	statePath := fs.String("state", hubState(), "the hub's list of devices and its switches")
	limit := fs.Int("pair-limit", 5, "devices that may pair in a day (0 = no limit)")
	noSync := fs.Bool("no-sync", false, "don't pair devices through Syncthing")
	stConfig := fs.String("syncthing-config", "", "Syncthing's config.xml, for its address and API key (default: found)")
	stURL := fs.String("syncthing-url", "", "Syncthing's address, if not the one in its config (as with --gui-address)")
	keyPath := fs.String("sharing-key", filepath.Join(yggstoreHome(), "sharing.key"), "the sharing key files are stored with")
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
		cfg.Users = hub.NewUsers(hub.UsersConfig{ST: syncthing.New(base, key), Dir: *usersDir, StatePath: *statePath,
			Limit: *limit, Logf: log.Printf,
			Watch: func(ctx context.Context, u hub.User) {
				w := outbox.New(outbox.Config{Dir: u.Path, PeersPath: *peersPath, Client: wc, Identity: id,
					Event: func(kind, msg string) { log.Printf("%s: %s", u.Name, msg) }})
				if err := w.Run(ctx); err != nil && ctx.Err() == nil {
					log.Printf("%s: stopped storing files: %v", u.Name, err)
				}
			}})
		go cfg.Users.Run(ctx)
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
// rather than flags to run it; those take only -state.
func hubControl(args []string) bool {
	for i := 0; i < len(args); i++ {
		switch a := strings.TrimPrefix(args[i], "-"); {
		case a == "-state" || a == "state":
			i++ // its value
		case strings.HasPrefix(a, "state=") || strings.HasPrefix(a, "-state="):
		default:
			return !strings.HasPrefix(args[i], "-")
		}
	}
	return false
}

func hubState() string { return filepath.Join(yggstoreHome(), "hub.json") }

// cmdHubControl changes the hub's switches and devices. The running hub
// picks the change up within a few seconds.
func cmdHubControl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	statePath := fs.String("state", hubState(), "the hub's list of devices and its switches")
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
