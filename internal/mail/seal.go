// Package mail brings the group's email in from Cloudflare.
//
// Cloudflare Email Routing hands each message to a Worker (see mail/worker),
// which seals it for the recipient's sharing code and posts it through the
// web nodes' tunnel. The web node that takes it stores the sealed message in
// the group and tells the recipient's node, which keeps a copy of its own and
// then lets the web node delete the first one. Only the recipient can open
// the message; the web nodes and the members holding shards see ciphertext.
package mail

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"

	"github.com/peterretief/yggstore/internal/share"
)

// A sealed message is magic, an ephemeral X25519 public key, a nonce, then
// the message encrypted with AES-256-GCM under a key derived from the
// ephemeral key and the recipient's. The Worker makes the same (worker.js).
const (
	magic   = "ygm1"
	keyLen  = 32
	nonceLn = 12
	hdrLen  = len(magic) + keyLen + nonceLn
	info    = "yggstore mail v1"
)

func aeadFor(shared, eph, to []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, shared, append(append([]byte{}, eph...), to...), info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts a message for the holder of a sharing code.
func Seal(code string, raw []byte) ([]byte, error) {
	to, err := share.ParseCode(code)
	if err != nil {
		return nil, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(to)
	if err != nil {
		return nil, err
	}
	aead, err := aeadFor(shared, eph.PublicKey().Bytes(), to.Bytes())
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, hdrLen+len(raw)+aead.Overhead())
	out = append(out, magic...)
	out = append(out, eph.PublicKey().Bytes()...)
	nonce := make([]byte, nonceLn)
	rand.Read(nonce)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, raw, out[:hdrLen]), nil
}

// IsSealed reports whether data looks like a sealed message.
func IsSealed(data []byte) bool {
	return len(data) > hdrLen && bytes.HasPrefix(data, []byte(magic))
}

// ErrNotForYou means the message was sealed for another sharing code, or
// was damaged.
var ErrNotForYou = errors.New("this message is damaged or was sealed for someone else")

// Open decrypts a sealed message with the recipient's identity.
func Open(id *share.Identity, data []byte) ([]byte, error) {
	if !IsSealed(data) {
		return nil, errors.New("not a sealed message")
	}
	eph, err := ecdh.X25519().NewPublicKey(data[len(magic) : len(magic)+keyLen])
	if err != nil {
		return nil, ErrNotForYou
	}
	shared, err := id.Agree(eph)
	if err != nil {
		return nil, ErrNotForYou
	}
	aead, err := aeadFor(shared, eph.Bytes(), id.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	raw, err := aead.Open(nil, data[len(magic)+keyLen:hdrLen], data[hdrLen:], data[:hdrLen])
	if err != nil {
		return nil, ErrNotForYou
	}
	return raw, nil
}
