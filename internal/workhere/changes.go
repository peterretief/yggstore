package workhere

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/recovery"
)

// Changing: deleting, moving or renaming, and new folders, from the link
// page. Like uploads they wait in the person's inbox until the place
// holding their working copy makes them; meanwhile the page shows the
// folder as it will be. A web node shows what waits on the group's other
// web nodes too, since the page's requests may reach any of them.

// What a change does.
const (
	OpDelete = "delete"
	OpMove   = "move" // and rename
	OpFolder = "folder"
)

// Change is what the page asks, as sealed in the inbox. Paths are as the
// page shows them.
type Change struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	To   string `json:"to,omitempty"` // a move's new path
}

// item is a file or folder as the page shows it.
type item struct {
	stub   []byte // nil for a folder, and for a file not downloadable yet
	folder bool
	note   string // why it isn't as in the copy yet
}

// tree is a person's files as the page shows them: the latest copy, with
// what waits, and what was made since the copy, applied.
type tree map[string]*item

// unlock opens a person's record with their link.
func (s *Store) unlock(id, link string) (Record, []byte, error) {
	rec, err := s.get(id)
	if err != nil {
		return rec, nil, errNoRecord
	}
	key, err := recovery.Key(link, rec.GroupHalf)
	if err != nil {
		return rec, nil, errBadLink
	}
	if _, err := recovery.Open(key, rec.Sealed); err != nil {
		return rec, nil, errWrongLink
	}
	return rec, key, nil
}

func (v *view) tree() tree {
	t := tree{}
	for rel, b := range v.stubs {
		t[strings.TrimSuffix(showPath(rel), files.StubExt)] = &item{stub: b}
	}
	for _, d := range v.dirs {
		if t[d] == nil {
			t[d] = &item{folder: true}
		}
	}
	return t
}

// isFolder is whether p is a folder: one of its own, or one with things in it.
func (t tree) isFolder(p string) bool {
	if p == "" {
		return true
	}
	if it := t[p]; it != nil {
		return it.folder
	}
	for k := range t {
		if strings.HasPrefix(k, p+"/") {
			return true
		}
	}
	return false
}

func (t tree) has(p string) bool { return t[p] != nil || t.isFolder(p) }

// under lists the paths at and in p.
func (t tree) under(p string) []string {
	var out []string
	for k := range t {
		if k == p || strings.HasPrefix(k, p+"/") {
			out = append(out, k)
		}
	}
	return out
}

// apply shows an entry's effect. Applying one the copy already holds
// changes nothing.
func (t tree) apply(wt Waiting, plain []byte) {
	waiting := wt.Done == 0
	if wt.Op == "" {
		switch {
		case waiting && (t[wt.Path] == nil || wt.Replace):
			note := "arriving in your files…"
			if t[wt.Path] != nil {
				note = "new version arriving…"
			}
			t[wt.Path] = &item{note: note}
		case !waiting && t[wt.Path] == nil:
			// In the working copy, and not in a copy here yet; its first
			// copy, which the page could download, is gone.
			t[wt.Path] = &item{note: "in your files; can be downloaded in a few minutes"}
		}
		return
	}
	var ch Change
	if json.Unmarshal(plain, &ch) != nil {
		return
	}
	switch ch.Op {
	case OpDelete:
		for _, k := range t.under(ch.Path) {
			delete(t, k)
		}
	case OpFolder:
		if !t.has(ch.Path) {
			t[ch.Path] = &item{folder: true}
		}
	case OpMove:
		if t.has(ch.To) {
			return
		}
		if t[ch.Path] == nil && t.isFolder(ch.Path) {
			t[ch.Path] = &item{folder: true} // so an empty one moves too
		}
		for _, k := range t.under(ch.Path) {
			it := t[k]
			delete(t, k)
			if waiting {
				it = &item{stub: it.stub, folder: it.folder, note: "moving…"}
			}
			t[ch.To+strings.TrimPrefix(k, ch.Path)] = it
		}
	}
}

// queued is an entry from this web node or another, opened.
type queued struct {
	wt    Waiting
	plain []byte
}

// otherCache keeps the group's web nodes for a while.
type otherCache struct {
	mu    sync.Mutex
	nodes []string
	at    time.Time
}

func (s *Store) otherNodes(ctx context.Context) []string {
	if s.Others == nil {
		return nil
	}
	s.others.mu.Lock()
	defer s.others.mu.Unlock()
	if time.Since(s.others.at) > 5*time.Minute {
		s.others.nodes, s.others.at = s.Others(ctx), time.Now()
	}
	return s.others.nodes
}

// queue is what waits for a person, and what was made lately, on every
// web node, in the order it was made.
func (s *Store) queue(ctx context.Context, id string, key []byte) []queued {
	seen := map[string]bool{}
	var all []Waiting
	add := func(list []Waiting) {
		for _, wt := range list {
			if !seen[wt.ID] {
				seen[wt.ID] = true
				all = append(all, wt)
			}
		}
	}
	add(s.inbox(id))
	add(s.done(id))
	if nodes := s.otherNodes(ctx); len(nodes) > 0 {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, n := range nodes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				var list []Waiting
				if getJSON(c, s.httpClient(), "http://"+n+"/v1/work/"+id+"/inbox?all=1", &list) == nil {
					mu.Lock()
					add(list)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	var out []queued
	for _, wt := range all {
		if plain, err := recovery.Open(key, wt.Sealed); err == nil {
			out = append(out, queued{wt, plain})
		}
	}
	return out
}

func (s *Store) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: time.Minute}
}

