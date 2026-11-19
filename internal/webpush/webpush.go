// Package webpush sends notices to browsers that asked for them (the
// browser's own notifications, on a phone or a computer), as RFC 8030 web
// push: the message is encrypted for the browser (RFC 8291, aes128gcm) and
// the push service is told who sends it (RFC 8292, VAPID). The push
// service (Google's, Mozilla's, Apple's) only passes it on, and can't read it.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var b64 = base64.RawURLEncoding

// Subscription is what a browser gives when it agrees to notices.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Check says whether a subscription is one a push service gave: an https
// endpoint, and keys of the right sizes.
func (s Subscription) Check() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(s.Endpoint) > 2048 {
		return errors.New("not a push service's address")
	}
	if k, err := decode(s.Keys.P256dh); err != nil || len(k) != 65 {
		return errors.New("not a browser's key")
	}
	if a, err := decode(s.Keys.Auth); err != nil || len(a) != 16 {
		return errors.New("not a browser's secret")
	}
	return nil
}

func decode(s string) ([]byte, error) {
	return b64.DecodeString(strings.TrimRight(s, "="))
}

// Sender is the key a sender signs with; browsers subscribe with its
// public half.
type Sender struct {
	key     *ecdsa.PrivateKey
	Contact string // RFC 8292's sub: a mailto: or https: address
	Client  *http.Client
}

// LoadOrCreate reads the sender's key, or makes one.
func LoadOrCreate(path, contact string) (*Sender, error) {
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: not a key", path)
		}
		k, err := x509.ParseECPrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		return &Sender{key: k, Contact: contact}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalECPrivateKey(k)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return &Sender{key: k, Contact: contact}, nil
}

// PublicKey is what a browser subscribes with (applicationServerKey).
func (s *Sender) PublicKey() string {
	pub, _ := s.key.PublicKey.ECDH()
	return b64.EncodeToString(pub.Bytes())
}

// ErrGone means the browser no longer takes notices: forget the
// subscription.
var ErrGone = errors.New("the browser no longer takes notices")

// Send encrypts payload for the subscription and gives it to its push
// service, to keep for up to ttl if the device is off.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte, ttl time.Duration) error {
	if err := sub.Check(); err != nil {
		return err
	}
	ua, _ := decode(sub.Keys.P256dh)
	auth, _ := decode(sub.Keys.Auth)
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	body, err := encrypt(payload, ua, auth, as, salt)
	if err != nil {
		return err
	}
	u, _ := url.Parse(sub.Endpoint)
	jwt, err := s.vapid(u.Scheme+"://"+u.Host, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", fmt.Sprint(int(ttl.Seconds())))
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+s.PublicKey())
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("the push service answered %s", resp.Status)
	}
	return nil
}

// vapid is the token that tells the push service who sends (RFC 8292).
func (s *Sender) vapid(aud string, now time.Time) (string, error) {
	head, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256"})
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": now.Add(12 * time.Hour).Unix(), "sub": s.Contact})
	signing := b64.EncodeToString(head) + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

func hkdf(salt, ikm, info []byte, n int) []byte {
	m := hmac.New(sha256.New, salt)
	m.Write(ikm)
	prk := m.Sum(nil)
	m = hmac.New(sha256.New, prk)
	m.Write(info)
	m.Write([]byte{1})
	return m.Sum(nil)[:n]
}

// encrypt is RFC 8291: one aes128gcm record, the application server's
// key (as) in its header.
func encrypt(plain, uaPublic, auth []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, err
	}
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm := hkdf(auth, secret, keyInfo, 32)
	cek := hkdf(salt, ikm, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdf(salt, ikm, []byte("Content-Encoding: nonce\x00"), 12)
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	const rs = 4096
	if len(plain)+1+16 > rs {
		return nil, errors.New("too long for a notice")
	}
	head := make([]byte, 0, 16+4+1+len(asPublic))
	head = append(head, salt...)
	head = binary.BigEndian.AppendUint32(head, rs)
	head = append(head, byte(len(asPublic)))
	head = append(head, asPublic...)
	return gcm.Seal(head, nonce, append(append([]byte{}, plain...), 2), nil), nil
}
