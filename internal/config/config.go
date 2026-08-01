package config

import (
	"encoding/hex"
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
	GEX          GEX          `yaml:"gex"`
	TopstepX     TopstepX     `yaml:"topstepx"`
	IBKR         IBKR         `yaml:"ibkr"`
	MWEmail      MWEmail      `yaml:"mw_email"`
	Timezone     string       `yaml:"timezone"`
}

// MWEmail configures the embedded SMTP listener that captures MotiveWave
// "Send Email On Filled Order" notifications - near-real-time fills for
// accounts without an API connector (Rithmic props, AMP/CQG). Every message
// is archived verbatim to ArchiveDir; complete fill notifications are also
// ingested as PROVISIONAL executions (unless Ingest is disabled), which the
// authoritative MotiveWave CSV import later supersedes via the shared
// (account, order id) key. The listener must only ever bind a LAN/VPN
// interface - it is not a mail server.
//
// MotiveWave infers encryption from the port number it is given: 25 = plain,
// 465 = implicit TLS, 587 = STARTTLS. Match TLSMode to the port MW dials.
type MWEmail struct {
	Enabled    bool   `yaml:"enabled"`       // default false
	ListenAddr string `yaml:"listen_addr"`   // default ":2525"
	TLSMode    string `yaml:"tls_mode"`      // "off" (default) | "implicit" | "starttls"
	TLSCert    string `yaml:"tls_cert"`      // PEM cert path; required unless tls_mode=off
	TLSKey     string `yaml:"tls_key"`       // PEM key path; required unless tls_mode=off
	Username   string `yaml:"smtp_username"` // both set => AUTH (PLAIN/LOGIN) required
	Password   string `yaml:"smtp_password"`
	ArchiveDir string `yaml:"archive_dir"` // default "data/mw-raw"; verbatim .eml audit trail
	Ingest     *bool  `yaml:"ingest"`      // nil = true; false = capture/archive only, no DB writes
}

// IngestEnabled resolves the tri-state Ingest flag (nil defaults to on).
func (m MWEmail) IngestEnabled() bool {
	return m.Ingest == nil || *m.Ingest
}

// IBKR configures live sync via Client Portal Gateway(s). Only global knobs
// live here - the gateways themselves (URL, TLS mode) are per-user data
// managed in the settings UI, since a gateway belongs to a journal user and
// holds no secrets (its session is established by the user's browser login).
// Scheduling mirrors TopstepX: a page-visit throttle plus a two-tier
// (RTH/session) scheduler gated to CME Globex hours.
type IBKR struct {
	Enabled         *bool  `yaml:"enabled"`           // nil = true
	TickleSec       int    `yaml:"tickle_sec"`        // per-gateway keep-alive; default 60; 0 disables
	SyncThrottleMin int    `yaml:"sync_throttle_min"` // min minutes between page-visit syncs per user; default 15
	SessionSyncMin  int    `yaml:"session_sync_min"`  // scheduled cadence during Globex hours; default 60; 0 disables
	RTHSyncMin      int    `yaml:"rth_sync_min"`      // faster cadence inside rth_window; default 15; 0 disables
	RTHWindow       string `yaml:"rth_window"`        // "HH:MM-HH:MM" Chicago, Mon-Fri; default "07:30-16:00"
}

// RTHWindowHM parses RTHWindow into start/end minutes-of-day (Chicago).
func (i IBKR) RTHWindowHM() (startMin, endMin int, ok bool) {
	return parseHMWindow(i.RTHWindow)
}

// TopstepX configures the TopstepX (ProjectX Gateway) API sync. The feature
// activates only when EncryptionKey is set: per-user API credentials are
// stored AES-256-GCM encrypted under it, so it is deliberately NOT
// auto-generated - losing it bricks every stored credential, so it must live
// in the user's backed-up config.yaml (or the TJ_TOPSTEPX_ENC_KEY env var).
// Generate one with: openssl rand -hex 32
type TopstepX struct {
	Enabled         *bool  `yaml:"enabled"`           // nil = true; feature still gated on EncryptionKey
	BaseURL         string `yaml:"base_url"`          // default https://api.topstepx.com
	EncryptionKey   string `yaml:"encryption_key"`    // 64 hex chars (32 bytes); env TJ_TOPSTEPX_ENC_KEY
	HTTPTimeoutSec  int    `yaml:"http_timeout_sec"`  // default 15
	SyncThrottleMin int    `yaml:"sync_throttle_min"` // min minutes between page-visit syncs per user; default 15

	// Scheduled-sync cadences. The scheduler runs at SessionSyncMin
	// throughout the CME Globex session (plus a 2h margin each side) and
	// speeds up to RTHSyncMin inside RTHWindow. 0 disables that tier.
	SessionSyncMin int `yaml:"session_sync_min"` // default 60
	RTHSyncMin     int `yaml:"rth_sync_min"`     // default 15
	// RTHWindow is the fast-cadence window, "HH:MM-HH:MM" in Chicago time,
	// Monday-Friday. Default "07:30-16:00" = equity-futures RTH
	// (08:30-15:00 CT) plus one hour before and after.
	RTHWindow string `yaml:"rth_window"`
}

