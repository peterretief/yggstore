package hub

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// LocalNode is what the hub needs from the yggstore node running on the
// same machine, which stores what devices sync to the hub: the group's
// member list it holds, and its messaging token.
type LocalNode struct {
	Peers    string // the newest member list: the one the admin pushed, else its own
	MsgToken string
}

// FindLocalNode looks for a "yggstore serve" process on this machine (Linux)
// and reads where it keeps its member list and messaging token from its
// command line, so the hub uses the node's group without being told.
func FindLocalNode() (LocalNode, bool) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return LocalNode{}, false
	}
	for _, e := range ents {
		pid := e.Name()
		if strings.Trim(pid, "0123456789") != "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
		if n, ok := nodeFromArgs(args, procDir(pid), procHome(pid)); ok {
			return n, true
		}
	}
	return LocalNode{}, false
}

// nodeFromArgs reads a node's files from its command line; dir is its
// working directory (for relative paths) and home its HOME (for defaults).
func nodeFromArgs(args []string, dir, home string) (LocalNode, bool) {
	if len(args) < 2 || filepath.Base(args[0]) != "yggstore" || args[1] != "serve" {
		return LocalNode{}, false
	}
	flags := map[string]string{}
	for i := 2; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		if a == args[i] {
			continue // not a flag
		}
		if k, v, ok := strings.Cut(a, "="); ok {
			flags[k] = v
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags[a] = args[i+1]
			i++
		} else {
			flags[a] = "true"
		}
	}
	abs := func(p string) string {
		if p != "" && !filepath.IsAbs(p) && dir != "" {
			return filepath.Join(dir, p)
		}
		return p
	}
	yh := filepath.Join(home, ".yggstore")
	data := abs(flags["data"])
	if data == "" && home != "" {
		data = filepath.Join(yh, "shards")
	}
	n := LocalNode{Peers: abs(flags["peers"]), MsgToken: abs(flags["msg-token"])}
	if pushed := filepath.Join(data, "peers.pushed.json"); data != "" && exists(pushed) {
		n.Peers = pushed
	}
	if n.Peers == "" && home != "" {
		n.Peers = filepath.Join(yh, "peers.json")
	}
	if n.MsgToken == "" && home != "" {
		n.MsgToken = filepath.Join(yh, "msg.token")
	}
	return n, n.Peers != "" && exists(n.Peers)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func procDir(pid string) string {
	d, _ := os.Readlink(filepath.Join("/proc", pid, "cwd"))
	return d
}

func procHome(pid string) string {
	raw, _ := os.ReadFile(filepath.Join("/proc", pid, "environ"))
	for _, kv := range bytes.Split(raw, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("HOME=")); ok {
			return string(v)
		}
	}
	return ""
}
