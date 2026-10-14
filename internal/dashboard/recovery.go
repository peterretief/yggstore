package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/workcopy"
	"github.com/peterretief/yggstore/internal/workhere"
)

// RecoveryDir is where the recovery copies' stubs and the recovery key are
// kept, under the yggstore home. Repair looks after the copies there.
func RecoveryDir(home string) string { return filepath.Join(home, "recovery") }

// runRecovery keeps a recovery copy in the group: the outbox's stubs and
// history, the person's things in the yggstore home, and the member list.
func (d *Dashboard) runRecovery(ctx context.Context) {
	if d.cfg.Outbox == nil || d.cfg.Home == "" {
		return
	}
	var t *workcopy.Tracker
	k := recovery.New(recovery.Config{
		Dir: RecoveryDir(d.cfg.Home),
		Sources: []recovery.Source{
			{Name: "outbox", Path: d.cfg.Outbox.Dir(), Keep: recovery.OutboxKeep},
			{Name: "home", Path: d.cfg.Home, Keep: recovery.HomeKeep},
			{Name: "peers.json", Path: d.cfg.PeersPath},
		},
		Client:  d.cfg.Client,
		Online:  func(context.Context) ([]peers.Peer, error) { return d.onlinePeers() },
		Event:   d.event,
		Holding: d.Holding,
		OnCopy: func(ctx context.Context, m manifest.Manifest) {
			d.kickRecords()
			if t == nil {
				return
			}
			if err := t.Copied(ctx, m); err != nil {
				d.event("warn", "could not tell your other places about the new recovery copy: "+err.Error())
			}
		},
	})
	if d.cfg.Identity != nil && d.cfg.MsgAPI != "" {
		t = workcopy.New(workcopy.Config{
			Bus:     func() (workcopy.Bus, error) { return d.msgClient() },
			ID:      func() *share.Identity { return d.cfg.Identity },
			Place:   d.placeName,
			SaveNow: k.SaveNow,
			Event:   d.event,
		})
		go t.Run(ctx)
	}
	kick := make(chan struct{}, 1)
	d.mu.Lock()
	d.recovery, d.workcopy, d.recoveryKick = k, t, kick
	d.mu.Unlock()
	go d.keepRecords(ctx, k, kick)
	if err := k.Run(ctx); err != nil {
		d.event("bad", "recovery: "+err.Error())
	}
}

// Holding is whether this place holds the person's working copy, and so may
// change their files. Without messaging it always does.
func (d *Dashboard) Holding() bool {
	d.mu.Lock()
	t := d.workcopy
	d.mu.Unlock()
	return t == nil || t.Holding()
}

// placeName is this place's name: its node's name in the group's list, or
// the machine's.
func (d *Dashboard) placeName() string {
	if list, err := peers.Load(d.cfg.PeersPath); err == nil {
		if p, ok := selfEntry(list, d.cfg.SelfID); ok {
			return p.Name
		}
	}
	h, _ := os.Hostname()
	return h
}

func (d *Dashboard) workcopyState() *workcopy.State {
	d.mu.Lock()
	t := d.workcopy
	d.mu.Unlock()
	if t == nil {
		return nil
	}
	s := t.State()
	return &s
}

