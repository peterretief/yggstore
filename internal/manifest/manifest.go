package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/peterretief/yggstore/internal/erasure"
)

type Manifest struct {
	Kind           string     `json:"kind,omitempty"`
	FileCount      int        `json:"file_count,omitempty"`
	ContentBytes   int64      `json:"content_bytes,omitempty"`
	Version        int        `json:"version,omitempty"`
	DataShards     int        `json:"data_shards,omitempty"`
	ParityShards   int        `json:"parity_shards,omitempty"`
	ShareID        string     `json:"share_id,omitempty"`
	FileID         string     `json:"file_id"`
	FileName       string     `json:"file_name"`
	CiphertextSize int        `json:"ciphertext_size"`
	PlaintextSize  int        `json:"plaintext_size,omitempty"`
	ChunkSize      int        `json:"chunk_size,omitempty"`
	Key            []byte     `json:"key"`
	Nonce          []byte     `json:"nonce,omitempty"`
	Shards         []ShardRef `json:"shards,omitempty"`
	Chunks         []Chunk    `json:"chunks,omitempty"`
	// SharedBy is set on a stub someone else sent you: its shards are theirs,
	// so removing it only forgets it here.
	SharedBy *Sender `json:"shared_by,omitempty"`
	// History: every version of an item shares a Lineage (the first
	// version's FileID). StoredAt is when this version was stored, and
	// SourceModTime the original's modification time (unix nanoseconds).
	Lineage       string `json:"lineage,omitempty"`
	StoredAt      int64  `json:"stored_at,omitempty"`
	SourceModTime int64  `json:"source_mtime,omitempty"`
}

// Sender says who shared an item, as opened from their .ysend file.
type Sender struct {
	Name       string `json:"name"` // as they gave it, not proven
	Code       string `json:"code"` // their sharing code, proven by the decryption
	Note       string `json:"note,omitempty"`
	ReceivedAt int64  `json:"received_at"` // unix seconds
}

type Chunk struct {
	// Tag identifies the chunk's plaintext under the item's key (a keyed
	// hash), so a later version can reuse a chunk that didn't change.
	Tag            string     `json:"tag,omitempty"`
	PlaintextSize  int        `json:"plaintext_size"`
	CiphertextSize int        `json:"ciphertext_size"`
	Nonce          []byte     `json:"nonce"`
	Shards         []ShardRef `json:"shards"`
}

type ShardRef struct {
	Hash string `json:"hash"`
	Peer string `json:"peer"`
	URL  string `json:"url,omitempty"`
}

func New(fileID, fileName string, ciphertextSize int, key, nonce []byte, shards [][]byte, peers []string) (Manifest, error) {
	return NewWithLayout(erasure.DefaultLayout(), fileID, fileName, ciphertextSize, key, nonce, shards, peers)
}

func NewWithLayout(layout erasure.Layout, fileID, fileName string, ciphertextSize int, key, nonce []byte, shards [][]byte, peers []string) (Manifest, error) {
	if err := layout.Validate(); err != nil {
		return Manifest{}, err
	}
	if len(shards) != layout.TotalShards() {
		return Manifest{}, fmt.Errorf("invalid shard count: got %d want %d", len(shards), layout.TotalShards())
	}
	if len(peers) != layout.TotalShards() {
		return Manifest{}, fmt.Errorf("invalid peer count: got %d want %d", len(peers), layout.TotalShards())
	}

	m := Manifest{
		Version: 1, DataShards: layout.DataShards, ParityShards: layout.ParityShards,
		Shards:         make([]ShardRef, layout.TotalShards()),
		FileID:         fileID,
		FileName:       fileName,
		CiphertextSize: ciphertextSize,
		Key:            append([]byte(nil), key...),
		Nonce:          append([]byte(nil), nonce...),
	}
	for i, shard := range shards {
		m.Shards[i] = ShardRef{
			Hash: Hash(shard),
			Peer: peers[i],
		}
	}
	return m, nil
}

// NewChunked creates a version-2 manifest whose encrypted data is split into
// independently authenticated chunks. Each chunk contains its own nonce and
// shard references so restore can stream to disk without buffering the file.
func NewChunked(layout erasure.Layout, fileID, fileName string, plaintextSize, chunkSize int, key []byte, chunks []Chunk) (Manifest, error) {
	if err := layout.Validate(); err != nil {
		return Manifest{}, err
	}
	if plaintextSize < 0 || chunkSize <= 0 {
		return Manifest{}, fmt.Errorf("invalid plaintext or chunk size")
	}
	if len(key) != 32 {
		return Manifest{}, fmt.Errorf("invalid key size: got %d want 32", len(key))
	}
	if len(chunks) == 0 {
		return Manifest{}, fmt.Errorf("chunked manifest requires at least one chunk")
	}
	ciphertextSize := int64(0)
	plainTotal := int64(0)
	for i, chunk := range chunks {
		if chunk.PlaintextSize > chunkSize || chunk.CiphertextSize > chunkSize+16 {
			return Manifest{}, fmt.Errorf("chunk %d exceeds configured chunk size", i)
		}
		if err := validateChunk(layout, chunk); err != nil {
			return Manifest{}, fmt.Errorf("chunk %d: %w", i, err)
		}
		plainTotal += int64(chunk.PlaintextSize)
		ciphertextSize += int64(chunk.CiphertextSize)
	}
	if ciphertextSize > int64(^uint(0)>>1) {
		return Manifest{}, fmt.Errorf("manifest size exceeds platform limit")
	}
	if plainTotal != int64(plaintextSize) {
		return Manifest{}, fmt.Errorf("chunk plaintext sizes do not match manifest total")
	}
	return Manifest{
		Version: 2, DataShards: layout.DataShards, ParityShards: layout.ParityShards,
		FileID: fileID, FileName: fileName, PlaintextSize: plaintextSize,
		ChunkSize: chunkSize, CiphertextSize: int(ciphertextSize),
		Key: append([]byte(nil), key...), Chunks: append([]Chunk(nil), chunks...),
	}, nil
}

