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
	Timezone     string       `yaml:"timezone"`
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
		Timezone:     "America/Chicago",
	}
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
