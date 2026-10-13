package topstepx

import "github.com/w4jnl/trading-journal/internal/secretbox"

// SecretBox encrypts the stored TopstepX apiKey and session token. It is the
// shared AES-256-GCM box; the alias keeps the TopstepX call sites unchanged.
type SecretBox = secretbox.Box

// NewSecretBox builds a SecretBox from a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) { return secretbox.New(key) }
