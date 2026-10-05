//go:build !windows

package atomicfile

import "os"

func SyncDirectory(dir *os.File) error { return dir.Sync() }
