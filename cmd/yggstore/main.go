// yggstore: proof-of-concept sharded file storage over Yggdrasil.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/dashboard"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/invite"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/mesh"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/repair"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/transport"
	"github.com/peterretief/yggstore/internal/yggnet"
)

const usage = `yggstore: sharded, encrypted file storage over Yggdrasil

  yggstore join    [-name NODE] [-me NAME] [-quota GB] [-service] INVITE
                                                           join a group with an invite from its dashboard
  yggstore id      [-transport ygg|loopback] [-port 7400]   print this node's ID and peers.json entry
  yggstore serve   -peers peers.json [-data DIR] [-port 7400] [-name NAME] [-quota GB] [-transport ygg|builtin]
  yggstore status  -peers peers.json                       show which peers answer
  yggstore put     -peers peers.json FILE                  shard FILE, write FILE.ystub
  yggstore get     [-o OUT] FILE.ystub                     rebuild the file from its stub
  yggstore verify  FILE.ystub                              challenge every shard holder
  yggstore rm      FILE.ystub                              delete the item from all nodes, then its stub
  yggstore history FILE.ystub | -folder PATH -at DATE      older versions; restore one, or a folder as it was
  yggstore dashboard -peers peers.json [-stubs DIR | -outfiles DIR [-keep]] [-listen 127.0.0.1:7480]
                   [-test-peers FILE -test-stubs DIR]   also show a test cluster, separately
                                                           live status page; with -outfiles it also
                                                           runs the outbox watcher (see watch)
  yggstore msg     send|pub|sub|unsub|read|status ...      message other nodes (see docs/messaging.md)
  yggstore site    publish|list|versions|rollback|announce|remove|status ...
                                                           host static websites on the group (see docs/sites.md)
  yggstore mail    address|token|list|read|send ...        the group's email (see docs/mail.md)
  yggstore repair  [-after 24h] [-outfiles DIR]            rebuild shards of nodes down that long (see docs/repair.md)
  yggstore mesh    [set NODE URI...]                        Yggdrasil links between members (see docs/mesh.md)
  yggstore gateway serve|customer|report ...               S3 service for paying customers (see docs/gateway.md)
  yggstore watch   -peers peers.json -dir DIR [-keep]      shard anything dropped into DIR (replacing it
                                                           with a .ystub); restore stubs dropped into
                                                           DIR/restore/ into DIR/restored/
  yggstore version                                         which version this is
`

// version is set by release builds (-ldflags "-X main.version=v1.2.3").
var version = "dev"

// versionString is the release, or for other builds the commit they were
// built from, if Go recorded it.
func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if len(rev) >= 12 {
			rev = rev[:12]
			if dirty {
				rev += "+changes"
			}
			return "dev (" + rev + ")"
		}
	}
	return version
}

// defaultYggPeers are public Yggdrasil peers put in invites, so a newcomer
// has somewhere to connect.
const defaultYggPeers = "tls://91.98.161.68:9001?key=0e638944bfd6b277fa5e0dddbeb4444778eea8bece63a9862c661797022a8f05,tls://37.205.14.171:993?key=0009e16b9e3afe7b13c3612560410434d3dfc70c8a8a0a63e51e0470cb8124f6,tls://5.252.118.13:443"

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

const peersHelp = " (default: $YGGSTORE_PEERS, else ./peers.json, else /etc/yggstore/peers.json)"

