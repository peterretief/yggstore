package cryptofile

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
)

const KeySize = 32

func Encrypt(plaintext []byte) (ciphertext []byte, key []byte, nonce []byte, err error) {
	key = make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, nil, nil, fmt.Errorf("generate key: %w", err)
	}
	nonce, err = RandomNonce()
	if err != nil {
		return nil, nil, nil, err
	}
	ciphertext, err = EncryptWithKey(plaintext, key, nonce)
	if err != nil {
		return nil, nil, nil, err
	}
	return ciphertext, key, nonce, nil
}

func RandomNonce() ([]byte, error) {
	block, err := aes.NewCipher(make([]byte, KeySize))
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, nil
}

func EncryptWithKey(plaintext, key, nonce []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce size: got %d want %d", len(nonce), gcm.NonceSize())
	}
	return gcm.Seal(nil, nonce, plaintext, nil), nil
}

func EncryptChunk(plaintext, key []byte) (ciphertext, nonce []byte, err error) {
	nonce, err = RandomNonce()
	if err != nil {
		return nil, nil, err
	}
	ciphertext, err = EncryptWithKey(plaintext, key, nonce)
	if err != nil {
		return nil, nil, err
	}
	return ciphertext, nonce, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("invalid key size: got %d want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return gcm, nil
}

func Decrypt(ciphertext, key, nonce []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce size: got %d want %d", len(nonce), gcm.NonceSize())
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}
