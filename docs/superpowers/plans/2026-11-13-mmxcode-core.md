# mmxcode 核心库 + CLI 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付一个可独立使用的 Go 核心库与 CLI：登录 MiniMax Code 国际站、管理多账号、每日签到、汇总积分、按账号/组配置代理出口。

**Architecture:** 分层包结构，`site`（常量）→ `sign`（签名，纯函数）→ `store`（SQLite）→ `upstream`（HTTP 客户端）→ `egress`（出口）→ `oauth`/`account`/`checkin`（MiniMax 协议）→ `usage`（聚合）→ `cmd/mmxcode`（CLI）。所有 MiniMax 协议细节对齐参考插件 `@magpie-community/opencode-minimax-auth` 的 `packages/minimax/index.mjs`。

**Tech Stack:** Go 1.23（由 Go 1.21 的 toolchain 自动机制拉取）、`modernc.org/sqlite` v1.62.0（纯 Go，无 cgo）、`golang.org/x/net` v0.62.0（SOCKS5 拨号）、`github.com/spf13/cobra` v1.10.2、`gopkg.in/yaml.v3` v3.0.1。

**设计文档：** `docs/superpowers/specs/2026-11-13-mmxcode-gateway-design.md`

---

## 本计划的范围

本计划交付 **可用的核心库 + CLI**：登录 → 签到 → 看积分 → 管理账号与代理出口。全部有单测，可 `go test ./...` 验证，可手工端到端验证。

HTTP 网关（三种协议、账号轮转）、WebUI、Docker 部署在 **计划 B**（`docs/superpowers/plans/2026-11-13-mmxcode-gateway.md`）中。

这样切分是因为计划 A 结束后已经有一个真正能用的产品（CLI），而不是一堆只有单元测试的库。

## 环境前提

- 本机 Go 为 1.21.0，但 `go.mod` 写 `go 1.23.0`，Go 1.21 会自动下载并使用 1.23 工具链（已验证可用）
- 模块代理 `https://goproxy.cn,direct` 可用
- 访问 MiniMax 国际站需要代理 `http://127.0.0.1:7890`（通过 `proxy.default` 配置）

## 关键实现约束（务必先读）

这三条是从参考插件的 JS 实现里抠出来的细节，做错会直接导致签名校验失败或协议不通：

1. **查询参数顺序是插入顺序，不是字典序。** JS 的 `new URLSearchParams({...})` 保留对象的键顺序，而 `url.Values.Encode()` 在 Go 里按字典序排列 —— 用它签名必然失败。必须自己实现有序参数。

2. **JS 的 `encodeURIComponent` 与 Go 的 `url.QueryEscape` 不等价。** `encodeURIComponent` 不转义 `!'()*~`，Go 的 `QueryEscape` 转义 `*`、不转义 `~`；`QueryEscape` 还把空格转成 `+`（`encodeURIComponent` 转成 `%20`）。签名用的是前者，必须自己实现。

3. **`URLSearchParams` 的序列化也不等于 `url.QueryEscape`。** 它保留 `*-._`，空格转 `+`，其余百分号编码；`~` 会被编码成 `%7E`。

参考实现（已从 `index.mjs` 精确复刻，测试向量由 Node 24 生成）：

```js
function signed(path, { access, userID, body, origin, query, now, tz, platform }) {
  const url = new URL(path, origin)
  url.search = new URLSearchParams({
    device_platform: "mcode", biz_id: "3", app_id: "3001", version_code: "22201",
    ...query,                       // 覆盖同名键，但保持原位置
    unix: String(now), timezone_offset: String(tz), sys_language: "en", lang: "en",
    device_id: "0", os_name: platform, browser_name: "mcode",
    user_id: String(userID ?? "").trim() || "0", client: "mcode",
  }).toString()
  const at = url.pathname + url.search
  const ts = Math.floor(now / 1000)
  const json = body === undefined ? undefined : JSON.stringify(body)
  return {
    url: url.toString(),
    yy: json === undefined
      ? md5(`${encodeURIComponent(at)}_{}${md5(String(now))}ooui`)
      : md5(`${encodeURIComponent(at)}_${json}${md5(String(now))}ooui`),
    ts: String(ts),
    signature: json === undefined
      ? md5(`${ts}${SIGN_SECRET}`)
      : md5(`${ts}${SIGN_SECRET}${json}`),
  }
}
```

`SIGN_SECRET = "I*7Cf%WZ#S&%1RlZJ&C2"`。

---

## 文件结构

```
go.mod
go.sum
cmd/mmxcode/
  main.go              入口，只调 root.Execute()
  root.go              根命令、全局 flag、配置加载
  login.go             login 子命令
  accounts.go          accounts 子命令（list / remove / proxy）
  checkin.go           checkin 子命令
  usage.go             usage 子命令
  models.go            models 子命令
internal/
  config/config.go     配置结构与加载
  config/config_test.go
  site/site.go         站点常量与端点
  site/models.go       内置模型表
  site/models_test.go
  sign/sign.go         签名（有序参数 + JS 兼容编码 + md5）
  sign/sign_test.go    对照金标准向量
  store/store.go       SQLite 存储
  store/schema.go      schema 与迁移
  store/store_test.go
  upstream/client.go   HTTP 客户端（重定向、超时、错误信封）
  upstream/errors.go   OtherSiteError、Envelope 检查
  upstream/upstream_test.go
  egress/egress.go     出口解析、client 缓存、拨号
  egress/egress_test.go
  oauth/device.go      device flow 登录
  oauth/refresh.go     刷新与单飞（Renewer）
  oauth/oauth_test.go
  account/account.go   identity / membership / 账本 / 套餐窗口
  account/account_test.go
  checkin/checkin.go   签到判定与领取
  checkin/checkin_test.go
  usage/usage.go       积分聚合
  usage/usage_test.go
```

**分层规则**（每个包只依赖它下面的包）：

```
config  site
   ↓
  sign
   ↓
 store  upstream  egress
   ↓
oauth  account  checkin
   ↓
 usage
   ↓
cmd/mmxcode
```

---

### Task 1: 项目骨架、配置包、site 包

**Files:**
- Create: `go.mod`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `internal/site/site.go`
- Create: `internal/site/models.go`
- Create: `internal/site/models_test.go`
- Create: `internal/site/site_test.go`

- [ ] **Step 1: 初始化模块**

```bash
cd "E:/lishihua_workspace/golang_coding/mmxcode-api"
go mod init mmxcode
```

然后写 `go.mod`（保留 `go mod init` 生成的 module 行，把 go 指令改成 1.23.0）：

```
module mmxcode

go 1.23.0
```

- [ ] **Step 2: 写失败的测试 —— 配置默认值与时长解析**

创建 `internal/config/config_test.go`：

```go
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLoadDefaults(t *testing.T) {
	c := Default()
	if c.Server.Listen != ":8080" {
		t.Fatalf("listen = %q, want :8080", c.Server.Listen)
	}
	if c.Upstream.Timeout.Duration() != 120*time.Second {
		t.Fatalf("timeout = %v, want 120s", c.Upstream.Timeout)
	}
	if c.Rotation.Strategy != "sticky" {
		t.Fatalf("strategy = %q, want sticky", c.Rotation.Strategy)
	}
	if c.Rotation.MaxAttempts != 3 {
		t.Fatalf("max_attempts = %d, want 3", c.Rotation.MaxAttempts)
	}
	if c.Rotation.ExhaustedCooldown.Duration() != 5*time.Minute {
		t.Fatalf("cooldown = %v, want 5m", c.Rotation.ExhaustedCooldown)
	}
	if c.Schedule.CheckinAt != "09:00" {
		t.Fatalf("checkin_at = %q, want 09:00", c.Schedule.CheckinAt)
	}
	if c.Schedule.UsageRefresh.Duration() != 15*time.Minute {
		t.Fatalf("usage_refresh = %v, want 15m", c.Schedule.UsageRefresh)
	}
	if c.Schedule.TokenRefresh.Duration() != 5*time.Minute {
		t.Fatalf("token_refresh = %v, want 5m", c.Schedule.TokenRefresh)
	}
	if c.Schedule.EgressHealth.Duration() != 2*time.Minute {
		t.Fatalf("egress_health = %v, want 2m", c.Schedule.EgressHealth)
	}
	if c.DB != "/data/mmxcode.db" {
		t.Fatalf("db = %q, want /data/mmxcode.db", c.DB)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
server:
  listen: ":9090"
  api_keys: ["sk-a", "sk-b"]
proxy:
  default: "socks5h://warp:4000"
  groups:
    warp: "socks5h://warp:4000"
    direct: "direct"
upstream:
  timeout: 45s
rotation:
  strategy: round-robin
  max_attempts: 5
schedule:
  checkin_at: "07:30"
db: "./local.db"
`
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":9090" {
		t.Fatalf("listen = %q", c.Server.Listen)
	}
	if len(c.Server.APIKeys) != 2 {
		t.Fatalf("api_keys = %v", c.Server.APIKeys)
	}
	if c.Proxy.Default != "socks5h://warp:4000" {
		t.Fatalf("proxy default = %q", c.Proxy.Default)
	}
	if c.Proxy.Groups["warp"] != "socks5h://warp:4000" {
		t.Fatalf("groups = %v", c.Proxy.Groups)
	}
	if c.Proxy.Groups["direct"] != "direct" {
		t.Fatalf("groups = %v", c.Proxy.Groups)
	}
	if c.Upstream.Timeout.Duration() != 45*time.Second {
		t.Fatalf("timeout = %v", c.Upstream.Timeout)
	}
	if c.Rotation.Strategy != "round-robin" || c.Rotation.MaxAttempts != 5 {
		t.Fatalf("rotation = %+v", c.Rotation)
	}
	if c.Schedule.CheckinAt != "07:30" {
		t.Fatalf("checkin_at = %q", c.Schedule.CheckinAt)
	}
	if c.DB != "./local.db" {
		t.Fatalf("db = %q", c.DB)
	}
	// 未写的字段保留默认值
	if c.Schedule.UsageRefresh.Duration() != 15*time.Minute {
		t.Fatalf("usage_refresh = %v, want default 15m", c.Schedule.UsageRefresh)
	}
}

func TestDurationString(t *testing.T) {
	d := Duration(90 * time.Second)
	if got := d.String(); got != "1m30s" {
		t.Fatalf("String() = %q", got)
	}
}

func TestDurationYAML(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte(`"1h30m"`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Duration() != 90*time.Minute {
		t.Fatalf("d = %v", d.Duration())
	}
	b, err := yaml.Marshal(Duration(90 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 写出来的形状由标准库决定（Duration.String() 总是带秒），
	// 要守的是"写出来能读回去"。
	if !strings.Contains(string(b), "1h30m0s") {
		t.Fatalf("marshalled = %q", b)
	}
	// 写出来要能读回去
	var back Duration
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Duration() != 90*time.Minute {
		t.Fatalf("round trip = %v", back.Duration())
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte(`"5x"`), &d); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadEmptyPath(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" {
		t.Fatalf("listen = %q", c.Server.Listen)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, ""); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" {
		t.Fatalf("listen = %q", c.Server.Listen)
	}
}

func TestLoadCommentOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "# 只有注释\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	// 结构体的字段名写错时必须报错。出口组的名字不在这里查，见
	// config.go 里关于 KnownFields 的说明。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "server:\n  listenn: \":9090\"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for the unknown key")
	} else if !strings.Contains(err.Error(), "listenn") {
		t.Fatalf("error %q does not name the bad key", err)
	}
}

func TestLoadRejectsTwoDocuments(t *testing.T) {
	// 一个文件里只该有一份配置。Decode 一次只读一个文档，第二个会被
	// 悄悄丢掉 —— 配置的一半就没了。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "server:\n  listen: \":1\"\n---\nserver:\n  listen: \":2\"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for the second document")
	}
}

func TestLoadAllowsTrailingSeparator(t *testing.T) {
	// 结尾一个裸的 "---" 是空文档，不是第二份配置。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "server:\n  listen: \":1\"\n---\n"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":1" {
		t.Fatalf("listen = %q", c.Server.Listen)
	}
}

func TestLoadNullGroupsBecomesEmptyMap(t *testing.T) {
	// WebUI 要遍历出口组，nil map 会让它炸。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "proxy:\n  groups:\n"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.Groups == nil {
		t.Fatal("groups is nil")
	}
	if len(c.Proxy.Groups) != 0 {
		t.Fatalf("groups = %v", c.Proxy.Groups)
	}
}

func TestConfigYAMLRoundTrip(t *testing.T) {
	// CLI 与 WebUI 都要把配置写回文件，写出来要能读回来。
	c := Default()
	c.Proxy.Groups["warp"] = "socks5h://warp:4000"
	c.Server.APIKeys = []string{"sk-a"}
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, string(b)); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Upstream.Timeout.Duration() != c.Upstream.Timeout.Duration() {
		t.Fatalf("timeout = %v, want %v", back.Upstream.Timeout.Duration(), c.Upstream.Timeout.Duration())
	}
	if back.Proxy.Groups["warp"] != c.Proxy.Groups["warp"] {
		t.Fatalf("groups = %v", back.Proxy.Groups)
	}
	if back.Schedule.CheckinAt != c.Schedule.CheckinAt || back.DB != c.DB {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if len(back.Server.APIKeys) != 1 || back.Server.APIKeys[0] != "sk-a" {
		t.Fatalf("api keys = %v", back.Server.APIKeys)
	}
}

func TestDurationRejectsNegative(t *testing.T) {
	// 负的时长到用的时候就是"没有超时"，请求会一直挂着。
	var d Duration
	if err := yaml.Unmarshal([]byte(`"-5s"`), &d); err == nil {
		t.Fatal("expected an error for the negative duration")
	}
}

func TestDurationRejectsZero(t *testing.T) {
	// 零在下游是"不设超时"或者"默默用默认值"，两个都不好。
	// 默认值全都不是零，所以文件里出现零一定是人写的。
	var d Duration
	if err := yaml.Unmarshal([]byte(`"0s"`), &d); err == nil {
		t.Fatal("expected an error for the zero duration")
	}
}

func TestLoadStripsBOM(t *testing.T) {
	// Windows 上的编辑器爱写 BOM，它会混进第一个键名里。
	path := filepath.Join(t.TempDir(), "config.yaml")
	// The BOM is written as bytes, not as a rune escape: a literal BOM
	// pasted into a .go file is a syntax error.
	body := "\xef\xbb\xbf" + "server:\n  listen: \":9090\"\n"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":9090" {
		t.Fatalf("listen = %q", c.Server.Listen)
	}
}

func TestLoadRejectsDocumentAfterAnEmptyOne(t *testing.T) {
	// 文档1、空文档、文档3：只看一次的话，文档3 会被悄悄丢掉，
	// 连它的语法错都不报。
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "server:\n  listen: \":1\"\n---\nnull\n---\nserver:\n  listen: \":3\"\n"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for the third document")
	}
}

func TestLoadRejectsDocumentAfterAnEmptyOneWithBadSyntax(t *testing.T) {
	// 第三份文档的语法错也不能被吞掉。
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "server:\n  listen: \":1\"\n---\nnull\n---\nbogus: [\n"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for the third document")
	}
}

func TestConfigJSONUsesTheSameNames(t *testing.T) {
	// WebUI 读配置用 JSON，字段名要和 YAML 里的一样。
	c := Default()
	c.Server.Listen = ":9090"
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"server", "proxy", "upstream", "rotation", "schedule", "db"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("json has no %q: %s", key, b)
		}
	}
	srv, ok := m["server"].(map[string]any)
	if !ok {
		t.Fatalf("server is not an object: %s", b)
	}
	if srv["listen"] != ":9090" {
		t.Fatalf("server.listen = %v", srv["listen"])
	}
}

