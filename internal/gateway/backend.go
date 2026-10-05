package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

// Backend is where the gateway keeps objects' contents.
type Backend interface {
	// Put stores r, encrypted with key, and returns its manifest.
	Put(ctx context.Context, r io.Reader, key []byte) (manifest.Manifest, error)
	Get(ctx context.Context, m manifest.Manifest, w io.Writer, offset, length int64) error
	Delete(ctx context.Context, m manifest.Manifest) error
}

// Group stores objects in the group, on the nodes whose owners opted in to
// holding customer data.
type Group struct {
	Client client.Client
	Peers  func() []peers.Peer
	Log    func(format string, args ...any)
	// MinMachines overrides how many opted-in machines an upload needs
	// (default 3). Lower only for testing: fewer can't survive a failure.
	MinMachines int

	mu     sync.Mutex
	online []peers.Peer
	at     time.Time
}

// ErrTooFewNodes means not enough opted-in machines answer to store safely.
var ErrTooFewNodes = errors.New("too few storage nodes are taking customer data right now")

// minMachines is how many separate machines an upload needs: with at most
// 2 of a chunk's 6 shards on each, any one can fail.
const minMachines = 3

func (g *Group) targets(ctx context.Context) ([]peers.Peer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.at) > 30*time.Second {
		var opted []peers.Peer
		var mu sync.Mutex
		peers.Online(ctx, g.Peers(), func(ctx context.Context, p peers.Peer) error {
			info, err := g.Client.Info(ctx, p.Addr)
			if err == nil && info.Customers {
				mu.Lock()
				opted = append(opted, p)
				mu.Unlock()
			}
			return err
		})
		// Keep the peer list's order, so placement rotates the same way.
		var ordered []peers.Peer
		for _, p := range g.Peers() {
			for _, o := range opted {
				if o.Addr == p.Addr {
					ordered = append(ordered, p)
				}
			}
		}
		g.online, g.at = ordered, time.Now()
	}
	need := minMachines
	if g.MinMachines > 0 {
		need = g.MinMachines
	}
	if n := peers.Machines(g.online); n < need {
		return nil, fmt.Errorf("%w (%d machines, need %d)", ErrTooFewNodes, n, need)
	}
	return g.online, nil
}

func (g *Group) Put(ctx context.Context, r io.Reader, key []byte) (manifest.Manifest, error) {
	online, err := g.targets(ctx)
	if err != nil {
		return manifest.Manifest{}, err
	}
	m, _, _, err := files.PutReader(ctx, g.Client, r, "s3-object", online, files.PutOptions{Key: key, Log: g.Log})
	if err != nil {
		g.mu.Lock()
		g.at = time.Time{} // look again next time; a node may have gone
		g.mu.Unlock()
	}
	return m, err
}

func (g *Group) Get(ctx context.Context, m manifest.Manifest, w io.Writer, offset, length int64) error {
	return files.GetRange(ctx, g.Client, m, w, offset, length, nil)
}

func (g *Group) Delete(ctx context.Context, m manifest.Manifest) error {
	return files.Delete(ctx, g.Client, m).Err()
}
