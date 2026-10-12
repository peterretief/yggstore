package main

import (
	"strings"
	"testing"
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