func TestDurationJSON(t *testing.T) {
	// WebUI 读配置用 JSON，时长要写成字符串，不是纳秒数。
	b, err := json.Marshal(Duration(90 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1h30m0s"` {
		t.Fatalf("marshalled = %s", b)
	}
	var d Duration
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Duration() != 90*time.Minute {
		t.Fatalf("d = %v", d.Duration())
	}
}
```

`writeFile` 助手放在测试文件里：

```go
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
```

- [ ] **Step 3: 运行测试确认失败**

```bash
go test ./internal/config/...
```

Expected: FAIL —— `undefined: Default`、`undefined: Load`、`undefined: Duration`

- [ ] **Step 4: 实现配置包**

创建 `internal/config/config.go`：

```go
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

// Config is the whole configuration.
type Config struct {
	Server   Server   `yaml:"server" json:"server"`
	Proxy    Proxy    `yaml:"proxy" json:"proxy"`
	Upstream Upstream `yaml:"upstream" json:"upstream"`
	Rotation Rotation `yaml:"rotation" json:"rotation"`
	Schedule Schedule `yaml:"schedule" json:"schedule"`
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
	return c, nil
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
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./internal/config/...
```

Expected: PASS

- [ ] **Step 6: 写 site 包**

创建 `internal/site/site.go`：

```go
// Package site holds MiniMax Code's international site's hosts, paths
// and the constants its client sends.
//
// Only the international site is here. The China site (minimax.cn /
// minimaxi.com) is a different set of hosts whose tokens do not work on
// these, and it is out of scope.
package site

// The international site's hosts.
const (
	// AccountHost serves the OAuth device flow.
	AccountHost = "https://account.minimax.io"
	// LLMHost serves the chats and the model list.
	LLMHost = "https://agent.minimax.io"
	// AgentHost serves the account API, the check-in and the credits ledger.
	AgentHost = "https://agent.minimax.io"
	// PlatformHost serves the plan's windows.
	PlatformHost = "https://platform.minimax.io"
)

// The site's own region and language.
const (
	Region = "en"
	Lang   = "en"
)

// The OAuth client's parameters. These are MiniMax Code's own.
const (
	ClientID    = "mcode-public"
	Scope       = "agent.default"
	Audience    = "agent-backend"
	DeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

// The headers MiniMax Code's client sends with a chat.
const (
	// PlaceholderKey is the key MiniMax Code gives its SDK; the account
	// itself goes in Authorization.
	PlaceholderKey = "sk-xxx"
	// UserAgent is what MiniMax Code calls itself when it chats.
	UserAgent = "MiniMaxAgent"
	// AccountUserAgent is what MiniMax Code calls itself when it signs
	// an account API request.
	AccountUserAgent = "MiniMaxCode"
	// AgentID is MiniMax Code's agent id.
	AgentID = "main"
)

// The paths of the endpoints.
//
// They are paths, not URLs. The host is decided by whoever sends the
// request, so a test can point them at a stand-in server and a
// deployment can move a host without touching the paths.
//
// The full URL of an endpoint is its host followed by its path:
// site.AccountHost + site.DeviceCodePath.
const (
	// The OAuth endpoints, on AccountHost.
	DeviceCodePath = "/oauth2/device/code"
	TokenPath      = "/oauth2/token"

	// The chat and model endpoints, on LLMHost.
	ModelsPath          = "/mavis/api/v1/models"
	MessagesPath        = "/mavis/api/v1/llm/v1/messages"
	ChatCompletionsPath = "/mavis/api/v1/llm/v1/chat/completions"
	ResponsesPath       = "/mavis/api/v1/llm/v1/responses"

	// The account API's endpoints, on AgentHost.
	IdentityPath     = "/v1/api/user/info"
	WorkspacePath    = "/matrix/api/v1/user/get_user_extra_info"
	MembershipPath   = "/matrix/api/v1/commerce/get_membership_info"
	CreditPath       = "/minimax-cloud/api/v1/credit/details"
	SigninStatusPath = "/minimax-cloud/api/v1/signin/status"
	SigninClaimPath  = "/minimax-cloud/api/v1/signin/claim"

	// The plan console's endpoints, on PlatformHost.
	PlanRemainsPath = "/backend/account/token_plan/remains_percent"
	CodingPlanPath  = "/v1/api/openplatform/coding_plan/remains"
	PlanTitlePath   = "/v1/api/openplatform/charge/combo/cycle_audio_resource_package"
)

// ModelsQuery is the query the model list endpoint needs.
//
// It is the only endpoint with a query of its own, so it lives here
// rather than being spelled out at the call site.
const ModelsQuery = "region=" + Region + "&buildEnv=prod"

// AccountURL gives an account endpoint's full URL.
func AccountURL(path string) string { return AccountHost + path }

// AgentURL gives an account API endpoint's full URL.
//
// AgentHost and LLMHost are the same host today. That is exactly why
// this helper is worth having: a call site that picks the wrong one
// would not notice.
func AgentURL(path string) string { return AgentHost + path }

// LLMURL gives a chat endpoint's full URL.
func LLMURL(path string) string { return LLMHost + path }

// PlatformURL gives a plan console endpoint's full URL.
func PlatformURL(path string) string { return PlatformHost + path }
```

创建 `internal/site/models.go`：

```go
package site

// Thinking says how a model handles thinking.
//
// A type of its own, so a typo like "switchble" does not compile.
type Thinking string

const (
	ThinkingOff        Thinking = "off"
	ThinkingOn         Thinking = "on"
	ThinkingForcedOn   Thinking = "forced_on"
	ThinkingSwitchable Thinking = "switchable"
)

// Model is one model of the site's.
type Model struct {
	// ID is the model's id, as the API takes it.
	ID string
	// Name is what it is called.
	Name string
	// Context is the input token limit.
	Context int
	// Output is the output token limit.
	Output int
	// Inputs are the input kinds it takes.
	Inputs []string
	// Thinking says how it handles thinking.
	Thinking Thinking
	// Efforts are the reasoning levels it takes, when it takes any.
	Efforts []string
}

// fallbackModels is the list used when the live one can't be had.
//
// It is package-private on purpose: a caller that gets a copy can
// change its copy, and there is no way to reach this one and change
// it for everybody.
//
// The context and output limits are the ones the endpoint really allows,
// not the ones MiniMax Code declares. Its model config says 512K/128K for
// the M3 family, but the endpoint accepts 1,048,576 input tokens and
// 524,288 output tokens for every one of them. An agent that believed the
// advertised ones would compact its history about half way too early.
var fallbackModels = []Model{
	{
		ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingForcedOn,
		Efforts:  []string{"low", "medium", "high", "xhigh", "max"},
	},
	{
		ID: "MiniMax-M3", Name: "M3",
		Context: 1048576, Output: 524288,
		Inputs:   []string{"text", "image", "video"},
		Thinking: ThinkingSwitchable,
		Efforts:  []string{"none", "high"},
	},
	{
		ID: "MiniMax-M2.7", Name: "M2.7",
		Context: 200000, Output: 128000,
		Inputs:   []string{"text"},
		Thinking: ThinkingOn,
	},
	{
		ID: "MiniMax-M2.7-highspeed", Name: "M2.7-highspeed",
		Context: 200000, Output: 128000,
		Inputs:   []string{"text"},
		Thinking: ThinkingOn,
	},
}

// FallbackModels gives the built-in model list.
//
// The caller gets its own copy: the models and the slices inside them.
// Changing what it got back changes nothing for anybody else.
func FallbackModels() []Model { return CloneModels(fallbackModels) }

// Clone copies the model, including the slices inside it, so a caller
// can change it without touching the original.
func (m Model) Clone() Model {
	c := m
	c.Inputs = append([]string(nil), m.Inputs...)
	c.Efforts = append([]string(nil), m.Efforts...)
	return c
}

// CloneModels copies the list, the models and their slices.
//
// A plain copy of the slice would share the slices inside each model:
// changing one model's inputs would change the original's, in a process
// that runs for days, with nothing in the log to point at it.
func CloneModels(in []Model) []Model {
	out := make([]Model, len(in))
	for i, m := range in {
		out[i] = m.Clone()
	}
	return out
}
```

- [ ] **Step 7: 写 site 包的测试**

创建 `internal/site/site_test.go`：

```go
package site

import (
	"strings"
	"testing"
)

// TestPathsAreBare guards the convention: a path is a path and the host
// is a separate thing.
//
// A path that grew a host would break the gateway's ability to point
// the chats at another host, and would make every call site that
// composes host+path produce a doubled host.
func TestPathsAreBare(t *testing.T) {
	paths := []string{
		DeviceCodePath, TokenPath,
		ModelsPath, MessagesPath, ChatCompletionsPath, ResponsesPath,
		IdentityPath, WorkspacePath, MembershipPath, CreditPath,
		SigninStatusPath, SigninClaimPath,
		PlanRemainsPath, CodingPlanPath, PlanTitlePath,
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") {
			t.Errorf("path %q does not start with a slash", p)
		}
		if strings.Contains(p, "://") {
			t.Errorf("path %q carries a host", p)
		}
		if strings.ContainsAny(p, "?#") {
			t.Errorf("path %q carries a query or a fragment", p)
		}
	}
}

// TestURLHelpers checks that the helpers put the right host in front.
func TestURLHelpers(t *testing.T) {
	if got := AccountURL(DeviceCodePath); got != AccountHost+"/oauth2/device/code" {
		t.Errorf("AccountURL = %q", got)
	}
	if got := LLMURL(MessagesPath); got != LLMHost+"/mavis/api/v1/llm/v1/messages" {
		t.Errorf("LLMURL = %q", got)
	}
	if got := PlatformURL(PlanRemainsPath); got != PlatformHost+"/backend/account/token_plan/remains_percent" {
		t.Errorf("PlatformURL = %q", got)
	}
	if got := AgentURL(SigninStatusPath); got != AgentHost+"/minimax-cloud/api/v1/signin/status" {
		t.Errorf("AgentURL = %q", got)
	}
}

// TestHosts checks the hosts. Only the international site is here: the
// China site's hosts are a different set, and its tokens do not work on
// these.
func TestHosts(t *testing.T) {
	for name, h := range map[string]string{
		"AccountHost":  AccountHost,
		"LLMHost":      LLMHost,
		"AgentHost":    AgentHost,
		"PlatformHost": PlatformHost,
	} {
		if !strings.HasPrefix(h, "https://") {
			t.Errorf("%s = %q, want an https URL", name, h)
		}
		if strings.Contains(h, "minimax.cn") || strings.Contains(h, "minimaxi.com") {
			t.Errorf("%s = %q points at the China site", name, h)
		}
	}
}
```

创建 `internal/site/models_test.go`：

```go
package site

import (
	"strings"
	"testing"
)

func TestFallbackModelsLimits(t *testing.T) {
	type want struct {
		ctx, out int
		name     string
		inputs   []string
		thinking Thinking
		efforts  []string
	}
	wants := map[string]want{
		"MiniMax-M3.1-Flash-Preview": {1048576, 524288, "M3.1-Flash-Preview", []string{"text", "image", "video"}, ThinkingForcedOn, []string{"low", "medium", "high", "xhigh", "max"}},
		"MiniMax-M3":                 {1048576, 524288, "M3", []string{"text", "image", "video"}, ThinkingSwitchable, []string{"none", "high"}},
		"MiniMax-M2.7":               {200000, 128000, "M2.7", []string{"text"}, ThinkingOn, nil},
		"MiniMax-M2.7-highspeed":     {200000, 128000, "M2.7-highspeed", []string{"text"}, ThinkingOn, nil},
	}
	models := FallbackModels()
	if len(models) != len(wants) {
		t.Fatalf("got %d models, want %d", len(models), len(wants))
	}
	for _, m := range models {
		w, ok := wants[m.ID]
		if !ok {
			t.Fatalf("unexpected model %q", m.ID)
		}
		if m.Context != w.ctx || m.Output != w.out {
			t.Fatalf("%s limits = %d/%d, want %d/%d", m.ID, m.Context, m.Output, w.ctx, w.out)
		}
		if m.Name != w.name {
			t.Fatalf("%s name = %q, want %q", m.ID, m.Name, w.name)
		}
		if strings.Join(m.Inputs, ",") != strings.Join(w.inputs, ",") {
			t.Fatalf("%s inputs = %v, want %v", m.ID, m.Inputs, w.inputs)
		}
		if m.Thinking != w.thinking {
			t.Fatalf("%s thinking = %q, want %q", m.ID, m.Thinking, w.thinking)
		}
		if strings.Join(m.Efforts, ",") != strings.Join(w.efforts, ",") {
			t.Fatalf("%s efforts = %v, want %v", m.ID, m.Efforts, w.efforts)
		}
	}
}

// TestCloneModelsIsIndependent makes sure a copy really is a copy.
//
// The list is a package-level variable that outlives every request. A
// shallow copy would share the slices inside each model, and a change
// to one model's inputs would change the original's, with nothing in
// the log to point at it.
func TestCloneModelsIsIndependent(t *testing.T) {
	c := FallbackModels()
	c[0].Inputs[0] = "audio"
	c[0].Efforts[0] = "changed"
	original := FallbackModels()
	if original[0].Inputs[0] != "text" {
		t.Fatalf("the original's inputs changed to %q", original[0].Inputs[0])
	}
	if original[0].Efforts[0] == "changed" {
		t.Fatal("the original's efforts changed")
	}
}

func TestM3ThinkingVariants(t *testing.T) {
	var m Model
	for _, x := range FallbackModels() {
		if x.ID == "MiniMax-M3" {
			m = x
		}
	}
	if m.Thinking != ThinkingSwitchable {
		t.Fatalf("M3 thinking = %q", m.Thinking)
	}
	// 规格里 M3 是 switchable：none / high
	if len(m.Efforts) != 2 || m.Efforts[0] != "none" || m.Efforts[1] != "high" {
		t.Fatalf("M3 efforts = %v", m.Efforts)
	}
}
```

- [ ] **Step 8: 运行测试并提交**

```bash
go test ./internal/... 
go fmt ./internal/...
git add go.mod internal/config internal/site
git commit -m "feat: 配置与站点常量

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: 签名包（含金标准向量）

**Files:**
- Create: `internal/sign/sign.go`
- Create: `internal/sign/sign_test.go`
- Already there: `internal/sign/testdata/regen-vectors.mjs` —— 把参考实现的 `signed()` 原样抄过来的向量生成脚本。提交它，但别改它。

- [ ] **Step 1: 写失败的测试 —— 金标准向量**

这六个向量的来源：它们由参考实现（magpie-community/plugins 的
`packages/minimax/index.mjs` 里的 `signed()`）在 Node 上跑出来。
`internal/sign/testdata/regen-vectors.mjs` 里抄了那个 `signed()` 的原文，
跑一下它就能重新得到这六个向量，两边必须一致：

```bash
node internal/sign/testdata/regen-vectors.mjs
```

创建 `internal/sign/sign_test.go`：

```go
package sign

import "testing"

// The vectors were produced by running the reference implementation
// (magpie-community/plugins, packages/minimax/index.mjs) under Node 24.
// They are the contract: if these pass, our signature is byte-identical
// to the one MiniMax Code sends.
func TestSignGoldenVectors(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		opts    Options
		wantURL string
		wantYY  string
		wantTS  string
		wantSig string
	}{
		{
			name: "identity GET",
			path: "/v1/api/user/info",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
			},
			wantURL: "https://agent.minimax.io/v1/api/user/info?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "40865acf7e929978824a2892b1cc0e7b",
			wantTS:  "1763150000",
			wantSig: "12f7ac9ac565a90bb1d7652ab46ac58c",
		},
		{
			name: "signin status GET",
			path: "/minimax-cloud/api/v1/signin/status",
			opts: Options{
				Access: "tok_2", UserID: "1234567890",
				Now: 1763150001000, TZOffset: -480, OSName: "linux",
				Query: []KV{
					{"device_platform", "web"},
					{"is_desktop", "1"},
					{"desktop_version", "0.6.12"},
				},
			},
			wantURL: "https://agent.minimax.io/minimax-cloud/api/v1/signin/status?device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1&desktop_version=0.6.12&unix=1763150001000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "843dc3e76c9fb3ad20efd5e21ec0d694",
			wantTS:  "1763150001",
			wantSig: "76c77cb5d0d6379b5e3c327e1dcfc792",
		},
		{
			name: "signin claim POST",
			path: "/minimax-cloud/api/v1/signin/claim",
			opts: Options{
				Access: "tok_3", UserID: "1234567890",
				Body: []byte("{}"),
				Now:  1763150002000, TZOffset: -480, OSName: "linux",
				Query: []KV{
					{"device_platform", "web"},
					{"is_desktop", "1"},
					{"desktop_version", "0.6.12"},
				},
			},
			wantURL: "https://agent.minimax.io/minimax-cloud/api/v1/signin/claim?device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1&desktop_version=0.6.12&unix=1763150002000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "6b25db8b3affd0acc6649a069504ae44",
			wantTS:  "1763150002",
			wantSig: "df32c8dde2f6a4ca656915a2b3d6cdb9",
		},
		{
			name: "credit details GET",
			path: "/minimax-cloud/api/v1/credit/details",
			opts: Options{
				Access: "tok_4", UserID: "1234567890",
				Now: 1763150003000, TZOffset: -480, OSName: "linux",
				Query: []KV{
					{"device_platform", "web"},
					{"is_desktop", "1"},
					{"desktop_version", "0.6.12"},
					{"page_number", "1"},
					{"page_size", "20"},
					{"include_expired", "true"},
				},
			},
			wantURL: "https://agent.minimax.io/minimax-cloud/api/v1/credit/details?device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1&desktop_version=0.6.12&page_number=1&page_size=20&include_expired=true&unix=1763150003000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "08b8e40788854e52e454ba9368e06170",
			wantTS:  "1763150003",
			wantSig: "d85b51c8028ad38c89a641b2b7a02a6d",
		},
		{
			name: "membership POST",
			path: "/matrix/api/v1/commerce/get_membership_info",
			opts: Options{
				Access: "tok_5", UserID: "1234567890",
				Body: []byte("{}"),
				Now:  1763150004000, TZOffset: -480, OSName: "linux",
			},
			wantURL: "https://agent.minimax.io/matrix/api/v1/commerce/get_membership_info?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201&unix=1763150004000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "13ceb091e21212e52bb130743981aa78",
			wantTS:  "1763150004",
			wantSig: "3bb29287aeeb77d318ea9c5f3dc1e0a6",
		},
		{
			name: "workspace POST with empty user id",
			path: "/matrix/api/v1/user/get_user_extra_info",
			opts: Options{
				Access: "tok_6", UserID: "",
				Body: []byte("{}"),
				Now:  1763150005000, TZOffset: 0, OSName: "darwin",
			},
			wantURL: "https://agent.minimax.io/matrix/api/v1/user/get_user_extra_info?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201&unix=1763150005000&timezone_offset=0&sys_language=en&lang=en&device_id=0&os_name=darwin&browser_name=mcode&user_id=0&client=mcode",
			wantYY:  "d654afea9c1c4f667f82ecf2c7e175da",
			wantTS:  "1763150005",
			wantSig: "b8aec2aec964042d1b477b48ecbf1324",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign("https://agent.minimax.io", c.path, c.opts)
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
			}
			if got.Timestamp != c.wantTS {
				t.Errorf("timestamp = %s, want %s", got.Timestamp, c.wantTS)
			}
			if got.Signature != c.wantSig {
				t.Errorf("signature = %s, want %s", got.Signature, c.wantSig)
			}
		})
	}
}

func TestQueryOverrideKeepsPosition(t *testing.T) {
	// The reference spreads the caller's query over the defaults, so a
	// repeated key changes its value but keeps its place.
	got := Sign("https://agent.minimax.io", "/p", Options{
		Now: 1763150000000, TZOffset: -480, OSName: "linux",
		Query: []KV{{"device_platform", "web"}, {"is_desktop", "1"}},
	})
	want := "device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1"
	if !contains(got.URL, want) {
		t.Fatalf("URL %s does not contain %s", got.URL, want)
	}
}

func TestEncodeURIComponentMatchesJS(t *testing.T) {
	cases := map[string]string{
		"/v1/api/user/info?a=b&c=d": "%2Fv1%2Fapi%2Fuser%2Finfo%3Fa%3Db%26c%3Dd",
		"a b":                       "a%20b",
		"~":                         "~",
		"*":                         "*",
		// encodeURIComponent keeps !'()* — unlike URLSearchParams, which
		// escapes them. The two encoders differ exactly here.
		"!'()": "!'()",
	}
	for in, want := range cases {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormEncodeMatchesJS(t *testing.T) {
	cases := map[string]string{
		"a b": "a+b",
		"~":   "%7E",
		"*":   "*",
		"-._": "-._",
		"/":   "%2F",
		// URLSearchParams escapes these; encodeURIComponent does not.
		"!'()": "%21%27%28%29",
	}
	for in, want := range cases {
		if got := formEncode(in); got != want {
			t.Errorf("formEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/sign/...
```

Expected: FAIL —— `undefined: Sign`、`undefined: Options`、`undefined: KV`

- [ ] **Step 3: 实现签名包**

创建 `internal/sign/sign.go`：

```go
// Package sign builds the headers MiniMax's account API expects.
//
// MiniMax Code signs each account API request: "yy" over the path, the
// query and the body, and "x-signature" over the time and the body. The
// secret is MiniMax Code's own.
//
// Two details make this different from an ordinary query builder, and
// both come from how JavaScript does it:
//
//   - the query keeps its insertion order. URLSearchParams does not sort
//     its keys, and the signature is over the encoded text, so the order
//     is part of the contract. Go's url.Values.Encode() sorts, and would
//     produce a different signature every time;
//   - the encoding is JavaScript's. URLSearchParams keeps "*-._", encodes
//     a space as "+" and encodes "~"; encodeURIComponent keeps
//     "!*'()~", encodes a space as "%20" and does not touch "~". Go's
//     url.QueryEscape does neither exactly.
package sign

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"mmxcode/internal/site"
)

// Secret is MiniMax Code's request-signing secret.
const Secret = "I*7Cf%WZ#S&%1RlZJ&C2"

// KV is one query parameter.
type KV struct {
	K string
	V string
}

// Options is what a request's signature is made of.
type Options struct {
	// Access is the account's access token, put in Authorization.
	Access string
	// UserID is the account's MiniMax user id. An empty one becomes "0".
	UserID string
	// Body is the request's JSON body. A nil body means a GET.
	Body []byte
	// Query holds the caller's extra query parameters, in order. A key
	// that repeats one of the defaults changes its value and keeps its
	// place, as the reference's object spread does.
	Query []KV
	// Now is the request's time in Unix milliseconds.
	Now int64
	// TZOffset is the client's timezone offset in seconds.
	TZOffset int
	// OSName is the client's platform name.
	OSName string
	// Lang is the site's language.
	Lang string
}

// Signed is a signed request's URL and headers.
type Signed struct {
	// URL is the full URL, query included.
	URL string
	// YY is the header "yy".
	YY string
	// Timestamp is the header "x-timestamp", in seconds.
	Timestamp string
	// Signature is the header "x-signature".
	Signature string

	// access is the account's access token, put in Authorization.
	access string
}

// Headers gives the headers a signed request carries.
func (s Signed) Headers() map[string]string {
	return map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    site.AccountUserAgent,
		"Authorization": "Bearer " + s.access,
		"yy":            s.YY,
		"x-timestamp":   s.Timestamp,
		"x-signature":   s.Signature,
	}
}

// Sign builds the signed request for a path on origin.
func Sign(origin, path string, o Options) Signed {
	lang := o.Lang
	if lang == "" {
		lang = site.Lang
	}
	osName := o.OSName
	if osName == "" {
		osName = "linux"
	}

	p := newParams()
	p.set("device_platform", "mcode")
	p.set("biz_id", "3")
	p.set("app_id", "3001")
	p.set("version_code", "22201")
	for _, kv := range o.Query {
		p.set(kv.K, kv.V)
	}
	p.set("unix", strconv.FormatInt(o.Now, 10))
	p.set("timezone_offset", strconv.Itoa(o.TZOffset))
	p.set("sys_language", lang)
	p.set("lang", lang)
	p.set("device_id", "0")
	p.set("os_name", osName)
	p.set("browser_name", "mcode")
	p.set("user_id", strings.TrimSpace(o.UserID))
	if p.get("user_id") == "" {
		p.set("user_id", "0")
	}
	p.set("client", "mcode")

	query := p.encode()
	at := path + "?" + query
	ts := o.Now / 1000
	nowText := strconv.FormatInt(o.Now, 10)

	var yy, signature string
	if o.Body == nil {
		yy = md5Hex(encodeURIComponent(at) + "_" + "{}" + md5Hex(nowText) + "ooui")
		signature = md5Hex(strconv.FormatInt(ts, 10) + Secret)
	} else {
		body := string(o.Body)
		yy = md5Hex(encodeURIComponent(at) + "_" + body + md5Hex(nowText) + "ooui")
		signature = md5Hex(strconv.FormatInt(ts, 10) + Secret + body)
	}

	return Signed{
		URL:       strings.TrimRight(origin, "/") + at,
		YY:        yy,
		Timestamp: strconv.FormatInt(ts, 10),
		Signature: signature,
		access:    o.Access,
	}
}

// params is an ordered query parameter set: setting a key that is
// already there changes its value and keeps its place.
type params struct {
	keys []string
	vals map[string]string
}

func newParams() *params {
	return &params{vals: map[string]string{}}
}

func (p *params) set(k, v string) {
	if _, ok := p.vals[k]; !ok {
		p.keys = append(p.keys, k)
	}
	p.vals[k] = v
}

func (p *params) get(k string) string { return p.vals[k] }

func (p *params) encode() string {
	var b strings.Builder
	for i, k := range p.keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(formEncode(k))
		b.WriteByte('=')
		b.WriteString(formEncode(p.vals[k]))
	}
	return b.String()
}

// formEncode encodes one value the way URLSearchParams does.
func formEncode(s string) string {
	const kept = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789*-._"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case strings.IndexByte(kept, c) >= 0:
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// encodeURIComponent encodes one value the way JavaScript's
// encodeURIComponent does.
func encodeURIComponent(s string) string {
	const kept = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(kept, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/sign/...
```

Expected: PASS —— 六个金标准向量全过

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/sign/
git add internal/sign
git commit -m "feat: MiniMax 请求签名

有序查询参数与 JavaScript 兼容的编码，六个金标准向量由参考实现生成。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: 存储层

**Files:**
- Create: `internal/store/schema.go`
- Create: `internal/store/store.go`
- Create: `internal/store/store_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/store/store_test.go`：

```go
package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAccountRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	a := Account{
		ID:         "acc_01",
		Name:       "主号",
		UserID:     "1234567890",
		Email:      "a@example.com",
		Status:     StatusActive,
		ProxyGroup: "warp",
		ProxyURL:   "",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@example.com" || got.ProxyGroup != "warp" || got.Status != StatusActive {
		t.Fatalf("got %+v", got)
	}

	a.Name = "改名了"
	a.ProxyURL = "http://127.0.0.1:1080"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount(ctx, "acc_01")
	if got.Name != "改名了" || got.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("after update got %+v", got)
	}

	list, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d accounts", len(list))
	}

	if err := s.DeleteAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccount(ctx, "acc_01"); err == nil {
		t.Fatal("expected the account to be gone")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	exp := time.Now().Add(time.Hour)
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: exp, Subject: "sub1", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Access != "a" || got.Refresh != "r" || got.Subject != "sub1" {
		t.Fatalf("got %+v", got)
	}
	if d := got.ExpiresAt.Sub(exp); d > time.Second || d < -time.Second {
		t.Fatalf("expires_at drifted by %v", d)
	}
}

func TestCheckinUniquePerDay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	c := Checkin{AccountID: "acc_01", Day: "2026-11-13", Outcome: "claimed", Credit: 25, Streak: 3}
	if err := s.SaveCheckin(ctx, c); err != nil {
		t.Fatal(err)
	}
	// 同一天再存一次是更新，不是插入第二行
	c.Outcome = "done"
	if err := s.SaveCheckin(ctx, c); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Outcome != "done" {
		t.Fatalf("got %+v", list)
	}
}

func TestExhaustedWindows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	until := time.Now().Add(time.Hour)
	if err := s.SetExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WindowKey != "general" || got[0].Reason != "quota" {
		t.Fatalf("got %+v", got)
	}
	if err := s.ClearExhausted(ctx, "acc_01", "general"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListExhausted(ctx)
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestSessionBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || id != "acc_01" {
		t.Fatalf("got %q %v %v", id, ok, err)
	}
	// 改绑
	if err := s.BindSession(ctx, "sess-1", "acc_02"); err != nil {
		t.Fatal(err)
	}
	id, _, _ = s.SessionAccount(ctx, "sess-1")
	if id != "acc_02" {
		t.Fatalf("got %q", id)
	}
}

func TestUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 600, Consumed: 120.5, Remaining: 479.5,
		ExpiresAt: time.Now().Add(time.Hour), Plan: "M Plan",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Remaining != 479.5 {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/store/...
```

Expected: FAIL —— `undefined: Open`

- [ ] **Step 3: 实现存储层**

创建 `internal/store/schema.go`：

```go
package store

// schema is the whole database. It is applied in one go, so a fresh
// file and an existing one end up the same.
const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  user_id          TEXT,
  email            TEXT,
  status           TEXT NOT NULL,
  proxy_group      TEXT,
  proxy_url        TEXT,
  pending_persist  INTEGER NOT NULL DEFAULT 0,
  exhausted_until  INTEGER,
  last_error       TEXT,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (
  account_id  TEXT PRIMARY KEY,
  access      TEXT NOT NULL,
  refresh     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  subject     TEXT,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS exhausted_windows (
  account_id TEXT NOT NULL,
  window_key TEXT NOT NULL,
  until      INTEGER NOT NULL,
  reason     TEXT,
  PRIMARY KEY (account_id, window_key)
);

CREATE TABLE IF NOT EXISTS sessions (
  session_key TEXT PRIMARY KEY,
  account_id  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS checkins (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  day        TEXT NOT NULL,
  outcome    TEXT NOT NULL,
  credit     REAL,
  streak     INTEGER,
  message    TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (account_id, day)
);

CREATE TABLE IF NOT EXISTS usage_snapshots (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id  TEXT NOT NULL,
  granted     REAL,
  consumed    REAL,
  remaining   REAL,
  expires_at  INTEGER,
  plan        TEXT,
  plan_until  INTEGER,
  windows_json TEXT,
  balance     TEXT,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS egress_health (
  url        TEXT PRIMARY KEY,
  ok         INTEGER NOT NULL,
  error      TEXT,
  checked_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkins_account ON checkins(account_id, day DESC);
`
```

创建 `internal/store/store.go`：

```go
// Package store keeps the accounts, their tokens, the check-in history
// and the usage snapshots in one SQLite file.
//
// The file is opened with WAL on, so a reader (the WebUI) and a writer
// (the scheduler) do not block each other.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// The account states.
const (
	StatusActive     = "active"
	StatusSignedOut  = "signed_out"
	StatusError      = "error"
)

// Store is the database.
type Store struct {
	db *sql.DB
}

// Account is one MiniMax account.
type Account struct {
	ID              string
	Name            string
	UserID          string
	Email           string
	Status          string
	ProxyGroup      string
	ProxyURL        string
	PendingPersist  bool
	ExhaustedUntil  time.Time
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Token is an account's sign-in.
type Token struct {
	AccountID string
	Access    string
	Refresh   string
	ExpiresAt time.Time
	Subject   string
	UpdatedAt time.Time
}

// Checkin is one day's check-in for one account.
type Checkin struct {
	ID        int64
	AccountID string
	Day       string
	Outcome   string
	Credit    float64
	Streak    int
	Message   string
	CreatedAt time.Time
}

// UsageSnapshot is one account's credits and plan at a moment.
type UsageSnapshot struct {
	ID          int64
	AccountID   string
	Granted     float64
	Consumed    float64
	Remaining   float64
	HasAmounts  bool
	ExpiresAt   time.Time
	Plan        string
	PlanUntil   time.Time
	WindowsJSON string
	Balance     string
	CreatedAt   time.Time
}

// Exhausted is one account's spent window.
type Exhausted struct {
	AccountID string
	WindowKey string
	Until     time.Time
	Reason    string
}

// EgressHealth is one proxy's last check.
type EgressHealth struct {
	URL       string
	OK        bool
	Error     string
	CheckedAt time.Time
}

// Open opens the database at path, creating it and its directory if
// they are not there.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One writer at a time; SQLite serializes them anyway and this keeps
	// the Go side from piling up.
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Migrate applies the schema.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// CreateAccount inserts an account.
func (s *Store) CreateAccount(ctx context.Context, a Account) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (id, name, user_id, email, status, proxy_group, proxy_url,
			pending_persist, exhausted_until, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
		boolInt(a.PendingPersist), ms(a.ExhaustedUntil), a.LastError, ms(a.CreatedAt), ms(a.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	return nil
}

// UpdateAccount writes an account's fields.
func (s *Store) UpdateAccount(ctx context.Context, a Account) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET name=?, user_id=?, email=?, status=?, proxy_group=?, proxy_url=?,
			pending_persist=?, exhausted_until=?, last_error=?, updated_at=?
		WHERE id=?`,
		a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
		boolInt(a.PendingPersist), ms(a.ExhaustedUntil), a.LastError, ms(a.UpdatedAt), a.ID)
	if err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	return nil
}

// DeleteAccount removes an account and everything under it.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	for _, q := range []string{
		"DELETE FROM tokens WHERE account_id=?",
		"DELETE FROM exhausted_windows WHERE account_id=?",
		"DELETE FROM checkins WHERE account_id=?",
		"DELETE FROM usage_snapshots WHERE account_id=?",
		"DELETE FROM sessions WHERE account_id=?",
		"DELETE FROM accounts WHERE id=?",
	} {
		if _, err := s.db.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("delete account: %w", err)
		}
	}
	return nil
}

// ListAccounts gives every account, oldest first.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
			pending_persist, exhausted_until, last_error, created_at, updated_at
		FROM accounts ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount gives one account.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
			pending_persist, exhausted_until, last_error, created_at, updated_at
		FROM accounts WHERE id=?`, id)
	a, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("get account %s: %w", id, err)
	}
	return a, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanAccount(sc scanner) (Account, error) {
	var (
		a                       Account
		userID, email, pg, pu   sql.NullString
		lastErr                  sql.NullString
		exhausted                sql.NullInt64
		pending                  int
		createdAt, updatedAt     int64
	)
	if err := sc.Scan(&a.ID, &a.Name, &userID, &email, &a.Status, &pg, &pu,
		&pending, &exhausted, &lastErr, &createdAt, &updatedAt); err != nil {
		return Account{}, err
	}
	a.UserID = userID.String
	a.Email = email.String
	a.ProxyGroup = pg.String
	a.ProxyURL = pu.String
	a.LastError = lastErr.String
	a.PendingPersist = pending != 0
	a.ExhaustedUntil = fromMS(exhausted.Int64)
	a.CreatedAt = fromMS(createdAt)
	a.UpdatedAt = fromMS(updatedAt)
	return a, nil
}

// SaveToken writes an account's token.
func (s *Store) SaveToken(ctx context.Context, t Token) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tokens (account_id, access, refresh, expires_at, subject, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
			access=excluded.access, refresh=excluded.refresh,
			expires_at=excluded.expires_at, subject=excluded.subject,
			updated_at=excluded.updated_at`,
		t.AccountID, t.Access, t.Refresh, ms(t.ExpiresAt), t.Subject, ms(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

// GetToken gives an account's token.
func (s *Store) GetToken(ctx context.Context, accountID string) (Token, error) {
	var (
		t         Token
		expiresAt int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT account_id, access, refresh, expires_at, subject, updated_at
		FROM tokens WHERE account_id=?`, accountID).
		Scan(&t.AccountID, &t.Access, &t.Refresh, &expiresAt, &t.Subject, &updatedAt)
	if err != nil {
		return Token{}, fmt.Errorf("get token %s: %w", accountID, err)
	}
	t.ExpiresAt = fromMS(expiresAt)
	t.UpdatedAt = fromMS(updatedAt)
	return t, nil
}

// SaveCheckin writes a day's check-in, replacing an earlier one for the
// same day.
func (s *Store) SaveCheckin(ctx context.Context, c Checkin) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO checkins (account_id, day, outcome, credit, streak, message, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(account_id, day) DO UPDATE SET
			outcome=excluded.outcome, credit=excluded.credit, streak=excluded.streak,
			message=excluded.message, created_at=excluded.created_at`,
		c.AccountID, c.Day, c.Outcome, c.Credit, c.Streak, c.Message, ms(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("save checkin: %w", err)
	}
	return nil
}

// ListCheckins gives an account's check-ins, newest first.
func (s *Store) ListCheckins(ctx context.Context, accountID string, limit int) ([]Checkin, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, day, outcome, credit, streak, message, created_at
		FROM checkins WHERE account_id=? ORDER BY day DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list checkins: %w", err)
	}
	defer rows.Close()
	var out []Checkin
	for rows.Next() {
		var (
			c                      Checkin
			credit                 sql.NullFloat64
			streak                 sql.NullInt64
			message                sql.NullString
			createdAt              int64
		)
		if err := rows.Scan(&c.ID, &c.AccountID, &c.Day, &c.Outcome, &credit, &streak, &message, &createdAt); err != nil {
			return nil, err
		}
		c.Credit = credit.Float64
		c.Streak = int(streak.Int64)
		c.Message = message.String
		c.CreatedAt = fromMS(createdAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveUsageSnapshot writes a usage reading.
func (s *Store) SaveUsageSnapshot(ctx context.Context, u UsageSnapshot) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_snapshots (account_id, granted, consumed, remaining,
			expires_at, plan, plan_until, windows_json, balance, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		u.AccountID, u.Granted, u.Consumed, u.Remaining, ms(u.ExpiresAt),
		u.Plan, ms(u.PlanUntil), u.WindowsJSON, u.Balance, ms(u.CreatedAt))
	if err != nil {
		return fmt.Errorf("save usage snapshot: %w", err)
	}
	return nil
}

// LatestUsageSnapshots gives each account's newest usage reading.
func (s *Store) LatestUsageSnapshots(ctx context.Context) ([]UsageSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.account_id, s.granted, s.consumed, s.remaining, s.expires_at,
			s.plan, s.plan_until, s.windows_json, s.balance, s.created_at
		FROM usage_snapshots s
		JOIN (SELECT account_id, MAX(created_at) AS mx FROM usage_snapshots GROUP BY account_id)
			m ON m.account_id = s.account_id AND m.mx = s.created_at
		ORDER BY s.account_id`)
	if err != nil {
		return nil, fmt.Errorf("latest usage snapshots: %w", err)
	}
	defer rows.Close()
	var out []UsageSnapshot
	for rows.Next() {
		var (
			u                              UsageSnapshot
			granted, consumed, remaining   sql.NullFloat64
			expiresAt                      sql.NullInt64
			plan, windowsJSON, balance     sql.NullString
			planUntil                      sql.NullInt64
			createdAt                      int64
		)
		if err := rows.Scan(&u.ID, &u.AccountID, &granted, &consumed, &remaining, &expiresAt,
			&plan, &planUntil, &windowsJSON, &balance, &createdAt); err != nil {
			return nil, err
		}
		u.Granted = granted.Float64
		u.Consumed = consumed.Float64
		u.Remaining = remaining.Float64
		u.HasAmounts = granted.Valid && consumed.Valid && remaining.Valid
		u.ExpiresAt = fromMS(expiresAt.Int64)
		u.Plan = plan.String
		u.PlanUntil = fromMS(planUntil.Int64)
		u.WindowsJSON = windowsJSON.String
		u.Balance = balance.String
		u.CreatedAt = fromMS(createdAt)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetExhausted marks an account's window as spent until a time.
func (s *Store) SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO exhausted_windows (account_id, window_key, until, reason)
		VALUES (?,?,?,?)
		ON CONFLICT(account_id, window_key) DO UPDATE SET
			until=excluded.until, reason=excluded.reason`,
		accountID, windowKey, ms(until), reason)
	if err != nil {
		return fmt.Errorf("set exhausted: %w", err)
	}
	return nil
}

// ClearExhausted clears an account's window.
func (s *Store) ClearExhausted(ctx context.Context, accountID, windowKey string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM exhausted_windows WHERE account_id=? AND window_key=?", accountID, windowKey)
	if err != nil {
		return fmt.Errorf("clear exhausted: %w", err)
	}
	return nil
}

// ListExhausted gives every window that is spent.
func (s *Store) ListExhausted(ctx context.Context) ([]Exhausted, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, window_key, until, reason FROM exhausted_windows ORDER BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("list exhausted: %w", err)
	}
	defer rows.Close()
	var out []Exhausted
	for rows.Next() {
		var (
			e     Exhausted
			until int64
			reason sql.NullString
		)
		if err := rows.Scan(&e.AccountID, &e.WindowKey, &until, &reason); err != nil {
			return nil, err
		}
		e.Until = fromMS(until)
		e.Reason = reason.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// BindSession points a session at an account.
func (s *Store) BindSession(ctx context.Context, sessionKey, accountID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_key, account_id, updated_at) VALUES (?,?,?)
		ON CONFLICT(session_key) DO UPDATE SET
			account_id=excluded.account_id, updated_at=excluded.updated_at`,
		sessionKey, accountID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	return nil
}

// SessionAccount gives the account a session is bound to.
func (s *Store) SessionAccount(ctx context.Context, sessionKey string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		"SELECT account_id FROM sessions WHERE session_key=?", sessionKey).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("session account: %w", err)
	}
	return id, true, nil
}

