//go:build !windows

package mesh

import (
	"os"
	"syscall"
)

// keepOwner gives path the owner and group fi had, so a file rewritten by
// root stays readable by the group it was shared with (Yggdrasil runs as
// the yggdrasil group and reads its settings through it).
func keepOwner(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Lchown(path, int(st.Uid), int(st.Gid))
}
