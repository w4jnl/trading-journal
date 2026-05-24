package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       Server       `yaml:"server"`
	Database     Database     `yaml:"database"`
	Imports      Imports      `yaml:"imports"`
	OIDashboards OIDashboards `yaml:"oi_dashboards"`
	Auth         Auth         `yaml:"auth"`
	Timezone     string       `yaml:"timezone"`
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
	Proxy           ProxyAuth `yaml:"proxy"`
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
	return nil
}

// Load reads cfgPath if it exists, otherwise returns defaults.
// A missing file is not an error: defaults are sensible for first boot.
func Load(cfgPath string) (Config, error) {
	cfg := defaults()
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return cfg, fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", abs, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}
