package outbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
)

// History: when a file (or a whole/ folder) is stored again beside its
// stub, the old stub moves into .yggstore/history/LINEAGE/ and its shards
// stay in the group, so any version can be restored. A new version reuses
// the chunks that didn't change, so it only costs what changed. Versions
// beyond KeepVersions are pruned, oldest first.

const historyDir = "history"

// KeepVersions is how many older versions of each item are kept.
var KeepVersions = 20

// Version is one stored version of an item.
type Version struct {
	ID       string `json:"id"`   // "current", or the history file's name
	Stub     string `json:"stub"` // path of its stub
	Name     string `json:"name"`
	StoredAt int64  `json:"stored_at"` // unix seconds
	Size     int64  `json:"size"`
	Current  bool   `json:"current,omitempty"`
}

func (w *Watcher) historyRoot() string { return filepath.Join(w.cfg.Dir, privateDir, historyDir) }

func (w *Watcher) historyPath(lineage string) string {
	return filepath.Join(w.historyRoot(), lineage)
}

// storedAt is when the version in stub was stored: from the manifest, or
// for stubs from before history, the stub file's time.
func storedAt(m manifest.Manifest, stub string) int64 {
	if m.StoredAt != 0 {
		return m.StoredAt
	}
	if info, err := os.Stat(stub); err == nil {
		return info.ModTime().Unix()
	}
	return 0
}

func itemSize(m manifest.Manifest) int64 {
	if m.Kind == files.KindFolder && m.ContentBytes > 0 {
		return m.ContentBytes
	}
	return int64(m.PlaintextSize)
}

// history lists an item's older versions, newest first.
func (w *Watcher) history(lineage string) []Version {
	dir := w.historyPath(lineage)
	entries, _ := os.ReadDir(dir)
	var out []Version
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), files.StubExt) {
			continue
		}
		stub := filepath.Join(dir, e.Name())
		m, err := files.ReadStub(stub)
		if err != nil {
			continue
		}
		out = append(out, Version{ID: e.Name(), Stub: stub, Name: m.FileName, StoredAt: storedAt(m, stub), Size: itemSize(m)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StoredAt > out[j].StoredAt })
	return out
}

// Versions lists every version of the item a stub describes, the current
// one first.
func (w *Watcher) Versions(stub string) ([]Version, error) {
	m, err := files.ReadStub(stub)
	if err != nil {
		return nil, err
	}
	cur := Version{ID: "current", Stub: stub, Name: m.FileName, StoredAt: storedAt(m, stub), Size: itemSize(m), Current: true}
	return append([]Version{cur}, w.history(manifest.LineageOf(m))...), nil
}

// VersionCount is how many older versions the item in stub has.
func (w *Watcher) VersionCount(m manifest.Manifest) int {
	entries, _ := os.ReadDir(w.historyPath(manifest.LineageOf(m)))
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), files.StubExt) {
			n++
		}
	}
	return n
}

// VersionStub finds the stub of one version of the item in stub.
func (w *Watcher) VersionStub(stub, id string) (string, error) {
	if id == "" || id == "current" {
		return stub, nil
	}
	m, err := files.ReadStub(stub)
	if err != nil {
		return "", err
	}
	if id != filepath.Base(id) || !strings.HasSuffix(id, files.StubExt) {
		return "", errors.New("no such version")
	}
	p := filepath.Join(w.historyPath(manifest.LineageOf(m)), id)
	if _, err := os.Stat(p); err != nil {
		return "", errors.New("no such version")
	}
	return p, nil
}

// same reports whether the item at path is the version its stub holds, by
// size and modification time. Stubs from before history have no time: in
// the outbox's usual mode (originals removed once stored) a file beside its
// stub is then taken as new; with -keep, a file of the same size as the same.
func (w *Watcher) same(path string, m manifest.Manifest) bool {
	sig, err := measure(path)
	if err != nil {
		return true // can't tell; leave it alone
	}
	info, err := os.Lstat(path)
	if err != nil {
		return true
	}
	sz, mt := sig.size, sig.newest.UnixNano()
	if !info.IsDir() {
		mt = info.ModTime().UnixNano()
	}
	if sz != itemSize(m) {
		return false
	}
	if m.SourceModTime == 0 {
		return w.cfg.Keep
	}
	return mt == m.SourceModTime
}

func modTime(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if info.IsDir() {
		if sig, err := measure(path); err == nil {
			return sig.newest.UnixNano()
		}
	}
	return info.ModTime().UnixNano()
}

