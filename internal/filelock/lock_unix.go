//go:build !windows

package filelock

import (
	"errors"
	"os"
	"syscall"
)

func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
func unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
