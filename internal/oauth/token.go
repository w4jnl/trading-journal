package oauth

import (
	"crypto/rand"
	"encoding/base64"

	"github.com/w4jnl/trading-journal/internal/auth"
)

// Prefixes make a secret's kind recognizable in logs and let the /mcp
// middleware dispatch without a lookup. None of them has the prefix "tj_"
// (third character differs), so they can never be mistaken for API keys.
const (
	PrefixClientID     = "tjc_"
	PrefixAccessToken  = "tjo_"
	PrefixRefreshToken = "tjr_"
)

// NewOpaque mints a 256-bit random secret rendered as prefix+base64url and
// returns it with the sha256 hex that is persisted for lookup. Mirrors
// auth.GenerateAPIKey; the plaintext is handed to the client exactly once.
func NewOpaque(prefix string) (full, sha string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	full = prefix + base64.RawURLEncoding.EncodeToString(buf)
	return full, HashToken(full), nil
}

// HashToken is the lookup hash for any opaque secret this package issues -
// the same hex(sha256(full)) the API-key middleware uses, so one convention
// covers every credential in the database.
func HashToken(full string) string { return auth.HashAPIKey(full) }
