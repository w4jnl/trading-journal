package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keyA = "4141414141414141414141414141414141414141414141414141414141414141"
	keyB = "4242424242424242424242424242424242424242424242424242424242424242"
)

// The secrets key falls back to the TopstepX one and vice versa, so an install
// with either key needs nothing new; with both set each keeps its own.
func TestSecretsKeyFallbacks(t *testing.T) {
	a, b := bytes.Repeat([]byte{0x41}, 32), bytes.Repeat([]byte{0x42}, 32)

	cfg := loadYAML(t, "timezone: UTC\n")
	if _, ok := cfg.SecretsKeyBytes(); ok {
		t.Error("no key configured, SecretsKeyBytes ok")
	}
	if cfg.AIUserKeys() {
		t.Error("user keys on without an encryption key")
	}

	cfg = loadYAML(t, "topstepx:\n  encryption_key: \""+keyA+"\"\n")
	if got, ok := cfg.SecretsKeyBytes(); !ok || !bytes.Equal(got, a) {
		t.Errorf("secrets key did not fall back to topstepx: %x %v", got, ok)
	}
	if !cfg.AIUserKeys() {
		t.Error("user keys off with the topstepx key set")
	}

	cfg = loadYAML(t, "secrets:\n  encryption_key: \""+keyB+"\"\n")
	if got, ok := cfg.TopstepXKeyBytes(); !ok || !bytes.Equal(got, b) {
		t.Errorf("topstepx key did not fall back to secrets: %x %v", got, ok)
	}

	cfg = loadYAML(t, "secrets:\n  encryption_key: \""+keyB+"\"\ntopstepx:\n  encryption_key: \""+keyA+"\"\n")
	if got, _ := cfg.SecretsKeyBytes(); !bytes.Equal(got, b) {
		t.Error("secrets key not preferred for secrets")
	}
	if got, _ := cfg.TopstepXKeyBytes(); !bytes.Equal(got, a) {
		t.Error("topstepx key not preferred for topstepx")
	}

	cfg = loadYAML(t, "secrets:\n  encryption_key: \""+keyB+"\"\nai:\n  user_keys: false\n")
	if cfg.AIUserKeys() {
		t.Error("user_keys: false ignored")
	}
}

func TestSecretsKeyValidationAndEnv(t *testing.T) {
	cfg := loadYAML(t, "secrets:\n  encryption_key: \"short\"\n")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "secrets.encryption_key") {
		t.Errorf("malformed key: err = %v", err)
	}

	t.Setenv("TJ_ENCRYPTION_KEY", keyA)
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("timezone: UTC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secrets.EncryptionKey != keyA || !cfg.AIUserKeys() {
		t.Errorf("TJ_ENCRYPTION_KEY not applied: %q", cfg.Secrets.EncryptionKey)
	}
}

// The shipped example must load under the strict decoder and validate.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("example does not validate: %v", err)
	}
}
