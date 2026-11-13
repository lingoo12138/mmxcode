// Package config loads the gateway's configuration from a YAML file.
//
// A file only needs to carry the keys it changes: Load starts from
// Default(), so a config with one key keeps the rest of the defaults.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration written in YAML as "5m", "120s", "1h30m".
type Duration time.Duration

// UnmarshalYAML reads a duration from a string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the duration back as a string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// String is the duration's own text.
func (d Duration) String() string { return time.Duration(d).String() }

// Duration gives the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Server holds the listener and the keys the gateway accepts.
type Server struct {
	Listen  string   `yaml:"listen"`
	APIKeys []string `yaml:"api_keys"`
}

// Proxy holds the global proxy and the named egress groups.
//
// The groups are the names an account can be pointed at; the addresses
// live here, so moving a group moves every account that uses it.
type Proxy struct {
	Default string            `yaml:"default"`
	Groups  map[string]string `yaml:"groups"`
}

// Upstream holds the upstream site's settings.
type Upstream struct {
	Timeout Duration `yaml:"timeout"`
	Site    string   `yaml:"site"`
}

// Rotation holds how accounts are picked and when one is given up on.
type Rotation struct {
	Strategy          string   `yaml:"strategy"`
	MaxAttempts       int      `yaml:"max_attempts"`
	ExhaustedCooldown Duration `yaml:"exhausted_cooldown"`
}

// Schedule holds the timers.
type Schedule struct {
	CheckinAt    string   `yaml:"checkin_at"`
	UsageRefresh Duration `yaml:"usage_refresh"`
	TokenRefresh Duration `yaml:"token_refresh"`
	EgressHealth Duration `yaml:"egress_health"`
}

// Config is the whole configuration.
type Config struct {
	Server   Server   `yaml:"server"`
	Proxy    Proxy    `yaml:"proxy"`
	Upstream Upstream `yaml:"upstream"`
	Rotation Rotation `yaml:"rotation"`
	Schedule Schedule `yaml:"schedule"`
	DB       string   `yaml:"db"`
}

// Default is the configuration a file starts from.
func Default() Config {
	return Config{
		Server:   Server{Listen: ":8080"},
		Proxy:    Proxy{Default: "", Groups: map[string]string{}},
		Upstream: Upstream{Timeout: Duration(120 * time.Second), Site: "global"},
		Rotation: Rotation{Strategy: "sticky", MaxAttempts: 3, ExhaustedCooldown: Duration(5 * time.Minute)},
		Schedule: Schedule{
			CheckinAt:    "09:00",
			UsageRefresh: Duration(15 * time.Minute),
			TokenRefresh: Duration(5 * time.Minute),
			EgressHealth: Duration(2 * time.Minute),
		},
		DB: "/data/mmxcode.db",
	}
}

// Load reads the configuration at path over the defaults.
//
// An empty path gives the defaults alone, which is what the CLI wants
// when it runs without a config file.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Proxy.Groups == nil {
		c.Proxy.Groups = map[string]string{}
	}
	return c, nil
}
