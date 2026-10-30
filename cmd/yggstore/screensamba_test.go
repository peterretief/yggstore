package main

import (
	"os"
	"testing"
)

// TestSambaOnABox runs only on a box set up for network drives, as root,
// with YGGSTORE_SAMBA_TEST set: it makes a drive for "drivetest", leaves
// it for the length of YGGSTORE_SAMBA_TEST's check, and takes it away.
func TestSambaOnABox(t *testing.T) {
	step := os.Getenv("YGGSTORE_SAMBA_TEST")
	if step == "" || !sambaOn() {
		t.Skip("not on a box set up for network drives")
	}
	switch step {
	case "add":
		os.MkdirAll("/tmp/drivetest", 0o755)
		os.WriteFile("/tmp/drivetest/hello.txt", []byte("hello\n"), 0o644)
		if err := sambaShare("drivetest", "/tmp/drivetest", "tea at four on fridays"); err != nil {
			t.Fatal(err)
		}
	case "remove":
		sambaRemove("drivetest")
		os.RemoveAll("/tmp/drivetest")
	}
}
