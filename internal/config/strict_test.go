package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An unknown or misplaced key fails loudly instead of silently reverting to
// the default (an "mcp:" block nested under "auth:" once did exactly that).
func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("server:\n  port: 8080\n  prot: 9090\nauth:\n  bootstrap_user: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "prot") {
		t.Fatalf("unknown key accepted: err = %v", err)
	}
	// Known keys, including the newly recognised ones, load fine.
	if err := os.WriteFile(p, []byte("server:\n  port: 8081\n  external_url: \"https://t.example/\"\nmarket_data:\n  bar_retention_days: 30\nmw_email:\n  archive_retention_days: 10\nauth:\n  bootstrap_user: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8081 || cfg.Server.ExternalURL != "https://t.example/" || cfg.MarketData.BarRetentionDays != 30 || cfg.MWEmail.ArchiveRetentionDays != 10 {
		t.Errorf("fields not loaded: %+v %+v", cfg.Server, cfg.MarketData)
	}
	// An empty file is fine (defaults + env).
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

func TestValidateChecksPortAndDatabase(t *testing.T) {
	base := func() Config {
		c := defaults()
		c.Auth.BootstrapUser = "x"
		return c
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"port 0", func(c *Config) { c.Server.Port = 0 }, "server.port"},
		{"port too high", func(c *Config) { c.Server.Port = 70000 }, "server.port"},
		{"empty db path", func(c *Config) { c.Database.Path = " " }, "database.path"},
		{"negative bar retention", func(c *Config) { c.MarketData.BarRetentionDays = -1 }, "bar_retention_days"},
		{"negative archive retention", func(c *Config) { c.MWEmail.ArchiveRetentionDays = -1 }, "archive_retention_days"},
	} {
		c := base()
		tc.mut(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %s", tc.name, err, tc.want)
		}
	}
	if err := base().Validate(); err != nil {
		t.Errorf("defaults should validate: %v", err)
	}
}
