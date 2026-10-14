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
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/recovery"
)

// Uploading: a file uploaded on the link page is stored in the group by
// the web node at once, and waits in the person's inbox there, its stub
// sealed with their recovery key. The place holding their working copy
// collects it: it fetches the file into the same folder of its outbox,
// where it is stored as its own (shards belong to the node that stores
// them), and then has the web node delete its copy.

// MaxUpload is the largest file the link page takes (Cloudflare's free
// plan stops bigger requests anyway).
const MaxUpload = 100 << 20

// Waiting is a file in a person's inbox.
type Waiting struct {
	ID     string `json:"id"`
	Path   string `json:"path"` // where it goes, under files/
	Sealed []byte `json:"sealed"`
	Sum    string `json:"sum"` // SHA-256 of the stub, so the holder can ask for its deletion
	At     int64  `json:"at"`
}

func (s *Store) inboxDir(id string) string { return filepath.Join(s.Dir, "inbox", id) }

func (s *Store) inbox(id string) []Waiting {
	ents, _ := os.ReadDir(s.inboxDir(id))
	var out []Waiting
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(s.inboxDir(id), e.Name()))
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
	b := make([]byte, 8)
	rand.Read(b)
	wt := Waiting{ID: hex.EncodeToString(b), Path: dest, Sealed: sealed, Sum: hex.EncodeToString(sum[:]), At: time.Now().Unix()}
	out, _ := json.Marshal(wt)
	if err := os.MkdirAll(s.inboxDir(id), 0o700); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := atomicfile.Replace(filepath.Join(s.inboxDir(id), wt.ID+".json"), out, 0o600); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logf("files: %s uploaded %d bytes; waiting for their working copy to take it", rec.Name, m.PlaintextSize)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": dest})
}

// serveInbox answers the places collecting: GET /v1/work/ID/inbox lists
// what waits, and DELETE /v1/work/ID/inbox/ENTRY (with the stub) deletes
// the web node's copy once the place has the file.
func (s *Store) serveInbox(w http.ResponseWriter, r *http.Request, id, entry string) {
	if !validID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && entry == "":
		w.Header().Set("Content-Type", "application/json")
		list := s.inbox(id)
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
		stub, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		sum := sha256.Sum256(stub)
		if err != nil || hex.EncodeToString(sum[:]) != wt.Sum {
			http.Error(w, "that isn't this file's stub", http.StatusForbidden)
			return
		}
		m, err := manifest.Unmarshal(stub)
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
		os.Remove(p)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Collect fetches the files waiting for a person on the web nodes into
// their outbox's files/ folder, where the outbox stores them as theirs,
// and then has each web node delete its copy. It returns how many came.
func Collect(ctx context.Context, c client.Client, nodes []string, ks recovery.Keys, filesDir string, logf func(string, ...any)) (int, error) {
	key, err := recovery.Key(ks.LinkHalf, ks.GroupHalf)
	if err != nil {
		return 0, err
	}
	got := 0
	var firstErr error
	for _, node := range nodes {
		var list []Waiting
		if err := getJSON(ctx, c.HTTP, "http://"+node+"/v1/work/"+ks.ID+"/inbox", &list); err != nil {
			continue // not a web node with this person's record, or down
		}
		for _, wt := range list {
			if err := collectOne(ctx, c, node, ks.ID, key, wt, filesDir); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", wt.Path, err)
				}
				continue
			}
			got++
			if logf != nil {
				logf("uploaded on the web: %s is in your files", wt.Path)
			}
		}
	}
	return got, firstErr
}

func collectOne(ctx context.Context, c client.Client, node, id string, key []byte, wt Waiting, filesDir string) error {
	stub, err := recovery.Open(key, wt.Sealed)
	if err != nil {
		return err
	}
	m, err := manifest.Unmarshal(stub)
	if err != nil {
		return err
	}
	rel := cleanPath(path.Dir(wt.Path), path.Base(wt.Path))
	if rel == "" {
		return errors.New("bad path")
	}
	dest := filepath.Join(filesDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// Fetched beside the outbox (a dot name it ignores), then moved in
	// whole, so it isn't stored half written.
	tmp := filepath.Join(filepath.Dir(dest), ".uploading-"+wt.ID)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = files.Get(ctx, c, m, f, func(string, ...any) {})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, freeName(dest)); err != nil {
		os.Remove(tmp)
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+node+"/v1/work/"+id+"/inbox/"+wt.ID, bytes.NewReader(stub))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil // it's in the outbox; the web node's copy goes next time
	}
	resp.Body.Close()
	return nil
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

// pending lists the files waiting, as the page shows them.
func (s *Store) pending(id string, key []byte) []string {
	var out []string
	for _, wt := range s.inbox(id) {
		if _, err := recovery.Open(key, wt.Sealed); err == nil {
			out = append(out, wt.Path)
		}
	}
	return out
}
