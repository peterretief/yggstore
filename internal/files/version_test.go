package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
)

func TestNewVersionReusesUnchangedChunks(t *testing.T) {
	nodes := startNodes(t, 6)
	list := peerList(nodes)
	c := client.New()
	v1 := make([]byte, 4<<20+100) // 5 chunks of 1 MiB
	rand.Read(v1)
	m1, _, _, err := PutReader(context.Background(), c, bytes.NewReader(v1), "f", list, PutOptions{ChunkSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// Change a few bytes in chunk 2 and append to the end.
	v2 := append(append([]byte(nil), v1...), []byte("more at the end")...)
	copy(v2[2<<20+10:], "changed")
	m2, _, sum, err := PutReader(context.Background(), c, bytes.NewReader(v2), "f", list, PutOptions{ChunkSize: 1 << 20, Previous: &m1})
	if err != nil {
		t.Fatal(err)
	}
	if n := Reused(m2, m1); n != 3 { // chunks 0, 1 and 3; 2 changed, 4 grew
		t.Fatalf("reused %d chunks, want 3", n)
	}
	if err := ReadBack(context.Background(), c, m2, sum); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := Get(context.Background(), c, m1, &got, nil); err != nil || !bytes.Equal(got.Bytes(), v1) {
		t.Fatalf("first version damaged: %v", err)
	}
	// Without Previous nothing is reused (fresh key).
	m3, _, _, _ := PutReader(context.Background(), c, bytes.NewReader(v2), "f", list, PutOptions{ChunkSize: 1 << 20})
	if Reused(m3, m2) != 0 {
		t.Fatal("unrelated upload reused chunks")
	}
}
