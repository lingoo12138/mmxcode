// Package config loads the gateway's configuration from a YAML file.
//
// A file only needs to carry the keys it changes: Load starts from
// Default(), so a config with one key keeps the rest of the defaults.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration written in YAML as "5m", "120s", "1h30m".
//
// It is never zero. Every place that reads one treats "not positive" as
// "no limit at all" or "fall back to the default", so a zero would be
// read as something the writer did not mean. Zero is refused where the
// configuration is read.
type Duration time.Duration

// UnmarshalYAML reads a duration from a string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the duration back as a string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// MarshalJSON writes the duration as a string.
//
// Not as a number of nanoseconds: the WebUI reads the configuration as
// JSON, and 120000000000 means nothing to it.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a duration from a string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// parseDuration reads a duration, refusing the impossible ones.
//
// Zero and negative are refused here, at the last gate before the
// configuration is used. Everywhere a timeout is read, "not positive"
// means either "no timeout at all" — a request that never comes back —
// or "fall back to the default" — a schedule that quietly runs at a
// time nobody asked for. Both are worse than refusing to start.
//
// There is no legitimate zero to protect: the defaults are all
// positive, so a zero in a file can only be a mistake.
func parseDuration(s string) (time.Duration, error) {
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be positive", s)
	}
	return v, nil
}

// String is the duration's own text.
func (d Duration) String() string { return time.Duration(d).String() }

// Duration gives the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Server holds the listener and the keys the gateway accepts.
type Server struct {
	Listen  string   `yaml:"listen" json:"listen"`
	APIKeys []string `yaml:"api_keys" json:"api_keys"`
}

// Proxy holds the global proxy and the named egress groups.
//
// The groups are the names an account can be pointed at; the addresses
// live here, so moving a group moves every account that uses it.
type Proxy struct {
	Default string            `yaml:"default" json:"default"`
	Groups  map[string]string `yaml:"groups" json:"groups"`
}

// Upstream holds the upstream site's settings.
type Upstream struct {
	Timeout Duration `yaml:"timeout" json:"timeout"`
	Site    string   `yaml:"site" json:"site"`
}

// Rotation holds how accounts are picked and when one is given up on.
type Rotation struct {
	Strategy          string   `yaml:"strategy" json:"strategy"`
	MaxAttempts       int      `yaml:"max_attempts" json:"max_attempts"`
	ExhaustedCooldown Duration `yaml:"exhausted_cooldown" json:"exhausted_cooldown"`
}

// Schedule holds the timers.
type Schedule struct {
	CheckinAt    string   `yaml:"checkin_at" json:"checkin_at"`
	UsageRefresh Duration `yaml:"usage_refresh" json:"usage_refresh"`
	TokenRefresh Duration `yaml:"token_refresh" json:"token_refresh"`
	EgressHealth Duration `yaml:"egress_health" json:"egress_health"`
}

// Batch is how a batch import runs.
type Batch struct {
	// Interval is the wait between two accounts.
	Interval Duration `yaml:"interval" json:"interval"`
	// Group is the egress group the logins go through. An empty one
	// is the default proxy.
	Group string `yaml:"group" json:"group"`
	// Relogin is the automatic signing-in again.
	Relogin Relogin `yaml:"relogin" json:"relogin"`
}

// Relogin is the automatic signing-in again.
type Relogin struct {
	// Enabled says whether it runs at all. It is off by default:
	// signing in again is the site's business, and a person should
	// say so.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Cooldown is how long the scheduler waits between two rounds.
	Cooldown Duration `yaml:"cooldown" json:"cooldown"`
	// MaxAccounts is how many accounts one round touches.
	MaxAccounts int `yaml:"max_accounts" json:"max_accounts"`
}

// Config is the whole configuration.
type Config struct {
	Server   Server   `yaml:"server" json:"server"`
	Proxy    Proxy    `yaml:"proxy" json:"proxy"`
	Upstream Upstream `yaml:"upstream" json:"upstream"`
	Rotation Rotation `yaml:"rotation" json:"rotation"`
	Schedule Schedule `yaml:"schedule" json:"schedule"`
	Batch    Batch    `yaml:"batch" json:"batch"`
	DB       string   `yaml:"db" json:"db"`
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
		Batch: Batch{
			Interval: Duration(3 * time.Second),
			Relogin: Relogin{
				Cooldown:    Duration(time.Hour),
				MaxAccounts: 20,
			},
		},
		DB: "/data/mmxcode.db",
	}
}

// Load reads the configuration at path over the defaults.
//
// An empty path gives the defaults alone, which is what the CLI wants
// when it runs without a config file.
//
// On an error the configuration returned is the zero value: a half-read
// configuration is worse than none, and the zero value must not be
// used. Whoever wants the defaults on an error has Default().
//
// The groups map belongs to the configuration it was read into, but a
// copy of the configuration shares it: whoever wants to change a group
// copies the map first (maps.Clone).
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	// Windows 上的编辑器爱写 BOM，它会混进第一个键名里，报错会指向
	// 一个根本不存在的字段。
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// 认不出的键是错字，不是新功能。直接报错，别让人以为配上了。
	//
	// 注意这只管结构体的字段。map 的键名管不了：出口组的名字是配置
	// 自己定的，加载器没有一份已知的名单可比。组名写错由设置出口组的
	// 地方挡（CLI 与 WebUI 都查配置里有没有这个组）。
	dec.KnownFields(true)
	// 空文件（或只有注释）没有文档可解，这不是错误：用默认值就好。
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	// 一个文件里只该有一份配置。Decode 一次只读一个文档，第二个会被
	// 悄悄丢掉 —— 配置的一半就没了，连语法错都不报。宁可报错。
	if err := oneDocument(dec); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Proxy.Groups == nil {
		c.Proxy.Groups = map[string]string{}
	}
	if err := c.checkAPIKeys(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

// checkAPIKeys refuses the api keys that are not keys.
//
// 空的一把 key 等于没有 key：网关拿请求带来的 key 跟配置里的每一把比，
// 配置里有一把空串的时候，一个不带 key 的请求带来的也是空串，两边一
// 比就"相等"了——于是所有请求都过了这道门。
//
// 这跟"一把 key 都不配"不是一回事：那是不用认证（只该听在本机），配
// 的人自己知道。配了一把空的，是配错了，所以在这里就报错。
func (c *Config) checkAPIKeys() error {
	for i, k := range c.Server.APIKeys {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("server.api_keys[%d] is empty: remove it to run without authentication, or give it a real key", i)
		}
	}
	return nil
}

// oneDocument makes sure the file holds exactly one document.
//
// It reads every document there is, not just the second one: a file
// with "doc1 --- (empty) --- doc3" would slip past a check that only
// looked once, and doc3 — with its syntax errors and all — would go
// missing without a word.
func oneDocument(dec *yaml.Decoder) error {
	for {
		var extra yaml.Node
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !nullDocument(&extra) {
			return fmt.Errorf("more than one YAML document, at line %d", documentLine(&extra))
		}
	}
}

// documentLine is the line a document starts on, for the error message.
func documentLine(n *yaml.Node) int {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0].Line
	}
	return n.Line
}

// nullDocument is true for an empty document, which is what a file
// ending in a bare "---" holds.
func nullDocument(n *yaml.Node) bool {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return true
		}
		if n.Content[0] == nil {
			return true
		}
		return n.Content[0].Tag == "!!null"
	}
	return n.Tag == "!!null"
}
