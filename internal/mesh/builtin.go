package mesh

import (
	"context"
	"encoding/hex"

	"github.com/peterretief/yggstore/internal/yggnet"
)

// Builtin is a node's built-in Yggdrasil, which needs no admin socket.
type Builtin struct{ Node *yggnet.Node }

func (b Builtin) Self(context.Context) (Self, error) {
	return Self{Key: hex.EncodeToString(b.Node.PublicKey()), Address: b.Node.Addr().String()}, nil
}

func (b Builtin) Links(context.Context) ([]Link, error) {
	var out []Link
	for _, l := range b.Node.Links() {
		out = append(out, Link{URI: l.URI, Up: l.Up, Inbound: l.Inbound, Address: l.Address, Key: l.Key, LastError: l.LastError})
	}
	return out, nil
}

func (b Builtin) AddLink(_ context.Context, uri string) (bool, error) { return b.Node.AddLink(uri) }

func (b Builtin) RemoveLink(_ context.Context, uri string) error { return b.Node.RemoveLink(uri) }
