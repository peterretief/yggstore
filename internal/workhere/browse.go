package workhere

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/recovery"
)

// Browsing: with their link, a person sees the folders of their latest
// recovery copy and downloads any file in it, fetched from the group's
// nodes. Nothing is changed this way, so it needs no place to hold the
// working copy, and works while the person's computer and box are off.

// maxCopy is the largest recovery copy the web node reads to browse.
const maxCopy = 64 << 20

// view is a recovery copy as the browser sees it: its stubs, by path.
type view struct {
	fileID string
	at     time.Time // when the copy was saved, as near as is known
	stubs  map[string][]byte
	used   time.Time
}

type viewCache struct {
	mu    sync.Mutex
	views map[string]*view // by record ID
}

// Entry is a folder or a file in a listing.
type Entry struct {
	Name   string `json:"name"`
	Folder bool   `json:"folder,omitempty"` // a folder of the tree
	Whole  bool   `json:"whole,omitempty"`  // a folder stored as one item, downloaded as .tar
	Size   int64  `json:"size,omitempty"`
	// Arriving: uploaded, and not in the working copy yet.
	Arriving bool `json:"arriving,omitempty"`
}

// Listing is a folder of the copy.
type Listing struct {
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
	Saved   int64   `json:"saved,omitempty"` // when the copy was saved (unix seconds)
}

// view opens a person's latest copy with their link, reading it from the
// group the first time and when a newer one is recorded.
func (s *Store) view(ctx context.Context, id, link string) (*view, error) {
	rec, err := s.get(id)
	if err != nil {
		return nil, errNoRecord
	}
	key, err := recovery.Key(link, rec.GroupHalf)
	if err != nil {
		return nil, errBadLink
	}
	plain, err := recovery.Open(key, rec.Sealed)
	if err != nil {
		return nil, errWrongLink
	}
	m, err := manifest.Unmarshal(plain)
	if err != nil {
		return nil, err
	}
	s.cache.mu.Lock()
	if s.cache.views == nil {
		s.cache.views = map[string]*view{}
	}
	if v := s.cache.views[id]; v != nil && v.fileID == m.FileID {
		v.used = time.Now()
		s.cache.mu.Unlock()
		return v, nil
	}
	s.cache.mu.Unlock()
	if m.PlaintextSize > maxCopy {
		return nil, errors.New("this copy is too big to browse here")
	}
	var buf bytes.Buffer
	if err := files.Get(ctx, s.shards(), m, &buf, func(string, ...any) {}); err != nil {
		return nil, err
	}
	v := &view{fileID: m.FileID, at: time.Unix(rec.Updated, 0), stubs: map[string][]byte{}, used: time.Now()}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(name, "outbox/") || !strings.HasSuffix(name, files.StubExt) {
			continue
		}
		rel := strings.TrimPrefix(name, "outbox/")
		top, _, _ := strings.Cut(rel, "/")
		if top != "files" && top != "whole" && top != "received" {
			continue // history and the like
		}
		b, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, err
		}
		v.stubs[rel] = b
	}
	s.cache.mu.Lock()
	s.cache.views[id] = v
	for k, old := range s.cache.views { // a few people at most: drop the idle
		if time.Since(old.used) > 30*time.Minute {
			delete(s.cache.views, k)
		}
	}
	s.cache.mu.Unlock()
	return v, nil
}

var (
	errNoRecord  = errors.New("This link isn't known here. It may be from before the newest email: use the link in that one.")
	errBadLink   = errors.New("This link is incomplete: open it from the email again, all of it.")
	errWrongLink = errors.New("This link doesn't open your files: use the link in your newest email.")
)

func (s *Store) shards() client.Client {
	if s.Shards.HTTP != nil {
		return s.Shards
	}
	return client.New()
}

