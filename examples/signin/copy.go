package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// A local app's data is on one machine, so each day a clean copy of it goes
// into a folder in the yggstore outbox (-copy-to outfiles/files/local-apps).
// The outbox stores it encrypted across the group and keeps earlier days as
// versions, and the recovery copy carries what's needed to get it back:
// drop its .ystub in restore/.

const copyEvery = 23 * time.Hour // a little under a day, so it doesn't drift

// source is a database and the name its copies go by.
type source struct {
	db   *sql.DB
	name string
}

// copyDB writes a clean copy. It takes shape under a hidden name, which the
// outbox leaves alone, so it never stores half of one.
func copyDB(s source, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+s.name+".tmp")
	os.Remove(tmp)
	defer os.Remove(tmp)
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("%s: %w", s.name, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, s.name))
}

func copyAll(sources []source, dir string) error {
	var errs []error
	for _, s := range sources {
		errs = append(errs, copyDB(s, dir))
	}
	return errors.Join(errs...)
}

// lastCopy is when the newest copy was stored, or made if it's still waiting.
func lastCopy(sources []source, dir string) time.Time {
	var last time.Time
	for _, s := range sources {
		for _, n := range []string{s.name, s.name + ".ystub"} {
			if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && fi.ModTime().After(last) {
				last = fi.ModTime()
			}
		}
	}
	return last
}

// unstored is a copy the outbox hasn't taken: it replaces each with a stub.
func unstored(sources []source, dir string) string {
	for _, s := range sources {
		if _, err := os.Stat(filepath.Join(dir, s.name)); err == nil {
			return s.name
		}
	}
	return ""
}

// keepCopies makes a copy whenever the last is a day old, checking hourly,
// so a machine that was off catches up when it's back.
func keepCopies(sources []source, dir string) {
	last := lastCopy(sources, dir)
	for {
		if time.Since(last) >= copyEvery {
			if n := unstored(sources, dir); n != "" {
				log.Printf("the last copy of %s is still in %s: is the dashboard running?", n, dir)
			}
			if err := copyAll(sources, dir); err != nil {
				log.Printf("copying the data: %v", err)
			} else {
				last = time.Now()
				log.Printf("copied the data to %s", dir)
			}
		}
		time.Sleep(time.Hour)
	}
}
