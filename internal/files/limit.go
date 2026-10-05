package files

import (
	"context"
	"sync"
)

// perPeer caps requests in flight to any one node. Servers turn away more
// than 8 at once as busy, and parallel chunks would otherwise get there.
const perPeer = 4

type peerLimit struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

func newPeerLimit() *peerLimit { return &peerLimit{slots: map[string]chan struct{}{}} }

func (l *peerLimit) acquire(ctx context.Context, addr string) error {
	l.mu.Lock()
	s, ok := l.slots[addr]
	if !ok {
		s = make(chan struct{}, perPeer)
		l.slots[addr] = s
	}
	l.mu.Unlock()
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *peerLimit) release(addr string) {
	l.mu.Lock()
	s := l.slots[addr]
	l.mu.Unlock()
	<-s
}
