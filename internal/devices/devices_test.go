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

func TestPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	w := NewWatch(path)
	if _, err := Add(path, "200::9", "phone"); err != nil {
		t.Fatal(err)
	}
	for _, p := range Pages { // added before pages: every page
		if !w.Can("200::9", p) {
			t.Fatalf("a device without pages can't open %s", p)
		}
	}
	if d, err := SetPages(path, "phone", []string{"mail", "messages", "mail"}); err != nil || len(d.Pages) != 2 || d.Pages[0] != "messages" {
		t.Fatalf("set pages: %+v %v", d, err)
	}
	if !w.Can("200::9", "mail") || w.Can("200::9", "admin") || w.Can("200::9", "files") || !w.Allowed("200::9") {
		t.Fatal("pages not followed")
	}
	if _, err := Add(path, "200::9", "my phone"); err != nil {
		t.Fatal(err)
	}
	if w.Can("200::9", "admin") {
		t.Fatal("a rename gave the device every page")
	}
	if _, err := SetPages(path, "200::9", nil); err == nil {
		t.Fatal("left a device with no pages")
	}
	if _, err := SetPages(path, "200::9", []string{"mail", "bank"}); err == nil {
		t.Fatal("took a page that doesn't exist")
	}
	if w.Can("201::1", "mail") {
		t.Fatal("a device not on the list can open a page")
	}
}
