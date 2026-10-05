package localstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/manifest"
)

var ErrInvalidHash = errors.New("invalid shard hash")
var ErrQuota = errors.New("storage quota exceeded")
var ErrNotOwner = errors.New("only the original writer can delete this shard")

type Store struct {
	dir   string
	quota int64
	mu    *sync.Mutex
}

func New(dir string) Store {
	return WithQuota(dir, 0)
}

func WithQuota(dir string, quota int64) Store {
	return Store{dir: dir, quota: quota, mu: &sync.Mutex{}}
}
func (s Store) Quota() int64 { return s.quota }

func (s Store) Put(shard []byte) (string, error) {
	return s.PutOwned(shard, "")
}

func (s Store) PutOwned(shard []byte, writer string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	hash := manifest.Hash(shard)
	if err := validateHash(hash); err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", fmt.Errorf("create store: %w", err)
	}

	path := filepath.Join(s.dir, hash)
	existing, err := os.ReadFile(path)
	if err == nil {
		if manifest.Hash(existing) != hash {
			return "", fmt.Errorf("existing shard hash mismatch for %s", hash)
		}
		return hash, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read existing shard: %w", err)
	}
	used, err := s.usage()
	if err != nil {
		return "", err
	}
	if s.quota > 0 && int64(len(shard)) > s.quota-used {
		return "", ErrQuota
	}
	if err := atomicfile.WriteNew(path, shard, 0o600); err != nil {
		return "", fmt.Errorf("write shard: %w", err)
	}
	if writer != "" {
		if err := atomicfile.WriteNew(path+".writer", []byte(writer), 0o600); err != nil {
			os.Remove(path)
			return "", fmt.Errorf("write shard owner: %w", err)
		}
	}
	return hash, nil
}

func (s Store) Usage() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	return s.usage()
}

func (s Store) usage() (int64, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if validateHash(entry.Name()) != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func (s Store) DeleteOwned(hash, writer string) error {
	if err := validateHash(hash); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(s.dir, hash)
	owner, err := os.ReadFile(path + ".writer")
	if err != nil {
		return err
	}
	if writer == "" || string(owner) != writer {
		return ErrNotOwner
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(path + ".writer")
}

func (s Store) Dir() string {
	return s.dir
}

func (s Store) Get(hash string) ([]byte, error) {
	if err := validateHash(hash); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, hash))
	if err != nil {
		return nil, fmt.Errorf("read shard %s: %w", hash, err)
	}
	if got := manifest.Hash(data); got != hash {
		return nil, fmt.Errorf("shard hash mismatch: got %s want %s", got, hash)
	}
	return data, nil
}

func validateHash(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("%w: length %d", ErrInvalidHash, len(hash))
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	return nil
}
