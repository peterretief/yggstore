// Package hub serves a small page on the LAN for a box such as a Raspberry
// Pi: whether its node is up and how much of the group answers, and a way to
// put a node on the machine looking at the page.
//
// A page can't run anything on the machine that opens it, so the hub gives
// that machine a one-line command instead: it fetches /install.sh from the
// hub, which downloads yggstore for its system (from the hub when it has that
// build, else from the project's releases) and joins the group with an invite
// the person pastes. The invite is the only secret, and it never passes
// through the hub: someone on the LAN without one can see the status and
// download yggstore, nothing more.
package hub

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

//go:embed hub.html
var pageHTML []byte

// Releases is where install.sh downloads builds the hub doesn't have.
const Releases = "https://github.com/peterretief/yggstore/releases/latest/download"

// Platforms are the release builds' names, as in yggstore-PLATFORM.
var Platforms = []string{"linux-amd64", "linux-arm64", "linux-armv7", "macos-intel", "macos-apple-silicon"}

// Platform is the release name for a GOOS and GOARCH, or "" if there is no
// release build for it.
func Platform(goos, goarch string) string {
	return map[string]string{
		"linux/amd64": "linux-amd64", "linux/arm64": "linux-arm64", "linux/arm": "linux-armv7",
		"darwin/amd64": "macos-intel", "darwin/arm64": "macos-apple-silicon",
	}[goos+"/"+goarch]
}

type Config struct {
	PeersPath string
	// Self is this box's node: its address in the peer list, or failing
	// that its name.
	SelfAddr, SelfName string
	Client             client.Client
	// Bins are the builds the hub hands out, by platform.
	Bins    map[string]string
	Version string
	// Users, if set, pairs devices through Syncthing.
	Users *Users
	// Join, if set, joins this box to a group with an invite, when an
	// admin's dashboard adds it (see join.go); it returns what join said.
	Join func(ctx context.Context, invite string) (string, error)
	// Work, if set, answers the group's web nodes over Yggdrasil when
	// someone opens their recovery link (POST /v1/work; see WorkHere).
	Work http.Handler
	// Files, if set, serves each person's files page at /files, over
	// Yggdrasil to their own devices (see files.go).
	Files http.Handler
}

type Hub struct {
	cfg Config

	mu      sync.Mutex
	state   State
	checked time.Time
	join    joinState
}

func New(cfg Config) *Hub { return &Hub{cfg: cfg} }

// NodeState is this box's node, as it answers.
type NodeState struct {
	Name       string `json:"name"`
	Addr       string `json:"addr"`
	Up         bool   `json:"up"`
	Error      string `json:"error,omitempty"`
	UsedBytes  int64  `json:"used_bytes,omitempty"`
	QuotaBytes int64  `json:"quota_bytes,omitempty"`
	Shards     int    `json:"shards,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
}

type State struct {
	Hostname string     `json:"hostname"`
	Version  string     `json:"version"`
	Node     *NodeState `json:"node,omitempty"` // nil: not in the peer list
	Problem  string     `json:"problem,omitempty"`
	Up       int        `json:"up"` // nodes in the group that answer
	Nodes    int        `json:"nodes"`
	Bins     []string   `json:"bins"` // platforms the hub has builds for
	Checked  int64      `json:"checked"`
	Sync     *Status    `json:"sync,omitempty"`     // pairing through Syncthing
	LAN      []string   `json:"lan"`                // this box's addresses on the LAN
	FilesAt  string     `json:"files_at,omitempty"` // the Yggdrasil address people open their files at
}

// check asks every node in the group whether it is up, at most every few
// seconds however often the page asks.
func (h *Hub) check(ctx context.Context) State {
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.checked) < 5*time.Second {
		return h.state
	}
	st := State{Version: h.cfg.Version, Bins: []string{}, LAN: LANAddrs()}
	st.Hostname, _ = os.Hostname()
	if h.cfg.Files != nil {
		st.FilesAt = h.cfg.SelfAddr
	}
	for p := range h.cfg.Bins {
		st.Bins = append(st.Bins, p)
	}
	sort.Strings(st.Bins)
	list, err := peers.Load(h.cfg.PeersPath)
	if err != nil {
		st.Problem = "This box isn't in a group yet, so files wait here until its owner adds it to one."
		if !os.IsNotExist(err) {
			st.Problem = "This box's member list can't be read: " + err.Error()
		}
	}
	st.Nodes = len(list)
	nodes := make([]NodeState, len(list))
	var wg sync.WaitGroup
	for i, p := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			n := NodeState{Name: p.Name, Addr: p.Addr}
			info, err := h.cfg.Client.Info(cctx, p.Addr)
			if err != nil {
				n.Error = err.Error()
			} else {
				n.Up, n.UsedBytes, n.QuotaBytes, n.Shards, n.StartedAt = true, info.UsedBytes, info.QuotaBytes, info.ShardCount, info.StartedAt
			}
			nodes[i] = n
		}()
	}
	wg.Wait()
	for i, p := range list {
		if nodes[i].Up {
			st.Up++
		}
		if st.Node == nil && h.cfg.SelfAddr != "" && p.IP() == h.cfg.SelfAddr {
			st.Node = &nodes[i]
		}
	}
	for i, p := range list {
		if st.Node == nil && p.Name == h.cfg.SelfName {
			st.Node = &nodes[i]
		}
	}
	st.Checked = time.Now().Unix()
	h.state, h.checked = st, time.Now()
	return st
}

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(pageHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		st := h.check(r.Context())
		if h.cfg.Users != nil {
			s := h.cfg.Users.Status()
			st.Sync = &s
		}
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("GET /install.sh", h.handleInstall)
	mux.HandleFunc("POST /api/join", h.handleJoin)
	mux.HandleFunc("POST /api/anywhere", h.handleAnywhere)
	mux.HandleFunc("GET /bin/{platform}", func(w http.ResponseWriter, r *http.Request) {
		path, ok := h.cfg.Bins[r.PathValue("platform")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
	})
	lan := lanOnly(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/work" && h.cfg.Work != nil {
			h.cfg.Work.ServeHTTP(w, r) // checks the caller is a member itself
			return
		}
		if (r.URL.Path == FilesPath || strings.HasPrefix(r.URL.Path, FilesPath+"/")) && h.cfg.Files != nil {
			h.cfg.Files.ServeHTTP(w, r) // checks whose device asks itself
			return
		}
		lan.ServeHTTP(w, r)
	})
}

// lanOnly answers this machine and the local network (see OnLAN), and
// nobody else, in case the port is opened to the internet by mistake.
func lanOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !OnLAN(r.RemoteAddr) {
			http.Error(w, "the hub only answers the local network", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostRE is what the hub accepts as its own name, since it goes into a
// shell script.
var hostRE = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]+)?$|^\[[0-9A-Fa-f:.]+\](:[0-9]+)?$`)

