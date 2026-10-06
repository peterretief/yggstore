package site

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCloudflared is a script that acts like cloudflared: it says it has
// connected, writes its pid, and runs until stopped.
func fakeCloudflared(t *testing.T, dir string) string {
	path := filepath.Join(dir, "cloudflared")
	script := "#!/bin/sh\necho $$ > " + filepath.Join(dir, "pid") + "\necho '2026-01-01T00:00:00Z INF Registered tunnel connection connIndex=0'\nexec sleep 600\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func running(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return false
	}
	_, err = os.Stat("/proc/" + strings.TrimSpace(string(b)))
	return err == nil
}

func waitState(t *testing.T, tun *Tunnel, prefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasPrefix(tun.State(), prefix) {
		if time.Now().After(deadline) {
			t.Fatalf("tunnel state %q, want %s…", tun.State(), prefix)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTunnelFollowsWebServer(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("needs /proc")
	}
	dir := t.TempDir()
	var up atomic.Bool
	up.Store(true)
	web := &Web{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		web.ServeHTTP(w, r)
	}))
	defer srv.Close()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("eyJhIjoi\n"), 0o600)
	tun := &Tunnel{Bin: fakeCloudflared(t, dir), TokenFile: token, Dir: dir,
		WebAddr: strings.TrimPrefix(srv.URL, "http://"), Log: t.Logf, Every: 20 * time.Millisecond}
	if err := tun.Check(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tun.Run(ctx); close(done) }()

	waitState(t, tun, "connected")
	if !running(dir) {
		t.Fatal("connector not running")
	}
	up.Store(false) // the web server stops answering
	waitState(t, tun, "stopped")
	time.Sleep(50 * time.Millisecond)
	if running(dir) {
		t.Fatal("connector kept running in front of a dead web server")
	}
	up.Store(true)
	waitState(t, tun, "connected")
	cancel()
	<-done
	time.Sleep(50 * time.Millisecond)
	if running(dir) {
		t.Fatal("connector outlived the node")
	}
}

func TestTunnelRejectsTunnelID(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("cb71a30e-6204-4dea-95e9-cdce349a2efe\n"), 0o600)
	tun := &Tunnel{Bin: "sh", TokenFile: token}
	if err := tun.Check(); err == nil || !strings.Contains(err.Error(), "ID is not its token") {
		t.Fatalf("Check: %v", err)
	}
}