// current is a person's files as the page shows them.
func (s *Store) current(ctx context.Context, id, link string) (tree, error) {
	v, err := s.view(ctx, id, link)
	if err != nil {
		return nil, err
	}
	_, key, err := s.unlock(id, link)
	if err != nil {
		return nil, err
	}
	t := v.tree()
	for _, q := range s.queue(ctx, id, key) {
		t.apply(q.wt, q.plain)
	}
	return t, nil
}

// editable is whether a path may be made, moved or moved to: only in the
// person's own files, not what is kept whole or was sent to them.
func editable(p string) bool {
	return p != "" && p != "Kept whole" && p != "Sent to you" && !strings.HasPrefix(p, "Kept whole/") && !strings.HasPrefix(p, "Sent to you/")
}

func cleanShown(p string) string { return strings.Trim(path.Clean("/"+p), "/") }

// change takes a change from the page.
func (s *Store) change(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID, Link string
		Change
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	t, err := s.current(ctx, req.ID, req.Link)
	if err != nil {
		viewError(w, err, true)
		return
	}
	_, key, _ := s.unlock(req.ID, req.Link)
	ch := Change{Op: req.Op, Path: cleanShown(req.Path)}
	bad := func(msg string) { jsonError(w, http.StatusBadRequest, msg) }
	switch ch.Op {
	case OpDelete:
		if ch.Path == "" || topFolder(ch.Path) || !t.has(ch.Path) {
			bad("That isn't in your files (any more).")
			return
		}
	case OpFolder:
		if !editable(ch.Path) || cleanPath(path.Dir(ch.Path), path.Base(ch.Path)) != ch.Path {
			bad("A folder can't be made with that name here.")
			return
		}
		if t.has(ch.Path) {
			bad("There's already something called " + path.Base(ch.Path) + " here.")
			return
		}
		if parent := path.Dir(ch.Path); parent != "." && !t.isFolder(parent) {
			bad("That folder isn't there (any more).")
			return
		}
	case OpMove:
		ch.To = cleanShown(req.To)
		msg := ""
		switch {
		case !t.has(ch.Path) || !editable(ch.Path):
			msg = "That can't be moved: it isn't in your own files (any more)."
		case !editable(ch.To) || cleanPath(path.Dir(ch.To), path.Base(ch.To)) != ch.To:
			msg = "That name can't be used here."
		case ch.To == ch.Path || strings.HasPrefix(ch.To, ch.Path+"/"):
			msg = "A folder can't be moved into itself."
		case t.has(ch.To):
			msg = "There's already something called " + path.Base(ch.To) + " there."
		case path.Dir(ch.To) != "." && !t.isFolder(path.Dir(ch.To)):
			msg = "That folder isn't there (any more)."
		}
		if msg != "" {
			bad(msg)
			return
		}
	default:
		bad("unknown change")
		return
	}
	plain, _ := json.Marshal(ch)
	sealed, err := recovery.Seal(key, plain)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256(plain)
	if err := s.enqueue(req.ID, Waiting{ID: newEntryID(), Op: ch.Op, Sealed: sealed, Sum: hex.EncodeToString(sum[:]), At: time.Now().Unix()}); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

// listing is a folder of the tree, and every folder the page may move to.
func (t tree) listing(dir string) (Listing, bool) {
	dir = cleanShown(dir)
	out := Listing{Path: dir, Entries: []Entry{}}
	if !t.isFolder(dir) {
		return out, false
	}
	seen := map[string]bool{}
	folders := map[string]bool{"": true}
	for k, it := range t {
		parts := strings.Split(k, "/")
		for i := range parts {
			f := strings.Join(parts[:i+1], "/")
			if (i < len(parts)-1 || it.folder) && editable(f) {
				folders[f] = true
			}
		}
		under := k
		if dir != "" {
			var ok bool
			if under, ok = strings.CutPrefix(k, dir+"/"); !ok {
				continue
			}
		}
		first, _, nested := strings.Cut(under, "/")
		if nested || it.folder {
			if !seen[first] {
				seen[first] = true
				note := ""
				if !nested {
					note = it.note
				}
				out.Entries = append(out.Entries, Entry{Name: first, Folder: true, Note: note})
			} else if !nested && it.note != "" {
				for i := range out.Entries {
					if out.Entries[i].Name == first && out.Entries[i].Folder {
						out.Entries[i].Note = it.note
					}
				}
			}
			continue
		}
		e := Entry{Name: first, Note: it.note}
		if it.stub != nil {
			if m, err := manifest.Unmarshal(it.stub); err == nil {
				e.Size, e.Whole = int64(m.PlaintextSize), m.Kind == files.KindFolder
				if e.Whole {
					e.Size = m.ContentBytes
				}
			}
		}
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Folder != b.Folder {
			return a.Folder
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for f := range folders {
		out.Folders = append(out.Folders, f)
	}
	sort.Strings(out.Folders)
	return out, true
}
