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
  yggstore gateway customer add -name NAME [-email EMAIL] [-plan TEXT] [-quota GB] [-trial 14d]
  yggstore gateway customer list
  yggstore gateway customer link WHO          a new one-time link to their keys
  yggstore gateway customer show WHO          print their keys
  yggstore gateway customer suspend|resume|new-secret WHO
  yggstore gateway customer quota WHO GB
  yggstore gateway customer extend WHO 7d     a longer trial
  yggstore gateway customer paid WHO [-plan TEXT] [-quota GB]
  yggstore gateway customer close WHO -yes    delete the account's files
  yggstore gateway report   [-month YYYY-MM]

All take -dir (default ~/.yggstore/gateway). WHO is a customer's ID, access
key or name. Give -endpoint https://… once; it is remembered for links.
See docs/gateway.md.
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
	trial := fs.String("trial", "", `make it a free trial for this long, e.g. "14d" (default space 5 GB)`)
	endpoint := fs.String("endpoint", "", "the gateway's public address, e.g. https://s3.yourgroup.example (remembered)")
	keys := fs.Bool("keys", false, "print the keys themselves instead of a one-time link to them")
	yes := fs.Bool("yes", false, "confirm closing an account")
	// Flags may come before or after WHO.
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	cs := gateway.OpenCustomers(filepath.Join(*dir, "customers.json"))
	quotaBytes := int64(*quota * 1e9)
	arg := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}
	who := arg(0)
	needWho := func() error {
		if who == "" {
			return fmt.Errorf("say which customer: yggstore gateway customer %s WHO", sub)
		}
		return nil
	}
	if *endpoint != "" {
		if err := gateway.SetEndpoint(*dir, *endpoint); err != nil {
			return err
		}
	}
	ep := gateway.Endpoint(*dir)
	// welcome prints what to send: a one-time link, or with -keys the keys.
	welcome := func(c gateway.Customer) error {
		if *keys {
			printWelcome(c, ep)
			return nil
		}
		if ep == "" {
			return errors.New("give the gateway's public address once with -endpoint https://…, or use -keys to print the keys instead")
		}
		link, err := gateway.NewKeyLink(*dir, c.ID, ep)
		if err != nil {
			return err
		}
		printLinkWelcome(c, link)
		return nil
	}
	switch sub {
	case "add":
		var trialEnds int64
		if *trial != "" {
			d, err := parseDays(*trial)
			if err != nil {
				return err
			}
			trialEnds = time.Now().Add(d).Unix()
			if !set["quota"] {
				quotaBytes = gateway.DefaultTrialQuota
			}
			if *plan == "" {
				*plan = "free trial"
			}
		}
		c, err := cs.Add(*name, *email, *plan, quotaBytes)
		if err != nil {
			return err
		}
		if trialEnds != 0 {
			if c, err = cs.Update(c.ID, func(c *gateway.Customer) { c.TrialEnds = trialEnds }); err != nil {
				return err
			}
		}
		fmt.Printf("Added %s (%s): %s, %s.\n\n", c.Name, c.ID, c.Status(time.Now()), quotaText(c.QuotaBytes))
		return welcome(c)
	case "list":
		list, err := cs.List()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tName\tEmail\tPlan\tQuota\tAccess key\tStatus")
		for _, c := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, c.Email, c.Plan, quotaText(c.QuotaBytes), c.AccessKey, c.Status(time.Now()))
		}
		return tw.Flush()
	case "show", "link":
		if err := needWho(); err != nil {
			return err
		}
		c, err := cs.Find(who)
		if err != nil {
			return err
		}
		if sub == "show" {
			*keys = true
		}
		return welcome(c)
	case "suspend", "resume":
		if err := needWho(); err != nil {
			return err
		}
		c, err := cs.Update(who, func(c *gateway.Customer) { c.Suspended = sub == "suspend" })
		if err != nil {
			return err
		}
		fmt.Printf("%s is now %s. Their files are kept either way.\n", c.Name, c.Status(time.Now()))
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
		return welcome(c)
	case "quota":
		if err := needWho(); err != nil {
			return err
		}
		var gb float64
		if _, err := fmt.Sscan(arg(1), &gb); err != nil {
			return errors.New("usage: yggstore gateway customer quota WHO GB")
		}
		c, err := cs.Update(who, func(c *gateway.Customer) { c.QuotaBytes = int64(gb * 1e9) })
		if err != nil {
			return err
		}
		fmt.Printf("%s may now store %s.\n", c.Name, quotaText(c.QuotaBytes))
		return nil
	case "extend":
		if err := needWho(); err != nil {
			return err
		}
		d, err := parseDays(arg(1))
		if err != nil {
			return errors.New("usage: yggstore gateway customer extend WHO 7d")
		}
		c, err := cs.Find(who)
		if err != nil {
			return err
		}
		if c.TrialEnds == 0 {
			return fmt.Errorf("%s isn't on a trial", c.Name)
		}
		c, err = cs.Update(c.ID, func(c *gateway.Customer) {
			c.TrialEnds = time.Unix(max(c.TrialEnds, time.Now().Unix()), 0).Add(d).Unix()
		})
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s.\n", c.Name, c.Status(time.Now()))
		return nil
	case "paid":
		if err := needWho(); err != nil {
			return err
		}
		c, err := cs.Update(who, func(c *gateway.Customer) {
			c.TrialEnds = 0
			if set["plan"] {
				c.Plan = *plan
			} else if c.Plan == "free trial" {
				c.Plan = ""
			}
			if set["quota"] {
				c.QuotaBytes = quotaBytes
			}
		})
		if err != nil {
			return err
		}
		fmt.Printf("%s is now a paying customer (%s, %s). Their keys and files stay the same.\n", c.Name, orDash(c.Plan), quotaText(c.QuotaBytes))
		return nil
	case "close":
		if err := needWho(); err != nil {
			return err
		}
		c, err := cs.Find(who)
		if err != nil {
			return err
		}
		if !*yes {
			return fmt.Errorf("closing %s deletes all their files from the group; this can't be undone. Run again with -yes to go ahead", c.Name)
		}
		if _, err := cs.Update(c.ID, func(c *gateway.Customer) { c.Closed = time.Now().Unix() }); err != nil {
			return err
		}
		fmt.Printf("Closed %s. Their keys stop working now; the running gateway deletes their files within the hour (or when it next starts).\n", c.Name)
		return nil
	}
	fmt.Fprint(os.Stderr, gatewayUsage)
	os.Exit(2)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "no plan set"
	}
	return s
}

