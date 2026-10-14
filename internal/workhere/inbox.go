package workhere

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/recovery"
)

// Uploading and changing: a file uploaded on the link page is stored in the
// group by the web node at once, and waits in the person's inbox there, its
// stub sealed with their recovery key. Deleting, moving and new folders wait
// there too, sealed the same way (see changes.go). The place holding their
// working copy collects them in the order they were made: it fetches an
// uploaded file into the same folder of its outbox, where it is stored as
// its own (shards belong to the node that stores them), and makes the
// changes in its folder; then it has the web node mark each done (and
// delete an uploaded file's first copy).

// MaxUpload is the largest file the link page takes (Cloudflare's free
// plan stops bigger requests anyway).
const MaxUpload = 100 << 20

// Waiting is an uploaded file or a change in a person's inbox.
type Waiting struct {
	ID string `json:"id"` // ordered by when it was made
	// Op is "" for an uploaded file, whose stub is sealed; otherwise a
	// Change is sealed (its Op).
	Op      string `json:"op,omitempty"`
	Path    string `json:"path,omitempty"` // an uploaded file's place, as the page shows it
	Replace bool   `json:"replace,omitempty"`
	Sealed  []byte `json:"sealed"`
	Sum     string `json:"sum"` // SHA-256 of what is sealed, so the holder can mark it done
	At      int64  `json:"at"`
	// Done is when the holder made it (unix seconds): the page shows it
	// until a recovery copy made since has come.
	Done int64 `json:"done,omitempty"`
}

func (s *Store) inboxDir(id string) string { return filepath.Join(s.Dir, "inbox", id) }
func (s *Store) doneDir(id string) string  { return filepath.Join(s.inboxDir(id), "done") }

// keepDone is how long the page shows a change made but not yet in a copy.
const keepDone = 30 * time.Minute

func readWaiting(dir string) []Waiting {
	ents, _ := os.ReadDir(dir) // sorted by name: the order they were made
	var out []Waiting
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var wt Waiting
		if json.Unmarshal(b, &wt) == nil {
			out = append(out, wt)
		}
	}
	return out
}

// inbox is what waits for the holder.
func (s *Store) inbox(id string) []Waiting { return readWaiting(s.inboxDir(id)) }

// done is what the holder made lately, dropping what a newer copy holds
// (one recorded well after it was made) and what is old.
func (s *Store) done(id string) []Waiting {
	var updated int64
	if rec, err := s.get(id); err == nil {
		updated = rec.Updated
	}
	var out []Waiting
	for _, wt := range readWaiting(s.doneDir(id)) {
		if wt.Done < updated-120 || time.Since(time.Unix(wt.Done, 0)) > keepDone {
			os.Remove(filepath.Join(s.doneDir(id), wt.ID+".json"))
			continue
		}
		out = append(out, wt)
	}
	return out
}

// newEntryID orders entries by when they were made, on any web node.
func newEntryID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(b))
}

func (s *Store) enqueue(id string, wt Waiting) error {
	out, _ := json.Marshal(wt)
	if err := os.MkdirAll(s.inboxDir(id), 0o700); err != nil {
		return err
	}
	return atomicfile.Replace(filepath.Join(s.inboxDir(id), wt.ID+".json"), out, 0o600)
}

// cleanPath makes a path under files/ from what the page sends, or "".
func cleanPath(dir, name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." || strings.HasPrefix(name, ".") || strings.HasSuffix(name, files.StubExt) {
		return ""
	}
	p := strings.Trim(path.Clean("/"+dir+"/"+name), "/")
	if strings.HasPrefix(p, "Kept whole/") || strings.HasPrefix(p, "Sent to you/") {
		return ""
	}
	return p
}

// upload stores a file from the page and puts it in the person's inbox.
func (s *Store) upload(w http.ResponseWriter, r *http.Request) {
	id, link := r.Header.Get("X-Id"), r.Header.Get("X-Link")
	name, err := url.PathUnescape(r.Header.Get("X-Name")) // headers can't hold every character
	if err != nil {
		name = ""
	}
	dir, _ := url.PathUnescape(r.Header.Get("X-Dir"))
	dest := cleanPath(dir, name)
	if dest == "" {
		jsonError(w, http.StatusBadRequest, "That file name can't be used here.")
		return
	}
	rec, err := s.get(id)
	if err != nil {
		jsonError(w, http.StatusNotFound, errNoRecord.Error())
		return
	}
	key, err := recovery.Key(link, rec.GroupHalf)
	if err != nil {
		jsonError(w, http.StatusBadRequest, errBadLink.Error())
		return
	}
	if _, err := recovery.Open(key, rec.Sealed); err != nil {
		jsonError(w, http.StatusForbidden, errWrongLink.Error())
		return
	}
	if s.Online == nil {
		jsonError(w, http.StatusServiceUnavailable, "This web node can't store files.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	online, err := s.Online(ctx)
	if err != nil || len(online) == 0 {
		jsonError(w, http.StatusServiceUnavailable, "The group's nodes don't answer right now. Try again in a few minutes.")
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxUpload)
	m, _, _, err := files.PutReader(ctx, s.shards(), body, path.Base(dest), online, files.PutOptions{})
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("Files up to %d MB can be uploaded here; sync bigger ones through Syncthing.", MaxUpload>>20))
			return
		}
		jsonError(w, http.StatusBadGateway, "Storing it failed: "+err.Error())
		return
	}
	stub, _ := manifest.Marshal(m)
	sealed, err := recovery.Seal(key, stub)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256(stub)
	wt := Waiting{ID: newEntryID(), Path: dest, Replace: r.Header.Get("X-Replace") == "1", Sealed: sealed,
		Sum: hex.EncodeToString(sum[:]), At: time.Now().Unix()}
	if err := s.enqueue(id, wt); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logf("files: %s uploaded %d bytes; waiting for their working copy to take it", rec.Name, m.PlaintextSize)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": dest})
}

