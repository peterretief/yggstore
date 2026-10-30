package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// rawTerminal makes f give each key as it's pressed, without echoing it
// and without Ctrl-C stopping the screen.
func rawTerminal(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG | unix.IEXTEN
	t.Iflag &^= unix.IXON | unix.ICRNL
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &t); err != nil {
		return nil, err
	}
	return func() { unix.IoctlSetTermios(fd, unix.TCSETS, old) }, nil
}
