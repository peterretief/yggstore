package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291, Appendix A.
func TestRFC8291(t *testing.T) {
	plain := mustB64(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	as, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b64.EncodeToString(as.PublicKey().Bytes()); got != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("as_public %s", got)
	}
	ua := mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	body, err := encrypt(plain, ua, mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"), as, mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	head := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	ct := mustB64(t, "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")
	if string(body) != string(head)+string(ct) {
		t.Fatalf("body %s", b64.EncodeToString(body))
	}
}

// Send posts the encrypted notice with a VAPID token the push service can
// check against the sender's public key; a gone browser is ErrGone.
func TestSend(t *testing.T) {
	s, err := LoadOrCreate(filepath.Join(t.TempDir(), "push.key"), "https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kept.key")
	k1, _ := LoadOrCreate(path, "")
	k2, _ := LoadOrCreate(path, "")
	if k1.PublicKey() != k2.PublicKey() {
		t.Fatal("the key isn't kept")
	}
	browser, _ := ecdh.P256().GenerateKey(rand.Reader)
	var got *http.Request
	var gotBody []byte
	status := 201
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	s.Client = srv.Client()
	var sub Subscription
	sub.Endpoint = srv.URL + "/push/abc"
	sub.Keys.P256dh = b64.EncodeToString(browser.PublicKey().Bytes())
	sub.Keys.Auth = b64.EncodeToString(make([]byte, 16))
	if err := s.Send(context.Background(), sub, []byte(`{"title":"New mail"}`), time.Hour); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("TTL") != "3600" || len(gotBody) < 86 {
		t.Fatalf("headers %v, %d bytes", got.Header, len(gotBody))
	}
	auth := got.Header.Get("Authorization")
	tok, k, ok := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ", k=")
	if !ok || k != s.PublicKey() {
		t.Fatalf("authorization %q", auth)
	}
	parts := strings.Split(tok, ".")
	var claims map[string]any
	json.Unmarshal(mustB64(t, parts[1]), &claims)
	if claims["aud"] != srv.URL || claims["sub"] != "https://example.org" {
		t.Fatalf("claims %v", claims)
	}
	pub, _ := ecdh.P256().NewPublicKey(mustB64(t, k))
	x, y := new(big.Int).SetBytes(pub.Bytes()[1:33]), new(big.Int).SetBytes(pub.Bytes()[33:])
	sig := mustB64(t, parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: s.key.Curve, X: x, Y: y}, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the VAPID token doesn't verify")
	}
	status = 410
	if err := s.Send(context.Background(), sub, []byte("x"), time.Hour); err != ErrGone {
		t.Fatalf("gone: %v", err)
	}
	sub.Endpoint = "http://insecure.example/push"
	if err := s.Send(context.Background(), sub, []byte("x"), time.Hour); err == nil {
		t.Fatal("sent to a plain http address")
	}
}
