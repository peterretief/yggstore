package hub

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
)

// TestFindAndAdd: a dashboard finds a hub that isn't in a group, hands it an
// invite, and it joins; then it's in a group, and takes no more.
func TestFindAndAdd(t *testing.T) {
	dir := t.TempDir()
	peersPath := filepath.Join(dir, "peers.json")
	var got string
	h := New(Config{PeersPath: peersPath, Client: client.New(), Version: "v9",
		Join: func(ctx context.Context, invite string) (string, error) {
			got = invite
			os.WriteFile(peersPath, []byte("[]"), 0o644) // as join would
			return "You're in", nil
		}})

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Answer(ctx, pc, 7490)
	boxes, err := Find(ctx, 0, []string{pc.LocalAddr().String()}, 500*time.Millisecond)
	if err != nil || len(boxes) != 1 || boxes[0].Joined || boxes[0].Addr != "127.0.0.1" || boxes[0].Version != "v9" {
		t.Fatalf("found %+v, %v; want one box not in a group", boxes, err)
	}

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	out, err := AddBox(ctx, "127.0.0.1", port, "yggjoin1:abc")
	if err != nil || out != "You're in" || got != "yggjoin1:abc" {
		t.Fatalf("add: %q, %v (invite %q)", out, err, got)
	}
	if _, err := AddBox(ctx, "127.0.0.1", port, "yggjoin1:again"); err == nil || got != "yggjoin1:abc" {
		t.Fatalf("a box in a group took another invite: %v", err)
	}
	if boxes, _ := Find(ctx, 0, []string{pc.LocalAddr().String()}, 500*time.Millisecond); len(boxes) != 1 || !boxes[0].Joined {
		t.Fatalf("after joining: %+v", boxes)
	}
	if _, err := AddBox(ctx, "8.8.8.8", port, "yggjoin1:x"); err == nil {
		t.Fatal("handed an invite to a box off the LAN")
	}
}

func TestJoinNeedsHeaderAndReportsFailure(t *testing.T) {
	h := New(Config{PeersPath: filepath.Join(t.TempDir(), "peers.json"), Client: client.New(),
		Join: func(context.Context, string) (string, error) {
			return "the group did not accept", errors.New("exit status 1")
		}})
	req := httptest.NewRequest("POST", "/api/join", strings.NewReader(`{"invite":"yggjoin1:x"}`))
	req.RemoteAddr = "192.168.1.9:5000"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no header: %d, want 403", rec.Code)
	}
	req = httptest.NewRequest("POST", "/api/join", strings.NewReader(`{"invite":"yggjoin1:x"}`))
	req.RemoteAddr, req.Header = "192.168.1.9:5000", http.Header{"X-Yggstore": {"1"}}
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "did not accept") {
		t.Fatalf("failed join: %d %s", rec.Code, rec.Body.String())
	}
}
