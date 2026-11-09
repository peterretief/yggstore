package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A local app's data is on one machine, so each day a clean copy of it goes
// into a folder in the yggstore outbox (-copy-to outfiles/files/local-apps).
// The outbox stores it encrypted across the group and keeps earlier days as
// versions, and the recovery copy carries what's needed to get it back:
// drop its .ystub in restore/. -copy-also adds the files that go with the
// data: its design, and the site's .yggstore.json.

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

// copyName is what a file's copy is called. The outbox leaves hidden files
// alone, so a site's .yggstore.json goes in as SITE.yggstore.json.
func copyName(path string) string {
	name := filepath.Base(path)
	if rest, hidden := strings.CutPrefix(name, "."); hidden {
		abs, err := filepath.Abs(path)
		if err != nil {
			return rest
		}
		return filepath.Base(filepath.Dir(abs)) + "." + rest
	}
	return name
}

// copyFile copies a file that goes with the data, such as its design. A
// folder, such as the documents, goes in file by file (copyFolder).
func copyFile(path, dir string) error {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return copyFolder(path, dir)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := copyName(path)
	tmp := filepath.Join(dir, "."+name+".tmp")
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func copyAll(sources []source, also []string, dir string) error {
	var errs []error
	for _, s := range sources {
		errs = append(errs, copyDB(s, dir))
	}
	for _, f := range also {
		errs = append(errs, copyFile(f, dir))
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
// After a nudge, it waits a minute for more changes, then copies everything.
func keepCopies(sources []source, also []string, dir string) {
	last := lastCopy(sources, dir)
	nudged := false
	for {
		if nudged || time.Since(last) >= copyEvery {
			nudged = false
			if n := unstored(sources, dir); n != "" {
				log.Printf("the last copy of %s is still in %s: is the dashboard running?", n, dir)
			}
			if err := copyAll(sources, also, dir); err != nil {
				log.Printf("copying the data: %v", err)
			} else {
				last = time.Now()
				log.Printf("copied the data to %s", dir)
			}
		}
		select {
		case <-time.After(time.Hour):
		case <-copySoon:
			time.Sleep(time.Minute)
			nudged = true
		}
	}
}

// copyFolder copies a folder, such as the documents, one file at a time into
// a folder of the same name, so the outbox stores each on its own. A file
// goes again only when it's newer than what was stored (the outbox replaces
// each with a .ystub, which keeps the time it was stored), so unchanged files
// cost nothing, and a replaced one becomes a new version.
func copyFolder(path, dir string) error {
	out := filepath.Join(dir, copyName(path))
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		stored := time.Time{}
		for _, n := range []string{e.Name(), e.Name() + ".ystub"} {
			if fi, err := os.Stat(filepath.Join(out, n)); err == nil && fi.ModTime().After(stored) {
				stored = fi.ModTime()
			}
		}
		if !info.ModTime().After(stored) {
			continue
		}
		if err := copyFile(filepath.Join(path, e.Name()), out); err != nil {
			return err
		}
	}
	return nil
}

// copySoon asks for a copy within a minute or so, after a change worth
// keeping straight away, such as an upload.
var copySoon = make(chan struct{}, 1)

func nudgeCopy() {
	select {
	case copySoon <- struct{}{}:
	default:
	}
}
