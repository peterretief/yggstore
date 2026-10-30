// Package password keeps the passwords people sign in on the web with.
// Only a salted, slow hash is kept: on the box, which holds the person's
// record, and on the group's web nodes, which check sign-ins.
package password

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const rounds = 600_000

// TempFor is how long a temporary password works, unused.
const TempFor = 7 * 24 * time.Hour

// MinLength is the shortest password someone may choose.
const MinLength = 10

// Password is someone's password, as the box keeps it and tells the web
// nodes. A temporary one, which the group's admin set, works until
// Expires and must be changed at the first sign-in.
type Password struct {
	Hash    string `json:"hash"`
	Temp    bool   `json:"temp,omitempty"`
	Expires int64  `json:"expires,omitempty"` // unix; a temporary one's
	Set     int64  `json:"set"`               // unix
}

// Live is whether p can still sign in at now.
func (p *Password) Live(now time.Time) bool {
	return p != nil && p.Hash != "" && (!p.Temp || now.Unix() < p.Expires)
}

// ID names this password in a session, so a new one (set by the admin, or
// chosen) ends the sessions started with the old.
func (p *Password) ID() string { return "pw:" + p.Hash[max(0, len(p.Hash)-16):] }

// Matches is whether pw is the password.
func (p *Password) Matches(pw string) bool { return p != nil && Matches(p.Hash, pw) }

func key(pw string, salt []byte) []byte {
	k, _ := pbkdf2.Key(sha256.New, pw, salt, rounds, 32)
	return k
}

// Hash is how a password is kept: "pbkdf2-sha256$<salt>$<key>".
func Hash(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	return "pbkdf2-sha256$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key(pw, salt))
}

// Matches is whether pw is the one hash was made from.
func Matches(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 3 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	salt, err1 := hex.DecodeString(parts[1])
	want, err2 := hex.DecodeString(parts[2])
	return err1 == nil && err2 == nil && subtle.ConstantTimeCompare(key(pw, salt), want) == 1
}

// OK says what's wrong with pw as a password someone chose, if anything.
func OK(pw string) error {
	switch {
	case len(pw) < MinLength:
		return errors.New("Choose a password of at least 10 letters, digits or other characters. A few words together is easy to remember.")
	case len(pw) > 200:
		return errors.New("That password is too long.")
	case strings.Count(pw, pw[:1]) == len(pw) || strings.Contains("01234567890123456789", pw) || strings.Contains("abcdefghijklmnopqrstuvwxyz", strings.ToLower(pw)):
		return errors.New("That password is too easy to guess. Choose another.")
	}
	return nil
}

// tempWords are 256 short, common words, so one random byte picks one.
var tempWords = [256]string{
	"apple", "arm", "baby", "back", "ball", "band", "bank", "bath", "bean",
	"bear", "bed", "bee", "bell", "belt", "bike", "bird", "boat", "body",
	"bone", "book", "boot", "bowl", "box", "bread", "brick", "bridge", "brush",
	"cake", "camp", "candy", "cap", "car", "card", "cart", "cat", "chair",
	"chalk", "cheese", "chest", "chin", "city", "clock", "cloud", "coat",
	"coin", "cook", "corn", "cow", "crab", "cup", "dance", "desk", "dog",
	"door", "dot", "dream", "dress", "drum", "duck", "dust", "ear", "earth",
	"egg", "farm", "fern", "field", "fire", "fish", "flag", "floor", "flower",
	"fog", "foot", "fork", "fox", "frog", "garden", "gate", "gift", "girl",
	"glass", "glove", "goat", "gold", "grape", "grass", "hair", "hall", "hand",
	"hat", "hill", "home", "honey", "hook", "horn", "horse", "house", "ice",
	"ink", "iron", "jam", "jar", "jelly", "juice", "key", "kite", "knee",
	"knot", "lake", "lamp", "leaf", "leg", "lemon", "light", "lime", "lion",
	"lip", "lock", "map", "milk", "mint", "moon", "moth", "mouse", "mud",
	"nail", "nest", "net", "nose", "note", "nut", "oak", "ocean", "oil",
	"onion", "orange", "owl", "page", "paint", "pan", "paper", "park", "pear",
	"pen", "pencil", "piano", "pie", "pig", "pin", "pipe", "plant", "plate",
	"plum", "pond", "pool", "pot", "rabbit", "rain", "ring", "river", "road",
	"rock", "roof", "room", "rope", "rose", "salt", "sand", "seed", "sheep",
	"shell", "ship", "shirt", "shoe", "shop", "silk", "sink", "sky", "smile",
	"snow", "soap", "sock", "sofa", "soup", "spoon", "star", "stone", "stove",
	"straw", "sugar", "sun", "swan", "table", "tail", "tea", "tent", "tiger",
	"toast", "toe", "tooth", "towel", "town", "toy", "train", "tree", "truck",
	"tulip", "van", "vase", "wall", "wave", "well", "whale", "wheel", "wind",
	"window", "wing", "wolf", "wood", "wool", "yard", "zebra", "bus", "flute",
	"harp", "drawer", "basket", "button", "carrot", "cherry", "cloak", "coral",
	"daisy", "eagle", "feather", "forest", "grain", "hammer", "island",
	"ladder", "lettuce", "maple", "marble", "meadow", "mirror", "olive",
	"parrot", "pebble", "pepper", "pillow", "pumpkin", "quilt", "rocket",
	"saddle", "shovel", "spider",
}

// NewTemp makes a temporary password, easy to read out over the phone and
// type, such as "river-tea-lamp-428" (about 34 bits; sign-ins are
// throttled and it lasts a week unused), and what to keep of it.
func NewTemp(now time.Time) (string, Password) {
	b := make([]byte, 3)
	rand.Read(b)
	n, _ := rand.Int(rand.Reader, big.NewInt(900))
	pw := fmt.Sprintf("%s-%s-%s-%d", tempWords[b[0]], tempWords[b[1]], tempWords[b[2]], n.Int64()+100)
	return pw, Password{Hash: Hash(pw), Temp: true, Expires: now.Add(TempFor).Unix(), Set: now.Unix()}
}

// Chosen is what to keep of a password someone chose.
func Chosen(pw string, now time.Time) Password { return Password{Hash: Hash(pw), Set: now.Unix()} }
