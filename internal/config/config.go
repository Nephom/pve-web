package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version int      `yaml:"version"`
	Server  Server   `yaml:"server"`
	Refresh Refresh  `yaml:"refresh"`
	Logging Logging  `yaml:"logging"`
	Targets []Target `yaml:"targets"`
}

type Server struct {
	Listen    string `yaml:"listen"`
	BasePath  string `yaml:"base_path"`
	StaticDir string `yaml:"static_dir"`
	HTTPS     HTTPS  `yaml:"https"`
}

type HTTPS struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}
type Refresh struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	MetricsSeconds  int `yaml:"metrics_seconds"`
	HistoryMinutes  int `yaml:"history_minutes"`
}
type Logging struct {
	Enabled bool   `yaml:"enabled"`
	File    string `yaml:"file"`
}
type Target struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	Type       string   `yaml:"type"`
	Enabled    bool     `yaml:"enabled"`
	VerifyTLS  bool     `yaml:"verify_tls"`
	DetectHA   *bool    `yaml:"detect_ha,omitempty"`
	DetectCeph *bool    `yaml:"detect_ceph,omitempty"`
	Endpoints  []string `yaml:"endpoints"`
}

func Defaults() Config {
	return Config{Version: 1, Server: Server{Listen: "127.0.0.1:8080", BasePath: "/pve-web", StaticDir: "web/dist", HTTPS: HTTPS{CertFile: "/usr/local/etc/pve-web/pve-web.crt", KeyFile: "/usr/local/etc/pve-web/pve-web.key"}}, Refresh: Refresh{IntervalSeconds: 5, MetricsSeconds: 5, HistoryMinutes: 5}, Logging: Logging{Enabled: true, File: "pve-web.log"}}
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	c := Defaults()
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen cannot be empty")
	}
	if c.Server.BasePath == "" {
		return fmt.Errorf("server.base_path cannot be empty")
	}
	if c.Server.BasePath[0] != '/' {
		return fmt.Errorf("server.base_path must start with /")
	}
	if c.Server.StaticDir == "" {
		return fmt.Errorf("server.static_dir cannot be empty")
	}
	if c.Refresh.IntervalSeconds < 1 || c.Refresh.MetricsSeconds < 1 || c.Refresh.HistoryMinutes < 1 {
		return fmt.Errorf("refresh values must be positive")
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if t.ID == "" || t.Name == "" || len(t.Endpoints) == 0 {
			return fmt.Errorf("target requires id, name and endpoint")
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate target %q", t.ID)
		}
		seen[t.ID] = true
	}
	return nil
}

func (t Target) HAEnabled() bool {
	return t.DetectHA == nil && t.Type == "cluster" || t.DetectHA != nil && *t.DetectHA
}
func (t Target) CephEnabled() bool {
	return t.DetectCeph == nil && t.Type == "cluster" || t.DetectCeph != nil && *t.DetectCeph
}
func Duration(seconds int) time.Duration { return time.Duration(seconds) * time.Second }
