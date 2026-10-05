package dashboard

import (
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
)

func TestAccounts(t *testing.T) {
	list := []peers.Peer{
		{Name: "desktop", Addr: "[200::1]:7400", Owner: "peter"},
		{Name: "pi", Addr: "[200::2]:7400", Owner: "peter"},
		{Name: "anna-pc", Addr: "[200::3]:7400"}, // owner defaults to the machine
	}
	const g = 1 << 30
	states := []PeerState{
		{Addr: "[200::1]:7400", Info: &server.Info{QuotaBytes: 50 * g, ByWriter: map[string]int64{"200::1": 4 * g, "200::3": 1 * g}}},
		{Addr: "[200::2]:7400", Info: &server.Info{QuotaBytes: 40 * g, ByWriter: map[string]int64{"200::1": 2 * g}}},
		{Addr: "[200::3]:7400", Info: &server.Info{QuotaBytes: 10 * g, ByWriter: map[string]int64{"200::1": 3 * g, "200::9": 5}}},
	}
	got := map[string]Account{}
	for _, a := range accounts(list, states, "200::1") {
		got[a.Owner] = a
	}
	peter, anna := got["peter"], got["anna-pc"]
	if !peter.Me || peter.Uses != 9*g || peter.Holds != 7*g || peter.ForOthers != 1*g || peter.Offered != 90*g || peter.Nodes != 2 {
		t.Fatalf("peter %+v", peter)
	}
	if anna.Me || anna.Uses != 1*g || anna.Holds != 3*g+5 || anna.ForOthers != 3*g+5 || anna.Offered != 10*g {
		t.Fatalf("anna %+v", anna)
	}
	if got["unknown node 200::9"].Uses != 5 {
		t.Fatalf("uploads from unlisted nodes not shown: %+v", got)
	}
}
