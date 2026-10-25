package join

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const (
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008
	newProcessGroup = 0x00000200
)

func syncthingDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Syncthing")
}

func findSyncthing() string {
	for _, p := range []string{
		filepath.Join(syncthingDir(), "syncthing.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Syncthing", "syncthing.exe"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("syncthing.exe"); err == nil {
		return p
	}
	return ""
}

// startAtSignIn starts Syncthing whenever the person signs in, for them
// only, so no administrator is needed. A shortcut from connect.ps1 does the
// same already.
func startAtSignIn(st, box string) error {
	lnk := filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs\Startup\Syncthing.lnk`)
	if _, err := os.Stat(lnk); err == nil {
		return nil
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("Syncthing", `"`+st+`" serve --no-browser --no-console --no-restart`)
}

// hide keeps a command's console window from flashing up.
func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// detach starts Syncthing on its own, with no window, so it keeps running
// once this window is closed.
func detach(cmd *exec.Cmd) {
	cmd.Args = append(cmd.Args, "--no-console")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedProcess | newProcessGroup}
}

// OpenFolder shows a folder in Explorer.
func OpenFolder(path string) { exec.Command("explorer.exe", path).Start() }
