// Package share sends a stored item to another person: the stub (which holds
// the item's key) is sealed for the recipient, so the .ysend file can travel
// by email or anything else and only they can open it.
//
// Everyone has a sharing identity, an X25519 key pair kept in a private file.
// Its public half, written as a sharing code ("ys1…"), is what you give people
// who want to send you things. A sealed file is encrypted with a key derived
// from the sender's private key and the recipient's public key, so opening it
// also proves which sharing code sent it.
package share

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Ext is the extension of a sealed item.
const Ext = ".ysend"

const codePrefix = "ys1"

var b64 = base64.RawURLEncoding

// Identity is one person's sharing key pair.
type Identity struct {
	key *ecdh.PrivateKey
}

// Load reads the identity at path.
func Load(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := b64.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Identity{key}, nil
}

// LoadOrCreate reads the identity at path, creating it (mode 0600) if missing.
func LoadOrCreate(path string) (*Identity, error) {
	if data, err := os.ReadFile(path); err == nil {
		raw, err := b64.DecodeString(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		key, err := ecdh.X25519().NewPrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return &Identity{key}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(b64.EncodeToString(key.Bytes()) + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	return &Identity{key}, f.Close()
}

// Code is the sharing code to give people who want to send you things.
func (id *Identity) Code() string {
	return codePrefix + b64.EncodeToString(id.key.PublicKey().Bytes())
}

// ParseCode reads a sharing code.
func ParseCode(code string) (*ecdh.PublicKey, error) {
	code = strings.TrimSpace(code)
	raw, err := b64.DecodeString(strings.TrimPrefix(code, codePrefix))
	if err != nil || !strings.HasPrefix(code, codePrefix) {
		return nil, errors.New(`not a sharing code (they start with "ys1")`)
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, errors.New("not a sharing code: wrong length")
	}
	return pub, nil
}

// envelope is a .ysend file. Only the two sharing codes are readable; the
// item's name, the sender's name and the stub are all inside Data.
type envelope struct {
	Format string `json:"yggstore_share"`
	From   string `json:"from"`
	To     string `json:"to"`
	Salt   string `json:"salt"`
	Nonce  string `json:"nonce"`
	Data   string `json:"data"`
}

type contents struct {
	FromName string          `json:"from_name"`
	Note     string          `json:"note,omitempty"`
	Stub     json.RawMessage `json:"stub"`
}

// Received is an opened .ysend.
type Received struct {
	From     string // the sender's sharing code, proven by the decryption
	FromName string // the name the sender gave, not proven
	Note     string
	Stub     []byte
}

const format = "1"

func (id *Identity) aead(peer *ecdh.PublicKey, salt []byte) (cipher.AEAD, error) {
	shared, err := id.key.ECDH(peer)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, shared, salt, "yggstore share v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal makes a .ysend of stub for the holder of the sharing code to.
func (id *Identity) Seal(to string, stub []byte, fromName, note string) ([]byte, error) {
	pub, err := ParseCode(to)
	if err != nil {
		return nil, err
	}
	if !json.Valid(stub) {
		return nil, errors.New("stub is not valid JSON")
	}
	plain, err := json.Marshal(contents{FromName: fromName, Note: note, Stub: stub})
	if err != nil {
		return nil, err
	}
	salt, nonce := make([]byte, 32), make([]byte, 12)
	rand.Read(salt)
	rand.Read(nonce)
	aead, err := id.aead(pub, salt)
	if err != nil {
		return nil, err
	}
	env := envelope{Format: format, From: id.Code(), To: codePrefix + b64.EncodeToString(pub.Bytes()),
		Salt: b64.EncodeToString(salt), Nonce: b64.EncodeToString(nonce)}
	env.Data = b64.EncodeToString(aead.Seal(nil, nonce, plain, []byte(env.From+env.To)))
	return json.MarshalIndent(env, "", "  ")
}

// ErrNotForYou means the .ysend was sealed for another sharing code.
var ErrNotForYou = errors.New("this was sent to someone else's sharing code")

// Open reads a .ysend sealed for this identity.
func (id *Identity) Open(data []byte) (Received, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil || env.Format == "" {
		return Received{}, errors.New("not a yggstore share file")
	}
	if env.Format != format {
		return Received{}, fmt.Errorf("share file format %q is newer than this yggstore", env.Format)
	}
	if env.To != id.Code() {
		return Received{}, ErrNotForYou
	}
	from, err := ParseCode(env.From)
	if err != nil {
		return Received{}, fmt.Errorf("sender: %w", err)
	}
	salt, err1 := b64.DecodeString(env.Salt)
	nonce, err2 := b64.DecodeString(env.Nonce)
	sealed, err3 := b64.DecodeString(env.Data)
	if err := errors.Join(err1, err2, err3); err != nil || len(nonce) != 12 {
		return Received{}, errors.New("share file is damaged")
	}
	aead, err := id.aead(from, salt)
	if err != nil {
		return Received{}, err
	}
	plain, err := aead.Open(nil, nonce, sealed, []byte(env.From+env.To))
	if err != nil {
		return Received{}, errors.New("share file is damaged or was altered")
	}
	var c contents
	if err := json.Unmarshal(plain, &c); err != nil {
		return Received{}, errors.New("share file is damaged")
	}
	return Received{From: env.From, FromName: c.FromName, Note: c.Note, Stub: c.Stub}, nil
}
