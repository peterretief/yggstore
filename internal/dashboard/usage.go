package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/ygg"
)

// Storage in the ledger (see docs/ygg.md): the keeper counts, from what the
// nodes report holding for each writer, how long each node held how much
// for each person. Once a day the day's storage goes into the ledger: the
// person whose files were held pays the holder, at the storage rate.
// Storage on your own nodes is free. For now this trusts what the nodes
// report, as Give and use does; writers checking it comes later.

// DefaultStorageRate is what a holder earns: yggs per GB-month.
const DefaultStorageRate = 10

const (
	gbMonth  = 1e9 * 30 * 86400 // byte-seconds in a GB-month
	gbDay    = 1e9 * 86400
	maxGap   = 15 * time.Minute // a longer gap between samples isn't counted
	saveEach = time.Minute
)

// usageFile is the keeper's count for the day.
type usageFile struct {
	Day string `json:"day"` // the UTC day being counted
	At  int64  `json:"at"`  // the last sample, unix seconds
	// Held is byte-seconds today, by holding node and the person whose
	// files they are: "node\tperson".
	Held map[string]float64 `json:"held"`
	// Carry is yggs owed but not yet a whole ygg, by "writer\tholder".
	Carry map[string]float64 `json:"carry"`
}

func (d *Dashboard) usagePath() string { return filepath.Join(d.cfg.WalletDir, "usage.json") }

func (d *Dashboard) storageRate() int64 {
	if d.cfg.StorageRate > 0 {
		return d.cfg.StorageRate
	}
	return DefaultStorageRate
}

// ownerByIP is each machine's owner: the first node at its address.
func ownerByIP(list []peers.Peer) map[string]string {
	out := map[string]string{}
	for _, p := range list {
		if _, ok := out[p.IP()]; !ok {
			out[p.IP()] = p.Operator()
			if p.Gateway {
				out[p.IP()] = CustomersOwner
			}
		}
	}
	return out
}

// countUsage adds what the nodes hold now to today's count, and closes the
// day before into the ledger once a new one starts. poll calls it on the
// keeper.
func (d *Dashboard) countUsage(ctx context.Context, list []peers.Peer, states []PeerState, now time.Time) {
	if d.cfg.WalletDir == "" {
		return
	}
	d.walletMu.Lock()
	defer d.walletMu.Unlock()
	if d.storage == nil {
		u := usageFile{}
		if b, err := os.ReadFile(d.usagePath()); err == nil {
			json.Unmarshal(b, &u)
		}
		d.storage = &u
	}
	u := d.storage
	if u.Held == nil {
		u.Held = map[string]float64{}
	}
	if u.Carry == nil {
		u.Carry = map[string]float64{}
	}
	today := now.UTC().Format("2006-01-02")
	if u.Day != "" && u.Day != today {
		d.closeDay(ctx, list, u)
		u.Held, u.Day = map[string]float64{}, today
		d.saveUsage()
	}
	u.Day = today
	gap := now.Sub(time.Unix(u.At, 0))
	if u.At == 0 || gap <= 0 || gap > maxGap {
		u.At = now.Unix() // start counting from here
		return
	}
	owners := ownerByIP(list)
	byAddr := map[string]peers.Peer{}
	for _, p := range list {
		byAddr[p.Addr] = p
	}
	for _, ps := range states {
		p, ok := byAddr[ps.Addr]
		if !ok || ps.Test || !ps.Up || ps.Info == nil || p.Gateway {
			continue
		}
		for writer, n := range ps.Info.ByWriter {
			who := owners[writer]
			if who == "" || who == CustomersOwner || who == p.Operator() {
				continue // unknown, paid for through the gateway, or your own
			}
			u.Held[p.Name+"\t"+who] += float64(n) * gap.Seconds()
		}
	}
	u.At = now.Unix()
	if now.Sub(d.storageSaved) >= saveEach {
		d.saveUsage()
	}
}

func (d *Dashboard) saveUsage() {
	d.storageSaved = time.Now()
	b, _ := json.MarshalIndent(d.storage, "", "  ")
	err := os.MkdirAll(d.cfg.WalletDir, 0o700)
	if err == nil {
		err = atomicfile.Replace(d.usagePath(), append(b, '\n'), 0o600)
	}
	if err != nil {
		d.event("warn", "could not save the storage count: "+err.Error())
	}
}

