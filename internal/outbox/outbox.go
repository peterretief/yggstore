// Package outbox watches a folder: files dropped into it, or into folders
// inside it, are sharded into the network one by one and each replaced by a
// .ystub, so the folder tree stays as it was. Files or folders dropped into
// whole/ are stored as one item each. Stubs dropped into restore/ are rebuilt
// into restored/, and stubs dropped into delete/ are removed from every node.
// A .ysend someone sent you, dropped in anywhere, is opened into received/.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

const (
	RestoreDir  = "restore"
	RestoredDir = "restored"
	DeleteDir   = "delete"
	WholeDir    = "whole"
	ReceivedDir = "received"
	privateDir  = ".yggstore"
	// With 6 shards per chunk, 3 machines keep any single machine to at most
	// 2 shards, so losing one machine never loses a chunk.
	minPeers   = 3
	settleTime = 3 * time.Second
	retryAfter = 2 * time.Minute
)

type Config struct {
	Dir        string
	PeersPath  string
	Client     client.Client
	Interval   time.Duration
	Keep       bool // keep originals after a verified upload
	ChunkSize  int
	Challenges int
	Event      func(kind, msg string) // kind: info, ok, warn, bad
	Identity   *share.Identity        // opens .ysend files sent to you; nil = none
}

type Watcher struct {
	cfg   Config
	work  sync.Mutex // one upload or restore at a time
	sigs  map[string]signature
	retry map[string]time.Time
	shown map[string]bool // "already has a stub" warnings given, per path
	stubs map[string]cachedStub

	actMu    sync.Mutex
	act      *Activity
	restored []Restored // latest restores, oldest first
}

// Activity is what the watcher is doing right now, for a progress bar.
type Activity struct {
	Op      string `json:"op"` // storing, verifying or restoring
	Name    string `json:"name"`
	Done    int64  `json:"done"`  // plaintext bytes so far
	Total   int64  `json:"total"` // plaintext bytes in all (for a folder, an estimate)
	Started int64  `json:"started"`
}

// Activity returns a copy of the current activity, or nil when idle.
func (w *Watcher) Activity() *Activity {
	w.actMu.Lock()
	defer w.actMu.Unlock()
	if w.act == nil {
		return nil
	}
	a := *w.act
	return &a
}

// begin starts reporting op on name and returns a context that feeds it.
func (w *Watcher) begin(ctx context.Context, op, name string, total int64) context.Context {
	w.actMu.Lock()
	w.act = &Activity{Op: op, Name: name, Total: total, Started: time.Now().Unix()}
	w.actMu.Unlock()
	return files.WithProgress(ctx, func(n int) {
		w.actMu.Lock()
		if w.act != nil {
			w.act.Done += int64(n)
		}
		w.actMu.Unlock()
	})
}

func (w *Watcher) idle() {
	w.actMu.Lock()
	w.act = nil
	w.actMu.Unlock()
}

type signature struct {
	size, count int64
	newest      time.Time
}

func New(cfg Config) *Watcher {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Challenges <= 0 {
		cfg.Challenges = 20
	}
	if cfg.Event == nil {
		cfg.Event = func(string, string) {}
	}
	return &Watcher{cfg: cfg, sigs: map[string]signature{}, retry: map[string]time.Time{}, shown: map[string]bool{}, stubs: map[string]cachedStub{}}
}

func (w *Watcher) Dir() string { return w.cfg.Dir }

// SetEvent replaces the event sink; call before Run.
func (w *Watcher) SetEvent(f func(kind, msg string)) { w.cfg.Event = f }

