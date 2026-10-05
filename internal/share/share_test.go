package share

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ids(t *testing.T) (*Identity, *Identity, *Identity) {
	dir := t.TempDir()
	var out []*Identity
	for _, n := range []string{"alice", "bob", "eve"} {
		id, err := LoadOrCreate(filepath.Join(dir, n, "sharing.key"))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out[0], out[1], out[2]
}

func TestSealAndOpen(t *testing.T) {
	alice, bob, eve := ids(t)
	stub := []byte(`{"file_name":"holiday.mkv","key":"c2VjcmV0"}`)
	sealed, err := alice.Seal(bob.Code(), stub, "Alice", "the video")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"holiday", "c2VjcmV0", "Alice", "the video"} {
		if bytes.Contains(sealed, []byte(secret)) {
			t.Fatalf("%q readable in the sealed file", secret)
		}
	}

	got, err := bob.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got.From != alice.Code() || got.FromName != "Alice" || got.Note != "the video" || !json.Valid(got.Stub) || !bytes.Equal(compact(t, got.Stub), compact(t, stub)) {
		t.Fatalf("opened %+v", got)
	}
	if _, err := eve.Open(sealed); err != ErrNotForYou {
		t.Fatalf("someone else opened it: %v", err)
	}

	// Changing any byte of the sealed data is caught.
	var env envelope
	json.Unmarshal(sealed, &env)
	env.Data = strings.Replace(env.Data, env.Data[10:11], flip(env.Data[10:11]), 1)
	tampered, _ := json.Marshal(env)
	if _, err := bob.Open(tampered); err == nil {
		t.Fatal("tampered file opened")
	}
	// Claiming another sender breaks the decryption, so From is proven.
	json.Unmarshal(sealed, &env)
	env.From = eve.Code()
	forged, _ := json.Marshal(env)
	if _, err := bob.Open(forged); err == nil {
		t.Fatal("forged sender accepted")
	}
}

func TestIdentityPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharing.key")
	a, _ := LoadOrCreate(path)
	b, err := LoadOrCreate(path)
	if err != nil || a.Code() != b.Code() {
		t.Fatalf("identity changed on reload: %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, want 0600", info.Mode().Perm())
	}
	if _, err := ParseCode("ys1short"); err == nil {
		t.Fatal("bad code accepted")
	}
	if _, err := ParseCode(a.Code()); err != nil {
		t.Fatal(err)
	}
}

func compact(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func flip(s string) string {
	if s == "A" {
		return "B"
	}
	return "A"
}