// parseDays reads "14d", "14" (days) or a Go duration like "36h".
func parseDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var n float64
	if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &n); err == nil && n > 0 && !strings.ContainsAny(strings.TrimSuffix(s, "d"), "hms") {
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("%q: give a number of days, such as 14d", s)
}

func quotaText(n int64) string { return gateway.QuotaText(n) }

// printWelcome prints what to send the customer.
func printWelcome(c gateway.Customer, endpoint string) {
	if endpoint == "" {
		endpoint = "https://s3.yourgroup.example (set it with -endpoint)"
	}
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

// printLinkWelcome prints a message to send, with a one-time link to the keys.
func printLinkWelcome(c gateway.Customer, link string) {
	what := "your storage account is ready"
	if c.TrialEnds != 0 {
		what = fmt.Sprintf("your free trial is ready (%s until %s)", quotaText(c.QuotaBytes), time.Unix(c.TrialEnds, 0).Format("2 Jan"))
	}
	fmt.Printf(`Send this to %s:

  Hi %s, %s.

  Open this link to get your keys. It works once, so save the keys
  somewhere safe, such as your password manager:

  %s

  The page explains how to set up rclone, Cyberduck or Duplicati.
  The link expires in 7 days.

(Make a new link with: yggstore gateway customer link %s)
`, c.Name, firstName(c.Name), what, link, c.ID)
}

func firstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return name
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
