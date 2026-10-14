package workhere

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"regexp"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/recovery"
)

// A file bigger than one request may carry is uploaded in parts. Each is
// stored in the group as it comes, by whichever web node it reaches, with
// a key made from the person's and the upload's, and the page is given it
// sealed. Once all are in, the page sends them back and they're joined
// into one stored file (files.Concat), which waits in the inbox like any
// other. A web node keeps nothing between the requests, so it doesn't
// matter which ones reach it.

// PartSize is how much of a file the page sends at once.
const PartSize = 32 << 20

// MaxParts is the most parts a file is uploaded in (64 GB).
const MaxParts = 2048

var validUpload = regexp.MustCompile(`^[0-9a-f]{32}$`)

// partKey is the key an upload's parts are stored with. Every web node
// makes the same one, and only with the person's link.
func partKey(key []byte, upload string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("yggstore link-page upload " + upload))
	return h.Sum(nil)
}

// sendPart gives the page a part it stored, sealed.
func (s *Store) sendPart(w http.ResponseWriter, key []byte, m manifest.Manifest) {
	stub, _ := manifest.Marshal(m)
	sealed, err := recovery.Seal(key, stub)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]byte{"part": sealed})
}

type partsRequest struct {
	ID, Link, Dir, Name string
	Upload              string
	Replace             bool
	Parts               [][]byte
}

// openParts opens the parts the page sent back: each must be one this
// upload stored.
func openParts(key []byte, req partsRequest) ([]manifest.Manifest, bool) {
	if !validUpload.MatchString(req.Upload) || len(req.Parts) == 0 || len(req.Parts) > MaxParts {
		return nil, false
	}
	want := partKey(key, req.Upload)
	var out []manifest.Manifest
	for _, p := range req.Parts {
		stub, err := recovery.Open(key, p)
		if err != nil {
			return nil, false
		}
		m, err := manifest.Unmarshal(stub)
		if err != nil || !bytes.Equal(m.Key, want) {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// parts joins an upload's parts into one file in the person's inbox
// (?op=parts), or deletes them when the page gives up (?op=parts-drop).
func (s *Store) parts(w http.ResponseWriter, r *http.Request, drop bool) {
	var req partsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, MaxParts*64<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	dest := cleanPath(req.Dir, req.Name)
	if dest == "" && !drop {
		jsonError(w, http.StatusBadRequest, "That file name can't be used here.")
		return
	}
	rec, key, ok := s.uploader(w, req.ID, req.Link)
	if !ok {
		return
	}
	parts, ok := openParts(key, req)
	if !ok {
		jsonError(w, http.StatusBadRequest, "Those aren't this upload's parts.")
		return
	}
	if drop {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		for _, m := range parts {
			if res := files.Delete(ctx, s.shards(), m); res.Err() != nil {
				s.logf("files: deleting an unfinished upload's part: %v", res.Err())
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}
	m, err := files.Concat(path.Base(dest), parts)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.queueUpload(w, rec, key, m, dest, req.Replace)
}
