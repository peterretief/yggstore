package atomicfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteNew publishes a complete, synced file without replacing an existing path.
func WriteNew(path string, data []byte, mode os.FileMode) error {
	return WriteNewFrom(path, mode, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteNewFrom publishes data produced by write without replacing an existing path.
// The callback writes directly to a temporary file, so callers can avoid buffering
// an entire output in memory.
func WriteNewFrom(path string, mode os.FileMode, write func(io.Writer) error) error {
	if write == nil {
		return fmt.Errorf("write callback is required")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".dstore-write-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return SyncDirectory(dir)
}
