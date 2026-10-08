// Package repair keeps stored items whole when nodes go away. A node that
// has been down longer than a grace period (a day by default), or that left
// the peer list, is taken as gone: each chunk with a shard there gets that
// shard rebuilt from the others and stored on a node that is up, and every
// stub naming the old place is rewritten. A shard missing from a node that
// is up (a wiped disk, say) is rebuilt straight away.
//
// It only looks after the owner's own items; shares received from others
// are theirs to repair. The old copies are left where they were, so a node
// that comes back still serves anyone holding an older stub.
package repair

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/erasure"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/lease"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

// DefaultGrace is how long a node may be down before its shards are
// rebuilt elsewhere.
const DefaultGrace = 24 * time.Hour

type Repairer struct {
	Client client.Client
	// Peers is the group's peer list.
	Peers func() ([]peers.Peer, error)
	// Stubs lists every stub to look after, older versions included.
	Stubs func() []string
	// Received lists the stubs of shares received: not this side's to
	// repair, but their leases are renewed (see Renew).
	Received func() []string
	// RenewOnly makes a pass only renew leases, repairing nothing.
	RenewOnly bool
	// State is where it remembers since when each node has been down.
	State string
	Grace time.Duration
	// Log reports what happened: kind is ok, warn or info.
	Log func(kind, msg string)

	mu sync.Mutex
}

type state struct {
	DownSince map[string]int64 `json:"down_since"`        // node address -> unix seconds
	Renewed   map[string]int64 `json:"renewed,omitempty"` // node address -> unix seconds, when its leases were last renewed
}

// Report is what one pass did.
type Report struct {
	Items    int // stubs looked at
	Rebuilt  int // shards rebuilt and stored again
	Spread   int // shards copied off crowded machines onto others
	Rewrote  int // stubs rewritten with the new places
	Waiting  int // shards on nodes down, but not for long enough yet
	Renewed  int // nodes whose leases were renewed
	Problems []string
}

func (r *Repairer) log(kind, format string, args ...any) {
	if r.Log != nil {
		r.Log(kind, fmt.Sprintf(format, args...))
	}
}

func (r *Repairer) load() state {
	st := state{DownSince: map[string]int64{}}
	if b, err := os.ReadFile(r.State); err == nil {
		json.Unmarshal(b, &st)
	}
	if st.DownSince == nil {
		st.DownSince = map[string]int64{}
	}
	if st.Renewed == nil {
		st.Renewed = map[string]int64{}
	}
	return st
}

func (r *Repairer) save(st state) {
	if r.State == "" {
		return
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := atomicfile.Replace(r.State, append(b, '\n'), 0o600); err != nil {
		r.log("warn", "repair: %v", err)
	}
}

// Run makes a pass every interval until ctx ends.
func (r *Repairer) Run(ctx context.Context, every time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		r.Pass(ctx)
	}
}