// list is a folder of the copy. The top shows files/ as it is, with whole/
// and received/ as folders beside its own.
func (v *view) list(dir string) (Listing, error) {
	dir = strings.Trim(path.Clean("/"+dir), "/")
	out := Listing{Path: dir, Entries: []Entry{}, Saved: v.at.Unix()}
	seen := map[string]bool{}
	for rel, b := range v.stubs {
		shown := showPath(rel)
		var under string
		switch {
		case dir == "":
			under = shown
		case strings.HasPrefix(shown, dir+"/"):
			under = strings.TrimPrefix(shown, dir+"/")
		default:
			continue
		}
		first, rest, nested := strings.Cut(under, "/")
		if nested {
			if !seen[first+"/"] {
				seen[first+"/"] = true
				out.Entries = append(out.Entries, Entry{Name: first, Folder: true})
			}
			continue
		}
		_ = rest
		e := Entry{Name: strings.TrimSuffix(first, files.StubExt)}
		if m, err := manifest.Unmarshal(b); err == nil {
			e.Size, e.Whole = int64(m.PlaintextSize), m.Kind == files.KindFolder
			if e.Whole {
				e.Size = m.ContentBytes
			}
		}
		out.Entries = append(out.Entries, e)
	}
	if dir != "" && len(out.Entries) == 0 {
		return out, errors.New("no such folder in your files")
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Folder != b.Folder {
			return a.Folder
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out, nil
}

// showPath is where a stub appears in the browser: files/ is the top, and
// whole/ and received/ are folders in it.
func showPath(rel string) string {
	if r, ok := strings.CutPrefix(rel, "files/"); ok {
		return r
	}
	if r, ok := strings.CutPrefix(rel, "whole/"); ok {
		return "Kept whole/" + r
	}
	if r, ok := strings.CutPrefix(rel, "received/"); ok {
		return "Sent to you/" + r
	}
	return rel
}

// stub finds the stub shown at p.
func (v *view) stub(p string) ([]byte, bool) {
	p = strings.Trim(path.Clean("/"+p), "/")
	for rel, b := range v.stubs {
		if strings.TrimSuffix(showPath(rel), files.StubExt) == p {
			return b, true
		}
	}
	return nil, false
}

// browse answers the page's listing (JSON) and download (form) requests.
func (s *Store) browse(w http.ResponseWriter, r *http.Request, op string) {
	var id, link, p string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req struct{ ID, Link, Path string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "bad request")
			return
		}
		id, link, p = req.ID, req.Link, req.Path
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id, link, p = r.PostForm.Get("id"), r.PostForm.Get("link"), r.PostForm.Get("path")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	v, err := s.view(ctx, id, link)
	if err != nil {
		code := http.StatusBadGateway
		switch err {
		case errNoRecord:
			code = http.StatusNotFound
		case errBadLink:
			code = http.StatusBadRequest
		case errWrongLink:
			code = http.StatusForbidden
		}
		if op == "list" {
			jsonError(w, code, err.Error())
		} else {
			http.Error(w, err.Error(), code)
		}
		return
	}
	if op == "list" {
		l, err := v.list(p)
		if rec, gerr := s.get(id); gerr == nil {
			if key, kerr := recovery.Key(link, rec.GroupHalf); kerr == nil {
				for _, up := range s.pending(id, key) {
					if dir := path.Dir(up); dir == l.Path || (dir == "." && l.Path == "") {
						l.Entries = append(l.Entries, Entry{Name: path.Base(up), Arriving: true})
						err = nil
					}
				}
			}
		}
		if err != nil {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(l)
		return
	}
	b, ok := v.stub(p)
	if !ok {
		http.Error(w, "no such file in your files", http.StatusNotFound)
		return
	}
	m, err := manifest.Unmarshal(b)
	if err != nil {
		http.Error(w, "this file's stub is damaged", http.StatusInternalServerError)
		return
	}
	name := path.Base(strings.TrimSuffix(p, "/"))
	ctype := mime.TypeByExtension(path.Ext(name))
	if m.Kind == files.KindFolder {
		name, ctype = name+".tar", "application/x-tar"
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if m.Kind != files.KindFolder {
		w.Header().Set("Content-Length", itoa(int64(m.PlaintextSize)))
	}
	dctx, dcancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer dcancel()
	if err := files.Get(dctx, s.shards(), m, w, func(string, ...any) {}); err != nil {
		s.logf("files: downloading %s: %v", name, err)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
