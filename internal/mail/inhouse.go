package mail

import (
	"context"
	"fmt"
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
		return false, nil
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

// send delivers in the group what it can, and the rest through the SMTP
// relay. An in-house address that can't be delivered just now goes out
// through the relay too, which brings it back in by Cloudflare as before.
func (r *Relay) send(ctx context.Context, from string, to []string, msg []byte) error {
	deliver := r.Deliver
	if deliver == nil {
		deliver = r.deliver
	}
	if r.InHouse == nil {
		return deliver(ctx, from, to, msg)
	}
	var out, in []string
	for _, a := range to {
		if inhouse, err := r.InHouse(ctx, from, a, msg); !inhouse {
			out = append(out, a)
		} else if err != nil {
			r.logf("mail: %s in the group: %v; sending it out instead", a, err)
			out = append(out, a)
		} else {
			in = append(in, a)
		}
	}
	if len(in) > 0 {
		r.logf("mail: delivered in the group from %s to %d recipient(s)", from, len(in))
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
