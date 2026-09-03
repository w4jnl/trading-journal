package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadYAML writes body to a temp config file and loads it.
func loadYAML(t *testing.T, body string) Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// The read-triggered sync gap defaults to 60s when the key is absent (YAML
// is unmarshalled OVER defaults()), an explicit 0 disables the trigger and
// passes validation, and negatives are rejected.
func TestAPISyncThrottleDefaultAndDisable(t *testing.T) {
	cfg := loadYAML(t, "topstepx:\n  http_timeout_sec: 15\nibkr:\n  tickle_sec: 60\n")
	if cfg.TopstepX.APISyncThrottleSec != 60 || cfg.IBKR.APISyncThrottleSec != 60 {
		t.Errorf("absent key: topstepx=%d ibkr=%d, want 60/60", cfg.TopstepX.APISyncThrottleSec, cfg.IBKR.APISyncThrottleSec)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}

	cfg = loadYAML(t, "topstepx:\n  api_sync_throttle_sec: 0\nibkr:\n  api_sync_throttle_sec: 0\n")
	if cfg.TopstepX.APISyncThrottleSec != 0 || cfg.IBKR.APISyncThrottleSec != 0 {
		t.Errorf("explicit 0: topstepx=%d ibkr=%d, want 0/0", cfg.TopstepX.APISyncThrottleSec, cfg.IBKR.APISyncThrottleSec)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("0 (disabled) must validate: %v", err)
	}

	cfg = loadYAML(t, "topstepx:\n  api_sync_throttle_sec: -5\n")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "topstepx.api_sync_throttle_sec") {
		t.Errorf("negative must be rejected naming the key, got %v", err)
	}
	cfg = loadYAML(t, "ibkr:\n  api_sync_throttle_sec: -1\n")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ibkr.api_sync_throttle_sec") {
		t.Errorf("negative must be rejected naming the key, got %v", err)
	}
}
