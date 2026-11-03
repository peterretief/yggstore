package dashboard

import (
	"math"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestPendingStorage(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Owner: "peter"}, {Name: "pi", Owner: "tim"}}
	d := New(Config{StorageRate: 10})
	d.storage = &usageFile{
		Held: map[string]float64{
			"pi\tpeter":      gbMonth / 10,  // 1 ygg's worth: peter pays tim
			"desktop\tpeter": gbMonth,       // peter's own node: free
			"desktop\ttim":   gbMonth / 100, // 0.1 ygg: tim pays peter
		},
		Carry: map[string]float64{"peter\ttim": 0.25},
	}
	got := d.pending(list)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !near(got["tim"], 1.25-0.1) || !near(got["peter"], -1.25+0.1) {
		t.Fatalf("pending: %v", got)
	}
	if !near(got["tim"]+got["peter"], 0) {
		t.Fatalf("doesn't add up to 0: %v", got)
	}
}
