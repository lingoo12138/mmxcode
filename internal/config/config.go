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
// 有一个地方的零是合法的：usage.refresh.scan_every。那一个键用的是
// 自己的类型（OptionalDuration），零在那里是"间隔交回
// schedule.usage_refresh" —— 是人写的意思。别的键没有这个意思，所以
// 它们还是拒绝零。
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

// parseOptionalDuration reads a duration that may be zero.
//
// 零在这里是许的：只有一个键有合法的 0（usage.refresh.scan_every），
// 它说的是"间隔交回 schedule.usage_refresh"。负数仍然不是：负的间隔
// 没有意思，而且它会顺着"小于等于 0 就按默认来"那条路，变成一个没人
// 要的间隔。
func parseOptionalDuration(s string) (time.Duration, error) {
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	return v, nil
}

// OptionalDuration is a time.Duration that may be zero.
//
// 它只给一个键用：usage.refresh.scan_every。那个键的零有确切的意思
// ——"间隔交回 schedule.usage_refresh"（规格 §9）—— 所以它得写得进去。
//
// 别的时长仍然拒绝零：零在那边是"没有时限"或者"悄悄按默认来"，两个
// 都比拒绝更坏。那些地方用的是 Duration。
type OptionalDuration time.Duration

// UnmarshalYAML reads a duration from a string.
//
// 零是许的，负数不是。
func (d *OptionalDuration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := parseOptionalDuration(s)
	if err != nil {
		return err
	}
	*d = OptionalDuration(v)
	return nil
}