// closeDay puts the day's storage into the ledger: for each person whose
// files others held, a payment to each holder.
func (d *Dashboard) closeDay(ctx context.Context, list []peers.Peer, u *usageFile) {
	lg, err := d.ledger()
	if err != nil {
		d.event("warn", "storage: the ledger: "+err.Error())
		return
	}
	holderOf := map[string]string{}
	for _, p := range list {
		holderOf[p.Name] = p.Operator()
	}
	type pair struct{ writer, holder string }
	byteSec := map[pair]float64{}
	nodes := map[pair][]string{}
	for key, bs := range u.Held {
		node, writer, _ := strings.Cut(key, "\t")
		holder := holderOf[node]
		if holder == "" || holder == writer {
			continue
		}
		k := pair{writer, holder}
		byteSec[k] += bs
		nodes[k] = append(nodes[k], node)
	}
	l, lerr := d.msgClient()
	for k, bs := range byteSec {
		carryKey := k.writer + "\t" + k.holder
		yggs := bs/gbMonth*float64(d.storageRate()) + u.Carry[carryKey]
		whole := int64(math.Floor(yggs))
		u.Carry[carryKey] = yggs - float64(whole)
		if whole < 1 {
			continue
		}
		sort.Strings(nodes[k])
		sum := sha256.Sum256([]byte(k.writer + "\x00" + k.holder))
		p := ygg.Payment{ID: "storage-" + u.Day + "-" + hex.EncodeToString(sum[:8]), Kind: ygg.KindStorage,
			From: k.writer, To: k.holder, Amount: min(whole, ygg.MaxAmount),
			Note: fmt.Sprintf("storage on %s, %s: %.2f GB-days", strings.Join(nodes[k], ", "), u.Day, bs/gbDay)}
		if _, taken, err := lg.Pay(p); err != nil {
			d.event("warn", fmt.Sprintf("storage: %s → %s: %v", k.writer, k.holder, err))
			continue
		} else if taken {
			d.event("info", fmt.Sprintf("storage: %s paid %s %d yggs for %s", k.writer, k.holder, p.Amount, u.Day))
		}
		if lerr == nil {
			d.tell(ctx, l, lg, list, k.writer)
			d.tell(ctx, l, lg, list, k.holder)
		}
	}
}

// storageToday is the day so far, for the view: by holding node and the
// person whose files they are.
type storageToday struct {
	Node   string  `json:"node"`
	Holder string  `json:"holder"`
	Writer string  `json:"writer"`
	GBDays float64 `json:"gb_days"`
	Yggs   float64 `json:"yggs"`
}

// today is the day's count so far; the caller holds walletMu.
func (d *Dashboard) today(list []peers.Peer) []storageToday {
	out := []storageToday{}
	if d.storage == nil {
		return out
	}
	holderOf := map[string]string{}
	for _, p := range list {
		holderOf[p.Name] = p.Operator()
	}
	for key, bs := range d.storage.Held {
		node, writer, _ := strings.Cut(key, "\t")
		out = append(out, storageToday{Node: node, Holder: holderOf[node], Writer: writer, GBDays: bs / gbDay,
			Yggs: bs / gbMonth * float64(d.storageRate())})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Writer < out[j].Writer
	})
	return out
}

// usageNode and usagePerson are what is stored now, worked out by each
// dashboard from the nodes it polls.
type usageNode struct {
	Node      string  `json:"node"`
	Owner     string  `json:"owner"`
	Up        bool    `json:"up"`
	Holds     int64   `json:"holds"`      // bytes, for everyone
	ForOthers int64   `json:"for_others"` // bytes, for other people
	Quota     int64   `json:"quota"`
	Earns     float64 `json:"earns"` // yggs a month at what it holds for others now
}

type usagePerson struct {
	Person    string  `json:"person"`
	Stores    int64   `json:"stores"`     // bytes, on every node
	OnOthers  int64   `json:"on_others"`  // bytes, on other people's nodes
	ForOthers int64   `json:"for_others"` // bytes their nodes hold for others
	Net       float64 `json:"net"`        // yggs a month, earned less paid, at today's holdings
	Nodes     int     `json:"nodes"`
}

func (d *Dashboard) usageNow(list []peers.Peer) ([]usageNode, []usagePerson) {
	d.mu.Lock()
	states := append([]PeerState{}, d.state.Peers...)
	d.mu.Unlock()
	owners := ownerByIP(list)
	byAddr := map[string]peers.Peer{}
	for _, p := range list {
		byAddr[p.Addr] = p
	}
	rate := float64(d.storageRate())
	nodes := []usageNode{}
	people := map[string]*usagePerson{}
	person := func(name string) *usagePerson {
		if people[name] == nil {
			people[name] = &usagePerson{Person: name}
		}
		return people[name]
	}
	for _, ps := range states {
		p, ok := byAddr[ps.Addr]
		if !ok || ps.Test || p.Gateway {
			continue
		}
		n := usageNode{Node: p.Name, Owner: p.Operator(), Up: ps.Up}
		person(n.Owner).Nodes++
		if ps.Info != nil && ps.Up { // a node that's down holds nothing for now
			n.Quota = ps.Info.QuotaBytes
			for writer, b := range ps.Info.ByWriter {
				who := owners[writer]
				n.Holds += b
				if who == "" || who == CustomersOwner {
					continue
				}
				person(who).Stores += b
				if who != n.Owner {
					n.ForOthers += b
					person(who).OnOthers += b
					person(n.Owner).ForOthers += b
				}
			}
		}
		n.Earns = float64(n.ForOthers) / 1e9 * rate
		nodes = append(nodes, n)
	}
	out := []usagePerson{}
	for _, p := range people {
		p.Net = float64(p.ForOthers-p.OnOthers) / 1e9 * rate
		out = append(out, *p)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })
	sort.Slice(out, func(i, j int) bool { return out[i].Person < out[j].Person })
	return nodes, out
}
