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

// sync_log_retention_days defaults to 2, 0 disables, negatives are rejected.
func TestSyncLogRetentionConfig(t *testing.T) {
	if cfg := loadYAML(t, "timezone: UTC\n"); cfg.SyncLogRetentionDays != 2 {
		t.Errorf("default = %d, want 2", cfg.SyncLogRetentionDays)
	}
	if cfg := loadYAML(t, "sync_log_retention_days: 0\n"); cfg.SyncLogRetentionDays != 0 || cfg.Validate() != nil {
		t.Errorf("0 must be accepted as disabled")
	}
	if err := loadYAML(t, "sync_log_retention_days: -1\n").Validate(); err == nil || !strings.Contains(err.Error(), "sync_log_retention_days") {
		t.Errorf("negative must be rejected naming the key, got %v", err)
	}
}

// The MotiveWave listener's SMTP credentials come from the environment when
// set, so a checked-in config.yaml can leave them empty: a non-empty variable
// wins over the file, an empty or unset one leaves the file's value alone,
// and the "set together" rule judges the values after the overrides.
func TestMWEmailCredentialsFromEnv(t *testing.T) {
	const both = "mw_email:\n  enabled: true\n  smtp_username: filename\n  smtp_password: filepass\n"
	const none = "mw_email:\n  enabled: true\n"
	const userOnly = "mw_email:\n  enabled: true\n  smtp_username: filename\n"
	cases := []struct {
		name     string
		yaml     string
		user     string // "" leaves the variable unset
		pass     string
		wantUser string
		wantPass string
		valid    bool
	}{
		{"env overrides the file", both, "envname", "envpass", "envname", "envpass", true},
		{"env alone", none, "envname", "envpass", "envname", "envpass", true},
		{"file alone", both, "", "", "filename", "filepass", true},
		{"only the username, from the file", userOnly, "", "", "filename", "", false},
		{"only the password, from env", none, "", "envpass", "", "envpass", false},
		{"only the username, from env", none, "envname", "", "envname", "", false},
		{"empty env keeps the file", both, "", "", "filename", "filepass", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// An empty string sets the variable to "" (present but empty), the
			// case the override must treat as unset.
			t.Setenv("TJ_MW_EMAIL_SMTP_USERNAME", c.user)
			t.Setenv("TJ_MW_EMAIL_SMTP_PASSWORD", c.pass)
			cfg := loadYAML(t, c.yaml)
			if cfg.MWEmail.Username != c.wantUser || (cfg.MWEmail.Password != c.wantPass) {
				// The password is reported only as "as expected" or not.
				t.Errorf("username = %q, password as expected = %v", cfg.MWEmail.Username, cfg.MWEmail.Password == c.wantPass)
			}
			err := cfg.Validate()
			if c.valid && err != nil {
				t.Errorf("validate: %v", err)
			}
			if !c.valid && (err == nil || !strings.Contains(err.Error(), "must be set together")) {
				t.Errorf("want the set-together error, got %v", err)
			}
		})
	}
}
