package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Network drives (docs/box-setup.md): on a box with Samba set up for it
// (/etc/samba/yggstore.d exists, and smb.conf includes the shares file and
// listens on the local network only), each person who chooses their
// password on the box gets their own folder as a network drive, signed in
// with that password. Samba keeps its own copy of it: a password changed
// later on the web doesn't reach the drive, so it's chosen on the box
// again.

const (
	sambaDir    = "/etc/samba/yggstore.d"
	sambaShares = "/etc/samba/yggstore-shares.conf"
	sambaGecos  = "yggstore network drive"
)

var sambaName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func sambaOn() bool {
	fi, err := os.Stat(sambaDir)
	return err == nil && fi.IsDir()
}

// sambaShare gives name their folder at path as a network drive, with pw.
func sambaShare(name, path, pw string) error {
	name = strings.ToLower(name)
	if !sambaName.MatchString(name) {
		return fmt.Errorf("%q can't be a network drive's sign-in name: letters, digits and dashes only", name)
	}
	if path == "" || strings.ContainsAny(path, "\n[]") {
		return errors.New("their folder isn't known")
	}
	if exec.Command("id", "-u", name).Run() != nil {
		if out, err := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin",
			"--comment", sambaGecos, name).CombinedOutput(); err != nil {
			return fmt.Errorf("useradd: %s", strings.TrimSpace(string(out)))
		}
	}
	cmd := exec.Command("smbpasswd", "-s", "-a", name)
	cmd.Stdin = strings.NewReader(pw + "\n" + pw + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("smbpasswd: %s", strings.TrimSpace(string(out)))
	}
	exec.Command("smbpasswd", "-e", name).Run()
	// The files stay root's, as Syncthing writes them; Syncthing's own
	// files in the folder can't be opened or deleted from the drive.
	share := fmt.Sprintf(`[%s]
   path = %s
   valid users = %s
   force user = root
   force group = root
   read only = no
   create mask = 0644
   directory mask = 0755
   veto files = /.stfolder/.stignore/.stversions/
`, name, path, name)
	if err := os.WriteFile(filepath.Join(sambaDir, name+".conf"), []byte(share), 0o644); err != nil {
		return err
	}
	return sambaReload()
}

// sambaEnable turns someone's drive off while they're shut out, and back on.
func sambaEnable(name string, on bool) {
	name = strings.ToLower(name)
	if !sambaOn() || !sambaName.MatchString(name) {
		return
	}
	if _, err := os.Stat(filepath.Join(sambaDir, name+".conf")); err != nil {
		return
	}
	flag := "-d"
	if on {
		flag = "-e"
	}
	exec.Command("smbpasswd", flag, name).Run()
}

// sambaRemove takes someone's drive away. Their files stay.
func sambaRemove(name string) {
	name = strings.ToLower(name)
	if !sambaOn() || !sambaName.MatchString(name) {
		return
	}
	if err := os.Remove(filepath.Join(sambaDir, name+".conf")); err != nil {
		return
	}
	sambaReload()
	exec.Command("smbpasswd", "-x", name).Run()
	// Only an account made for the drive goes; never a login of the box's.
	if out, err := exec.Command("getent", "passwd", name).Output(); err == nil && strings.Split(string(out), ":")[4] == sambaGecos {
		exec.Command("userdel", name).Run()
	}
}

// sambaReload writes the shares file from the per-person ones, and tells
// Samba.
func sambaReload() error {
	files, _ := filepath.Glob(filepath.Join(sambaDir, "*.conf"))
	sort.Strings(files)
	var b strings.Builder
	b.WriteString("# Written by yggstore admin from " + sambaDir + "; don't edit.\n")
	for _, f := range files {
		c, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		b.WriteString("\n")
		b.Write(c)
	}
	if err := os.WriteFile(sambaShares, []byte(b.String()), 0o644); err != nil {
		return err
	}
	exec.Command("smbcontrol", "smbd", "reload-config").Run()
	return nil
}

// sambaAddress is how a PC on the same network opens name's drive.
func sambaAddress(name string) string {
	host, _ := os.Hostname()
	return `\\` + host + `\` + strings.ToLower(name)
}
