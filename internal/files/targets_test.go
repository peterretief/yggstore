package files

import (
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func perMachineOf(online []peers.Peer, to []int) map[string]int {
	got := map[string]int{}
	for _, i := range to {
		got[online[i].Name]++
	}
	return got
}

func TestTargetsGiveSlowPeersOneShard(t *testing.T) {
	online := []peers.Peer{{Name: "desktop"}, {Name: "pi", Slow: true}, {Name: "pi2", Slow: true}, {Name: "office"}}
	for start := range 8 {
		got := perMachineOf(online, targets(online, 6, start, 2))
		if got["pi"] != 1 || got["pi2"] != 1 || got["desktop"] != 2 || got["office"] != 2 {
			t.Fatalf("start %d: %v, want the Pis 1 each and the others 2", start, got)
		}
	}
}

func TestTargetsNeverExceedParityWhenAvoidable(t *testing.T) {
	cases := []struct {
		name   string
		online []peers.Peer
	}{
		{"one fast peer left", []peers.Peer{{Name: "a"}, {Name: "b", Slow: true}, {Name: "c", Slow: true}}},
		{"no slow peers", []peers.Peer{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}},
		{"many peers", []peers.Peer{{Name: "a"}, {Name: "b", Slow: true}, {Name: "c"}, {Name: "d"}, {Name: "e"}, {Name: "f"}, {Name: "g"}}},
	}
	for _, tc := range cases {
		for start := range 8 {
			to := targets(tc.online, 6, start, 2)
			if len(to) != 6 {
				t.Fatalf("%s: %d targets, want 6", tc.name, len(to))
			}
			for name, n := range perMachineOf(tc.online, to) {
				if n > 2 {
					t.Fatalf("%s start %d: %s got %d shards, more than the 2 parity covers", tc.name, start, name, n)
				}
			}
		}
	}
}

func TestTargetsWithTooFewPeersStillPlaceEverything(t *testing.T) {
	online := []peers.Peer{{Name: "a"}, {Name: "b", Slow: true}}
	if to := targets(online, 6, 0, 2); len(to) != 6 {
		t.Fatalf("%d targets, want 6", len(to))
	}
}

func TestTargetsCapShardsPerMachine(t *testing.T) {
	online := []peers.Peer{{Name: "desktop"}, {Name: "pi", Slow: true}, {Name: "pi2", Slow: true}, {Name: "office"}}
	for _, n := range []string{"t1", "t2", "t3", "t4", "t5"} {
		online = append(online, peers.Peer{Name: n, Host: "desktop"})
	}
	for _, n := range []string{"l1", "l2", "l3"} {
		online = append(online, peers.Peer{Name: n, Host: "office"})
	}
	for start := range 24 {
		got := map[string]int{}
		for _, i := range targets(online, 6, start, 2) {
			got[online[i].Machine()]++
		}
		if got["desktop"] != 2 || got["office"] != 2 || got["pi"] != 1 || got["pi2"] != 1 {
			t.Fatalf("start %d: per machine %v, want desktop 2, office 2, pi 1, pi2 1", start, got)
		}
	}
}
