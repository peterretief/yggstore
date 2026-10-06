// Package yggnet runs Yggdrasil inside yggstore, as yggmail does: no
// Yggdrasil daemon, no TUN device, no root, and no TCP/IP stack. Nodes talk
// HTTP/3, which is QUIC over UDP. On a node with the built-in Yggdrasil the
// UDP datagrams go straight into Yggdrasil's encrypted sessions, wrapped in
// the IPv6 and UDP headers a TUN device would carry, so a node that still
// runs the system daemon reaches it with an ordinary UDP socket on its
// 200::/7 address. Node IDs stay Yggdrasil addresses either way. (yggmail's
// yggquic sends bare QUIC addressed by key instead, which only reaches
// other yggquic programs.)
package yggnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	gologme "github.com/gologme/log"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggdrasil-go/src/multicast"
)

// Config is how a node's built-in Yggdrasil joins the network.
type Config struct {
	Key ed25519.PrivateKey
	// Peers are Yggdrasil nodes to link to, e.g. tls://203.0.113.5:9001.
	Peers []string
	// Listen takes links from other Yggdrasil nodes, e.g. tls://0.0.0.0:9001.
	Listen []string
	// Multicast finds and links to Yggdrasil nodes on the local network.
	Multicast bool
	// Port is the UDP port yggstore uses on this node's address.
	Port int
	Logf func(string, ...any)
}

// Node is a running built-in Yggdrasil with yggstore's QUIC on top.
type Node struct {
	core *core.Core
	mc   *multicast.Multicast
	pc   *packetConn
	qt   *quic.Transport
	rt   *http3.Transport
	addr net.IP
	port int
}

// Start brings the node up. Links to its peers come up in the background.
func Start(cfg Config) (*Node, error) {
	if len(cfg.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("yggnet: no key")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	nc := &config.NodeConfig{PrivateKey: config.KeyBytes(cfg.Key)}
	if err := nc.GenerateSelfSignedCertificate(); err != nil {
		return nil, err
	}
	logger := gologme.New(logWriter(cfg.Logf), "yggdrasil: ", 0)
	logger.EnableLevel("error")
	logger.EnableLevel("warn")
	var opts []core.SetupOption
	for _, l := range cfg.Listen {
		opts = append(opts, core.ListenAddress(l))
	}
	for _, p := range cfg.Peers {
		opts = append(opts, core.Peer{URI: p})
	}
	c, err := core.New(nc.Certificate, logger, opts...)
	if err != nil {
		return nil, fmt.Errorf("yggdrasil: %w", err)
	}
	n := &Node{core: c, addr: c.Address(), port: cfg.Port}
	if cfg.Multicast {
		mc, err := multicast.New(c, logger, multicast.MulticastInterface{Regex: regexp.MustCompile(".*"), Beacon: true, Listen: true})
		if err != nil {
			cfg.Logf("yggdrasil: not looking for nodes on the local network: %v", err)
		} else {
			n.mc = mc
		}
	}
	n.pc = newPacketConn(c, cfg.Port)
	n.qt = &quic.Transport{Conn: n.pc}
	n.rt = &http3.Transport{
		TLSClientConfig: clientTLS(),
		QUICConfig:      quicConfig(),
		Dial: func(ctx context.Context, addr string, tlsCfg *tls.Config, qcfg *quic.Config) (*quic.Conn, error) {
			ua, err := net.ResolveUDPAddr("udp6", addr)
			if err != nil {
				return nil, err
			}
			return n.qt.Dial(ctx, ua, tlsCfg, qcfg)
		},
	}
	return n, nil
}

// Addr is the node's Yggdrasil address, its node ID.
func (n *Node) Addr() net.IP { return append(net.IP(nil), n.addr...) }

// PublicKey is the node's Yggdrasil key.
func (n *Node) PublicKey() ed25519.PublicKey { return n.core.PublicKey() }

// RoundTripper makes requests to other nodes through this one.
func (n *Node) RoundTripper() http.RoundTripper { return n.rt }

// Serve answers other nodes' requests with h until ctx ends.
func (n *Node) Serve(ctx context.Context, h http.Handler) error {
	ln, err := n.qt.Listen(serverTLS(), quicConfig())
	if err != nil {
		return err
	}
	return serve(ctx, ln, h)
}

func serve(ctx context.Context, ln http3.QUICListener, h http.Handler) error {
	srv := &http3.Server{Handler: h}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		ln.Close()
	}()
	err := srv.ServeListener(ln)
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, quic.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the node.
func (n *Node) Close() error {
	n.rt.Close()
	n.qt.Close()
	if n.mc != nil {
		n.mc.Stop()
	}
	n.core.Stop()
	return n.pc.Close()
}

// LoadKey reads a key file: this package's (the key in hex), or a Yggdrasil
// config, so a machine can drop its daemon and keep its address. A missing
// file is created with a new key when create is set.
func LoadKey(path string, create bool) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
			f.Close()
			return nil, err
		}
		return key, f.Close()
	} else if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(b))
	if m := confKey.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s: not a Yggdrasil private key", path)
	}
	return ed25519.PrivateKey(key), nil
}

// confKey finds the key in yggdrasil.conf (HJSON or JSON).
var confKey = regexp.MustCompile(`"?PrivateKey"?\s*:\s*"?([0-9a-fA-F]{128})`)

// AddrForKey is the Yggdrasil address a key has.
func AddrForKey(key ed25519.PublicKey) net.IP {
	a := address.AddrForKey(key)
	return append(net.IP(nil), a[:]...)
}

func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:  time.Minute,
		KeepAlivePeriod: 20 * time.Second,
		// Yggdrasil carries whole datagrams; there is no path MTU to find.
		DisablePathMTUDiscovery: true,
	}
}

// TLS is only there because QUIC needs it: who is calling is the Yggdrasil
// address, which only the key's owner can send from, as with TCP.
func serverTLS() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{selfSigned()}, NextProtos: []string{http3.NextProtoH3}}
}

func clientTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}}
}

var (
	certOnce sync.Once
	cert     tls.Certificate
)

func selfSigned() tls.Certificate {
	certOnce.Do(func() {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0)}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
		if err != nil {
			panic(err)
		}
		cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	})
	return cert
}

type logWriter func(string, ...any)

func (l logWriter) Write(p []byte) (int, error) {
	l("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Links are the node's Yggdrasil peerings, for the mesh.
type Link struct {
	URI       string
	Up        bool
	Inbound   bool
	Address   string
	Key       string
	LastError string
}

func (n *Node) Links() []Link {
	var out []Link
	for _, p := range n.core.GetPeers() {
		l := Link{URI: p.URI, Up: p.Up, Inbound: p.Inbound}
		if len(p.Key) == ed25519.PublicKeySize {
			l.Key = hex.EncodeToString(p.Key)
			l.Address = AddrForKey(p.Key).String()
		}
		if p.LastError != nil {
			l.LastError = p.LastError.Error()
		}
		out = append(out, l)
	}
	return out
}

// AddLink peers with uri; added is false if the node had it already.
func (n *Node) AddLink(uri string) (added bool, err error) {
	u, err := url.Parse(uri)
	if err != nil {
		return false, err
	}
	err = n.core.AddPeer(u, "")
	if errors.Is(err, core.ErrLinkAlreadyConfigured) {
		return false, nil
	}
	return err == nil, err
}

func (n *Node) RemoveLink(uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return err
	}
	if err := n.core.RemovePeer(u, ""); err != nil && !errors.Is(err, core.ErrLinkNotConfigured) {
		return err
	}
	return nil
}
