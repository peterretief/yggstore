package site

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
)

var fetchMu sync.Mutex // one fetch at a time

// fetchWanted fetches the versions and previews announced but not fetched
// yet, and starts serving them.
func (w *Web) fetchWanted(ctx context.Context) {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	w.mu.Lock()
	todo := map[string]manifest.Manifest{}
	previews := map[string]manifest.Manifest{}
	for name, e := range w.st.Sites {
		if e.Want != nil {
			todo[name] = *e.Want
		}
		if e.PreviewWant != nil {
			previews[name] = *e.PreviewWant
		}
	}
	w.mu.Unlock()
	for name, m := range previews {
		w.fetchPreview(ctx, name, m)
	}
	for name, m := range todo {
		w.fetchVersion(ctx, name, m)
	}
}

func (w *Web) fetchPreview(ctx context.Context, name string, m manifest.Manifest) {
	err := w.fetch(ctx, name, m)
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.st.Sites[name]
	if e == nil || e.PreviewWant == nil || e.PreviewWant.FileID != m.FileID {
		return // changed meanwhile
	}
	if err != nil {
		w.logf("web: could not fetch the preview of %s (will retry): %v", name, err)
		return
	}
	e.Preview, e.PreviewWant = m.FileID, nil
	w.logf("web: preview of %s is version %s", name, m.FileID[:8])
	w.save()
}

func (w *Web) fetchVersion(ctx context.Context, name string, m manifest.Manifest) {
	err := w.fetch(ctx, name, m)
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.st.Sites[name]
	if e == nil || e.Want == nil || e.Want.FileID != m.FileID {
		return // changed meanwhile
	}
	defer w.save()
	if err != nil {
		e.Error = err.Error()
		w.logf("web: could not fetch %s (will retry): %v", name, err)
		return
	}
	old := e.Serving
	e.Serving, e.Want, e.Error = m.FileID, nil, ""
	w.roots[name] = w.versionDir(name, m.FileID)
	w.logf("web: now serving %s version %s (%d files)", name, m.FileID[:8], m.FileCount)
	if e.Code && !w.NoCode {
		go w.warm(name, w.roots[name]) // so the first request needn't wait
	}
	w.cleanup(name, old, m.FileID, e.Preview)
}

// fetch unpacks version m of a site beside the one being served.
func (w *Web) fetch(ctx context.Context, name string, m manifest.Manifest) error {
	if m.Kind != files.KindFolder {
		return errors.New("the announced item is not a folder")
	}
	dir := w.siteDir(name)
	final := filepath.Join(dir, m.FileID)
	if _, err := os.Stat(final); err == nil {
		return nil // fetched before (a rollback to it)
	}
	incoming := filepath.Join(dir, ".incoming")
	os.RemoveAll(incoming)
	fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	got, err := files.Restore(fctx, w.Client, m, incoming, nil)
	if err != nil {
		os.RemoveAll(incoming)
		return err
	}
	defer os.RemoveAll(incoming)
	return os.Rename(got, final)
}

// cleanup keeps the version being served, the one before it for a quick
// rollback, and the preview.
func (w *Web) cleanup(name, previous, current, preview string) {
	dir := w.siteDir(name)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := e.Name(); n != previous && n != current && (n != preview || preview == "") {
			os.RemoveAll(filepath.Join(dir, n))
		}
	}
}
