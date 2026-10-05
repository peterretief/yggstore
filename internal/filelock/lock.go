// Package filelock provides cancellable advisory locks across processes.
package filelock

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

func Acquire(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		locked, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if locked {
			return func() { unlock(f); f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