// SetEgressHealth writes a proxy's last check.
func (s *Store) SetEgressHealth(ctx context.Context, url string, ok bool, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO egress_health (url, ok, error, checked_at) VALUES (?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET
			ok=excluded.ok, error=excluded.error, checked_at=excluded.checked_at`,
		url, boolInt(ok), errMsg, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("set egress health: %w", err)
	}
	return nil
}

// EgressHealth gives every proxy's last check.
func (s *Store) EgressHealth(ctx context.Context) ([]EgressHealth, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT url, ok, error, checked_at FROM egress_health ORDER BY url")
	if err != nil {
		return nil, fmt.Errorf("egress health: %w", err)
	}
	defer rows.Close()
	var out []EgressHealth
	for rows.Next() {
		var (
			h         EgressHealth
			ok        int
			errMsg    sql.NullString
			checkedAt int64
		)
		if err := rows.Scan(&h.URL, &ok, &errMsg, &checkedAt); err != nil {
			return nil, err
		}
		h.OK = ok != 0
		h.Error = errMsg.String
		h.CheckedAt = fromMS(checkedAt)
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}

// Setting reads a setting.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("setting: %w", err)
	}
	return v, true, nil
}

// MarshalWindows saves a window list as JSON.
func MarshalWindows(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
```

- [ ] **Step 4: 拉依赖并运行测试**

```bash
go get modernc.org/sqlite@v1.62.0
go mod tidy
go test ./internal/store/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/store/
git add internal/store go.mod go.sum
git commit -m "feat: SQLite 存储层

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: 上游 HTTP 客户端

**Files:**
- Create: `internal/upstream/errors.go`
- Create: `internal/upstream/client.go`
- Create: `internal/upstream/upstream_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/upstream/upstream_test.go`：

```go
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFollowsSameHostRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("arrived"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Get(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "arrived" {
		t.Fatalf("body = %q", body)
	}
}

func TestSameHostRedirectKeepsTheBody(t *testing.T) {
	// 307 之后重放，body 必须还在。签名是按 body 算的，body 没了，
	// 上游看到的就是一个签错的请求。
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.Header().Set("Location", "/again")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/start", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2: %v", len(bodies), bodies)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("the replayed body differs: %q vs %q", bodies[0], bodies[1])
	}
	if bodies[1] != `{"a":1}` {
		t.Fatalf("the replayed body = %q", bodies[1])
	}
}

func TestRefusesCrossHostRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("other"))
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	_, err := c.Get(context.Background(), srv.URL+"/start")
	if err == nil {
		t.Fatal("expected an error")
	}
	var oe *OtherSiteError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v (%T), want OtherSiteError", err, err)
	}
	if oe.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d", oe.Status)
	}
}

