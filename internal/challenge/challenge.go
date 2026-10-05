// Package challenge is proof-of-storage: the owner precomputes answers at
// upload time (while it still has the shard) and later asks the holder to
// hash a random byte range with a fresh nonce. Each challenge is used once.
package challenge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	mathrand "math/rand/v2"
)

type Request struct {
	Hash   string `json:"hash"`
	Nonce  string `json:"nonce"` // hex
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type Response struct {
	Proof string `json:"proof"`
}

// Prepared is a request together with its expected answer; kept by the owner only.
type Prepared struct {
	Request
	Expect string `json:"expect"`
	Used   bool   `json:"used,omitempty"`
}

const maxRange = 64 << 10

// Answer computes sha256(nonce || shard[offset:offset+length]).
func Answer(shard []byte, c Request) (string, error) {
	nonce, err := hex.DecodeString(c.Nonce)
	if err != nil || len(nonce) < 16 {
		return "", fmt.Errorf("nonce must be at least 16 hex-encoded bytes")
	}
	if c.Length <= 0 || c.Length > maxRange || c.Offset < 0 || c.Offset+c.Length > len(shard) {
		return "", fmt.Errorf("range out of bounds")
	}
	h := sha256.New()
	h.Write(nonce)
	h.Write(shard[c.Offset : c.Offset+c.Length])
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Prepare makes n single-use challenges for a shard the caller still holds.
func Prepare(shardHash string, shard []byte, n int) ([]Prepared, error) {
	if len(shard) == 0 {
		return nil, fmt.Errorf("empty shard")
	}
	out := make([]Prepared, 0, n)
	for range n {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		length := min(len(shard), 4096)
		offset := 0
		if len(shard) > length {
			offset = mathrand.IntN(len(shard) - length + 1)
		}
		req := Request{Hash: shardHash, Nonce: hex.EncodeToString(nonce), Offset: offset, Length: length}
		expect, err := Answer(shard, req)
		if err != nil {
			return nil, err
		}
		out = append(out, Prepared{Request: req, Expect: expect})
	}
	return out, nil
}
