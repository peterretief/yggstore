package site

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

// buildHandler builds testdata/handler for WASI, as a site's api/NAME.wasm.
func buildHandler(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a WebAssembly module")
	}
	out := filepath.Join(t.TempDir(), "hello.wasm")
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", out, "main.go")
	cmd.Dir = filepath.Join("testdata", "handler")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the handler: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func request(h http.Handler, method, host, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("User-Agent", "tester/1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A site's handler runs on the web node once its publisher turns code on:
// a request in, the reply out, with the time and size limits, and the
// module itself never handed out.
func TestHandlers(t *testing.T) {
	wasm := buildHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := storageNodes(t, 6)
	nodes := append(append([]peers.Peer{}, storage...), peers.Peer{Name: "admin", Addr: "[200::1]:7400", Admin: true})
	b := newBus() // publishes as 200::1, the admin
	pub := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	web := &Web{Dir: t.TempDir(), Msgs: b, Peers: func() []peers.Peer { return nodes }, Client: client.New(), Log: t.Logf}
	go web.Run(ctx)

	src := t.TempDir()
	writeSite(t, src, map[string]string{"index.html": "<h1>courses</h1>", "api/hello.wasm": string(wasm)})
	if _, _, err := pub.Publish(ctx, "courses.example", src, storage); err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "courses.example", "courses")
	if rec := request(web, "GET", "courses.example", "/api/hello?name=Kath", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("before code is on: %d", rec.Code)
	}

	if err := pub.SetCode(ctx, "courses.example", true); err != nil {
		t.Fatal(err)
	}
	var rec *httptest.ResponseRecorder
	for deadline := time.Now().Add(20 * time.Second); ; {
		if rec = request(web, "GET", "courses.example", "/api/hello?name=Kath", ""); rec.Code != http.StatusNotFound || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(rec.Body.String(), `"greeting":"Hello, Kath"`) || !strings.Contains(rec.Body.String(), `"host":"courses.example"`) {
		t.Fatalf("hello: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := request(web, "POST", "courses.example", "/api/hello/echo", "shout"); rec.Code != http.StatusCreated || rec.Body.String() != "SHOUT" || rec.Header().Get("X-Seen-Agent") != "tester/1" {
		t.Fatalf("echo: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	for path, want := range map[string]int{
		"/api/hello/slow":  http.StatusGatewayTimeout,
		"/api/hello/big":   http.StatusBadGateway,
		"/api/hello/crash": http.StatusBadGateway,
		"/api/hello.wasm":  http.StatusNotFound,
		"/api/nothing":     http.StatusNotFound,
	} {
		start := time.Now()
		if rec := request(web, "GET", "courses.example", path, ""); rec.Code != want {
			t.Errorf("%s: %d, want %d (%s)", path, rec.Code, want, rec.Body)
		}
		if took := time.Since(start); took > HandlerTime+2*time.Second {
			t.Errorf("%s took %v", path, took)
		}
	}
	if rec := request(web, "GET", "courses.example", "/api/hello?name=Kath", ""); rec.Code != http.StatusOK {
		t.Fatalf("after the failures: %d", rec.Code)
	}

	// A web node can refuse to run code; then the others answer.
	web.NoCode = true
	if rec := request(web, "GET", "courses.example", "/api/hello", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("with NoCode: %d", rec.Code)
	}
	web.NoCode = false

	// Someone other than the admin: their code waits for the admin, before
	// anything is run (so this module needn't be a real one).
	b.mu.Lock()
	b.from = "200::7"
	b.mu.Unlock()
	nodes = append(nodes, peers.Peer{Name: "member", Addr: "[200::7]:7400"})
	other := &Publisher{Dir: t.TempDir(), Client: client.New(), Announce: b, Log: t.Logf}
	src2 := t.TempDir()
	writeSite(t, src2, map[string]string{"index.html": "<h1>shop</h1>", "api/hello.wasm": "not run"})
	if _, _, err := other.Publish(ctx, "shop.example", src2, storage); err != nil {
		t.Fatal(err)
	}
	eventually(t, web, "shop.example", "shop")
	other.SetCode(ctx, "shop.example", true)
	for deadline := time.Now().Add(10 * time.Second); ; {
		if rec = request(web, "GET", "shop.example", "/api/hello", ""); rec.Code != http.StatusNotFound || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a member's code: %d %s", rec.Code, rec.Body)
	}
}
