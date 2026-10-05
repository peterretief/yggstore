package localstore

import (
	"errors"
	"os"
	"path/filepath"
)

// Stats returns the number of shards and their total size.
func (s Store) Stats() (count int, bytes int64, err error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if validateHash(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, 0, err
		}
		count++
		bytes += info.Size()
	}
	return count, bytes, nil
}

// Has reports whether a shard file exists, without reading or verifying it.
func (s Store) Has(hash string) (bool, error) {
	if err := validateHash(hash); err != nil {
		return false, err
	}
	_, err := os.Stat(filepath.Join(s.dir, hash))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// UsageByWriter sums shard bytes per writer (the node that uploaded them). Shards
// stored without a writer are counted under "".
func (s Store) UsageByWriter() (map[string]int64, error) {
	out := map[string]int64{}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if validateHash(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // deleted meanwhile
		}
		writer, _ := os.ReadFile(filepath.Join(s.dir, e.Name()+".writer"))
		out[string(writer)] += info.Size()
	}
	return out, nil
}
