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
	dirs   []string // folders of files/, as shown, so empty ones show too
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
	// Note says why it isn't as in the copy yet: arriving, moving and such.
	Note string `json:"note,omitempty"`
}

// Listing is a folder of the copy.
type Listing struct {
	Path    string   `json:"path"`
	Entries []Entry  `json:"entries"`
	Saved   int64    `json:"saved,omitempty"` // when the copy was saved (unix seconds)
	Folders []string `json:"folders"`         // every folder things can be moved to
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
		if d, ok := strings.CutPrefix(name, "outbox/files/"); ok && h.Typeflag == tar.TypeDir && !strings.HasPrefix(path.Base(d), ".") {
			v.dirs = append(v.dirs, d)
			continue
		}
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
	t, err := s.current(ctx, id, link)
	if err != nil {
		viewError(w, err, op == "list")
		return
	}
	if op == "list" {
		l, ok := t.listing(p)
		if !ok {
			jsonError(w, http.StatusNotFound, "That folder isn't in your files (any more).")
			return
		}
		if v, err := s.view(ctx, id, link); err == nil {
			l.Saved = v.at.Unix()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(l)
		return
	}
	it := t[cleanShown(p)]
	if it == nil || it.stub == nil {
		http.Error(w, "That file isn't in your files, or can't be downloaded yet.", http.StatusNotFound)
		return
	}
	b := it.stub
	m, err := manifest.Unmarshal(b)
	if err != nil {
		http.Error(w, "this file's stub is damaged", http.StatusInternalServerError)
		return
	}
	sendStored(w, r.Context(), s.shards(), m, path.Base(strings.TrimSuffix(p, "/")), s.logf)
}

// sendStored sends a stored file, or a folder kept whole as a .tar, to
// the browser to save.
func sendStored(w http.ResponseWriter, ctx context.Context, c client.Client, m manifest.Manifest, name string, logf func(string, ...any)) {
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
	dctx, dcancel := context.WithTimeout(ctx, 6*time.Hour)
	defer dcancel()
	if m.Kind != files.KindFolder {
		if err := files.Get(dctx, c, m, w, func(string, ...any) {}); err != nil {
			logf("files: downloading %s: %v", name, err)
		}
		return
	}
	// A folder is stored as a tar of what is in it; it's sent inside a
	// folder of its own name, so it unpacks into one.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(files.Get(dctx, c, m, pw, func(string, ...any) {})) }()
	err := inFolder(w, pr, strings.TrimSuffix(name, ".tar"))
	pr.CloseWithError(err)
	if err != nil {
		logf("files: downloading %s: %v", name, err)
	}
}

// inFolder copies a tar into dst with everything in it under top/.
func inFolder(dst io.Writer, src io.Reader, top string) error {
	tr, tw := tar.NewReader(src), tar.NewWriter(dst)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()}); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		if rel == "" {
			continue
		}
		h.Name = top + "/" + rel
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		if h.Typeflag == tar.TypeLink {
			h.Linkname = top + "/" + strings.TrimPrefix(path.Clean("/"+h.Linkname), "/")
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// viewError answers a failure to open a person's files.
func viewError(w http.ResponseWriter, err error, asJSON bool) {
	code := http.StatusBadGateway
	switch err {
	case errNoRecord:
		code = http.StatusNotFound
	case errBadLink:
		code = http.StatusBadRequest
	case errWrongLink:
		code = http.StatusForbidden
	}
	if asJSON {
		jsonError(w, code, err.Error())
	} else {
		http.Error(w, err.Error(), code)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