// serveInbox answers the places collecting: GET /v1/work/ID/inbox lists
// what waits (with ?all=1, what was made lately too, for the other web
// nodes' pages), and DELETE /v1/work/ID/inbox/ENTRY (with what was sealed)
// marks it done once the place has made it, deleting an uploaded file's
// first copy.
func (s *Store) serveInbox(w http.ResponseWriter, r *http.Request, id, entry string) {
	if !validID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && entry == "":
		w.Header().Set("Content-Type", "application/json")
		list := s.inbox(id)
		if r.URL.Query().Get("all") == "1" {
			list = append(list, s.done(id)...)
		}
		if list == nil {
			list = []Waiting{}
		}
		json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodDelete && entry != "":
		p := filepath.Join(s.inboxDir(id), filepath.Base(entry)+".json")
		b, err := os.ReadFile(p)
		if err != nil {
			w.WriteHeader(http.StatusNoContent) // gone already
			return
		}
		var wt Waiting
		json.Unmarshal(b, &wt)
		plain, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		sum := sha256.Sum256(plain)
		if err != nil || hex.EncodeToString(sum[:]) != wt.Sum {
			http.Error(w, "that isn't what waits here", http.StatusForbidden)
			return
		}
		if wt.Op == "" {
			m, err := manifest.Unmarshal(plain)
			if err != nil {
				http.Error(w, "bad stub", http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
			defer cancel()
			if res := files.Delete(ctx, s.shards(), m); res.Err() != nil {
				s.logf("files: deleting an uploaded file's first copy: %v (will retry)", res.Err())
				http.Error(w, res.Err().Error(), http.StatusBadGateway)
				return
			}
		}
		wt.Done = time.Now().Unix()
		out, _ := json.Marshal(wt)
		os.MkdirAll(s.doneDir(id), 0o700)
		atomicfile.Replace(filepath.Join(s.doneDir(id), wt.ID+".json"), out, 0o600)
		os.Remove(p)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Collect makes what waits for a person on the web nodes, in the order it
// was made there: uploaded files are fetched into their outbox, where it
// stores them as theirs, and deletes, moves and new folders are made in it.
// Each web node is then told it's done. It returns how many were made.
func Collect(ctx context.Context, c client.Client, nodes []string, ks recovery.Keys, outbox string, logf func(string, ...any)) (int, error) {
	key, err := recovery.Key(ks.LinkHalf, ks.GroupHalf)
	if err != nil {
		return 0, err
	}
	type at struct {
		node string
		wt   Waiting
	}
	var all []at
	for _, node := range nodes {
		var list []Waiting
		if err := getJSON(ctx, c.HTTP, "http://"+node+"/v1/work/"+ks.ID+"/inbox", &list); err != nil {
			continue // not a web node with this person's record, or down
		}
		for _, wt := range list {
			all = append(all, at{node, wt})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].wt.ID < all[j].wt.ID })
	made := loadMade(outbox)
	got := 0
	for _, a := range all {
		plain, err := recovery.Open(key, a.wt.Sealed)
		if err != nil {
			continue // not sealed with this person's key: not theirs to make
		}
		if !made.has(a.wt.ID) {
			what, err := apply(ctx, c, a.wt, plain, outbox)
			if err != nil {
				// Later ones may depend on it (a move after an upload):
				// they wait for the next time too.
				return got, fmt.Errorf("%s: %w", what, err)
			}
			made.add(outbox, a.wt.ID)
			got++
			if logf != nil && what != "" {
				logf("from the web: %s", what)
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+a.node+"/v1/work/"+ks.ID+"/inbox/"+a.wt.ID, bytes.NewReader(plain))
		if resp, err := c.HTTP.Do(req); err == nil {
			resp.Body.Close()
		} // else the web node is told next time
	}
	return got, nil
}

// made is the entries this outbox made lately, so none is made twice when
// a web node wasn't told.
type made []string

func madePath(outbox string) string { return filepath.Join(outbox, ".yggstore", "web-made.json") }

func loadMade(outbox string) made {
	var m made
	if b, err := os.ReadFile(madePath(outbox)); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (m made) has(id string) bool {
	for _, x := range m {
		if x == id {
			return true
		}
	}
	return false
}

func (m *made) add(outbox, id string) {
	*m = append(*m, id)
	if len(*m) > 500 {
		*m = (*m)[len(*m)-500:]
	}
	b, _ := json.Marshal(*m)
	os.MkdirAll(filepath.Dir(madePath(outbox)), 0o755)
	atomicfile.Replace(madePath(outbox), b, 0o644)
}

// apply makes one entry in the outbox, and says what it did. Making it a
// second time does nothing more.
func apply(ctx context.Context, c client.Client, wt Waiting, plain []byte, outbox string) (string, error) {
	if wt.Op == "" {
		return collectFile(ctx, c, wt, plain, outbox)
	}
	var ch Change
	if err := json.Unmarshal(plain, &ch); err != nil || ch.Op != wt.Op {
		return "", nil // damaged: skipped
	}
	from, ok := realPath(outbox, ch.Path)
	if !ok {
		return "", nil
	}
	switch ch.Op {
	case OpDelete:
		return "deleted " + ch.Path, deleteAt(outbox, from, wt.ID)
	case OpFolder:
		return "new folder " + ch.Path, os.MkdirAll(from, 0o755)
	case OpMove:
		to, ok := realPath(outbox, ch.To)
		if !ok {
			return "", nil
		}
		return "moved " + ch.Path + " to " + ch.To, moveTo(from, to)
	}
	return "", nil
}

// realPath is where a path the page shows is in the outbox.
func realPath(outbox, shown string) (string, bool) {
	p := strings.Trim(path.Clean("/"+shown), "/")
	if p == "" {
		return "", false
	}
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	top := "files"
	if r, ok := strings.CutPrefix(p, "Kept whole/"); ok {
		top, p = "whole", r
	} else if r, ok := strings.CutPrefix(p, "Sent to you/"); ok {
		top, p = "received", r
	}
	return filepath.Join(outbox, top, filepath.FromSlash(p)), true
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// deleteAt deletes a file or folder: its stubs go into delete/, which the
// outbox deletes from the group, and what wasn't stored yet goes at once.
func deleteAt(outbox, p, entry string) error {
	bin := filepath.Join(outbox, "delete")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	n := 0
	drop := func(stub string) error {
		n++
		return os.Rename(stub, filepath.Join(bin, fmt.Sprintf("%s-%d-%s", entry, n, filepath.Base(stub))))
	}
	if stub := p + files.StubExt; exists(stub) {
		if err := drop(stub); err != nil { // its challenges file goes when the outbox deletes it
			return err
		}
	}
	info, err := os.Lstat(p)
	if err != nil {
		return nil // gone already
	}
	if info.IsDir() {
		err := filepath.WalkDir(p, func(q string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), files.StubExt) {
				return drop(q)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return os.RemoveAll(p)
}

// moveTo moves or renames a file (its stub) or a folder.
func moveTo(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if stub := from + files.StubExt; exists(stub) {
		dest := freeName(to) + files.StubExt
		if err := os.Rename(stub, dest); err != nil {
			return err
		}
		if exists(files.ChallengesPath(stub)) {
			os.Rename(files.ChallengesPath(stub), files.ChallengesPath(dest))
		}
		return nil
	}
	if !exists(from) {
		return nil // moved already
	}
	return os.Rename(from, freeName(to))
}

// collectFile fetches an uploaded file into the outbox.
func collectFile(ctx context.Context, c client.Client, wt Waiting, stub []byte, outbox string) (string, error) {
	m, err := manifest.Unmarshal(stub)
	if err != nil {
		return "", nil
	}
	rel := cleanPath(path.Dir(wt.Path), path.Base(wt.Path))
	if rel == "" {
		return "", nil
	}
	dest := filepath.Join(outbox, "files", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return rel, err
	}
	// Fetched beside the outbox (a dot name it ignores), then moved in
	// whole, so it isn't stored half written.
	tmp := filepath.Join(filepath.Dir(dest), ".uploading-"+wt.ID)
	f, err := os.Create(tmp)
	if err != nil {
		return rel, err
	}
	err = files.Get(ctx, c, m, f, func(string, ...any) {})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return rel, err
	}
	if !wt.Replace {
		// Beside what's there; a file put where its stub is would be
		// stored as a new version of it.
		dest = freeName(dest)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return rel, err
	}
	return rel + " is in your files", nil
}

// freeName is p, or p with " (2)" and so on before its extension if p or
// its stub is taken.
func freeName(p string) string {
	taken := func(q string) bool {
		_, e1 := os.Stat(q)
		_, e2 := os.Stat(q + files.StubExt)
		return e1 == nil || e2 == nil
	}
	if !taken(p) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		q := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if !taken(q) {
			return q
		}
	}
}

func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}
