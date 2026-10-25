//go:build !windows

package join

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// Elsewhere the installer is only for tests: people on Linux and Mac use
// connect.sh, which also starts Syncthing at sign-in.

func syncthingDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

func findSyncthing() string {
	if p, err := exec.LookPath("syncthing"); err == nil {
		return p
	}
	for _, p := range []string{filepath.Join(syncthingDir(), "syncthing"), "/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func startAtSignIn(st, box string) error {
	return errors.New("not on " + runtime.GOOS + ": use the box's connect.sh")
}

func hide(cmd *exec.Cmd) {}

func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func OpenFolder(path string) {}
