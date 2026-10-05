package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/gateway"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/transport"
)

const gatewayUsage = `yggstore gateway: an S3 service for people who pay to use the group's storage

  yggstore gateway serve    [-listen :9000] [-domain DOMAIN] [-tls-cert FILE -tls-key FILE] [-trust-proxy]
  yggstore gateway customer add -name NAME [-email EMAIL] [-plan TEXT] -quota GB
  yggstore gateway customer list
  yggstore gateway customer show|suspend|resume|new-secret WHO
  yggstore gateway customer quota WHO GB
  yggstore gateway report   [-month YYYY-MM]

All take -dir (default ~/.yggstore/gateway). WHO is a customer's ID, access
key or name. See docs/gateway.md.
`

func defaultGatewayDir() string { return filepath.Join(yggstoreHome(), "gateway") }

func cmdGateway(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, gatewayUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "serve":
		return gatewayServe(ctx, args[1:])
	case "customer", "customers":
		return gatewayCustomer(args[1:])
	case "report":
		return gatewayReport(args[1:])
	}
	fmt.Fprint(os.Stderr, gatewayUsage)
	os.Exit(2)
	return nil
}

func gatewayServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gateway serve", flag.ExitOnError)
	dir := fs.String("dir", defaultGatewayDir(), "the gateway's accounts, index and metering")
	peersPath := fs.String("peers", defaultPeers(), "the group's member list"+peersHelp)
	pushed := fs.String("pushed", filepath.Join(defaultDataDir(), "peers.pushed.json"), "newer member list pushed by the admin, if this machine runs a node")
	listen := fs.String("listen", ":9000", "address customers connect to")
	domain := fs.String("domain", "", "also accept virtual-hosted requests at BUCKET.DOMAIN")
	region := fs.String("region", "us-east-1", "region reported to clients")
	cert := fs.String("tls-cert", "", "TLS certificate (or put a reverse proxy such as Caddy in front)")
	key := fs.String("tls-key", "", "TLS key")
	minMachines := fs.Int("min-machines", 3, "opted-in machines an upload needs; lower only for testing")
	trustProxy := fs.Bool("trust-proxy", false, "behind a reverse proxy: take client addresses from X-Forwarded-For")
	fs.Parse(args)

	t, err := transport.ByName("ygg")
	if err != nil {
		return err
	}
	ip, err := t.LocalIP()
	if err != nil {
		return fmt.Errorf("the gateway reaches the group over Yggdrasil: %w", err)
	}
	live, err := peers.NewLive(*peersPath, *pushed, ip.String())
	if err != nil {
		return err
	}
	go live.Watch(ctx, 10*time.Second, log.Printf)
	self := false
	for _, p := range live.List() {
		if p.IP() != ip.String() {
			continue
		}
		if !p.Gateway {
			// Nodes tell gateway uploads by address, so a member's own
			// uploads from here would be counted as customers'.
			return fmt.Errorf(`%s (%s) is also in the member list as a normal node; run the gateway on a machine (or container) with its own Yggdrasil address`, p.Name, ip)
		}
		self = true
	}
	if !self {
		return fmt.Errorf(`this machine (%s) must be in the member list with "gateway": true, so nodes know its uploads are customers'`, ip)
	}

	backend := &gateway.Group{Client: client.New(), Peers: live.List, MinMachines: *minMachines}
	if *minMachines < 3 {
		log.Printf("warning: -min-machines %d: uploads may sit on fewer than 3 machines and not survive one failing. For testing only.", *minMachines)
	}
	g, err := gateway.New(*dir, gateway.OpenCustomers(filepath.Join(*dir, "customers.json")), backend,
		gateway.Options{Domain: *domain, Region: *region, Peers: live.List, TrustProxy: *trustProxy, Log: log.Printf})
	if err != nil {
		return err
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(runCtx); close(done) }()

	srv := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	log.Printf("gateway serving S3 on %s://%s, storing in the group as %s", scheme, *listen, ip)
	if scheme == "http" && !*trustProxy {
		log.Printf("warning: plain HTTP; customers' keys and data cross the internet unencrypted unless a TLS proxy is in front")
	}
	if *cert != "" {
		err = srv.ListenAndServeTLS(*cert, *key)
	} else {
		err = srv.ListenAndServe()
	}
	stopRun()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func gatewayCustomer(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, gatewayUsage)
		os.Exit(2)
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("gateway customer "+sub, flag.ExitOnError)
	dir := fs.String("dir", defaultGatewayDir(), "the gateway's directory")
	name := fs.String("name", "", "customer's name")
	email := fs.String("email", "", "customer's email, for your records")
	plan := fs.String("plan", "", `plan, for your records, e.g. "100 GB, R50/month"`)
	quota := fs.Float64("quota", 0, "storage allowed, in GB (0 = unlimited)")
	endpoint := fs.String("endpoint", "https://s3.example.org", "the gateway's public address, for the welcome message")
	fs.Parse(args)
	cs := gateway.OpenCustomers(filepath.Join(*dir, "customers.json"))
	quotaBytes := int64(*quota * 1e9)

	who := fs.Arg(0)
	needWho := func() error {
		if who == "" {
			return fmt.Errorf("say which customer: yggstore gateway customer %s WHO", sub)
		}
		return nil
	}
	switch sub {
	case "add":
		c, err := cs.Add(*name, *email, *plan, quotaBytes)
		if err != nil {
			return err
		}
		fmt.Printf("Added %s (%s).\n\n", c.Name, c.ID)
		printWelcome(c, *endpoint)
		return nil
	case "list":
		list, err := cs.List()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tName\tEmail\tPlan\tQuota\tAccess key\tStatus")
		for _, c := range list {
			status := "active"
			if c.Suspended {
				status = "suspended"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, c.Email, c.Plan, quotaText(c.QuotaBytes), c.AccessKey, status)
		}
		return tw.Flush()
	case "show":
		if err := needWho(); err != nil {
			return err
		}
		c, err := cs.Find(who)
		if err != nil {
			return err
		}
		printWelcome(c, *endpoint)
		return nil
	case "suspend", "resume":
		if err := needWho(); err != nil {
			return err
		}
		c, err := cs.Update(who, func(c *gateway.Customer) { c.Suspended = sub == "suspend" })
		if err != nil {
			return err
		}
		fmt.Printf("%s is now %s. Their files are kept either way.\n", c.Name, map[bool]string{true: "suspended", false: "active"}[c.Suspended])
		return nil
	case "new-secret":
		if err := needWho(); err != nil {
			return err
		}
		c, err := cs.Update(who, func(c *gateway.Customer) { *c = gateway.WithNewSecret(*c) })
		if err != nil {
			return err
		}
		fmt.Printf("New secret for %s; the old one no longer works.\n\n", c.Name)
		printWelcome(c, *endpoint)
		return nil
	case "quota":
		if err := needWho(); err != nil {
			return err
		}
		var gb float64
		if _, err := fmt.Sscan(fs.Arg(1), &gb); err != nil {
			return errors.New("usage: yggstore gateway customer quota WHO GB")
		}
		c, err := cs.Update(who, func(c *gateway.Customer) { c.QuotaBytes = int64(gb * 1e9) })
		if err != nil {
			return err
		}
		fmt.Printf("%s may now store %s.\n", c.Name, quotaText(c.QuotaBytes))
		return nil
	}
	fmt.Fprint(os.Stderr, gatewayUsage)
	os.Exit(2)
	return nil
}