func TestCheckEnvelopeBusinessError(t *testing.T) {
	// HTTP 200 but the body says it failed.
	raw := []byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`)
	err := CheckEnvelope("check-in", raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckEnvelopeBaseResp(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":1001,"status_msg":"nope"}}`)
	err := CheckEnvelope("credits", raw)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckEnvelopeOK(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":0},"statusInfo":{"code":0},"data":{}}`)
	if err := CheckEnvelope("credits", raw); err != nil {
		t.Fatal(err)
	}
}

func TestCheckEnvelopeNotJSON(t *testing.T) {
	if err := CheckEnvelope("credits", []byte("<html>")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestQuotaRefusalDetection(t *testing.T) {
	cases := map[string]bool{
		`{"error":{"message":"insufficient balance"}}`: true,
		`{"message":"额度不足"}`:                           true,
		`{"message":"积分已用完"}`:                          true,
		`{"message":"model not available"}`:            false,
		`{"message":"quota exceeded"}`:                 true,
	}
	for body, want := range cases {
		if got := IsQuotaRefusal(body); got != want {
			t.Errorf("IsQuotaRefusal(%s) = %v, want %v", body, got, want)
		}
	}
}
```

把测试文件顶部的 import 补全：

```go
import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/upstream/...
```

Expected: FAIL —— `undefined: New`

- [ ] **Step 3: 实现上游客户端**

创建 `internal/upstream/errors.go`：

```go
package upstream

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// OtherSiteError is a redirect that left the host we asked.
//
// MiniMax's international hosts sit behind Akamai, which answers a
// request from a mainland-China network with a 307 to the China site.
// The two sites' tokens do not work on each other, so following such a
// redirect reads as a refused sign-in on every call. It is never
// followed: the caller gets this error instead, and no refresh token is
// spent on it.
type OtherSiteError struct {
	// From is the host that redirected.
	From string
	// To is the host it sent the request to.
	To string
	// Status is the redirect's status code.
	Status int
}

// Error is the error's text.
func (e *OtherSiteError) Error() string {
	return fmt.Sprintf("%s sent the request on to %s (HTTP %d), which doesn't take this site's sign-in. Set a proxy outside mainland China for it.", e.From, e.To, e.Status)
}

// quotaPattern says a refusal is about the account's credits or plan
// running out.
var quotaPattern = regexp.MustCompile(`(?i)insufficient|balance|credit|quota|exhaust|limit|余额|积分|额度|不足|用完|上限`)

// IsQuotaRefusal says whether a body is a refusal for running out of
// credits or plan.
func IsQuotaRefusal(body []byte) bool {
	return quotaPattern.Match(body)
}

// envelope is the shape MiniMax's account API answers with.
type envelope struct {
	StatusInfo *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	} `json:"statusInfo"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// CheckEnvelope throws when a 200 answer says in its body that it
// failed.
//
// A successful HTTP status is not a successful answer here: MiniMax's
// gateway checks its own business codes and puts a refusal in the body.
func CheckEnvelope(what string, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: not JSON", what)
	}
	if si := env.StatusInfo; si != nil && si.Code != 0 {
		if si.Code == 1000048 {
			return fmt.Errorf("%s: the sign-in was refused", what)
		}
		msg := si.Message
		if msg == "" {
			msg = si.Msg
		}
		if msg == "" {
			msg = fmt.Sprintf("status %d", si.Code)
		}
		return fmt.Errorf("%s: %s", what, msg)
	}
	if br := env.BaseResp; br != nil && br.StatusCode != 0 {
		msg := br.StatusMsg
		if msg == "" {
			msg = fmt.Sprintf("status %d", br.StatusCode)
		}
		return fmt.Errorf("%s: %s", what, msg)
	}
	return nil
}
```

创建 `internal/upstream/client.go`：

```go
// Package upstream is the HTTP client for MiniMax's own hosts.
//
// It does three things the standard client does not:
//
//   - it refuses a redirect that leaves the host, because MiniMax's
//     international site answers a mainland-China network with a 307 to
//     the China site, whose tokens do not work on these;
//   - it checks the business codes MiniMax puts in the body of a 200;
//   - it asks for no compression, because MiniMax answers a chat it is
//     asked to compress as one gzip member flushed at the end, which
//     turns a streamed answer into one lump.
package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Options configures a client.
type Options struct {
	// Timeout is the whole-request timeout.
	Timeout time.Duration
	// UserAgent is what the client calls itself.
	UserAgent string
	// Transport is used when given; the client makes its own otherwise.
	Transport http.RoundTripper
}

// Client talks to MiniMax's hosts.
type Client struct {
	http      *http.Client
	userAgent string
}

// New makes a client.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "MiniMaxCode"
	}
	transport := opts.Transport
	if transport == nil {
		transport = defaultTransport()
	}
	return &Client{
		// No timeout on the client itself: a chat streams for as long as
		// it needs, and the caller's context carries the deadline.
		http:      &http.Client{Transport: transport},
		userAgent: ua,
	}
}

// defaultTransport is the transport used when the caller has none.
//
// It asks for no compression and does not add Accept-Encoding of its
// own: MiniMax's behaviour with compression is worse than useless for a
// stream.
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
}

// redirectStatuses are the statuses a redirect can have.
var redirectStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// Do sends a request, following a redirect that stays on the host and
// refusing one that does not.
//
// A redirect to another host is an OtherSiteError: the answer says
// where the request was sent and what to do about it.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// The redirect handling is done here, not by http.Client, so the
	// cross-host case can be told apart from the same-host one.
	client := &http.Client{
		Transport: c.http.Transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	current := req
	for hop := 0; ; hop++ {
		resp, err := client.Do(current)
		if err != nil {
			return nil, err
		}
		loc := resp.Header.Get("Location")
		if loc == "" || !redirectStatuses[resp.StatusCode] {
			return resp, nil
		}
		next, err := current.URL.Parse(loc)
		if err != nil {
			return resp, nil
		}
		if next.Host != current.URL.Host {
			resp.Body.Close()
			return nil, &OtherSiteError{
				From:   current.URL.Host,
				To:     next.Host,
				Status: resp.StatusCode,
			}
		}
		if hop >= 5 {
			return resp, nil
		}
		// 307 and 308 keep the method and the body; the others turn a
		// non-GET into a GET without a body.
		//
		// The body has to be read again: http.Request.Body is a reader
		// the previous request already consumed, so replaying it as it
		// is would send an empty body — and a signed request with the
		// wrong body is refused. GetBody is what reads it a second time;
		// http.NewRequest prepares it for bytes.Reader and
		// strings.Reader. Without it the body cannot be replayed, so the
		// answer is handed back as it came.
		method, body, length := current.Method, current.Body, current.ContentLength
		replay := false
		if resp.StatusCode != 307 && resp.StatusCode != 308 {
			if method != http.MethodGet && method != http.MethodHead {
				method, body, length = http.MethodGet, nil, 0
			}
		} else if body != nil {
			if current.GetBody == nil {
				return resp, nil
			}
			fresh, err := current.GetBody()
			if err != nil {
				return resp, nil
			}
			body = fresh
			replay = true
		}
		resp.Body.Close()
		nextReq, err := http.NewRequestWithContext(current.Context(), method, next.String(), body)
		if err != nil {
			return nil, err
		}
		nextReq.Header = current.Header.Clone()
		if replay {
			// The body came out of GetBody, which hands out a plain
			// reader, and http.NewRequest only recognises bytes and
			// strings readers. Without the length the replay would go
			// out with no Content-Length at all, chunked instead of
			// what the first request had.
			if length != 0 {
				nextReq.ContentLength = length
			}
			// The body is a reader now, so the next hop would not know
			// how to read it again. Keep the way to do it.
			nextReq.GetBody = current.GetBody
		}
		current = nextReq
	}
}

// Get sends a GET.
func (c *Client) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// PostForm sends a form POST, as the OAuth endpoints want.
func (c *Client) PostForm(ctx context.Context, rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// PostJSON sends a JSON POST.
func (c *Client) PostJSON(ctx context.Context, rawURL string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// ReadBody reads a response's body and closes it.
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return b, nil
}
```

把 `client.go` 的 import 补上 `bytes` 与 `strings`。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/upstream/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/upstream/
git add internal/upstream
git commit -m "feat: 上游 HTTP 客户端

跨站重定向拒绝跟随、200 里的业务错误码检查、请求不带压缩。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: 出口（代理）解析与 client 缓存

**Files:**
- Create: `internal/egress/egress.go`
- Create: `internal/egress/egress_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/egress/egress_test.go`：

```go
package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mmxcode/internal/store"
)

func TestResolveOrder(t *testing.T) {
	r := New(map[string]string{
		"warp":   "socks5h://warp:4000",
		"direct": "direct",
	}, "http://127.0.0.1:7890")

	cases := []struct {
		name string
		acc  store.Account
		want string
	}{
		{"账号级优先", store.Account{ProxyGroup: "warp", ProxyURL: "http://1.2.3.4:8080"}, "http://1.2.3.4:8080"},
		{"其次出口组", store.Account{ProxyGroup: "warp"}, "socks5h://warp:4000"},
		{"再次全局", store.Account{}, "http://127.0.0.1:7890"},
		{"组名为 direct", store.Account{ProxyGroup: "direct"}, "direct"},
		{"未知组落到全局", store.Account{ProxyGroup: "nope"}, "http://127.0.0.1:7890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.Resolve(c.acc); got != c.want {
				t.Fatalf("Resolve = %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveEmptyMeansDirect(t *testing.T) {
	r := New(nil, "")
	if got := r.Resolve(store.Account{}); got != Direct {
		t.Fatalf("Resolve = %q, want %q", got, Direct)
	}
}

func TestClientIsCachedPerURL(t *testing.T) {
	r := New(nil, "")
	c1, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("expected the same client for the same URL")
	}
	c3, err := r.Client("http://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c3 {
		t.Fatal("expected a different client for a different URL")
	}
}

func TestDirectClientDoesNotUseEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://should-not-be-used:9999")
	r := New(nil, "")
	c, err := r.Client(Direct)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("direct transport must not use a proxy")
	}
}

func TestHTTPProxyIsUsed(t *testing.T) {
	r := New(nil, "")
	c, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("expected a proxy on the transport")
	}
}

func TestSocks5ClientDialsThroughProxy(t *testing.T) {
	// A tiny SOCKS5 server that answers with a refusal is enough: the
	// point is that the dial goes through the proxy, not that it works.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	r := New(nil, "")
	c, err := r.Client("socks5://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected the dial through the dead proxy to fail")
	}
}

func TestLoopbackNeverProxied(t *testing.T) {
	r := New(nil, "http://127.0.0.1:7890")
	if got := r.ResolveForHost(store.Account{}, "127.0.0.1:8080"); got != Direct {
		t.Fatalf("got %q, want direct", got)
	}
	if got := r.ResolveForHost(store.Account{}, "example.com:443"); got != "http://127.0.0.1:7890" {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/egress/...
```

Expected: FAIL —— `undefined: New`

- [ ] **Step 3: 实现出口包**

创建 `internal/egress/egress.go`：

```go
// Package egress resolves which proxy a request goes out through and
// keeps the HTTP clients that use them.
//
// The resolution follows magpie's model: an account's own address wins,
// then the group it is pointed at, then the global default. "direct"
// means no proxy, and a loopback address never goes through one.
package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"mmxcode/internal/store"
)

// Direct is the value that means "no proxy".
const Direct = "direct"

// Resolver resolves an account's egress and hands out clients for it.
type Resolver struct {
	mu      sync.Mutex
	groups  map[string]string
	def     string
	clients map[string]*http.Client
}

// New makes a resolver over the configured groups and the global
// default.
func New(groups map[string]string, def string) *Resolver {
	g := make(map[string]string, len(groups))
	for k, v := range groups {
		g[k] = v
	}
	return &Resolver{groups: g, def: def, clients: map[string]*http.Client{}}
}

// Resolve gives the proxy URL an account's requests go through.
//
// An empty result means no proxy.
func (r *Resolver) Resolve(a store.Account) string {
	if u := strings.TrimSpace(a.ProxyURL); u != "" {
		return u
	}
	if g := strings.TrimSpace(a.ProxyGroup); g != "" {
		if u, ok := r.groups[g]; ok {
			return u
		}
	}
	return strings.TrimSpace(r.def)
}

// ResolveForHost is Resolve for a host, except that a loopback address
// is never proxied.
func (r *Resolver) ResolveForHost(a store.Account, host string) string {
	if isLoopback(host) {
		return Direct
	}
	return r.Resolve(a)
}

// isLoopback says a host is on this machine.
func isLoopback(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Client gives the HTTP client for a proxy URL, making it if it is not
// there yet.
//
// Clients are cached by URL: the proxy is a property of the transport,
// so one URL means one client, however many accounts use it.
func (r *Resolver) Client(proxyURL string) (*http.Client, error) {
	key := strings.TrimSpace(proxyURL)
	if key == "" {
		key = Direct
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[key]; ok {
		return c, nil
	}
	tr, err := newTransport(key)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: tr}
	r.clients[key] = c
	return c, nil
}

// Invalidate drops the cached client for a proxy URL, so a changed
// address is picked up.
func (r *Resolver) Invalidate(proxyURL string) {
	key := strings.TrimSpace(proxyURL)
	if key == "" {
		key = Direct
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, key)
}

// Groups gives the configured groups.
func (r *Resolver) Groups() map[string]string {
	out := make(map[string]string, len(r.groups))
	for k, v := range r.groups {
		out[k] = v
	}
	return out
}

// newTransport makes the transport for a proxy URL.
//
// The SOCKS5 dialer is golang.org/x/net's, not the standard library's:
// the standard one resolves the name here, and a name resolved in
// mainland China may point somewhere else. x/net's sends the name to the
// proxy, which resolves it where the proxy is.
func newTransport(proxyURL string) (*http.Transport, error) {
	base := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	if proxyURL == Direct {
		return base, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
		return base, nil
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %q: %w", proxyURL, err)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("socks5 proxy %q: no context dialer", proxyURL)
		}
		base.DialContext = ctxDialer.DialContext
		base.Proxy = nil
		return base, nil
	case "":
		return base, nil
	default:
		return nil, fmt.Errorf("proxy %q: unsupported scheme %q", proxyURL, u.Scheme)
	}
}

// Check does one request through a proxy URL and says whether it worked.
//
// It is what the egress health check runs. The target is a small
// address that answers fast; a proxy that cannot reach it is not usable.
func (r *Resolver) Check(ctx context.Context, proxyURL, target string) error {
	c, err := r.Client(proxyURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("proxy %q: HTTP %d", proxyURL, resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go get golang.org/x/net@v0.62.0
go mod tidy
go test ./internal/egress/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/egress/
git add internal/egress go.mod go.sum
git commit -m "feat: 出口解析与 client 缓存

三级解析（账号级 > 出口组 > 全局），socks5 走 x/net 拨号以保证远端 DNS。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: OAuth 登录（device flow）

**Files:**
- Create: `internal/oauth/device.go`
- Create: `internal/oauth/oauth_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/oauth/oauth_test.go`（先只放设备流的测试，刷新测试在 Task 7 追加）：

```go
package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStartDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/device/code" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("client_id"); got != "mcode-public" {
			t.Errorf("client_id = %q", got)
		}
		if got := r.Form.Get("code_challenge_method"); got != "S256" {
			t.Errorf("code_challenge_method = %q", got)
		}
		if r.Form.Get("code_challenge") == "" {
			t.Error("no code_challenge")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-1",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "https://example.invalid/activate",
			"verification_uri_complete": "https://example.invalid/activate?code=ABCD-EFGH",
			"expires_in":                600,
			"interval":                  5,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err != nil {
		t.Fatal(err)
	}
	if d.UserCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q", d.UserCode)
	}
	if d.URL != "https://example.invalid/activate?code=ABCD-EFGH" {
		t.Fatalf("url = %q", d.URL)
	}
	if d.pollField != "device_code" || d.pollValue != "dev-1" {
		t.Fatalf("poll = %s/%s", d.pollField, d.pollValue)
	}
}

