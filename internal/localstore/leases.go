package localstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/lease"
)

// Leases (see package lease): beside each shard, HASH.leases lists the
// leases it is stored under, one ID a line. leases/ID is a lease's record,
// its modification time when it was last renewed (or a shard stored under
// it). Renewing only touches records that exist, so callers can't fill
// the disk with them. leases/writers.json is when each writer last renewed,
// which keeps shards stored before leases (with none listed) wanted.
// leases/.since is when this store started keeping track: nothing counts
// as unrenewed from before then.

const leasesExt = ".leases"

func (s Store) leaseDir() string { return filepath.Join(s.dir, "leases") }

func (s Store) startLeases() error {
	if err := os.MkdirAll(s.leaseDir(), 0o755); err != nil {
		return err
	}
	since := filepath.Join(s.leaseDir(), ".since")
	if _, err := os.Stat(since); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(since, nil, 0o600)
	}
	return nil
}

func readLeases(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

// addLease lists a lease beside a shard and marks it renewed now. The
// caller holds the lock.
func (s Store) addLease(hash, id string) error {
	if id == "" {
		return nil
	}
	if err := s.startLeases(); err != nil {
		return err
	}
	path := filepath.Join(s.dir, hash+leasesExt)
	if have := readLeases(path); !slices.Contains(have, id) {
		if err := atomicfile.Replace(path, []byte(strings.Join(append(have, id), "\n")+"\n"), 0o600); err != nil {
			return err
		}
	}
	rec := filepath.Join(s.leaseDir(), id)
	info, err := os.Stat(rec)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.WriteFile(rec, nil, 0o600)
	case err != nil:
		return err
	case time.Since(info.ModTime()) < time.Hour: // storing a file's shards needn't touch it for each
		return nil
	}
	now := time.Now()
	return os.Chtimes(rec, now, now)
}

// MaxRenew is the most tokens one renewal may carry.
const MaxRenew = 10000

// Renew marks renewed the leases of the tokens given that are held here,
// and writer as having renewed (which keeps its shards from before leases
// wanted). It returns how many of the leases are held here.
func (s Store) Renew(writer string, tokens []string) (int, error) {
	if len(tokens) > MaxRenew {
		return 0, errors.New("too many tokens at once")
	}
	if err := s.startLeases(); err != nil {
		return 0, err
	}
	now := time.Now()
	known := 0
	for _, t := range tokens {
		if !lease.Valid(t) {
			continue
		}
		err := os.Chtimes(filepath.Join(s.leaseDir(), lease.ID(t)), now, now)
		if err == nil {
			known++
		} else if !errors.Is(err, os.ErrNotExist) {
			return known, err
		}
	}
	if err := s.pruneDaily(); err != nil {
		return known, err
	}
	if writer == "" {
		return known, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.writers()
	w[writer] = now.Unix()
	data, _ := json.MarshalIndent(w, "", "  ")
	return known, atomicfile.Replace(filepath.Join(s.leaseDir(), "writers.json"), append(data, '\n'), 0o600)
}

// pruneDaily runs PruneLeases if it hasn't run for a day.
func (s Store) pruneDaily() error {
	mark := filepath.Join(s.leaseDir(), ".pruned")
	if info, err := os.Stat(mark); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return nil
	}
	if err := s.PruneLeases(); err != nil {
		return err
	}
	return os.WriteFile(mark, nil, 0o600)
}

// PruneLeases removes the records of leases no shard here is stored under
// any more (their shards were deleted).
func (s Store) PruneLeases() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock() // no shard stored meanwhile under a lease about to go
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, e := range entries {
		if hash, ok := strings.CutSuffix(e.Name(), leasesExt); ok && validateHash(hash) == nil {
			for _, id := range readLeases(filepath.Join(s.dir, e.Name())) {
				used[id] = true
			}
		}
	}
	recs, err := os.ReadDir(s.leaseDir())
	if err != nil {
		return err
	}
	for _, r := range recs {
		if lease.Valid(r.Name()) && !used[r.Name()] {
			if err := os.Remove(filepath.Join(s.leaseDir(), r.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s Store) writers() map[string]int64 {
	w := map[string]int64{}
	if data, err := os.ReadFile(filepath.Join(s.leaseDir(), "writers.json")); err == nil {
		json.Unmarshal(data, &w)
	}
	return w
}

// Holding is a shard's standing: who stored it, and when it was last
// wanted.
type Holding struct {
	Hash   string
	Size   int64
	Writer string
	Leases int // leases it is stored under; 0 if stored before leases
	// Renewed is the last renewal of any of its leases (of its writer, if
	// it has none, or when it was stored), and never before the store
	// started keeping track.
	Renewed time.Time
}

// Holdings lists every shard's standing. It changes nothing (an operator
// may run it as another user).
func (s Store) Holdings() ([]Holding, error) {
	var since time.Time
	if info, err := os.Stat(filepath.Join(s.leaseDir(), ".since")); err == nil {
		since = info.ModTime()
	}
	s.mu.Lock()
	writers := s.writers()
	s.mu.Unlock()
	records := map[string]time.Time{}
	renewed := func(id string) time.Time {
		t, ok := records[id]
		if !ok && lease.Valid(id) {
			if info, err := os.Stat(filepath.Join(s.leaseDir(), id)); err == nil {
				t = info.ModTime()
			}
			records[id] = t
		}
		return t
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Holding
	for _, e := range entries {
		if validateHash(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // deleted meanwhile
		}
		path := filepath.Join(s.dir, e.Name())
		writer, _ := os.ReadFile(path + ".writer")
		h := Holding{Hash: e.Name(), Size: info.Size(), Writer: string(writer), Renewed: since}
		ids := readLeases(path + leasesExt)
		h.Leases = len(ids)
		latest := func(t time.Time) {
			if t.After(h.Renewed) {
				h.Renewed = t
			}
		}
		if len(ids) == 0 {
			latest(info.ModTime())
			if t := writers[h.Writer]; t != 0 {
				latest(time.Unix(t, 0))
			}
		}
		for _, id := range ids {
			latest(renewed(id))
		}
		out = append(out, h)
	}
	return out, nil
}
