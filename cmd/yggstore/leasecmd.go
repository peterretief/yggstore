package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
)

// cmdLeases reports, on a node's machine, how long since the shards it
// holds were last wanted (see docs/leases.md). It only reports.
func cmdLeases(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leases", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "the node's shard folder")
	peersPath := fs.String("peers", defaultPeers(), "peer list, to name writers"+peersHelp)
	days := fs.Int("days", 90, "count shards unrenewed this many days")
	fs.Parse(args)

	held, err := localstore.New(*dataDir).Holdings()
	if err != nil {
		return err
	}
	if len(held) == 0 {
		fmt.Println("no shards held")
		return nil
	}
	names := map[string]string{}
	if list, err := peers.Load(*peersPath); err == nil {
		for _, p := range list {
			names[p.IP()] = p.Name
		}
	}
	type row struct {
		shards, stale, old int
		bytes, staleBytes  int64
		oldest             time.Time
	}
	rows := map[string]*row{}
	cutoff := time.Now().AddDate(0, 0, -*days)
	for _, h := range held {
		r := rows[h.Writer]
		if r == nil {
			r = &row{oldest: h.Renewed}
			rows[h.Writer] = r
		}
		r.shards++
		r.bytes += h.Size
		if h.Leases == 0 {
			r.old++
		}
		if h.Renewed.Before(cutoff) {
			r.stale++
			r.staleBytes += h.Size
		}
		if h.Renewed.Before(r.oldest) {
			r.oldest = h.Renewed
		}
	}
	writers := make([]string, 0, len(rows))
	for w := range rows {
		writers = append(writers, w)
	}
	sort.Slice(writers, func(i, j int) bool { return rows[writers[i]].oldest.Before(rows[writers[j]].oldest) })

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "WRITER\tSHARDS\tSIZE\tUNRENEWED %dd+\tLONGEST UNRENEWED\tBEFORE LEASES\n", *days)
	for _, w := range writers {
		r := rows[w]
		who := names[w]
		switch {
		case w == "":
			who = "(unknown)"
		case who == "":
			who = w + " (not in the peer list)"
		}
		stale := "-"
		if r.stale > 0 {
			stale = fmt.Sprintf("%d (%s)", r.stale, humanBytes(r.staleBytes))
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\tsince %s\t%d\n", who, r.shards, humanBytes(r.bytes), stale, r.oldest.Format("2006-01-02"), r.old)
	}
	return tw.Flush()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