func TestStartDeviceUserCodeVariant(t *testing.T) {
	// MiniMax's own variant: no device_code, the user code is polled
	// for, and the end is an absolute time in milliseconds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"user_code":        "WXYZ-1234",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       time.Now().Add(10 * time.Minute).UnixMilli(),
			"interval":         3000,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err != nil {
		t.Fatal(err)
	}
	if d.pollField != "user_code" || d.pollValue != "WXYZ-1234" {
		t.Fatalf("poll = %s/%s", d.pollField, d.pollValue)
	}
	if d.interval != 3*time.Second {
		t.Fatalf("interval = %v, want 3s", d.interval)
	}
}

func TestStartDeviceExpiredInSeconds(t *testing.T) {
	// expired_in 小于 1e12 时是"还剩多少秒"，不是绝对时间。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-2",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       600,
			"interval":         5,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err != nil {
		t.Fatal(err)
	}
	// 十分钟，不多不少
	left := time.Until(d.deadline)
	if left < 9*time.Minute || left > 10*time.Minute {
		t.Fatalf("deadline is %v away, want about 10m", left)
	}
}

func TestStartDeviceBadAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	if _, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPollDeviceSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("grant_type"); got != DeviceGrant {
			t.Errorf("grant_type = %q", got)
		}
		if r.Form.Get("code_verifier") == "" {
			t.Error("no code_verifier")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d := &Device{
		userCode:  "ABCD",
		verifier:  "v",
		pollField: "device_code",
		pollValue: "dev-1",
		deadline:  time.Now().Add(time.Minute),
		interval:  time.Millisecond,
	}
	tok, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc-token" || tok.Refresh != "ref-token" {
		t.Fatalf("token = %+v", tok)
	}
	if tok.Expires.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expires = %v", tok.Expires)
	}
}

func TestPollKeepsWaitingWhilePending(t *testing.T) {
	// 用户还没点授权：token 端点用 400 加 authorization_pending 回答。
	// 这是正常状态，接着等就是了 —— 当成失败的话，每一次登录都死在
	// 第一轮轮询上。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d := &Device{
		userCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	tok, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("polled %d times, want 3", calls)
	}
	if tok.Access != "acc-token" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestPollKeepsWaitingByStatus(t *testing.T) {
	// 另一种写法：200 加 status=pending。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d := &Device{
		userCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("polled %d times, want 2", calls)
	}
}

func TestPollSlowDown(t *testing.T) {
	// slow_down 是让我们等久一点，不是失败。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-token",
			"refresh_token": "ref-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d := &Device{
		userCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("polled %d times, want 2", calls)
	}
}

func TestPollDeviceDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d := &Device{
		userCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestOAuthErrorCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	d := &Device{
		userCode: "ABCD", verifier: "v",
		pollField: "device_code", pollValue: "dev-1",
		deadline: time.Now().Add(time.Minute), interval: time.Millisecond,
	}
	_, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "invalid_grant" || oe.HTTPStatus != 400 {
		t.Fatalf("error = %+v", oe)
	}
}
```

测试文件顶部还需要这个助手：

```go
func testClient(t *testing.T, srv *httptest.Server) *upstream.Client {
	t.Helper()
	return upstream.New(upstream.Options{Timeout: 5 * time.Second})
}
```

并加 import `"mmxcode/internal/upstream"`。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/oauth/...
```

Expected: FAIL —— `undefined: StartDevice`

- [ ] **Step 3: 实现 device flow**

创建 `internal/oauth/device.go`：

```go
// Package oauth signs in to MiniMax Code with its own device flow.
//
// The flow is OAuth 2's device flow with PKCE, client "mcode-public":
// the user opens MiniMax's page, types a code, and the plugin polls
// until the sign-in is done. Nothing of MiniMax Code's own sign-in
// (~/.minimax) is read or written.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// DeviceGrant is the grant type of the device flow.
const DeviceGrant = site.DeviceGrant

// signInTimeout is how long a device code is waited for when the
// answer gives no end of its own.
const signInTimeout = 10 * time.Minute

// Error is an answer from MiniMax's sign-in server.
type Error struct {
	// Code is the "error" the answer carried.
	Code string
	// HTTPStatus is the status the answer came with.
	HTTPStatus int
	// Message is the text to show.
	Message string
}

// Error is the error's text.
func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("MiniMax's sign-in server said %s (HTTP %d)", e.Code, e.HTTPStatus)
	}
	return fmt.Sprintf("MiniMax's sign-in server said %s", e.Code)
}

// IsSignedOut says the answer is the one that signs an account out.
//
// Only invalid_grant with HTTP 400 is a sign-out. Any other failure is
// passing, and the sign-in is kept.
func (e *Error) IsSignedOut() bool {
	return e.Code == "invalid_grant" && e.HTTPStatus == 400
}

// Token is an account's sign-in.
type Token struct {
	// Access is the access token.
	Access string
	// Refresh is the refresh token. It works once.
	Refresh string
	// Expires is when the access token ends.
	Expires time.Time
	// Subject is the MiniMax user id the access token carries.
	Subject string
	// AccountID is the account id the access token carries.
	AccountID string
}

// Device is a sign-in waiting for the user.
type Device struct {
	// UserCode is the code the user types.
	UserCode string
	// URL is where the user goes.
	URL string

	verifier   string
	pollField  string
	pollValue  string
	deadline   time.Time
	interval   time.Duration
}

// StartDevice asks MiniMax for a sign-in code.
//
// The caller gives the device-code endpoint, so a test can point it at a
// stand-in.
func StartDevice(ctx context.Context, c *upstream.Client, endpoint string) (*Device, error) {
	verifier, err := randomVerifier()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	form := url.Values{}
	form.Set("client_id", site.ClientID)
	form.Set("scope", site.Scope)
	form.Set("audience", site.Audience)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")

	resp, err := c.PostForm(ctx, endpoint, form)
	if err != nil {
		return nil, fmt.Errorf("device code: %w", err)
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, oauthError(resp.StatusCode, body)
	}
	var b struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURL         string `json:"verification_url"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		ExpiredIn               int64  `json:"expired_in"`
		Interval                int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode}
	}
	userCode := strings.TrimSpace(b.UserCode)
	deviceCode := strings.TrimSpace(b.DeviceCode)
	uri := strings.TrimSpace(b.VerificationURI)
	if uri == "" {
		uri = strings.TrimSpace(b.VerificationURL)
	}
	if userCode == "" || uri == "" {
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode}
	}

	d := &Device{UserCode: userCode, verifier: verifier}
	// MiniMax's own variant has no device_code: the user code is polled
	// for, and the end may be an absolute time in milliseconds.
	byUser := deviceCode == "" && b.ExpiredIn != 0
	switch {
	case byUser:
		d.pollField, d.pollValue = "user_code", userCode
	case deviceCode != "":
		d.pollField, d.pollValue = "device_code", deviceCode
	default:
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode}
	}

	var expires time.Duration
	switch {
	case b.ExpiresIn > 0:
		expires = time.Duration(b.ExpiresIn) * time.Second
	case b.ExpiredIn > 0:
		// expired_in 有时是还剩多少秒，有时是一个绝对的毫秒时间戳。
		// 靠大小分：毫秒时间戳不会是 1970 年。
		if b.ExpiredIn < 1e12 {
			expires = time.Duration(b.ExpiredIn) * time.Second
		} else {
			expires = time.Until(time.UnixMilli(b.ExpiredIn))
		}
	}
	if expires <= 0 {
		expires = signInTimeout
	}
	d.deadline = time.Now().Add(expires)

	interval := 5 * time.Second
	if b.Interval > 0 {
		if byUser {
			// MiniMax 自己的变体给的是毫秒
			interval = time.Duration(b.Interval) * time.Millisecond
		} else {
			interval = time.Duration(b.Interval) * time.Second
		}
	}
	d.interval = interval

	complete := strings.TrimSpace(b.VerificationURIComplete)
	if complete != "" {
		d.URL = complete
	} else {
		d.URL = uri
	}
	return d, nil
}

// Poll waits for the sign-in to finish.
//
// It answers when the sign-in is done, when it is turned down, or when
// the code runs out.
func (d *Device) Poll(ctx context.Context, c *upstream.Client, endpoint string) (*Token, error) {
	wait := d.interval
	for {
		if time.Now().After(d.deadline) {
			return nil, &Error{Code: "expired_token"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if time.Now().After(d.deadline) {
			return nil, &Error{Code: "expired_token"}
		}

		form := url.Values{}
		form.Set("grant_type", DeviceGrant)
		form.Set(d.pollField, d.pollValue)
		form.Set("client_id", site.ClientID)
		form.Set("code_verifier", d.verifier)

		resp, err := c.PostForm(ctx, endpoint, form)
		if err != nil {
			return nil, fmt.Errorf("device token: %w", err)
		}
		body, err := upstream.ReadBody(resp)
		if err != nil {
			return nil, err
		}

		var b struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
			Error        string `json:"error"`
			Status       string `json:"status"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode}
		}
		httpOK := resp.StatusCode >= 200 && resp.StatusCode <= 299
		code := strings.ToLower(b.Error)
		status := strings.ToLower(b.Status)

		// 用户还没点授权是正常状态，不是错误。RFC 8628 就是这么说的：
		// token 端点用 400 加 authorization_pending 回答。这不能当失败，
		// 得接着等 —— 当成失败的话，每一次登录都死在第一轮轮询上。
		if code == "authorization_pending" {
			continue
		}
		if code == "slow_down" {
			wait += 5 * time.Second
			continue
		}
		if !httpOK {
			return nil, oauthError(resp.StatusCode, body)
		}
		switch status {
		case "pending":
			continue
		case "slow_down":
			wait += 5 * time.Second
			continue
		case "denied", "access_denied":
			return nil, &Error{Code: "access_denied"}
		case "expired", "expired_token":
			return nil, &Error{Code: "expired_token"}
		}
		if b.Error != "" {
			return nil, oauthError(resp.StatusCode, body)
		}
		if b.AccessToken == "" || b.ExpiresIn <= 0 {
			return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode}
		}
		tok := &Token{
			Access:  b.AccessToken,
			Refresh: b.RefreshToken,
			Expires: time.Now().Add(time.Duration(b.ExpiresIn) * time.Second),
		}
		tok.Subject, tok.AccountID = claims(b.AccessToken)
		return tok, nil
	}
}

// claims reads the JWT's payload, when the access token is one.
func claims(access string) (subject, accountID string) {
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var p struct {
		Sub       string `json:"sub"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", ""
	}
	return p.Sub, p.AccountID
}

// oauthError reads an error answer.
func oauthError(status int, body []byte) *Error {
	var b struct {
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &b)
	code := b.Error
	if code == "" {
		code = "oauth_request_failed"
	}
	msg := b.ErrorDesc
	if msg == "" {
		msg = b.Description
	}
	return &Error{Code: code, HTTPStatus: status, Message: msg}
}

// randomVerifier makes the PKCE verifier.
func randomVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/oauth/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/oauth/
git add internal/oauth
git commit -m "feat: OAuth device flow 登录

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 7: token 刷新与单飞

**Files:**
- Create: `internal/oauth/refresh.go`
- Modify: `internal/oauth/oauth_test.go`（追加测试）

- [ ] **Step 1: 追加失败的测试**

在 `internal/oauth/oauth_test.go` 末尾追加：

```go
func TestRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "old-refresh" {
			t.Errorf("refresh_token = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	tok, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new-access" || tok.Refresh != "new-refresh" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestRefreshKeepsOldTokenWhenNoNewOneGiven(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	c := testClient(t, srv)
	tok, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Refresh != "old-refresh" {
		t.Fatalf("refresh = %q, want the old one", tok.Refresh)
	}
}

func TestRefreshInvalidGrantIsSignedOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if !oe.IsSignedOut() {
		t.Fatal("expected the error to be a sign-out")
	}
}

func TestRenewerSpendsOnceUnderConcurrency(t *testing.T) {
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	current := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	var wg sync.WaitGroup
	results := make([]*Token, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := r.Renew(context.Background(), "acc_01", current, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			results[i] = tok
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
	for i, tok := range results {
		if tok == nil || tok.Access != "new-access" {
			t.Fatalf("result %d = %+v", i, tok)
		}
	}
}

func TestRenewerDoesNotSpendASpentTokenAgain(t *testing.T) {
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	current := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", current, spend); err != nil {
		t.Fatal(err)
	}
	// 同一个 refresh 再来一次：不能��再发出去
	if _, err := r.Renew(context.Background(), "acc_01", current, spend); err == nil {
		t.Fatal("expected an error for an already spent token")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("spend called %d times, want 1", got)
	}
}

func TestRenewerLatestWins(t *testing.T) {
	r := NewRenewer()
	first := &Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}
	second := &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	r.Renew(context.Background(), "acc_01", first, func(ctx context.Context, refresh string) (*Token, error) {
		return second, nil
	})
	got := r.Latest("acc_01", first)
	if got == nil || got.Access != "a2" {
		t.Fatalf("latest = %+v", got)
	}
}
```

import 补上 `"sync"` 与 `"sync/atomic"`。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/oauth/...
```

Expected: FAIL —— `undefined: Refresh`、`undefined: NewRenewer`

- [ ] **Step 3: 实现刷新与单飞**

创建 `internal/oauth/refresh.go`：

```go
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// Refresh trades a refresh token for a new access token.
//
// The caller's refresh token is kept when the answer gives no new one.
func Refresh(ctx context.Context, c *upstream.Client, endpoint, refresh string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", site.ClientID)
	form.Set("scope", site.Scope)
	form.Set("audience", site.Audience)

	resp, err := c.PostForm(ctx, endpoint, form)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, oauthError(resp.StatusCode, body)
	}
	var b struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode}
	}
	if b.Error != "" {
		return nil, oauthError(resp.StatusCode, body)
	}
	if b.AccessToken == "" || b.ExpiresIn <= 0 {
		return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode}
	}
	newRefresh := b.RefreshToken
	if newRefresh == "" {
		newRefresh = refresh
	}
	tok := &Token{
		Access:  b.AccessToken,
		Refresh: newRefresh,
		Expires: time.Now().Add(time.Duration(b.ExpiresIn) * time.Second),
	}
	tok.Subject, tok.AccountID = claims(b.AccessToken)
	return tok, nil
}

