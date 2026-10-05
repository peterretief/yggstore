package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
)

type recordNet struct {
	mu   sync.Mutex
	sent []msg.Message
}

func (n *recordNet) Deliver(ctx context.Context, addr string, m msg.Message) error {
	n.mu.Lock()
	n.sent = append(n.sent, m)
	n.mu.Unlock()
	return nil
}
func (n *recordNet) Announce(context.Context, string, []string) error { return nil }
func (n *recordNet) History(context.Context, string, string, uint64) ([]msg.Message, error) {
	return nil, nil
}

func TestMessagesThroughTheNode(t *testing.T) {
	dir := t.TempDir()
	list := []peers.Peer{{Name: "desktop", Addr: "[200::1]:7400"}, {Name: "pi", Addr: "[200::2]:7400"}}
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, list)
	net := &recordNet{}
	e, err := msg.Open(filepath.Join(dir, "msg"), "200::1", func() []peers.Peer { return list }, net, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	token := strings.Repeat("cd", 24)
	tokenPath := filepath.Join(dir, "msg.token")
	os.WriteFile(tokenPath, []byte(token), 0o600)
	api := httptest.NewServer(e.LocalHandler(token))
	defer api.Close()

	d := New(Config{PeersPath: peersPath, StubDir: dir, SelfID: "200::1", Listen: "127.0.0.1:7480",
		MsgAPI: api.Listener.Addr().String(), MsgTokenPath: tokenPath})
	h := d.Handler()
	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do(post("/api/msg/send", strings.NewReader(`{"to":"pi","body":"hello pi"}`))); w.Code != http.StatusNoContent {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	if w := do(post("/api/msg/subscribe", strings.NewReader(`{"topic":"alerts"}`))); w.Code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	if w := do(post("/api/msg/send", strings.NewReader(`{"topic":"alerts","body":"to the topic"}`))); w.Code != http.StatusNoContent {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		net.mu.Lock()
		n := len(net.sent)
		net.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("direct message never left")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/msg", nil)
	r.Host = "127.0.0.1:7480"
	w := do(r)
	var st msgState
	json.Unmarshal(w.Body.Bytes(), &st)
	if !st.Available || len(st.Members) != 1 || st.Members[0] != "pi" || st.Status.Subscriptions[0] != "alerts" ||
		len(st.Messages) != 1 || st.Messages[0].Body != "to the topic" {
		t.Fatalf("state: %s", w.Body)
	}
	// Without the header, a web page can't send through the dashboard.
	bad := httptest.NewRequest(http.MethodPost, "/api/msg/send", strings.NewReader(`{"to":"pi","body":"x"}`))
	bad.Host = "127.0.0.1:7480"
	if w := do(bad); w.Code == http.StatusNoContent {
		t.Fatal("send without the dashboard's header was accepted")
	}
}
