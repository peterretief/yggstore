package workhere

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/share"
)

// Live is the files page served by the place holding a person's files
// itself, over Yggdrasil to their own devices: the same page as the link's,
// working on the folder directly, so changes are made at once and files of
// any size go up and down. Who may open it is the server's business (the
// dashboard lets in only the devices on its list).
type Live struct {
	Outbox string        // the folder: files/, whole/, received/ in it
	Client client.Client // fetches stored files from the group
	// Holding, if set, is whether this place holds the working copy; only
	// then may it change the folder.
	Holding func() bool
	// Header, if set, is a header the page must send with its requests
	// (the dashboard asks for X-Yggstore: 1).
	Header string
}

var liveMark = []byte(`<html lang="en" data-live="1">`)

func (l *Live) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := r.URL.Query().Get("op")
	switch {
	case op == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(bytes.Replace(page, []byte(`<html lang="en">`), liveMark, 1))
	case op == "get" && r.Method == http.MethodGet:
		l.get(w, r, r.URL.Query().Get("path"))
	case r.Method != http.MethodPost:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case op == "list":
		var req struct{ Path string }
		json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
		lst, err := l.list(req.Path)
		if err != nil {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(lst)
	case op == "put":
		l.put(w, r)
	case op == "change":
		l.change(w, r)
	default:
		http.Error(w, "unknown request", http.StatusBadRequest)
	}
}

var errElsewhere = errors.New("Your files are being worked on at another place right now, so they can only be looked at here. Change them there, or on your link page.")

func (l *Live) holding() bool { return l.Holding == nil || l.Holding() }

// Websites is where the page shows the person's websites, which only this
// page has: a change to one is published as on the computer.
const Websites = "Your websites"

// tops are the folders the page shows at the top beside the person's files.
var tops = []struct{ dir, shown string }{{"whole", "Kept whole"}, {"received", "Sent to you"}, {"websites", Websites}}

// at is where a path the page shows is in the folder, and its stub.
func (l *Live) at(shown string) (real string, stub string, ok bool) {
	if p := cleanShown(shown); p == Websites || strings.HasPrefix(p, Websites+"/") {
		if strings.HasPrefix(path.Base(p), ".") {
			return "", "", false
		}
		real = filepath.Join(l.Outbox, "websites", filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(p, Websites), "/")))
		return real, real + files.StubExt, true
	}
	for _, t := range tops {
		if cleanShown(shown) == t.shown {
			real = filepath.Join(l.Outbox, t.dir)
			return real, real + files.StubExt, true
		}
	}
	real, ok = realPath(l.Outbox, shown)
	return real, real + files.StubExt, ok
}

// website is whether p is in one of the person's websites (not a site's
// own folder, which is made and named on the computer).
func website(p string) bool {
	r, ok := strings.CutPrefix(p, Websites+"/")
	return ok && strings.Contains(r, "/")
}

// changeable is whether the page may make, move or delete p here.
func changeable(p string) bool {
	return website(p) || (editable(p) && p != Websites && !strings.HasPrefix(p, Websites+"/"))
}

// sameSite is whether a move stays where it may: in the person's files, or
// in the one website.
func sameSite(a, b string) bool {
	site := func(p string) string {
		if !website(p) {
			return ""
		}
		return strings.SplitN(p, "/", 3)[1]
	}
	return site(a) == site(b)
}

func (l *Live) exists(shown string) bool {
	real, stub, ok := l.at(shown)
	return ok && (exists(real) || exists(stub))
}

func (l *Live) isFolder(shown string) bool {
	if shown == "" {
		return true
	}
	real, _, ok := l.at(shown)
	info, err := os.Stat(real)
	return ok && err == nil && info.IsDir()
}

// hidden is what the page doesn't show: the outbox's own files and files
// being written.
func hidden(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, files.ChallengesExt) || strings.HasSuffix(name, share.Ext) || isPartial(name)
}

