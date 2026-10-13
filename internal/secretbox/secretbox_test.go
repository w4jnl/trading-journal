package topstepx

import (
	"bytes"
	"testing"
)

func TestSecretBoxRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	box, err := NewSecretBox(key)
	if err != nil {
		t.Fatalf("NewSecretBox: %v", err)
	}
	ct, nonce, err := box.Seal("my-api-key-123")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := box.Open(ct, nonce)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != "my-api-key-123" {
		t.Errorf("round-trip = %q", got)
	}
}

func TestSecretBoxTamperFails(t *testing.T) {
	box, _ := NewSecretBox(bytes.Repeat([]byte{0x42}, 32))
	ct, nonce, _ := box.Seal("secret")
	ct[0] ^= 0xFF
	if _, err := box.Open(ct, nonce); err == nil {
		t.Error("tampered ciphertext decrypted without error")
	}
}

func TestSecretBoxWrongKeyFails(t *testing.T) {
	box1, _ := NewSecretBox(bytes.Repeat([]byte{0x42}, 32))
	box2, _ := NewSecretBox(bytes.Repeat([]byte{0x43}, 32))
	ct, nonce, _ := box1.Seal("secret")
	if _, err := box2.Open(ct, nonce); err == nil {
		t.Error("wrong key decrypted without error")
	}
}

func TestSecretBoxKeyLength(t *testing.T) {
	if _, err := NewSecretBox([]byte("short")); err == nil {
		t.Error("short key accepted")
	}
}

func TestSecretBoxNonceUnique(t *testing.T) {
	box, _ := NewSecretBox(bytes.Repeat([]byte{0x42}, 32))
	_, n1, _ := box.Seal("x")
	_, n2, _ := box.Seal("x")
	if bytes.Equal(n1, n2) {
		t.Error("two Seals produced the same nonce")
	}
}
