package peers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Live is a node's allow-list, kept up to date while it runs. It follows two
// files: the one the node was started with, and the one an admin node last
// pushed (kept in the node's data folder). Whichever changed last wins, so a
// hand edit still overrides an older push.
type Live struct {
	base, pushed, self string

	mu      sync.RWMutex
	list    []Peer
	allowed map[string]bool
	admins  map[string]bool
	stamps  [2]time.Time
}

// NewLive loads the list. self is this node's own overlay IP, always allowed.
func NewLive(base, pushed, self string) (*Live, error) {
	l := &Live{base: base, pushed: pushed, self: self}
	if err := l.Refresh(); err != nil {
		return nil, err
	}
	return l, nil
}

func mtime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// Refresh reloads the list if either file changed. A broken file is
// reported and the list in use is kept.
func (l *Live) Refresh() error {
	stamps := [2]time.Time{mtime(l.base), mtime(l.pushed)}
	l.mu.RLock()
	same := stamps == l.stamps && l.list != nil
	l.mu.RUnlock()
	if same {
		return nil
	}
	// On a tie (same clock tick) the push wins: it was written on purpose
	// after the base file was read.
	path := l.base
	if !stamps[1].IsZero() && !stamps[1].Before(stamps[0]) {
		path = l.pushed
	}
	list, err := Load(path)
	if err != nil {
		return err
	}
	l.set(list, stamps)
	return nil
}

func (l *Live) set(list []Peer, stamps [2]time.Time) {
	allowed := AllowedIPs(list)
	allowed[l.self] = true // a node may always talk to itself
	admins := map[string]bool{}
	for _, p := range list {
		if p.Admin {
			admins[p.IP()] = true
		}
	}
	l.mu.Lock()
	l.list, l.allowed, l.admins, l.stamps = list, allowed, admins, stamps
	l.mu.Unlock()
}

// Watch refreshes the list every interval until ctx ends.
func (l *Live) Watch(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Refresh(); err != nil {
				logf("peer list not reloaded: %v", err)
			}
		}
	}
}

func (l *Live) Allowed(ip string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.allowed[ip]
}

func (l *Live) IsAdmin(ip string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.admins[ip]
}

func (l *Live) List() []Peer {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Peer(nil), l.list...)
}

func (l *Live) Hash() string { return Hash(l.List()) }

// Replace stores a list pushed by caller, who must be an admin now and stay
// one in the new list, so an admin cannot lock every admin out by mistake.
func (l *Live) Replace(caller string, list []Peer) error {
	if !l.IsAdmin(caller) {
		return ErrNotAdmin
	}
	if err := Validate(list); err != nil {
		return err
	}
	stillAdmin := false
	for _, p := range list {
		if p.Admin && p.IP() == caller {
			stillAdmin = true
		}
	}
	if !stillAdmin {
		return errors.New("the new list must keep the sender as an admin")
	}
	if err := Write(l.pushed, list); err != nil {
		return fmt.Errorf("save pushed list: %w", err)
	}
	l.set(list, [2]time.Time{mtime(l.base), mtime(l.pushed)})
	return nil
}

var ErrNotAdmin = errors.New("only an admin node may change the peer list")