// defaultPeers finds the peer list when -peers is not given, so commands
// work from any directory on a machine with a system-wide copy.
func defaultPeers() string {
	if p := os.Getenv("YGGSTORE_PEERS"); p != "" {
		return p
	}
	for _, p := range []string{"peers.json", "/etc/yggstore/peers.json"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "peers.json"
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Requests to other nodes go over whichever Yggdrasil this machine has.
	yggnet.Install()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "id":
		err = cmdID(args)
	case "serve":
		err = cmdServe(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "put":
		err = cmdPut(ctx, args)
	case "get":
		err = cmdGet(ctx, args)
	case "verify":
		err = cmdVerify(ctx, args)
	case "rm":
		err = cmdRm(ctx, args)
	case "dashboard":
		err = cmdDashboard(ctx, args)
	case "watch":
		err = cmdWatch(ctx, args)
	case "join":
		err = cmdJoin(ctx, args)
	case "gateway":
		err = cmdGateway(ctx, args)
	case "msg":
		err = cmdMsg(ctx, args)
	case "site":
		err = cmdSite(ctx, args)
	case "mesh":
		err = cmdMesh(ctx, args)
	case "mail":
		err = cmdMail(ctx, args)
	case "repair":
		err = cmdRepair(ctx, args)
	case "history":
		err = cmdHistory(ctx, args)
	case "version", "-version", "--version":
		fmt.Println("yggstore", versionString())
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("yggstore %s: %v", cmd, err)
	}
}

func cmdID(args []string) error {
	fs := flag.NewFlagSet("id", flag.ExitOnError)
	tname := fs.String("transport", "ygg", "ygg or loopback")
	port := fs.Int("port", 7400, "shard server port")
	name := fs.String("name", hostname(), "node name")
	fs.Parse(args)
	t, err := transport.ByName(*tname)
	if err != nil {
		return err
	}
	ip, err := t.LocalIP()
	if err != nil {
		return err
	}
	fmt.Printf("node ID: %s\n", ip)
	fmt.Printf("peers.json entry: {\"name\": %q, \"addr\": %q}\n", *name, net.JoinHostPort(ip.String(), strconv.Itoa(*port)))
	return nil
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	tname := fs.String("transport", "ygg", "ygg (the Yggdrasil daemon), builtin (Yggdrasil inside yggstore: no daemon, TUN or root; see docs/builtin.md), or loopback")
	port := fs.Int("port", 7400, "shard server port")
	dataDir := fs.String("data", defaultDataDir(), "where this node keeps other peers' shards")
	peersPath := fs.String("peers", defaultPeers(), "allow-list of peers"+peersHelp)
	name := fs.String("name", hostname(), "node name")
	quotaGB := fs.Float64("quota", 10, "storage quota in GB (0 = unlimited)")
	invites := fs.String("invites", filepath.Join(yggstoreHome(), "invites.json"), "invites made on this machine's dashboard (admin nodes only)")
	contactsPath := fs.String("contacts", filepath.Join(yggstoreHome(), "contacts.json"), "where people who join are added as contacts")
	keyPath := fs.String("sharing-key", filepath.Join(yggstoreHome(), "sharing.key"), "your sharing key")
	msgAPI := fs.String("msg-api", "127.0.0.1:7401", `local messaging API for programs on this machine ("" for none)`)
	msgToken := fs.String("msg-token", filepath.Join(yggstoreHome(), "msg.token"), "token file for the local messaging API")
	customers := fs.Bool("customers", false, "also hold paying customers' files, stored through the group's gateway (it earns you credit)")
	webAddr := fs.String("web", "", `serve the group's websites on this address, e.g. 127.0.0.1:8480 ("" for none; see docs/sites.md)`)
	webTunnel := fs.String("web-tunnel", "", "a Cloudflare Tunnel token file: run its connector while this web node serves (see docs/sites.md)")
	cloudflared := fs.String("cloudflared", "cloudflared", "the cloudflared program, for -web-tunnel")
	mailIn := fs.String("mail-in", "", "take the group's email from the mail Worker; the file holds its token (needs -web; see docs/mail.md)")
	mailbox := fs.String("mailbox", filepath.Join(yggstoreHome(), "mail"), `where your email is collected ("" for none; see docs/mail.md)`)
	mailOut := fs.String("mail-out", "", "send members' email through the SMTP relay in this file (see docs/mail.md)")
	yggAdmin := fs.String("ygg-admin", "auto", `Yggdrasil's admin socket, for linking to other members ("none" to leave Yggdrasil's links alone)`)
	yggKey := fs.String("ygg-key", filepath.Join(yggstoreHome(), "ygg.key"), "builtin: the node's Yggdrasil key, made if missing (a yggdrasil.conf works too, keeping that machine's address)")
	yggPeers := fs.String("ygg-peers", defaultYggPeers, "builtin: comma-separated Yggdrasil peers to link to")
	yggListen := fs.String("ygg-listen", "", "builtin: comma-separated URIs to take Yggdrasil links on, e.g. tls://0.0.0.0:9001")
	yggLAN := fs.Bool("ygg-lan", true, "builtin: find and link to Yggdrasil nodes on the local network")
	yggProxy := fs.String("ygg-proxy", yggnet.ProxyAddr(), `builtin: where the dashboard and commands on this machine reach other nodes through this one ("" for none)`)
	fs.Parse(args)

	t, err := transport.ByName(*tname)
	if err != nil {
		return err
	}
	builtin := *tname == "builtin" || *tname == "embedded"
	var ip net.IP
	var node *yggnet.Node
	if builtin {
		if node, err = startBuiltin(*yggKey, *yggPeers, *yggListen, *yggLAN, *port); err != nil {
			return err
		}
		defer node.Close()
		ip = node.Addr()
		if *yggProxy != "" {
			proxy := &http.Server{Addr: *yggProxy, Handler: node.Proxy(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-ctx.Done()
				proxy.Close()
			}()
			go func() {
				if err := proxy.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Printf("the dashboard and commands on this machine can't reach the group: %v", err)
				}
			}()
		}
	} else if ip, err = t.LocalIP(); err != nil {
		return err
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	// An admin node can push a newer list; it is kept next to the shards.
	live, err := peers.NewLive(*peersPath, filepath.Join(*dataDir, "peers.pushed.json"), ip.String())
	if err != nil {
		return err
	}
	go live.Watch(ctx, 10*time.Second, log.Printf)

	engine, err := msg.Open(filepath.Join(*dataDir, "msg"), ip.String(), live.List, msg.NewClient(), log.Printf)
	if err != nil {
		return fmt.Errorf("messaging: %w", err)
	}
	go engine.Run(ctx)
	// The local API is only for programs on this machine; a node without a
	// home folder (a service user on a storage box) still serves without it.
	token := ""
	if *msgAPI != "" {
		if token, err = msg.LoadOrCreateToken(*msgToken); err != nil {
			log.Printf("local messaging API not started: no token file (%v); give -msg-token a writable path to use it", err)
		}
	}
	if token != "" {
		local := &http.Server{Addr: *msgAPI, Handler: engine.LocalHandler(token), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			local.Close()
		}()
		go func() {
			if err := local.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("local messaging API not available on %s: %v", *msgAPI, err)
			}
		}()
	}

	var meshStatus func() mesh.Status
	var ygg mesh.Yggdrasil
	switch {
	case builtin:
		ygg = mesh.Builtin{Node: node}
	case *yggAdmin != "none" && *tname == "ygg":
		ygg = mesh.Find(*yggAdmin)
	}
	if ygg != nil {
		m := mesh.New(ygg, ip.String(), live.List, filepath.Join(*dataDir, "mesh.json"), log.Printf)
		go m.Run(ctx, time.Minute)
		meshStatus = m.Status
	}

	mailNode := &mail.Node{Dir: filepath.Join(*dataDir, "mail"), Self: ip.String(), Msgs: engine, Peers: live.List, Client: client.New(), Log: log.Printf}
	if *mailbox != "" {
		if err := os.MkdirAll(*mailbox, 0o700); err != nil {
			log.Printf("mail: not collecting email: %v", err)
		} else {
			mailNode.Box = *mailbox
		}
	}
	if *mailIn != "" {
		if *webAddr == "" {
			return errors.New("-mail-in needs -web")
		}
		b, err := os.ReadFile(*mailIn)
		if err != nil {
			return fmt.Errorf("-mail-in: %w", err)
		}
		if mailNode.Token = strings.TrimSpace(string(b)); len(mailNode.Token) < 32 {
			return errors.New("-mail-in: the token is too short (make one with: yggstore mail token FILE)")
		}
	}
	if err := mailNode.Start(); err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	go mailNode.Run(ctx)

	var relay *mail.Relay
	if *mailOut != "" {
		// A mistake here stops only sending, not the node.
		if relay, err = mail.LoadRelay(*mailOut); err != nil {
			log.Printf("mail: not sending members' email: -mail-out: %v", err)
			relay = nil
		} else {
			relay.Log = log.Printf
			log.Printf("mail: sending members' email through %s", relay.SMTP)
		}
	}

	var webStatus func() json.RawMessage
	if *webAddr != "" {
		web := &site.Web{Dir: filepath.Join(*dataDir, "web"), Msgs: engine, Peers: live.List, Client: client.New(), Log: log.Printf}
		if mailNode.Token != "" {
			web.Mail = mailNode
		}
		go func() {
			if err := web.Run(ctx); err != nil {
				log.Printf("web: %v", err)
			}
		}()
		ws := &http.Server{Addr: *webAddr, Handler: web, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			ws.Close()
		}()
		go func() {
			log.Printf("serving the group's websites on http://%s", *webAddr)
			if err := ws.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("web: %v", err)
			}
		}()
		var tun *site.Tunnel
		if *webTunnel != "" {
			tun = &site.Tunnel{Bin: *cloudflared, TokenFile: *webTunnel, Dir: filepath.Join(*dataDir, "web"), WebAddr: *webAddr, Log: log.Printf}
			if err := tun.Check(); err != nil {
				log.Printf("tunnel: not started: %v", err)
				tun = nil
			} else {
				go tun.Run(ctx)
			}
		}
		webStatus = func() json.RawMessage {
			st := site.NodeStatus{Sites: web.Status()}
			if tun != nil {
				st.Tunnel = tun.State()
			}
			b, _ := json.Marshal(st)
			return b
		}
	} else if *webTunnel != "" {
		return errors.New("-web-tunnel needs -web")
	}

	store := localstore.WithQuota(*dataDir, int64(*quotaGB*(1<<30)))
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(*port))
	handler := server.Handler(store, server.Options{
		Name: *name, NodeID: ip.String(), Transport: t, Peers: live, Customers: *customers, Messages: engine, Mesh: meshStatus, Web: webStatus,
		MailOut: mailOutHandler(relay),
		Join: func(caller string, req invite.Request) (invite.Response, error) {
			own := ""
			if id, err := share.Load(*keyPath); err == nil {
				own = id.Code()
			}
			acc := invite.Acceptor{InvitesPath: *invites, PeersPath: *peersPath, ContactsPath: *contactsPath, OwnCode: own,
				Refresh: func() { live.Refresh() }}
			resp, err := acc.Accept(caller, req)
			if err == nil {
				log.Printf("%s joined as %s (%s)", req.Person, resp.Node, req.Addr)
			}
			return resp, err
		}})
	if builtin {
		log.Printf("%s serving shards from %s on %s (built-in Yggdrasil, %d peers in list %s, yggstore %s)", *name, *dataDir, addr, len(live.List()), live.Hash(), versionString())
		return node.Serve(ctx, handler)
	}
	if *tname == "ygg" {
		// HTTP/3 on UDP too, for members with the built-in Yggdrasil.
		go func() {
			if err := yggnet.ServeSystem(ctx, ip, *port, handler); err != nil {
				log.Printf("not answering HTTP/3 (members with the built-in Yggdrasil can't reach this node): %v", err)
			}
		}()
	}
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("%s serving shards from %s on %s (%s, %d peers in list %s, yggstore %s)", *name, *dataDir, addr, t.Name(), len(live.List()), live.Hash(), versionString())
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	fs.Parse(args)
	list, err := peers.Load(*peersPath)
	if err != nil {
		return err
	}
	c := client.New()
	for _, p := range list {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		start := time.Now()
		info, err := c.Info(pctx, p.Addr)
		cancel()
		if err != nil {
			fmt.Printf("DOWN  %-12s %s  (%v)\n", p.Name, p.Addr, err)
			continue
		}
		fmt.Printf("UP    %-12s %s  %4dms  used %s / quota %s\n", p.Name, p.Addr,
			time.Since(start).Milliseconds(), size(info.UsedBytes), size(info.QuotaBytes))
	}
	return nil
}

