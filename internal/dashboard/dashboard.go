// Package dashboard serves a local status page: which nodes are up, how full
// they are, and whether every stored file still has enough shards.
package dashboard

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/contacts"
	"github.com/peterretief/yggstore/internal/devices"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/invite"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
)

//go:embed index.html
var indexHTML []byte

const (
	historyLen = 60
	maxEvents  = 200
)

type Config struct {
	PeersPath string
	StubDir   string
	SelfID    string
	Interval  time.Duration
	Client    client.Client
	Outbox    *outbox.Watcher // optional: enables the Restore button
	// Optional test cluster, shown in its own section and left out of the
	// totals: extra nodes to probe, and a folder of test stubs.
	TestPeersPath string
	TestStubDir   string
	// Sharing: your identity, the people you send to, and the name they see.
	Identity     *share.Identity
	ContactsPath string
	Name         string
	// Listen is the dashboard's own address; requests naming another host
	// are refused (see guard).
	Listen string
	// YggAddr, if set, is this machine's Yggdrasil address, which the
	// dashboard also answers on, but only to the Devices allowed in. Requests
	// from the LAN are sent there (see guard).
	YggAddr string
	Devices *devices.Watch
	// Invites (admin dashboards): where they are kept, the group's name, and
	// Yggdrasil peers a newcomer can connect through.
	InvitesPath string
	Group       string
	YggPeers    []string
	// Messaging goes through this machine's node: its local API address
	// and token file.
	MsgAPI       string
	MsgTokenPath string
	// MailDir is the mailbox this machine's node collects email into.
	MailDir string
	// NamesPath keeps name requests: those made here, and on an admin
	// dashboard those members sent (see names.go).
	NamesPath string
	// OwnSites, if set, serves the sites published here at SiteURL (this
	// box's Yggdrasil address), and gives their files for download.
	OwnSites *site.Own
	SiteURL  string
	// MePath keeps the name the person set on the dashboard (see me.go),
	// which takes the place of Name.
	MePath string
	// SitesDir keeps the websites published from this machine (site.Publisher).
	SitesDir string
	// Version is this yggstore's, shown under My box.
	Version string
	// LogPath keeps the activity log on disk (default: in the outbox's
	// .yggstore folder), so it survives restarts and can be looked at later.
	LogPath string
}

type PeerState struct {
	Name     string       `json:"name"`
	Addr     string       `json:"addr"`
	Up       bool         `json:"up"`
	RTTms    float64      `json:"rtt_ms"`
	Error    string       `json:"error,omitempty"`
	Info     *server.Info `json:"info,omitempty"`
	History  []float64    `json:"history"` // RTT per poll, -1 = down
	LastSeen int64        `json:"last_seen,omitempty"`
	Test     bool         `json:"test,omitempty"`
	List     string       `json:"list,omitempty"` // node's peer list: current, outdated, or old software
}

type ShardState struct {
	Peer    string `json:"peer"` // peer name, or address if unknown
	Present bool   `json:"present"`
}

type ChunkState struct {
	Reachable int          `json:"reachable"`
	Shards    []ShardState `json:"shards"`
}

type FileState struct {
	Stub           string           `json:"stub"`
	Name           string           `json:"name"`
	Kind           string           `json:"kind"` // file or folder
	FileCount      int              `json:"file_count,omitempty"`
	Size           int              `json:"size"`
	DataShards     int              `json:"data_shards"`
	TotalShards    int              `json:"total_shards"`
	Chunks         []ChunkState     `json:"chunks"`
	Status         string           `json:"status"`          // healthy, degraded, lost
	ChallengesLeft int              `json:"challenges_left"` // -1 = no challenges file
	Error          string           `json:"error,omitempty"`
	Test           bool             `json:"test,omitempty"`
	SharedBy       *manifest.Sender `json:"shared_by,omitempty"` // set if someone sent you this
	Versions       int              `json:"versions,omitempty"`  // older versions kept
	StoredAt       int64            `json:"stored_at,omitempty"`
}

type Event struct {
	Time int64  `json:"time"`
	Kind string `json:"kind"` // up, down, ok, warn, bad, info
	Msg  string `json:"msg"`
}

