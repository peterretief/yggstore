package hub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/workcopy"
	"github.com/peterretief/yggstore/internal/workhere"
)

// A person who came through their recovery link works through the box: it
// is one of their places (see package workcopy). The box asks the place
// holding their working copy to hand it over, claims it, keeps making
// recovery copies of their folder, and keeps their record on the web nodes
// up to date, so they can move on again, or back.

// place is the box as one of a person's places.
type place struct {
	tracker *workcopy.Tracker
	keeper  *recovery.Keeper
	cancel  context.CancelFunc
}

// handOverWait is how long the box waits for the place holding a person's
// working copy to hand it over before taking over from the latest copy.
var handOverWait = 90 * time.Second

func homeOf(p Person) string { return filepath.Join(p.Path, ".yggstore", "home") }

// startPlace starts the box's tracker and recovery copies for a person,
// once their sharing key is in their folder.
func (u *Users) startPlace(ctx context.Context, p Person) (*place, error) {
	u.mu.Lock()
	if pl := u.places[p.Name]; pl != nil {
		u.mu.Unlock()
		return pl, nil
	}
	u.mu.Unlock()
	if u.cfg.Bus == nil {
		return nil, fmt.Errorf("the box can't reach its node's messaging")
	}
	id, err := share.Load(filepath.Join(homeOf(p), "sharing.key"))
	if err != nil {
		return nil, fmt.Errorf("%s's sharing key: %w", p.Name, err)
	}
	host, _ := os.Hostname()
	pctx, cancel := context.WithCancel(ctx)
	pl := &place{cancel: cancel}
	pl.keeper = recovery.New(recovery.Config{
		Dir:    filepath.Join(homeOf(p), "recovery"),
		Client: u.cfg.Client,
		Sources: []recovery.Source{
			{Name: "outbox", Path: p.Path, Keep: recovery.OutboxKeep},
			{Name: "home", Path: homeOf(p), Keep: recovery.HomeKeep},
			{Name: "peers.json", Path: u.cfg.PeersPath},
		},
		Online:  func(ctx context.Context) ([]peers.Peer, error) { return u.online(ctx) },
		Event:   func(kind, msg string) { u.cfg.Logf("%s: %s", p.Name, msg) },
		Holding: func() bool { return pl.tracker.Holding() },
		OnCopy: func(ctx context.Context, m manifest.Manifest) {
			if err := pl.tracker.Copied(ctx, m); err != nil {
				u.cfg.Logf("%s: telling their other places of the new copy: %v", p.Name, err)
			}
			u.keepRecord(ctx, p, pl.keeper)
		},
	})
	pl.tracker = workcopy.New(workcopy.Config{
		Bus:     u.cfg.Bus,
		ID:      func() *share.Identity { return id },
		Place:   func() string { return host + "/" + p.Name },
		SaveNow: pl.keeper.SaveNow,
		Event:   func(kind, msg string) { u.cfg.Logf("%s: %s", p.Name, msg) },
	})
	go pl.tracker.Run(pctx)
	go func() {
		if err := pl.keeper.Run(pctx); err != nil {
			u.cfg.Logf("%s: recovery copies: %v", p.Name, err)
		}
	}()
	u.mu.Lock()
	if u.places == nil {
		u.places = map[string]*place{}
	}
	u.places[p.Name] = pl
	u.mu.Unlock()
	return pl, nil
}

// Holding is whether the box holds a person's working copy, and so may
// store, restore and delete in their folder. People who didn't come
// through a recovery link have no other place, so the box always does.
func (u *Users) Holding(person string) bool {
	u.mu.Lock()
	pl := u.places[person]
	u.mu.Unlock()
	return pl == nil || pl.tracker.Holding()
}

// takeOver asks the place holding the person's working copy to hand it to
// the box, and waits a while for its final copy. It returns that copy's
// stub, or nil if none came in time (the latest copy is used then).
func (u *Users) takeOver(ctx context.Context, pl *place) *manifest.Manifest {
	if err := pl.tracker.Step(ctx); err != nil {
		u.cfg.Logf("hub: reading where the working copy is: %v", err)
		return nil
	}
	if pl.tracker.State().Mine {
		return nil
	}
	if err := pl.tracker.Ask(ctx); err != nil {
		u.cfg.Logf("hub: asking for the working copy: %v", err)
		return nil
	}
	deadline := time.Now().Add(handOverWait)
	for time.Now().Before(deadline) {
		pl.tracker.Step(ctx)
		if m, ok := pl.tracker.HandedHere(); ok {
			return &m
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

// keepRecord puts the person's record on the web nodes, naming this box.
func (u *Users) keepRecord(ctx context.Context, p Person, k *recovery.Keeper) {
	if u.cfg.WebNodes == nil || u.cfg.SelfAddr == "" {
		return
	}
	ks := k.Keys()
	if len(ks.Sealed) == 0 {
		return
	}
	rec := workhere.Record{ID: ks.ID, GroupHalf: ks.GroupHalf, Sealed: ks.Sealed, Box: u.cfg.SelfAddr, Name: p.Name}
	c := u.cfg.Client.HTTP
	for _, n := range u.cfg.WebNodes(ctx) {
		if err := workhere.Put(ctx, c, n, rec); err != nil {
			u.cfg.Logf("%s: keeping their record on %s: %v", p.Name, n, err)
		}
	}
}

func (u *Users) online(ctx context.Context) ([]peers.Peer, error) {
	if u.cfg.Online == nil {
		return nil, fmt.Errorf("the box doesn't know the group's nodes")
	}
	return u.cfg.Online(ctx)
}
