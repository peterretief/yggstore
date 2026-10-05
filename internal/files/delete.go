package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/manifest"
)

// DeleteResult says how a delete went. Failed lists one line per shard that
// could not be deleted (node down, or not the writer).
type DeleteResult struct {
	Deleted int
	Failed  []string
}

func (r DeleteResult) Err() error {
	if len(r.Failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d shards not deleted:\n  %s", len(r.Failed), r.Deleted+len(r.Failed), strings.Join(r.Failed, "\n  "))
}

// Delete removes every shard of m from its holders, 8 at a time and at most
// perPeer per node, so busy nodes don't turn requests away. A shard that
// is already gone counts as deleted, so a retry after a partial failure only
// has the leftovers to do.
func Delete(ctx context.Context, c client.Client, m manifest.Manifest) DeleteResult {
	var res DeleteResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	lim := newPeerLimit()
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				err := lim.acquire(ctx, ref.Peer)
				if err == nil {
					err = c.Delete(ctx, ref.Peer, ref.Hash)
					lim.release(ref.Peer)
				}
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					res.Failed = append(res.Failed, fmt.Sprintf("%s %s: %v", ref.Peer, ref.Hash[:12], err))
				} else {
					res.Deleted++
				}
			}()
		}
	}
	wg.Wait()
	sort.Strings(res.Failed)
	return res
}

// DeleteStub deletes the item a stub describes. Only once every shard is gone
// are the stub and its challenges removed; otherwise they are kept so the
// delete can be retried and no shard is left without a record.
func DeleteStub(ctx context.Context, c client.Client, stub string) (manifest.Manifest, DeleteResult, error) {
	m, err := ReadStub(stub)
	if err != nil {
		return m, DeleteResult{}, err
	}
	res := Delete(ctx, c, m)
	if err := res.Err(); err != nil {
		return m, res, err
	}
	if err := os.Remove(ChallengesPath(stub)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return m, res, err
	}
	return m, res, os.Remove(stub)
}