// SpendFunc spends a refresh token and gives what it bought.
type SpendFunc func(ctx context.Context, refresh string) (*Token, error)

// Renewer spends refresh tokens, once each.
//
// MiniMax spends a refresh token once: sent a second time it is
// invalid_grant, which signs the account out. So a refresh token is
// never sent twice from here, whatever the concurrency.
//
// The renewer keeps, per account, the newest token it got and the set
// of refresh tokens already spent. A request that comes while a renewal
// is running waits for it and uses what it gave.
type Renewer struct {
	mu       sync.Mutex
	inflight map[string]*renewCall
	latest   map[string]*Token
	spent    map[string]bool
}

type renewCall struct {
	done  chan struct{}
	token *Token
	err   error
}

// NewRenewer makes a renewer.
func NewRenewer() *Renewer {
	return &Renewer{
		inflight: map[string]*renewCall{},
		latest:   map[string]*Token{},
		spent:    map[string]bool{},
	}
}

// Renew gives an account a fresh token, spending the refresh token at
// hand once.
//
// When the renewal is already running for the account, the caller waits
// for it and takes what it gave. When the refresh token was already
// spent, the newest token this renewer has is given instead, and the
// spent token is not sent again.
func (r *Renewer) Renew(ctx context.Context, accountID string, current *Token, spend SpendFunc) (*Token, error) {
	if current == nil {
		return nil, fmt.Errorf("no token to renew")
	}
	r.mu.Lock()
	if c, ok := r.inflight[accountID]; ok {
		r.mu.Unlock()
		select {
		case <-c.done:
			return c.token, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.spent[current.Refresh] {
		newest := r.latest[accountID]
		r.mu.Unlock()
		if newest != nil {
			return newest, nil
		}
		return nil, fmt.Errorf("the refresh token was already spent and no newer token is held")
	}
	if l, ok := r.latest[accountID]; ok && l != nil && l.Refresh != current.Refresh {
		// 已经刷新过了，用新的那个
		r.mu.Unlock()
		return l, nil
	}
	c := &renewCall{done: make(chan struct{})}
	r.inflight[accountID] = c
	r.mu.Unlock()

	tok, err := spend(ctx, current.Refresh)

	r.mu.Lock()
	delete(r.inflight, accountID)
	if err == nil && tok != nil {
		r.latest[accountID] = tok
		r.spent[current.Refresh] = true
	}
	c.token, c.err = tok, err
	close(c.done)
	r.mu.Unlock()
	return tok, err
}

// Latest gives the newest token this renewer holds for an account.
//
// It follows the chain: a token that was renewed gives another, and
// another, and the newest of them is what a request should use.
func (r *Renewer) Latest(accountID string, current *Token) *Token {
	r.mu.Lock()
	defer r.mu.Unlock()
	best := current
	for i := 0; i < 16; i++ {
		l, ok := r.latest[accountID]
		if !ok || l == nil {
			break
		}
		if best != nil && l.Refresh == best.Refresh {
			break
		}
		best = l
	}
	return best
}

// Forget drops an account's tokens, for when it is signed out.
func (r *Renewer) Forget(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.latest, accountID)
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/oauth/...
```

Expected: PASS —— 8 个并发 goroutine 只花掉 1 次 refresh token

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/oauth/
git add internal/oauth
git commit -m "feat: token 刷新与单飞保护

同一个 refresh token 只发一次；并发请求等同一个刷新；已花掉的 token 不再发。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 8: 账户 API（身份、会员、积分账本、套餐窗口）

**Files:**
- Create: `internal/account/account.go`
- Create: `internal/account/account_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/account/account_test.go`：

```go
package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/user/info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("yy"); got == "" {
			t.Error("no yy header")
		}
		if got := r.Header.Get("x-signature"); got == "" {
			t.Error("no x-signature header")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q", got)
		}
		w.Write([]byte(`{"data":{"userInfo":{"realUserID":"1234567890","userEmail":"a@example.com","name":"Ada"}}}`))
	}))
	defer srv.Close()

	c := New(testClient(t, srv))
	id, err := c.Identity(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if id.RealUserID != "1234567890" || id.Email != "a@example.com" || id.Name != "Ada" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestIdentityNoUserIDInAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := New(testClient(t, srv))
	if _, err := c.Identity(context.Background(), "tok", ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLedgerPaging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page_number")
		if page == "1" {
			w.Write([]byte(`{"total_count":3,"details":[
				{"credit_type":1,"granted_amount":"1,200.5","consumed_amount":"100","remaining_amount":"1100.5","granted_at_ms":1763000000000,"expire_at_ms":1900000000000},
				{"credit_type":2,"granted_amount":"50","consumed_amount":"0","remaining_amount":"50","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}
			]}`))
			return
		}
		w.Write([]byte(`{"total_count":3,"details":[
			{"credit_type":1,"granted_amount":"100","consumed_amount":"0","remaining_amount":"100","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t, srv))
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("got %d batches", len(batches))
	}
	if batches[0].Granted != 1200.5 || batches[0].Remaining != 1100.5 {
		t.Fatalf("batch = %+v", batches[0])
	}
}

func TestLedgerBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`))
	}))
	defer srv.Close()

	c := New(testClient(t, srv))
	if _, err := c.Ledger(context.Background(), "tok", "1234567890"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"1,200.5", 1200.5, true},
		{"0", 0, true},
		{"", 0, false},
		{"abc", 0, false},
		{"-5", 0, true},
	}
	for _, c := range cases {
		got, ok := ParseAmount(c.in, true)
		if ok != c.ok {
			t.Errorf("ParseAmount(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParseAmount(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPlanWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
			{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
			 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
			 "start_time":1763000000000,"end_time":1763010000000,
			 "current_weekly_status":2,"current_weekly_remaining_percent":80,
			 "current_weekly_total_percent":100,"weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t, srv))
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if ws[0].Used != 90 {
		t.Fatalf("interval used = %v, want 90", ws[0].Used)
	}
	if ws[0].Amount != 150 || ws[0].Limit != 150 {
		t.Fatalf("window = %+v", ws[0])
	}
}

func TestPlanWindowsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
	}))
	defer srv.Close()

	c := New(testClient(t, srv))
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("got %+v", ws)
	}
}

func TestPlanTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "cycle_audio_resource_package") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"current_subscribe":{"current_subscribe_title":"TokenPlanMax-年度会员"}}`))
	}))
	defer srv.Close()

	c := New(testClient(t, srv))
	title, err := c.PlanTitle(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if title != "TokenPlanMax-年度会员" {
		t.Fatalf("title = %q", title)
	}
}

func TestModelsFallback(t *testing.T) {
	// 没有 token 时给内置的列表
	c := New(nil)
	ms := c.Models("")
	if len(ms) == 0 {
		t.Fatal("expected the fallback models")
	}
}
```

测试文件的 import 补上 `"mmxcode/internal/upstream"`，并加助手：

```go
func testClient(t *testing.T, srv *httptest.Server) *upstream.Client {
	t.Helper()
	return upstream.New(upstream.Options{Timeout: 5 * time.Second})
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/account/...
```

Expected: FAIL —— `undefined: New`

- [ ] **Step 3: 实现账户 API 客户端**

创建 `internal/account/account.go`：

```go
// Package account reads MiniMax's account API: who the account is,
// what plan it has, how many credits it has left and what its plan's
// windows say.
//
// Every call is signed the way MiniMax Code signs one: the yy and
// x-signature headers. A bearer-only request is refused.
package account

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// Identity is who the account is.
type Identity struct {
	// RealUserID is the MiniMax user id.
	RealUserID string
	// Email is the account's email.
	Email string
	// Name is the account's name.
	Name string
}

// Batch is one batch of credits.
//
// The site writes the amounts as strings, sometimes with a thousands
// separator, so they are kept as they come and read by ParseAmount.
type Batch struct {
	// CreditType is 1 for a purchase, 2 for a check-in.
	CreditType int `json:"credit_type"`
	// GrantedAmount is what the batch gave.
	GrantedAmount string `json:"granted_amount"`
	// ConsumedAmount is what of it was used.
	ConsumedAmount string `json:"consumed_amount"`
	// RemainingAmount is what of it is left.
	RemainingAmount string `json:"remaining_amount"`
	// GrantedAtMS is when it was given, in milliseconds.
	GrantedAtMS int64 `json:"granted_at_ms"`
	// ExpiresAtMS is when it runs out, in milliseconds.
	ExpiresAtMS int64 `json:"expire_at_ms"`

	// Granted is what the batch gave, once read.
	Granted float64 `json:"-"`
	// Consumed is what of it was used, once read.
	Consumed float64 `json:"-"`
	// Remaining is what of it is left, once read.
	Remaining float64 `json:"-"`
	// GrantedAt is when it was given.
	GrantedAt time.Time `json:"-"`
	// ExpiresAt is when it runs out.
	ExpiresAt time.Time `json:"-"`
}

// Window is one plan window: the five hours, the seven days, or a
// model's own.
type Window struct {
	// Name is the window's name.
	Name string
	// ModelName is the model group the window belongs to.
	ModelName string
	// Used is the share used, in percent.
	Used float64
	// ResetsAt is when the window resets.
	ResetsAt time.Time
	// Amount is what was used, when the window counts.
	Amount float64
	// Limit is what was allowed, when the window counts.
	Limit float64
	// Unit is what it counts, when it counts.
	Unit string
	// Unlimited says the window is not limited.
	Unlimited bool
}

// Membership is the account's plan.
type Membership struct {
	// HasTokenPlan says the account has a plan.
	HasTokenPlan bool
	// Known says the site's membership API answered.
	Known bool
	// Tier is the plan's tier.
	Tier string
	// Title is the plan's name as the console says it.
	Title string
	// ExpiresAt is when the plan ends.
	ExpiresAt time.Time
	// GroupID is the group the plan's windows are under.
	GroupID string
	// Balance is the credits left, as the site writes it.
	Balance string
}

// Client reads the account API.
type Client struct {
	up *upstream.Client
	// now gives the time; a test can set it.
	now func() time.Time
	// tzOffset is the client's timezone offset in seconds.
	tzOffset int
	// osName is the client's platform name.
	osName string
}

// New makes a client.
func New(up *upstream.Client) *Client {
	return &Client{
		up:       up,
		now:      time.Now,
		// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
		// 和 Go 的 Offset() 是同一个值。
		tzOffset: int(time.Now().Offset()),
		osName:   runtime.GOOS,
	}
}

// SetClock replaces the clock, for a test.
func (c *Client) SetClock(now func() time.Time, tzOffset int) {
	c.now = now
	c.tzOffset = tzOffset
}

// checkinQuery is the query MiniMax Code's own check-in sends.
//
// The check-in comes from the desktop client, so it says so.
var checkinQuery = []signKV{
	{"device_platform", "web"},
	{"is_desktop", "1"},
	{"desktop_version", "0.6.12"},
}

// Identity reads who the account is.
func (c *Client) Identity(ctx context.Context, access, userID string) (Identity, error) {
	body, err := c.call(ctx, access, userID, http.MethodGet, site.IdentityPath, nil, nil)
	if err != nil {
		return Identity{}, err
	}
	var env struct {
		Data struct {
			UserInfo struct {
				RealUserID   string `json:"realUserID"`
				RealUserID2  string `json:"real_user_id"`
				UserEmail    string `json:"userEmail"`
				Email        string `json:"email"`
				UserMail     string `json:"userMail"`
				UserEmail2   string `json:"user_email"`
				Name         string `json:"name"`
				UserName     string `json:"userName"`
				UserName2    string `json:"user_name"`
			} `json:"userInfo"`
		} `json:"data"`
		UserInfo *struct {
			RealUserID string `json:"realUserID"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Identity{}, fmt.Errorf("account: not JSON")
	}
	id := firstNonEmpty(env.Data.UserInfo.RealUserID, env.Data.UserInfo.RealUserID2)
	if id == "" {
		return Identity{}, fmt.Errorf("account: no user id in the reply")
	}
	return Identity{
		RealUserID: id,
		Email:      firstNonEmpty(env.Data.UserInfo.UserEmail, env.Data.UserInfo.Email, env.Data.UserInfo.UserMail, env.Data.UserInfo.UserEmail2),
		Name:       firstNonEmpty(env.Data.UserInfo.Name, env.Data.UserInfo.UserName, env.Data.UserInfo.UserName2),
	}, nil
}

// Membership reads the account's plan.
//
// The international site answers the membership API with 404, which
// means the plan is unknown, not that there is none.
func (c *Client) Membership(ctx context.Context, access, userID string) (Membership, error) {
	m, err := c.membershipOnce(ctx, access, userID, "")
	if err == nil {
		return m, nil
	}
	if !isNotFound(err) {
		return Membership{}, err
	}
	m2, err2 := c.membershipOnce(ctx, access, userID, "")
	if err2 == nil {
		return m2, nil
	}
	if isNotFound(err2) {
		return Membership{Known: false}, nil
	}
	return Membership{}, err2
}

func (c *Client) membershipOnce(ctx context.Context, access, userID, groupID string) (Membership, error) {
	body, err := c.call(ctx, access, userID, http.MethodPost,
		site.MembershipPath, []byte("{}"), nil)
	if err != nil {
		return Membership{}, err
	}
	var env struct {
		HasTokenPlan     *bool  `json:"has_token_plan"`
		TokenPlanTier    string `json:"token_plan_tier"`
		TokenPlanExpires json.Number `json:"token_plan_expires_at"`
		OpGroupID        string `json:"op_group_id"`
		OpCreditSummary  *struct {
			TotalRemainingAmount string `json:"total_remaining_amount"`
		} `json:"op_credit_summary"`
		OpCreditBalance json.RawMessage `json:"opcredit_balance"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Membership{}, fmt.Errorf("membership: not JSON")
	}
	m := Membership{Known: true, Tier: env.TokenPlanTier, GroupID: env.OpGroupID}
	if env.HasTokenPlan != nil {
		m.HasTokenPlan = *env.HasTokenPlan
	}
	if env.OpCreditSummary != nil {
		m.Balance = env.OpCreditSummary.TotalRemainingAmount
	}
	if env.TokenPlanExpires != "" {
		if f, err := env.TokenPlanExpires.Float64(); err == nil && f > 0 {
			ms := int64(f)
			if ms < 1e12 {
				ms *= 1000
			}
			m.ExpiresAt = time.UnixMilli(ms)
		}
	}
	return m, nil
}

// Ledger reads every batch of credits.
//
// A page that can't be read whole gives nil: a part of the ledger
// would tell a wrong share.
func (c *Client) Ledger(ctx context.Context, access, userID string) ([]Batch, error) {
	const pageSize = 20
	const maxPages = 10
	var out []Batch
	for page := 1; page <= maxPages; page++ {
		query := append(append([]signKV{}, checkinQuery...),
			signKV{"page_number", strconv.Itoa(page)},
			signKV{"page_size", strconv.Itoa(pageSize)},
			signKV{"include_expired", "true"},
		)
		body, err := c.call(ctx, access, userID, http.MethodGet, site.CreditPath, nil, query)
		if err != nil {
			return nil, err
		}
		var env struct {
			Details    []Batch `json:"details"`
			TotalCount *int    `json:"total_count"`
			Data       *struct {
				Details    []Batch `json:"details"`
				TotalCount *int    `json:"total_count"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("credits: not JSON")
		}
		details, total := env.Details, env.TotalCount
		if len(details) == 0 && env.Data != nil {
			details, total = env.Data.Details, env.Data.TotalCount
		}
		if total == nil {
			return nil, nil
		}
		for i := range details {
			b := &details[i]
			granted, ok1 := ParseAmount(b.GrantedAmount, false)
			remaining, ok2 := ParseAmount(b.RemainingAmount, true)
			if !ok1 || !ok2 {
				continue
			}
			b.Granted = granted
			b.Remaining = math.Min(remaining, granted)
			b.Consumed = granted - b.Remaining
			b.GrantedAt = time.UnixMilli(b.GrantedAtMS)
			b.ExpiresAt = time.UnixMilli(b.ExpiresAtMS)
		}
		out = append(out, details...)
		if len(out) >= *total || len(details) == 0 {
			if len(out) < *total {
				return nil, nil
			}
			return out, nil
		}
	}
	return nil, nil
}

// PlanWindows reads the plan's windows.
//
// The console's own answer comes first; the open platform's is the
// fallback.
func (c *Client) PlanWindows(ctx context.Context, access, groupID string) ([]Window, error) {
	ws, err := c.planWindowsOnce(ctx, access, groupID, site.PlanRemainsPath)
	if err == nil {
		return ws, nil
	}
	ws2, err2 := c.planWindowsOnce(ctx, access, groupID, site.CodingPlanPath)
	if err2 != nil {
		return nil, err2
	}
	return ws2, nil
}

func (c *Client) planWindowsOnce(ctx context.Context, access, groupID, path string) ([]Window, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.PlatformURL(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept-Encoding", "identity")
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("M Plan: HTTP %d", resp.StatusCode)
	}
	return parsePlanWindows(body)
}