func (w *Watcher) Run(ctx context.Context) error {
	for _, d := range []string{w.cfg.Dir, filepath.Join(w.cfg.Dir, RestoreDir), filepath.Join(w.cfg.Dir, RestoredDir), filepath.Join(w.cfg.Dir, DeleteDir),
		filepath.Join(w.cfg.Dir, WholeDir), filepath.Join(w.cfg.Dir, ReceivedDir), filepath.Join(w.cfg.Dir, privateDir, "restored-stubs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	os.Chmod(filepath.Join(w.cfg.Dir, privateDir), 0o700)
	w.cfg.Event("info", "watching "+w.cfg.Dir+" (drop files or folders in, or into whole/ to keep a folder as one item; stubs into restore/ or delete/); restores go to "+w.RestoreDir())
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		w.poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (w *Watcher) poll(ctx context.Context) {
	seen := map[string]bool{}
	for _, path := range w.items() {
		seen[path] = true
		if w.ready(path) {
			w.store(ctx, path)
		}
	}
	for _, path := range w.candidates(filepath.Join(w.cfg.Dir, RestoreDir)) {
		seen[path] = true
		if w.ready(path) {
			w.restoreDropped(ctx, path)
		}
	}
	for _, path := range w.sends() {
		seen[path] = true
		if w.ready(path) {
			w.receive(path)
		}
	}
	for _, path := range w.candidates(filepath.Join(w.cfg.Dir, DeleteDir)) {
		seen[path] = true
		if w.ready(path) {
			w.deleteDropped(ctx, path)
		}
	}
	for p := range w.sigs {
		if !seen[p] {
			delete(w.sigs, p)
			delete(w.retry, p)
		}
	}
	for p := range w.shown {
		if _, err := os.Lstat(p); err != nil {
			delete(w.shown, p)
		}
	}
}

// candidates lists the stubs dropped into dir (restore/ or delete/).
func (w *Watcher) candidates(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".") && !isTemp(name) && !e.IsDir() && strings.HasSuffix(name, files.StubExt) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// items lists what is waiting to be stored: every file anywhere in the
// outbox, each its own item with its stub beside it, and whatever sits at
// the top of whole/, files or folders stored as one item each. Hidden and
// half-downloaded files, and the restore/, restored/ and delete/ folders,
// are left alone.
func (w *Watcher) items() []string {
	whole := filepath.Join(w.cfg.Dir, WholeDir)
	var out []string
	filepath.WalkDir(w.cfg.Dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || path == w.cfg.Dir {
			return nil
		}
		name, parent := e.Name(), filepath.Dir(path)
		if strings.HasPrefix(name, ".") || isTemp(name) || strings.HasSuffix(name, share.Ext) ||
			(parent == w.cfg.Dir && e.IsDir() && (name == RestoreDir || name == RestoredDir || name == DeleteDir || name == ReceivedDir)) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			if parent == whole {
				if w.unstored(path) {
					out = append(out, path)
				}
				return filepath.SkipDir
			}
			return nil
		}
		if e.Type().IsRegular() && !strings.HasSuffix(name, files.StubExt) && w.unstored(path) {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// sends lists .ysend files waiting to be opened, at the top of the outbox or
// in received/.
func (w *Watcher) sends() []string {
	var out []string
	for _, dir := range []string{w.cfg.Dir, filepath.Join(w.cfg.Dir, ReceivedDir)} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), share.Ext) && !strings.HasPrefix(e.Name(), ".") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out
}

// receive opens a .ysend into received/NAME.ystub. A file that can't be
// opened is left where it is and reported once.
func (w *Watcher) receive(path string) {
	refuse := func(why string) {
		if !w.shown[path] {
			w.shown[path] = true
			w.cfg.Event("bad", fmt.Sprintf("cannot open %s: %s", filepath.Base(path), why))
		}
	}
	if w.cfg.Identity == nil {
		refuse("this dashboard has no sharing identity")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		refuse(err.Error())
		return
	}
	got, err := w.cfg.Identity.Open(data)
	if err != nil {
		refuse(err.Error())
		return
	}
	m, err := manifest.Unmarshal(got.Stub)
	if err != nil {
		refuse("the item inside is damaged: " + err.Error())
		return
	}
	// Restoring contacts every address in the stub, so from someone else
	// only overlay addresses and our own nodes are accepted.
	known := map[string]bool{}
	if list, err := peers.Load(w.cfg.PeersPath); err == nil {
		for _, p := range list {
			known[p.Addr] = true
		}
	}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if !peers.IsOverlay(ref.Peer) && !known[ref.Peer] {
				refuse("it points at a non-Yggdrasil address (" + ref.Peer + ")")
				return
			}
		}
	}
	m.SharedBy = &manifest.Sender{Name: got.FromName, Code: got.From, Note: got.Note, ReceivedAt: time.Now().Unix()}
	stub := freeStub(filepath.Join(w.cfg.Dir, ReceivedDir, m.FileName+files.StubExt))
	if err := files.WriteJSON(stub, m); err != nil {
		refuse(err.Error())
		return
	}
	os.Remove(path)
	from := got.FromName
	if from == "" {
		from = "someone"
	}
	w.cfg.Event("ok", fmt.Sprintf("received %s (%s) from %s; it is in %s/", m.FileName, human(int64(m.PlaintextSize)), from, ReceivedDir))
}

// freeStub returns path, or path with " (2)", " (3)", ... before the
// extension if that is taken.
func freeStub(path string) string {
	base := strings.TrimSuffix(path, files.StubExt)
	for i := 2; ; i++ {
		if _, err := os.Lstat(path); err != nil {
			return path
		}
		path = fmt.Sprintf("%s (%d)%s", base, i, files.StubExt)
	}
}

// unstored is true if path has no stub beside it, or if it differs from
// the version the stub holds: then it is stored as a new version. A file
// that is the same as its stub's version is left alone (and, unless
// originals are kept on purpose, the user is told once).
func (w *Watcher) unstored(path string) bool {
	stub := path + files.StubExt
	m, err := w.stubInfo(stub)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		if !w.shown[path] {
			w.shown[path] = true
			w.cfg.Event("bad", fmt.Sprintf("%s is not stored: its stub %s can't be read (%v)", w.rel(path), filepath.Base(stub), err))
		}
		return false
	}
	if m.SharedBy != nil || !w.same(path, m) {
		return m.SharedBy == nil
	}
	if !w.cfg.Keep && !w.shown[path] {
		w.shown[path] = true
		w.cfg.Event("warn", fmt.Sprintf("%s is the same as the version already stored, so it isn't stored again. You can delete it.", w.rel(path)))
	}
	return false
}

