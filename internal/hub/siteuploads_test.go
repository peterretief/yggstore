package hub

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
)

var b64 = base64.RawURLEncoding

// loginKey is a key like a web node's, and the JWK it publishes.
func loginKey(t *testing.T, kid string) (*ecdsa.PrivateKey, light.JWK) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.PublicKey.ECDH()
	raw := pub.Bytes() // 0x04, X, Y
	return k, light.JWK{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(raw[1:33]), Y: b64.EncodeToString(raw[33:]), Kid: kid, Use: "sig", Alg: "ES256"}
}

func signToken(t *testing.T, k *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": kid})
	body, _ := json.Marshal(claims)
	in := b64.EncodeToString(head) + "." + b64.EncodeToString(body)
	h := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + b64.EncodeToString(sig)
}

// photo is a real JPEG with an EXIF block saying where it was taken and
// that it's turned (orientation 6).
func photo(t *testing.T) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x02" +
		"\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00" + // orientation 6
		"\x88\x25\x00\x04\x00\x00\x00\x01\x00\x00\x00\x00" + // GPS IFD (empty here)
		"\x00\x00\x00\x00GPS 51.5N 0.1W SECRET")
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := append([]byte{0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}, seg...)
	j := b.Bytes()
	return append(append(append([]byte{}, j[:2]...), app1...), j[2:]...)
}

func TestSiteUploads(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	nodeMail := filepath.Join(dir, "nodemail")
	os.MkdirAll(nodeMail, 0o700)
	ctx := context.Background()
	st, _ := Update(ctx, statePath, func(s *HubState) error {
		_, err := s.AddPerson("Anna", filepath.Join(dir, "users"))
		return err
	})
	anna := st.People[0]
	key, err := PersonKey(statePath, anna)
	if err != nil {
		t.Fatal(err)
	}
	const name = "anna.example.org"
	list := []peers.Peer{
		{Name: "admin", Addr: "[200::9]:7400", Admin: true, Domain: "example.org"},
		{Name: "box", Addr: "[200::2]:7400", PeopleNames: []peers.PersonName{{Name: name, Person: "Anna", Code: key.Code()}}},
	}
	web, jwk := loginKey(t, "w1")
	stranger, _ := loginKey(t, "w1") // same key ID, not the login's key
	ps := NewPeopleSites(PeopleSitesConfig{StatePath: statePath, Self: "200::2", NodeMail: nodeMail,
		Dir: filepath.Join(dir, "people"), Logf: t.Logf,
		LoginKeys: func(context.Context) []light.JWK { return []light.JWK{jwk} }})
	// Anna's site, as ensure starts it, with Tim on its list.
	pub := &site.Publisher{Dir: filepath.Join(dir, "people", anna.Folder, "sites")}
	os.MkdirAll(filepath.Join(pub.Dir, name), 0o700)
	os.WriteFile(filepath.Join(pub.Dir, name, "site.json"), []byte(`{"uploaders":["tim@example.org"]}`), 0o600)
	ps.running[anna.Folder] = &personSite{name: name, names: []string{name}, pub: pub, cancel: func() {}}

	claims := func(email, aud string) map[string]any {
		return map[string]any{"iss": "https://my.example.org", "aud": aud, "use": "access", "email": email,
			"name": map[string]string{"tim@example.org": "Tim", "bob@example.org": "Bob"}[email], "exp": time.Now().Add(time.Hour).Unix()}
	}
	pic := photo(t)
	put := func(id string, raw []byte) {
		sealed, err := mail.Seal(key.Code(), raw)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(nodeMail, id+".sealed"), sealed, 0o600)
		os.WriteFile(filepath.Join(nodeMail, id+".json"), []byte(`{"received":1}`), 0o600)
		os.WriteFile(filepath.Join(nodeMail, id+".ystub"), []byte(`{}`), 0o600)
	}
	upload := func(id, token string) {
		raw, err := site.UploadMessage(site.Upload{Site: name, Token: token, Caption: "the beach", Name: "beach.jpg", Type: "image/jpeg",
			File: pic, Preview: pic}, "Tim", "tim@example.org", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		put(id, raw)
	}
	upload("good", signToken(t, web, "w1", claims("tim@example.org", name)))
	upload("forged", signToken(t, stranger, "w1", claims("tim@example.org", name)))
	upload("notListed", signToken(t, web, "w1", claims("bob@example.org", name)))
	upload("otherSite", signToken(t, web, "w1", claims("tim@example.org", "evil.example.org")))
	put("plainMail", []byte("Subject: hi\r\n\r\nhello\r\n"))

	ps.sortMail(ctx, map[string]*share.Identity{anna.Folder: key}, st, list)

	box, _ := ps.Mailbox(anna)
	msgs, _ := box.List()
	if len(msgs) != 4 {
		t.Fatalf("in her messages: %d, want the 4 that aren't good uploads", len(msgs))
	}
	updir := filepath.Join(anna.Path, outbox.WebsitesDir, name, UploadsDir)
	var idx struct{ Uploads []Uploaded }
	b, err := os.ReadFile(filepath.Join(updir, "index.json"))
	if err != nil || json.Unmarshal(b, &idx) != nil || len(idx.Uploads) != 1 {
		t.Fatalf("index: %s %v", b, err)
	}
	u := idx.Uploads[0]
	if u.By != "Tim" || u.Caption != "the beach" || u.Name != "beach.jpg" || u.Preview == "" || strings.Contains(string(b), "tim@example.org") {
		t.Fatalf("listed: %+v (no address on the public site)", u)
	}
	kept, _ := os.ReadFile(filepath.Join(updir, u.File))
	if bytes.Contains(kept, []byte("SECRET")) {
		t.Fatal("the photo still says where it was taken")
	}
	if exifOrientation(kept[bytes.Index(kept, []byte("Exif")):]) != 6 {
		t.Fatal("the photo lost which way up it goes")
	}
	if _, err := jpeg.Decode(bytes.NewReader(kept)); err != nil {
		t.Fatalf("the photo doesn't open: %v", err)
	}
	if logged, _ := os.ReadFile(filepath.Join(dir, "people", anna.Folder, "uploads.log")); !bytes.Contains(logged, []byte("tim@example.org")) {
		t.Fatal("the box didn't note whose address it came from")
	}
	if _, err := os.Stat(filepath.Join(nodeMail, "good.sealed")); err == nil {
		t.Fatal("the upload is still in the node's mailbox")
	}

	// Who may add things is set on her website page.
	if err := ps.SetUploaders(ctx, anna, []string{"tim@example.org", "not an address"}); err == nil || !strings.Contains(err.Error(), "isn't an email address") {
		t.Fatalf("a bad address: %v", err)
	}
	if err := ps.SetUploaders(ctx, anna, []string{"kath@example.org"}); err == nil || !strings.Contains(err.Error(), "isn't published yet") {
		t.Fatalf("before the site is published: %v", err)
	}

	// Anna deletes it on her files page: it leaves the list.
	os.Remove(filepath.Join(updir, u.File))
	ps.indexAll(st)
	b, _ = os.ReadFile(filepath.Join(updir, "index.json"))
	if json.Unmarshal(b, &idx); len(idx.Uploads) != 0 {
		t.Fatalf("still listed: %s", b)
	}
	if _, err := os.Stat(filepath.Join(updir, u.ID+".json")); err == nil {
		t.Fatal("its record is still there")
	}
}
