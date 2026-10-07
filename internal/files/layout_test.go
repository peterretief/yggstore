package files

import (
	"fmt"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestLayoutFor(t *testing.T) {
	for _, tc := range []struct{ machines, data, parity, per int }{
		{1, 4, 2, 2}, {3, 4, 2, 2}, {4, 2, 2, 1}, {5, 3, 2, 1}, {6, 4, 2, 1}, {8, 4, 2, 1}, {9, 6, 3, 1}, {20, 6, 3, 1},
	} {
		var online []peers.Peer
		for i := range tc.machines {
			online = append(online, peers.Peer{Name: fmt.Sprint(i)})
			// More nodes on a machine don't make more machines.
			online = append(online, peers.Peer{Name: fmt.Sprint(i, "b"), Host: fmt.Sprint(i)})
		}
		l, per := LayoutFor(online)
		if l.DataShards != tc.data || l.ParityShards != tc.parity || per != tc.per {
			t.Errorf("%d machines: %d+%d, %d per machine; want %d+%d, %d", tc.machines, l.DataShards, l.ParityShards, per, tc.data, tc.parity, tc.per)
		}
	}
}

// With four machines (one of them holding several nodes), each gets one of
// a 2+2 chunk's shards, so any two machines can be lost.
func TestFourMachinesOneShardEach(t *testing.T) {
	online := []peers.Peer{{Name: "desktop"}, {Name: "pi", Slow: true}, {Name: "pi2", Slow: true}, {Name: "office"}}
	for _, n := range []string{"t1", "t2", "t3"} {
		online = append(online, peers.Peer{Name: n, Host: "desktop"})
	}
	l, per := LayoutFor(online)
	for start := range 14 {
		got := map[string]int{}
		for _, i := range targets(online, l.TotalShards(), start, per) {
			got[online[i].Machine()]++
		}
		if len(got) != 4 {
			t.Fatalf("start %d: %v, want one shard on each machine", start, got)
		}
	}
}