func cmdPut(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	chunkMB := fs.Int("chunk", 4, "chunk size in MiB")
	nChal := fs.Int("challenges", 20, "precomputed challenges per shard")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("need exactly one FILE")
	}
	path := fs.Arg(0)
	stub := path + files.StubExt
	if _, err := os.Stat(stub); err == nil {
		return fmt.Errorf("%s already exists", stub)
	}
	list, err := peers.Load(*peersPath)
	if err != nil {
		return err
	}
	c := client.New()
	online := peers.Online(ctx, list, func(ctx context.Context, p peers.Peer) error {
		_, err := c.Info(ctx, p.Addr)
		return err
	})
	log.Printf("%d of %d peers online", len(online), len(list))
	m, chal, err := files.Put(ctx, c, path, online, files.PutOptions{
		ChunkSize: *chunkMB << 20, Challenges: *nChal, Log: log.Printf})
	if err != nil {
		return err
	}
	if err := files.WriteJSON(stub, m); err != nil {
		return err
	}
	if err := files.WriteJSON(files.ChallengesPath(stub), chal); err != nil {
		return err
	}
	log.Printf("stored %s (%s, %d chunks) -> %s", filepath.Base(path), size(int64(m.PlaintextSize)), len(m.Chunks), stub)
	log.Printf("the stub contains the decryption key: anyone with it and peer access can read the file")
	return nil
}

func cmdGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	out := fs.String("o", "", "output path for a file, or parent directory for a folder (default: current dir)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("need exactly one FILE.ystub")
	}
	m, err := files.ReadStub(fs.Arg(0))
	if err != nil {
		return err
	}
	if m.Kind == files.KindFolder {
		parent := *out
		if parent == "" {
			parent = "."
		}
		target, err := files.Restore(ctx, client.New(), m, parent, log.Printf)
		if err != nil {
			return err
		}
		if abs, err := filepath.Abs(target); err == nil {
			target = abs
		}
		log.Printf("restored folder %s (%d files)", target, m.FileCount)
		return nil
	}
	dest := *out
	if dest == "" {
		dest = m.FileName
	}
	f, err := os.OpenFile(dest+".partial", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := files.Get(ctx, client.New(), m, f, log.Printf); err != nil {
		f.Close()
		os.Remove(dest + ".partial")
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(dest+".partial", dest); err != nil {
		return err
	}
	if abs, err := filepath.Abs(dest); err == nil {
		dest = abs
	}
	log.Printf("restored %s (%s)", dest, size(int64(m.PlaintextSize)))
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("need exactly one FILE.ystub")
	}
	stub := fs.Arg(0)
	m, err := files.ReadStub(stub)
	if err != nil {
		return err
	}
	chal, err := files.ReadChallenges(files.ChallengesPath(stub))
	if err != nil {
		return fmt.Errorf("challenges file (only the uploader has it): %w", err)
	}
	results := files.Verify(ctx, client.New(), m, &chal)
	if err := files.WriteJSON(files.ChallengesPath(stub), chal); err != nil {
		return err
	}
	failed := 0
	for _, r := range results {
		status := "ok  "
		detail := ""
		if r.Err != nil {
			status, detail = "FAIL", "  "+r.Err.Error()
			failed++
		}
		fmt.Printf("%s chunk %d shard %d  %s  %s%s\n", status, r.Chunk, r.Index, r.Peer, r.Hash[:12], detail)
	}
	fmt.Printf("%d/%d shards passed\n", len(results)-failed, len(results))
	if failed > 0 {
		os.Exit(1)
	}
	return nil
}

func cmdRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("need exactly one FILE.ystub")
	}
	if dir := outboxOf(fs.Arg(0)); dir != "" {
		// In an outbox, older versions go too.
		w := outbox.New(outbox.Config{Dir: dir, Client: client.New(), Event: printEvent})
		return w.DeleteStub(ctx, fs.Arg(0))
	}
	m, res, err := files.DeleteStub(ctx, client.New(), fs.Arg(0))
	if err != nil {
		if res.Deleted+len(res.Failed) > 0 {
			return fmt.Errorf("%w\nthe stub is kept; run rm again when those nodes are back", err)
		}
		return err
	}
	log.Printf("deleted %s: %d shards, stub and challenges removed", m.FileName, res.Deleted)
	return nil
}

func cmdDashboard(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	stubDir := fs.String("stubs", ".", "directory searched for .ystub files")
	outfiles := fs.String("outfiles", "", "outbox folder to watch (also used as -stubs)")
	keep := fs.Bool("keep", false, "with -outfiles: keep originals after a verified upload")
	listen := fs.String("listen", "127.0.0.1:7480", "address for the dashboard page")
	tname := fs.String("transport", "ygg", "ygg or loopback (only used to show this node's ID)")
	interval := fs.Duration("interval", 3*time.Second, "how often to poll peers and files")
	testPeers := fs.String("test-peers", "", "optional test cluster peer list, shown separately")
	testStubs := fs.String("test-stubs", "", "optional folder of test cluster stubs")
	keyPath := fs.String("sharing-key", filepath.Join(yggstoreHome(), "sharing.key"), "your private sharing key (created if missing)")
	contactsPath := fs.String("contacts", filepath.Join(yggstoreHome(), "contacts.json"), "people you share with")
	me := fs.String("me", userName(), "your name, as people you share with see it")
	group := fs.String("group", userName()+"'s group", "the group's name, shown in invites")
	invites := fs.String("invites", filepath.Join(yggstoreHome(), "invites.json"), "invites you make (admins only)")
	yggPeers := fs.String("ygg-peers", defaultYggPeers, "comma-separated Yggdrasil peers newcomers can connect through")
	msgAPI := fs.String("msg-api", "127.0.0.1:7401", "this machine's node's local messaging API")
	msgToken := fs.String("msg-token", filepath.Join(yggstoreHome(), "msg.token"), "the node's messaging token file")
	mailbox := fs.String("mailbox", filepath.Join(yggstoreHome(), "mail"), "where this machine's node collects your email")
	repairAfter := fs.Duration("repair-after", repair.DefaultGrace, "rebuild shards held by a node down this long on nodes that are up (0 = never)")
	fs.Parse(args)

	id, err := share.LoadOrCreate(*keyPath)
	if err != nil {
		return fmt.Errorf("sharing key: %w", err)
	}

	selfID := ""
	if t, err := transport.ByName(*tname); err == nil {
		if ip, err := t.LocalIP(); err == nil {
			selfID = ip.String()
		}
	}
	if *outfiles != "" {
		*stubDir = *outfiles
	}
	absStubs, err := filepath.Abs(*stubDir)
	if err != nil {
		return err
	}
	c := client.New()
	c.HTTP.Timeout = 10 * time.Second
	cfg := dashboard.Config{PeersPath: *peersPath, StubDir: absStubs, SelfID: selfID, Interval: *interval, Client: c,
		TestPeersPath: *testPeers, Identity: id, ContactsPath: *contactsPath, Name: *me, Listen: *listen,
		InvitesPath: *invites, Group: *group, YggPeers: splitList(*yggPeers), MsgAPI: *msgAPI, MsgTokenPath: *msgToken,
		MailDir: *mailbox}
	if *testStubs != "" {
		if cfg.TestStubDir, err = filepath.Abs(*testStubs); err != nil {
			return err
		}
	}
	var w *outbox.Watcher
	if *outfiles != "" {
		// Uploads and restores move whole files, so they get a longer timeout.
		wc := client.New()
		wc.HTTP.Timeout = 2 * time.Minute
		w = outbox.New(outbox.Config{Dir: absStubs, PeersPath: *peersPath, Client: wc, Keep: *keep, Identity: id})
		cfg.Outbox = w
	}
	d := dashboard.New(cfg)
	go d.Run(ctx)
	if w != nil {
		w.SetEvent(d.Event)
		go func() {
			if err := w.Run(ctx); err != nil {
				log.Printf("outbox: %v", err)
			}
		}()
	}
	if *repairAfter > 0 {
		history := ""
		if w != nil {
			history = w.HistoryDir()
		}
		r := newRepairer(*peersPath, *repairAfter, absStubs, history, *mailbox)
		r.Log = d.Event
		go r.Run(ctx, 30*time.Minute)
	}

	srv := &http.Server{Addr: *listen, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("dashboard on http://%s (peers %s, stubs %s)", *listen, *peersPath, absStubs)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	dir := fs.String("dir", "outfiles", "outbox folder")
	keep := fs.Bool("keep", false, "keep originals after a verified upload")
	fs.Parse(args)
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	c := client.New()
	c.HTTP.Timeout = 2 * time.Minute
	w := outbox.New(outbox.Config{Dir: abs, PeersPath: *peersPath, Client: c, Keep: *keep,
		Event: func(kind, msg string) { log.Printf("[%s] %s", kind, msg) }})
	return w.Run(ctx)
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func defaultDataDir() string { return filepath.Join(yggstoreHome(), "shards") }

// yggstoreHome is ~/.yggstore, where a user's own yggstore files live.
func yggstoreHome() string {
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".yggstore")
	}
	return ".yggstore"
}

func userName() string {
	if u, err := user.Current(); err == nil {
		if u.Name != "" {
			return strings.Split(u.Name, ",")[0]
		}
		return u.Username
	}
	return hostname()
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// startBuiltin brings up the node's built-in Yggdrasil and sends this
// process's requests to other nodes through it.
func startBuiltin(keyPath, peerList, listen string, lan bool, port int) (*yggnet.Node, error) {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, err
	}
	key, err := yggnet.LoadKey(keyPath, true)
	if err != nil {
		return nil, fmt.Errorf("Yggdrasil key: %w", err)
	}
	node, err := yggnet.Start(yggnet.Config{Key: key, Peers: splitList(peerList), Listen: splitList(listen), Multicast: lan, Port: port, Logf: log.Printf})
	if err != nil {
		return nil, err
	}
	yggnet.Default.UseNode(node)
	return node, nil
}

// mailOutHandler keeps a nil relay a nil interface.
func mailOutHandler(r *mail.Relay) interface {
	Serve(http.ResponseWriter, *http.Request, string) bool
} {
	if r == nil {
		return nil
	}
	return r
}
