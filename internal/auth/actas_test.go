package auth

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestActAsTokenRoundTrip(t *testing.T) {
	secret := []byte("test-secret-key")
	tok := signActAsToken(42, time.Now().Add(time.Hour), secret)
	id, ok := verifyActAsToken(tok, secret)
	if !ok || id != 42 {
		t.Fatalf("roundtrip failed: id=%d ok=%v", id, ok)
	}
}

func TestActAsTokenExpired(t *testing.T) {
	secret := []byte("test-secret-key")
	// exp one hour in the past: signature is valid, but the token is stale.
	tok := signActAsToken(42, time.Now().Add(-time.Hour), secret)
	if _, ok := verifyActAsToken(tok, secret); ok {
		t.Fatal("expired token accepted; must be rejected")
	}
}

func TestActAsTokenWrongSecret(t *testing.T) {
	tok := signActAsToken(42, time.Now().Add(time.Hour), []byte("secret-a"))
	if _, ok := verifyActAsToken(tok, []byte("secret-b")); ok {
		t.Fatal("token verified under a different secret; HMAC broken")
	}
}

func TestActAsTokenTamperedExpiry(t *testing.T) {
	secret := []byte("test-secret-key")
	// Sign for an expiry in the past, then rewrite only the expiry field to the
	// future while keeping the original signature. The HMAC covers
	// "userID|expUnix", so verification must reject the swap.
	valid := signActAsToken(42, time.Now().Add(-time.Hour), secret)
	raw, err := base64.RawURLEncoding.DecodeString(valid)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape: %q", raw)
	}
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	forgedRaw := parts[0] + "|" + future + "|" + parts[2] // new exp, stale sig
	forged := base64.RawURLEncoding.EncodeToString([]byte(forgedRaw))
	if _, ok := verifyActAsToken(forged, secret); ok {
		t.Fatal("tampered-expiry token accepted; expiry is not authenticated")
	}
}
