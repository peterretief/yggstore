// Package lease lets whoever holds a stub say its shards are still wanted.
//
// A stub's key gives a token (Token), and the token a lease ID (ID). An
// upload tells each node the lease ID of the shards it stores; later,
// anyone holding the stub (its writer, someone it was shared with, a
// backup restored on a new node) renews the lease by showing the token.
// The node can check it against the ID but can't make it up, so a lease
// that goes unrenewed for months means nobody has the stub any more, or
// nobody who has it is about: the shards can't be decrypted, or won't be.
//
// Nodes only keep track; nothing is deleted for want of renewal.
package lease

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Header carries a shard's lease ID on upload.
const Header = "X-Yggstore-Lease"

// Token is the renewal token for items stored with key. Every version of
// an item shares its key, so one token renews them all.
func Token(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("yggstore lease\x00"))
	return hex.EncodeToString(mac.Sum(nil))
}

// ID is the lease a token renews: what nodes keep beside the shards.
func ID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ForKey is the lease ID for items stored with key ("" without one).
func ForKey(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	return ID(Token(key))
}

// Valid is whether s is a token or ID: 64 lower-case hex digits (IDs name
// files on nodes).
func Valid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
