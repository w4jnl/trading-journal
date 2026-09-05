package config

import (
	"strings"
	"testing"
)

// mcp.oauth is opt-in (nil = off) with defaulted lifetimes; enabling it
// requires a bare https issuer (http only on loopback) and positive TTLs, and
// cannot be combined with mcp.enabled=false.
func TestMCPOAuthConfig(t *testing.T) {
	cfg := loadYAML(t, "mcp:\n  enabled: true\n")
	if cfg.MCP.OAuthEnabled() {
		t.Errorf("absent oauth block must be off")
	}
	if o := cfg.MCP.OAuth; o.AccessTokenTTLSec != 3600 || o.RefreshTokenTTLSec != 2592000 || o.CodeTTLSec != 60 {
		t.Errorf("defaults = %+v", o)
	}
	if !cfg.MCP.OAuth.ConsentRequired() {
		t.Errorf("consent must default to required")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}

	cfg = loadYAML(t, "mcp:\n  oauth:\n    enabled: true\n    issuer: \"https://trading.example.com/\"\n    consent: false\n")
	if !cfg.MCP.OAuthEnabled() || cfg.MCP.OAuth.ConsentRequired() || cfg.MCP.OAuth.IssuerURL() != "https://trading.example.com" {
		t.Errorf("enabled cfg = %+v (enabled=%v)", cfg.MCP.OAuth, cfg.MCP.OAuthEnabled())
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid https issuer rejected: %v", err)
	}

	// Loopback http is fine for local development; a real host over http is not.
	if err := loadYAML(t, "mcp:\n  oauth:\n    enabled: true\n    issuer: \"http://127.0.0.1:18099\"\n").Validate(); err != nil {
		t.Errorf("loopback http issuer must validate: %v", err)
	}
	bad := map[string]string{
		"missing issuer":  "mcp:\n  oauth:\n    enabled: true\n",
		"http real host":  "mcp:\n  oauth:\n    enabled: true\n    issuer: \"http://trading.example.com\"\n",
		"path in issuer":  "mcp:\n  oauth:\n    enabled: true\n    issuer: \"https://trading.example.com/mcp\"\n",
		"query in issuer": "mcp:\n  oauth:\n    enabled: true\n    issuer: \"https://trading.example.com?x=1\"\n",
		"relative issuer": "mcp:\n  oauth:\n    enabled: true\n    issuer: \"trading.example.com\"\n",
		"zero code ttl":   "mcp:\n  oauth:\n    enabled: true\n    issuer: \"https://trading.example.com\"\n    code_ttl_sec: 0\n",
		"negative ttl":    "mcp:\n  oauth:\n    enabled: true\n    issuer: \"https://trading.example.com\"\n    access_token_ttl_sec: -1\n",
		"mcp disabled":    "mcp:\n  enabled: false\n  oauth:\n    enabled: true\n    issuer: \"https://trading.example.com\"\n",
	}
	for name, body := range bad {
		if err := loadYAML(t, body).Validate(); err == nil || !strings.Contains(err.Error(), "mcp.oauth") {
			t.Errorf("%s: must be rejected naming mcp.oauth, got %v", name, err)
		}
	}
	// Enabled without issuer is also simply "not enabled" at runtime.
	if loadYAML(t, "mcp:\n  oauth:\n    enabled: true\n").MCP.OAuthEnabled() {
		t.Errorf("enabled without issuer must report OAuthEnabled()=false")
	}
}
