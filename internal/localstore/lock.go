package localstore

import (
	"context"
	"github.com/peterretief/yggstore/internal/filelock"
	"path/filepath"
)

func (s Store) lock() (func(), error) {
	return filelock.Acquire(context.Background(), filepath.Join(s.dir, ".dstore.lock"))
}
