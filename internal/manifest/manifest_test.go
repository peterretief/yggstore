package manifest

import (
	"testing"

	"github.com/peterretief/yggstore/internal/erasure"
)

func TestFivePlusTwoManifestRoundTrip(t *testing.T) {
	layout, err := erasure.NewLayout(5, 2)
	if err != nil {
		t.Fatal(err)
	}
	shards, err := layout.Encode([]byte("manifest layout test"))
	if err != nil {
		t.Fatal(err)
	}
	peers := []string{"a", "b", "c", "d", "e", "f", "g"}
	m, err := NewWithLayout(layout, "id", "file.bin", 35, make([]byte, 32), make([]byte, 12), shards, peers)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DataShards != 5 || loaded.ParityShards != 2 || len(loaded.Shards) != 7 {
		t.Fatalf("layout: got %d+%d with %d shards", loaded.DataShards, loaded.ParityShards, len(loaded.Shards))
	}
}

func TestChunkedManifestRoundTrip(t *testing.T) {
	layout, err := erasure.NewLayout(5, 2)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	makeChunk := func(plain byte, size int) Chunk {
		ciphertext := make([]byte, size+16)
		shards, err := layout.Encode(ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		refs := make([]ShardRef, len(shards))
		for i, shard := range shards {
			refs[i] = ShardRef{Hash: Hash(shard), Peer: "peer", URL: "http://peer/shards/" + Hash(shard)}
		}
		return Chunk{PlaintextSize: size, CiphertextSize: len(ciphertext), Nonce: make([]byte, 12), Shards: refs}
	}
	chunks := []Chunk{makeChunk('a', 1024), makeChunk('b', 512)}
	m, err := NewChunked(layout, "id", "file.bin", 1536, 1024, key, chunks)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 2 || loaded.ChunkSize != 1024 || loaded.PlaintextSize != 1536 || len(loaded.Chunks) != 2 {
		t.Fatalf("chunked manifest mismatch: version=%d chunk_size=%d plaintext=%d chunks=%d", loaded.Version, loaded.ChunkSize, loaded.PlaintextSize, len(loaded.Chunks))
	}
}
