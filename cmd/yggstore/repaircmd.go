package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/repair"
)

// newRepairer looks after the items under outfiles (and their older
// versions) and the mailbox's messages, and renews their leases and those
// of shares received.
func newRepairer(peersPath string, grace time.Duration, outfiles, history, mailbox string) *repair.Repairer {
	c := client.New()
	c.HTTP.Timeout = 2 * time.Minute
	return &repair.Repairer{Client: c, Grace: grace,
		State:    filepath.Join(yggstoreHome(), "repair.json"),
		Peers:    func() ([]peers.Peer, error) { return peers.Load(peersPath) },
		Stubs:    func() []string { return append(repair.Find(outfiles, history), repair.Find(mailbox)...) },
		Received: func() []string { return repair.Find(filepath.Join(outfiles, outbox.ReceivedDir)) }}
}

// cmdRepair makes one repair pass now, e.g. after retiring a node for good
// (-after 0 takes every node that doesn't answer as gone).
func cmdRepair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repair", flag.ExitOnError)
	peersPath := fs.String("peers", defaultPeers(), "peer list"+peersHelp)
	outfiles := fs.String("outfiles", "outfiles", "your outbox folder")
	mailbox := fs.String("mailbox", filepath.Join(yggstoreHome(), "mail"), "your mailbox")
	after := fs.Duration("after", repair.DefaultGrace, "rebuild shards held by nodes down at least this long (0: every node that doesn't answer now)")
	fs.Parse(args)
	if *after <= 0 {
		*after = time.Nanosecond
	}
	history := filepath.Join(*outfiles, ".yggstore", "history")
	r := newRepairer(*peersPath, *after, *outfiles, history, *mailbox)
	r.Log = func(kind, msg string) { fmt.Fprintln(os.Stderr, msg) }
	rep, err := r.Pass(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d items, %d shards rebuilt, %d stubs updated, %d shards on nodes down for less than %s\n", rep.Items, rep.Rebuilt, rep.Rewrote, rep.Waiting, *after)
	for _, p := range rep.Problems {
		fmt.Println("  " + p)
	}
	return nil
}
