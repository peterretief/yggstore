// Package passkey checks passkeys (WebAuthn): making one, and signing in
// with one. It uses only the standard library. The browser gives the
// passkey's public key in the usual SubjectPublicKeyInfo form
// (AuthenticatorAttestationResponse.getPublicKey), so no CBOR is needed,
// and attestation isn't checked: making a passkey is only allowed with an
// invite, so who made it matters, not which device it's on.
package passkey

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// The COSE algorithms a passkey may use.
const (
	ES256 = -7
	EdDSA = -8
	RS256 = -257
)

// Algs are the algorithms offered when making a passkey, in order of
// preference.
var Algs = []int{ES256, EdDSA, RS256}

// B64 is the encoding WebAuthn uses for binary values in JSON.
var B64 = base64.RawURLEncoding

// Key is a passkey's public half, as kept by the server.
type Key struct {
	ID   string `json:"id"`   // the credential ID, B64
	SPKI []byte `json:"spki"` // the public key
	Alg  int    `json:"alg"`
}

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

// authData is the authenticator data's fixed start.
type authData struct {
	rpIDHash [32]byte
	flags    byte
	count    uint32
	credID   []byte // only when a passkey is made
}

const (
	flagUP = 0x01 // the user was present
	flagAT = 0x40 // attested credential data follows
)

func parseAuthData(b []byte) (authData, error) {
	var a authData
	if len(b) < 37 {
		return a, errors.New("the authenticator data is too short")
	}
	copy(a.rpIDHash[:], b[:32])
	a.flags = b[32]
	a.count = binary.BigEndian.Uint32(b[33:37])
	if a.flags&flagAT != 0 {
		rest := b[37:]
		if len(rest) < 18 {
			return a, errors.New("the attested credential data is too short")
		}
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		if len(rest) < 18+n {
			return a, errors.New("the credential ID is cut short")
		}
		a.credID = rest[18 : 18+n]
	}
	return a, nil
}

// check verifies what both ceremonies share: the client data's type,
// challenge and origin, and the authenticator data's site and presence.
func check(clientJSON, auth []byte, typ, challenge, origin, rpID string) (authData, error) {
	var c clientData
	if err := json.Unmarshal(clientJSON, &c); err != nil {
		return authData{}, errors.New("the client data isn't JSON")
	}
	switch {
	case c.Type != typ:
		return authData{}, fmt.Errorf("the client data is for %q, not %q", c.Type, typ)
	case c.Challenge != challenge:
		return authData{}, errors.New("the challenge doesn't match")
	case c.Origin != origin:
		return authData{}, fmt.Errorf("the passkey was used on %s, not %s", c.Origin, origin)
	case c.CrossOrigin:
		return authData{}, errors.New("the passkey was used from inside another site")
	}
	a, err := parseAuthData(auth)
	if err != nil {
		return a, err
	}
	if want := sha256.Sum256([]byte(rpID)); a.rpIDHash != want {
		return a, fmt.Errorf("the passkey is for another site than %s", rpID)
	}
	if a.flags&flagUP == 0 {
		return a, errors.New("nobody was present at the device")
	}
	return a, nil
}

// ChallengeOf is the challenge the client data carries, unchecked, so the
// caller can look it up before Verify.
func ChallengeOf(clientJSON []byte) string {
	var c clientData
	json.Unmarshal(clientJSON, &c)
	return c.Challenge
}

// Register checks a new passkey: made just now, for this site, with this
// challenge, and that the key the browser gave is usable. id is the
// credential ID (B64) the browser reported.
func Register(clientJSON, auth, spki []byte, alg int, id, challenge, origin, rpID string) (Key, error) {
	a, err := check(clientJSON, auth, "webauthn.create", challenge, origin, rpID)
	if err != nil {
		return Key{}, err
	}
	if a.credID == nil || B64.EncodeToString(a.credID) != id {
		return Key{}, errors.New("the credential ID doesn't match the authenticator data")
	}
	k := Key{ID: id, SPKI: spki, Alg: alg}
	if _, err := k.public(); err != nil {
		return Key{}, err
	}
	return k, nil
}