func quotaText(n int64) string {
	if n == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%g GB", float64(n)/1e9)
}

// printWelcome prints what to send the customer.
func printWelcome(c gateway.Customer, endpoint string) {
	fmt.Printf(`Send this to %s (keep the secret out of shared channels):

  Endpoint:    %s
  Access key:  %s
  Secret key:  %s
  Region:      us-east-1
  Space:       %s

  Any S3 program works: rclone, Cyberduck, Duplicati and others.
  Choose "S3 compatible" / "Other", use path-style addressing, and turn on
  the program's own encryption so only you can read your files.
  Setup guide: https://github.com/peterretief/yggstore/blob/main/docs/gateway.md#for-customers
`, c.Name, endpoint, c.AccessKey, c.Secret, quotaText(c.QuotaBytes))
}

func gatewayReport(args []string) error {
	fs := flag.NewFlagSet("gateway report", flag.ExitOnError)
	dir := fs.String("dir", defaultGatewayDir(), "the gateway's directory")
	month := fs.String("month", time.Now().UTC().Format("2006-01"), "month to report, YYYY-MM")
	fs.Parse(args)
	mo, err := gateway.LoadMonth(*dir, *month)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no metering for %s in %s", *month, *dir)
	}
	if err != nil {
		return err
	}
	list, err := gateway.OpenCustomers(filepath.Join(*dir, "customers.json")).List()
	if err != nil {
		return err
	}
	gateway.Report(os.Stdout, mo, list)
	if !strings.HasPrefix(time.Now().UTC().Format("2006-01"), *month) {
		return nil
	}
	fmt.Println("\nThe month isn't over: these are the figures so far.")
	return nil
}
