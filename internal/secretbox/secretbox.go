// Package secretbox encrypts the per-user secrets the app stores (TopstepX API
// keys, AI provider keys) with AES-256-GCM. The key comes from config.yaml
// (secrets.encryption_key, or the older topstepx.encryption_key) and is never
// auto-generated: losing it would brick every stored credential, so it must
// live where the user backs it up.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// Box seals and opens secrets under one 32-byte key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secretbox: encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext under a fresh random nonce. Ciphertext and nonce
// are stored as separate DB columns.
func (b *Box) Seal(plaintext string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	return b.aead.Seal(nil, nonce, []byte(plaintext), nil), nonce, nil
}

// Open decrypts a (ciphertext, nonce) pair. Fails when the key differs from
// the one that sealed it or the data was tampered with.
func (b *Box) Open(ciphertext, nonce []byte) (string, error) {
	plain, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("secretbox: decrypt (wrong or rotated encryption key?): %w", err)
	}
	return string(plain), nil
}
