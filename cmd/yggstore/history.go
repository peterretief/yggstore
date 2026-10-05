package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/outbox"
)

const historyUsage = `yggstore history: older versions of what's in your outbox

  yggstore history FILE.ystub                    list every version
  yggstore history -restore ID FILE.ystub        restore one version (ID from the list)
  yggstore history -folder PATH -at "2026-10-01 14:00" [-outfiles DIR]
                                                 restore a folder as it was then
                                                 (PATH inside the outbox; "" = all of it)

Restores go to the restore folder chosen on the dashboard (default
OUTBOX/restored). A file stored again beside its stub becomes a new version;
the latest 20 older versions are kept.
`

// outboxOf finds the outbox a path is in: the nearest folder above it
// holding a .yggstore folder.
func outboxOf(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(filepath.Join(dir, ".yggstore")); err == nil && fi.IsDir() && filepath.Join(dir, ".yggstore") != yggstoreHome() {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

func printEvent(kind, msg string) { log.Printf("%s: %s", kind, msg) }

func cmdHistory(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, historyUsage) }
	restore := fs.String("restore", "", "restore the version with this ID")
	folder := fs.String("folder", "-", "restore this folder of the outbox as it was at -at")
	at := fs.String("at", "", `with -folder: when, as "2026-10-01 14:00" or "2026-10-01"`)
	outfiles := fs.String("outfiles", "", "the outbox (default: found from the stub, or the current folder)")
	fs.Parse(args)

	if *folder != "-" {
		dir := *outfiles
		if dir == "" {
			if dir = outboxOf(filepath.Join(".", "x")); dir == "" {
				return errors.New("say which outbox with -outfiles DIR")
			}
		}
		t, err := parseWhen(*at)
		if err != nil {
			return err
		}
		w := outbox.New(outbox.Config{Dir: dir, Client: client.New(), Event: printEvent})
		target, n, err := w.RestoreFolderAt(ctx, *folder, t)
		if err != nil && n == 0 {
			return err
		}
		log.Printf("restored %d items to %s", n, target)
		return err
	}

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	stub := fs.Arg(0)
	dir := *outfiles
	if dir == "" {
		dir = outboxOf(stub)
	}
	if dir == "" {
		return fmt.Errorf("%s isn't in an outbox, so it has no history", stub)
	}
	w := outbox.New(outbox.Config{Dir: dir, Client: client.New(), Event: printEvent})
	if *restore != "" {
		p, err := w.VersionStub(stub, *restore)
		if err != nil {
			return err
		}
		_, err = w.RestoreStub(ctx, p)
		return err
	}
	vs, err := w.Versions(stub)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Stored\tSize\tID")
	for _, v := range vs {
		when := "before history was kept"
		if v.StoredAt != 0 {
			when = time.Unix(v.StoredAt, 0).Format("2 Jan 2006 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", when, size(v.Size), v.ID)
	}
	tw.Flush()
	if len(vs) > 1 {
		fmt.Printf("\nRestore one with: yggstore history -restore ID %s\n", stub)
	}
	return nil
}

// parseWhen reads a local date, with or without a time.
func parseWhen(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if layout == "2006-01-02" {
				t = t.Add(24*time.Hour - time.Second) // the end of that day
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(`-at %q: give a date like "2026-10-01" or "2026-10-01 14:00"`, s)
}
