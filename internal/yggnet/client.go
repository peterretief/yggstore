package yggnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// Router sends each request for a Yggdrasil address the way this process
// can reach it:
//
//   - through the process's own built-in Yggdrasil (a node's serve);
//   - else through the node on this machine, if it runs the built-in one
//     (the dashboard and commands, which have no Yggdrasil of their own);
//   - else over the system Yggdrasil: HTTP/3 to nodes that speak it, plain
//     HTTP over TCP to nodes from before.
//
// Everything else goes to Other.
type Router struct {
	Other http.RoundTripper

	mu       sync.Mutex
	node     *Node
	sys      *http3.Transport  // HTTP/3 over the system Yggdrasil
	viaNode  http.RoundTripper // through the local node's proxy
	checked  time.Time         // when the local node was last looked for
	nodeAddr net.IP            // the local node's address, if it has a proxy
	ways     map[string]way    // by host:port, over the system Yggdrasil
	probing  map[string]chan struct{}
}

type way struct {
	quic  bool
	until time.Time
}

// Default is the Router Install puts in place.
var Default = &Router{}

// Install routes all of this process's HTTP through Default, so every
// http.Client that doesn't set its own Transport reaches other nodes.
func Install() {
	if http.DefaultTransport != Default {
		Default.Other = http.DefaultTransport
		http.DefaultTransport = Default
	}
}

// UseNode sends this process's requests through its own built-in Yggdrasil.
func (r *Router) UseNode(n *Node) {
	r.mu.Lock()
	r.node = n
	r.mu.Unlock()
}

// ProxyAddr is where a node with the built-in Yggdrasil takes requests from
// programs on its machine ($YGGSTORE_PROXY, else 127.0.0.1:7402).
func ProxyAddr() string {
	if a := os.Getenv("YGGSTORE_PROXY"); a != "" {
		return a
	}
	return "127.0.0.1:7402"
}

// SelfPath is where the proxy tells programs the node's address.
const SelfPath = "/_yggstore/self"

func (r *Router) RoundTrip(req *http.Request) (*http.Response, error) {
	other := r.Other
	if other == nil {
		other = http.DefaultTransport
		if other == r {
			return nil, errors.New("yggnet: Router has no Other")
		}
	}
	ip := net.ParseIP(req.URL.Hostname())
	if req.URL.Scheme != "http" || !IsYggdrasil(ip) {
		return other.RoundTrip(req)
	}
	r.mu.Lock()
	node := r.node
	r.mu.Unlock()
	if node != nil {
		return node.rt.RoundTrip(h3Request(req))
	}
	if _, ok := r.localNode(other); ok {
		return r.viaNode.RoundTrip(req)
	}
	quic, err := r.way(req.Context(), req.URL.Host)
	if err != nil {
		return nil, err
	}
	var resp *http.Response
	if quic {
		resp, err = r.system().RoundTrip(h3Request(req))
	} else {
		resp, err = other.RoundTrip(req)
	}
	if err != nil && req.Context().Err() == nil {
		// The node may have changed how it listens: ask again next time.
		r.mu.Lock()
		delete(r.ways, req.URL.Host)
		r.mu.Unlock()
	}
	return resp, err
}

// h3Request is req for an HTTP/3 transport, which only takes https URLs.
// TLS there is only what QUIC requires; see serverTLS.
func h3Request(req *http.Request) *http.Request {
	out := req.Clone(req.Context())
	out.URL.Scheme = "https"
	return out
}

func (r *Router) system() *http3.Transport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sys == nil {
		r.sys = &http3.Transport{TLSClientConfig: clientTLS(), QUICConfig: quicConfig()}
	}
	return r.sys
}

// LocalNode is the address of the node on this machine, if it runs the
// built-in Yggdrasil.
func LocalNode() (net.IP, bool) {
	other := Default.Other
	if other == nil {
		other = http.DefaultTransport
		if other == Default {
			other = &http.Transport{}
		}
	}
	return Default.localNode(other)
}

