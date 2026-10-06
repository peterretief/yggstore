package yggnet

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	iwt "github.com/Arceliar/ironwood/types"
	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
)

// packetConn is a UDP socket on the built-in Yggdrasil: one port on this
// node's address. Each datagram travels in Yggdrasil's encrypted session to
// the key that owns the other address, inside the IPv6 and UDP headers a TUN
// device would add, so the system daemon on the other end hands it to an
// ordinary UDP socket.
type packetConn struct {
	core *core.Core
	self address.Address
	port int

	in     chan datagram
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	keys     map[address.Address]ed25519.PublicKey
	waiting  map[address.Address][][]byte // packets for addresses being looked up
	deadline time.Time
	wake     chan struct{} // closed when the deadline changes
}

type datagram struct {
	data []byte
	from *net.UDPAddr
}

const (
	ipv6Header = 40
	udpHeader  = 8
	udpProto   = 17
	// maxWaiting is how many packets are kept per address while its key is
	// looked up; QUIC sends again if they are lost.
	maxWaiting = 8
)

func newPacketConn(c *core.Core, port int) *packetConn {
	pc := &packetConn{
		core:    c,
		self:    *address.AddrForKey(c.PublicKey()),
		port:    port,
		in:      make(chan datagram, 256),
		closed:  make(chan struct{}),
		keys:    map[address.Address]ed25519.PublicKey{},
		waiting: map[address.Address][][]byte{},
		wake:    make(chan struct{}),
	}
	c.SetPathNotify(pc.found)
	go pc.read()
	return pc
}

// read takes packets from Yggdrasil and keeps the UDP ones for this port.
func (pc *packetConn) read() {
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.core.ReadFrom(buf)
		if err != nil {
			pc.Close()
			return
		}
		key, ok := from.(iwt.Addr)
		if !ok {
			continue
		}
		src, port, payload, ok := pc.parse(buf[:n])
		if !ok || src != *address.AddrForKey(ed25519.PublicKey(key)) {
			continue // not UDP to us, or not from the key's own address
		}
		pc.learn(src, ed25519.PublicKey(key))
		d := datagram{data: append([]byte(nil), payload...), from: &net.UDPAddr{IP: append(net.IP(nil), src[:]...), Port: port}}
		select {
		case pc.in <- d:
		case <-pc.closed:
			return
		default: // full: drop it, as a busy socket would
		}
	}
}

// parse checks an IPv6 packet is UDP to this node's port and returns its
// source and payload. Yggdrasil's sessions are authenticated, so the UDP
// checksum isn't checked.
func (pc *packetConn) parse(b []byte) (src address.Address, port int, payload []byte, ok bool) {
	if len(b) < ipv6Header+udpHeader || b[0]>>4 != 6 || b[6] != udpProto {
		return src, 0, nil, false
	}
	if !bytes.Equal(b[24:40], pc.self[:]) {
		return src, 0, nil, false
	}
	plen := int(binary.BigEndian.Uint16(b[4:6]))
	if plen < udpHeader || ipv6Header+plen > len(b) {
		return src, 0, nil, false
	}
	u := b[ipv6Header : ipv6Header+plen]
	if int(binary.BigEndian.Uint16(u[2:4])) != pc.port || int(binary.BigEndian.Uint16(u[4:6])) != plen {
		return src, 0, nil, false
	}
	copy(src[:], b[8:24])
	return src, int(binary.BigEndian.Uint16(u[0:2])), u[udpHeader:], true
}

// packet wraps a UDP payload in IPv6 and UDP headers.
func packet(src, dst address.Address, sport, dport int, payload []byte) []byte {
	ulen := udpHeader + len(payload)
	b := make([]byte, ipv6Header+ulen)
	b[0] = 6 << 4
	binary.BigEndian.PutUint16(b[4:6], uint16(ulen))
	b[6] = udpProto
	b[7] = 64 // hop limit
	copy(b[8:24], src[:])
	copy(b[24:40], dst[:])
	u := b[ipv6Header:]
	binary.BigEndian.PutUint16(u[0:2], uint16(sport))
	binary.BigEndian.PutUint16(u[2:4], uint16(dport))
	binary.BigEndian.PutUint16(u[4:6], uint16(ulen))
	copy(u[udpHeader:], payload)
	binary.BigEndian.PutUint16(u[6:8], udpChecksum(src, dst, u))
	return b
}

