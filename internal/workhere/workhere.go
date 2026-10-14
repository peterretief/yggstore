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
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
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
	// Email, if set, sends a message from the group (this node holds the
	// mail relay): the link, for people whose box asks.
	Email func(ctx context.Context, to, subject, text string) error
	// Shards fetches files from the group, for browsing (see browse.go).
	Shards client.Client
	// Online lists the nodes that answer, for storing uploads.
	Online func(context.Context) ([]peers.Peer, error)
	// Others lists the group's other web nodes (addresses), whose
	// waiting changes the page shows too.
	Others func(context.Context) []string

	cache  viewCache
	others otherCache
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
	if id, ok := strings.CutSuffix(id, "/email"); ok {
		s.email(w, r, id)
		return true
	}
	if pid, rest, ok := strings.Cut(id, "/inbox"); ok {
		s.serveInbox(w, r, pid, strings.TrimPrefix(rest, "/"))
		return true
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

// EmailRequest asks a web node with the mail relay to email a person
// their link: a box sends it, since a box has no address to send from.
type EmailRequest struct {
	To   string `json:"to"`
	Link string `json:"link"` // the whole link, with the key's half after the #
}

// email sends a person their link. The link must open their record, so
// only someone holding it can have it sent anywhere.
func (s *Store) email(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost || s.Email == nil {
		http.Error(w, "this node sends no email", http.StatusNotFound)
		return
	}
	var req EmailRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rec, err := s.get(id)
	if err != nil {
		http.Error(w, "no record "+id+" here yet", http.StatusNotFound)
		return
	}
	_, frag, _ := strings.Cut(req.Link, "#")
	fid, half, _ := strings.Cut(frag, ".")
	key, err := recovery.Key(half, rec.GroupHalf)
	if fid != id || err != nil || !strings.Contains(req.Link, PagePath+"#") {
		http.Error(w, "that isn't this record's link", http.StatusBadRequest)
		return
	}
	if _, err := recovery.Open(key, rec.Sealed); err != nil {
		http.Error(w, "that link doesn't open this record", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.Email(ctx, req.To, LinkSubject, LinkText(req.Link)); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.logf("work here: emailed %s their link", rec.Name)
	w.WriteHeader(http.StatusNoContent)
}

// LinkSubject and LinkText are the email that carries a person's link.
const LinkSubject = "Your link for working from anywhere"

func LinkText(link string) string {
	return `This is your yggstore link for working from anywhere. Keep this email.

When you want to work remotely, or can't get to your computer, open the link
on the device you want to work on. It asks for that device's Syncthing ID,
and your files then come to it through your group's box:

` + link + `

Only one device has your files at a time: opening the link elsewhere moves
them there. Anyone who can read this email can open your files, so keep it
somewhere safe. If that worries you, turn "Work from anywhere" off.
`
}

// SendEmail asks a web node to email a person their link.
func SendEmail(ctx context.Context, c *http.Client, node, id string, req EmailRequest) error {
	b, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+node+"/v1/work/"+id+"/email", bytes.NewReader(b))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(hr)
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
		switch op := r.URL.Query().Get("op"); op {
		case "list", "get":
			s.browse(w, r, op)
		case "put":
			s.upload(w, r)
		case "change":
			s.change(w, r)
		default:
			s.open(w, r)
		}
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
		jsonError(w, http.StatusConflict, "No box is set for you to work through. On your dashboard, choose one under \"Work from anywhere\".")
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
