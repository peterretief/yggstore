package join

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testID = "AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"

func TestTrailer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Connect.exe")
	b := Box{Hub: "http://192.168.0.5:7490", Name: "localbox-004", ID: testID, Addrs: []string{"tcp://192.168.0.5:22000", "dynamic"}}
	prog := []byte("MZ...the program...#YGGSTORE-BOX# in the program's own text")
	os.WriteFile(path, append(prog, Trailer(b)...), 0o755)
	got, err := Read(path)
	if err != nil || got.Hub != b.Hub || got.Name != b.Name || got.ID != b.ID || len(got.Addrs) != 2 {
		t.Fatalf("%+v %v", got, err)
	}

	os.WriteFile(path, prog, 0o755)
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "download it again") {
		t.Fatalf("no details: %v", err)
	}
	os.WriteFile(path, append(prog, Trailer(Box{Hub: "http://x", ID: "not-an-id"})...), 0o755)
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("bad ID: %v", err)
	}
}

func TestChecksum(t *testing.T) {
	sums := []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nabc123  syncthing-windows-amd64-v2.0.0.zip\ndef456  syncthing-linux-amd64-v2.0.0.tar.gz\n-----BEGIN PGP SIGNATURE-----\n")
	if !checksumOK(sums, "syncthing-windows-amd64-v2.0.0.zip", "ABC123") {
		t.Fatal("listed file not matched")
	}
	if checksumOK(sums, "syncthing-windows-amd64-v2.0.0.zip", "def456") {
		t.Fatal("wrong hash matched")
	}
	if checksumOK(sums, "syncthing-windows-arm64-v2.0.0.zip", "abc123") {
		t.Fatal("unlisted file matched")
	}
}
