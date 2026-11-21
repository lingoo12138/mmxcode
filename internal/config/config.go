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
// 有两个地方的零是合法的：usage.refresh.scan_every 和
// schedule.checkin_jitter。那两个键用的是自己的类型
// （OptionalDuration），零在第一个键那里是"间隔交回
// schedule.usage_refresh"，在第二个键那里是"不抖" —— 都是人写的意思。
// 别的键没有这个意思，所以它们还是拒绝零。
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
// 零在这里是许的：两个键有合法的 0（usage.refresh.scan_every、
// schedule.checkin_jitter），第一个说的是"间隔交回
// schedule.usage_refresh"，第二个说的是"不抖"。负数仍然不是：负的间隔
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
// 它只给两个键用：usage.refresh.scan_every 和
// schedule.checkin_jitter。那两个键的零有确切的意思
// ——"间隔交回 schedule.usage_refresh"（规格 §9）和"不抖"—— 所以它们
// 得写得进去。
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

// Proxy holds the global outlet.
//
// 出口是四级的：账号自己的地址（写在库里，页面上每一行都能改），账号指
// 着的组，然后是全局组（也是一个组，名字是保留的 default）。两个都没有
// 就是直连 —— 直连是最后一级，不是错。
type Proxy struct {
	// Default is the address the global group is made from.
	//
	// 它只是种子：开机的时候全局组不在库里，就用它建一个。组在库里之
	// 后，页面上改的是库里那份，配置里这个就不再管事了。
	Default string `yaml:"default" json:"default"`
	// Groups are the outlet groups of an older version.
	//
	// 也是种子：库里没有的组用它们建出来 —— 老库里那些指着组名的号，靠
	// 这一步还指着原来的那条路。库里已经有了的就是库里的算数。
	//
	// 老部署的配置文件里还有这个键，加载器得认它：认不出的键是错字，直
	// 接报错，那台机器就起不来了，而起不来的机器上的号就全废了。
	Groups map[string]string `yaml:"groups" json:"groups"`
}

// Upstream holds the upstream site's settings.
type Upstream struct {
	Timeout Duration `yaml:"timeout" json:"timeout"`
	Site    string   `yaml:"site" json:"site"`
	// MaxBodyMB is the most an upstream response body may be, in MiB.
	//
	// 它是**回包**的大小，不是请求的。回包的大小是上游的事：站点答一个正经
	// 问题的回答最多是它那个输出上限（524288 个 token，约 2 MB），所以 8
	// MiB 有三倍多的余量。
	//
	// 0 和负数不许（那是「没有上限」，正是要修的那个洞）。
	MaxBodyMB int `yaml:"max_body_mb" json:"max_body_mb"`
}

// Rotation holds how accounts are picked and when one is given up on.
type Rotation struct {
	Strategy          string   `yaml:"strategy" json:"strategy"`
	MaxAttempts       int      `yaml:"max_attempts" json:"max_attempts"`
	ExhaustedCooldown Duration `yaml:"exhausted_cooldown" json:"exhausted_cooldown"`
}

// Schedule holds the timers.
type Schedule struct {
	// CheckinAt is when the daily check-in runs, HH:MM.
	//
	// 它是**大陆时间**：站点按大陆时间算一天，签到要落在站点的那一天
	// 里（规格 §5.1）。
	CheckinAt string `yaml:"checkin_at" json:"checkin_at"`
	// CheckinJitter is how long the daily check-in may be held back.
	//
	// 它的零是有意思的：不抖。几个实例一起跑的时候，抖一下就散开了
	// （规格 §5.1）。
	CheckinJitter OptionalDuration `yaml:"checkin_jitter" json:"checkin_jitter"`
	UsageRefresh  Duration         `yaml:"usage_refresh" json:"usage_refresh"`
	TokenRefresh  Duration         `yaml:"token_refresh" json:"token_refresh"`
	EgressHealth  Duration         `yaml:"egress_health" json:"egress_health"`
}

