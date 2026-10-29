package mesh

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/yggdrasil-network/yggdrasil-go/src/address"
)

// KeyAddress is the Yggdrasil address a public key (hex) gives, or "" if it
// isn't one.
func KeyAddress(key string) string {
	b, err := hex.DecodeString(key)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return ""
	}
	a := address.AddrForKey(ed25519.PublicKey(b))
	if a == nil {
		return ""
	}
	return net.IP(a[:]).String()
}

// Allowed works out the AllowedPublicKeys of a machine that members link
// to, so the list follows the member list. current is the list now, found
// the keys members reported (a key counts only if it gives a member's
// address, so a node can't vouch for another), and added the keys an
// earlier run put in. A member's key that's missing is added. A key this
// added before is dropped once its address is no member's; keys put in by
// hand are never dropped. It returns the new list and the keys it added.
func Allowed(current []string, members []peers.Peer, found []string, added []string) (next, nowAdded []string) {
	member := map[string]bool{}
	for _, p := range members {
		if ip := normal(p.IP()); ip != "" {
			member[ip] = true
		}
	}
	byUs := map[string]bool{}
	for _, k := range added {
		byUs[k] = true
	}
	have := map[string]bool{}
	for _, k := range current {
		if have[k] || byUs[k] && !member[KeyAddress(k)] {
			continue
		}
		have[k] = true
		next = append(next, k)
		if byUs[k] {
			nowAdded = append(nowAdded, k)
		}
	}
	for _, k := range found {
		if !have[k] && member[KeyAddress(k)] {
			have[k] = true
			next = append(next, k)
			nowAdded = append(nowAdded, k)
		}
	}
	sort.Strings(nowAdded)
	return next, nowAdded
}

// AllowConf reads and writes the AllowedPublicKeys of a yggdrasil.conf in
// JSON (Yggdrasil also takes HJSON, with comments; such a file is refused,
// not rewritten). Other settings are kept.
type AllowConf struct {
	Path string
	conf map[string]json.RawMessage
	fi   os.FileInfo
}

func (c *AllowConf) Keys() ([]string, error) {
	b, err := os.ReadFile(c.Path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(c.Path)
	if err != nil {
		return nil, err
	}
	c.fi = fi
	c.conf = nil
	if err := json.Unmarshal(b, &c.conf); err != nil {
		return nil, fmt.Errorf("%s isn't plain JSON, so it's left alone: %w", c.Path, err)
	}
	var keys []string
	if raw, ok := c.conf["AllowedPublicKeys"]; ok {
		if err := json.Unmarshal(raw, &keys); err != nil {
			return nil, fmt.Errorf("%s: AllowedPublicKeys: %w", c.Path, err)
		}
	}
	if len(keys) == 0 {
		// An empty list lets anyone link; filling it would shut out
		// everyone not in the group, which is the owner's call.
		return nil, errors.New(c.Path + " lets anyone link (no AllowedPublicKeys), so it's left alone")
	}
	return keys, nil
}

// SetKeys writes keys as the AllowedPublicKeys, after Keys.
func (c *AllowConf) SetKeys(keys []string) error {
	if c.conf == nil {
		return errors.New("read the keys first")
	}
	raw, _ := json.Marshal(keys)
	c.conf["AllowedPublicKeys"] = raw
	b, err := json.MarshalIndent(c.conf, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Replace(c.Path, append(b, '\n'), c.fi.Mode().Perm()); err != nil {
		return err
	}
	return keepOwner(c.Path, c.fi)
}
