package files

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/erasure"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

// RepairChunk rebuilds the shards of ch at the indexes in bad from the
// others and stores them on online peers, returning the chunk with their
// new places. It works on the encrypted shards, so it needs no key. A
// rebuilt shard is the original byte for byte (its hash is checked), so
// challenges prepared for it still hold.
//
// New places follow the rules for storing (see LayoutFor): a peer the
// chunk doesn't use yet, on a machine below its share of the chunk, fast
// peers before slow ones. A shard with nowhere like that to go is left as
// it was, and its error returned with the shards that were placed.
func RepairChunk(ctx context.Context, c client.Client, layout erasure.Layout, ch manifest.Chunk, bad []int, online []peers.Peer) (manifest.Chunk, int, error) {
	if len(ch.Shards) != layout.TotalShards() {
		return ch, 0, fmt.Errorf("chunk has %d shards, its layout %d", len(ch.Shards), layout.TotalShards())
	}
	isBad := map[int]bool{}
	for _, i := range bad {
		isBad[i] = true
	}
	shards := make([][]byte, len(ch.Shards))
	var mu sync.Mutex
	var wg sync.WaitGroup
	have := 0
	for i, ref := range ch.Shards {
		if isBad[i] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := c.Get(ctx, ref.Peer, ref.Hash)
			if err != nil {
				return
			}
			mu.Lock()
			shards[i] = data
			have++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if have < layout.DataShards {
		return ch, 0, fmt.Errorf("only %d of %d shards reachable, need %d", have, len(ch.Shards), layout.DataShards)
	}
	if err := layout.Reconstruct(shards); err != nil {
		return ch, 0, err
	}
	for i, s := range shards {
		if manifest.Hash(s) != ch.Shards[i].Hash {
			return ch, 0, fmt.Errorf("shard %d rebuilt wrong; not storing it", i)
		}
	}

	byAddr := map[string]peers.Peer{}
	for _, p := range online {
		byAddr[p.Addr] = p
	}
	machineOf := func(addr string) string {
		if p, ok := byAddr[addr]; ok {
			return p.Machine()
		}
		return addr
	}
	used := map[string]bool{}
	onMachine := map[string]int{}
	for i, ref := range ch.Shards {
		if !isBad[i] {
			used[ref.Peer] = true
			onMachine[machineOf(ref.Peer)]++
		}
	}
	limit := PerMachine(layout, machineCount(online))
	out := ch
	out.Shards = append([]manifest.ShardRef(nil), ch.Shards...)
	placed := 0
	var errs []error
	for _, i := range bad {
		var cands []peers.Peer
		for _, p := range online {
			if !p.Gateway && !used[p.Addr] && onMachine[p.Machine()] < limit {
				cands = append(cands, p)
			}
		}
		sort.SliceStable(cands, func(a, b int) bool {
			ma, mb := onMachine[cands[a].Machine()], onMachine[cands[b].Machine()]
			if ma != mb {
				return ma < mb
			}
			return !cands[a].Slow && cands[b].Slow
		})
		ok := false
		var lastErr error = errors.New("no peer to hold it: every machine online has its share of this chunk")
		for _, p := range cands {
			hash, err := c.Put(ctx, p.Addr, shards[i])
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", p.Name, err)
				continue
			}
			out.Shards[i] = manifest.ShardRef{Hash: hash, Peer: p.Addr, URL: client.ShardURL(p.Addr, hash)}
			used[p.Addr] = true
			onMachine[p.Machine()]++
			placed++
			ok = true
			break
		}
		if !ok {
			errs = append(errs, fmt.Errorf("shard %d: %w", i, lastErr))
		}
	}
	return out, placed, errors.Join(errs...)
}