// Batch is how a batch import runs.
type Batch struct {
	// Interval is the wait between two accounts.
	Interval Duration `yaml:"interval" json:"interval"`
	// LegacyGroup is the outlet group the logins went through, of an
	// older version.
	//
	// 退役了：导入不再有「走哪个组」这件事，它走全局默认出口。这个键只
	// 是还认着 —— 老配置里有它，认不出的键是错字，直接报错，那台机器就
	// 起不来了。
	LegacyGroup string `yaml:"group" json:"group"`
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
	// TotalMax is the ceiling on the total concurrency.
	//
	// 天花板：不管池子里有多少个号、库里有多少个号，承诺的并发不超过
	// 它。0 = 不限（用可用账号数 ÷ 5 算出来的那个数）。
	//
	// 它是「并发可控」的那根杆：池子的大小是按承诺的并发算的
	// （max(总并发, 活跃会话数) × 倍数），所以天花板定 64，池子就是
	// 128 个号 —— 「n×2」那个说法才有着落。
	//
	// 池空的时候它也在：那时候承诺从库里数，库里有多少号就承诺多少，
	// 那一下能从 20 抬到 80。天花板就是给这一下准备的。
	TotalMax int `yaml:"total_max" json:"total_max"`
	// Wait is how long a request waits for a free place.
	Wait Duration `yaml:"wait" json:"wait"`
	// BodySmallMB is how large a request body may be and still count as
	// small.
	//
	// 小的走普通并发，大的（几十兆 —— 图片就是那么大的）一次只让一个进
	// 来。两个大的一起来那是一百多兆，三个就更多了。
	//
	// 配 0 的时候按 1 算：那样每个请求都是大身体，网关就成了一条道。
	BodySmallMB int `yaml:"body_small_mb" json:"body_small_mb"`
	// BodyBudgetMB is how many bytes of small request bodies may be in
	// the air at once.
	//
	// 它是内存的天花板，跟有多少账号、承诺多少并发没关系：80 MiB 装得
	// 下 19 个 4 MiB 的小请求。
	//
	// 0 = 不限（跟 total_max 一个规矩）：那时候只剩并发闸门管着。
	BodyBudgetMB int `yaml:"body_budget_mb" json:"body_budget_mb"`
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

// SQLite is the database's own tuning.
//
// 它是「库怎么干活」的几件事：页缓存多大、写盘要不要每次都 fsync、签到
// 的历史留多少天。
type SQLite struct {
	// CacheMB is how many MB of pages one connection may keep.
	//
	// 页缓存是「读过的页先留在手边」：工作集装得下的时候，同一批查询不
	// 用一遍一遍地去磁盘要页（1000 个号、一年的签到：填池那条查询从
	// 11.7 ms 到 5.9 ms）。0 是 SQLite 自己的默认（2 MB）。
	//
	// 负数是不许的：它会悄悄按 0 办（store 的 dsn 只在 cacheMB > 0 的时
	// 候才加那条 pragma），写的人以为自己说了什么，其实什么也没说。
	CacheMB int `yaml:"cache_mb" json:"cache_mb"`
	// Synchronous is how hard a write hits the disk: "normal" or "full".
	//
	// "normal"：写下去就交给操作系统，断电的时候最近这几笔可能丢（库不
	// 会坏）。"full"：每一笔都 fsync（同一笔写：normal 64µs 对 full
	// 874µs —— full 那一笔要等盘）。空是 SQLite 自己的默认（"full"）。
	Synchronous string `yaml:"synchronous" json:"synchronous"`
	// CheckinRetentionDays is how many days of check-ins are kept.
	//
	// 签到的积分七天就过期了，网页上只摆最近 5 条 —— 再往前的记录除了
	// 占地方没有别的用处（1000 个号一年 36 万行、40 MB）。0 是永远留。
	//
	// 少于 7 天是不许的（负数也不行）：「待补」那一档只看最近七天的签
	// 到，而清理是按站点的那个日子删的，站点时区跟 UTC 差着几个钟头。
	// 留 6 天的话，被删掉的那一段里可能有七天窗口还要用的行 —— 那些号
	// 就永远等不到「签到待补」那一读，卡上摆的数就一直旧着。7 天是刚够
	// 的。
	CheckinRetentionDays int `yaml:"checkin_retention_days" json:"checkin_retention_days"`
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
	// SQLite holds how the database itself works.
	SQLite SQLite `yaml:"sqlite" json:"sqlite"`
}

// Default is the configuration a file starts from.
func Default() Config {
	return Config{
		Server:   Server{Listen: ":8080"},
		Proxy:    Proxy{Default: "", Groups: map[string]string{}},
		Upstream: Upstream{Timeout: Duration(120 * time.Second), Site: "global", MaxBodyMB: 8},
		Rotation: Rotation{Strategy: "sticky", MaxAttempts: 3, ExhaustedCooldown: Duration(5 * time.Minute)},
		Schedule: Schedule{
			CheckinAt:     "00:00",
			CheckinJitter: OptionalDuration(0),
			UsageRefresh:  Duration(15 * time.Minute),
			TokenRefresh:  Duration(5 * time.Minute),
			EgressHealth:  Duration(2 * time.Minute),
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
			PerAccount:   1,
			TotalRatio:   5,
			TotalMin:     3,
			TotalMax:     0,
			Wait:         Duration(10 * time.Second),
			BodySmallMB:  4,
			BodyBudgetMB: 80,
		},
		Pool: Pool{
			Factor:         2,
			MaxSize:        0,
			MemoryBudgetMB: 32,
			SessionWindow:  Duration(time.Hour),
			ReadOnAdmit:    true,
		},
		SQLite: SQLite{
			CacheMB:              8,
			Synchronous:          "normal",
			CheckinRetentionDays: 30,
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
	// 注意这只管结构体的字段。map 的键名管不了：退役的 proxy.groups 的
	// 组名是配置自己定的，加载器没有一份已知的名单可比。那个键只被读一
	// 次（开机的时候收拾账号身上的组名），读完了就不管了。
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
	if err := c.checkUpstream(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := c.checkSQLite(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

// checkUpstream refuses the settings that would mean something else.
//
// 回包能有多大：8 MiB 是站点自己那个输出上限的三倍多。0 和负数不许 —— 「没
// 有上限」不是一个选项：那正是要修的那个洞（聊天的回答从前完全没有闸）。
func (c *Config) checkUpstream() error {
	if c.Upstream.MaxBodyMB < 1 {
		return fmt.Errorf("upstream.max_body_mb is %d: it is 1 or more", c.Upstream.MaxBodyMB)
	}
	return nil
}

// checkSQLite refuses the tuning the database does not know, and the
// numbers that would quietly mean something else.
//
// 写盘的狠劲只有两档：normal 和 full。别的词 SQLite 不认，库打开的时候
// 才说，那时已经晚了一步 —— 宁可在这里报错。
//
// 另外两个键写错的时候没有人会报错：它们会悄悄按别的意思来。负的页缓存
// 按 0 办（store 的 dsn 只在 cacheMB > 0 的时候才加那条 pragma），负的
// 保留天数按"永远留"办（pruneCheckins 见到 days <= 0 就不删）。写 -1
// 的人以为自己说了什么，其实什么也没说 —— 宁可在这里报错。
//
// 保留天数还有一条更细的：1 到 6 天不许。签到的积分七天就过期，所以
// 「待补」那一档只看最近七天的签到（usage/round.go 的 checkinWindow =
// 7 * 24 * time.Hour，它问 Store.CheckinTimes(ctx, now.Add(-checkinWindow))）。
// 而清理是按**站点的那个日子**删的（Store.PruneCheckins 删的是
// day < DayKey(cut)，day 是站点时区的那一天），站点时区跟 UTC 差着几个
// 钟头。留 6 天的话，被删掉的那一段里可能有七天窗口还要用的行 —— 那些
// 号就永远等不到「签到待补」那一读，卡上摆的数就一直旧着。留 7 天是刚
// 够的：DayKey 对时刻是单调的，一条 created_at >= now-7天 的行，它的
// 站点那天一定 >= DayKey(now-7天)，所以删 day < DayKey(now-7天) 删不到
// 它。
func (c *Config) checkSQLite() error {
	switch strings.ToLower(c.SQLite.Synchronous) {
	case "", "normal", "full":
	default:
		return fmt.Errorf("sqlite.synchronous is %q: it is \"normal\" or \"full\"",
			c.SQLite.Synchronous)
	}
	if c.SQLite.CacheMB < 0 {
		return fmt.Errorf("sqlite.cache_mb is %d: it is 0 (SQLite's own default, 2 MB) or more",
			c.SQLite.CacheMB)
	}
	if c.SQLite.CheckinRetentionDays < 0 {
		return fmt.Errorf("sqlite.checkin_retention_days is %d: it is 0 (keep every check-in) or 7 or more",
			c.SQLite.CheckinRetentionDays)
	}
	if c.SQLite.CheckinRetentionDays > 0 && c.SQLite.CheckinRetentionDays < 7 {
		return fmt.Errorf("sqlite.checkin_retention_days is %d: it is 0 (keep every check-in) or 7 or more, because the check-in window is seven days",
			c.SQLite.CheckinRetentionDays)
	}
	return nil
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
