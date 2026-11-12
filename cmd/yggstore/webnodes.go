package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

// knownWebNodes says whether a member serves the group's websites. A member
// is asked (its info says so), and what it said is remembered, across
// restarts too: a web node that can't be asked just now, as when the
// group's nodes have all just restarted, is still taken for one. Without
// that, sites' local apps and the group login answered "only the group's
// web nodes" for minutes after a deploy (2026-11-12).
type knownWebNodes struct {
	Path      string        // where what they said is kept
	PeersPath string        // the member list: only members are asked
	Keep      time.Duration // how long a web node that can't be asked stays one
	// Ask says whether the node at addr serves websites; an error means
	// it couldn't be asked.
	Ask func(ctx context.Context, addr string) (bool, error)
	Log func(format string, args ...any)

	mu     sync.Mutex
	loaded bool
	said   map[string]int64 // node ID to when it last said it's a web node, unix
	warned map[string]bool
}

func newKnownWebNodes(path, peersPath string) *knownWebNodes {
	c := client.New()
	return &knownWebNodes{Path: path, PeersPath: peersPath, Keep: 7 * 24 * time.Hour, Log: log.Printf,
		Ask: func(ctx context.Context, addr string) (bool, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			info, err := c.Info(ctx, addr)
			return err == nil && len(info.Web) > 0, err
		}}
}

// Is says whether the member with this node ID is a web node.
func (k *knownWebNodes) Is(ctx context.Context, id string) bool {
	list, err := peers.Load(k.PeersPath)
	if err != nil {
		return false
	}
	addr := ""
	for _, p := range list {
		if p.IP() == id {
			addr = p.Addr
			break
		}
	}
	if addr == "" {
		return false // not a member
	}
	web, err := k.Ask(ctx, addr)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.load()
	now := time.Now()
	if err != nil {
		last := time.Unix(k.said[id], 0)
		was := k.said[id] != 0 && now.Sub(last) < k.Keep
		if !k.warned[id] && k.Log != nil {
			if was {
				k.Log("web nodes: %s can't be asked just now (%v): it was one at %s, so it still is", addr, err, last.Format("2 Jan 15:04"))
			} else {
				k.Log("web nodes: %s can't be asked (%v), and isn't known as one", addr, err)
			}
		}
		k.warned[id] = true
		return was
	}
	delete(k.warned, id)
	switch {
	case web && now.Unix()-k.said[id] > 3600: // no need to write it each time
		k.said[id] = now.Unix()
		k.save()
	case !web && k.said[id] != 0:
		delete(k.said, id)
		k.save()
	}
	return web
}

func (k *knownWebNodes) load() {
	if k.loaded {
		return
	}
	k.loaded = true
	k.said, k.warned = map[string]int64{}, map[string]bool{}
	if b, err := os.ReadFile(k.Path); err == nil {
		json.Unmarshal(b, &k.said)
	}
}

func (k *knownWebNodes) save() {
	b, _ := json.MarshalIndent(k.said, "", "  ")
	if err := atomicfile.Replace(k.Path, append(b, '\n'), 0o600); err != nil && k.Log != nil {
		k.Log("web nodes: %v", err)
	}
}
