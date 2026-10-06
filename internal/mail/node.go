package mail

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
)

const (
	// MaxSize is the largest sealed message taken in. Cloudflare passes
	// messages of up to 25 MiB.
	MaxSize = 32 << 20
	// IngestPath is where the Worker posts messages on a web node.
	IngestPath = "/_yggstore/mail"

	typeMail = "mail"     // web node → recipient: a message is stored for you
	typeGot  = "mail-got" // recipient → web node: I have my own copy now
)

// Messages is the node's messaging, as mail uses it.
type Messages interface {
	Send(to, typ, body string) (msg.Message, error)
	Wait(ctx context.Context, after int64, topic string) []msg.Stored
}

// Node is a node's part in the group's mail: on a web node it takes
// messages from the Worker (Token set), and on anyone's node it collects the
// messages sent to its owner into Box.
type Node struct {
	Dir    string // the node's mail state
	Box    string // the owner's mailbox; "" if this node collects no mail
	Token  string // the Worker's token; "" if this node takes no mail in
	Self   string // this node's ID
	Msgs   Messages
	Peers  func() []peers.Peer
	Client client.Client
	Log    func(string, ...any)

	mu   sync.Mutex
	st   state
	wake chan struct{}
}

type state struct {
	After int64 `json:"after"` // last message handled
	// Taken are messages this web node stored and handed on, kept until
	// the recipient has its own copy.
	Taken map[string]*taken `json:"taken,omitempty"`
	// Todo are messages for this node's owner not collected yet.
	Todo map[string]*notice `json:"todo,omitempty"`
}

type taken struct {
	Node  string            `json:"node"`
	Time  int64             `json:"time"`
	Stub  manifest.Manifest `json:"stub"`
	Error string            `json:"error,omitempty"`
}

// notice is what a web node sends the recipient.
type notice struct {
	ID       string          `json:"id"`
	Received int64           `json:"received"` // unix ms, when the web node took it
	Stub     json.RawMessage `json:"stub"`
	From     string          `json:"from,omitempty"` // the web node, filled in by the recipient
	Error    string          `json:"error,omitempty"`
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (n *Node) logf(format string, args ...any) {
	if n.Log != nil {
		n.Log(format, args...)
	}
}

func (n *Node) statePath() string { return filepath.Join(n.Dir, "state.json") }

func (n *Node) load() error {
	if err := os.MkdirAll(n.Dir, 0o700); err != nil {
		return err
	}
	n.st = state{}
	if b, err := os.ReadFile(n.statePath()); err == nil {
		if err := json.Unmarshal(b, &n.st); err != nil {
			return fmt.Errorf("%s: %w", n.statePath(), err)
		}
	}
	if n.st.Taken == nil {
		n.st.Taken = map[string]*taken{}
	}
	if n.st.Todo == nil {
		n.st.Todo = map[string]*notice{}
	}
	n.wake = make(chan struct{}, 1)
	return nil
}

// save writes the state. The caller holds n.mu.
func (n *Node) save() {
	b, _ := json.MarshalIndent(n.st, "", "  ")
	if err := atomicfile.Replace(n.statePath(), append(b, '\n'), 0o600); err != nil {
		n.logf("mail: %v", err)
	}
}

func (n *Node) poke() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Start loads the state; call it before serving IngestPath or Run.
func (n *Node) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.load()
}

// Run handles mail notices until ctx ends, retrying what failed every
// minute.
func (n *Node) Run(ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			n.mu.Lock()
			after := n.st.After
			n.mu.Unlock()
			got := n.Msgs.Wait(ctx, after, "direct")
			n.mu.Lock()
			for _, s := range got {
				n.st.After = max(n.st.After, s.N)
				n.handle(s)
			}
			n.save()
			n.mu.Unlock()
			n.poke()
		}
	}()
	for {
		n.work(ctx)
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
		case <-time.After(time.Minute):
		}
	}
}

// handle takes one direct message. The caller holds n.mu.
func (n *Node) handle(s msg.Stored) {
	switch s.Type {
	case typeMail:
		if n.Box == "" {
			return
		}
		var no notice
		if json.Unmarshal([]byte(s.Body), &no) != nil || !validID.MatchString(no.ID) {
			n.logf("mail: ignoring a bad notice from %s", s.From)
			return
		}
		if _, err := os.Stat(n.boxPath(no.ID, ".ystub")); err == nil {
			n.st.Todo[no.ID] = &notice{ID: no.ID, From: s.From, Error: "done"} // collected before; just confirm
			return
		}
		no.From, no.Error = s.From, ""
		n.st.Todo[no.ID] = &no
	case typeGot:
		if t := n.st.Taken[s.Body]; t != nil && t.Node == s.From {
			t.Error = "collected" // delete our copy (in work)
		}
	}
}

func (n *Node) boxPath(id, ext string) string { return filepath.Join(n.Box, id+ext) }

