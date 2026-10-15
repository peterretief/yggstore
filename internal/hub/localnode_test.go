package hub

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodeFromArgs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "shards"), 0o755)
	os.WriteFile(filepath.Join(dir, "peers.json"), []byte("[]"), 0o644)

	// Relative paths, member list only: that's the one used.
	n, ok := nodeFromArgs([]string{"/usr/bin/yggstore", "serve", "-peers", "peers.json", "-data=shards", "-name", "pi"}, dir, "/home/x")
	if !ok || n.Peers != filepath.Join(dir, "peers.json") || n.MsgToken != "/home/x/.yggstore/msg.token" || n.Mailbox != "/home/x/.yggstore/mail" {
		t.Fatalf("got %+v, %v", n, ok)
	}
	// A second node on the machine has no messaging: not the box's node.
	if _, ok := nodeFromArgs([]string{"yggstore", "serve", "-peers", "peers.json", "-port", "7401", "-msg-api", ""}, dir, "/home/x"); ok {
		t.Fatal("a node without messaging taken for the box's")
	}
	// A list the admin pushed is newer, so it wins.
	os.WriteFile(filepath.Join(dir, "shards", "peers.pushed.json"), []byte("[]"), 0o644)
	n, ok = nodeFromArgs([]string{"yggstore", "serve", "-peers", filepath.Join(dir, "peers.json"), "-data", filepath.Join(dir, "shards"), "-msg-token", "/t/msg.token"}, "/", "")
	if !ok || n.Peers != filepath.Join(dir, "shards", "peers.pushed.json") || n.MsgToken != "/t/msg.token" {
		t.Fatalf("got %+v, %v", n, ok)
	}
	for _, args := range [][]string{{"yggstore", "dashboard", "-peers", "peers.json"}, {"other", "serve"}, {"yggstore"}} {
		if _, ok := nodeFromArgs(args, dir, ""); ok {
			t.Fatalf("%v taken for a node", args)
		}
	}
}
