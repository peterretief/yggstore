package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Adding a box with one press: a hub answers probes on the LAN, so an
// admin's dashboard can list the boxes there, and a hub that isn't in a
// group yet takes one invite from the dashboard and joins with it. Until it
// is in a group, whoever on the LAN sends it an invite first gets it, as
// with setting up a new router; after that it takes no more.

// probe is what a dashboard sends to find hubs.
const probe = "yggstore-hub?"

// Beacon is a hub's answer to a probe.
type Beacon struct {
	Name    string `json:"name"`
	Joined  bool   `json:"joined"` // in a group already
	Version string `json:"version"`
	Port    int    `json:"port"` // its page and API
}

// Box is a hub found on the LAN.
type Box struct {
	Beacon
	Addr string `json:"addr"` // its LAN address
}

// joined is whether this box is in a group: its member list exists.
func (h *Hub) joined() bool {
	_, err := os.Stat(h.cfg.PeersPath)
	return err == nil
}

// Answer answers probes arriving on conn until ctx ends.
func (h *Hub) Answer(ctx context.Context, conn net.PacketConn, port int) {
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	name, _ := os.Hostname()
	buf := make([]byte, 64)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if string(buf[:n]) != probe || !OnLAN(from.String()) {
			continue
		}
		b, _ := json.Marshal(Beacon{Name: name, Joined: h.joined(), Version: h.cfg.Version, Port: port})
		conn.WriteTo(b, from)
	}
}

// Find asks the LAN for hubs and lists those that answer within wait.
// targets are where to send the probe, host:port; nil means every IPv4
// broadcast address of this machine's networks, on port.
func Find(ctx context.Context, port int, targets []string, wait time.Duration) ([]Box, error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if targets == nil {
		targets = broadcasts(port)
	}
	for _, t := range targets {
		if a, err := net.ResolveUDPAddr("udp4", t); err == nil {
			conn.WriteTo([]byte(probe), a)
		}
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)
	seen := map[string]bool{}
	var out []Box
	buf := make([]byte, 1024)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return out, nil // the deadline: done listening
		}
		host, _, _ := net.SplitHostPort(from.String())
		var b Beacon
		if json.Unmarshal(buf[:n], &b) != nil || seen[host] || !OnLAN(host) {
			continue
		}
		seen[host] = true
		out = append(out, Box{Beacon: b, Addr: host})
	}
}

// broadcasts are the IPv4 broadcast addresses of this machine's networks,
// and the all-ones one.
func broadcasts(port int) []string {
	p := ":" + strconv.Itoa(port)
	out := []string{"255.255.255.255" + p}
	for _, n := range localNets() {
		ip := n.IP.To4()
		if ip == nil || len(n.Mask) != net.IPv4len {
			continue
		}
		b := make(net.IP, 4)
		for i := range b {
			b[i] = ip[i] | ^n.Mask[i]
		}
		out = append(out, b.String()+p)
	}
	return out
}

// joinState keeps one join at a time.
type joinState struct {
	mu   sync.Mutex
	busy bool
}

// handleJoin takes an invite from an admin's dashboard and joins the group
// with it, if this box isn't in one yet.
func (h *Hub) handleJoin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Join == nil {
		http.Error(w, "this hub doesn't join groups by itself", http.StatusNotImplemented)
		return
	}
	if r.Header.Get("X-Yggstore") != "1" {
		http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
		return
	}
	if h.joined() {
		http.Error(w, "this box is in a group already", http.StatusConflict)
		return
	}
	var req struct {
		Invite string `json:"invite"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil || !strings.HasPrefix(req.Invite, "yggjoin1:") {
		http.Error(w, "no invite", http.StatusBadRequest)
		return
	}
	h.join.mu.Lock()
	if h.join.busy {
		h.join.mu.Unlock()
		http.Error(w, "this box is joining a group already", http.StatusConflict)
		return
	}
	h.join.busy = true
	h.join.mu.Unlock()
	defer func() {
		h.join.mu.Lock()
		h.join.busy = false
		h.join.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	out, err := h.cfg.Join(ctx, req.Invite)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error(), "output": out})
		return
	}
	h.mu.Lock()
	h.checked = time.Time{} // show the group at once
	h.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "output": out})
}

// AddBox hands a box an invite and waits for it to join; it returns what
// the box said.
func AddBox(ctx context.Context, addr string, port int, invite string) (string, error) {
	if !OnLAN(addr) {
		return "", errors.New("that box isn't on the local network")
	}
	body, _ := json.Marshal(map[string]string{"invite": invite})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+net.JoinHostPort(addr, strconv.Itoa(port))+"/api/join", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Yggstore", "1")
	resp, err := (&http.Client{Timeout: 4 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var res struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return "", errors.New(strings.TrimSpace(string(raw)))
	}
	if !res.OK {
		return res.Output, errors.New(res.Error)
	}
	return res.Output, nil
}
