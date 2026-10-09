package devices

import (
	"path/filepath"
	"testing"
)

func TestDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	w := NewWatch(path)
	if w.Allowed("200::9") {
		t.Fatal("allowed with no list")
	}
	if _, err := Add(path, "[200:0::9]", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(path, "300:1::5", "phone"); err != nil {
		t.Fatal(err)
	}
	if !w.Allowed("200::9") || !w.Allowed("300:1::5") || w.Allowed("200::8") {
		t.Fatal("list not followed")
	}
	if d, err := Add(path, "200::9", "work laptop"); err != nil || d.Name != "work laptop" {
		t.Fatalf("rename: %+v %v", d, err)
	}
	if list, _ := Load(path); len(list) != 2 {
		t.Fatalf("%d devices after a rename, want 2", len(list))
	}
	if _, err := Remove(path, "phone"); err != nil {
		t.Fatal(err)
	}
	if w.Allowed("300:1::5") || !w.Allowed("200::9") {
		t.Fatal("removal not followed")
	}
	if _, err := Remove(path, "nobody"); err == nil {
		t.Fatal("removed a device that isn't there")
	}
	for _, bad := range []string{"192.168.0.5", "127.0.0.1", "::1", "2001:db8::1", "fe80::1", "laptop"} {
		if _, err := Add(path, bad, "x"); err == nil {
			t.Errorf("%s accepted as a Yggdrasil address", bad)
		}
	}
}
