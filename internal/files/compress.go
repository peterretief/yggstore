package files

import (
	"fmt"

	"github.com/klauspost/compress/zstd"

	"github.com/peterretief/yggstore/internal/manifest"
)

// Chunks are compressed before they're encrypted (encrypted bytes don't
// compress), when that saves enough to be worth it: text, JSON, logs and
// web pages shrink several times; photos, video and archives don't, and
// stay as they are.
var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithLowerEncoderMem(true))
	// A chunk is at most 1 GB (manifest.Unmarshal).
	decoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(1<<30))
)

const (
	// probeSize is how much of a big chunk is tried first: if it doesn't
	// compress, neither will the rest, and a Pi needn't try.
	probeSize = 64 << 10
)

// worth says whether compressed (c bytes of n) saves at least 1/16.
func worth(c, n int) bool { return c <= n-n/16 }

// compress is what to encrypt for the chunk plain, and its
// manifest.Chunk.Compression.
func compress(plain []byte) ([]byte, string) {
	if len(plain) < 64 {
		return plain, ""
	}
	if len(plain) > 4*probeSize {
		probe := encoder.EncodeAll(plain[:probeSize], nil)
		if !worth(len(probe), probeSize) {
			return plain, ""
		}
	}
	c := encoder.EncodeAll(plain, make([]byte, 0, len(plain)/2))
	if !worth(len(c), len(plain)) {
		return plain, ""
	}
	return c, manifest.Zstd
}

// decompress undoes compress, for a chunk of size bytes.
func decompress(body []byte, how string, size int) ([]byte, error) {
	switch how {
	case "":
		return body, nil
	case manifest.Zstd:
	default:
		return nil, fmt.Errorf("compressed with %q, which this yggstore doesn't know", how)
	}
	// Its header says how big it is: no more than the manifest says is
	// decompressed.
	var h zstd.Header
	if err := h.Decode(body); err != nil || !h.HasFCS || h.FrameContentSize != uint64(size) {
		return nil, fmt.Errorf("compressed chunk isn't the size its manifest says")
	}
	plain, err := decoder.DecodeAll(body, make([]byte, 0, size))
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}
	if len(plain) != size {
		return nil, fmt.Errorf("decompressed to %d bytes, not %d", len(plain), size)
	}
	return plain, nil
}