// localNode looks for the local node's proxy, at most every 30 seconds.
func (r *Router) localNode(other http.RoundTripper) (net.IP, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) < 30*time.Second {
		return r.nodeAddr, r.nodeAddr != nil
	}
	r.checked = time.Now()
	r.nodeAddr = nil
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ProxyAddr()+SelfPath, nil)
	resp, err := other.RoundTrip(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var self struct {
		Address string `json:"address"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&self) != nil {
		return nil, false
	}
	if ip := net.ParseIP(self.Address); IsYggdrasil(ip) {
		r.nodeAddr = ip
		if r.viaNode == nil {
			r.viaNode = &http.Transport{
				Proxy:               http.ProxyURL(&url.URL{Scheme: "http", Host: ProxyAddr()}),
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			}
		}
	}
	return r.nodeAddr, r.nodeAddr != nil
}

// way finds out, and remembers for ten minutes, whether a node reached
// over the system Yggdrasil speaks HTTP/3 or only HTTP over TCP.
func (r *Router) way(ctx context.Context, hostport string) (bool, error) {
	for {
		r.mu.Lock()
		if w, ok := r.ways[hostport]; ok && time.Now().Before(w.until) {
			r.mu.Unlock()
			return w.quic, nil
		}
		if wait, ok := r.probing[hostport]; ok {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		if r.probing == nil {
			r.probing, r.ways = map[string]chan struct{}{}, map[string]way{}
		}
		done := make(chan struct{})
		r.probing[hostport] = done
		r.mu.Unlock()

		quic, err := probe(ctx, hostport)
		r.mu.Lock()
		if err == nil {
			r.ways[hostport] = way{quic: quic, until: time.Now().Add(10 * time.Minute)}
		}
		delete(r.probing, hostport)
		close(done)
		r.mu.Unlock()
		return quic, err
	}
}

// probe tries QUIC and TCP at once, preferring QUIC.
func probe(ctx context.Context, hostport string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	qc, tc := make(chan error, 1), make(chan error, 1)
	go func() {
		conn, err := quic.DialAddr(ctx, hostport, clientTLS(), quicConfig())
		if err == nil {
			conn.CloseWithError(0, "")
		}
		qc <- err
	}()
	go func() {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", hostport)
		if err == nil {
			conn.Close()
		}
		tc <- err
	}()
	var qerr, terr error
	for qc != nil || tc != nil {
		select {
		case qerr = <-qc:
			if qerr == nil {
				return true, nil
			}
			qc = nil
		case terr = <-tc:
			if terr == nil {
				if qc != nil { // a node that has both answers QUIC soon after
					select {
					case qerr = <-qc:
						return qerr == nil, nil
					case <-time.After(time.Second):
					}
				}
				return false, nil
			}
			tc = nil
		}
	}
	return false, fmt.Errorf("%s: no answer over QUIC (%v) or TCP (%v)", hostport, qerr, terr)
}

// ServeSystem answers HTTP/3 on the system Yggdrasil's UDP, next to the
// node's TCP, for nodes with the built-in Yggdrasil.
func ServeSystem(ctx context.Context, ip net.IP, port int, h http.Handler) error {
	pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return err
	}
	qt := &quic.Transport{Conn: pc}
	defer qt.Close()
	ln, err := qt.Listen(serverTLS(), quicConfig())
	if err != nil {
		return err
	}
	return serve(ctx, ln, h)
}

// Proxy takes requests from programs on this machine (absolute-form, as to
// any HTTP proxy) and sends them to other nodes through n. Like the
// system Yggdrasil's TUN device, it lets local programs act as this node.
func (n *Node) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "" {
			if r.URL.Path != SelfPath {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"address": n.addr.String()})
			return
		}
		if r.URL.Scheme != "http" || !IsYggdrasil(net.ParseIP(r.URL.Hostname())) {
			http.Error(w, "only other nodes, on their Yggdrasil addresses", http.StatusForbidden)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.URL.Scheme = "https"
		for _, h := range hopHeaders {
			out.Header.Del(h)
		}
		resp, err := n.rt.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, h := range hopHeaders {
			resp.Header.Del(h)
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		f, _ := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		for {
			k, err := resp.Body.Read(buf)
			if k > 0 {
				if _, werr := w.Write(buf[:k]); werr != nil {
					return
				}
				if f != nil {
					f.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	})
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

var yggNet = &net.IPNet{IP: net.ParseIP("200::"), Mask: net.CIDRMask(7, 128)}

// IsYggdrasil is whether ip is a Yggdrasil address (200::/7).
func IsYggdrasil(ip net.IP) bool { return ip != nil && ip.To4() == nil && yggNet.Contains(ip) }
