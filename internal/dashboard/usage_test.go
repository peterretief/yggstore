package dashboard

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/ygg"
)

func TestStorageInTheLedger(t *testing.T) {
	dir := t.TempDir()
	list := []peers.Peer{
		{Name: "anna-box", Addr: "[201::1]:7400", Owner: "anna", Admin: true},
		{Name: "ben-pi", Addr: "[201::2]:7400", Owner: "ben"},
		{Name: "ben-usb1", Addr: "[201::2]:7401", Owner: "ben", Host: "ben-pi"},
		{Name: "gw", Addr: "[201::9]:7400", Owner: "anna", Gateway: true},
	}
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: dir, SelfID: "201::1", WalletDir: filepath.Join(dir, "ygg")})
	const tb = int64(1e12)
	states := []PeerState{
		// Ben's node holds 1 TB of Anna's, and some of Ben's own (free).
		{Name: "ben-pi", Addr: "[201::2]:7400", Up: true, Info: &server.Info{QuotaBytes: 4 * tb,
			ByWriter: map[string]int64{"201::1": tb, "201::2": 5e9}}},
		// Anna's holds some of Ben's, and of gateway customers (not counted).
		{Name: "anna-box", Addr: "[201::1]:7400", Up: true, Info: &server.Info{
			ByWriter: map[string]int64{"201::2": 2e9, "201::9": 7e9}}},
		{Name: "ben-usb1", Addr: "[201::2]:7401", Up: false, Info: &server.Info{ByWriter: map[string]int64{"201::1": tb}}}, // down: not counted
	}
	ctx := context.Background()
	day := time.Date(2026, 10, 16, 23, 30, 0, 0, time.UTC)
	d.countUsage(ctx, list, states, day)                     // starts counting
	d.countUsage(ctx, list, states, day.Add(10*time.Minute)) // +10 min
	d.countUsage(ctx, list, states, day.Add(20*time.Minute)) // +10 min
	d.countUsage(ctx, list, states, day.Add(50*time.Minute)) // a 30-minute gap: not counted (and a new day: closes the 16th)

	lg, err := d.ledger()
	if err != nil {
		t.Fatal(err)
	}
	// 1 TB for 20 minutes at 10 yggs per GB-month: 4.63 yggs, 4 now, the rest carried.
	b := lg.Balances()
	if b["anna"] != -4 || b["ben"] != 4 {
		t.Fatalf("balances after the 16th: %v", b)
	}
	h := lg.History("ben", 10)
	if len(h) != 1 || h[0].Kind != ygg.KindStorage || h[0].From != "anna" || h[0].Note != "storage on ben-pi, 2026-10-16: 13.89 GB-days" {
		t.Fatalf("history %+v", h)
	}
	// Ben's 2 GB on Anna's box for 20 minutes is far under a ygg: carried.
	if c := d.storage.Carry["ben\tanna"]; c <= 0 || c >= 1 {
		t.Fatalf("carry %v", d.storage.Carry)
	}
	if c := d.storage.Carry["anna\tben"]; math.Abs(c-0.6296) > 0.001 {
		t.Fatalf("anna's carry %v", c)
	}

	// The 17th counts afresh; closing the 16th again changes nothing.
	d.countUsage(ctx, list, states, day.Add(60*time.Minute))
	d.mu.Lock()
	d.state.Peers = states
	d.mu.Unlock()
	if got := d.today(list); len(got) != 2 || got[1].Node != "ben-pi" || got[1].Writer != "anna" || got[1].Holder != "ben" || got[1].Yggs <= 0 {
		t.Fatalf("today %+v", got)
	}
	nodes, people := d.usageNow(list)
	if len(nodes) != 3 || nodes[2].Holds != 0 || nodes[1].Node != "ben-pi" || nodes[1].ForOthers != tb || nodes[1].Earns != 10000 {
		t.Fatalf("nodes now %+v", nodes)
	}
	if len(people) != 2 || people[0].Person != "anna" || people[0].OnOthers != tb || math.Round(people[0].Net) != -9980 || math.Round(people[1].Net) != 9980 {
		t.Fatalf("people now %+v", people)
	}
}
