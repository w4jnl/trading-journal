package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       Server       `yaml:"server"`
	Database     Database     `yaml:"database"`
	Imports      Imports      `yaml:"imports"`
	OIDashboards OIDashboards `yaml:"oi_dashboards"`
	Auth         Auth         `yaml:"auth"`
	Backup       Backup       `yaml:"backup"`
	MarketData   MarketData   `yaml:"market_data"`
	MCP          MCP          `yaml:"mcp"`
	AI           AI           `yaml:"ai"`
	Timezone     string       `yaml:"timezone"`
}

// MCP toggles the Model Context Protocol server at /mcp (for AI clients). It
// reuses the same per-user api_keys as the REST API. Enabled (nil = true).
type MCP struct {
	Enabled *bool `yaml:"enabled"`
	// InstanceLabel (e.g. "prod" / "dev") is reported by the MCP server_info
	// tool so an AI client can tell which instance it is connected to.
	InstanceLabel string `yaml:"instance_label"`
}

// AI configures the in-app analysis engine (server-side journal analysis). It calls
// an OpenAI-compatible chat-completions endpoint: v1 targets a local MLX server but
// the same client speaks to OpenAI, OpenRouter and other aggregators, LM Studio,
// vLLM - anything exposing /chat/completions. Disabled by default; the whole feature
// is gated on Enabled. APIKey can be injected via the TJ_AI_API_KEY env var.
type AI struct {
	Enabled     bool              `yaml:"enabled"`
	Provider    string            `yaml:"provider"`
	BaseURL     string            `yaml:"base_url"`
	Model       string            `yaml:"model"`
	APIKey      string            `yaml:"api_key"`
	TimeoutSec  int               `yaml:"timeout_sec"`
	MaxTokens   int               `yaml:"max_tokens"`
	Temperature float64           `yaml:"temperature"`
	Headers     map[string]string `yaml:"headers"`
}

// MarketData configures the historical price-bar source behind the trade-replay
// charts. The v1 source is Yahoo Finance v8 (free, no key). Enabled (nil = true)
// toggles the whole feature; Prefetch (nil = true) toggles only the background
// warm-on-import worker (set false to keep charts lazy/button-on-miss).
type MarketData struct {
	Source         string `yaml:"source"`
	YahooBaseURL   string `yaml:"yahoo_base_url"`
	HTTPTimeoutSec int    `yaml:"http_timeout_sec"`
	Enabled        *bool  `yaml:"enabled"`
	Prefetch       *bool  `yaml:"prefetch"`
	// IntradayRefreshMin bounds how long the current day's chart is served from
	// cache before re-fetching (minutes; default 15). NightlyRefresh is the local
	// HH:MM for a daily catch-up that completes recent days + backfills gaps
	// ("" disables it; default "00:30").
	IntradayRefreshMin int    `yaml:"intraday_refresh_min"`
	NightlyRefresh     string `yaml:"nightly_refresh"`
}

// NightlyRefreshHM parses NightlyRefresh ("HH:MM") into hour+minute. ok is false
// when it is empty (disabled) or malformed.
func (m MarketData) NightlyRefreshHM() (hour, min int, ok bool) {
	s := strings.TrimSpace(m.NightlyRefresh)
	if s == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	mn, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || mn < 0 || mn > 59 {
		return 0, 0, false
	}
	return h, mn, true
}

// Backup controls the periodic on-disk snapshot. When Enabled, the app writes
// one archive at startup (deduped per day) and one daily at Hour local time
// into Dir. Retention keeps the 10 newest plus one per calendar month forever.
type Backup struct {
	Dir     string `yaml:"dir"`
	Hour    int    `yaml:"hour"` // 0-23, local server time
	Enabled bool   `yaml:"enabled"`
}

// Auth controls the multi-user authentication layer. Mode "local" runs the
// built-in login UI with bcrypt + sqlite-backed sessions. Mode "proxy" trusts
// identity headers set by an upstream reverse proxy (Authelia, oauth2-proxy,
// Caddy forward_auth). BootstrapUser is the admin username assigned ownership
// of all pre-multiuser data; it must be present on first boot and is then
// inert.
type Auth struct {
	Mode            string    `yaml:"mode"`
	BootstrapUser   string    `yaml:"bootstrap_user"`
	SessionSecret   string    `yaml:"session_secret"`
	SessionTTLHours int       `yaml:"session_ttl_hours"`
	// CookieSecure marks the session cookie Secure (browser only sends it
	// over HTTPS). Nil = default true. Set to false for plain-HTTP deploys
	// on a trusted VPN where TLS isn't terminated in front of the app.
	CookieSecure *bool     `yaml:"cookie_secure"`
	Proxy        ProxyAuth `yaml:"proxy"`
}

type ProxyAuth struct {
	UserHeader        string   `yaml:"user_header"`
	EmailHeader       string   `yaml:"email_header"`
	DisplayNameHeader string   `yaml:"display_name_header"`
	GroupsHeader      string   `yaml:"groups_header"`
	AdminGroup        string   `yaml:"admin_group"`
	TrustedProxies    []string `yaml:"trusted_proxies"`
	AutoProvision     bool     `yaml:"auto_provision"`
	LogoutURL         string   `yaml:"logout_url"`
}