// stubInfo reads a stub, caching it while the stub file doesn't change, as
// every item is checked on every poll.
func (w *Watcher) stubInfo(stub string) (manifest.Manifest, error) {
	info, err := os.Stat(stub)
	if err != nil {
		return manifest.Manifest{}, err
	}
	if c, ok := w.stubs[stub]; ok && c.mtime.Equal(info.ModTime()) && c.size == info.Size() {
		return c.m, nil
	}
	m, err := files.ReadStub(stub)
	if err != nil {
		return m, err
	}
	m.Chunks, m.Shards, m.Key = nil, nil, nil // only the details are needed
	w.stubs[stub] = cachedStub{info.ModTime(), info.Size(), m}
	return m, nil
}

type cachedStub struct {
	mtime time.Time
	size  int64
	m     manifest.Manifest
}

// rel names path relative to the outbox, for messages.
func (w *Watcher) rel(path string) string {
	if r, err := filepath.Rel(w.cfg.Dir, path); err == nil {
		return r
	}
	return filepath.Base(path)
}

func isTemp(name string) bool {
	for _, suf := range []string{".part", ".partial", ".crdownload", ".download", ".tmp", ".swp", "~"} {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

// ready is true once an item has looked the same on two polls in a row and
// was last modified a few seconds ago, so half-copied items are left alone.
func (w *Watcher) ready(path string) bool {
	if until, ok := w.retry[path]; ok && time.Now().Before(until) {
		return false
	}
	sig, err := measure(path)
	if err != nil {
		return false
	}
	prev, ok := w.sigs[path]
	w.sigs[path] = sig
	return ok && prev == sig && time.Since(sig.newest) > settleTime
}

func measure(path string) (signature, error) {
	var s signature
	err := filepath.WalkDir(path, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		s.count++
		if info.Mode().IsRegular() {
			s.size += info.Size()
		}
		if info.ModTime().After(s.newest) {
			s.newest = info.ModTime()
		}
		return nil
	})
	return s, err
}

func (w *Watcher) store(ctx context.Context, path string) {
	w.work.Lock()
	defer w.work.Unlock()
	defer w.idle()
	name, shown := filepath.Base(path), w.rel(path)
	fail := func(err error) {
		w.retry[path] = time.Now().Add(retryAfter)
		w.cfg.Event("bad", fmt.Sprintf("could not store %s: %v (retrying in %s)", shown, err, retryAfter))
	}

	list, err := peers.Load(w.cfg.PeersPath)
	if err != nil {
		fail(err)
		return
	}
	online := peers.Online(ctx, list, func(ctx context.Context, p peers.Peer) error {
		_, err := w.cfg.Client.Info(ctx, p.Addr)
		return err
	})
	if n := peers.Machines(online); n < minPeers {
		fail(fmt.Errorf("only %d machines online, need %d so that losing one cannot lose the file", n, minPeers))
		return
	}

	info, err := os.Lstat(path)
	if err != nil {
		fail(err)
		return
	}
	size, err := measure(path)
	if err != nil {
		fail(err)
		return
	}
	stub := path + files.StubExt
	var prev *manifest.Manifest
	if m, err := files.ReadStub(stub); err == nil && m.SharedBy == nil {
		prev = &m
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		fail(fmt.Errorf("its stub %s can't be read: %w", filepath.Base(stub), err))
		return
	}
	mtime := modTime(path)
	if prev != nil {
		w.cfg.Event("info", fmt.Sprintf("storing a new version of %s (%s) on %d nodes", shown, human(size.size), len(online)))
	} else {
		w.cfg.Event("info", fmt.Sprintf("storing %s (%s) on %d nodes", shown, human(size.size), len(online)))
	}
	start := time.Now()
	putCtx := w.begin(ctx, "storing", shown, size.size)
	opts := files.PutOptions{ChunkSize: w.cfg.ChunkSize, Challenges: w.cfg.Challenges, Previous: prev}
	var m manifest.Manifest
	var chal files.Challenges
	var sum string
	if info.IsDir() {
		m, chal, sum, err = files.PutFolder(putCtx, w.cfg.Client, path, online, opts)
	} else {
		var f *os.File
		if f, err = os.Open(path); err == nil {
			m, chal, sum, err = files.PutReader(putCtx, w.cfg.Client, f, name, online, opts)
			f.Close()
		}
	}
	if err != nil {
		fail(err)
		return
	}
	if prev != nil && m.PlaintextSize == prev.PlaintextSize && len(m.Chunks) == len(prev.Chunks) && files.Reused(m, *prev) == len(m.Chunks) {
		// Nothing changed: keep the version there is, with the new time.
		prev.SourceModTime = mtime
		if err := files.WriteJSON(stub, *prev); err != nil {
			fail(err)
			return
		}
		msg := fmt.Sprintf("%s is the same as the version already stored; no new version made", shown)
		if w.cfg.Keep {
			w.cfg.Event("ok", msg)
			return
		}
		if err := os.RemoveAll(path); err != nil {
			w.cfg.Event("warn", msg+"; could not remove it: "+err.Error())
			return
		}
		w.cfg.Event("ok", msg+"; original replaced by "+filepath.Base(stub))
		return
	}
	// Before anything is deleted, prove the network can give it back intact.
	if err := files.ReadBack(w.begin(ctx, "verifying", shown, int64(m.PlaintextSize)), w.cfg.Client, m, sum); err != nil {
		fail(fmt.Errorf("read-back check failed, original kept: %w", err))
		return
	}
	m.StoredAt, m.SourceModTime = time.Now().Unix(), mtime
	versionNote := ""
	if prev != nil {
		// The old version goes into the history; its shards stay.
		lineage, err := w.archive(stub, *prev)
		if err != nil {
			files.Delete(context.WithoutCancel(ctx), w.cfg.Client, only(m, []manifest.Manifest{*prev}))
			fail(fmt.Errorf("could not keep the previous version: %w", err))
			return
		}
		m.Lineage = lineage
		mergeChallenges(&chal, m, stub)
		reused := files.Reused(m, *prev)
		versionNote = fmt.Sprintf("; new version, %d of %d chunks changed, previous version kept in history", len(m.Chunks)-reused, len(m.Chunks))
	}
	if err := files.WriteJSON(files.ChallengesPath(stub), chal); err != nil {
		fail(err)
		return
	}
	if err := files.WriteJSON(stub, m); err != nil {
		fail(err)
		return
	}
	what := "file"
	if info.IsDir() {
		what = fmt.Sprintf("folder of %d files", m.FileCount)
	}
	msg := fmt.Sprintf("stored %s (%s, %s, %d chunks on %d nodes, %.1fs, read-back verified%s)",
		shown, what, human(int64(m.PlaintextSize)), len(m.Chunks), len(online), time.Since(start).Seconds(), versionNote)
	if prev != nil {
		defer w.prune(context.WithoutCancel(ctx), m)
	}
	if w.cfg.Keep {
		w.cfg.Event("ok", msg+"; original kept")
		return
	}
	if err := os.RemoveAll(path); err != nil {
		w.cfg.Event("warn", msg+"; could not remove original: "+err.Error())
		return
	}
	w.cfg.Event("ok", msg+"; original replaced by "+filepath.Base(stub))
}

func (w *Watcher) restoreDropped(ctx context.Context, stub string) {
	target, err := w.RestoreStub(ctx, stub)
	if err != nil {
		w.retry[stub] = time.Now().Add(retryAfter)
		return
	}
	done := filepath.Join(w.cfg.Dir, privateDir, "restored-stubs", filepath.Base(target)+files.StubExt)
	if err := os.Rename(stub, done); err != nil {
		w.cfg.Event("warn", "restored, but could not move the stub out of restore/: "+err.Error())
	}
}

// RestoreStub rebuilds the item a stub describes into the restore folder
// (see RestoreDir) and returns its full path.
func (w *Watcher) RestoreStub(ctx context.Context, stub string) (string, error) {
	w.work.Lock()
	defer w.work.Unlock()
	m, err := files.ReadStub(stub)
	if err != nil {
		w.cfg.Event("bad", fmt.Sprintf("cannot read %s: %v", filepath.Base(stub), err))
		w.noteRestore(Restored{Name: filepath.Base(stub), Error: err.Error()})
		return "", err
	}
	defer w.idle()
	if w.inHistory(stub) {
		m.FileName = versionName(m.FileName, storedAt(m, stub))
	}
	start := time.Now()
	dir := w.RestoreDir()
	target, err := files.Restore(w.begin(ctx, "restoring", m.FileName, int64(m.PlaintextSize)), w.cfg.Client, m, dir, nil)
	if err != nil {
		w.cfg.Event("bad", fmt.Sprintf("restore of %s into %s failed: %v", m.FileName, dir, err))
		w.noteRestore(Restored{Name: m.FileName, Dir: dir, Size: int64(m.PlaintextSize), Error: err.Error()})
		return "", err
	}
	w.cfg.Event("ok", fmt.Sprintf("restored %s (%s) to %s in %.1fs", m.FileName, human(int64(m.PlaintextSize)), target, time.Since(start).Seconds()))
	w.noteRestore(Restored{Name: m.FileName, Path: target, Dir: dir, Size: int64(m.PlaintextSize)})
	return target, nil
}

// DeleteStub removes the item a stub describes from every node, then every
// stub of it in the outbox. If some nodes are unreachable nothing is removed
// locally, so the delete can be retried.
func (w *Watcher) DeleteStub(ctx context.Context, stub string) error {
	w.work.Lock()
	defer w.work.Unlock()
	return w.deleteItem(ctx, stub)
}

// deleteDropped handles a stub dropped into delete/. On a partial failure the
// stub stays there and is retried later.
func (w *Watcher) deleteDropped(ctx context.Context, stub string) {
	w.work.Lock()
	defer w.work.Unlock()
	if err := w.deleteItem(ctx, stub); err != nil {
		w.retry[stub] = time.Now().Add(retryAfter)
		return
	}
	os.Remove(stub)
	os.Remove(files.ChallengesPath(stub))
}

func (w *Watcher) deleteItem(ctx context.Context, stub string) error {
	m, err := files.ReadStub(stub)
	if err != nil {
		w.cfg.Event("bad", fmt.Sprintf("cannot delete %s: %v", filepath.Base(stub), err))
		return err
	}
	if m.SharedBy != nil {
		// The shards are the sender's; only our record of them goes.
		if err := os.Remove(stub); err != nil && !os.IsNotExist(err) {
			return err
		}
		w.cfg.Event("ok", fmt.Sprintf("removed %s (shared by %s) from your list; the sender's copy is untouched", m.FileName, m.SharedBy.Name))
		return nil
	}
	versions := w.allVersions(m)
	res := files.Delete(ctx, w.cfg.Client, everyShard(versions))
	if err := res.Err(); err != nil {
		w.cfg.Event("warn", fmt.Sprintf("deleted %d shards of %s, but %d could not be deleted (node down?); stub kept, will retry",
			res.Deleted, m.FileName, len(res.Failed)))
		return err
	}
	w.forget(m)
	os.RemoveAll(w.historyPath(manifest.LineageOf(m)))
	also := ""
	if len(versions) > 1 {
		also = fmt.Sprintf(", with its %d older versions", len(versions)-1)
	}
	w.cfg.Event("ok", fmt.Sprintf("deleted %s (%s)%s from all nodes: %d shards", m.FileName, human(int64(m.PlaintextSize)), also, res.Deleted))
	return nil
}

// forget removes every stub of a deleted item from the outbox and from the
// restored-stubs record, and any challenges file for its shards (a stub moved
// into delete/ leaves its hidden challenges file behind).
func (w *Watcher) forget(m manifest.Manifest) {
	shard := map[string]bool{}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			shard[ref.Hash] = true
		}
	}
	restored := filepath.Join(w.cfg.Dir, RestoredDir)
	for _, root := range []string{w.cfg.Dir, filepath.Join(w.cfg.Dir, privateDir, "restored-stubs")} {
		filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			switch {
			case e.IsDir():
				if p == restored || (p != root && e.Name() == privateDir) {
					return filepath.SkipDir
				}
			case strings.HasSuffix(e.Name(), files.ChallengesExt):
				// Only orphans (their stub was moved away, e.g. into
				// delete/) need opening; the rest go with their stub below.
				stub := filepath.Join(filepath.Dir(p), strings.TrimSuffix(strings.TrimPrefix(e.Name(), "."), files.ChallengesExt))
				if _, err := os.Lstat(stub); err == nil {
					return nil
				}
				if chal, err := files.ReadChallenges(p); err == nil {
					for h := range chal.Shards {
						if shard[h] {
							os.Remove(p)
							break
						}
					}
				}
			case strings.HasSuffix(e.Name(), files.StubExt):
				if sm, err := files.ReadStub(p); err == nil && sm.FileID == m.FileID {
					os.Remove(p)
					os.Remove(files.ChallengesPath(p))
				}
			}
			return nil
		})
	}
}

// IsUnder reports whether path lies inside the watched folder.
func (w *Watcher) IsUnder(path string) bool {
	rel, err := filepath.Rel(w.cfg.Dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func human(n int64) string {
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