func (k Key) public() (crypto.PublicKey, error) {
	pub, err := x509.ParsePKIXPublicKey(k.SPKI)
	if err != nil {
		return nil, fmt.Errorf("the passkey's public key: %w", err)
	}
	ok := false
	switch pub.(type) {
	case *ecdsa.PublicKey:
		ok = k.Alg == ES256
	case ed25519.PublicKey:
		ok = k.Alg == EdDSA
	case *rsa.PublicKey:
		ok = k.Alg == RS256
	}
	if !ok {
		return nil, fmt.Errorf("a passkey with algorithm %d and a %T key isn't supported", k.Alg, pub)
	}
	return pub, nil
}

// Verify checks a sign-in with k: for this site, with this challenge, and
// signed by the passkey. It gives the device's signature counter, which a
// caller may keep to spot a cloned key (0 means the device keeps none).
func (k Key) Verify(clientJSON, auth, sig []byte, challenge, origin, rpID string) (uint32, error) {
	a, err := check(clientJSON, auth, "webauthn.get", challenge, origin, rpID)
	if err != nil {
		return 0, err
	}
	pub, err := k.public()
	if err != nil {
		return 0, err
	}
	h := sha256.Sum256(clientJSON)
	signed := append(append([]byte{}, auth...), h[:]...)
	digest := sha256.Sum256(signed)
	good := false
	switch pk := pub.(type) {
	case *ecdsa.PublicKey:
		good = ecdsa.VerifyASN1(pk, digest[:], sig)
	case ed25519.PublicKey:
		good = ed25519.Verify(pk, signed, sig)
	case *rsa.PublicKey:
		good = rsa.VerifyPKCS1v15(pk, crypto.SHA256, digest[:], sig) == nil
	}
	if !good {
		return 0, errors.New("the passkey's signature doesn't match")
	}
	return a.count, nil
}

// Challenges are one-time challenges that carry their time, so any server
// can check one it didn't issue (a web node may issue one for a box to
// check). Each is good for Life and only once.
type Challenges struct {
	mu   sync.Mutex
	used map[string]time.Time
}

// Life is how long a challenge is good for.
const Life = 5 * time.Minute

// New is a fresh challenge: the time and 16 random bytes, B64.
func NewChallenge(now time.Time) string {
	b := make([]byte, 24)
	binary.BigEndian.PutUint64(b, uint64(now.Unix()))
	rand.Read(b[8:])
	return B64.EncodeToString(b)
}

// Use checks that c is a fresh challenge not used before, and uses it.
func (cs *Challenges) Use(c string, now time.Time) error {
	b, err := B64.DecodeString(c)
	if err != nil || len(b) != 24 || bytes.Equal(b[8:], make([]byte, 16)) {
		return errors.New("not a challenge")
	}
	at := time.Unix(int64(binary.BigEndian.Uint64(b)), 0)
	if now.Sub(at) > Life || at.Sub(now) > time.Minute {
		return errors.New("the challenge is too old: try again")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.used == nil {
		cs.used = map[string]time.Time{}
	}
	for k, t := range cs.used {
		if now.Sub(t) > 2*Life {
			delete(cs.used, k)
		}
	}
	if _, seen := cs.used[c]; seen {
		return errors.New("the challenge was used already: try again")
	}
	cs.used[c] = now
	return nil
}

// A browser key is the fallback for a device that can't make a passkey,
// such as a phone without a screen lock: an ECDSA P-256 key the page makes
// with WebCrypto and keeps in the browser (IndexedDB), where it can't be
// copied out. It proves who someone is only by being in that browser: no
// lock guards it.

// BrowserMessage is what a browser key signs: the address it is used at
// and the challenge, so a signature can't be used anywhere else.
func BrowserMessage(origin, challenge string) []byte {
	return []byte("yggstore browser key\n" + origin + "\n" + challenge)
}

// VerifyBrowser checks a browser key's signature (WebCrypto's form: r and
// s, 32 bytes each) of BrowserMessage.
func VerifyBrowser(spki []byte, origin, challenge string, sig []byte) error {
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return fmt.Errorf("the browser key: %w", err)
	}
	pk, ok := pub.(*ecdsa.PublicKey)
	if !ok || pk.Curve.Params().Name != "P-256" {
		return errors.New("a browser key must be ECDSA P-256")
	}
	if len(sig) != 64 {
		return errors.New("the browser key's signature is the wrong size")
	}
	h := sha256.Sum256(BrowserMessage(origin, challenge))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pk, h[:], r, s) {
		return errors.New("the browser key's signature doesn't match")
	}
	return nil
}
