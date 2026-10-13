package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

// A delete doesn't wait for nodes that are down: the item goes, and the
// shards they hold are owed. Those nodes are sent the deletes when they
// answer again, and their debt is dropped once they leave the group. The
// shards are encrypted with a key that was only in the stub, so nothing
// can be read from them meanwhile.

// owedRetry is how often nodes owed deletes are tried again.
const owedRetry = 10 * time.Minute

// probeTimeout is how long a node has to answer before a delete passes it by.
var probeTimeout = 10 * time.Second

func (w *Watcher) owedPath() string { return filepath.Join(w.cfg.Dir, privateDir, "owed-deletes.json") }

// loadOwed reads the shards owed deletes, by node address.
func (w *Watcher) loadOwed() map[string][]string {
	owed := map[string][]string{}
	if b, err := os.ReadFile(w.owedPath()); err == nil {
		json.Unmarshal(b, &owed)
	}
	return owed
}

func (w *Watcher) saveOwed(owed map[string][]string) error {
	if len(owed) == 0 {
		if err := os.Remove(w.owedPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(owed, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Replace(w.owedPath(), append(b, '\n'), 0o600)
}

// answering is the set of nodes holding shards of m that answer now.
func (w *Watcher) answering(ctx context.Context, m manifest.Manifest) map[string]bool {
	up := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, addr := range holders(m) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			if _, err := w.cfg.Client.Info(pctx, addr); err == nil {
				mu.Lock()
				up[addr] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return up
}

func holders(m manifest.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if !seen[ref.Peer] {
				seen[ref.Peer] = true
				out = append(out, ref.Peer)
			}
		}
	}
	sort.Strings(out)
	return out
}

// splitUp splits m's shards into those on nodes that answer and, by node,
// the hashes of those on nodes that don't.
func splitUp(m manifest.Manifest, up map[string]bool) (manifest.Manifest, map[string][]string) {
	here := manifest.Manifest{Version: m.Version}
	away := map[string][]string{}
	for _, ch := range m.Chunks {
		var refs []manifest.ShardRef
		for _, ref := range ch.Shards {
			if up[ref.Peer] {
				refs = append(refs, ref)
			} else {
				away[ref.Peer] = append(away[ref.Peer], ref.Hash)
			}
		}
		here.Chunks = append(here.Chunks, manifest.Chunk{Shards: refs})
	}
	return here, away
}

// owe adds deletes for nodes that are down.
func (w *Watcher) owe(away map[string][]string) error {
	owed := w.loadOwed()
	for addr, hashes := range away {
		have := map[string]bool{}
		for _, h := range owed[addr] {
			have[h] = true
		}
		for _, h := range hashes {
			if !have[h] {
				have[h] = true
				owed[addr] = append(owed[addr], h)
			}
		}
	}
	return w.saveOwed(owed)
}

// nodeNames maps node addresses to names, from the peer list.
func (w *Watcher) nodeNames() (map[string]string, bool) {
	if w.cfg.PeersPath == "" {
		return nil, false
	}
	list, err := peers.Load(w.cfg.PeersPath)
	if err != nil {
		return nil, false
	}
	names := map[string]string{}
	for _, p := range list {
		names[p.Addr] = p.Name
	}
	return names, true
}

// awayNote says which nodes still hold shards of a deleted item.
func (w *Watcher) awayNote(away map[string][]string) string {
	if len(away) == 0 {
		return ""
	}
	names, _ := w.nodeNames()
	var parts []string
	for _, addr := range sortedKeys(away) {
		n := names[addr]
		if n == "" {
			n = addr
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", n, len(away[addr])))
	}
	return fmt.Sprintf("; down, and sent the delete when back: %s", strings.Join(parts, ", "))
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// payOwed sends owed deletes to the nodes that answer, and drops the debts
// of nodes no longer in the group.
func (w *Watcher) payOwed(ctx context.Context) {
	if time.Now().Before(w.owedNext) {
		return
	}
	w.owedNext = time.Now().Add(owedRetry)
	w.work.Lock()
	defer w.work.Unlock()
	owed := w.loadOwed()
	if len(owed) == 0 {
		return
	}
	names, listed := w.nodeNames()
	changed := false
	for _, addr := range sortedKeys(owed) {
		name, inGroup := names[addr]
		if listed && !inGroup {
			w.cfg.Event("info", fmt.Sprintf("%s left the group: the %d shards of deleted items it held go with it", addr, len(owed[addr])))
			delete(owed, addr)
			changed = true
			continue
		}
		if name == "" {
			name = addr
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, err := w.cfg.Client.Info(pctx, addr)
		cancel()
		if err != nil {
			continue
		}
		left := owed[addr][:0:0]
		done := 0
		for i, h := range owed[addr] {
			if err := w.cfg.Client.Delete(ctx, addr, h); err != nil {
				left = append(left, owed[addr][i:]...)
				break
			}
			done++
		}
		if done == 0 {
			continue
		}
		changed = true
		if len(left) == 0 {
			delete(owed, addr)
			w.cfg.Event("ok", fmt.Sprintf("%s is back: deleted the %d shards of deleted items it still held", name, done))
		} else {
			owed[addr] = left
			w.cfg.Event("warn", fmt.Sprintf("%s is back: deleted %d shards of deleted items, %d to go", name, done, len(left)))
		}
	}
	if changed {
		if err := w.saveOwed(owed); err != nil {
			w.cfg.Event("warn", "could not save the deletes owed by nodes that are down: "+err.Error())
		}
	}
}
