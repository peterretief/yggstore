package passkey

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// A browser key's PIN. The group checks it (the web nodes, and the box),
// not the browser, so someone who takes the device apart can't try PINs
// without them being counted (MaxPINFails). Only a salted, slow hash is
// kept.

const pinRounds = 100_000

// MaxPINFails wrong PINs in a row lock a browser key.
const MaxPINFails = 5

var (
	// ErrNeedPIN: the browser key has no PIN yet, so one must be chosen.
	ErrNeedPIN   = errors.New("Choose a PIN for this browser's key.")
	ErrPINLocked = errors.New("This browser's key is locked after too many wrong PINs. Ask the box's owner for a new invite.")
)

// WrongPIN is the answer to a wrong PIN, with left tries before the lock.
func WrongPIN(left int) error {
	if left <= 0 {
		return ErrPINLocked
	}
	if left == 1 {
		return errors.New("That PIN is wrong. One more wrong try locks this browser's key.")
	}
	return fmt.Errorf("That PIN is wrong. %d more wrong tries lock this browser's key.", left)
}

// PINOK says what's wrong with pin as a new PIN, if anything.
func PINOK(pin string) error {
	if len(pin) < 6 {
		return errors.New("Choose a PIN of at least 6 digits or letters.")
	}
	if len(pin) > 64 {
		return errors.New("That PIN is too long.")
	}
	if strings.Count(pin, pin[:1]) == len(pin) || strings.Contains("01234567890", pin) || strings.Contains("09876543210", pin) {
		return errors.New("That PIN is too easy to guess. Choose another.")
	}
	return nil
}

func pinKey(pin string, salt []byte) []byte {
	k, _ := pbkdf2.Key(sha256.New, pin, salt, pinRounds, 32)
	return k
}

// PINHash is how the box keeps pin: "pbkdf2-sha256$<salt>$<key>".
func PINHash(pin string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	return "pbkdf2-sha256$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(pinKey(pin, salt))
}

// PINMatches is whether pin is the one hash was made from.
func PINMatches(hash, pin string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 3 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	salt, err1 := hex.DecodeString(parts[1])
	want, err2 := hex.DecodeString(parts[2])
	return err1 == nil && err2 == nil && subtle.ConstantTimeCompare(pinKey(pin, salt), want) == 1
}