func isPartial(name string) bool {
	for _, suf := range []string{".part", ".crdownload", ".tmp", "~"} {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

func (l *Live) list(dir string) (Listing, error) {
	dir = cleanShown(dir)
	out := Listing{Path: dir, Entries: []Entry{}}
	var real string
	if dir == "" {
		real = filepath.Join(l.Outbox, "files")
		for _, t := range tops {
			ents, _ := os.ReadDir(filepath.Join(l.Outbox, t.dir))
			for _, e := range ents {
				if !hidden(e.Name()) {
					out.Entries = append(out.Entries, Entry{Name: t.shown, Folder: true})
					break
				}
			}
		}
	} else {
		var ok bool
		if real, _, ok = l.at(dir); !ok || !l.isFolder(dir) {
			return out, errors.New("That folder isn't in your files (any more).")
		}
	}
	plain := dir == Websites || strings.HasPrefix(dir, Websites+"/") // a site's files are kept as they are
	ents, err := os.ReadDir(real)
	if err != nil && dir != "" {
		return out, err
	}
	for _, e := range ents {
		name := e.Name()
		if hidden(name) {
			continue
		}
		if e.IsDir() {
			out.Entries = append(out.Entries, Entry{Name: name, Folder: true})
			continue
		}
		if base, ok := strings.CutSuffix(name, files.StubExt); ok {
			en := Entry{Name: base}
			if m, err := files.ReadStub(filepath.Join(real, name)); err == nil {
				en.Size, en.Whole = int64(m.PlaintextSize), m.Kind == files.KindFolder
				if en.Whole {
					en.Size = m.ContentBytes
				}
			}
			out.Entries = append(out.Entries, en)
			continue
		}
		if exists(filepath.Join(real, name+files.StubExt)) {
			continue // a new version being stored; its stub is listed
		}
		info, _ := e.Info()
		en := Entry{Name: name, Note: "storing…"}
		if plain {
			en.Note = ""
		}
		if info != nil {
			en.Size = info.Size()
		}
		out.Entries = append(out.Entries, en)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Folder != b.Folder {
			return a.Folder
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	out.Folders = []string{""}
	root := filepath.Join(l.Outbox, "files")
	filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil || !e.IsDir() || p == root {
			return nil
		}
		if hidden(e.Name()) {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		out.Folders = append(out.Folders, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out.Folders)
	return out, nil
}

func (l *Live) get(w http.ResponseWriter, r *http.Request, shown string) {
	shown = cleanShown(shown)
	real, stub, ok := l.at(shown)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := path.Base(shown)
	m, err := files.ReadStub(stub)
	if err != nil {
		// Not stored yet: the file itself.
		if info, err := os.Stat(real); err == nil && !info.IsDir() {
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
			http.ServeFile(w, r, real)
			return
		}
		http.Error(w, "That file isn't in your files (any more).", http.StatusNotFound)
		return
	}
	sendStored(w, r.Context(), l.Client, m, name, func(string, ...any) {})
}

func (l *Live) put(w http.ResponseWriter, r *http.Request) {
	if !l.holding() {
		jsonError(w, http.StatusConflict, errElsewhere.Error())
		return
	}
	name, _ := urlUnescape(r.Header.Get("X-Name"))
	dir, _ := urlUnescape(r.Header.Get("X-Dir"))
	rel := cleanPath(cleanShown(dir), name)
	dest, _, ok := l.at(rel)
	if rel == "" || !ok || !changeable(rel) || (path.Dir(rel) != "." && !l.isFolder(path.Dir(rel))) {
		jsonError(w, http.StatusBadRequest, "A file can't be put there.")
		return
	}
	// Written beside it under a name the outbox ignores, then moved in
	// whole, so it isn't stored half written.
	tmp := filepath.Join(filepath.Dir(dest), ".uploading-"+newEntryID())
	f, err := os.Create(tmp)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, err = io.Copy(f, r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		jsonError(w, http.StatusBadRequest, "The upload stopped: "+err.Error())
		return
	}
	if r.Header.Get("X-Replace") != "1" {
		dest = freeName(dest)
	}
	// Put where its stub is, the outbox stores it as a new version.
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": rel})
}

func (l *Live) change(w http.ResponseWriter, r *http.Request) {
	var ch Change
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&ch); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	if !l.holding() {
		jsonError(w, http.StatusConflict, errElsewhere.Error())
		return
	}
	ch.Path, ch.To = cleanShown(ch.Path), cleanShown(ch.To)
	msg := ""
	from, _, ok := l.at(ch.Path)
	switch {
	case !ok:
		msg = "That isn't in your files."
	case ch.Op == OpDelete:
		if !changeable(ch.Path) {
			msg = "That can't be deleted here."
		} else if !l.exists(ch.Path) {
			msg = "That isn't in your files (any more)."
		} else if err := deleteAt(l.Outbox, from, newEntryID()); err != nil {
			msg = "Deleting it failed: " + err.Error()
		}
	case ch.Op == OpFolder:
		switch {
		case !changeable(ch.Path) || cleanPath(path.Dir(ch.Path), path.Base(ch.Path)) != ch.Path:
			msg = "A folder can't be made with that name here."
		case l.exists(ch.Path):
			msg = "There's already something called " + path.Base(ch.Path) + " here."
		case path.Dir(ch.Path) != "." && !l.isFolder(path.Dir(ch.Path)):
			msg = "That folder isn't there (any more)."
		default:
			if err := os.Mkdir(from, 0o755); err != nil {
				msg = err.Error()
			}
		}
	case ch.Op == OpMove:
		to, _, _ := l.at(ch.To)
		switch {
		case !l.exists(ch.Path) || !changeable(ch.Path):
			msg = "That can't be moved: it isn't in your own files (any more)."
		case !changeable(ch.To) || cleanPath(path.Dir(ch.To), path.Base(ch.To)) != ch.To:
			msg = "That name can't be used here."
		case !sameSite(ch.Path, ch.To):
			msg = "Things can't be moved between your files and a website, or between websites."
		case ch.To == ch.Path || strings.HasPrefix(ch.To, ch.Path+"/"):
			msg = "A folder can't be moved into itself."
		case l.exists(ch.To):
			msg = "There's already something called " + path.Base(ch.To) + " there."
		case path.Dir(ch.To) != "." && !l.isFolder(path.Dir(ch.To)):
			msg = "That folder isn't there (any more)."
		default:
			if err := moveTo(from, to); err != nil {
				msg = "Moving it failed: " + err.Error()
			}
		}
	default:
		msg = "unknown change"
	}
	if msg != "" {
		jsonError(w, http.StatusBadRequest, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

func urlUnescape(s string) (string, error) { return url.PathUnescape(s) }
