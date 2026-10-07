// Package server is the per-node shard store over HTTP, adapted from
// PEER_TO_PEER's shardserver with Tailscale WhoIs replaced by overlay identity
// plus an allow-list.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/challenge"
	"github.com/peterretief/yggstore/internal/invite"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/mesh"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/transport"
)

const DefaultMaxShardBytes int64 = 64 << 20

type Info struct {
	Service    string `json:"service"`
	Name       string `json:"name"`
	NodeID     string `json:"node_id"`
	Transport  string `json:"transport"`
	UsedBytes  int64  `json:"used_bytes"`
	QuotaBytes int64  `json:"quota_bytes"`
	ShardCount int    `json:"shard_count"`
	StartedAt  int64  `json:"started_at"`           // unix seconds
	PeersHash  string `json:"peers_hash,omitempty"` // which peer list the node holds (see peers.Hash)
	// ByWriter is bytes stored per uploading node ID, for accounting. It is
	// recounted at most every 30 s.
	ByWriter map[string]int64 `json:"by_writer,omitempty"`
	// Customers is whether the node's owner lets it hold paying customers'
	// files, stored through a gateway.
	Customers bool `json:"customers,omitempty"`
	// Mesh is the node's Yggdrasil links to other members (see package mesh).
	Mesh *mesh.Status `json:"mesh,omitempty"`
	// Web is the sites this node serves, if it is a web node (see package
	// site).
	Web json.RawMessage `json:"web,omitempty"`
	// MailOut is whether the node sends members' email (see package mail).
	MailOut bool `json:"mail_out,omitempty"`
}

type Options struct {
	Name      string
	NodeID    string
	Transport transport.Transport
	Allowed   map[string]bool // node IDs allowed to call; unknown callers are denied
	Peers     *peers.Live     // if set, used instead of Allowed, and admins may replace it
	// Join, if set, answers join requests, the one call open to non-members
	// (it needs a valid invite). Only admin nodes set it.
	Join          func(caller string, req invite.Request) (invite.Response, error)
	MaxShardBytes int64
	MaxConcurrent int
	// Customers lets gateway peers store shards here (the owner's opt-in).
	Customers bool
	// Messages, if set, answers members' messaging calls (/v1/msg...).
	Messages interface {
		Serve(w http.ResponseWriter, r *http.Request, caller string) bool
	}
	// Mesh, if set, reports the node's Yggdrasil links in its info.
	Mesh func() mesh.Status
	// Web, if set, reports the sites the node serves in its info.
	Web func() json.RawMessage
	// MailOut, if set, sends members' email (/v1/mail/send).
	MailOut interface {
		Serve(w http.ResponseWriter, r *http.Request, caller string) bool
	}
}

