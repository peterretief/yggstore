package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/manifest"
)

// text is n bytes of a JSON-ish log, as compresses like the real thing.
func text(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, `{"at": %d, "sign": "platform-%d", "text": "TRAIN %d DELAYED %d MIN", "by": "thandi"}`+"\n", 1763540000+i*7, i%9, i%400, i%17)
	}
	return b.Bytes()[:n]
}

func noise(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// stored is the bytes the nodes hold.
func stored(t *testing.T, nodes []*node) int64 {
	var total int64
	for _, n := range nodes {
		filepath.Walk(n.dir, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				total += info.Size()
			}
			return nil
		})
	}
	return total
}

func TestTextIsStoredCompressed(t *testing.T) {
	nodes := startNodes(t, 6)
	data := text(3<<20 + 777) // four 1 MiB chunks
	_, get, m := putFile(t, nodes, data)
	if !bytes.Equal(get(), data) {
		t.Fatal("read back differs")
	}
	if m.Version != 3 {
		t.Fatalf("version %d, want 3", m.Version)
	}
	for i, ch := range m.Chunks {
		if ch.Compression != manifest.Zstd {
			t.Fatalf("chunk %d isn't compressed", i)
		}
	}
	if m.CiphertextSize*5 > m.PlaintextSize {
		t.Fatalf("%d bytes encrypted for %d of text: hardly compressed", m.CiphertextSize, m.PlaintextSize)
	}
	// The stub reads back, and a range across chunks comes out right.
	b, _ := manifest.Marshal(m)
	m2, err := manifest.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := GetRange(context.Background(), client.New(), m2, &buf, 1<<20-50, 2<<20, nil); err != nil || !bytes.Equal(buf.Bytes(), data[1<<20-50:3<<20-50]) {
		t.Fatalf("range: %v", err)
	}
	t.Logf("%d bytes of text: %d encrypted, %d on the nodes", len(data), m.CiphertextSize, stored(t, nodes))
}

func TestNoiseIsStoredAsItIs(t *testing.T) {
	nodes := startNodes(t, 6)
	data := noise(2<<20 + 5)
	_, get, m := putFile(t, nodes, data)
	if !bytes.Equal(get(), data) {
		t.Fatal("read back differs")
	}
	if m.Version != 2 {
		t.Fatalf("version %d, want 2: an item with nothing compressed stays readable by older yggstore", m.Version)
	}
	for i, ch := range m.Chunks {
		if ch.Compression != "" {
			t.Fatalf("chunk %d compressed", i)
		}
	}
}

func TestMixedAndNewVersions(t *testing.T) {
	nodes := startNodes(t, 6)
	list := peerList(nodes)
	c := client.New()
	v1 := append(append(text(1<<20), noise(1<<20)...), text(300)...) // text, noise, a bit of text
	m1, _, sum, err := PutReader(context.Background(), c, bytes.NewReader(v1), "f", list, PutOptions{ChunkSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ch := range m1.Chunks {
		got = append(got, ch.Compression)
	}
	if strings.Join(got, ",") != "zstd,,zstd" || m1.Version != 3 {
		t.Fatalf("compression %q, version %d", got, m1.Version)
	}
	if err := ReadBack(context.Background(), c, m1, sum); err != nil {
		t.Fatal(err)
	}
	// A new version with the first chunk changed reuses the others,
	// compressed or not.
	v2 := append([]byte(nil), v1...)
	copy(v2[100:], "CHANGED")
	m2, _, sum2, err := PutReader(context.Background(), c, bytes.NewReader(v2), "f", list, PutOptions{ChunkSize: 1 << 20, Previous: &m1})
	if err != nil {
		t.Fatal(err)
	}
	if n := Reused(m2, m1); n != 2 {
		t.Fatalf("reused %d chunks, want 2", n)
	}
	if err := ReadBack(context.Background(), c, m2, sum2); err != nil {
		t.Fatal(err)
	}
	// Joined with an uncompressed item, it's still version 3.
	m3, _, _, err := PutReader(context.Background(), c, bytes.NewReader(noise(1000)), "g", list, PutOptions{ChunkSize: 1 << 20, Key: m1.Key})
	if err != nil {
		t.Fatal(err)
	}
	mk, _, _, err := PutReader(context.Background(), c, bytes.NewReader(text(1000)), "h", list, PutOptions{ChunkSize: 1 << 20, Key: m1.Key})
	if err != nil {
		t.Fatal(err)
	}
	j, err := Concat("j", []manifest.Manifest{m3, mk})
	if err != nil || j.Version != 3 {
		t.Fatalf("joined: version %d, %v", j.Version, err)
	}
}

func TestCompressedStubsChecked(t *testing.T) {
	nodes := startNodes(t, 6)
	_, _, m := putFile(t, nodes, text(5000))
	m.Version = 2 // as if an old yggstore had made it
	b, _ := manifest.Marshal(m)
	if _, err := manifest.Unmarshal(b); err == nil {
		t.Fatal("a version 2 item with compressed chunks was accepted")
	}
	m.Version = 4
	b, _ = manifest.Marshal(m)
	if _, err := manifest.Unmarshal(b); err == nil {
		t.Fatal("version 4 was accepted")
	}

	body, how := compress(text(5000))
	if how != manifest.Zstd {
		t.Fatal("text not compressed")
	}
	if _, err := decompress(body, how, 4999); err == nil {
		t.Fatal("a chunk bigger than its manifest says was decompressed")
	}
	if _, err := decompress(body, "lzma", 5000); err == nil {
		t.Fatal("an unknown compression was taken")
	}
	if p, err := decompress(body, how, 5000); err != nil || !bytes.Equal(p, text(5000)) {
		t.Fatalf("round trip: %v", err)
	}
	if b, how := compress([]byte("short")); how != "" || string(b) != "short" {
		t.Fatal("a few bytes were compressed")
	}
}
