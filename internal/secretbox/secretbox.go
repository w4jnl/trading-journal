package topstepx

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// SecretBox encrypts the stored TopstepX apiKey with AES-256-GCM. The key
// comes from config.yaml (topstepx.encryption_key, 32 bytes hex) and is never
// auto-generated: losing it would brick every stored credential, so it must
// live where the user backs it up.
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox builds a SecretBox from a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("topstepx: encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("topstepx: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("topstepx: gcm: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

// Seal encrypts plaintext under a fresh random nonce. Ciphertext and nonce
// are stored as separate DB columns.
func (b *SecretBox) Seal(plaintext string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("topstepx: nonce: %w", err)
	}
	return b.aead.Seal(nil, nonce, []byte(plaintext), nil), nonce, nil
}

// Open decrypts a (ciphertext, nonce) pair. Fails when the key differs from
// the one that sealed it or the data was tampered with.
func (b *SecretBox) Open(ciphertext, nonce []byte) (string, error) {
	plain, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("topstepx: decrypt credentials (wrong or rotated encryption key?): %w", err)
	}
	return string(plain), nil
}