func Handler(store localstore.Store, opts Options) http.Handler {
	if opts.MaxShardBytes <= 0 {
		opts.MaxShardBytes = DefaultMaxShardBytes
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 8
	}
	slots := make(chan struct{}, opts.MaxConcurrent)
	started := time.Now().Unix()
	joinLimit := newLimiter(6, time.Minute) // invites are 144-bit secrets; this just stops floods
	var msgMu sync.Mutex
	msgLimits := map[string]*limiter{} // per member, so one can't flood the others out
	var usageMu sync.Mutex
	var usage map[string]int64
	var usageAt time.Time
	byWriter := func() map[string]int64 {
		usageMu.Lock()
		defer usageMu.Unlock()
		if time.Since(usageAt) > 30*time.Second {
			if u, err := store.UsageByWriter(); err == nil {
				usage, usageAt = u, time.Now()
			}
		}
		return usage
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		caller, err := opts.Transport.Identify(r.RemoteAddr)
		if err != nil {
			http.Error(w, "overlay identity required", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1/join" && r.Method == http.MethodPost && opts.Join != nil {
			if !joinLimit.Allow() {
				w.Header().Set("Retry-After", "10")
				http.Error(w, "too many join attempts; try again shortly", http.StatusTooManyRequests)
				return
			}
			var req invite.Request
			if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil {
				http.Error(w, "bad join request", http.StatusBadRequest)
				return
			}
			resp, err := opts.Join(caller, req)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, invite.ErrBadToken) {
					status = http.StatusForbidden
				}
				http.Error(w, err.Error(), status)
				return
			}
			writeJSON(w, resp)
			return
		}
		allowed := opts.Allowed[caller]
		if opts.Peers != nil {
			allowed = opts.Peers.Allowed(caller)
		}
		if !allowed {
			http.Error(w, "node is not on the allow-list", http.StatusForbidden)
			return
		}

		if opts.Messages != nil && strings.HasPrefix(r.URL.Path, "/v1/msg") {
			msgMu.Lock()
			l := msgLimits[caller]
			if l == nil {
				l = newLimiter(1200, time.Minute)
				msgLimits[caller] = l
			}
			msgMu.Unlock()
			if !l.Allow() {
				w.Header().Set("Retry-After", "5")
				http.Error(w, "too many messages; slow down", http.StatusTooManyRequests)
				return
			}
			if opts.Messages.Serve(w, r, caller) {
				return
			}
		}
		if opts.MailOut != nil && opts.MailOut.Serve(w, r, caller) {
			return
		}
		switch {
		case r.URL.Path == "/v1/info" && r.Method == http.MethodGet:
			count, used, err := store.Stats()
			if err != nil {
				http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
				return
			}
			info := Info{Service: "yggstore", Name: opts.Name, NodeID: opts.NodeID,
				Transport: opts.Transport.Name(), UsedBytes: used, QuotaBytes: store.Quota(),
				ShardCount: count, StartedAt: started, Customers: opts.Customers}
			if opts.Peers != nil {
				info.PeersHash = opts.Peers.Hash()
			}
			info.ByWriter = byWriter()
			if opts.Web != nil {
				info.Web = opts.Web()
			}
			info.MailOut = opts.MailOut != nil
			if opts.Mesh != nil {
				st := opts.Mesh()
				info.Mesh = &st
			}
			writeJSON(w, info)
			return
		case r.URL.Path == "/v1/peers" && r.Method == http.MethodPut:
			if opts.Peers == nil {
				http.Error(w, "this node's peer list is fixed", http.StatusForbidden)
				return
			}
			var list []peers.Peer
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&list); err != nil {
				http.Error(w, "bad peer list", http.StatusBadRequest)
				return
			}
			if err := opts.Peers.Replace(caller, list); err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, peers.ErrNotAdmin) {
					status = http.StatusForbidden
				}
				http.Error(w, err.Error(), status)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case r.URL.Path == "/v1/challenge" && r.Method == http.MethodPost:
			var c challenge.Request
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&c); err != nil {
				http.Error(w, "bad challenge", http.StatusBadRequest)
				return
			}
			data, err := store.Get(c.Hash)
			if err != nil {
				storeError(w, err)
				return
			}
			proof, err := challenge.Answer(data, c)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, challenge.Response{Proof: proof})
			return
		}

		hash, ok := strings.CutPrefix(r.URL.Path, "/shards/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(hash) != 64 || strings.ContainsAny(hash, "/\\") {
			http.Error(w, "invalid shard hash", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodHead:
			ok, err := store.Has(hash)
			if err != nil {
				storeError(w, err)
				return
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, err := store.Get(hash)
			if err != nil {
				storeError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(data)
		case http.MethodPut:
			if !opts.Customers && opts.Peers != nil && opts.Peers.IsGateway(caller) {
				http.Error(w, "this node does not hold customer data", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, opts.MaxShardBytes)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "shard exceeds size limit", http.StatusRequestEntityTooLarge)
				} else {
					http.Error(w, "read body", http.StatusBadRequest)
				}
				return
			}
			if manifest.Hash(body) != hash {
				http.Error(w, "shard hash does not match request path", http.StatusBadRequest)
				return
			}
			if _, err := store.PutOwned(body, caller); err != nil {
				storeError(w, err)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			if err := store.DeleteOwned(hash, caller); err != nil {
				storeError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "HEAD, GET, PUT, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// limiter allows n events per window, across all callers.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	times  []time.Time
}

func newLimiter(n int, window time.Duration) *limiter { return &limiter{n: n, window: window} }

func (l *limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	keep := l.times[:0]
	for _, t := range l.times {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	l.times = keep
	if len(l.times) >= l.n {
		return false
	}
	l.times = append(l.times, now)
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func storeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, localstore.ErrInvalidHash):
		status = http.StatusBadRequest
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case errors.Is(err, localstore.ErrQuota):
		status = http.StatusInsufficientStorage
	case errors.Is(err, localstore.ErrNotOwner):
		status = http.StatusForbidden
	}
	http.Error(w, http.StatusText(status), status)
}
