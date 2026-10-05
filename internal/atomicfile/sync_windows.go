package atomicfile

import "os"

// Windows does not support File.Sync on directory handles. File data is synced
// before publication; the filesystem manages directory metadata persistence.
func SyncDirectory(dir *os.File) error { return nil }
