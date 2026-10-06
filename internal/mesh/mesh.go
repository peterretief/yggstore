package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/peers"
)

// Status is what a node reports about its links to the other members.
type Status struct {
	// On is whether the node can manage its Yggdrasil links. If not, Error
	// says why (usually no access to the admin socket).
	On    bool   `json:"on"`
	Error string `json:"error,omitempty"`
	Key   string `json:"key,omitempty"` // this machine's Yggdrasil public key
	// Direct names the members this machine has a Yggdrasil link to.
	Direct []string `json:"direct,omitempty"`
	// Trying names members whose ygg_listen links are not up, with the
	// latest error for each.
	Trying map[string]string `json:"trying,omitempty"`
}

// Yggdrasil is what the mesh needs from this machine's Yggdrasil: the
// daemon's admin socket (Admin) or the node's built-in one (Builtin).
type Yggdrasil interface {
	Self(ctx context.Context) (Self, error)
	Links(ctx context.Context) ([]Link, error)
	AddLink(ctx context.Context, uri string) (added bool, err error)
	RemoveLink(ctx context.Context, uri string) error
}

// Mesh opens Yggdrasil links to the other members.
type Mesh struct {
	admin Yggdrasil
	self  string // this node's address
	list  func() []peers.Peer
	state string // file of links this node added, so it only removes its own
	logf  func(string, ...any)

	mu     sync.Mutex
	status Status
	logged string // last error logged, so a lasting one is logged once
}

func New(admin Yggdrasil, self string, list func() []peers.Peer, statePath string, logf func(string, ...any)) *Mesh {
	return &Mesh{admin: admin, self: self, list: list, state: statePath, logf: logf}
}

// Status is the latest result.
func (m *Mesh) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	return s
}

// Run checks the links now and then every interval.
func (m *Mesh) Run(ctx context.Context, interval time.Duration) {
	for {
		m.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Sync opens links to members this machine isn't linked to, and closes
// links it opened earlier to addresses no longer listed.
func (m *Mesh) Sync(ctx context.Context) {
	st, err := m.sync(ctx)
	if err != nil {
		st = Status{Error: err.Error()}
		if err.Error() != m.logged {
			m.logf("mesh: %v", err)
		}
		m.logged = err.Error()
	} else {
		m.logged = ""
	}
	m.mu.Lock()
	m.status = st
	m.mu.Unlock()
}

func (m *Mesh) sync(ctx context.Context) (Status, error) {
	self, err := m.admin.Self(ctx)
	if err != nil {
		return Status{}, err
	}
	links, err := m.admin.Links(ctx)
	if err != nil {
		return Status{}, err
	}
	up := linkedTo(links)
	others := []peers.Peer{}
	for _, p := range m.list() {
		if ip := normal(p.IP()); ip != normal(m.self) && ip != normal(self.Address) {
			others = append(others, p)
		}
	}

	added := m.loadAdded()
	want := map[string]bool{}
	failed := map[string]string{} // member → why a link couldn't be added
	opened, closed := false, false
	for _, p := range others {
		for _, uri := range p.YggListen {
			want[uri] = true
			if up[normal(p.IP())] {
				continue // linked already, by this or another way
			}
			isNew, err := m.admin.AddLink(ctx, uri)
			switch {
			case err != nil:
				failed[p.Name] = err.Error()
			case isNew:
				added[uri], opened = true, true
				m.logf("mesh: linking to %s at %s", p.Name, uri)
			}
		}
	}
	for uri := range added {
		if !want[uri] {
			if err := m.admin.RemoveLink(ctx, uri); err != nil {
				m.logf("mesh: could not close the link to %s: %v", uri, err)
				continue
			}
			m.logf("mesh: closed the link to %s, which is no longer listed", uri)
			delete(added, uri)
			closed = true
		}
	}
	m.saveAdded(added)

	if opened || closed {
		// New links usually come up within a second or two; report them.
		if opened {
			select {
			case <-ctx.Done():
			case <-time.After(linkWait):
			}
		}
		if l, err := m.admin.Links(ctx); err == nil {
			links, up = l, linkedTo(l)
		}
	}
	byURI := map[string]Link{}
	for _, l := range links {
		byURI[l.URI] = l
	}
	st := Status{On: true, Key: self.Key, Trying: map[string]string{}}
	for _, p := range others {
		if up[normal(p.IP())] {
			st.Direct = append(st.Direct, p.Name)
			continue
		}
		for _, uri := range p.YggListen {
			why := "connecting"
			if l, ok := byURI[uri]; ok && l.LastError != "" {
				why = l.LastError
			}
			if f, ok := failed[p.Name]; ok {
				why = f
			}
			st.Trying[p.Name] = why
		}
	}
	sort.Strings(st.Direct)
	if len(st.Trying) == 0 {
		st.Trying = nil
	}
	return st, nil
}

// linkWait is how long to wait for new links before reporting them.
var linkWait = 3 * time.Second

// linkedTo is the set of addresses with a link up.
func linkedTo(links []Link) map[string]bool {
	up := map[string]bool{}
	for _, l := range links {
		if l.Up {
			up[normal(l.Address)] = true
		}
	}
	return up
}

func normal(addr string) string {
	if ip := net.ParseIP(addr); ip != nil {
		return ip.String()
	}
	return addr
}

func (m *Mesh) loadAdded() map[string]bool {
	added := map[string]bool{}
	if b, err := os.ReadFile(m.state); err == nil {
		var list []string
		if json.Unmarshal(b, &list) == nil {
			for _, u := range list {
				added[u] = true
			}
		}
	}
	return added
}

func (m *Mesh) saveAdded(added map[string]bool) {
	list := make([]string, 0, len(added))
	for u := range added {
		list = append(list, u)
	}
	sort.Strings(list)
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := atomicfile.Replace(m.state, append(b, '\n'), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logf("mesh: %v", err)
	}
}
