package files

import (
	"errors"

	"github.com/peterretief/yggstore/internal/erasure"
	"github.com/peterretief/yggstore/internal/peers"
)

// LayoutFor picks how a new item is split, from how many separate machines
// (see peers.Peer.Machine) are online to hold it, and how many of a chunk's
// shards one machine may hold. With enough machines every shard gets a
// machine of its own and the item survives losing the parity's worth of
// machines. One machine is never enough (see ErrTooFewMachines):
//
//	machines  layout  survives    stored
//	2         2+2     1 machine   2x
//	3         4+2     1 machine   1.5x
//	4         2+2     2 machines  2x
//	5         3+2     2 machines  1.67x
//	6-8       4+2     2 machines  1.5x
//	9+        6+3     3 machines  1.5x
func LayoutFor(online []peers.Peer) (erasure.Layout, int) {
	n := machineCount(online)
	var l erasure.Layout
	switch {
	case n >= 9:
		l = erasure.Layout{DataShards: 6, ParityShards: 3}
	case n >= 6:
		l = erasure.Layout{DataShards: 4, ParityShards: 2}
	case n == 5:
		l = erasure.Layout{DataShards: 3, ParityShards: 2}
	case n == 4 || n == 2:
		l = erasure.Layout{DataShards: 2, ParityShards: 2}
	default:
		l = Layout()
	}
	return l, PerMachine(l, n)
}

// PerMachine is how many of a chunk's shards one machine may hold: as few
// as the machines allow, and never more than the parity covers.
func PerMachine(l erasure.Layout, machines int) int {
	per := l.ParityShards
	if machines > 0 {
		per = min(per, (l.TotalShards()+machines-1)/machines)
	}
	return max(per, 1)
}

func machineCount(online []peers.Peer) int { return peers.Machines(online) }

// ErrTooFewMachines means the machines online can't hold an item so that
// losing any one of them leaves enough shards of every chunk. Like Tahoe-
// LAFS's "servers of happiness", such an upload is refused rather than
// stored where it isn't safe; try again when more machines are up.
var ErrTooFewMachines = errors.New("too few machines online")

// Happy is whether machines can hold an item split as l so that each chunk
// survives losing any one machine: no machine more than the parity's worth
// of its shards.
func Happy(l erasure.Layout, machines int) bool {
	return machines*l.ParityShards >= l.TotalShards()
}