type Server struct {
	Port int `yaml:"port"`
}

type Database struct {
	Path string `yaml:"path"`
}

type Imports struct {
	Root string `yaml:"root"`
}

// OIDashboards points at the folder where an external pipeline drops
// daily Open Interest dashboards (HTML + PDF pairs named
// "dashboard-YYYY-MM-DD.{html,pdf}"). The /oi view lists everything
// in this folder and serves the latest by default.
type OIDashboards struct {
	Root string `yaml:"root"`
}

func defaults() Config {
	return Config{
		Server:       Server{Port: 8080},
		Database:     Database{Path: "./data/trading.db"},
		Imports:      Imports{Root: "./imports"},
		OIDashboards: OIDashboards{Root: "./oi_dashboard"},
		Auth: Auth{
			Mode:            "local",
			BootstrapUser:   "admin",
			SessionTTLHours: 168,
			Proxy: ProxyAuth{
				UserHeader:  "Remote-User",
				EmailHeader: "Remote-Email",
				GroupsHeader: "Remote-Groups",
				AdminGroup:  "admin",
			},
		},
		Backup:     Backup{Dir: "./backups", Hour: 4, Enabled: true},
		MarketData: MarketData{Source: "yahoo", HTTPTimeoutSec: 10, IntradayRefreshMin: 15, NightlyRefresh: "00:30"},
		AI:         AI{Provider: "openai", BaseURL: "http://0.0.0.0:8080/v1", TimeoutSec: 120, MaxTokens: 1024, Temperature: 0.4},
		Timezone:   "America/Chicago",
	}
}

// Validate checks the Auth section for misconfiguration that would make the
// server unsafe to start. Called from cmd/server after Load.
func (c Config) Validate() error {
	a := c.Auth
	if a.BootstrapUser == "" {
		return fmt.Errorf("auth.bootstrap_user is required")
	}
	switch a.Mode {
	case "", "local":
		// SessionSecret is required for CSRF HMAC; auto-generate at startup
		// if empty would be convenient but defeats restart-stable cookies.
		// Caller (cmd/server) is responsible for generating + persisting one
		// if absent — see ensureSessionSecret in main.go.
	case "proxy":
		if len(a.Proxy.TrustedProxies) == 0 {
			return fmt.Errorf("auth.mode=proxy requires auth.proxy.trusted_proxies")
		}
		if a.Proxy.UserHeader == "" {
			return fmt.Errorf("auth.mode=proxy requires auth.proxy.user_header")
		}
	default:
		return fmt.Errorf("auth.mode must be \"local\" or \"proxy\", got %q", a.Mode)
	}
	if c.Backup.Enabled && (c.Backup.Hour < 0 || c.Backup.Hour > 23) {
		return fmt.Errorf("backup.hour must be 0-23, got %d", c.Backup.Hour)
	}
	switch c.MarketData.Source {
	case "", "yahoo":
	default:
		return fmt.Errorf("market_data.source must be \"yahoo\" or empty, got %q", c.MarketData.Source)
	}
	if c.MarketData.HTTPTimeoutSec < 0 {
		return fmt.Errorf("market_data.http_timeout_sec must be >= 0, got %d", c.MarketData.HTTPTimeoutSec)
	}
	if c.MarketData.NightlyRefresh != "" {
		if _, _, ok := c.MarketData.NightlyRefreshHM(); !ok {
			return fmt.Errorf("market_data.nightly_refresh must be \"HH:MM\" (00:00-23:59) or empty, got %q", c.MarketData.NightlyRefresh)
		}
	}
	if c.AI.Enabled {
		switch c.AI.Provider {
		case "", "openai":
		default:
			return fmt.Errorf("ai.provider must be \"openai\" (OpenAI-compatible) for now, got %q", c.AI.Provider)
		}
		if c.AI.BaseURL == "" {
			return fmt.Errorf("ai.enabled requires ai.base_url")
		}
		if c.AI.Model == "" {
			return fmt.Errorf("ai.enabled requires ai.model")
		}
		if c.AI.TimeoutSec < 0 {
			return fmt.Errorf("ai.timeout_sec must be >= 0, got %d", c.AI.TimeoutSec)
		}
	}
	return nil
}

// Load reads cfgPath if it exists, otherwise returns defaults.
// A missing file is not an error: defaults are sensible for first boot.
// Environment variables override the YAML for a small allowlist of
// secret-like fields - currently TJ_SESSION_SECRET. Env wins over file so
// you can ship a checked-in config.yaml and inject the secret at run
// time (Docker secrets, systemd EnvironmentFile, etc.).
func Load(cfgPath string) (Config, error) {
	cfg := defaults()
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return cfg, fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		// No file is OK; we still consult env overrides below.
	} else if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", abs, err)
	} else if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	if v := os.Getenv("TJ_SESSION_SECRET"); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := os.Getenv("TJ_AI_API_KEY"); v != "" {
		cfg.AI.APIKey = v
	}

	return cfg, nil
}