// LineageOf is m's lineage: its own FileID for a first version.
func LineageOf(m Manifest) string {
	if m.Lineage != "" {
		return m.Lineage
	}
	return m.FileID
}

func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func Marshal(m Manifest) ([]byte, error) {
	stub, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	return append(stub, '\n'), nil
}

func Unmarshal(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("unmarshal manifest: %w", err)
	}
	if m.Version != 0 && m.Version != 1 && m.Version != 2 {
		return m, fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if m.Kind != "" && m.Kind != "folder" {
		return m, fmt.Errorf("unsupported manifest kind")
	}
	if m.Kind == "folder" && (m.Version != 2 || m.FileCount < 0 || m.FileCount > 100000 || m.ContentBytes < 0 || m.ContentBytes > 1<<40 || m.ContentBytes > int64(m.PlaintextSize)) {
		return m, fmt.Errorf("invalid folder metadata")
	}
	if m.Kind == "" && (m.FileCount != 0 || m.ContentBytes != 0) {
		return m, fmt.Errorf("folder metadata requires folder kind")
	}
	if m.Version == 0 {
		m.DataShards, m.ParityShards = erasure.DataShards, erasure.ParityShards
	}
	layout, err := erasure.NewLayout(m.DataShards, m.ParityShards)
	if err != nil {
		return m, fmt.Errorf("unsupported shard layout: %w", err)
	}
	if m.FileName == "" || m.FileName == "." || m.FileName == ".." || filepath.Base(m.FileName) != m.FileName || strings.ContainsAny(m.FileName, "/\\\x00") {
		return m, fmt.Errorf("invalid manifest filename")
	}
	if len(m.Key) != 32 {
		return m, fmt.Errorf("invalid manifest key")
	}
	if m.Version == 2 {
		if len(m.Shards) != 0 || len(m.Nonce) != 0 || m.ChunkSize <= 0 || m.ChunkSize > 1<<30 || len(m.Chunks) == 0 || m.PlaintextSize < 0 || int64(m.PlaintextSize) > 1<<40 || m.CiphertextSize < 16 || int64(m.CiphertextSize) > 1<<40 {
			return m, fmt.Errorf("invalid chunked manifest parameters")
		}
		plainTotal := int64(0)
		cipherTotal := int64(0)
		for i, chunk := range m.Chunks {
			if chunk.PlaintextSize > m.ChunkSize || chunk.CiphertextSize > m.ChunkSize+16 {
				return m, fmt.Errorf("chunk %d exceeds configured chunk size", i)
			}
			if err := validateChunk(layout, chunk); err != nil {
				return m, fmt.Errorf("chunk %d: %w", i, err)
			}
			plainTotal += int64(chunk.PlaintextSize)
			cipherTotal += int64(chunk.CiphertextSize)
		}
		if cipherTotal != int64(m.CiphertextSize) || plainTotal != int64(m.PlaintextSize) {
			return m, fmt.Errorf("chunk sizes do not match manifest totals")
		}
		return m, nil
	}
	if len(m.Shards) != layout.TotalShards() {
		return m, fmt.Errorf("unsupported shard layout; manifest has %d shards for %d+%d", len(m.Shards), layout.DataShards, layout.ParityShards)
	}
	if len(m.Nonce) != 12 || m.CiphertextSize < 16 || m.CiphertextSize > (1<<30)+16 {
		return m, fmt.Errorf("invalid manifest encryption parameters or size")
	}
	for _, ref := range m.Shards {
		if err := validateShardRef(ref); err != nil {
			return m, err
		}
	}
	return m, nil
}

func validateChunk(layout erasure.Layout, chunk Chunk) error {
	if chunk.PlaintextSize < 0 || chunk.CiphertextSize < 16 || len(chunk.Nonce) != 12 {
		return fmt.Errorf("invalid chunk encryption parameters")
	}
	if len(chunk.Shards) != layout.TotalShards() {
		return fmt.Errorf("invalid shard count: got %d want %d", len(chunk.Shards), layout.TotalShards())
	}
	for _, ref := range chunk.Shards {
		if err := validateShardRef(ref); err != nil {
			return err
		}
	}
	return nil
}

func validateShardRef(ref ShardRef) error {
	if len(ref.Hash) != 64 {
		return fmt.Errorf("invalid shard hash")
	}
	if _, err := hex.DecodeString(ref.Hash); err != nil {
		return fmt.Errorf("invalid shard hash: %w", err)
	}
	return nil
}
