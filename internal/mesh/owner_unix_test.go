//go:build !windows

package mesh

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The settings are rewritten by root but read by Yggdrasil through their
// group, so a rewrite must keep the group.
func TestSetKeysKeepsGroup(t *testing.T) {
	groups, _ := os.Getgroups()
	gid := -1
	for _, g := range groups {
		if g != os.Getgid() {
			gid = g
		}
	}
	if gid < 0 {
		t.Skip("needs a second group to test with")
	}
	path := filepath.Join(t.TempDir(), "yggdrasil.conf")
	os.WriteFile(path, []byte(`{"AllowedPublicKeys": ["aa"]}`), 0o640)
	if err := os.Chown(path, -1, gid); err != nil {
		t.Skip(err)
	}
	c := &AllowConf{Path: path}
	if _, err := c.Keys(); err != nil {
		t.Fatal(err)
	}
	if err := c.SetKeys([]string{"aa", "bb"}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if got := int(fi.Sys().(*syscall.Stat_t).Gid); got != gid {
		t.Fatalf("group %d, want %d", got, gid)
	}
}
