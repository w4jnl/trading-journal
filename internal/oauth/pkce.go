package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// ValidPKCEString reports whether s is a legal PKCE code_verifier /
// code_challenge value (RFC 7636 section 4.1): 43 to 128 characters from the
// unreserved set [A-Za-z0-9-._~].
func ValidPKCEString(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// VerifyPKCES256 checks base64url(sha256(verifier)) == challenge in constant
// time. S256 is the only method this server accepts; "plain" is refused at
// /oauth/authorize so a challenge stored here is always an S256 digest.
func VerifyPKCES256(verifier, challenge string) bool {
	if !ValidPKCEString(verifier) || !ValidPKCEString(challenge) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	calc := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(calc), []byte(challenge)) == 1
}