type State struct {
	GeneratedAt int64            `json:"generated_at"`
	SelfID      string           `json:"self_id"`
	StubDir     string           `json:"stub_dir"`
	Outbox      bool             `json:"outbox"`
	PeersError  string           `json:"peers_error,omitempty"`
	TestError   string           `json:"test_error,omitempty"`
	Peers       []PeerState      `json:"peers"`
	Files       []FileState      `json:"files"`
	Events      []Event          `json:"events"`
	Activity    *outbox.Activity `json:"activity,omitempty"` // upload, read-back or restore in progress
	Restore     *RestoreState    `json:"restore,omitempty"`  // where restores go, and the latest ones
	// This node's own peers.json entry, for a new node's first list; empty
	// unless this node is an admin and so can add nodes.
	AdminEntry string `json:"admin_entry,omitempty"`
	// SharingCode is what you give people who want to send you things.
	SharingCode string           `json:"sharing_code,omitempty"`
	Me          string           `json:"me,omitempty"`
	Contacts    []Contact        `json:"contacts"`
	Accounts    []Account        `json:"accounts"`
	Invites     []invite.Pending `json:"invites"`
	Box         *BoxState        `json:"box,omitempty"`
}

// Account is one person's share of the network, from what the nodes report.
// It is accounting only: nothing is enforced yet.
type Account struct {
	Owner     string `json:"owner"`
	Me        bool   `json:"me,omitempty"`
	Uses      int64  `json:"uses"`       // bytes their uploads take up across all nodes
	Holds     int64  `json:"holds"`      // bytes stored on their nodes
	ForOthers int64  `json:"for_others"` // of Holds, bytes other people uploaded
	Offered   int64  `json:"offered"`    // quota of their nodes that are up
	Nodes     int    `json:"nodes"`
	// ForCustomers is the part of ForOthers stored for paying customers
	// through the gateway, which earns credit.
	ForCustomers int64 `json:"for_customers,omitempty"`
	// Customers marks the row for the gateway's paying customers.
	Customers bool `json:"customers,omitempty"`
}

// CustomersOwner is the account paying customers' storage is counted under.
const CustomersOwner = "Paying customers"

// Contact is someone you can share with.
type Contact = contacts.Contact

type Dashboard struct {
	cfg     Config
	mu      sync.Mutex
	box     *mail.Box
	state   State
	history map[string][]float64
	seen    map[string]int64
	events  []Event
	wake    chan struct{}
	pushed  map[string]time.Time // last list push per node, used only by poll
	took    map[string]string    // list hash@start time each node accepted last, used only by poll
	logMu   sync.Mutex           // the activity log file
	namesMu sync.Mutex           // NamesPath, and the list while names change
	siteMu  sync.Mutex           // one website publish at a time
}

func New(cfg Config) *Dashboard {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.LogPath == "" && cfg.Outbox != nil {
		cfg.LogPath = cfg.Outbox.LogPath()
	}
	d := &Dashboard{cfg: cfg, history: map[string][]float64{}, seen: map[string]int64{}, wake: make(chan struct{}, 1),
		pushed: map[string]time.Time{}, took: map[string]string{}}
	d.events = readLog(cfg.LogPath, maxEvents)
	return d
}