// work collects pending messages and deletes collected ones.
func (n *Node) work(ctx context.Context) {
	n.mu.Lock()
	var todo []notice
	for _, no := range n.st.Todo {
		todo = append(todo, *no)
	}
	n.mu.Unlock()

	for _, no := range todo {
		err := error(nil)
		if no.Error != "done" {
			err = n.collect(ctx, no)
		}
		if err == nil {
			err = n.confirm(ctx, no)
		}
		n.mu.Lock()
		if err != nil {
			if cur := n.st.Todo[no.ID]; cur != nil && cur.Error != err.Error() {
				cur.Error = err.Error()
				n.logf("mail: collecting %s: %v (will retry)", no.ID, err)
			}
		} else {
			delete(n.st.Todo, no.ID)
			n.logf("mail: collected %s", no.ID)
		}
		n.save()
		n.mu.Unlock()
	}
	// Collected copies go after collecting, so a message this node both
	// took and collected is cleaned up in the same pass.
	n.mu.Lock()
	var done []string
	for id, t := range n.st.Taken {
		if t.Error == "collected" {
			done = append(done, id)
		}
	}
	n.mu.Unlock()
	for _, id := range done {
		n.mu.Lock()
		t := n.st.Taken[id]
		n.mu.Unlock()
		if t == nil {
			continue
		}
		if res := files.Delete(ctx, n.Client, t.Stub); res.Err() != nil {
			n.logf("mail: deleting the web node's copy of %s: %v (will retry)", id, res.Err())
			continue
		}
		n.mu.Lock()
		delete(n.st.Taken, id)
		n.save()
		n.mu.Unlock()
	}
}

// confirm tells the web node it may delete its copy.
func (n *Node) confirm(ctx context.Context, no notice) error {
	if no.From == n.Self {
		n.mu.Lock()
		if t := n.st.Taken[no.ID]; t != nil {
			t.Error = "collected"
		}
		n.mu.Unlock()
		return nil
	}
	_, err := n.Msgs.Send(no.From, typeGot, no.ID)
	return err
}

// collect fetches a message, keeps it in the mailbox and stores a copy of
// its own in the group.
func (n *Node) collect(ctx context.Context, no notice) error {
	m, err := manifest.Unmarshal(no.Stub)
	if err != nil {
		return fmt.Errorf("bad stub: %w", err)
	}
	if m.PlaintextSize > MaxSize || !n.atMembers(m) {
		return errors.New("stub is too large or points outside the group")
	}
	sealed := n.boxPath(no.ID, ".sealed")
	data, err := os.ReadFile(sealed)
	if err != nil {
		var buf bytes.Buffer
		if err := files.Get(ctx, n.Client, m, &buf, func(string, ...any) {}); err != nil {
			return err
		}
		data = buf.Bytes()
		if !IsSealed(data) {
			return errors.New("not a sealed message")
		}
		if err := os.MkdirAll(n.Box, 0o700); err != nil {
			return err
		}
		meta, _ := json.Marshal(Meta{Received: no.Received})
		if err := atomicfile.Replace(n.boxPath(no.ID, ".json"), meta, 0o600); err != nil {
			return err
		}
		if err := atomicfile.Replace(sealed, data, 0o600); err != nil {
			return err
		}
	}
	online := n.online(ctx)
	own, _, _, err := files.PutReader(ctx, n.Client, bytes.NewReader(data), "mail-"+no.ID, online, files.PutOptions{})
	if err != nil {
		return fmt.Errorf("storing a copy: %w", err)
	}
	return files.WriteJSON(n.boxPath(no.ID, ".ystub"), own)
}

// Meta is what the mailbox keeps beside a message.
type Meta struct {
	Received int64 `json:"received"` // unix ms
}

func (n *Node) atMembers(m manifest.Manifest) bool {
	known := map[string]bool{}
	for _, p := range n.Peers() {
		known[p.Addr] = true
	}
	for _, ch := range m.Chunks {
		for _, ref := range ch.Shards {
			if !peers.IsOverlay(ref.Peer) && !known[ref.Peer] {
				return false
			}
		}
	}
	return true
}

func (n *Node) online(ctx context.Context) []peers.Peer {
	var list []peers.Peer
	for _, p := range n.Peers() {
		if !p.Gateway {
			list = append(list, p)
		}
	}
	return peers.Online(ctx, list, func(ctx context.Context, p peers.Peer) error {
		_, err := n.Client.Info(ctx, p.Addr)
		return err
	})
}

// ServeHTTP takes a sealed message from the Worker: POST IngestPath?node=ID
// with the token. It answers only once the message is stored in the group
// and the recipient's notice is queued, so the Worker can tell the sender
// to retry if anything fails.
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if n.Token == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(n.Token)) != 1 {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}
	node := r.URL.Query().Get("node")
	member := false
	for _, p := range n.Peers() {
		if p.IP() == node && !p.Gateway {
			member = true
		}
	}
	if !member {
		http.Error(w, "no member node "+node, http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxSize))
	if err != nil {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !IsSealed(data) {
		http.Error(w, "not a sealed message", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	m, _, _, err := files.PutReader(ctx, n.Client, bytes.NewReader(data), "mail", n.online(ctx), files.PutOptions{})
	if err != nil {
		n.logf("mail: storing a message for %s: %v", node, err)
		http.Error(w, "can't store the message now", http.StatusServiceUnavailable)
		return
	}
	stub, _ := manifest.Marshal(m)
	no := notice{ID: m.FileID, Received: time.Now().UnixMilli(), Stub: stub}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.st.Taken[no.ID] = &taken{Node: node, Time: no.Received, Stub: m}
	if node == n.Self {
		if n.Box != "" {
			no.From = n.Self
			n.st.Todo[no.ID] = &no
		}
	} else {
		body, _ := json.Marshal(no)
		if _, err := n.Msgs.Send(node, typeMail, string(body)); err != nil {
			delete(n.st.Taken, no.ID)
			files.Delete(ctx, n.Client, m)
			n.logf("mail: telling %s: %v", node, err)
			http.Error(w, "can't reach the recipient's node now", http.StatusServiceUnavailable)
			return
		}
	}
	n.save()
	n.logf("mail: took %s for %s (%d bytes)", no.ID, node, len(data))
	n.poke()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": no.ID})
}