// MarshalYAML writes the duration back as a string.
func (d OptionalDuration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// MarshalJSON writes the duration as a string.
//
// Not as a number of nanoseconds: the WebUI reads the configuration as
// JSON, and 120000000000 means nothing to it.
func (d OptionalDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a duration from a string.
func (d *OptionalDuration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := parseOptionalDuration(s)
	if err != nil {
		return err
	}
	*d = OptionalDuration(v)
	return nil
}

// String is the duration's own text.
func (d OptionalDuration) String() string { return time.Duration(d).String() }

// Duration gives the value as a time.Duration.
func (d OptionalDuration) Duration() time.Duration { return time.Duration(d) }

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
	//
	// 它是常驻的：网页上传和命令行没有另外指定的时候，导入都走这个组。
	// 导入带了组的时候，账号自己那个出口地址要让位——出口是按「先看地
	// 址、再看组」找的，地址留着就还是从那个地方出去。所以配了这个组
	// 之后，清单里那些账号自己手填的地址会被清掉，导入结果里会写明
	// 清掉的是哪个地址。
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

// Usage holds how the credits are read.
type Usage struct {
	// Refresh holds how one round of reading the credits goes.
	Refresh Refresh `yaml:"refresh" json:"refresh"`
}

// Refresh holds the controls of one round of reading the credits.
//
// 一趟读额度有四道控：并发（读得快，不是读得多）、每个账号的时限
// （一个卡住的账号不能拖住整趟）、一趟的预算（预算用完就停）、一趟
// 最多读几个（剩下的下一趟接着读）。
//
// 配 0 的时候按 1 算：0 并发、0 个一批都是一趟也读不了，那不是关掉，
// 是坏了。
type Refresh struct {
	// HotWindow is how long an account counts as hot after it was used.
	HotWindow Duration `yaml:"hot_window" json:"hot_window"`
	// ScanEvery is how often the accounts that should be read are read.
	//
	// 它是唯一可以写 0 的时长：零说的是"间隔交回
	// schedule.usage_refresh"。
	ScanEvery OptionalDuration `yaml:"scan_every" json:"scan_every"`
	// Concurrency is how many accounts one round reads at once.
	Concurrency int `yaml:"concurrency" json:"concurrency"`
	// Timeout is how long one account's reading may take.
	Timeout Duration `yaml:"timeout" json:"timeout"`
	// Budget is how long one round may take.
	Budget Duration `yaml:"budget" json:"budget"`
	// Batch is how many accounts one round reads at most.
	Batch int `yaml:"batch" json:"batch"`
	// Card is how many accounts the card's own refresh reads.
	Card int `yaml:"card" json:"card"`
	// CardBudget is how long the card's own refresh may take.
	CardBudget Duration `yaml:"card_budget" json:"card_budget"`
}

// Concurrency holds how many requests may be in the air at once.
//
// 配 0 的时候按 1 算：0 并发是一个请求也进不来，那不是关掉，是坏了。
type Concurrency struct {
	// PerAccount is how many requests one account takes at once.
	PerAccount int `yaml:"per_account" json:"per_account"`
	// TotalRatio is how many accounts share one place (5 = one in five).
	//
	// 五分之一：100 个账号同时只用 20 个，这 20 个把额度用完了，还有
	// 80 个是满的可以接。一个接一个地烧，而不是所有账号一起烧到没。
	TotalRatio int `yaml:"total_ratio" json:"total_ratio"`
	// TotalMin is the floor for a small number of accounts.
	//
	// 5 个账号只跑 1 个太憋屈：小规模的时候用 min(账号数, 3)。
	TotalMin int `yaml:"total_min" json:"total_min"`
	// Wait is how long a request waits for a free place.
	Wait Duration `yaml:"wait" json:"wait"`
}

// Pool holds how many accounts the gateway keeps in memory.
//
// 池是缓存，不是真相（规格不变量 1）：账号、耗尽标记、会话绑定的真相都
// 在库里。池存在的理由是选号不用每次扫全表，还有内存有个上界。
//
// 池多大 = max(总并发, 活跃会话数) × 倍数。倍数是 2：总并发是「该跑多
// 少个请求」，活跃会话是「有多少段对话在用」，池要装得下它们（会话钉
// 的号不在池里会被改绑，缓存命中就没了），再留一倍给补号。
type Pool struct {
	// Factor is how many times over the demand the pool holds.
	//
	// 池装几个号 = max(总并发, 活跃会话数) × 这个倍数（2 = 装两倍）。
	//
	// 配 0 按 1 算：0 倍数是「池里一个号都没有」，那不是关掉，是坏了。
	Factor int `yaml:"factor" json:"factor"`
	// MaxSize caps the pool by the number of accounts. 0 means no cap.
	//
	// 配 0 是不限：这时候池的大小由内存预算和库里能用的号数说了算。
	MaxSize int `yaml:"max_size" json:"max_size"`
	// MemoryBudgetMB is how much memory the pool may take.
	//
	// 一个号按 512 字节算。配 0 是不设内存上限。
	MemoryBudgetMB int `yaml:"memory_budget_mb" json:"memory_budget_mb"`
	// SessionWindow is how long a session counts as active.
	//
	// 「活跃」= 这段时间里被碰过的会话，按最后一次碰的时间算。会话表从
	// 来不删，所以数的是最近碰过的，不是一共有的。
	//
	// 配 0 是错的（配置文件会拒），因为一个没有窗口的「活跃」数不清楚。
	SessionWindow Duration `yaml:"session_window" json:"session_window"`
	// ReadOnAdmit says whether a new account's credits are read before
	// it goes into the pool.
	//
	// 读一次是准的（读到的数说它有额度，它才进池），但读一次是 2~11 个
	// 上游请求。冷启动的填池不等这个读：池要立刻能用（规格 §6.3）。
	ReadOnAdmit bool `yaml:"read_on_admit" json:"read_on_admit"`
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

	// Usage holds how the credits are read.
	Usage Usage `yaml:"usage" json:"usage"`
	// Concurrency holds how many requests may be in the air at once.
	Concurrency Concurrency `yaml:"concurrency" json:"concurrency"`
	// Pool holds how many accounts the gateway keeps in memory.
	Pool Pool `yaml:"pool" json:"pool"`
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
		Usage: Usage{Refresh: Refresh{
			HotWindow:   Duration(5 * time.Minute),
			ScanEvery:   OptionalDuration(30 * time.Second),
			Concurrency: 4,
			Timeout:     Duration(15 * time.Second),
			Budget:      Duration(30 * time.Second),
			Batch:       6,
			Card:        2,
			CardBudget:  Duration(5 * time.Second),
		}},
		Concurrency: Concurrency{
			PerAccount: 1,
			TotalRatio: 5,
			TotalMin:   3,
			Wait:       Duration(10 * time.Second),
		},
		Pool: Pool{
			Factor:         2,
			MaxSize:        0,
			MemoryBudgetMB: 32,
			SessionWindow:  Duration(time.Hour),
			ReadOnAdmit:    true,
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