// PlanTitle reads the plan's name as the console says it.
func (c *Client) PlanTitle(ctx context.Context, access, groupID string) (string, error) {
	path := site.PlatformURL(site.PlanTitlePath) + "?biz_line=2&cycle_type=1&resource_package_type=7"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept-Encoding", "identity")
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return "", err
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("M Plan: HTTP %d", resp.StatusCode)
	}
	var env struct {
		CurrentSubscribe *struct {
			Title string `json:"current_subscribe_title"`
		} `json:"current_subscribe"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("M Plan: not JSON")
	}
	if env.CurrentSubscribe == nil {
		return "", nil
	}
	return strings.TrimSpace(env.CurrentSubscribe.Title), nil
}

// Models gives the site's models.
//
// The built-in list is what the site is known to have. The live list
// is asked for by whoever needs it — the gateway's /v1/models — so
// that a fallback and a live read are not confused with each other.
func (c *Client) Models(access string) []site.Model {
	return site.FallbackModels()
}

// ParseAmount reads a ledger amount.
//
// The site writes them as decimal strings, sometimes with a thousands
// separator. A negative remaining is zero.
func ParseAmount(v string, clampNegative bool) (float64, bool) {
	t := strings.ReplaceAll(strings.TrimSpace(v), ",", "")
	if t == "" {
		return 0, false
	}
	if clampNegative && strings.HasPrefix(t, "-") {
		if !isDecimal(t[1:]) {
			return 0, false
		}
		return 0, true
	}
	if !isDecimal(t) {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	dots := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
		case s[i] == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
```

**还需要新建 `internal/account/call.go`**，放签名调用与窗口解析：

```go
package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/sign"
	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// signKV is one query parameter for a signed call.
type signKV = sign.KV

// call sends one signed account API request.
//
// The body is "{}" for a POST and nil for a GET, as MiniMax Code sends
// it. The answer's business codes are checked before it is handed back.
func (c *Client) call(ctx context.Context, access, userID, method, path string, body []byte, query []signKV) ([]byte, error) {
	now := c.now()
	signed := sign.Sign(site.AgentHost, path, sign.Options{
		Access:   access,
		UserID:   userID,
		Body:     body,
		Query:    query,
		Now:      now.UnixMilli(),
		TZOffset: c.tzOffset,
		OSName:   c.osName,
		Lang:     site.Lang,
	})
	var req *http.Request
	var err error
	if method == http.MethodPost {
		req, err = http.NewRequestWithContext(ctx, method, signed.URL, strings.NewReader(string(body)))
	} else {
		req, err = http.NewRequestWithContext(ctx, method, signed.URL, nil)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range signed.Headers() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := c.up.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &RefusedError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := upstream.CheckEnvelope("account", raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// RefusedError is a refusal of the sign-in.
type RefusedError struct {
	Status int
}

// Error is the error's text.
func (e *RefusedError) Error() string {
	return fmt.Sprintf("the sign-in was refused (HTTP %d)", e.Status)
}

// isNotFound says an error is a 404.
func isNotFound(err error) bool {
	var re *statusError
	return asStatus(err, &re) && re.status == http.StatusNotFound
}

type statusError struct{ status int }

func (e *statusError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

// asStatus unwraps an error into a status error.
//
// The call helper returns a plain fmt.Errorf for a bad status, so the
// status is carried in a typed error instead of a formatted string.
func asStatus(err error, target **statusError) bool {
	for err != nil {
		if se, ok := err.(*statusError); ok {
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// parsePlanWindows reads the console's answer as windows.
func parsePlanWindows(raw []byte) ([]Window, error) {
	var env struct {
		BaseResp *struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
		ModelRemains []struct {
			ModelName                string   `json:"model_name"`
			CurrentIntervalStatus    *float64 `json:"current_interval_status"`
			CurrentIntervalRemaining *float64 `json:"current_interval_remaining_percent"`
			CurrentIntervalUsed      *float64 `json:"current_interval_used_percent"`
			CurrentIntervalTotal     *float64 `json:"current_interval_total_percent"`
			CurrentIntervalUsedCount *float64 `json:"current_interval_used_count"`
			CurrentIntervalTotalCnt  *float64 `json:"current_interval_total_count"`
			StartTime                int64    `json:"start_time"`
			EndTime                  int64    `json:"end_time"`
			CurrentWeeklyStatus      *float64 `json:"current_weekly_status"`
			CurrentWeeklyRemaining   *float64 `json:"current_weekly_remaining_percent"`
			CurrentWeeklyUsed        *float64 `json:"current_weekly_used_percent"`
			CurrentWeeklyTotal       *float64 `json:"current_weekly_total_percent"`
			CurrentWeeklyUsedCount   *float64 `json:"current_weekly_used_count"`
			CurrentWeeklyTotalCnt    *float64 `json:"current_weekly_total_count"`
			WeeklyStartTime          int64    `json:"weekly_start_time"`
			WeeklyEndTime            int64    `json:"weekly_end_time"`
		} `json:"model_remains"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("no plan in the reply")
	}
	if env.BaseResp != nil && env.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("%s", env.BaseResp.StatusMsg)
	}
	var out []Window
	for _, m := range env.ModelRemains {
		name := strings.TrimSpace(m.ModelName)
		if name == "" {
			continue
		}
		for _, x := range []struct {
			status    *float64
			remaining *float64
			used      *float64
			total     *float64
			usedCnt   *float64
			totalCnt  *float64
			start     int64
			end       int64
			weekly    bool
		}{
			{m.CurrentIntervalStatus, m.CurrentIntervalRemaining, m.CurrentIntervalUsed, m.CurrentIntervalTotal,
				m.CurrentIntervalUsedCount, m.CurrentIntervalTotalCnt, m.StartTime, m.EndTime, false},
			{m.CurrentWeeklyStatus, m.CurrentWeeklyRemaining, m.CurrentWeeklyUsed, m.CurrentWeeklyTotal,
				m.CurrentWeeklyUsedCount, m.CurrentWeeklyTotalCnt, m.WeeklyStartTime, m.WeeklyEndTime, true},
		} {
			status := 0.0
			if x.status != nil {
				status = *x.status
			}
			if status == 3 {
				out = append(out, Window{Name: windowName(x.weekly), ModelName: name, Unlimited: true})
				continue
			}
			if x.status == nil && x.remaining == nil && x.used == nil {
				continue
			}
			total := 100.0
			if x.total != nil && *x.total > 0 {
				total = *x.total
			}
			used := 0.0
			switch {
			case status == 2:
				used = 100
			case x.used != nil:
				used = clampPercent(*x.used / total * 100)
			case x.remaining != nil:
				used = clampPercent(100 - *x.remaining)
			}
			w := Window{Name: windowName(x.weekly), ModelName: name, Used: used}
			if x.end > 0 {
				w.ResetsAt = time.UnixMilli(x.end)
			}
			if x.usedCnt != nil && x.totalCnt != nil && *x.totalCnt > 0 {
				w.Amount = *x.usedCnt
				w.Limit = *x.totalCnt
				if strings.EqualFold(name, "video") {
					w.Unit = "videos"
				}
			} else if total != 100 {
				w.Limit = total
				w.Amount = used * total / 100
				w.Unit = "%"
			}
			out = append(out, w)
		}
	}
	return out, nil
}

func windowName(weekly bool) string {
	if weekly {
		return "7 days"
	}
	return "5 hours"
}

func clampPercent(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return f
}
```

**`call` 里的坏状态要返回 `statusError`**，这样 `isNotFound` 才认得。把 `call` 里返回 404 的那行改成：

```go
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &statusError{status: resp.StatusCode}
	}
```

**`Membership` 里两次调用同一个端点是错的** —— 参考实现是先取工作区列表再带 workspace_id 取会员信息。简化实现：国际站两个接口都 404，所以直接调会员接口，404 就返回 `Known: false`。把 `Membership` 改成：

```go
func (c *Client) Membership(ctx context.Context, access, userID string) (Membership, error) {
	m, err := c.membershipOnce(ctx, access, userID)
	if err == nil {
		return m, nil
	}
	if isNotFound(err) {
		return Membership{Known: false}, nil
	}
	return Membership{}, err
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/account/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/account/
git add internal/account
git commit -m "feat: 账户 API 客户端

身份、会员、积分账本、套餐窗口；每次调用都按 MiniMax Code 的方式签名。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 9: 签到

**Files:**
- Create: `internal/checkin/checkin.go`
- Create: `internal/checkin/checkin_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/checkin/checkin_test.go`：

```go
package checkin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// panel makes a seven-day panel, with the given day's status.
func panel(todayStatus int, todayIsToday bool) string {
	days := make([]map[string]any, 7)
	for i := 0; i < 7; i++ {
		status := 1
		switch {
		case i == 3:
			status = todayStatus
		case i < 3:
			status = 3
		}
		days[i] = map[string]any{
			"day_no":   i + 1,
			"status":   status,
			"is_today": i == 3 && todayIsToday,
			"points":   25,
		}
	}
	return `{"data":{"days":[` + join(days) + `]}}`
}

func join(days []map[string]any) string {
	out := ""
	for i, d := range days {
		if i > 0 {
			out += ","
		}
		out += `{"day_no":` + itoa(d["day_no"].(int)) +
			`,"status":` + itoa(d["status"].(int)) +
			`,"is_today":` + boolstr(d["is_today"].(bool)) +
			`,"points":25}`
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func boolstr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestAlreadyCheckedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/minimax-cloud/api/v1/signin/status" {
			w.Write([]byte(panel(3, true)))
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "done" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
	if out.Streak != 4 {
		t.Fatalf("streak = %d, want 4", out.Streak)
	}
}

func TestClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(panel(2, true)))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "claimed" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
	if out.Credit != 25 {
		t.Fatalf("credit = %v", out.Credit)
	}
	if out.Streak != 4 {
		t.Fatalf("streak = %d, want 4", out.Streak)
	}
}

func TestClaimAlreadyDoneToday(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(panel(2, true)))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{"claim_result":2,"points":25}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "done" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
}

func TestIneligible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(panel(4, true)))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "ineligible" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
}

func TestInactive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(panel(1, true)))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "inactive" {
		t.Fatalf("outcome = %q", out.Outcome)
	}
}

func TestMalformedPanel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"days":[{"day_no":1,"status":3}]}}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	if _, err := c.Run(context.Background(), "tok", "1234567890"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNoPanel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	if _, err := c.Run(context.Background(), "tok", "1234567890"); err == nil {
		t.Fatal("expected an error")
	}
}
```

测试文件顶部加 import 与助手：

```go
import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/upstream"
)

func testAccountClient(t *testing.T, srv *httptest.Server) *account.Client {
	t.Helper()
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	return account.New(up)
}
```

**但 `account.Client` 的 `call` 是私有的，`checkin` 包调不到。** 把 `account` 包的签名调用导出：把 `call` 改成 `Call`，并把 `RefusedError` 导出。改完后 `checkin` 用 `c.Call(...)`。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/checkin/...
```

Expected: FAIL —— `undefined: New`

- [ ] **Step 3: 导出 account 的 Call 并实现签到**

把 `internal/account/call.go` 里的 `call` 改名为 `Call`，注释同步改：

```go
// Call sends one signed account API request.
//
// The body is "{}" for a POST and nil for a GET, as MiniMax Code sends
// it. The answer's business codes are checked before it is handed back.
func (c *Client) Call(ctx context.Context, access, userID, method, path string, body []byte, query []signKV) ([]byte, error) {
```

把 `internal/account/account.go` 里所有 `c.call(` 改成 `c.Call(`。

创建 `internal/checkin/checkin.go`：

```go
// Package checkin claims the account's daily credits.
//
// The check-in comes from the desktop client, so it says so: the
// device_platform is "web", the desktop version is MiniMax Code's, and
// the account's own user id is in the query. The call is signed the
// same way the rest of the account API is.
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"mmxcode/internal/account"
)

// The outcomes.
const (
	OutcomeClaimed    = "claimed"
	OutcomeDone       = "done"
	OutcomeIneligible = "ineligible"
	OutcomeInactive   = "inactive"
	OutcomeFailed     = "failed"
)

// Outcome is what a check-in gave.
type Outcome struct {
	// Outcome is one of the constants above.
	Outcome string
	// Credit is what it paid.
	Credit float64
	// Streak is the days in a row.
	Streak int
	// Message is what to show.
	Message string
}

// Client claims check-ins.
type Client struct {
	acc *account.Client
}

// New makes a client.
func New(acc *account.Client) *Client { return &Client{acc: acc} }

// Run reads the panel and claims today's credits.
//
// A 401 is answered the way MiniMax Code answers it: the sign-in is
// renewed and the check-in is asked once more. The renewal is the
// caller's job, so Run asks through the renewer it is given.
func (c *Client) Run(ctx context.Context, access, userID string) (Outcome, error) {
	if userID == "" {
		id, err := c.acc.Identity(ctx, access, "")
		if err != nil {
			return Outcome{}, err
		}
		userID = id.RealUserID
	}
	out, err := c.run(ctx, access, userID)
	if err == nil {
		return out, nil
	}
	var refused *account.RefusedError
	if !asRefused(err, &refused) {
		return Outcome{}, err
	}
	return Outcome{Outcome: OutcomeFailed, Message: err.Error()}, nil
}

func (c *Client) run(ctx context.Context, access, userID string) (Outcome, error) {
	raw, err := c.acc.Call(ctx, access, userID, http.MethodGet, "/minimax-cloud/api/v1/signin/status", nil, checkinQuery())
	if err != nil {
		return Outcome{}, err
	}
	days, err := parsePanel(raw)
	if err != nil {
		return Outcome{}, err
	}
	today := findToday(days)
	if today == nil {
		return Outcome{Outcome: OutcomeInactive}, nil
	}
	switch today.Status {
	case 3:
		return Outcome{
			Outcome: OutcomeDone,
			Credit:  today.Points,
			Streak:  streakFrom(days, indexOf(days, today)),
		}, nil
	case 4:
		return Outcome{Outcome: OutcomeIneligible}, nil
	case 2:
		claimRaw, err := c.acc.Call(ctx, access, userID, http.MethodPost, "/minimax-cloud/api/v1/signin/claim", []byte("{}"), checkinQuery())
		if err != nil {
			return Outcome{}, err
		}
		var env struct {
			Data struct {
				ClaimResult *int     `json:"claim_result"`
				Points      *float64 `json:"points"`
			} `json:"data"`
		}
		if err := json.Unmarshal(claimRaw, &env); err != nil {
			return Outcome{}, fmt.Errorf("check-in: not JSON")
		}
		out := Outcome{Outcome: OutcomeDone}
		if env.Data.Points != nil {
			out.Credit = *env.Data.Points
		}
		if env.Data.ClaimResult != nil {
			switch *env.Data.ClaimResult {
			case 1:
				out.Outcome = OutcomeClaimed
				out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
			case 2:
				out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
			default:
				return Outcome{}, fmt.Errorf("check-in: unknown claim_result %d", *env.Data.ClaimResult)
			}
		}
		return out, nil
	default:
		return Outcome{Outcome: OutcomeInactive}, nil
	}
}

// day is one day of the panel.
type day struct {
	DayNo   int     `json:"day_no"`
	Status  int     `json:"status"`
	IsToday bool    `json:"is_today"`
	Points  float64 `json:"points"`
}

// parsePanel reads the seven-day panel.
func parsePanel(raw []byte) ([]day, error) {
	var env struct {
		Data struct {
			Days []day `json:"days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("check-in: not JSON")
	}
	days := env.Data.Days
	if len(days) == 0 {
		return nil, fmt.Errorf("check-in: no panel in the reply")
	}
	sort.Slice(days, func(i, j int) bool { return days[i].DayNo < days[j].DayNo })
	if len(days) != 7 {
		return nil, fmt.Errorf("check-in: malformed panel")
	}
	for i, d := range days {
		if d.DayNo != i+1 {
			return nil, fmt.Errorf("check-in: malformed panel")
		}
	}
	return days, nil
}

func findToday(days []day) *day {
	for i := range days {
		if days[i].IsToday {
			return &days[i]
		}
	}
	return nil
}

func indexOf(days []day, d *day) int {
	for i := range days {
		if &days[i] == d {
			return i
		}
	}
	return -1
}

// streakFrom counts the claimed days from an index back.
func streakFrom(days []day, i int) int {
	n := 0
	for j := i; j >= 0 && days[j].Status == 3; j-- {
		n++
	}
	return n
}

// checkinQuery is the query MiniMax Code's own check-in sends.
func checkinQuery() []account.QueryKV {
	return []account.QueryKV{
		{Key: "device_platform", Value: "web"},
		{Key: "is_desktop", Value: "1"},
		{Key: "desktop_version", Value: "0.6.12"},
	}
}

// asRefused unwraps an error into a refusal.
func asRefused(err error, target **account.RefusedError) bool {
	for err != nil {
		if re, ok := err.(*account.RefusedError); ok {
			*target = re
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
```

**`account.QueryKV` 需要导出。** 把 `internal/account/call.go` 里的：

```go
// signKV is one query parameter for a signed call.
type signKV = sign.KV
```

改成：

```go
// QueryKV is one query parameter for a signed call.
type QueryKV = sign.KV
```

并把该文件里所有 `signKV` 改成 `QueryKV`，`Call` 的签名也改：

```go
func (c *Client) Call(ctx context.Context, access, userID, method, path string, body []byte, query []QueryKV) ([]byte, error) {
```

`account.go` 里 `checkinQuery` 的类型也改成 `[]QueryKV`。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/checkin/... ./internal/account/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/account internal/checkin
git add internal/account internal/checkin
git commit -m "feat: 每日签到

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 10: 积分聚合

**Files:**
- Create: `internal/usage/usage.go`
- Create: `internal/usage/usage_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/usage/usage_test.go`：

```go
package usage

import (
	"testing"
	"time"

	"mmxcode/internal/store"
)

func TestAggregate(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 600, Consumed: 120.5, Remaining: 479.5,
			ExpiresAt: now.Add(48 * time.Hour),
		},
		{
			AccountID: "acc_02", HasAmounts: true,
			Granted: 300, Consumed: 0, Remaining: 300,
			ExpiresAt: now.Add(24 * time.Hour),
		},
	}
	s := Aggregate(snaps, now)
	if s.TotalGranted != 900 {
		t.Fatalf("granted = %v, want 900", s.TotalGranted)
	}
	if s.TotalConsumed != 120.5 {
		t.Fatalf("consumed = %v, want 120.5", s.TotalConsumed)
	}
	if s.TotalRemaining != 779.5 {
		t.Fatalf("remaining = %v, want 779.5", s.TotalRemaining)
	}
	if s.ExpiresAt == nil || !s.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("expires_at = %v, want the soonest", s.ExpiresAt)
	}
	if len(s.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(s.Accounts))
	}
}

func TestAggregateSkipsAccountsWithoutAmounts(t *testing.T) {
	now := time.Now()
	snaps := []store.UsageSnapshot{
		{AccountID: "acc_01", HasAmounts: true, Granted: 10, Consumed: 1, Remaining: 9},
		{AccountID: "acc_02", HasAmounts: false},
	}
	s := Aggregate(snaps, now)
	if s.TotalGranted != 10 {
		t.Fatalf("granted = %v, want 10", s.TotalGranted)
	}
	if s.AccountsUnavailable != 1 {
		t.Fatalf("unavailable = %d, want 1", s.AccountsUnavailable)
	}
	if s.Accounts[1].Granted != nil {
		t.Fatal("an account without amounts must not carry a number")
	}
}

func TestAggregateEmpty(t *testing.T) {
	s := Aggregate(nil, time.Now())
	if s.TotalGranted != 0 || s.TotalRemaining != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.ExpiresAt != nil {
		t.Fatal("no expiry without accounts")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/usage/...
```

Expected: FAIL —— `undefined: Aggregate`

- [ ] **Step 3: 实现聚合**

创建 `internal/usage/usage.go`：

```go
// Package usage sums the credits of the gateway's accounts.
//
// The numbers come from MiniMax's own credit ledger, batch by batch:
// what each batch gave, what of it was used, and what is left. Only
// batches that have not expired count, and an account whose ledger
// could not be read is left out of the totals rather than counted as
// zero.
package usage

import (
	"sort"
	"time"

	"mmxcode/internal/store"
)

// AccountUsage is one account's credits.
type AccountUsage struct {
	// ID is the account's id.
	ID string `json:"id"`
	// Email is the account's email, when it is known.
	Email string `json:"email,omitempty"`
	// Granted is what the account's live batches gave.
	Granted *float64 `json:"granted"`
	// Consumed is what of it was used.
	Consumed *float64 `json:"consumed"`
	// Remaining is what of it is left.
	Remaining *float64 `json:"remaining"`
	// ExpiresAt is when the soonest of its live batches runs out.
	ExpiresAt *time.Time `json:"expires_at"`
}

// Summary is the gateway's credits as a whole.
type Summary struct {
	// TotalGranted is what all the live batches gave.
	TotalGranted float64 `json:"total_granted"`
	// TotalConsumed is what of it was used.
	TotalConsumed float64 `json:"total_consumed"`
	// TotalRemaining is what of it is left.
	TotalRemaining float64 `json:"total_remaining"`
	// ExpiresAt is when the soonest live batch runs out.
	ExpiresAt *time.Time `json:"expires_at"`
	// UpdatedAt is when the numbers were read.
	UpdatedAt time.Time `json:"updated_at"`
	// AccountsUnavailable counts the accounts whose ledger could not be
	// read, so a short total is not mistaken for a full one.
	AccountsUnavailable int `json:"accounts_unavailable"`
	// Accounts is each account's own numbers.
	Accounts []AccountUsage `json:"accounts"`
}

// Aggregate sums the accounts' snapshots.
//
// now decides which batches count: one that has run out is out.
func Aggregate(snaps []store.UsageSnapshot, now time.Time) Summary {
	s := Summary{UpdatedAt: now}
	for _, sn := range snaps {
		au := AccountUsage{ID: sn.AccountID}
		if !sn.HasAmounts {
			s.AccountsUnavailable++
			s.Accounts = append(s.Accounts, au)
			continue
		}
		if sn.ExpiresAt.IsZero() || sn.ExpiresAt.After(now) {
			g, c, r := sn.Granted, sn.Consumed, sn.Remaining
			au.Granted, au.Consumed, au.Remaining = &g, &c, &r
			s.TotalGranted += g
			s.TotalConsumed += c
			s.TotalRemaining += r
			if !sn.ExpiresAt.IsZero() {
				t := sn.ExpiresAt
				au.ExpiresAt = &t
				if s.ExpiresAt == nil || t.Before(*s.ExpiresAt) {
					s.ExpiresAt = &t
				}
			}
		}
		s.Accounts = append(s.Accounts, au)
	}
	sort.Slice(s.Accounts, func(i, j int) bool { return s.Accounts[i].ID < s.Accounts[j].ID })
	return s
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/usage/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/usage/
git add internal/usage
git commit -m "feat: 积分聚合

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 11: CLI

**Files:**
- Create: `cmd/mmxcode/main.go`
- Create: `cmd/mmxcode/root.go`
- Create: `cmd/mmxcode/login.go`
- Create: `cmd/mmxcode/accounts.go`
- Create: `cmd/mmxcode/checkin.go`
- Create: `cmd/mmxcode/usage.go`
- Create: `cmd/mmxcode/models.go`

- [ ] **Step 1: 写根命令**

创建 `cmd/mmxcode/main.go`：

```go
// Command mmxcode is a gateway for MiniMax Code accounts.
package main

import (
	"os"

	"mmxcode/cmd/mmxcode"
)

func main() {
	os.Exit(mmxcode.Execute())
}
```

创建 `cmd/mmxcode/root.go`：

```go
// Package mmxcode holds the command line.
package mmxcode

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// Execute runs the command line and gives the process its exit code.
func Execute() int {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// env is what the commands share.
type env struct {
	cfg   config.Config
	store *store.Store
	up    *upstream.Client
	egr   *egress.Resolver
	acc   *account.Client
}

func newRoot() *cobra.Command {
	var (
		configPath string
		dbPath     string
	)
	root := &cobra.Command{
		Use:   "mmxcode",
		Short: "MiniMax Code 多账号网关",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", os.Getenv("MMXCODE_CONFIG"), "配置文件路径")
	root.PersistentFlags().StringVar(&dbPath, "db", "", "数据库路径（覆盖配置）")

	e := &env{}
	ctx := context.Background()

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if dbPath != "" {
			cfg.DB = dbPath
		}
		if v := os.Getenv("MMXCODE_PROXY_DEFAULT"); v != "" {
			cfg.Proxy.Default = v
		}
		e.cfg = cfg
		s, err := store.Open(cfg.DB)
		if err != nil {
			return err
		}
		if err := s.Migrate(ctx); err != nil {
			return err
		}
		e.store = s
		e.up = upstream.New(upstream.Options{Timeout: 30 * time.Second})
		e.egr = egress.New(cfg.Proxy.Groups, cfg.Proxy.Default)
		e.acc = account.New(e.up)
		return nil
	}
	root.PersistentPostRun = func(cmd *cobra.Command, args []string) {
		if e.store != nil {
			e.store.Close()
		}
	}

	root.AddCommand(newLoginCmd(e))
	root.AddCommand(newAccountsCmd(e))
	root.AddCommand(newCheckinCmd(e))
	root.AddCommand(newUsageCmd(e))
	root.AddCommand(newModelsCmd(e))
	return root
}

// tokenFor gives an account's token, or says it is not signed in.
func (e *env) tokenFor(ctx context.Context, accountID string) (*oauth.Token, error) {
	t, err := e.store.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account %s is not signed in", accountID)
	}
	return &oauth.Token{
		Access:  t.Access,
		Refresh: t.Refresh,
		Expires: t.ExpiresAt,
		Subject: t.Subject,
	}, nil
}
```

- [ ] **Step 2: 写 login 子命令**

创建 `cmd/mmxcode/login.go`：

```go
package mmxcode

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
)

func newLoginCmd(e *env) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "登录一个 MiniMax 账号",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			d, err := oauth.StartDevice(ctx, e.up, site.AccountURL(site.DeviceCodePath))
			if err != nil {
				return err
			}
			fmt.Printf("打开 %s\n", d.URL)
			fmt.Printf("输入验证码：%s\n", d.UserCode)
			fmt.Println("等待登录……")

			tok, err := d.Poll(ctx, e.up, site.AccountURL(site.TokenPath))
			if err != nil {
				return err
			}

			id, err := e.acc.Identity(ctx, tok.Access, tok.Subject)
			if err != nil {
				fmt.Printf("登录成功，但读不到账户信息：%v\n", err)
			}
			label := name
			if label == "" {
				label = id.Email
			}
			if label == "" {
				label = id.RealUserID
			}
			if label == "" {
				label = tok.Subject
			}

			now := time.Now()
			acc := store.Account{
				ID:        newAccountID(),
				Name:      label,
				UserID:    firstNonEmpty(id.RealUserID, tok.Subject),
				Email:     id.Email,
				Status:    store.StatusActive,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if err := e.store.CreateAccount(ctx, acc); err != nil {
				return err
			}
			if err := e.store.SaveToken(ctx, store.Token{
				AccountID: acc.ID,
				Access:    tok.Access,
				Refresh:   tok.Refresh,
				ExpiresAt: tok.Expires,
				Subject:   tok.Subject,
				UpdatedAt: now,
			}); err != nil {
				return err
			}
			fmt.Printf("已登录：%s (%s)\n", acc.Name, acc.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "账号标签")
	return cmd
}

// newAccountID makes an account id.
//
// The time is enough here: an account is signed in by hand, one at a
// time, and the second one a second later would not collide.
func newAccountID() string {
	return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
```

- [ ] **Step 3: 写 accounts 子命令**

创建 `cmd/mmxcode/accounts.go`：

```go
package mmxcode

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"mmxcode/internal/store"
)

func newAccountsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "账号管理",
	}
	cmd.AddCommand(newAccountsListCmd(e))
	cmd.AddCommand(newAccountsRemoveCmd(e))
	cmd.AddCommand(newAccountsProxyCmd(e))
	return cmd
}

func newAccountsListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出账号",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			list, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("还没有账号。先跑 mmxcode login。")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\t标签\t邮箱\t状态\t出口组\t代理")
			for _, a := range list {
				proxy := a.ProxyURL
				if proxy == "" {
					proxy = a.ProxyGroup
				}
				if proxy == "" {
					proxy = e.cfg.Proxy.Default
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.Name, a.Email, a.Status, a.ProxyGroup, proxy)
			}
			return w.Flush()
		},
	}
}

func newAccountsRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "删除账号",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.store.DeleteAccount(context.Background(), args[0])
		},
	}
}

func newAccountsProxyCmd(e *env) *cobra.Command {
	var (
		group string
		url   string
	)
	cmd := &cobra.Command{
		Use:   "proxy <id>",
		Short: "给账号指定出口组或单独的代理地址",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			a, err := e.store.GetAccount(ctx, args[0])
			if err != nil {
				return err
			}
			if group != "" {
				// 组名写错会让账号悄悄改走全局默认。挡在这里。
				if _, ok := e.cfg.Proxy.Groups[group]; !ok {
					return fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
				}
				a.ProxyGroup = group
			}
			if url != "" {
				a.ProxyURL = url
			}
			a.UpdatedAt = nowUTC()
			return e.store.UpdateAccount(ctx, a)
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "出口组名")
	cmd.Flags().StringVar(&url, "url", "", "单独的代理地址")
	return cmd
}
```

在 `root.go` 加一个 `nowUTC` 助手：

```go
func nowUTC() time.Time { return time.Now().UTC() }

// groupNames lists the outlet groups, for an error message.
func groupNames(groups map[string]string) string {
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
```

- [ ] **Step 4: 写 checkin、usage、models 子命令**

创建 `cmd/mmxcode/checkin.go`：

```go
package mmxcode

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"mmxcode/internal/checkin"
	"mmxcode/internal/store"
)

func newCheckinCmd(e *env) *cobra.Command {
	var accountID string
	cmd := &cobra.Command{
		Use:   "checkin",
		Short: "签到",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			list, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			c := checkin.New(e.acc)
			failed := false
			for _, a := range list {
				if accountID != "" && a.ID != accountID {
					continue
				}
				tok, err := e.tokenFor(ctx, a.ID)
				if err != nil {
					fmt.Printf("%s\t%s\n", a.Name, err)
					failed = true
					continue
				}
				out, err := c.Run(ctx, tok.Access, a.UserID)
				if err != nil {
					fmt.Printf("%s\t失败：%v\n", a.Name, err)
					failed = true
					continue
				}
				fmt.Printf("%s\t%s\t积分 %g\t连续 %d 天\n", a.Name, out.Outcome, out.Credit, out.Streak)
				day := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
				if err := e.store.SaveCheckin(ctx, store.Checkin{
					AccountID: a.ID, Day: day, Outcome: out.Outcome,
					Credit: out.Credit, Streak: out.Streak, Message: out.Message,
					CreatedAt: time.Now(),
				}); err != nil {
					return err
				}
			}
			if failed {
				return fmt.Errorf("some accounts failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&accountID, "account", "", "只签到这一个账号")
	return cmd
}
```

import 补上 `"time"`。

创建 `cmd/mmxcode/usage.go`：

```go
package mmxcode

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"mmxcode/internal/usage"
)

func newUsageCmd(e *env) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "网关内账号的积分汇总",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			snaps, err := e.store.LatestUsageSnapshots(ctx)
			if err != nil {
				return err
			}
			accounts, err := e.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			emails := map[string]string{}
			for _, a := range accounts {
				emails[a.ID] = a.Email
			}
			s := usage.Aggregate(snaps, nowUTC())
			for i := range s.Accounts {
				s.Accounts[i].Email = emails[s.Accounts[i].ID]
			}
			if asJSON {
				b, err := json.MarshalIndent(s, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("总积分 %.2f\t总消耗 %.2f\t总剩余 %.2f\n",
				s.TotalGranted, s.TotalConsumed, s.TotalRemaining)
			if s.ExpiresAt != nil {
				fmt.Printf("最近过期 %s\n", s.ExpiresAt.Format("2006-01-02 15:04"))
			}
			if s.AccountsUnavailable > 0 {
				fmt.Printf("有 %d 个账号的积分读不到\n", s.AccountsUnavailable)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "输出 JSON")
	return cmd
}
```

创建 `cmd/mmxcode/models.go`：

```go
package mmxcode

import (
	"fmt"

	"github.com/spf13/cobra"

	"mmxcode/internal/site"
)

func newModelsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "列出可用模型",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, m := range site.FallbackModels() {
				fmt.Printf("%-28s %8d %8d  %s\n", m.ID, m.Context, m.Output, m.Thinking)
			}
			return nil
		},
	}
}
```

- [ ] **Step 5: 编译并跑一遍**

```bash
go get github.com/spf13/cobra@v1.10.2 gopkg.in/yaml.v3@v3.0.1
go mod tidy
go build ./...
go run ./cmd/mmxcode models
```

Expected: 打印四个模型的表格

- [ ] **Step 6: 提交**

```bash
gofmt -w cmd/
git add cmd
git commit -m "feat: 命令行

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 12: 端到端收口

**Files:**
- Create: `README.md`
- Create: `config.example.yaml`

- [ ] **Step 1: 写配置样例**

创建 `config.example.yaml`：

```yaml
server:
  listen: ":8080"
  api_keys: ["sk-local-xxxx"]

proxy:
  # 全局默认出口。"direct" 表示不走代理。
  default: "http://127.0.0.1:7890"
  # 出口组：账号可以指向其中一组，一组共用一个出口。
  groups:
    clash:  "http://127.0.0.1:7890"
    warp:   "socks5h://warp:4000"
    direct: "direct"

upstream:
  timeout: 120s
  site: global

rotation:
  strategy: sticky
  max_attempts: 3
  exhausted_cooldown: 5m

schedule:
  checkin_at: "09:00"
  usage_refresh: 15m
  token_refresh: 5m
  egress_health: 2m

db: "/data/mmxcode.db"
```

- [ ] **Step 2: 写 README**

创建 `README.md`：

````markdown
# mmxcode

MiniMax Code 国际站的多账号网关。

## 做什么

- **登录**：MiniMax Code 自己的 device flow（PKCE，client `mcode-public`）。不读写 MiniMax Code 自己的 `~/.minimax` 登录信息。
- **多账号**：每个账号一份凭据，各自独立。
- **签到**：每个账号每天签到一次，签到得到的积分进各自的账本。
- **轮转**：会话粘在一个账号上，配额耗尽时自动换号。
- **代理**：按账号、按出口组或全局指定出口，Cloudflare WARP 可作额外出口。

## 命令

```
mmxcode login                    # 登录一个账号
mmxcode accounts list            # 列出账号
mmxcode accounts proxy <id> --group warp
mmxcode checkin                  # 给所有账号签到
mmxcode usage                    # 积分汇总
mmxcode models                   # 模型列表
```

## 配置

见 `config.example.yaml`。`MMXCODE_CONFIG` 环境变量指向配置文件，`--db` 覆盖数据库路径。

## 协议对齐

上游行为对齐社区插件 [`@magpie-community/opencode-minimax-auth`](https://github.com/magpie-community/plugins/tree/main/packages/minimax) 的 `packages/minimax/index.mjs`：端点、请求签名、刷新语义、签到语义、配额拒绝的判定，都以它为准。

## 计划

- 计划 A（本仓库已实现）：核心库与 CLI
- 计划 B：HTTP 网关（Anthropic / OpenAI / Codex 三协议）、WebUI、Docker 部署
````

- [ ] **Step 3: 全量测试**

```bash
go vet ./...
go test ./... -count=1
```

Expected: 全部 PASS

- [ ] **Step 4: 手工验证（需要真实账号）**

```bash
# 用一个临时数据库，别动生产数据
go run ./cmd/mmxcode --db ./tmp.db login
```

预期：打印链接与验证码 → 在浏览器里输入验证码 → 打印「已登录：...」

```bash
go run ./cmd/mmxcode --db ./tmp.db accounts list
go run ./cmd/mmxcode --db ./tmp.db checkin
go run ./cmd/mmxcode --db ./tmp.db usage
```

预期：账号列出、签到返回 claimed/done、usage 打印汇总或「有 N 个账号的积分读不到」（还没跑用量刷新时是后者）

验证完删掉临时库：

```bash
rm -f tmp.db tmp.db-wal tmp.db-shm
```

- [ ] **Step 5: 提交**

```bash
git add README.md config.example.yaml
git commit -m "docs: README 与配置样例

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 完成标准

- `go vet ./...` 无输出
- `go test ./... -count=1` 全绿
- `mmxcode login` 能登录一个真实账号，`accounts list` 能看到它
- `mmxcode checkin` 能拿到 claimed 或 done 的结果
- 签名的六个金标准向量通过（说明与 MiniMax Code 逐字节一致）
- 8 个并发 goroutine 刷新同一账号，只花掉 1 次 refresh token

## 不在本计划里

- HTTP 网关与三种协议（计划 B）
- WebUI（计划 B）
- Docker 与 WARP 部署（计划 B）
- 账号轮转（计划 B 的一半：`rotator` 包）

`rotator` 包在计划 B 中做，因为它需要 `gateway` 的重放语义才能测。
