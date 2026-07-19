package topstepx

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

// buildJWT assembles an unsigned header.payload.sig token with the given exp.
func buildJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":"trader","exp":%d}`, exp)))
	return header + "." + payload + ".sig"
}

func TestJWTExpiry(t *testing.T) {
	want := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	got, ok := jwtExpiry(buildJWT(want.Unix()))
	if !ok || !got.Equal(want) {
		t.Errorf("jwtExpiry = (%v, %v), want (%v, true)", got, ok, want)
	}
}

func TestJWTExpiryMalformed(t *testing.T) {
	for _, tok := range []string{
		"",
		"not-a-jwt",
		"a.b",               // two segments
		"a.!!notbase64!!.c", // bad encoding
		"a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".c", // no exp
		"a." + base64.RawURLEncoding.EncodeToString([]byte(`not json`)) + ".c",
	} {
		if _, ok := jwtExpiry(tok); ok {
			t.Errorf("jwtExpiry(%q) ok = true, want false", tok)
		}
	}
}

func TestTokenExpiryFallback(t *testing.T) {
	before := time.Now().UTC().Add(tokenFallbackTTL - time.Minute)
	got := tokenExpiry("opaque-non-jwt-token")
	after := time.Now().UTC().Add(tokenFallbackTTL + time.Minute)
	if got.Before(before) || got.After(after) {
		t.Errorf("fallback expiry %v outside [%v, %v]", got, before, after)
	}
	// A real JWT exp wins over the fallback.
	exp := time.Now().Add(10 * time.Hour).Truncate(time.Second).UTC()
	if got := tokenExpiry(buildJWT(exp.Unix())); !got.Equal(exp) {
		t.Errorf("tokenExpiry = %v, want %v", got, exp)
	}
}
