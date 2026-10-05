package localstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Sweep verifies content-addressed shards without modifying data or owner records.
// It streams one shard at a time and does not hold the storage write lock.
// onIssue is called synchronously for each damaged or unreadable shard.
func (s Store) Sweep(ctx context.Context, onIssue func(string, error)) (checked, failed int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	dir, err := os.Open(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer dir.Close()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return checked, failed, err
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return checked, failed, err
			}
			if validateHash(entry.Name()) != nil {
				continue
			}
			issue := s.verifyShard(ctx, entry, buffer)
			if errors.Is(issue, context.Canceled) || errors.Is(issue, context.DeadlineExceeded) {
				return checked, failed, issue
			}
			// A concurrent owner-authorized deletion is not corruption.
			if errors.Is(issue, os.ErrNotExist) {
				continue
			}
			checked++
			if issue != nil {
				failed++
				if onIssue != nil {
					onIssue(entry.Name(), issue)
				}
			}
		}
		if readErr == io.EOF {
			return checked, failed, nil
		}
		if readErr != nil {
			return checked, failed, readErr
		}
	}
}

func (s Store) verifyShard(ctx context.Context, entry os.DirEntry, buffer []byte) error {
	if !entry.Type().IsRegular() {
		return fmt.Errorf("shard is not a regular file")
	}
	path := filepath.Join(s.dir, entry.Name())
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("shard is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, opened) {
		return fmt.Errorf("shard changed during verification")
	}
	hash := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := f.Read(buffer)
		hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != entry.Name() {
		return fmt.Errorf("shard hash mismatch: got %s want %s", got, entry.Name())
	}
	return nil
}
