package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

func peerList(nodes []*node) []peers.Peer {
	var list []peers.Peer
	for _, n := range nodes {
		list = append(list, n.peer)
	}
	return list
}

func TestGetRange(t *testing.T) {
	nodes := startNodes(t, 6)
	data := make([]byte, 3<<20+1234)
	rand.Read(data)
	_, _, m := putFile(t, nodes, data) // 1 MiB chunks
	c := client.New()
	for _, r := range [][2]int64{{0, 10}, {1<<20 - 5, 10}, {2 << 20, 1 << 20}, {int64(len(data)) - 7, 7}, {500, 0}, {0, int64(len(data))}} {
		var buf bytes.Buffer
		if err := GetRange(context.Background(), c, m, &buf, r[0], r[1], nil); err != nil {
			t.Fatalf("range %v: %v", r, err)
		}
		if !bytes.Equal(buf.Bytes(), data[r[0]:r[0]+r[1]]) {
			t.Fatalf("range %v: wrong bytes", r)
		}
	}
	if err := GetRange(context.Background(), c, m, io.Discard, int64(len(data)), 1, nil); err == nil {
		t.Fatal("range past the end accepted")
	}
}

func TestConcatJoinsParts(t *testing.T) {
	nodes := startNodes(t, 6)
	list := peerList(nodes)
	c := client.New()
	key := make([]byte, 32)
	rand.Read(key)
	var parts []manifest.Manifest
	var whole []byte
	for _, size := range []int{1<<20 + 77, 3, 2 << 20} {
		part := make([]byte, size)
		rand.Read(part)
		whole = append(whole, part...)
		m, _, _, err := PutReader(context.Background(), c, bytes.NewReader(part), "part", list, PutOptions{ChunkSize: 1 << 20, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, m)
	}
	m, err := Concat("joined.bin", parts)
	if err != nil {
		t.Fatal(err)
	}
	stub, _ := manifest.Marshal(m)
	if _, err := manifest.Unmarshal(stub); err != nil {
		t.Fatalf("joined stub doesn't load: %v", err)
	}
	var buf bytes.Buffer
	if err := Get(context.Background(), c, m, &buf, nil); err != nil || !bytes.Equal(buf.Bytes(), whole) {
		t.Fatalf("joined item reads back wrong: %v", err)
	}
	var off int64 = 1<<20 + 70
	buf.Reset()
	if err := GetRange(context.Background(), c, m, &buf, off, 20, nil); err != nil || !bytes.Equal(buf.Bytes(), whole[off:off+20]) {
		t.Fatalf("range across parts: %v", err)
	}

	other, _, _, _ := PutReader(context.Background(), c, bytes.NewReader([]byte("x")), "x", list, PutOptions{})
	if _, err := Concat("bad", []manifest.Manifest{parts[0], other}); err == nil {
		t.Fatal("parts with different keys joined")
	}
}

type failAfter struct {
	r io.Reader
	n int
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("client went away")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

func TestFailedUploadLeavesNothing(t *testing.T) {
	nodes := startNodes(t, 6)
	data := make([]byte, 5<<20)
	rand.Read(data)
	_, _, _, err := PutReader(context.Background(), client.New(), &failAfter{bytes.NewReader(data), 3<<20 + 5}, "f", peerList(nodes), PutOptions{ChunkSize: 1 << 20, Log: t.Logf})
	if err == nil {
		t.Fatal("upload of a broken stream succeeded")
	}
	for _, n := range nodes {
		filepath.WalkDir(n.dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && len(d.Name()) == 64 {
				t.Errorf("shard left behind: %s", path)
			}
			return nil
		})
	}
}

func TestShardBytes(t *testing.T) {
	nodes := startNodes(t, 6)
	_, _, m := putFile(t, nodes, make([]byte, 2<<20))
	var total int64
	for _, n := range ShardBytes(m) {
		total += n
	}
	if want := int64(m.CiphertextSize) * 6 / 4; total < want || total > want+64 {
		t.Fatalf("shard bytes %d, want about %d", total, want)
	}
}
