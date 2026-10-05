package gateway

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
)

// Month is one month's metering, kept in usage/YYYY-MM.json. Storage is
// measured in byte-hours: bytes stored, times how long. Dividing by the
// hours in the month gives the average stored, which is what is billed.
type Month struct {
	Month      string                  `json:"month"`
	LastSample int64                   `json:"last_sample"` // unix seconds
	Customers  map[string]*CustomerUse `json:"customers"`
	Members    map[string]*MemberUse   `json:"members"`
}

// CustomerUse is what one customer used in a month.
type CustomerUse struct {
	ByteHours   float64 `json:"byte_hours"`
	StoredBytes int64   `json:"stored_bytes"` // at the last sample
	PeakBytes   int64   `json:"peak_bytes"`
	Uploaded    int64   `json:"uploaded_bytes"`
	Downloaded  int64   `json:"downloaded_bytes"`
	Requests    int64   `json:"requests"`
}

// MemberUse is what one member's nodes held for customers in a month,
// counting the shards (about 1.5 times the customers' bytes).
type MemberUse struct {
	ByteHours float64 `json:"byte_hours"`
	HeldBytes int64   `json:"held_bytes"` // at the last sample
}

type counters struct{ up, down, requests int64 }

type meter struct {
	dir string
	mu  sync.Mutex
	c   map[string]*counters
}

func newMeter(dir string) *meter { return &meter{dir: dir, c: map[string]*counters{}} }

func (m *meter) get(cust string) *counters {
	if m.c[cust] == nil {
		m.c[cust] = &counters{}
	}
	return m.c[cust]
}

func (m *meter) request(cust string) {
	m.mu.Lock()
	m.get(cust).requests++
	m.mu.Unlock()
}

func (m *meter) ingress(cust string, n int64) {
	m.mu.Lock()
	m.get(cust).up += n
	m.mu.Unlock()
}

func (m *meter) egress(cust string, n int64) {
	m.mu.Lock()
	m.get(cust).down += n
	m.mu.Unlock()
}

func monthPath(dir string, t time.Time) string {
	return filepath.Join(dir, t.UTC().Format("2006-01")+".json")
}

// LoadMonth reads a month's metering ("2026-10") from a gateway directory.
func LoadMonth(gatewayDir, month string) (Month, error) {
	var mo Month
	err := readJSON(filepath.Join(gatewayDir, "usage", month+".json"), &mo)
	return mo, err
}

// maxGap caps the time one sample accounts for, so a gateway that was off
// for days doesn't bill (or credit) a lump it never saw.
const maxGap = 2 * time.Hour

// sample adds the time since the last sample to the month's totals.
func (m *meter) sample(now time.Time, stored, held map[string]int64, list []peers.Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := monthPath(m.dir, now)
	var mo Month
	if err := readJSON(path, &mo); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mo = Month{Month: now.UTC().Format("2006-01")}
		// Carry on from last month's final sample.
		var prev Month
		if readJSON(monthPath(m.dir, now.AddDate(0, 0, -now.Day())), &prev) == nil {
			mo.LastSample = prev.LastSample
		}
	}
	if mo.Customers == nil {
		mo.Customers = map[string]*CustomerUse{}
	}
	if mo.Members == nil {
		mo.Members = map[string]*MemberUse{}
	}
	hours := 0.0
	if mo.LastSample > 0 {
		gap := now.Sub(time.Unix(mo.LastSample, 0))
		hours = min(max(gap, 0), maxGap).Hours()
	}
	cu := func(id string) *CustomerUse {
		if mo.Customers[id] == nil {
			mo.Customers[id] = &CustomerUse{}
		}
		return mo.Customers[id]
	}
	for id, n := range stored {
		u := cu(id)
		u.ByteHours += float64(n) * hours
		u.StoredBytes = n
		u.PeakBytes = max(u.PeakBytes, n)
	}
	for id, c := range m.c {
		u := cu(id)
		u.Uploaded += c.up
		u.Downloaded += c.down
		u.Requests += c.requests
	}
	operator := map[string]string{}
	for _, p := range list {
		operator[p.Addr] = p.Operator()
	}
	for _, mu := range mo.Members {
		mu.HeldBytes = 0
	}
	for addr, n := range held {
		who := operator[addr]
		if who == "" {
			who = "node no longer in the group (" + addr + ")"
		}
		if mo.Members[who] == nil {
			mo.Members[who] = &MemberUse{}
		}
		mo.Members[who].ByteHours += float64(n) * hours
		mo.Members[who].HeldBytes += n
	}
	mo.LastSample = now.Unix()
	if err := writeJSON(path, mo); err != nil {
		return err
	}
	m.c = map[string]*counters{}
	return nil
}

// hoursIn is the number of hours in the month of t.
func hoursIn(month string) float64 {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return 30 * 24
	}
	return t.AddDate(0, 1, 0).Sub(t).Hours()
}

// Report writes a month's bill for each customer and credit for each
// member. Storage is in GB-months: 1 GB stored for the whole month.
func Report(w io.Writer, mo Month, customers []Customer) {
	hours := hoursIn(mo.Month)
	gbMonths := func(byteHours float64) float64 { return byteHours / hours / 1e9 }
	byID := map[string]Customer{}
	for _, c := range customers {
		byID[c.ID] = c
	}
	ids := make([]string, 0, len(mo.Customers))
	for id := range mo.Customers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return byID[ids[i]].Name < byID[ids[j]].Name })

	fmt.Fprintf(w, "Usage for %s", mo.Month)
	if mo.LastSample > 0 {
		fmt.Fprintf(w, " (up to %s)", time.Unix(mo.LastSample, 0).UTC().Format("2 Jan 15:04 UTC"))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\nCustomers")
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "Customer\tPlan\tGB-months\tNow GB\tPeak GB\tUp GB\tDown GB\tRequests\t")
	for _, id := range ids {
		u, c := mo.Customers[id], byID[id]
		name := c.Name
		if name == "" {
			name = id + " (deleted)"
		} else if c.Suspended {
			name += " (suspended)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%.3f\t%.2f\t%.2f\t%.2f\t%.2f\t%d\t\n", name, c.Plan, gbMonths(u.ByteHours),
			float64(u.StoredBytes)/1e9, float64(u.PeakBytes)/1e9, float64(u.Uploaded)/1e9, float64(u.Downloaded)/1e9, u.Requests)
	}
	tw.Flush()

	names := make([]string, 0, len(mo.Members))
	var total float64
	for n, u := range mo.Members {
		names = append(names, n)
		total += u.ByteHours
	}
	sort.Strings(names)
	fmt.Fprintln(w, "\nMembers (space held for customers, shards included)")
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "Member\tGB-months\tShare\tNow GB\t")
	for _, n := range names {
		u := mo.Members[n]
		share := 0.0
		if total > 0 {
			share = 100 * u.ByteHours / total
		}
		fmt.Fprintf(tw, "%s\t%.3f\t%.1f%%\t%.2f\t\n", n, gbMonths(u.ByteHours), share, float64(u.HeldBytes)/1e9)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nShare is each member's part of what customers' data took up this month: split\ncredit or fees by it.")
}
