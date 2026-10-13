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
	stamps  [2]stamp
}

// stamp is what Refresh checks a file by: when it was last written, and
// its size.
type stamp struct {
	mod  time.Time
	size int64
}

// NewLive loads the list. self is this node's own overlay IP, always allowed.
func NewLive(base, pushed, self string) (*Live, error) {
	l := &Live{base: base, pushed: pushed, self: self}
	if err := l.Refresh(); err != nil {
		return nil, err
	}
	return l, nil
}

func stampOf(path string) stamp {
	if info, err := os.Stat(path); err == nil {
		return stamp{info.ModTime(), info.Size()}
	}
	return stamp{}
}

// racy is how recently a file may have been written for its stamp not to
// be trusted: a second write in the same clock tick leaves the time as it
// was, and may leave the size too.
const racy = 2 * time.Second

// Refresh reloads the list if either file changed. A broken file is
// reported and the list in use is kept.
func (l *Live) Refresh() error {
	stamps := [2]stamp{stampOf(l.base), stampOf(l.pushed)}
	l.mu.RLock()
	same := stamps == l.stamps && l.list != nil
	l.mu.RUnlock()
	for _, st := range stamps {
		if time.Since(st.mod) < racy {
			same = false
		}
	}
	if same {
		return nil
	}
	// On a tie (same clock tick) the push wins: it was written on purpose
	// after the base file was read.
	path := l.base
	if !stamps[1].mod.IsZero() && !stamps[1].mod.Before(stamps[0].mod) {
		path = l.pushed
	}
	list, err := Load(path)
	if err != nil {
		return err
	}
	l.set(list, stamps)
	return nil
}

func (l *Live) set(list []Peer, stamps [2]stamp) {
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

// IsGateway reports whether ip is a gateway peer's.
func (l *Live) IsGateway(ip string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, p := range l.list {
		if p.Gateway && p.IP() == ip {
			return true
		}
	}
	return false
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
	l.set(list, [2]stamp{stampOf(l.base), stampOf(l.pushed)})
	return nil
}

var ErrNotAdmin = errors.New("only an admin node may change the peer list")

// Follow copies the list an admin pushed to this machine's node over base,
// the list its dashboard, outbox and repair read, when the push is newer.
// It reports whether it copied. A member's programs then see nodes the
// admin added, or marked leaving, as the node does.
func Follow(base, pushed string) (bool, error) {
	pt := stampOf(pushed).mod
	if pt.IsZero() || !pt.After(stampOf(base).mod) {
		return false, nil
	}
	list, err := Load(pushed)
	if err != nil {
		return false, err
	}
	if old, err := Load(base); err == nil && Hash(old) == Hash(list) {
		return false, nil
	}
	return true, Write(base, list)
}
