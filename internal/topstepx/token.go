package topstepx

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const (
	// tokenRefreshMargin: the background maintainer renews a token once less
	// than this much validity remains (validate rolls the ~24h window).
	tokenRefreshMargin = 2 * time.Hour
	// tokenUseMargin: a sync refuses to send a token this close to expiry
	// and refreshes inline instead - avoids a request dying mid-flight on a
	// token the maintainer was about to renew anyway.
	tokenUseMargin = 5 * time.Minute
	// tokenMaintainInterval is the maintainer's check cadence. Comfortably
	// inside tokenRefreshMargin so a renewal is never skipped past expiry.
	tokenMaintainInterval = 15 * time.Minute
	// tokenFallbackTTL is assumed when the token's exp claim can't be read
	// (ProjectX session tokens live ~24h; stay one hour conservative).
	tokenFallbackTTL = 23 * time.Hour
)

// jwtExpiry extracts the exp claim from a JWT session token. No signature
// verification - we only read our own token's lifetime hint to schedule its
// renewal. ok=false when the token isn't a parsable JWT or has no exp.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders emit padded base64url; try that before giving up.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}

// tokenExpiry is jwtExpiry with the conservative fallback applied.
func tokenExpiry(token string) time.Time {
	if exp, ok := jwtExpiry(token); ok {
		return exp
	}
	return time.Now().UTC().Add(tokenFallbackTTL)
}