func (d *Dashboard) kickRecords() {
	d.mu.Lock()
	kick := d.recoveryKick
	d.mu.Unlock()
	if kick != nil {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

func (d *Dashboard) recoveryStatus() *recovery.Status {
	d.mu.Lock()
	k := d.recovery
	d.mu.Unlock()
	if k == nil {
		return nil
	}
	s := k.Status()
	return &s
}

// handleRecovery turns recovery on or off, and sets where the link goes.
func (d *Dashboard) handleRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On    bool    `json:"on"`
		Email string  `json:"email"`
		Box   *string `json:"box"`
		// Resend emails the link again, to the same address.
		Resend bool `json:"resend"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	k := d.recovery
	d.mu.Unlock()
	if k == nil {
		http.Error(w, "recovery needs the outbox", http.StatusConflict)
		return
	}
	was := k.Status().On
	if err := k.Set(req.On, req.Email); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Resend {
		k.SetSent("")
	}
	if req.Box != nil {
		if err := k.SetBox(*req.Box); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	d.kickRecords()
	switch {
	case was && !req.On:
		d.event("warn", "working from anywhere turned off: your files can only be reached through this computer")
	case !was && req.On:
		d.event("info", "working from anywhere turned on")
	}
	w.WriteHeader(http.StatusNoContent)
}

// keepRecords keeps this person's recovery record on every web node, and
// emails the link once there is a record to open and an address to send
// it to. It runs after each copy, after a setting changes, and every 10
// minutes (for web nodes that joined since).
func (d *Dashboard) keepRecords(ctx context.Context, k *recovery.Keeper, kick <-chan struct{}) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
		if !d.Holding() || !k.Status().On {
			continue
		}
		msg := ""
		if err := d.pushRecord(ctx, k); err != nil {
			msg = "recovery link: " + err.Error()
		} else if err := d.sendLink(ctx, k); err != nil {
			msg = "recovery link not emailed: " + err.Error()
		}
		if msg != "" && msg != last {
			d.event("warn", msg)
		}
		last = msg
	}
}

// webNodes are the members that serve the group's websites, as last polled.
func (d *Dashboard) webNodes() []PeerState {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []PeerState
	for _, p := range d.state.Peers {
		if p.Up && !p.Test && p.Info != nil && len(p.Info.Web) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func (d *Dashboard) pushRecord(ctx context.Context, k *recovery.Keeper) error {
	ks := k.Keys()
	if len(ks.Sealed) == 0 {
		return nil // no copy yet
	}
	nodes := d.webNodes()
	if len(nodes) == 0 {
		return errors.New("no web node answers to keep it")
	}
	rec := workhere.Record{ID: ks.ID, GroupHalf: ks.GroupHalf, Sealed: ks.Sealed, Box: k.Status().Box, Name: d.placeName()}
	if me := d.me(); me != "" {
		rec.Name = me
	}
	c := &http.Client{Timeout: 30 * time.Second}
	took := 0
	var firstErr error
	for _, n := range nodes {
		if err := workhere.Put(ctx, c, n.Addr, rec); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", n.Name, err)
			}
			continue
		}
		took++
	}
	if took == 0 {
		return firstErr
	}
	return nil
}

// linkHost is the site the recovery link opens on: one of this person's
// names, or else any site the web nodes serve.
func (d *Dashboard) linkHost() string {
	if pub, err := d.publisher(); err == nil {
		if _, all := d.mySites(pub); len(all) > 0 {
			return all[0] // this node's names come first
		}
	}
	for _, n := range d.webNodes() {
		var st site.NodeStatus
		if json.Unmarshal(n.Info.Web, &st) == nil {
			for _, s := range st.Sites {
				if s.Site != "" && s.Serving != "" {
					return s.Site
				}
			}
		}
	}
	return ""
}

// RecoveryLink is the link that opens this person's files from anywhere.
func (d *Dashboard) recoveryLink(k *recovery.Keeper) (string, error) {
	host := d.linkHost()
	if host == "" {
		return "", errors.New("the group serves no website to put the link on")
	}
	ks := k.Keys()
	return "https://" + host + workhere.PagePath + "#" + ks.ID + "." + ks.LinkHalf, nil
}

// sendLink emails the link to the person's recovery address, once per
// address.
func (d *Dashboard) sendLink(ctx context.Context, k *recovery.Keeper) error {
	st := k.Status()
	if st.Email == "" || st.Sent == st.Email || st.Last == nil {
		return nil
	}
	if st.Box == "" {
		return errors.New("choose the box you work through when away first")
	}
	link, err := d.recoveryLink(k)
	if err != nil {
		return err
	}
	box, err := d.mailBox()
	if err != nil {
		return err
	}
	from := box.Address()
	if from == "" {
		return errors.New("you have no email address on the group to send it from (yggstore mail address you@example.org)")
	}
	text := workhere.LinkText(link)
	raw, rcpts, err := mail.Draft{From: from, Name: d.me(), To: st.Email, Subject: workhere.LinkSubject, Text: text}.Compose()
	if err != nil {
		return err
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if err := mail.Send(sctx, d.cfg.Client, list, raw, rcpts); err != nil {
		return err
	}
	if err := k.SetSent(st.Email); err != nil {
		return err
	}
	d.event("ok", "recovery link emailed to "+st.Email)
	return nil
}

// boxes are the members running a hub, found by asking each one's hub port.
func (d *Dashboard) boxes(ctx context.Context) []BoxChoice {
	d.mu.Lock()
	if time.Since(d.boxesAt) < 10*time.Minute && d.boxList != nil {
		out := d.boxList
		d.mu.Unlock()
		return out
	}
	ps := append([]PeerState(nil), d.state.Peers...)
	d.mu.Unlock()
	c := &http.Client{Timeout: 5 * time.Second}
	out := []BoxChoice{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range ps {
		if !p.Up || p.Test {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			host, _, err := net.SplitHostPort(p.Addr)
			if err != nil {
				return
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, workhere.HubPort)+"/v1/work", nil)
			resp, err := c.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			mu.Lock()
			out = append(out, BoxChoice{Name: p.Name, Addr: p.Addr})
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	d.mu.Lock()
	d.boxList, d.boxesAt = out, time.Now()
	d.mu.Unlock()
	return out
}

// BoxChoice is a box a person can work through.
type BoxChoice struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

func (d *Dashboard) handleBoxes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSONResp(w, d.boxes(ctx))
}
