package mail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
)

// In-house mail: a message from a member to an address the group gives
// (a name at the group's domain, or at a client's own domain) doesn't go
// out to the internet and back through Cloudflare. The web node that would
// have relayed it seals it for the recipient and stores it in the group,
// as it does with mail the Worker brings in, so it never leaves the group's
// machines. Addresses that aren't the group's go out through the relay as
// before, and so does an in-house one that can't be delivered just now.

// DeliverInHouse seals a message for the address's holder and stores it for
// their node, with the headers the Worker would add. It reports false when
// the address isn't one the group gives, so the caller sends it out instead.
func (n *Node) DeliverInHouse(ctx context.Context, from, to string, raw []byte) (bool, error) {
	if n.Peers == nil || n.Msgs == nil {
		return false, nil
	}
	p, holder, ok := peers.MailHolder(n.Peers(), to)
	if !ok || p.Gateway || holder.Code == "" {
		// A person's local address on their box.
		if p, holder.Code, ok = n.localHolder(to); !ok {
			return false, nil
		}
	}
	clean := func(s string) string { return strings.NewReplacer("\r", "", "\n", "", "<", "", ">", "").Replace(s) }
	added := fmt.Sprintf("Delivered-To: %s\r\nReturn-Path: <%s>\r\nReceived: by %s (yggstore, in the group); %s\r\n",
		clean(strings.ToLower(to)), clean(from), clean(n.Self), time.Now().Format(time.RFC1123Z))
	sealed, err := Seal(holder.Code, append([]byte(added), raw...))
	if err != nil {
		return true, fmt.Errorf("sealing for %s: %w", to, err)
	}
	if _, err := n.take(ctx, p.IP(), sealed, strings.ToLower(from)); err != nil {
		return true, err
	}
	return true, nil
}

// localHolder is the box a local address is on, and the code its person's
// mail is sealed for.
func (n *Node) localHolder(addr string) (peers.Peer, string, bool) {
	if n.LocalCode == nil {
		return peers.Peer{}, "", false
	}
	p, local, ok := peers.LocalBox(n.Peers(), addr)
	if !ok {
		return peers.Peer{}, "", false
	}
	code, ok := n.LocalCode(p.Name, local)
	return p, code, ok
}

// IsLocal says whether an address is a box person's local address that this
// web node can deliver to.
func (n *Node) IsLocal(addr string) bool {
	_, _, ok := n.localHolder(addr)
	return ok
}

// send delivers in the group what it can, and the rest through the SMTP
// relay. An in-house address that can't be delivered just now goes out
// through the relay too, which brings it back in by Cloudflare as before;
// except an internal one (peers.InternalDomain), or anything from one:
// those never leave the group, and fail instead.
func (r *Relay) send(ctx context.Context, from string, to []string, msg []byte) error {
	deliver := r.Deliver
	if deliver == nil {
		deliver = r.deliver
	}
	var list []peers.Peer
	if r.Peers != nil {
		list = r.Peers()
	}
	internal := func(a string) bool { return peers.IsInternal(list, a) }
	fromInside := internal(from)
	inGroup := func(a string) bool {
		if p, _, ok := peers.MailHolder(list, a); ok && !p.Gateway {
			return true
		}
		return r.Local != nil && r.Local(a)
	}
	for _, a := range to {
		switch {
		case internal(a) && !inGroup(a):
			return fmt.Errorf("%s: no such address in the group", a)
		case fromInside && !inGroup(a):
			return fmt.Errorf("mail from %s stays in the group: %s isn't one of its addresses", from, a)
		}
	}
	if r.InHouse == nil {
		if fromInside || slices.ContainsFunc(to, internal) {
			return errors.New("this web node can't deliver mail in the group, and internal mail never goes out")
		}
		return deliver(ctx, from, to, msg)
	}
	var out, in, failed []string
	for _, a := range to {
		inhouse, err := r.InHouse(ctx, from, a, msg)
		switch {
		case inhouse && err == nil:
			in = append(in, a)
		case inhouse && (fromInside || internal(a)):
			r.logf("mail: %s in the group: %v", a, err)
			failed = append(failed, a)
		case inhouse:
			r.logf("mail: %s in the group: %v; sending it out instead", a, err)
			out = append(out, a)
		default:
			out = append(out, a)
		}
	}
	if len(in) > 0 {
		r.logf("mail: delivered in the group from %s to %d recipient(s)", from, len(in))
	}
	if len(failed) > 0 {
		err := fmt.Errorf("can't reach %s in the group just now; internal mail never goes out, so try again later", strings.Join(failed, ", "))
		if len(in) > 0 {
			err = fmt.Errorf("%w (it did reach %s)", err, strings.Join(in, ", "))
		}
		return err
	}
	if len(out) == 0 {
		return nil
	}
	if err := deliver(ctx, from, out, msg); err != nil {
		if len(in) > 0 {
			return fmt.Errorf("%w (it did reach %s, in the group)", err, strings.Join(in, ", "))
		}
		return err
	}
	return nil
}
