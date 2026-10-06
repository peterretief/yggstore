package yggnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func node(t *testing.T, cfg Config) *Node {
	_, cfg.Key, _ = ed25519.GenerateKey(rand.Reader)
	cfg.Port = 7400
	cfg.Logf = t.Logf
	n, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// get retries while the link and the path between the nodes come up.
func get(t *testing.T, rt http.RoundTripper, url string) string {
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := rt.RoundTrip(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: %s %s", url, resp.Status, b)
			}
			return string(b)
		}
		cancel()
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", url, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestNodesTalk(t *testing.T) {
	port := freePort(t)
	a := node(t, Config{Listen: []string{fmt.Sprintf("tcp://127.0.0.1:%d", port)}})
	b := node(t, Config{Peers: []string{fmt.Sprintf("tcp://127.0.0.1:%d", port)}})
	if !IsYggdrasil(a.Addr()) || a.Addr().Equal(b.Addr()) {
		t.Fatalf("addresses %s %s", a.Addr(), b.Addr())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The handler answers with who called, as the shard server checks it.
	whoCalled := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s", host, r.URL.Path, body)
	})
	go a.Serve(ctx, whoCalled)

	url := "http://" + net.JoinHostPort(a.Addr().String(), "7400") + "/v1/info"
	if got, want := get(t, &Router{Other: http.DefaultTransport, node: b}, url), b.Addr().String()+" /v1/info "; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A program on b's machine goes through b's proxy and is b to a.
	proxy := httptest.NewServer(b.Proxy())
	defer proxy.Close()
	t.Setenv("YGGSTORE_PROXY", strings.TrimPrefix(proxy.URL, "http://"))
	local := &Router{Other: &http.Transport{}}
	if ip, ok := local.localNode(local.Other); !ok || !ip.Equal(b.Addr()) {
		t.Fatalf("local node: %v %v", ip, ok)
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("hello"))
	resp, err := local.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := b.Addr().String() + " /v1/info hello"; string(body) != want {
		t.Fatalf("through the proxy: %q, want %q", body, want)
	}
	// The proxy is only a way to other nodes.
	req, _ = http.NewRequest(http.MethodGet, "http://example.org/", nil)
	resp, err = (&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustURL(proxy.URL))}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("proxy to the web: %s", resp.Status)
	}
}

func mustURL(s string) *neturl.URL {
	u, err := neturl.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestPacket(t *testing.T) {
	_, k1, _ := ed25519.GenerateKey(rand.Reader)
	_, k2, _ := ed25519.GenerateKey(rand.Reader)
	src := *address.AddrForKey(k1.Public().(ed25519.PublicKey))
	dst := *address.AddrForKey(k2.Public().(ed25519.PublicKey))
	for _, payload := range []string{"", "x", "hello, node"} {
		b := packet(src, dst, 7400, 51234, []byte(payload))
		// A correct checksum makes the one's-complement sum come to 0xffff.
		var sum uint32
		add := func(p []byte) {
			for i := 0; i < len(p); i += 2 {
				v := uint32(p[i]) << 8
				if i+1 < len(p) {
					v |= uint32(p[i+1])
				}
				sum += v
			}
		}
		add(b[8:40])
		sum += uint32(len(b)-ipv6Header) + udpProto
		add(b[ipv6Header:])
		for sum>>16 != 0 {
			sum = sum&0xffff + sum>>16
		}
		if sum != 0xffff {
			t.Fatalf("%q: checksum sums to %#x", payload, sum)
		}
		pc := &packetConn{self: dst, port: 51234}
		from, port, got, ok := pc.parse(b)
		if !ok || from != src || port != 7400 || string(got) != payload {
			t.Fatalf("%q: parsed %v %d %q %v", payload, from, port, got, ok)
		}
		pc.port = 9
		if _, _, _, ok := pc.parse(b); ok {
			t.Fatal("took a packet for another port")
		}
		short := append([]byte(nil), b...)
		binary.BigEndian.PutUint16(short[4:6], uint16(len(b)))
		if _, _, _, ok := (&packetConn{self: dst, port: 51234}).parse(short); ok {
			t.Fatal("took a packet longer than it is")
		}
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ygg.key")
	key, err := LoadKey(path, true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadKey(path, false)
	if err != nil || !key.Equal(again) {
		t.Fatalf("reloaded: %v", err)
	}
	conf := filepath.Join(dir, "yggdrasil.conf")
	os.WriteFile(conf, []byte("{\n  # comment\n  PrivateKey: "+fmt.Sprintf("%x", []byte(key))+"\n  Peers: []\n}\n"), 0o600)
	if fromConf, err := LoadKey(conf, false); err != nil || !key.Equal(fromConf) {
		t.Fatalf("from yggdrasil.conf: %v", err)
	}
	if _, err := LoadKey(filepath.Join(dir, "missing"), false); err == nil {
		t.Fatal("a missing key file loaded")
	}
}