// Run polls until ctx ends.
func (d *Dashboard) Run(ctx context.Context) {
	d.event("info", "dashboard started, watching "+d.cfg.StubDir)
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		d.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		st := d.state
		st.Events = append([]Event{}, d.events...)
		d.mu.Unlock()
		if st.Peers == nil {
			st.Peers = []PeerState{}
		}
		if st.Files == nil {
			st.Files = []FileState{}
		}
		if d.cfg.Outbox != nil {
			st.Activity = d.cfg.Outbox.Activity()
			st.Restore = d.restoreState()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /api/verify", d.handleVerify)
	mux.HandleFunc("POST /api/restore", d.handleRestore)
	mux.HandleFunc("POST /api/restore-to", d.handleRestoreTo)
	mux.HandleFunc("GET /api/versions", d.handleVersions)
	mux.HandleFunc("POST /api/restore-folder", d.handleRestoreFolder)
	mux.HandleFunc("POST /api/open-folder", d.handleOpenFolder)
	mux.HandleFunc("POST /api/delete", d.handleDelete)
	mux.HandleFunc("POST /api/nodes", d.handleAddNode)
	mux.HandleFunc("POST /api/share", d.handleShare)
	mux.HandleFunc("POST /api/contacts", d.handleAddContact)
	mux.HandleFunc("POST /api/invites", d.handleInvite)
	mux.HandleFunc("POST /api/contacts/remove", d.handleRemoveContact)
	mux.HandleFunc("GET /api/msg", d.handleMsgState)
	mux.HandleFunc("POST /api/msg/send", d.handleMsgSend)
	mux.HandleFunc("POST /api/msg/subscribe", d.handleMsgSub)
	mux.HandleFunc("POST /api/msg/unsubscribe", d.handleMsgSub)
	mux.HandleFunc("GET /api/mail", d.handleMailList)
	mux.HandleFunc("GET /api/mail/message", d.handleMailRead)
	mux.HandleFunc("GET /api/mail/raw", d.handleMailRaw)
	mux.HandleFunc("GET /api/mail/attachment", d.handleMailAttachment)
	mux.HandleFunc("POST /api/mail/delete", d.handleMailDelete)
	mux.HandleFunc("POST /api/mail/send", d.handleMailSend)
	mux.HandleFunc("GET /api/names", d.handleNames)
	mux.HandleFunc("POST /api/names/claim", d.handleClaim)
	mux.HandleFunc("POST /api/names/decide", d.handleDecide)
	mux.HandleFunc("POST /api/names/remove", d.handleRemoveName)
	mux.HandleFunc("POST /api/me", d.handleSetMe)
	mux.HandleFunc("GET /api/sites", d.handleSites)
	mux.HandleFunc("POST /api/sites/publish", d.handleSitePublish)
	mux.HandleFunc("GET /api/sites/download", d.handleSiteDownload)
	mux.HandleFunc("GET /api/sites/files", d.handleSiteFiles)
	mux.HandleFunc("POST /api/sites/{action}", d.handleSiteAction)
	return d.guard(mux)
}

// guard keeps other web pages out. The dashboard listens on localhost, but
// any page open in your browser can send requests there. Actions need a
// header such pages cannot add without the dashboard's consent (it never
// gives it), and the Host header must name the dashboard itself, which stops
// DNS rebinding.
//
// With YggAddr set it also listens on every address. Over Yggdrasil only the
// allowed devices get in: a Yggdrasil address can't be used without its
// private key, so it identifies the device. From anywhere else (the LAN, by
// IP or a .local name) a page is only sent on to the Yggdrasil address, so
// nothing is served without that check.
func (d *Dashboard) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if local := localIP(r); local != nil && !local.IsLoopback() {
			if d.cfg.YggAddr == "" {
				http.Error(w, "the dashboard only answers on this machine", http.StatusForbidden)
				return
			}
			if local.String() != d.cfg.YggAddr {
				if r.Method != http.MethodGet {
					http.Error(w, "open the dashboard at its Yggdrasil address", http.StatusForbidden)
					return
				}
				http.Redirect(w, r, d.yggURL()+r.URL.RequestURI(), http.StatusFound)
				return
			}
			if remote := remoteIP(r); remote != d.cfg.YggAddr && (d.cfg.Devices == nil || !d.cfg.Devices.Allowed(remote)) {
				d.notAllowed(w, remote)
				return
			}
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		listenHost, _, _ := net.SplitHostPort(d.cfg.Listen)
		ownName := host == "localhost" || (host != "" && host == listenHost) || net.ParseIP(host).IsLoopback() ||
			(d.cfg.YggAddr != "" && net.ParseIP(host).Equal(net.ParseIP(d.cfg.YggAddr)))
		if !ownName {
			http.Error(w, "the dashboard only answers to its own address", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Yggstore") != "1" {
			http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localIP is the address the request came in on, or nil if unknown (as in
// tests, where it's treated as localhost).
func localIP(r *http.Request) net.IP {
	a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return nil
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// yggURL is the dashboard's address on Yggdrasil.
func (d *Dashboard) yggURL() string {
	_, port, _ := net.SplitHostPort(d.cfg.Listen)
	return "http://" + net.JoinHostPort(d.cfg.YggAddr, port)
}

// notAllowed tells a device that isn't on the list how to be added.
func (d *Dashboard) notAllowed(w http.ResponseWriter, remote string) {
	file := ""
	if d.cfg.Devices != nil {
		file = " -file " + d.cfg.Devices.Path()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Not one of your devices</title>
<body style="font-family:system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em;line-height:1.5">
<h1>This dashboard opens only on its owner's devices</h1>
<p>This device's Yggdrasil address is <code>%s</code>.</p>
<p>If it's yours, run this on the machine the dashboard runs on, then reload this page:</p>
<pre style="background:#eee;padding:.8em;white-space:pre-wrap">yggstore devices%s add %s "my laptop"</pre>
</body>`, html.EscapeString(remote), html.EscapeString(file), html.EscapeString(remote))
}

func (d *Dashboard) handleVerify(w http.ResponseWriter, r *http.Request) {
	// Only stubs the dashboard itself found may be verified.
	stub := r.URL.Query().Get("stub")
	d.mu.Lock()
	known := false
	for _, f := range d.state.Files {
		if f.Stub == stub {
			known = true
		}
	}
	d.mu.Unlock()
	if !known {
		http.Error(w, "unknown stub", http.StatusNotFound)
		return
	}
	m, err := files.ReadStub(stub)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chal, err := files.ReadChallenges(files.ChallengesPath(stub))
	if err != nil {
		http.Error(w, "no challenges file for this stub", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results := files.Verify(ctx, d.cfg.Client, m, &chal)
	if err := files.WriteJSON(files.ChallengesPath(stub), chal); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := d.peerNames()
	type row struct {
		Chunk int    `json:"chunk"`
		Index int    `json:"index"`
		Peer  string `json:"peer"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	out := struct {
		Passed int   `json:"passed"`
		Total  int   `json:"total"`
		Rows   []row `json:"rows"`
	}{Total: len(results)}
	for _, res := range results {
		rw := row{Chunk: res.Chunk, Index: res.Index, Peer: nameFor(names, res.Peer), OK: res.Err == nil}
		if res.Err != nil {
			rw.Error = shortErr(res.Err)
		} else {
			out.Passed++
		}
		out.Rows = append(out.Rows, rw)
	}
	kind := "ok"
	if out.Passed < out.Total {
		kind = "bad"
	}
	d.event(kind, fmt.Sprintf("verify %s: %d/%d shards proved storage", m.FileName, out.Passed, out.Total))
	select {
	case d.wake <- struct{}{}:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleRestore rebuilds a listed stub into the outbox's restored/ folder in
// the background; progress shows up in the activity log.
func (d *Dashboard) handleRestore(w http.ResponseWriter, r *http.Request) {
	if d.cfg.Outbox == nil {
		http.Error(w, "dashboard was started without -outfiles", http.StatusBadRequest)
		return
	}
	stub := r.URL.Query().Get("stub")
	if !d.knownStub(stub) {
		http.Error(w, "unknown stub", http.StatusNotFound)
		return
	}
	// An older version, if asked for.
	path, err := d.cfg.Outbox.VersionStub(stub, r.URL.Query().Get("version"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	what := filepath.Base(stub)
	if path != stub {
		what += " (an older version)"
	}
	d.Event("info", "restoring "+what+"…")
	go d.cfg.Outbox.RestoreStub(context.Background(), path)
	w.WriteHeader(http.StatusAccepted)
}

// knownStub reports whether stub is one of the listed real items.
func (d *Dashboard) knownStub(stub string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.state.Files {
		if f.Stub == stub && !f.Test {
			return true
		}
	}
	return false
}

// handleDelete removes a listed item from all nodes in the background, then its
// stub; the outcome shows up in the activity log. Real files go through the
// outbox so a delete never overlaps an upload or restore.
func (d *Dashboard) handleDelete(w http.ResponseWriter, r *http.Request) {
	stub := r.URL.Query().Get("stub")
	d.mu.Lock()
	var f *FileState
	for i := range d.state.Files {
		if d.state.Files[i].Stub == stub {
			f = &d.state.Files[i]
		}
	}
	test := f != nil && f.Test
	var item FileState
	if f != nil {
		item = *f
	}
	d.mu.Unlock()
	if f == nil {
		http.Error(w, "unknown stub", http.StatusNotFound)
		return
	}
	f = &item
	d.Event("info", "deleting "+filepath.Base(stub)+"…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if d.cfg.Outbox != nil && !test {
			d.cfg.Outbox.DeleteStub(ctx, stub)
		} else if f.SharedBy != nil {
			// The shards are the sender's; only our record of them goes.
			if err := os.Remove(stub); err != nil {
				d.Event("warn", "could not remove "+filepath.Base(stub)+": "+err.Error())
			} else {
				d.Event("ok", fmt.Sprintf("removed %s (shared by %s) from your list", f.Name, f.SharedBy.Name))
			}
		} else {
			m, res, err := files.DeleteStub(ctx, d.cfg.Client, stub)
			if err != nil {
				d.Event("warn", fmt.Sprintf("delete %s: %d shards deleted, %d not; stub kept: %s", filepath.Base(stub), res.Deleted, len(res.Failed), shortErr(err)))
			} else {
				d.Event("ok", fmt.Sprintf("deleted %s from all nodes: %d shards", m.FileName, res.Deleted))
			}
		}
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}()
	w.WriteHeader(http.StatusAccepted)
}

func (d *Dashboard) poll(ctx context.Context) {
	st := State{GeneratedAt: time.Now().Unix(), SelfID: d.cfg.SelfID, StubDir: d.cfg.StubDir, Outbox: d.cfg.Outbox != nil, Box: d.boxState()}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		st.PeersError = err.Error()
	}
	mainList := list
	if d.cfg.Identity != nil {
		st.SharingCode, st.Me = d.cfg.Identity.Code(), d.me()
	}
	st.Contacts = d.contacts()
	if d.cfg.InvitesPath != "" {
		st.Invites = invite.List(d.cfg.InvitesPath)
	}
	for _, p := range mainList {
		if p.Admin && p.IP() == d.cfg.SelfID {
			b, _ := json.Marshal(p)
			st.AdminEntry = string(b)
		}
	}
	test := map[string]bool{}
	if d.cfg.TestPeersPath != "" {
		inMain := map[string]bool{}
		for _, p := range list {
			inMain[p.Addr] = true
		}
		tlist, err := peers.Load(d.cfg.TestPeersPath)
		if err != nil {
			st.TestError = err.Error()
		}
		for _, p := range tlist {
			if !inMain[p.Addr] {
				test[p.Addr] = true
				list = append(list, p)
			}
		}
	}
	st.Peers = d.probePeers(ctx, list)
	for i := range st.Peers {
		st.Peers[i].Test = test[st.Peers[i].Addr]
	}
	if st.PeersError == "" {
		d.syncLists(ctx, mainList, st.Peers, st.AdminEntry != "")
		d.syncNames(ctx, mainList)
	}
	st.Accounts = accounts(mainList, st.Peers, d.cfg.SelfID)

	up := map[string]bool{}
	names := map[string]string{}
	for _, p := range st.Peers {
		up[p.Addr] = p.Up
		names[p.Addr] = p.Name
	}
	st.Files = d.scanFiles(ctx, d.cfg.StubDir, up, names)
	if d.cfg.TestStubDir != "" {
		for _, f := range d.scanFiles(ctx, d.cfg.TestStubDir, up, names) {
			f.Test = true
			st.Files = append(st.Files, f)
		}
	}

	d.mu.Lock()
	prev := d.state
	d.state = st
	d.mu.Unlock()
	d.diff(prev, st)
}

// accounts totals, per person, the space their uploads use and the space
// their nodes give, from each node's per-uploader counts.
func accounts(list []peers.Peer, states []PeerState, self string) []Account {
	byIP, byAddr := map[string]peers.Peer{}, map[string]peers.Peer{}
	for _, p := range list {
		byIP[p.IP()], byAddr[p.Addr] = p, p
	}
	operator := func(ip string) string {
		if p, ok := byIP[ip]; ok {
			if p.Gateway {
				return CustomersOwner
			}
			return p.Operator()
		}
		if ip == "" {
			return "(unrecorded)"
		}
		return "unknown node " + ip
	}
	acc := map[string]*Account{}
	get := func(owner string) *Account {
		if acc[owner] == nil {
			acc[owner] = &Account{Owner: owner}
		}
		return acc[owner]
	}
	for _, ps := range states {
		p, ok := byAddr[ps.Addr]
		if !ok || ps.Test {
			continue
		}
		mine := get(p.Operator())
		mine.Nodes++
		if ps.Info == nil {
			continue
		}
		mine.Offered += ps.Info.QuotaBytes
		for writer, n := range ps.Info.ByWriter {
			who := operator(writer)
			get(who).Uses += n
			mine.Holds += n
			if who != p.Operator() {
				mine.ForOthers += n
			}
			if who == CustomersOwner {
				mine.ForCustomers += n
			}
		}
	}
	me := operator(self)
	out := make([]Account, 0, len(acc))
	for _, a := range acc {
		a.Me = a.Owner == me
		a.Customers = a.Owner == CustomersOwner
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Me != out[j].Me {
			return out[i].Me
		}
		return out[i].Owner < out[j].Owner
	})
	return out
}

// syncLists marks which nodes hold this node's peer list and, if this node is
// an admin, sends it to those that don't, at most once a minute each. So an
// added node, or a hand edit of peers.json, reaches every node by itself.
// Nodes on older software report no list and are left alone. So is a node
// that took the list but still reports another one: its yggstore is older and
// drops fields it doesn't know, so sending it again would never help, until
// it restarts (as an update does).
func (d *Dashboard) syncLists(ctx context.Context, list []peers.Peer, states []PeerState, admin bool) {
	want := peers.Hash(list)
	for i := range states {
		ps := &states[i]
		if ps.Test || ps.Info == nil {
			continue
		}
		took := want + "@" + strconv.FormatInt(ps.Info.StartedAt, 10)
		switch h := ps.Info.PeersHash; {
		case h == "":
			ps.List = "old software"
		case h == want:
			ps.List = "current"
		case d.took[ps.Addr] == took:
			ps.List = "old software"
		default:
			ps.List = "outdated"
			if !admin || time.Since(d.pushed[ps.Addr]) < time.Minute {
				continue
			}
			d.pushed[ps.Addr] = time.Now()
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := d.cfg.Client.PushPeers(pctx, ps.Addr, list)
			cancel()
			if err != nil {
				d.event("warn", fmt.Sprintf("could not send the node list to %s: %s", ps.Name, shortErr(err)))
				continue
			}
			ps.List = "current"
			d.took[ps.Addr] = took
			d.event("info", "sent the updated node list to "+ps.Name)
		}
	}
}

// handleInvite makes a one-time invite for someone to join the group.
// inviteYggPeers are the Yggdrasil peers a newcomer connects through: the
// members' own (ygg_listen) first, then the group's public ones (ygg_peers),
// then public.
func inviteYggPeers(peersPath string, public []string) []string {
	var out []string
	seen := map[string]bool{}
	list, _ := peers.Load(peersPath)
	for _, p := range list {
		for _, u := range p.YggListen {
			if !seen[u] {
				out, seen[u] = append(out, u), true
			}
		}
	}
	for _, u := range append(peers.PublicPeers(list), public...) {
		if !seen[u] {
			out, seen[u] = append(out, u), true
		}
	}
	return out
}

func (d *Dashboard) handleInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		For string `json:"for"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	d.mu.Lock()
	adminEntry := d.state.AdminEntry
	d.mu.Unlock()
	if adminEntry == "" || d.cfg.InvitesPath == "" {
		http.Error(w, "only an admin dashboard can invite people", http.StatusForbidden)
		return
	}
	var admin peers.Peer
	json.Unmarshal([]byte(adminEntry), &admin)
	inv, err := invite.Create(d.cfg.InvitesPath, req.For)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	inv.Group, inv.From, inv.Admin, inv.YggPeers = d.cfg.Group, d.me(), admin, inviteYggPeers(d.cfg.PeersPath, d.cfg.YggPeers)
	if d.cfg.Identity != nil {
		inv.SharingCode = d.cfg.Identity.Code()
	}
	who := strings.TrimSpace(req.For)
	if who == "" {
		who = "someone"
	}
	d.event("info", fmt.Sprintf("made an invite for %s (valid 7 days, once)", who))
	writeJSONResp(w, map[string]any{"invite": inv.Encode(), "expires": inv.Expires, "group": inv.Group, "ygg_peers": inv.YggPeers})
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// contacts reads the contact list.
func (d *Dashboard) contacts() []Contact {
	if d.cfg.ContactsPath == "" {
		return []Contact{}
	}
	return contacts.Load(d.cfg.ContactsPath)
}

func (d *Dashboard) handleAddContact(w http.ResponseWriter, r *http.Request) {
	var c Contact
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&c); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	own := ""
	if d.cfg.Identity != nil {
		own = d.cfg.Identity.Code()
	}
	if d.cfg.ContactsPath == "" {
		http.Error(w, "no contacts file configured", http.StatusInternalServerError)
		return
	}
	if err := contacts.Add(d.cfg.ContactsPath, c, own); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, contacts.ErrExists) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	d.mu.Lock()
	d.state.Contacts = d.contacts()
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (d *Dashboard) handleRemoveContact(w http.ResponseWriter, r *http.Request) {
	if err := contacts.Remove(d.cfg.ContactsPath, r.URL.Query().Get("code")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.mu.Lock()
	d.state.Contacts = d.contacts()
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleShare seals one of your stubs for a contact and returns the .ysend
// file for you to send them. They need a node in this network to download it.
func (d *Dashboard) handleShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stub string `json:"stub"`
		To   string `json:"to"` // the contact's sharing code
		Note string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if d.cfg.Identity == nil {
		http.Error(w, "this dashboard has no sharing identity", http.StatusForbidden)
		return
	}
	d.mu.Lock()
	known := false
	for _, f := range d.state.Files {
		known = known || (f.Stub == req.Stub && !f.Test)
	}
	to := ""
	for _, c := range d.state.Contacts {
		if c.Code == req.To {
			to = c.Name
		}
	}
	d.mu.Unlock()
	if !known {
		http.Error(w, "unknown stub", http.StatusNotFound)
		return
	}
	if to == "" {
		http.Error(w, "add them as a contact first", http.StatusBadRequest)
		return
	}
	m, err := files.ReadStub(req.Stub)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.SharedBy = nil // passing on something you were sent: you are the sender now
	stub, err := json.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sealed, err := d.cfg.Identity.Seal(req.To, stub, d.me(), strings.TrimSpace(req.Note))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.event("ok", fmt.Sprintf("sealed %s for %s; send them the .ysend file", m.FileName, to))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": m.FileName + share.Ext}))
	w.Write(sealed)
}

// handleAddNode adds a node to peers.json from the output of "yggstore id"
// on that machine. syncLists then sends every node the new list.
func (d *Dashboard) handleAddNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
		Host string `json:"host"`
		Slow bool   `json:"slow"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p, err := parseNodeEntry(req.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.Host, p.Slow = strings.TrimSpace(req.Host), req.Slow
	d.mu.Lock()
	admin := d.state.AdminEntry != ""
	d.mu.Unlock()
	if !admin {
		http.Error(w, "this node is not marked \"admin\": true in peers.json, so it cannot add nodes", http.StatusForbidden)
		return
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list = append(list, p)
	if err := peers.Validate(list); err != nil {
		http.Error(w, "a node with that name or address is already in the list", http.StatusConflict)
		return
	}
	if err := peers.Write(d.cfg.PeersPath, list); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.event("ok", fmt.Sprintf("added node %s (%s); every node gets the new list as soon as it answers", p.Name, p.Addr))
	select {
	case d.wake <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseNodeEntry reads the peers.json entry that "yggstore id" prints.
func parseNodeEntry(text string) (peers.Peer, error) {
	var p peers.Peer
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j < i {
		return p, errors.New(`paste what "yggstore id -name NAME" prints on the new machine`)
	}
	if err := json.Unmarshal([]byte(text[i:j+1]), &p); err != nil {
		return p, fmt.Errorf("could not read the entry: %v", err)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || strings.ContainsAny(p.Name, " /\\\"<>") {
		return p, errors.New("the node needs a short name without spaces")
	}
	if !peers.IsOverlay(p.Addr) {
		return p, errors.New("the address must be a Yggdrasil address (200::/7) with a port, as yggstore id prints it")
	}
	p.Admin = false // admins are made by editing peers.json, not from a paste
	return p, nil
}

func (d *Dashboard) probePeers(ctx context.Context, list []peers.Peer) []PeerState {
	out := make([]PeerState, len(list))
	var wg sync.WaitGroup
	for i, p := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ps := PeerState{Name: p.Name, Addr: p.Addr}
			pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			start := time.Now()
			info, err := d.cfg.Client.Info(pctx, p.Addr)
			cancel()
			if err != nil {
				ps.Error = shortErr(err)
			} else {
				ps.Up = true
				ps.RTTms = float64(time.Since(start).Microseconds()) / 1000
				ps.Info = &info
			}
			out[i] = ps
		}()
	}
	wg.Wait()

	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range out {
		v := -1.0
		if out[i].Up {
			v = out[i].RTTms
			d.seen[out[i].Addr] = time.Now().Unix()
		}
		h := append(d.history[out[i].Addr], v)
		if len(h) > historyLen {
			h = h[len(h)-historyLen:]
		}
		d.history[out[i].Addr] = h
		out[i].History = append([]float64(nil), h...)
		out[i].LastSeen = d.seen[out[i].Addr]
	}
	return out
}

func (d *Dashboard) scanFiles(ctx context.Context, root string, up map[string]bool, names map[string]string) []FileState {
	var stubs []string
	filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Stubs waiting in the outbox's restore/ and delete/ folders are not
		// stored items, and restored/ holds rebuilt files.
		inRestore := d.cfg.Outbox != nil && filepath.Dir(path) == root &&
			(e.Name() == outbox.RestoreDir || e.Name() == outbox.DeleteDir || e.Name() == outbox.RestoredDir)
		if e.IsDir() && path != root && (inRestore || strings.HasPrefix(e.Name(), ".") || strings.Count(strings.TrimPrefix(path, root), string(os.PathSeparator)) > 16) {
			return filepath.SkipDir
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), files.StubExt) {
			stubs = append(stubs, path)
		}
		return nil
	})
	sort.Strings(stubs)

	sem := make(chan struct{}, 16)
	out := make([]FileState, len(stubs))
	var wg sync.WaitGroup
	for i, stub := range stubs {
		fsx := FileState{Stub: stub, Name: filepath.Base(strings.TrimSuffix(stub, files.StubExt)), ChallengesLeft: -1}
		m, err := files.ReadStub(stub)
		if err != nil {
			fsx.Status, fsx.Error = "lost", "unreadable stub: "+shortErr(err)
			out[i] = fsx
			continue
		}
		fsx.Name, fsx.Size, fsx.Kind, fsx.FileCount = m.FileName, m.PlaintextSize, "file", m.FileCount
		fsx.SharedBy, fsx.StoredAt = m.SharedBy, m.StoredAt
		if d.cfg.Outbox != nil && root == d.cfg.Outbox.Dir() && m.SharedBy == nil {
			fsx.Versions = d.cfg.Outbox.VersionCount(m)
		}
		if m.Kind == files.KindFolder {
			fsx.Kind = "folder"
		}
		fsx.DataShards, fsx.TotalShards = m.DataShards, m.DataShards+m.ParityShards
		if chal, err := files.ReadChallenges(files.ChallengesPath(stub)); err == nil {
			left := 0
			for _, list := range chal.Shards {
				for _, c := range list {
					if !c.Used {
						left++
					}
				}
			}
			fsx.ChallengesLeft = left
		}
		fsx.Chunks = make([]ChunkState, len(m.Chunks))
		for ci, ch := range m.Chunks {
			fsx.Chunks[ci].Shards = make([]ShardState, len(ch.Shards))
			for si, ref := range ch.Shards {
				shard := &fsx.Chunks[ci].Shards[si] // backing arrays are fixed before any goroutine starts
				shard.Peer = nameFor(names, ref.Peer)
				if !up[ref.Peer] {
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
					ok, _ := d.cfg.Client.Has(hctx, ref.Peer, ref.Hash)
					cancel()
					shard.Present = ok
				}()
			}
		}
		out[i] = fsx
	}
	wg.Wait()

	for i := range out {
		if out[i].Error != "" {
			continue
		}
		status := "healthy"
		for ci := range out[i].Chunks {
			n := 0
			for _, s := range out[i].Chunks[ci].Shards {
				if s.Present {
					n++
				}
			}
			out[i].Chunks[ci].Reachable = n
			switch {
			case n < out[i].DataShards:
				status = "lost"
			case n < out[i].TotalShards && status == "healthy":
				status = "degraded"
			}
		}
		out[i].Status = status
	}
	return out
}

// diff turns state changes into events.
func (d *Dashboard) diff(prev, cur State) {
	if prev.GeneratedAt == 0 {
		for _, p := range cur.Peers {
			if p.Up {
				d.event("up", fmt.Sprintf("%s is up (%.0f ms)", p.Name, p.RTTms))
			} else {
				d.event("down", fmt.Sprintf("%s is down: %s", p.Name, p.Error))
			}
		}
		for _, f := range cur.Files {
			if f.Status != "healthy" {
				d.event(statusKind(f.Status), fmt.Sprintf("%s is %s", f.Name, f.Status))
			}
		}
		return
	}
	was := map[string]PeerState{}
	for _, p := range prev.Peers {
		was[p.Addr] = p
	}
	for _, p := range cur.Peers {
		old, ok := was[p.Addr]
		switch {
		case !ok:
			d.event("info", fmt.Sprintf("%s added to peers.json", p.Name))
		case old.Up && !p.Up:
			d.event("down", fmt.Sprintf("%s went down: %s", p.Name, p.Error))
		case !old.Up && p.Up:
			d.event("up", fmt.Sprintf("%s came up (%.0f ms)", p.Name, p.RTTms))
		case p.Up && old.Info != nil && p.Info != nil && p.Info.ShardCount != old.Info.ShardCount:
			d.event("info", fmt.Sprintf("%s now holds %d shards (%+d)", p.Name, p.Info.ShardCount, p.Info.ShardCount-old.Info.ShardCount))
		}
	}
	wasFile := map[string]string{}
	for _, f := range prev.Files {
		wasFile[f.Stub] = f.Status
	}
	for _, f := range cur.Files {
		old, ok := wasFile[f.Stub]
		if !ok {
			d.event("info", fmt.Sprintf("new file %s (%d chunks)", f.Name, len(f.Chunks)))
			if f.Status != "healthy" {
				d.event(statusKind(f.Status), fmt.Sprintf("%s is %s", f.Name, f.Status))
			}
		} else if old != f.Status {
			d.event(statusKind(f.Status), fmt.Sprintf("%s: %s → %s", f.Name, old, f.Status))
		}
	}
}

// Event adds an entry to the activity log.
func (d *Dashboard) Event(kind, msg string) { d.event(kind, msg) }

func (d *Dashboard) event(kind, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ev := Event{Time: time.Now().Unix(), Kind: kind, Msg: msg}
	d.events = append(d.events, ev)
	if len(d.events) > maxEvents {
		d.events = d.events[len(d.events)-maxEvents:]
	}
	d.writeLog(ev)
}

func (d *Dashboard) peerNames() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	names := map[string]string{}
	for _, p := range d.state.Peers {
		names[p.Addr] = p.Name
	}
	return names
}

func nameFor(names map[string]string, addr string) string {
	if n, ok := names[addr]; ok {
		return n
	}
	return addr
}

func statusKind(status string) string {
	switch status {
	case "healthy":
		return "ok"
	case "degraded":
		return "warn"
	}
	return "bad"
}

// shortErr trims Go's long URL-prefixed network errors to the useful tail.
func shortErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "deadline exceeded") || strings.Contains(s, "Client.Timeout"):
		return "no response (timed out): node offline, overlay not connected, or firewall"
	case strings.Contains(s, "connection refused"):
		return "connection refused: yggstore not running on that port"
	case strings.Contains(s, "network is unreachable") || strings.Contains(s, "no route to host"):
		return "no route: Yggdrasil not running here, or address wrong"
	case strings.Contains(s, "403"):
		return "refused: this node is not in their peers.json"
	}
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}
