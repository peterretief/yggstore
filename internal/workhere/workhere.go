// Package workhere is the web nodes' side of a person's recovery link.
//
// A person's dashboard keeps a sealed record on every web node: the stub
// of their latest recovery copy, sealed with a key whose other half is in
// the link emailed to them, the web node's half of that key, and the box
// they work through when away. Opening the link shows a page that asks for
// the device's Syncthing ID; the web node opens the record with the link's
// half and asks the box (over Yggdrasil) to unpack the copy into a folder
// for the person and pair that device with it.
package workhere

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/recovery"
)

// Record is what a web node keeps for a person.
type Record struct {
	ID        string `json:"id"`
	GroupHalf string `json:"group_half"`
	Sealed    []byte `json:"sealed"` // the copy's stub, sealed with the recovery key
	Box       string `json:"box"`    // the box's node address, as in the peer list
	Name      string `json:"name"`   // what the box calls the person
	Updated   int64  `json:"updated"`
}

// PagePath is where the link opens, on any site the web node serves.
const PagePath = "/_yggstore/work"

// HubPort is where a box's hub answers.
const HubPort = "7490"

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// Store keeps the records on a web node.
type Store struct {
	Dir    string
	Client *http.Client // reaches boxes over Yggdrasil
	Log    func(format string, args ...any)
	// BoxURL is where a box's hub answers, from its node address (tests
	// change it); by default the same host on HubPort.
	BoxURL func(node string) string
}

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

func (s *Store) get(id string) (Record, error) {
	var rec Record
	if !validID.MatchString(id) {
		return rec, os.ErrNotExist
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(b, &rec)
}

// Serve answers members' PUT /v1/work/ID on the node's API: a dashboard
// keeping its person's record up to date.
func (s *Store) Serve(w http.ResponseWriter, r *http.Request, caller string) bool {
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/work/")
	if !ok {
		return false
	}
	if r.Method != http.MethodPut {
		http.Error(w, "PUT a record", http.StatusMethodNotAllowed)
		return true
	}
	var rec Record
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&rec); err != nil || rec.ID != id || !validID.MatchString(id) {
		http.Error(w, "bad record", http.StatusBadRequest)
		return true
	}
	rec.Updated = time.Now().Unix()
	b, _ := json.Marshal(rec)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	if err := atomicfile.Replace(s.path(id), b, 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

//go:embed page.html
var page []byte

// openRequest is what the page sends.
type openRequest struct {
	ID     string `json:"id"`
	Link   string `json:"link"` // the link's half of the key
	Device string `json:"device"`
	Move   bool   `json:"move,omitempty"`
}

// ServeHTTP is the page and what it asks, on PagePath.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Write(page)
	case http.MethodPost:
		s.open(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Store) open(w http.ResponseWriter, r *http.Request) {
	var req openRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	rec, err := s.get(req.ID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "This link isn't known here. It may be from before the newest email: use the link in that one.")
		return
	}
	key, err := recovery.Key(req.Link, rec.GroupHalf)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "This link is incomplete: open it from the email again, all of it.")
		return
	}
	stub, err := recovery.Open(key, rec.Sealed)
	if err != nil {
		jsonError(w, http.StatusForbidden, "This link doesn't open your copy: use the link in your newest recovery email.")
		return
	}
	if rec.Box == "" {
		jsonError(w, http.StatusConflict, "No box is set for you to work through. On your dashboard, choose one under \"If you lose this computer\".")
		return
	}
	body, _ := json.Marshal(map[string]any{"id": rec.ID, "name": rec.Name, "stub": json.RawMessage(stub), "device": req.Device, "move": req.Move})
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	url := ""
	if s.BoxURL != nil {
		url = s.BoxURL(rec.Box)
	} else if host, _, err := net.SplitHostPort(rec.Box); err == nil {
		url = "http://" + net.JoinHostPort(host, HubPort)
	} else {
		jsonError(w, http.StatusInternalServerError, "the box's address is wrong: "+rec.Box)
		return
	}
	hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/work", bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := c.Do(hr)
	if err != nil {
		s.logf("work here: %s: the box %s: %v", rec.Name, rec.Box, err)
		jsonError(w, http.StatusBadGateway, "Your box doesn't answer right now. Try again in a few minutes.")
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	if resp.StatusCode == http.StatusOK {
		s.logf("work here: %s opened their files through %s", rec.Name, rec.Box)
	}
}

func (s *Store) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Put sends a record to a web node (from a dashboard, over Yggdrasil).
func Put(ctx context.Context, c *http.Client, node string, rec Record) error {
	b, _ := json.Marshal(rec)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+node+"/v1/work/"+rec.ID, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ErrNoWebNode means no web node took the record.
var ErrNoWebNode = errors.New("no web node took the recovery record")