// Pass checks the nodes and every item once, repairing what it can.
func (r *Repairer) Pass(ctx context.Context) (Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var rep Report
	grace := r.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	list, err := r.Peers()
	if err != nil {
		return rep, err
	}

	// Who is up, and since when the others have been down.
	var members []peers.Peer
	for _, p := range list {
		if !p.Gateway {
			members = append(members, p)
		}
	}
	online := peers.Online(ctx, members, func(ctx context.Context, p peers.Peer) error {
		_, err := r.Client.Info(ctx, p.Addr)
		return err
	})
	if ctx.Err() != nil {
		return rep, ctx.Err()
	}
	up := map[string]bool{}
	for _, p := range online {
		up[p.Addr] = true
	}
	st := r.load()
	now := time.Now()
	inList := map[string]bool{}
	for _, p := range members {
		inList[p.Addr] = true
		if up[p.Addr] {
			delete(st.DownSince, p.Addr)
		} else if st.DownSince[p.Addr] == 0 {
			st.DownSince[p.Addr] = now.Unix()
		}
	}
	for addr := range st.DownSince {
		if !inList[addr] {
			delete(st.DownSince, addr)
		}
	}
	for addr := range st.Renewed {
		if !inList[addr] {
			delete(st.Renewed, addr)
		}
	}
	r.renew(ctx, online, &st, now, &rep)
	r.save(st)
	if r.RenewOnly {
		return rep, nil
	}

	// If most machines seem down, the trouble is more likely this side's
	// network than theirs: don't move anything.
	machines, upMachines := map[string]bool{}, map[string]bool{}
	for _, p := range members {
		machines[p.Machine()] = true
		if up[p.Addr] {
			upMachines[p.Machine()] = true
		}
	}
	if len(upMachines)*2 < len(machines) {
		msg := fmt.Sprintf("only %d of %d machines answer; not repairing until more are back", len(upMachines), len(machines))
		rep.Problems = append(rep.Problems, msg)
		r.log("warn", "repair: %s", msg)
		return rep, nil
	}
	gone := func(addr string) bool {
		if !inList[addr] {
			return !isGateway(list, addr)
		}
		since := st.DownSince[addr]
		return since != 0 && now.Sub(time.Unix(since, 0)) >= grace
	}

	// Gather the chunks, once each: versions of an item share unchanged
	// chunks.
	type item struct {
		path  string
		m     manifest.Manifest
		mtime time.Time
	}
	type work struct {
		layout erasure.Layout
		chunk  manifest.Chunk
		lease  string
	}
	var items []item
	chunks := map[string]work{}
	for _, path := range r.Stubs() {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		m, err := files.ReadStub(path)
		if err != nil || m.Version != 2 || m.SharedBy != nil {
			continue
		}
		layout, err := erasure.NewLayout(m.DataShards, m.ParityShards)
		if err != nil {
			continue
		}
		items = append(items, item{path, m, info.ModTime()})
		for _, ch := range m.Chunks {
			chunks[chunkKey(ch)] = work{layout, ch, lease.ForKey(m.Key)}
		}
	}
	rep.Items = len(items)

	// Which shards need rebuilding: those on gone nodes, and those a node
	// that is up no longer has.
	type job struct {
		key     string
		w       work
		bad     []int
		crowded bool
	}
	var jobs []job
	var jmu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for key, w := range chunks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var bad []int
			waiting := 0
			for i, ref := range w.chunk.Shards {
				switch {
				case gone(ref.Peer):
					bad = append(bad, i)
				case !up[ref.Peer]:
					waiting++
				default:
					sem <- struct{}{}
					has, err := r.Client.Has(ctx, ref.Peer, ref.Hash)
					<-sem
					if err == nil && !has {
						bad = append(bad, i)
					}
				}
			}
			crowded := files.Crowded(w.layout, w.chunk, online, list)
			jmu.Lock()
			rep.Waiting += waiting
			if len(bad) > 0 || crowded {
				jobs = append(jobs, job{key, w, bad, crowded})
			}
			jmu.Unlock()
		}()
	}
	wg.Wait()
	if len(jobs) == 0 {
		return rep, nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].key < jobs[j].key })

	fixed := map[string]manifest.Chunk{}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		out := j.w.chunk
		if len(j.bad) > 0 {
			var n int
			var err error
			out, n, err = files.RepairChunk(ctx, r.Client, j.w.layout, out, j.bad, j.w.lease, online, list)
			rep.Rebuilt += n
			if err != nil {
				rep.Problems = append(rep.Problems, err.Error())
			}
		}
		// Spread chunks crowded onto few machines (stored while others were
		// down, say) now that more are up.
		if files.Crowded(j.w.layout, out, online, list) {
			spread, n, err := files.SpreadChunk(ctx, r.Client, j.w.layout, out, j.w.lease, online, list)
			rep.Spread += n
			out = spread
			if err != nil {
				rep.Problems = append(rep.Problems, err.Error())
			}
		}
		if chunkKey(out) != j.key {
			fixed[j.key] = out
		}
	}

	// Rewrite the stubs that name the old places, unless one changed
	// meanwhile (a new version, say); the next pass gets it then.
	for _, it := range items {
		changed := false
		for i, ch := range it.m.Chunks {
			if nc, ok := fixed[chunkKey(ch)]; ok {
				it.m.Chunks[i] = nc
				changed = true
			}
		}
		if !changed {
			continue
		}
		if info, err := os.Stat(it.path); err != nil || !info.ModTime().Equal(it.mtime) {
			rep.Problems = append(rep.Problems, it.path+" changed while it was repaired; trying again next time")
			continue
		}
		if err := files.WriteJSON(it.path, it.m); err != nil {
			rep.Problems = append(rep.Problems, err.Error())
			continue
		}
		rep.Rewrote++
	}
	if rep.Rebuilt > 0 {
		r.log("ok", "repair: rebuilt %d shard(s) on nodes that are up, in %d item(s)", rep.Rebuilt, rep.Rewrote)
	}
	if rep.Spread > 0 {
		r.log("ok", "repair: spread %d shard(s) over more machines, in %d item(s)", rep.Spread, rep.Rewrote)
	}
	if len(rep.Problems) > 0 {
		r.log("warn", "repair: %s", strings.Join(rep.Problems, "; "))
	}
	return rep, nil
}

func chunkKey(ch manifest.Chunk) string {
	var b strings.Builder
	for _, s := range ch.Shards {
		b.WriteString(s.Hash)
		b.WriteString(s.Peer)
	}
	return b.String()
}

func isGateway(list []peers.Peer, addr string) bool {
	for _, p := range list {
		if p.Addr == addr {
			return p.Gateway
		}
	}
	return false
}

// Find lists the stubs under dirs: stored items and their older versions,
// leaving out hidden folders and an outbox's restore/, delete/ and
// restored/ (requests and rebuilt files, not stored items).
func Find(dirs ...string) []string {
	var out []string
	for _, root := range dirs {
		if root == "" {
			continue
		}
		filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() && path != root {
				name := e.Name()
				top := filepath.Dir(path) == root
				if strings.HasPrefix(name, ".") || (top && (name == "restore" || name == "delete" || name == "restored")) {
					return filepath.SkipDir
				}
			}
			if !e.IsDir() && strings.HasSuffix(e.Name(), files.StubExt) {
				out = append(out, path)
			}
			return nil
		})
	}
	return out
}
