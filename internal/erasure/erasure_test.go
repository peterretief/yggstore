package erasure

import (
	"bytes"
	"testing"
)

func TestLayoutFivePlusTwoReconstructsWithTwoMissingShards(t *testing.T) {
	layout, err := NewLayout(5, 2)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := bytes.Repeat([]byte("five plus two layout\n"), 4096)
	shards, err := layout.Encode(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 7 {
		t.Fatalf("shard count: got %d want 7", len(shards))
	}
	shards[1] = nil
	shards[6] = nil
	got, err := layout.Decode(shards, len(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("reconstructed data mismatch")
	}
}