// RTHWindowHM parses RTHWindow into start/end minutes-of-day (Chicago).
// ok=false when the string is not "HH:MM-HH:MM" with start < end.
func (t TopstepX) RTHWindowHM() (startMin, endMin int, ok bool) {
	return parseHMWindow(t.RTHWindow)
}

// parseHMWindow parses "HH:MM-HH:MM" into start/end minutes-of-day; ok=false
// when malformed or start >= end. Shared by the TopstepX and IBKR RTH windows.
func parseHMWindow(window string) (startMin, endMin int, ok bool) {
	parts := strings.Split(window, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	parse := func(s string) (int, bool) {
		hm := strings.Split(strings.TrimSpace(s), ":")
		if len(hm) != 2 {
			return 0, false
		}
		h, err1 := strconv.Atoi(hm[0])
		m, err2 := strconv.Atoi(hm[1])
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, false
		}
		return h*60 + m, true
	}
	start, ok1 := parse(parts[0])
	end, ok2 := parse(parts[1])
	if !ok1 || !ok2 || start >= end {
		return 0, 0, false
	}
	return start, end, true
}

// EncryptionKeyBytes decodes the hex key; ok=false when unset or malformed.
func (t TopstepX) EncryptionKeyBytes() ([]byte, bool) {
	if t.EncryptionKey == "" {
		return nil, false
	}
	b, err := hex.DecodeString(strings.TrimSpace(t.EncryptionKey))
	if err != nil || len(b) != 32 {
		return nil, false
	}
	return b, true
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

// GEX configures the gamma-exposure context annotations (design §8 / P4): during
// daily review prep, each ES/NQ trade entry gets a regime/flip/wall block fetched
// from a gex-engine instance's snapshot API. Disabled by default; the whole
// feature is gated on Enabled. APIKey is the engine's bearer token (its
// GEX_API_TOKEN) and can be injected via the TJ_GEX_API_KEY env var.
type GEX struct {
	Enabled    bool   `yaml:"enabled"`
	BaseURL    string `yaml:"base_url"`
	APIKey     string `yaml:"api_key"`
	TimeoutSec int    `yaml:"timeout_sec"`
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
	Mode            string `yaml:"mode"`
	BootstrapUser   string `yaml:"bootstrap_user"`
	SessionSecret   string `yaml:"session_secret"`
	SessionTTLHours int    `yaml:"session_ttl_hours"`
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
				UserHeader:   "Remote-User",
				EmailHeader:  "Remote-Email",
				GroupsHeader: "Remote-Groups",
				AdminGroup:   "admin",
			},
		},
		Backup:     Backup{Dir: "./backups", Hour: 4, Enabled: true},
		MarketData: MarketData{Source: "yahoo", HTTPTimeoutSec: 10, IntradayRefreshMin: 15, NightlyRefresh: "00:30"},
		AI:         AI{Provider: "openai", BaseURL: "http://0.0.0.0:8080/v1", TimeoutSec: 120, MaxTokens: 1024, Temperature: 0.4},
		GEX:        GEX{TimeoutSec: 5},
		TopstepX: TopstepX{
			BaseURL: "https://api.topstepx.com", HTTPTimeoutSec: 15, SyncThrottleMin: 15,
			SessionSyncMin: 60, RTHSyncMin: 15, RTHWindow: "07:30-16:00",
		},
		IBKR: IBKR{
			TickleSec: 60, SyncThrottleMin: 15,
			SessionSyncMin: 60, RTHSyncMin: 15, RTHWindow: "07:30-16:00",
		},
		MWEmail:  MWEmail{ListenAddr: ":2525", TLSMode: "off", ArchiveDir: "data/mw-raw"},
		Timezone: "America/Chicago",
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
	if c.GEX.Enabled {
		if c.GEX.BaseURL == "" {
			return fmt.Errorf("gex.enabled requires gex.base_url")
		}
		if c.GEX.TimeoutSec < 0 {
			return fmt.Errorf("gex.timeout_sec must be >= 0, got %d", c.GEX.TimeoutSec)
		}
	}
	if c.TopstepX.EncryptionKey != "" {
		if _, ok := c.TopstepX.EncryptionKeyBytes(); !ok {
			return fmt.Errorf("topstepx.encryption_key must be 64 hex characters (generate with: openssl rand -hex 32)")
		}
	}
	if c.TopstepX.HTTPTimeoutSec < 0 {
		return fmt.Errorf("topstepx.http_timeout_sec must be >= 0, got %d", c.TopstepX.HTTPTimeoutSec)
	}
	if c.TopstepX.SyncThrottleMin < 0 {
		return fmt.Errorf("topstepx.sync_throttle_min must be >= 0, got %d", c.TopstepX.SyncThrottleMin)
	}
	if c.TopstepX.SessionSyncMin < 0 {
		return fmt.Errorf("topstepx.session_sync_min must be >= 0 (0 disables), got %d", c.TopstepX.SessionSyncMin)
	}
	if c.TopstepX.RTHSyncMin < 0 {
		return fmt.Errorf("topstepx.rth_sync_min must be >= 0 (0 disables), got %d", c.TopstepX.RTHSyncMin)
	}
	if c.TopstepX.RTHSyncMin > 0 {
		if _, _, ok := c.TopstepX.RTHWindowHM(); !ok {
			return fmt.Errorf("topstepx.rth_window must be \"HH:MM-HH:MM\" (Chicago time, start < end), got %q", c.TopstepX.RTHWindow)
		}
	}
	if c.IBKR.TickleSec < 0 {
		return fmt.Errorf("ibkr.tickle_sec must be >= 0 (0 disables), got %d", c.IBKR.TickleSec)
	}
	if c.IBKR.SyncThrottleMin < 0 {
		return fmt.Errorf("ibkr.sync_throttle_min must be >= 0, got %d", c.IBKR.SyncThrottleMin)
	}
	if c.IBKR.SessionSyncMin < 0 {
		return fmt.Errorf("ibkr.session_sync_min must be >= 0 (0 disables), got %d", c.IBKR.SessionSyncMin)
	}
	if c.IBKR.RTHSyncMin < 0 {
		return fmt.Errorf("ibkr.rth_sync_min must be >= 0 (0 disables), got %d", c.IBKR.RTHSyncMin)
	}
	if c.IBKR.RTHSyncMin > 0 {
		if _, _, ok := c.IBKR.RTHWindowHM(); !ok {
			return fmt.Errorf("ibkr.rth_window must be \"HH:MM-HH:MM\" (Chicago time, start < end), got %q", c.IBKR.RTHWindow)
		}
	}
	if c.MWEmail.Enabled {
		switch c.MWEmail.TLSMode {
		case "", "off":
		case "implicit", "starttls":
			if c.MWEmail.TLSCert == "" || c.MWEmail.TLSKey == "" {
				return fmt.Errorf("mw_email.tls_mode=%s requires mw_email.tls_cert and tls_key", c.MWEmail.TLSMode)
			}
			for _, p := range []string{c.MWEmail.TLSCert, c.MWEmail.TLSKey} {
				if _, err := os.Stat(p); err != nil {
					return fmt.Errorf("mw_email tls file %q: %w", p, err)
				}
			}
		default:
			return fmt.Errorf("mw_email.tls_mode must be \"off\", \"implicit\" or \"starttls\", got %q", c.MWEmail.TLSMode)
		}
		if (c.MWEmail.Username == "") != (c.MWEmail.Password == "") {
			return fmt.Errorf("mw_email.smtp_username and smtp_password must be set together")
		}
		if c.MWEmail.ListenAddr == "" {
			return fmt.Errorf("mw_email.listen_addr is required when mw_email is enabled")
		}
		if c.MWEmail.ArchiveDir == "" {
			return fmt.Errorf("mw_email.archive_dir is required when mw_email is enabled")
		}
	}
	return nil
}

// Load reads cfgPath if it exists, otherwise returns defaults.
// A missing file is not an error: defaults are sensible for first boot.
// Environment variables override the YAML for a small allowlist of
// secret-like fields (TJ_SESSION_SECRET, TJ_AI_API_KEY, TJ_GEX_API_KEY,
// TJ_TOPSTEPX_ENC_KEY). Env wins over file so you can ship a checked-in
// config.yaml and inject the secret at run time (Docker secrets, systemd
// EnvironmentFile, etc.).
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
	if v := os.Getenv("TJ_GEX_API_KEY"); v != "" {
		cfg.GEX.APIKey = v
	}
	if v := os.Getenv("TJ_TOPSTEPX_ENC_KEY"); v != "" {
		cfg.TopstepX.EncryptionKey = v
	}

	return cfg, nil
}
