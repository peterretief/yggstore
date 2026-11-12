package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A web node that can't be asked, as just after every node restarted, is
// still one, on a node that has itself just restarted; a member that never
// said it's a web node isn't; and one that says it no longer is, isn't.
func TestWebNodeRememberedWhenItCantBeAsked(t *testing.T) {
	dir := t.TempDir()
	peersPath := filepath.Join(dir, "peers.json")
	os.WriteFile(peersPath, []byte(`[{"name":"web","addr":"[200::1]:7400"},{"name":"box","addr":"[200::2]:7400"}]`), 0o600)
	web, down := true, false
	newK := func() *knownWebNodes {
		return &knownWebNodes{Path: filepath.Join(dir, "web-nodes.json"), PeersPath: peersPath, Keep: time.Hour,
			Ask: func(_ context.Context, addr string) (bool, error) {
				if down {
					return false, errors.New("no answer")
				}
				return web && addr == "[200::1]:7400", nil
			}}
	}
	ctx := context.Background()
	k := newK()
	if !k.Is(ctx, "200::1") || k.Is(ctx, "200::2") {
		t.Fatal("asked: the web node is one, the box isn't")
	}
	if k.Is(ctx, "200::9") {
		t.Fatal("not a member, but taken for a web node")
	}
	down = true
	k = newK() // this node restarted too
	if !k.Is(ctx, "200::1") {
		t.Fatal("a web node that can't be asked was refused")
	}
	if k.Is(ctx, "200::2") {
		t.Fatal("a box that can't be asked was taken for a web node")
	}
	k.Keep = 0
	if k.Is(ctx, "200::1") {
		t.Fatal("what it said long ago still counts")
	}
	down, web = false, false
	k = newK()
	if k.Is(ctx, "200::1") {
		t.Fatal("it says it isn't a web node any more")
	}
	down = true
	if newK().Is(ctx, "200::1") {
		t.Fatal("it stopped being a web node, then couldn't be asked: still taken for one")
	}
}