// archive moves the current version of an item (prev, in stub) into its
// history, ready for the new version to take its place. It returns the
// lineage the new version belongs to.
func (w *Watcher) archive(stub string, prev manifest.Manifest) (string, error) {
	lineage := manifest.LineageOf(prev)
	dir := w.historyPath(lineage)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	at := storedAt(prev, stub)
	name := time.Unix(at, 0).UTC().Format("20060102T150405Z") + "-" + prev.FileID[:min(8, len(prev.FileID))] + files.StubExt
	dst := filepath.Join(dir, name)
	if err := files.WriteJSON(dst, prev); err != nil {
		return "", err
	}
	if chal, err := os.ReadFile(files.ChallengesPath(stub)); err == nil {
		os.WriteFile(files.ChallengesPath(dst), chal, 0o600)
	}
	if rel, err := filepath.Rel(w.cfg.Dir, stub); err == nil {
		os.WriteFile(filepath.Join(dir, "path"), []byte(filepath.ToSlash(strings.TrimSuffix(rel, files.StubExt))), 0o600)
	}
	return lineage, nil
}

// mergeChallenges adds prev's challenges for the shards m reused.
func mergeChallenges(chal *files.Challenges, m manifest.Manifest, prevStub string) {
	old, err := files.ReadChallenges(files.ChallengesPath(prevStub))
	if err != nil {
		return
	}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if _, ok := chal.Shards[ref.Hash]; !ok {
				if p, ok := old.Shards[ref.Hash]; ok {
					chal.Shards[ref.Hash] = p
				}
			}
		}
	}
}

// shardKey identifies one stored shard.
type shardKey struct{ peer, hash string }

func shardsOf(m manifest.Manifest) map[shardKey]manifest.ShardRef {
	out := map[shardKey]manifest.ShardRef{}
	for _, ch := range m.Chunks {
		for _, r := range ch.Shards {
			out[shardKey{r.Peer, r.Hash}] = r
		}
	}
	for _, r := range m.Shards {
		out[shardKey{r.Peer, r.Hash}] = r
	}
	return out
}

// only returns m's shards that none of others use, as a manifest to delete.
func only(m manifest.Manifest, others []manifest.Manifest) manifest.Manifest {
	used := map[shardKey]bool{}
	for _, o := range others {
		for k := range shardsOf(o) {
			used[k] = true
		}
	}
	var refs []manifest.ShardRef
	for k, r := range shardsOf(m) {
		if !used[k] {
			refs = append(refs, r)
		}
	}
	return manifest.Manifest{Version: 2, FileID: m.FileID, FileName: m.FileName, Chunks: []manifest.Chunk{{Shards: refs}}}
}

// prune removes the oldest versions beyond KeepVersions, deleting the shards
// no remaining version uses. A version whose shards can't all be deleted
// (a node is down) stays, and is tried again after the next new version.
func (w *Watcher) prune(ctx context.Context, current manifest.Manifest) {
	hist := w.history(manifest.LineageOf(current))
	if len(hist) <= KeepVersions {
		return
	}
	keep := []manifest.Manifest{current}
	for _, v := range hist[:KeepVersions] {
		if m, err := files.ReadStub(v.Stub); err == nil {
			keep = append(keep, m)
		}
	}
	for _, v := range hist[KeepVersions:] {
		m, err := files.ReadStub(v.Stub)
		if err != nil {
			continue
		}
		res := files.Delete(ctx, w.cfg.Client, only(m, keep))
		if err := res.Err(); err != nil {
			w.cfg.Event("warn", fmt.Sprintf("could not prune the version of %s from %s: %v", m.FileName, day(v.StoredAt), err))
			keep = append(keep, m)
			continue
		}
		os.Remove(v.Stub)
		os.Remove(files.ChallengesPath(v.Stub))
		w.cfg.Event("info", fmt.Sprintf("pruned the version of %s from %s (only the latest %d older versions are kept)", m.FileName, day(v.StoredAt), KeepVersions))
	}
}

// allVersions is the current manifest and every older one of its lineage.
func (w *Watcher) allVersions(m manifest.Manifest) []manifest.Manifest {
	out := []manifest.Manifest{m}
	for _, v := range w.history(manifest.LineageOf(m)) {
		if old, err := files.ReadStub(v.Stub); err == nil {
			out = append(out, old)
		}
	}
	return out
}

// everyShard is one manifest naming every shard of every version, so one
// delete removes the whole history.
func everyShard(list []manifest.Manifest) manifest.Manifest {
	all := map[shardKey]manifest.ShardRef{}
	for _, m := range list {
		for k, r := range shardsOf(m) {
			all[k] = r
		}
	}
	refs := make([]manifest.ShardRef, 0, len(all))
	for _, r := range all {
		refs = append(refs, r)
	}
	return manifest.Manifest{Version: 2, Chunks: []manifest.Chunk{{Shards: refs}}}
}

func day(unix int64) string { return time.Unix(unix, 0).Format("2 Jan 2006 15:04") }

// versionName is the name an older version is restored under:
// "report (5 Oct 2026 14.32).docx".
func versionName(name string, at int64) string {
	ext := filepath.Ext(name)
	if ext == name {
		ext = ""
	}
	return strings.TrimSuffix(name, ext) + " (" + time.Unix(at, 0).Format("2 Jan 2006 15.04") + ")" + ext
}

