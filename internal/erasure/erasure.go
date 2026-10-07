package erasure

import (
	"fmt"

	"github.com/klauspost/reedsolomon"
)

const (
	DataShards           = 2
	ParityShards         = 1
	TotalShards          = DataShards + ParityShards
	MaxShards            = 64
	BalancedDataShards   = 5
	BalancedParityShards = 2
)

// Layout describes the Reed-Solomon data and parity geometry for one
// manifest. Older manifests use the package default (2+1); newer manifests
// may record a different validated layout.
type Layout struct {
	DataShards   int
	ParityShards int
}

func NewLayout(dataShards, parityShards int) (Layout, error) {
	if dataShards == 0 && parityShards == 0 {
		dataShards, parityShards = DataShards, ParityShards
	}
	l := Layout{DataShards: dataShards, ParityShards: parityShards}
	if err := l.Validate(); err != nil {
		return Layout{}, err
	}
	return l, nil
}

func DefaultLayout() Layout {
	return Layout{DataShards: DataShards, ParityShards: ParityShards}
}

func (l Layout) Validate() error {
	if l.DataShards < 1 || l.ParityShards < 1 {
		return fmt.Errorf("data and parity shards must be positive")
	}
	if l.DataShards > MaxShards || l.ParityShards > MaxShards || l.DataShards+l.ParityShards > MaxShards {
		return fmt.Errorf("total shards must not exceed %d", MaxShards)
	}
	return nil
}

func (l Layout) TotalShards() int { return l.DataShards + l.ParityShards }

func Encode(data []byte) ([][]byte, error) {
	return DefaultLayout().Encode(data)
}

func (l Layout) Encode(data []byte) ([][]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	enc, err := reedsolomon.New(l.DataShards, l.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("new encoder: %w", err)
	}
	shards, err := enc.Split(data)
	if err != nil {
		return nil, fmt.Errorf("split: %w", err)
	}
	if err := enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return shards, nil
}

func Decode(shards [][]byte, outputSize int) ([]byte, error) {
	return DefaultLayout().Decode(shards, outputSize)
}

func (l Layout) Decode(shards [][]byte, outputSize int) ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	if len(shards) != l.TotalShards() {
		return nil, fmt.Errorf("invalid shard count: got %d want %d", len(shards), l.TotalShards())
	}

	enc, err := reedsolomon.New(l.DataShards, l.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("new encoder: %w", err)
	}
	if err := enc.Reconstruct(shards); err != nil {
		return nil, fmt.Errorf("reconstruct: %w", err)
	}

	out := make([]byte, 0, outputSize)
	writer := appendWriter{buf: &out}
	if err := enc.Join(writer, shards, outputSize); err != nil {
		return nil, fmt.Errorf("join: %w", err)
	}
	return out, nil
}

type appendWriter struct {
	buf *[]byte
}

func (w appendWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

func BalancedLayout() Layout {
	return Layout{DataShards: BalancedDataShards, ParityShards: BalancedParityShards}
}

// Reconstruct fills in the missing (nil) shards, data and parity, from the
// ones present. It needs at least DataShards of them.
func (l Layout) Reconstruct(shards [][]byte) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if len(shards) != l.TotalShards() {
		return fmt.Errorf("invalid shard count: got %d want %d", len(shards), l.TotalShards())
	}
	enc, err := reedsolomon.New(l.DataShards, l.ParityShards)
	if err != nil {
		return fmt.Errorf("new encoder: %w", err)
	}
	return enc.Reconstruct(shards)
}
