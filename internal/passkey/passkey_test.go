package passkey

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"
)

const (
	rp     = "my.example.org"
	origin = "https://my.example.org"
)

// device is a fake authenticator.
type device struct {
	id    []byte
	alg   int
	sign  func(digest, msg []byte) []byte
	spki  []byte
	count uint32
}

func newDevice(t *testing.T, alg int) *device {
	d := &device{id: []byte("cred-" + string(rune('a'+(-alg)%26))), alg: alg}
	var pub crypto.PublicKey
	switch alg {
	case ES256:
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		pub = &k.PublicKey
		d.sign = func(digest, _ []byte) []byte { s, _ := ecdsa.SignASN1(rand.Reader, k, digest); return s }
	case EdDSA:
		p, k, _ := ed25519.GenerateKey(rand.Reader)
		pub = p
		d.sign = func(_, msg []byte) []byte { return ed25519.Sign(k, msg) }
	case RS256:
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		pub = &k.PublicKey
		d.sign = func(digest, _ []byte) []byte {
			s, _ := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest)
			return s
		}
	}
	var err error
	if d.spki, err = x509.MarshalPKIXPublicKey(pub); err != nil {
		t.Fatal(err)
	}
	return d
}

func client(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin})
	return b
}

func (d *device) auth(rpID string, flags byte, withCred bool) []byte {
	h := sha256.Sum256([]byte(rpID))
	b := append(h[:], flags)
	b = binary.BigEndian.AppendUint32(b, d.count)
	if withCred {
		b = append(b, make([]byte, 16)...) // AAGUID
		b = binary.BigEndian.AppendUint16(b, uint16(len(d.id)))
		b = append(b, d.id...)
		b = append(b, 0xa0) // a COSE key we never read
	}
	return b
}

func (d *device) assert(rpID, typ, challenge, origin string) (cj, auth, sig []byte) {
	d.count++
	cj, auth = client(typ, challenge, origin), d.auth(rpID, flagUP|0x04, false)
	h := sha256.Sum256(cj)
	msg := append(append([]byte{}, auth...), h[:]...)
	digest := sha256.Sum256(msg)
	return cj, auth, d.sign(digest[:], msg)
}

func TestPasskeys(t *testing.T) {
	for _, alg := range Algs {
		d := newDevice(t, alg)
		ch := NewChallenge(time.Now())
		id := B64.EncodeToString(d.id)
		k, err := Register(client("webauthn.create", ch, origin), d.auth(rp, flagUP|flagAT, true), d.spki, alg, id, ch, origin, rp)
		if err != nil {
			t.Fatalf("alg %d: register: %v", alg, err)
		}
		for name, bad := range map[string]func() error{
			"wrong challenge": func() error {
				_, err := Register(client("webauthn.create", "x", origin), d.auth(rp, flagUP|flagAT, true), d.spki, alg, id, ch, origin, rp)
				return err
			},
			"wrong id": func() error {
				_, err := Register(client("webauthn.create", ch, origin), d.auth(rp, flagUP|flagAT, true), d.spki, alg, "other", ch, origin, rp)
				return err
			},
			"another site": func() error {
				_, err := Register(client("webauthn.create", ch, origin), d.auth("evil.example", flagUP|flagAT, true), d.spki, alg, id, ch, origin, rp)
				return err
			},
			"wrong alg": func() error {
				_, err := Register(client("webauthn.create", ch, origin), d.auth(rp, flagUP|flagAT, true), d.spki, alg+100, id, ch, origin, rp)
				return err
			},
		} {
			if bad() == nil {
				t.Errorf("alg %d: registered with %s", alg, name)
			}
		}

		ch2 := NewChallenge(time.Now())
		cj, auth, sig := d.assert(rp, "webauthn.get", ch2, origin)
		if n, err := k.Verify(cj, auth, sig, ch2, origin, rp); err != nil || n != d.count {
			t.Fatalf("alg %d: sign in: %v (count %d)", alg, err, n)
		}
		if ChallengeOf(cj) != ch2 {
			t.Fatal("ChallengeOf")
		}
		sig[len(sig)-1] ^= 1
		if _, err := k.Verify(cj, auth, sig, ch2, origin, rp); err == nil {
			t.Errorf("alg %d: a changed signature passed", alg)
		}
		cj, auth, sig = d.assert(rp, "webauthn.get", ch2, "https://evil.example")
		if _, err := k.Verify(cj, auth, sig, ch2, origin, rp); err == nil {
			t.Errorf("alg %d: another origin passed", alg)
		}
		cj, auth, sig = d.assert(rp, "webauthn.create", ch2, origin)
		if _, err := k.Verify(cj, auth, sig, ch2, origin, rp); err == nil {
			t.Errorf("alg %d: a create passed as a get", alg)
		}
		other := newDevice(t, alg)
		cj, auth, sig = other.assert(rp, "webauthn.get", ch2, origin)
		if _, err := k.Verify(cj, auth, sig, ch2, origin, rp); err == nil {
			t.Errorf("alg %d: another device's passkey passed", alg)
		}
		// Not present at the device.
		cj = client("webauthn.get", ch2, origin)
		auth = d.auth(rp, 0, false)
		h := sha256.Sum256(cj)
		msg := append(append([]byte{}, auth...), h[:]...)
		dg := sha256.Sum256(msg)
		if _, err := k.Verify(cj, auth, d.sign(dg[:], msg), ch2, origin, rp); err == nil {
			t.Errorf("alg %d: passed without the user present", alg)
		}
	}
}

func TestChallenges(t *testing.T) {
	var cs Challenges
	now := time.Now()
	c := NewChallenge(now)
	if err := cs.Use(c, now); err != nil {
		t.Fatal(err)
	}
	if cs.Use(c, now) == nil {
		t.Fatal("a challenge was used twice")
	}
	if cs.Use(NewChallenge(now.Add(-6*time.Minute)), now) == nil {
		t.Fatal("an old challenge passed")
	}
	if cs.Use("bm9wZQ", now) == nil || cs.Use(B64.EncodeToString(make([]byte, 24)), now) == nil {
		t.Fatal("a made-up challenge passed")
	}
}