// udpChecksum is the UDP checksum over IPv6, which the receiving kernel
// insists on (RFC 8200 section 8.1).
func udpChecksum(src, dst address.Address, udp []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	add(src[:])
	add(dst[:])
	sum += uint32(len(udp)) + udpProto
	add(udp[:6]) // the checksum field itself counts as zero
	add(udp[8:])
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	c := ^uint16(sum)
	if c == 0 {
		c = 0xffff
	}
	return c
}

func (pc *packetConn) learn(a address.Address, key ed25519.PublicKey) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if _, ok := pc.keys[a]; !ok {
		pc.keys[a] = append(ed25519.PublicKey(nil), key...)
	}
}

// found is Yggdrasil telling us it has a path to key, after a lookup.
func (pc *packetConn) found(key ed25519.PublicKey) {
	a := *address.AddrForKey(key)
	pc.mu.Lock()
	pc.keys[a] = append(ed25519.PublicKey(nil), key...)
	queued := pc.waiting[a]
	delete(pc.waiting, a)
	pc.mu.Unlock()
	for _, p := range queued {
		pc.core.WriteTo(p, iwt.Addr(key))
	}
}

func (pc *packetConn) WriteTo(p []byte, to net.Addr) (int, error) {
	select {
	case <-pc.closed:
		return 0, net.ErrClosed
	default:
	}
	ua, ok := to.(*net.UDPAddr)
	if !ok {
		return 0, errors.New("yggnet: not a UDP address")
	}
	var dst address.Address
	if ua.IP.To4() != nil || len(ua.IP) != net.IPv6len {
		return 0, errors.New("yggnet: not a Yggdrasil address")
	}
	if copy(dst[:], ua.IP); !dst.IsValid() {
		return 0, errors.New("yggnet: not a Yggdrasil address")
	}
	if dst == pc.self {
		// To itself, as a kernel loops back packets to its own address.
		if ua.Port == pc.port {
			select {
			case pc.in <- datagram{data: append([]byte(nil), p...), from: &net.UDPAddr{IP: append(net.IP(nil), pc.self[:]...), Port: pc.port}}:
			default:
			}
		}
		return len(p), nil
	}
	b := packet(pc.self, dst, pc.port, ua.Port, p)
	pc.mu.Lock()
	key, ok := pc.keys[dst]
	if !ok {
		if q := pc.waiting[dst]; len(q) < maxWaiting {
			pc.waiting[dst] = append(q, b)
		}
		pc.mu.Unlock()
		pc.core.SendLookup(dst.GetKey())
		return len(p), nil
	}
	pc.mu.Unlock()
	if _, err := pc.core.WriteTo(b, iwt.Addr(key)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (pc *packetConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		pc.mu.Lock()
		deadline, wake := pc.deadline, pc.wake
		pc.mu.Unlock()
		var timeout <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}
		stop := func() {
			if timer != nil {
				timer.Stop()
			}
		}
		select {
		case d := <-pc.in:
			stop()
			return copy(p, d.data), d.from, nil
		case <-pc.closed:
			stop()
			return 0, nil, net.ErrClosed
		case <-timeout:
			return 0, nil, os.ErrDeadlineExceeded
		case <-wake:
			stop()
		}
	}
}

func (pc *packetConn) SetReadDeadline(t time.Time) error {
	pc.mu.Lock()
	pc.deadline = t
	close(pc.wake)
	pc.wake = make(chan struct{})
	pc.mu.Unlock()
	return nil
}

func (pc *packetConn) SetDeadline(t time.Time) error      { return pc.SetReadDeadline(t) }
func (pc *packetConn) SetWriteDeadline(t time.Time) error { return nil }

// The buffer is the in channel; these keep quic-go from asking for a kernel
// socket's.
func (pc *packetConn) SetReadBuffer(int) error  { return nil }
func (pc *packetConn) SetWriteBuffer(int) error { return nil }
func (pc *packetConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: append(net.IP(nil), pc.self[:]...), Port: pc.port}
}

func (pc *packetConn) Close() error {
	pc.once.Do(func() { close(pc.closed) })
	return nil
}
