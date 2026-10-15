package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestUnitAsRootOrUser(t *testing.T) {
	saved := asRoot
	defer func() { asRoot = saved }()

	asRoot = func() bool { return true }
	u := unit("yggstore hub", []string{"/usr/local/bin/yggstore", "hub"}, "syncthing@root.service")
	for _, want := range []string{"WantedBy=multi-user.target", "Environment=HOME=", "Wants=syncthing@root.service", "After=syncthing@root.service"} {
		if !strings.Contains(u, want) {
			t.Errorf("root unit lacks %s:\n%s", want, u)
		}
	}
	asRoot = func() bool { return false }
	if u := unit("yggstore node", []string{"yggstore", "serve"}); !strings.Contains(u, "WantedBy=default.target") || strings.Contains(u, "syncthing") {
		t.Errorf("user unit:\n%s", u)
	}
}

func TestJoinRefusesASecondGroupInOneFolder(t *testing.T) {
	dir := t.TempDir()
	if err := alreadyJoined(dir); err != nil {
		t.Fatalf("an empty folder was refused: %v", err)
	}
	list := []peers.Peer{{Name: "desktop", Addr: "[200::1]:7400", Admin: true}, {Name: "laptop", Addr: "[200::2]:7400"}}
	if err := peers.Write(filepath.Join(dir, "peers.json"), list); err != nil {
		t.Fatal(err)
	}
	err := alreadyJoined(dir)
	if err == nil || !strings.Contains(err.Error(), "admin desktop") || !strings.Contains(err.Error(), "-port 7401") {
		t.Fatalf("joined over another group's list: %v", err)
	}
}

func TestSecondNodeGetsItsOwnServices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	saved := asRoot
	defer func() { asRoot = saved }()
	asRoot = func() bool { return false }

	n, d, l := nodeServices(filepath.Join(home, ".yggstore"), "laptop", 7400)
	if n != "yggstore-node.service" || d != "yggstore-dashboard.service" || l != defaultDashListen {
		t.Fatalf("first node: %s %s %s", n, d, l)
	}
	n2, d2, l2 := nodeServices(filepath.Join(home, ".yggstore-2"), "laptop b", 7401)
	if n2 != "yggstore-node-laptop-b.service" || d2 != "yggstore-dashboard-laptop-b.service" || l2 != "127.0.0.1:7481" {
		t.Fatalf("second node: %s %s %s", n2, d2, l2)
	}

	if err := unitsFree(n, d); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(unitDir(), 0o755)
	os.WriteFile(filepath.Join(unitDir(), n), []byte("[Unit]\n"), 0o644)
	if err := unitsFree(n, d); err == nil {
		t.Fatal("a unit already there was not noticed")
	}
	if err := unitsFree(n2, d2); err != nil {
		t.Fatalf("the second node's units clash with the first's: %v", err)
	}
}
