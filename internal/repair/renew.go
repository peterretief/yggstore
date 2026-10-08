package repair

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/lease"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

// RenewEvery is how often each node is told which of its leases are still
// wanted (see package lease).
const RenewEvery = 24 * time.Hour

// renewBatch is the most tokens sent in one call.
const renewBatch = 5000

// renew tells each node that is up, once every RenewEvery, the lease
// tokens of the stubs here with shards on it: the leases still wanted.
// Shares received count too, since their stubs open the files as well as
// the sender's does. A node told nothing still learns this machine is
// about, which keeps its shards from before leases wanted.
func (r *Repairer) renew(ctx context.Context, online []peers.Peer, st *state, now time.Time, rep *Report) {
	var due []peers.Peer
	for _, p := range online {
		if now.Sub(time.Unix(st.Renewed[p.Addr], 0)) >= RenewEvery {
			due = append(due, p)
		}
	}
	if len(due) == 0 {
		return
	}
	tokens := map[string]map[string]bool{} // node address -> tokens
	stubs := r.Stubs()
	if r.Received != nil {
		stubs = append(stubs, r.Received()...)
	}
	for _, path := range stubs {
		m, err := files.ReadStub(path)
		if err != nil {
			continue
		}
		t := lease.Token(m.Key)
		add := func(refs []manifest.ShardRef) {
			for _, ref := range refs {
				if tokens[ref.Peer] == nil {
					tokens[ref.Peer] = map[string]bool{}
				}
				tokens[ref.Peer][t] = true
			}
		}
		add(m.Shards)
		for _, ch := range m.Chunks {
			add(ch.Shards)
		}
	}
	for _, p := range due {
		var list []string
		for t := range tokens[p.Addr] {
			list = append(list, t)
		}
		slices.Sort(list)
		var err error
		for start := 0; start == 0 || start < len(list); start += renewBatch {
			if _, err = r.Client.Renew(ctx, p.Addr, list[start:min(start+renewBatch, len(list))]); err != nil {
				break
			}
		}
		switch {
		case errors.Is(err, client.ErrNoLeases): // an older node: nothing to renew there; ask again tomorrow
		case err != nil:
			rep.Problems = append(rep.Problems, fmt.Sprintf("renewing leases on %s: %v", p.Name, err))
			continue
		default:
			rep.Renewed++
		}
		st.Renewed[p.Addr] = now.Unix()
	}
}
