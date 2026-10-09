//go:build !windows

package devices

import (
	"os"
	"syscall"
)

type owner struct {
	uid, gid int
	ok       bool
}

func ownerOf(path string) owner {
	info, err := os.Stat(path)
	if err != nil {
		return owner{}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return owner{}
	}
	return owner{uid: int(st.Uid), gid: int(st.Gid), ok: true}
}

func (o owner) restore(path string) {
	if o.ok && (o.uid != os.Getuid() || o.gid != os.Getgid()) {
		os.Chown(path, o.uid, o.gid)
	}
}
