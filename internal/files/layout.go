package files

import (
	"github.com/peterretief/yggstore/internal/erasure"
	"github.com/peterretief/yggstore/internal/peers"
)

// LayoutFor picks how a new item is split, from how many separate machines
// (see peers.Peer.Machine) are online to hold it, and how many of a chunk's
// shards one machine may hold. With enough machines every shard gets a
// machine of its own and the item survives losing the parity's worth of
// machines:
//
//	machines  layout  survives    stored
//	1-3       4+2     1 machine   1.5x
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
	case n == 4:
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

func machineCount(online []peers.Peer) int {
	machines := map[string]bool{}
	for _, p := range online {
		machines[p.Machine()] = true
	}
	return len(machines)
}