func (h *Hub) handleInstall(w http.ResponseWriter, r *http.Request) {
	if !hostRE.MatchString(r.Host) {
		http.Error(w, "bad host", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, installScript, "http://"+r.Host, Releases)
}

// Own is the hub's own executable, as a build for this platform, so a box
// like it can get yggstore without going to the internet.
func Own() map[string]string {
	bins := map[string]string{}
	if p := Platform(runtime.GOOS, runtime.GOARCH); p != "" {
		if exe, err := os.Executable(); err == nil {
			bins[p] = exe
		}
	}
	return bins
}

// FromDir adds the builds in dir named as releases are (yggstore-PLATFORM).
func FromDir(bins map[string]string, dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p, ok := strings.CutPrefix(e.Name(), "yggstore-")
		if ok && !e.IsDir() && slices.Contains(Platforms, p) {
			bins[p] = dir + string(os.PathSeparator) + e.Name()
		}
	}
	return nil
}

// installScript is run on the new box as
// curl -fsSL HUB/install.sh | sh -s -- INVITE. Its %s are the hub and the
// releases.
const installScript = `#!/bin/sh
# Puts a yggstore node on this machine and joins the group with an invite.
# From the hub at %[1]s.
set -eu
HUB='%[1]s'
RELEASES='%[2]s'
invite="${1:-}"
if [ -z "$invite" ]; then
	echo "usage: curl -fsSL $HUB/install.sh | sh -s -- INVITE" >&2
	echo "(make the invite on an admin's dashboard)" >&2
	exit 2
fi
case "$(uname -s)-$(uname -m)" in
	Linux-x86_64) p=linux-amd64 ;;
	Linux-aarch64|Linux-arm64) p=linux-arm64 ;;
	Linux-armv7l|Linux-armv8l) p=linux-armv7 ;;
	Darwin-x86_64) p=macos-intel ;;
	Darwin-arm64) p=macos-apple-silicon ;;
	*) echo "There is no yggstore build for $(uname -s) $(uname -m)." >&2; exit 1 ;;
esac
if [ -e "$HOME/.yggstore/peers.json" ]; then
	echo "This machine already has a yggstore node ($HOME/.yggstore); not touching it." >&2
	exit 1
fi
bin="$HOME/.local/bin"
mkdir -p "$bin"
tmp="$(mktemp)"
trap 'rm -f "$tmp" "$tmp.sums"' EXIT
fetch() { # URL FILE
	if command -v curl >/dev/null 2>&1; then curl -fsSL -o "$2" "$1"; else wget -qO "$2" "$1"; fi
}
if fetch "$HUB/bin/$p" "$tmp" 2>/dev/null; then
	echo "Got yggstore ($p) from the hub."
else
	echo "The hub has no $p build; downloading the latest release."
	fetch "$RELEASES/yggstore-$p" "$tmp"
	if fetch "$RELEASES/SHA256SUMS" "$tmp.sums" 2>/dev/null; then
		want="$(awk -v f="yggstore-$p" '$2 == f || $2 == "*" f { print $1 }' "$tmp.sums")"
		if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$tmp" | cut -d' ' -f1)"; else got="$(shasum -a 256 "$tmp" | cut -d' ' -f1)"; fi
		if [ -z "$want" ] || [ "$want" != "$got" ]; then echo "The download doesn't match its checksum; stopping." >&2; exit 1; fi
	fi
fi
chmod +x "$tmp"
mv "$tmp" "$bin/yggstore"
echo "Installed $bin/yggstore ($("$bin/yggstore" version))."
if [ "$(uname -s)" = Linux ] && command -v systemctl >/dev/null 2>&1; then
	"$bin/yggstore" join -service "$invite"
	if command -v loginctl >/dev/null 2>&1 && [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
		echo
		echo "To keep the node running when you log out:"
		echo "  sudo loginctl enable-linger $(id -un)"
	fi
else
	"$bin/yggstore" join "$invite"
fi
case ":$PATH:" in *":$bin:"*) ;; *) echo; echo "Add $bin to your PATH to run yggstore by name." ;; esac
`

// handleAnywhere turns on working from anywhere for the person whose
// device is named, from the box's page.
func (h *Hub) handleAnywhere(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.cfg.Users == nil {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "this box doesn't sync devices"})
		return
	}
	var req struct{ Device, Email string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "bad request"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	msg, err := h.cfg.Users.Anywhere(ctx, req.Device, req.Email)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