func (w *Watcher) inHistory(stub string) bool {
	rel, err := filepath.Rel(w.historyRoot(), stub)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// RestoreFolderAt rebuilds a folder of the outbox (rel, as shown in the
// dashboard; "" for all of it) as it was at a moment: for each item in
// it, the latest version stored by then. Items stored only later are left
// out. It returns where the folder went and how many items it holds.
func (w *Watcher) RestoreFolderAt(ctx context.Context, rel string, at time.Time) (string, int, error) {
	rel = strings.Trim(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
	type pick struct {
		m    manifest.Manifest
		path string // slash-separated, relative to the outbox
	}
	picks := map[string]pick{} // by lineage
	consider := func(m manifest.Manifest, stub, path string) {
		t := storedAt(m, stub)
		if t > at.Unix() || m.SharedBy != nil {
			return
		}
		if rel != "" && path != rel && !strings.HasPrefix(path, rel+"/") {
			return
		}
		l := manifest.LineageOf(m)
		if p, ok := picks[l]; !ok || storedAt(p.m, "") < t {
			m.StoredAt = t
			picks[l] = pick{m, path}
		}
	}
	// Current stubs, where they are now.
	filepath.WalkDir(w.cfg.Dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() && p != w.cfg.Dir && filepath.Dir(p) == w.cfg.Dir &&
			(e.Name() == privateDir || e.Name() == RestoreDir || e.Name() == RestoredDir || e.Name() == DeleteDir || e.Name() == ReceivedDir) {
			return filepath.SkipDir
		}
		if e.IsDir() || !strings.HasSuffix(p, files.StubExt) {
			return nil
		}
		m, err := files.ReadStub(p)
		if err != nil {
			return nil
		}
		r, _ := filepath.Rel(w.cfg.Dir, strings.TrimSuffix(p, files.StubExt))
		consider(m, p, filepath.ToSlash(r))
		for _, v := range w.history(manifest.LineageOf(m)) {
			if old, err := files.ReadStub(v.Stub); err == nil {
				old.StoredAt = v.StoredAt
				consider(old, v.Stub, filepath.ToSlash(r))
			}
		}
		return nil
	})
	if len(picks) == 0 {
		err := fmt.Errorf("nothing in /%s was stored by %s", rel, at.Format("2 Jan 2006 15:04"))
		w.cfg.Event("bad", "restore of /"+rel+": "+err.Error())
		w.noteRestore(Restored{Name: "/" + rel + " as of " + at.Format("2 Jan 15:04"), Error: err.Error()})
		return "", 0, err
	}
	w.work.Lock()
	defer w.work.Unlock()
	defer w.idle()
	base := filepath.Base(w.cfg.Dir)
	if rel != "" {
		base = filepath.Base(filepath.FromSlash(rel))
	}
	target := freeDir(filepath.Join(w.RestoreDir(), base+" (as of "+at.Format("2 Jan 2006 15.04")+")"))
	var total int64
	for _, p := range picks {
		total += itemSize(p.m)
	}
	ctx = w.begin(ctx, "restoring", base+" as of "+at.Format("2 Jan 15:04"), total)
	failed := 0
	for _, p := range picks {
		sub := filepath.FromSlash(p.path)
		if rel != "" {
			sub, _ = filepath.Rel(filepath.FromSlash(rel), sub)
		}
		dest := filepath.Join(target, filepath.Dir(sub))
		m := p.m
		m.FileName = filepath.Base(sub)
		if _, err := files.Restore(ctx, w.cfg.Client, m, dest, nil); err != nil {
			failed++
			w.cfg.Event("bad", fmt.Sprintf("restoring %s as of %s: %v", p.path, at.Format("2 Jan 15:04"), err))
		}
	}
	n := len(picks) - failed
	msg := fmt.Sprintf("restored %s as it was on %s (%d items, %s) to %s", "/"+rel, at.Format("2 Jan 2006 15:04"), n, human(total), target)
	if failed > 0 {
		w.cfg.Event("warn", msg+fmt.Sprintf("; %d items could not be restored", failed))
		w.noteRestore(Restored{Name: "/" + rel + " as of " + at.Format("2 Jan 15:04"), Path: target, Dir: w.RestoreDir(), Size: total,
			Error: fmt.Sprintf("%d of %d items could not be restored; the rest are in %s", failed, len(picks), target)})
		return target, n, fmt.Errorf("%d items could not be restored", failed)
	}
	w.cfg.Event("ok", msg)
	w.noteRestore(Restored{Name: "/" + rel + " as of " + at.Format("2 Jan 15:04"), Path: target, Dir: w.RestoreDir(), Size: total})
	return target, n, nil
}

func freeDir(path string) string {
	p := path
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); err != nil {
			return p
		}
		p = path + " " + strconv.Itoa(i)
	}
}

// HistoryDir holds the older versions' stubs.
func (w *Watcher) HistoryDir() string { return w.historyRoot() }
