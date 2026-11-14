# mmxcode 核心库 + CLI 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付一个可独立使用的 Go 核心库与 CLI：登录 MiniMax Code 国际站、管理多账号、每日签到、汇总积分、按账号/组配置代理出口。

**Architecture:** 分层包结构，`site`（常量）→ `sign`（签名，纯函数）→ `store`（SQLite）→ `upstream`（HTTP 客户端）→ `egress`（出口）→ `oauth`/`account`/`checkin`（MiniMax 协议）→ `usage`（聚合）→ `cmd/mmxcode`（CLI）。所有 MiniMax 协议细节对齐参考插件 `@magpie-community/opencode-minimax-auth` 的 `packages/minimax/index.mjs`。

**Tech Stack:** Go 1.26（由 Go 1.21 的 toolchain 自动机制拉取）、`modernc.org/sqlite` v1.62.0（纯 Go，无 cgo）、`golang.org/x/net` v0.62.0（SOCKS5 拨号）、`github.com/spf13/cobra` v1.10.2、`gopkg.in/yaml.v3` v3.0.1。

**设计文档：** `docs/superpowers/specs/2026-11-13-mmxcode-gateway-design.md`

---

## 本计划的范围

本计划交付 **可用的核心库 + CLI**：登录 → 签到 → 看积分 → 管理账号与代理出口。全部有单测，可 `go test ./...` 验证，可手工端到端验证。

HTTP 网关（三种协议、账号轮转）、WebUI、Docker 部署在 **计划 B**（`docs/superpowers/plans/2026-11-13-mmxcode-gateway.md`）中。

这样切分是因为计划 A 结束后已经有一个真正能用的产品（CLI），而不是一堆只有单元测试的库。

## 环境前提

- 本机 Go 为 1.21.0，但 `go.mod` 写 `go 1.26.0`，Go 1.21 会自动下载并使用 1.26 工具链（已验证可用）
- `modernc.org/sqlite` v1.62.0 要求 `go >= 1.26.0`，所以 go 指令不能低于 1.26 —— 它比 sqlite 老两个大版本（v1.59.0 起就要 1.25，v1.48.2 起就要 1.25），没有既能配 1.23 的版本
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

然后写 `go.mod`（保留 `go mod init` 生成的 module 行，把 go 指令改成 1.26.0）：

```
module mmxcode

go 1.26.0
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
	body := "\ufeff" + "server:\n  listen: \":9090\"\n"
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

func TestLoadRejectsAnEmptyAPIKey(t *testing.T) {
	// 空的一把 key 等于没有 key：配置里写了空串的时候，一个不带 key
	// 的请求带来的也是空串，两边一比就"相等"了。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "server:\n  api_keys: [\"sk-a\", \"\"]\n"); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for the empty key")
	}
	if !strings.Contains(err.Error(), "api_keys[1]") {
		t.Fatalf("error %q does not name the position of the key", err)
	}
}

func TestLoadRejectsAWhitespaceAPIKey(t *testing.T) {
	// 只有空白的 key 和空的一样：一把 key 总得有字。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "server:\n  api_keys: [\"   \"]\n"); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for the whitespace key")
	}
	if !strings.Contains(err.Error(), "api_keys[0]") {
		t.Fatalf("error %q does not name the position of the key", err)
	}
}

func TestLoadAcceptsAKeyWithSpacesInIt(t *testing.T) {
	// key 前后有空格是有可能的：它是一段口令，不是一个词。
	// 只有全空白的 key 才是配错了。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "server:\n  api_keys: [\" sk-a \"]\n"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Server.APIKeys) != 1 || c.Server.APIKeys[0] != " sk-a " {
		t.Fatalf("api_keys = %v", c.Server.APIKeys)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
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

// KV is one query parameter.
//
// It is the site's own type, so that a package that builds a query does
// not have to know about the signing package to say what a parameter
// is. The signing package has one of its own, and the two are the same
// shape.
type KV struct {
	// K is the parameter's name.
	K string
	// V is the parameter's value.
	V string
}

// CheckinQuery is the query MiniMax Code's own check-in sends.
//
// The check-in comes from the desktop client, so it says so: the
// device_platform is "web", the desktop version is MiniMax Code's, and
// the account's own user id is in the query.
//
// It is kept here, once: the check-in and the credits ledger ask the
// same way, and the reference has one copy of it too.
var CheckinQuery = []KV{
	{K: "device_platform", V: "web"},
	{K: "is_desktop", V: "1"},
	{K: "desktop_version", V: "0.6.12"},
}

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

import (
	"strings"
	"testing"

	"mmxcode/internal/site"
)

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

			headers := got.Headers()
			want := map[string]string{
				"Authorization": "Bearer " + c.opts.Access,
				"Accept":        "application/json",
				"Content-Type":  "application/json",
				"User-Agent":    site.AccountUserAgent,
				"yy":            c.wantYY,
				"x-timestamp":   c.wantTS,
				"x-signature":   c.wantSig,
			}
			for name, value := range want {
				if headers[name] != value {
					t.Errorf("header %s = %q, want %q", name, headers[name], value)
				}
			}
		})
	}
}

// The query the reference builds for a plain request, so that the cases
// below can spell out only what they change.
const plainQuery = "device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
	"&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
	"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode"

// The reference resolves the path against the origin and signs the
// resolved, percent-encoded path, so these all sign something other
// than what the path looks like.
func TestSignResolvesThePath(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		path    string
		wantURL string
		wantYY  string
	}{
		{
			name:   "a path without a leading slash",
			origin: "https://agent.minimax.io",
			path:   "v1/api/user/info",
			// The reference resolves it against the origin, so it is
			// the same request as the golden identity GET.
			wantURL: "https://agent.minimax.io/v1/api/user/info?" + plainQuery,
			wantYY:  "40865acf7e929978824a2892b1cc0e7b",
		},
		{
			name:    "a path with a space in it",
			origin:  "https://agent.minimax.io",
			path:    "/v1/a b",
			wantURL: "https://agent.minimax.io/v1/a%20b?" + plainQuery,
			wantYY:  "5bf73d0b32d9919785be6e1cf3b76a84",
		},
		{
			name:   "a path with a query of its own",
			origin: "https://agent.minimax.io",
			path:   "/v1/x?a=1",
			// The reference assigns the defaults to url.search, which
			// replaces the path's own query.
			wantURL: "https://agent.minimax.io/v1/x?" + plainQuery,
			wantYY:  "390ae9a1f47d7d6bb3e1146f2ae666e5",
		},
		{
			name:   "an origin with a path of its own",
			origin: "https://agent.minimax.io/base/",
			path:   "/v1/x",
			// An absolute path replaces the origin's path.
			wantURL: "https://agent.minimax.io/v1/x?" + plainQuery,
			wantYY:  "390ae9a1f47d7d6bb3e1146f2ae666e5",
		},
		{
			name:   "a path with a percent that is not an escape",
			origin: "https://agent.minimax.io",
			path:   "/v1/100%",
			// JavaScript keeps a "%" that is not an escape as it is,
			// so the reference signs it as it is too.
			wantURL: "https://agent.minimax.io/v1/100%?" + plainQuery,
			wantYY:  "203556ac721d12836c9fa21a52201df3",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign(c.origin, c.path, Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
			})
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
			}
		})
	}
}

// The reference builds its query as an object literal with the
// caller's query spread into the middle of it, so a repeated key takes
// the caller's place but only the leading defaults take the caller's
// value.
func TestQueryDefaults(t *testing.T) {
	cases := []struct {
		name    string
		query   []KV
		wantURL string
		wantYY  string
	}{
		{
			name:  "a leading default is the caller's to override",
			query: []KV{{"device_platform", "web"}},
			wantURL: "https://agent.minimax.io/p?device_platform=web&biz_id=3&app_id=3001&version_code=22201" +
				"&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "98e6fa51a036f804c2c65e80295a731b",
		},
		{
			name:  "lang keeps the reference's value",
			query: []KV{{"lang", "zh"}},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&lang=en&unix=1763150000000&timezone_offset=-480&sys_language=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "d2e7a1e8b377f6fa116a09b1d9825613",
		},
		{
			name:  "client keeps the reference's value",
			query: []KV{{"client", "x"}},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&client=mcode&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890",
			wantYY: "f73c2ba5320fdc2145e65d82d40b14b8",
		},
		{
			name:  "user_id keeps the option's value",
			query: []KV{{"user_id", "q"}},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&user_id=1234567890&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&client=mcode",
			wantYY: "84f842303887f8956faa37fad5226a13",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign("https://agent.minimax.io", "/p", Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
				Query: c.query,
			})
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
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
	if !strings.Contains(got.URL, want) {
		t.Fatalf("URL %s does not contain %s", got.URL, want)
	}
}

// The options that fall back to a default, and the ones that are
// trimmed, are all part of the signed query.
func TestSignOptions(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantURL string
		wantYY  string
	}{
		{
			name: "a language of its own",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
				Lang: "zh",
			},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&unix=1763150000000&timezone_offset=-480&sys_language=zh&lang=zh" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "d6bbe2721fce479b89ca1b7c05de3591",
		},
		{
			name: "no platform name falls back to linux",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480,
			},
			wantURL: "https://agent.minimax.io/p?" + plainQuery,
			wantYY:  "cf6860c1fac0feba1367dc97c06cd876",
		},
		{
			name: "a user id with whitespace around it",
			opts: Options{
				Access: "tok_1", UserID: "  1234567890  ",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
			},
			wantURL: "https://agent.minimax.io/p?" + plainQuery,
			wantYY:  "cf6860c1fac0feba1367dc97c06cd876",
		},
		{
			name: "a query value with a space and a non-ASCII character",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
				Query: []KV{{"q", "a b 中文"}},
			},
			// URLSearchParams encodes a space as "+" and everything
			// else as UTF-8 percent escapes.
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&q=a+b+%E4%B8%AD%E6%96%87&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "79bfb3e01c70c286bc75523a8c43a2b2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign("https://agent.minimax.io", "/p", c.opts)
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
			}
		})
	}
}

// An empty body is a GET: the reference has no way to send one, and a
// POST with an empty body would sign a body the reference never signs.
func TestSignEmptyBodyIsGet(t *testing.T) {
	opts := Options{
		Access: "tok_1", UserID: "1234567890",
		Now: 1763150000000, TZOffset: -480, OSName: "linux",
	}
	get := Sign("https://agent.minimax.io", "/p", opts)
	empty := Sign("https://agent.minimax.io", "/p", Options{
		Access: opts.Access, UserID: opts.UserID, Body: []byte{},
		Now: opts.Now, TZOffset: opts.TZOffset, OSName: opts.OSName,
	})
	if empty.YY != get.YY {
		t.Errorf("yy = %s, want the GET one %s", empty.YY, get.YY)
	}
	if empty.Signature != get.Signature {
		t.Errorf("signature = %s, want the GET one %s", empty.Signature, get.Signature)
	}
	if want := "cf6860c1fac0feba1367dc97c06cd876"; empty.YY != want {
		t.Errorf("yy = %s, want %s", empty.YY, want)
	}
	if want := "12f7ac9ac565a90bb1d7652ab46ac58c"; empty.Signature != want {
		t.Errorf("signature = %s, want %s", empty.Signature, want)
	}
}

// The reference floors the seconds and Go's division truncates towards
// zero.
func TestSignFloorsNegativeSeconds(t *testing.T) {
	got := Sign("https://agent.minimax.io", "/p", Options{
		Access: "tok_1", UserID: "1234567890",
		Now: -1500, TZOffset: -480, OSName: "linux",
	})
	if got.Timestamp != "-2" {
		t.Errorf("timestamp = %s, want -2", got.Timestamp)
	}
	if want := "f13f639021bd969f8a1dad1c171b5785"; got.YY != want {
		t.Errorf("yy = %s, want %s", got.YY, want)
	}
	if want := "c70bef52e842c40feaa0a33d76040802"; got.Signature != want {
		t.Errorf("signature = %s, want %s", got.Signature, want)
	}
}

// The user id is trimmed the way JavaScript trims a string, which is
// not quite strings.TrimSpace: JavaScript strips U+FEFF and keeps
// U+0085, and Go does the opposite.
func TestJSTrimMatchesJavaScript(t *testing.T) {
	bom := string(rune(0xFEFF))
	nel := string(rune(0x0085))
	cases := map[string]string{
		"  42  ":     "42",
		"\t\n42\r\n": "42",
		bom + "42":   "42",
		"42" + nel:   "42" + nel,
	}
	for in, want := range cases {
		if got := jsTrim(in); got != want {
			t.Errorf("jsTrim(%q) = %q, want %q", in, got, want)
		}
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
// Three details make this different from an ordinary query builder, and
// all three come from how JavaScript does it:
//
//   - the query keeps its insertion order. URLSearchParams does not sort
//     its keys, and the signature is over the encoded text, so the order
//     is part of the contract. Go's url.Values.Encode() sorts, and would
//     produce a different signature every time;
//   - the encoding is JavaScript's. URLSearchParams keeps "*-._", encodes
//     a space as "+" and encodes "~"; encodeURIComponent keeps
//     "!*'()~", encodes a space as "%20" and does not touch "~". Go's
//     url.QueryEscape does neither exactly;
//   - the path is resolved before it is signed. The reference resolves
//     the path against the origin with `new URL(path, origin)` and signs
//     the resolved, percent-encoded path, so a path without a leading
//     slash, a path with a space in it and a path with a query of its own
//     all sign something other than what they look like.
package sign

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
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
	// UserID is the account's MiniMax user id. It is trimmed the way
	// JavaScript trims a string — the WhiteSpace and LineTerminator
	// characters, which is not quite what strings.TrimSpace does — and
	// an empty one becomes "0".
	UserID string
	// Body is the request's JSON body. Its bytes go into both hashes
	// as they are, so the caller must pass exactly the bytes it will
	// put on the wire. Go's json.Marshal is not JSON.stringify: it
	// escapes "<", ">" and "&" and it sorts a map's keys, and
	// JSON.stringify does neither. A body that has to match the
	// reference byte for byte is built by hand.
	//
	// A nil or empty body means a GET.
	Body []byte
	// Query holds the caller's extra query parameters, in order.
	//
	// The reference builds its query as an object literal with the
	// caller's query spread into the middle of it, and the two halves
	// do not treat a repeated key the same way:
	//
	//   - the four leading defaults — device_platform, biz_id, app_id
	//     and version_code — come before the spread, so a repeated key
	//     takes the caller's value and keeps the caller's place;
	//   - the trailing defaults — unix, timezone_offset, sys_language,
	//     lang, device_id, os_name, browser_name, user_id and client —
	//     come after it, so a repeated key keeps the caller's place but
	//     takes the reference's value. "lang" and "client" are the
	//     reference's, "user_id" is the option's.
	Query []KV
	// Now is the request's time in Unix milliseconds.
	//
	// It is required. There is no default on purpose — a default of
	// time.Now() would make a signed request unreproducible — and the
	// zero value signs the Unix epoch, which the server reads as a
	// stale request.
	Now int64
	// TZOffset is the client's timezone offset in seconds.
	TZOffset int
	// OSName is the client's platform name. An empty one is "linux".
	OSName string
	// Lang is the site's language. An empty one is the site's own.
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
	p.set("user_id", jsTrim(o.UserID))
	if p.get("user_id") == "" {
		p.set("user_id", "0")
	}
	p.set("client", "mcode")

	query := p.encode()
	target, at := resolveURL(origin, path, query)
	ts := o.Now / 1000
	if o.Now < 0 && o.Now%1000 != 0 {
		// The reference floors the seconds and Go's division truncates
		// towards zero, so -1500 milliseconds is -2 seconds there and
		// -1 second here.
		ts--
	}
	nowText := strconv.FormatInt(o.Now, 10)

	// The reference's signed() — packages/minimax/index.mjs of
	// @magpie-community/opencode-minimax-auth, the one copied verbatim
	// into testdata/regen-vectors.mjs — builds the two hashes out of
	// the same parts:
	//
	//	yy:          md5(encodeURIComponent(at) + "_" + body + md5(now) + "ooui")
	//	x-signature: md5(seconds + secret + body)
	//
	// The "_" separator, the md5 of the time and the "ooui" salt are
	// the reference's, verbatim. They read like typos and are not.
	var yy, signature string
	if len(o.Body) == 0 {
		// The reference has no body for a GET, and signs a literal
		// "{}" in the body's place. The "{}" is the reference's too,
		// and not a placeholder to be tidied away: dropping it changes
		// every GET signature.
		yy = md5Hex(encodeURIComponent(at) + "_" + "{}" + md5Hex(nowText) + "ooui")
		signature = md5Hex(strconv.FormatInt(ts, 10) + Secret)
	} else {
		body := string(o.Body)
		yy = md5Hex(encodeURIComponent(at) + "_" + body + md5Hex(nowText) + "ooui")
		signature = md5Hex(strconv.FormatInt(ts, 10) + Secret + body)
	}

	return Signed{
		URL:       target,
		YY:        yy,
		Timestamp: strconv.FormatInt(ts, 10),
		Signature: signature,
		access:    o.Access,
	}
}

// resolveURL gives the URL a signed request goes to and the string the
// reference signs.
//
// The reference resolves the path against the origin with
// `new URL(path, origin)` and signs url.pathname, which is the
// resolved and percent-encoded path. So a path without a leading slash
// is resolved against the origin's own path, a space in the path is
// escaped, and a query in the path is dropped — the reference assigns
// the defaults to url.search afterwards, and that replaces it.
//
// The fragment is left out of the signed string, as it is in the
// reference, and kept in the URL, as it is there too.
func resolveURL(origin, path, query string) (target, at string) {
	base, err := url.Parse(origin)
	if err != nil || base.Host == "" {
		// An origin that is not a URL is used as the prefix as it is,
		// and the path is taken as an absolute path.
		escaped := absolutePath(path)
		return strings.TrimRight(origin, "/") + escaped + "?" + query, escaped + "?" + query
	}
	if base.Scheme == "" {
		base.Scheme = "https"
	}
	ref, err := url.Parse(path)
	if err != nil {
		// url.Parse rejects a path with an invalid percent escape
		// ("/v1/100%"), which JavaScript keeps as it is. Such a path
		// is escaped here the way JavaScript escapes a path.
		escaped := absolutePath(path)
		return base.Scheme + "://" + base.Host + escaped + "?" + query, escaped + "?" + query
	}
	resolved := base.ResolveReference(ref)
	// The reference replaces the search with the defaults, so the
	// path's own query is dropped.
	resolved.RawQuery = ""
	resolved.ForceQuery = false
	escaped := resolved.EscapedPath()
	if escaped == "" {
		// A URL always has a path, at least a slash.
		escaped = "/"
	}
	target = resolved.Scheme + "://" + resolved.Host + escaped + "?" + query
	if resolved.Fragment != "" {
		target += "#" + resolved.EscapedFragment()
	}
	return target, escaped + "?" + query
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

// absolutePath escapes a path the way JavaScript escapes one and makes
// it absolute.
func absolutePath(path string) string {
	escaped := jsPath(path)
	if !strings.HasPrefix(escaped, "/") {
		escaped = "/" + escaped
	}
	return escaped
}

// jsPath escapes a path the way JavaScript's URL parser escapes one:
// the C0 controls, everything above "~" and " # < > ? ` { }" are
// percent-encoded and the rest is left as it is. A "%" that is not a
// valid escape is left alone, as JavaScript leaves it.
func jsPath(s string) string {
	const escaped = "\"#<>?`{}"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c <= 0x20 || c >= 0x7F:
			fmt.Fprintf(&b, "%%%02X", c)
		case strings.IndexByte(escaped, c) >= 0:
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// jsTrim trims a string the way JavaScript's String.prototype.trim
// does: the WhiteSpace and LineTerminator characters.
//
// It is not quite strings.TrimSpace, which differs on two code points —
// JavaScript strips U+FEFF and keeps U+0085 — and the difference
// matters here, because the user id goes into the signed query.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// isJSSpace says whether a rune is one of JavaScript's WhiteSpace and
// LineTerminator characters.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0xFEFF,
		0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
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
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

// makeAccount puts an account in the store.
//
// The children say which account they belong to, so a child without an
// account is refused.
func makeAccount(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.CreateAccount(context.Background(), Account{
		ID: id, Name: id, Status: StatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
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
	got, err = s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
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

// TestNotFoundIsOneSentinel says every way of saying "there is no
// such row" says it the same way.
//
// 找不到就是找不到，一种说法就够。
func TestNotFoundIsOneSentinel(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.GetAccount(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get account: %v, want ErrNotFound", err)
	}
	if _, err := s.GetToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get token: %v, want ErrNotFound", err)
	}
	err := s.UpdateAccount(ctx, Account{ID: "nope", Name: "x", Status: StatusActive})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update account: %v, want ErrNotFound", err)
	}
	err = s.DeleteAccount(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete account: %v, want ErrNotFound", err)
	}
	// 哨兵不能是 database/sql 那个
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("the error is the database's own sentinel")
	}
}

// TestDeleteAccountTakesTheChildrenWithIt says an account's children
// go with the account.
//
// 删除账号要带着它的账本、签到、用量和会话一起走。
func TestDeleteAccountTakesTheChildrenWithIt(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveToken(ctx, Token{AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExhausted(ctx, "acc_01", "general", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: "2026-11-13",
		Outcome: "claimed", Credit: 25, Streak: 3, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Remaining: 100, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"tokens", "exhausted_windows", "checkins", "usage_snapshots", "sessions",
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows left, want none", table, n)
		}
	}
}

// TestChildRowWithoutAnAccountIsRefused says a child row of an
// account that is not there is not accepted.
//
// 没有账号的孩子不算数。
func TestChildRowWithoutAnAccountIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	err := s.SaveToken(ctx, Token{AccountID: "nope", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if err == nil {
		t.Fatal("the token of an account that is not there was accepted")
	}
	err = s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "nope", HasAmounts: true,
		Granted: 1, Remaining: 1, CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("the reading of an account that is not there was accepted")
	}
	err = s.BindSession(ctx, "sess-1", "nope")
	if err == nil {
		t.Fatal("the session of an account that is not there was accepted")
	}
	err = s.SaveCheckin(ctx, Checkin{AccountID: "nope", Day: "2026-11-13",
		Outcome: "claimed", CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("the check-in of an account that is not there was accepted")
	}
	err = s.SetExhausted(ctx, "nope", "general", time.Now().Add(time.Hour), "quota")
	if err == nil {
		t.Fatal("the window of an account that is not there was accepted")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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

// TestANullSubjectIsReadable says a token whose subject was never
// written can still be read.
//
// 老库里的 subject 可以是空的，空的也要读得出来。
func TestANullSubjectIsReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "" {
		t.Fatalf("subject = %q, want nothing", got.Subject)
	}
	if got.Access != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestCheckinUniquePerDay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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

// TestANullCreditIsReadable says a check-in whose credit was never
// written can still be read.
func TestANullCreditIsReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %+v", list)
	}
	if list[0].Credit != 0 || list[0].Streak != 0 {
		t.Fatalf("got %+v", list[0])
	}
}

// TestListCheckinsLimit says a limit of zero is no limit.
func TestListCheckinsLimit(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	for _, day := range []string{"2026-11-11", "2026-11-12", "2026-11-13"} {
		if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: day,
			Outcome: "claimed", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListCheckins(ctx, "acc_01", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d check-ins, want all three", len(list))
	}
	list, err = s.ListCheckins(ctx, "acc_01", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d check-ins, want two", len(list))
	}
}

func TestExhaustedWindows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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
	got, err = s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// TestTheWindowsThatHaveComeBackAreStillThere says ListExhausted
// gives the windows that have run out as well.
//
// 到期的窗口还在表里，谁在用自己看着办。
func TestTheWindowsThatHaveComeBackAreStillThere(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	now := time.Now()
	if err := s.SetExhausted(ctx, "acc_01", "general", now.Add(-time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want the expired window too", got)
	}
	if err := s.PruneExpiredWindows(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want the expired window gone", got)
	}
}

// TestPruneUsageSnapshots says the old readings are thrown away and
// the newest ones are kept.
//
// 旧读数扔掉，新的留下。
func TestPruneUsageSnapshots(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 5; i++ {
		if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
			Granted: float64(i), Remaining: float64(i), CreatedAt: old.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	// 一条新的
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
		Granted: 9, Remaining: 9, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := s.PruneUsageSnapshots(ctx, time.Now().Add(-24*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	// 每个账号留最新的一条
	if n != 1 {
		t.Fatalf("got %d readings, want the newest one", n)
	}
	// 一条都不留的时候就没有了
	if err := s.PruneUsageSnapshots(ctx, time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d readings, want none", n)
	}
}

func TestSessionBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	makeAccount(t, s, "acc_02")
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

// TestUpstreamIDIsRandomAndKept says every session has an upstream id
// of its own, and keeps it.
//
// 两段一样的开场白不能共用一个上游会话。
func TestUpstreamIDIsRandomAndKept(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	first, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("the session has no upstream id")
	}
	again, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("got %q and %q, want the same id", again, first)
	}
	other, err := s.UpstreamID(ctx, "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("two sessions got the same upstream id")
	}
	// 同一个 key 再问还是那个 id
	third, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("got %q, want %q", third, first)
	}
}

// TestTheUpstreamIDSurvivesTheBinding says the upstream id belongs
// to the conversation and not to the account.
func TestTheUpstreamIDSurvivesTheBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	id, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}
	again, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("got %q, want %q", again, id)
	}
	acc, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || acc != "acc_01" {
		t.Fatalf("got %q %v %v", acc, ok, err)
	}
}

func TestUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 600, Consumed: 120.5, Remaining: 479.5,
		HasAmounts: true,
		ExpiresAt:  time.Now().Add(time.Hour), Plan: "M Plan",
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
	if !got[0].HasAmounts {
		t.Fatal("the amounts were lost")
	}
}

func TestUsageSnapshotWithoutAmounts(t *testing.T) {
	// 读不到账本和读到 0 是两回事：读不到就没有数
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_02")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_02", Plan: "M Plan", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].HasAmounts {
		t.Fatalf("a reading with no amounts says it has amounts: %+v", got[0])
	}
}

// TestTheAmountsAreKeptWithoutTheFlag says a caller that forgets to
// say that a reading has amounts does not lose the numbers.
//
// 忘了说有没有数，数还在。
func TestTheAmountsAreKeptWithoutTheFlag(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 100, Consumed: 10, Remaining: 90,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasAmounts {
		t.Fatalf("the numbers were lost: %+v", got)
	}
	if got.Granted != 100 || got.Consumed != 10 || got.Remaining != 90 {
		t.Fatalf("got %+v", got)
	}
}

// TestLatestUsageSnapshotIsOneRowPerAccount says two readings of one
// account in the same millisecond come back as one row.
//
// 两行都回来的时候求和会把同一个账号的积分算两遍。
func TestLatestUsageSnapshotIsOneRowPerAccount(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	// 同一毫秒里的两次刷新
	at := time.UnixMilli(1763000000000)
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Consumed: 0, Remaining: 100,
		ExpiresAt: at.Add(time.Hour), CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 300, Consumed: 0, Remaining: 300,
		ExpiresAt: at.Add(time.Hour), CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want one per account", len(got))
	}
	if got[0].Remaining != 300 {
		t.Fatalf("got %+v, want the newest reading", got[0])
	}
}

// TestLatestUsageSnapshotGivesOneAccount says one account's newest
// reading is asked for on its own.
func TestLatestUsageSnapshotGivesOneAccount(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	for _, id := range []string{"acc_01", "acc_02"} {
		makeAccount(t, s, id)
		if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
			AccountID: id, HasAmounts: true, Granted: 10, Remaining: 10, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_02")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("got %+v, want acc_02", got)
	}
	// 没有读过的账号没有读数，也没有错
	none, err := s.LatestUsageSnapshot(ctx, "acc_99")
	if err != nil {
		t.Fatal(err)
	}
	if none.AccountID != "" {
		t.Fatalf("got %+v, want nothing", none)
	}
}

// TestUsageSnapshotKeepsTheReadErrors says the errors of the parts
// that could not be read are written down with the reading.
//
// 读不到窗口和没有窗口不是一回事，读数要能说出来。
func TestUsageSnapshotKeepsTheReadErrors(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 10, Remaining: 10,
		MembershipError: "the membership is not there",
		WindowsError:    "the windows are not there",
		CreatedAt:       time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.MembershipError != "the membership is not there" {
		t.Fatalf("got %+v", got)
	}
	if got.WindowsError != "the windows are not there" {
		t.Fatalf("got %+v", got)
	}
}

// TestDayKeyIsTheSitesDay says the day key is the site's day.
//
// The site counts its days in China: a check-in at half past midnight
// in that zone is the next day, whatever the machine's own zone is.
func TestDayKeyIsTheSitesDay(t *testing.T) {
	// 2026-11-13 20:00 UTC is 2026-11-14 04:00 in China
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	if got := DayKey(at); got != "2026-11-14" {
		t.Fatalf("day = %q, want 2026-11-14", got)
	}
}

// TestTheTimesComeBackInUTC says a time that was written in UTC comes
// back in UTC.
//
// 写进去是 UTC，读出来也得是 UTC。
func TestTheTimesComeBackInUTC(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 1, Remaining: 1, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at is in %v, want UTC", got.CreatedAt.Location())
	}
	if !got.CreatedAt.Equal(at) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, at)
	}
}

// TestTheStoreHasAClock says the store's clock is what a row is
// written at when the caller does not say.
//
// 调用方不说是几点，存储层自己说。
func TestTheStoreHasAClock(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	s.Clock = func() time.Time { return at }
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 1, Remaining: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(at) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, at)
	}
}

// TestAnUnknownStatusIsRefused says an account cannot have a status
// nobody wrote.
//
// 写错的状态会让账号悄悄不能用。
func TestAnUnknownStatusIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	err := s.CreateAccount(ctx, Account{ID: "acc_01", Name: "a", Status: "actve"})
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
	makeAccount(t, s, "acc_02")
	err = s.UpdateAccount(ctx, Account{ID: "acc_02", Name: "a", Status: "actve"})
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
	err = s.UpdateAccountStatus(ctx, "acc_02", "actve", "", time.Now())
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
}

// TestTheNarrowSetters says the state and the outlet are written
// without reading the account first.
func TestTheNarrowSetters(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.UpdateAccountStatus(ctx, "acc_01", StatusSignedOut, "the sign-in was refused", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSignedOut || got.LastError != "the sign-in was refused" {
		t.Fatalf("got %+v", got)
	}
	if err := s.SetAccountProxy(ctx, "acc_01", "warp", "http://127.0.0.1:1080", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyGroup != "warp" || got.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("got %+v", got)
	}
	// 没有的账号还是找不到
	if err := s.UpdateAccountStatus(ctx, "nope", StatusActive, "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update the status: %v, want ErrNotFound", err)
	}
	if err := s.SetAccountProxy(ctx, "nope", "warp", "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set the outlet: %v, want ErrNotFound", err)
	}
}

// TestThePragmasAreOnEveryConnection says every connection of the
// pool has the pragmas, and not only the first one.
//
// 每个连接都得带着 pragma，不能只有第一个。
func TestThePragmasAreOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	conns := make([]*sql.Conn, 0, maxConns)
	for i := 0; i < maxConns; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i, c := range conns {
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Fatalf("connection %d has foreign_keys = %d, want 1", i, fk)
		}
		var busy int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if busy != 5000 {
			t.Fatalf("connection %d has busy_timeout = %d, want 5000", i, busy)
		}
		var mode string
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Fatalf("connection %d has journal_mode = %q, want wal", i, mode)
		}
	}
}

// TestOneStoreIsUsedFromSeveralGoroutines says the store can be used
// by several goroutines at once.
//
// 几个线程一起用也不该打架。
func TestOneStoreIsUsedFromSeveralGoroutines(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("acc_%02d", i)
			if err := s.CreateAccount(ctx, Account{ID: id, Name: id, Status: StatusActive}); err != nil {
				errs <- err
				return
			}
			for j := 0; j < 5; j++ {
				if _, err := s.GetAccount(ctx, id); err != nil {
					errs <- err
					return
				}
				if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
					AccountID: id, HasAmounts: true, Granted: 1, Remaining: 1,
					CreatedAt: time.Now(),
				}); err != nil {
					errs <- err
					return
				}
				if _, err := s.LatestUsageSnapshots(ctx); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("the store was used from several goroutines: %v", err)
	}
}

// TestThePathWithAQuestionMarkIsThePath says a path with a question
// mark in it is the path and not the end of the file name.
//
// 路径里有问号，问号不能把文件名截断。
func TestThePathWithAQuestionMarkIsThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a?b&c#d%e.db")

	d := dsn(path)
	if !strings.HasPrefix(d, "file:") {
		t.Fatalf("dsn = %q, want a file: URI", d)
	}
	if !strings.Contains(d, "%3F") {
		t.Fatalf("dsn = %q, want the question mark escaped", d)
	}
	if !strings.Contains(d, "%26") || !strings.Contains(d, "%23") || !strings.Contains(d, "%25") {
		t.Fatalf("dsn = %q, want the other marks escaped", d)
	}
	if !strings.Contains(d, "_pragma=foreign_keys(1)") {
		t.Fatalf("dsn = %q, want the pragmas", d)
	}

	if runtime.GOOS == "windows" {
		// Windows does not allow a question mark in a file name, so
		// the file itself cannot be made there.
		t.Skip("windows does not allow a question mark in a file name")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database is not at the path: %v", err)
	}
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

// TestTheFileIsOnlyForItsOwner says the database file is not for
// everybody.
//
// 数据库里有明文的 refresh token，别人不能读。
//
// 目录是存储层自己建的那一个：它建成只有属主能进的样子。已经在那儿
// 的目录不归它管 —— 把用户自己挑的目录改成 700 是它不该做的事。文件
// 不一样：不管是谁建的，存储层都把它改成 600。
func TestTheFileIsOnlyForItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no such a mode")
	}
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the file's mode is %o, want 600", mode)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := di.Mode().Perm(); mode != 0o700 {
		t.Fatalf("the directory's mode is %o, want 700", mode)
	}
	// WAL 和 SHM 里也是明文：SQLite 照着数据库文件的权限建它们。
	for _, name := range []string{path + "-wal", path + "-shm"} {
		wi, err := os.Stat(name)
		if err != nil {
			continue
		}
		if mode := wi.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s's mode is %o, want 600", filepath.Base(name), mode)
		}
	}
}

// oldSchema is the schema of a file that was made before the
// versions were kept.
const oldSchema = `
CREATE TABLE accounts (
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
CREATE TABLE tokens (
  account_id  TEXT PRIMARY KEY,
  access      TEXT NOT NULL,
  refresh     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  subject     TEXT,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE exhausted_windows (
  account_id TEXT NOT NULL,
  window_key TEXT NOT NULL,
  until      INTEGER NOT NULL,
  reason     TEXT,
  PRIMARY KEY (account_id, window_key)
);
CREATE TABLE sessions (
  session_key TEXT PRIMARY KEY,
  account_id  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE checkins (
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
CREATE TABLE usage_snapshots (
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
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE egress_health (
  url        TEXT PRIMARY KEY,
  ok         INTEGER NOT NULL,
  error      TEXT,
  checked_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkins_account ON checkins(account_id, day DESC);
`

// makeOldFile writes a file in the shape the code used to make.
func makeOldFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO accounts (id,name,user_id,email,status,proxy_group,proxy_url,pending_persist,exhausted_until,last_error,created_at,updated_at)
		  VALUES ('acc_01','old','123','a@example.com','active','warp','',1,NULL,'',?,?)`, []any{now, now}},
		{`INSERT INTO tokens (account_id,access,refresh,expires_at,subject,updated_at) VALUES ('acc_01','a','r',?,NULL,?)`, []any{now, now}},
		{`INSERT INTO checkins (account_id,day,outcome,credit,streak,message,created_at) VALUES ('acc_01','2026-11-13','claimed',NULL,NULL,'',?)`, []any{now}},
		{`INSERT INTO usage_snapshots (account_id,granted,consumed,remaining,created_at) VALUES ('acc_01',600,100,500,?)`, []any{now}},
		{`INSERT INTO sessions (session_key,account_id,updated_at) VALUES ('sess-1','acc_01',?)`, []any{now}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

// TestTheOldFileIsBroughtForward says a file that was made by an
// older version is brought to the shape the code wants, with its
// rows still in it.
//
// 老库要能打开，行还要在。
func TestTheOldFileIsBroughtForward(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}

	// 行还在
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "old" || a.ProxyGroup != "warp" {
		t.Fatalf("got %+v", a)
	}
	tok, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" || tok.Subject != "" {
		t.Fatalf("got %+v", tok)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Credit != 0 {
		t.Fatalf("got %+v", list)
	}
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Remaining != 500 {
		t.Fatalf("got %+v", snaps)
	}
	acc, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || acc != "acc_01" {
		t.Fatalf("got %q %v %v", acc, ok, err)
	}

	// 新列有了
	for _, c := range []struct{ table, column string }{
		{"usage_snapshots", "membership_error"},
		{"usage_snapshots", "windows_error"},
		{"sessions", "upstream_id"},
	} {
		cols, err := columnsOf(ctx, s.db, c.table)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cols[c.column]; !ok {
			t.Fatalf("%s has no column %s", c.table, c.column)
		}
	}

	// 外键在
	err = s.SaveToken(ctx, Token{AccountID: "nope", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if err == nil {
		t.Fatal("a token of an account that is not there was accepted")
	}

	// 自增的号接着走
	if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: "2026-11-14",
		Outcome: "claimed", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Day == "2026-11-14" && c.ID <= 1 {
			t.Fatalf("the new check-in has the id %d, want one after the old ones", c.ID)
		}
	}
}

// TestMigrateIsIdempotent says the migration can be done twice, and
// the second time changes nothing.
//
// 迁移做两遍也没关系。
func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	if _, err := s.GetAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
}

// TestTheNameIsWrittenOnItsOwn says the name setter writes the name
// and nothing else.
//
// 改名只写名字，别的列不碰。
func TestTheNameIsWrittenOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	// 状态那一列一被写就报错，所以改名字的时候它一定没被写。
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER no_status_write BEFORE UPDATE OF status ON accounts
		BEGIN SELECT RAISE(ABORT, 'the status was written'); END`); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAccountName(ctx, "acc_01", "renamed", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "renamed" {
		t.Fatalf("the name is %q", a.Name)
	}
	if a.Status != StatusActive {
		t.Fatalf("the status is %q", a.Status)
	}

	// 没有的账号是找不到，不是改成功。
	err = s.SetAccountName(ctx, "nope", "x", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found", err)
	}
}

// TestTheLastErrorIsWrittenOnItsOwn says the last-error setter writes
// the error and neither the status nor anything else.
//
// 记一条错误只写错误，不改状态。
func TestTheLastErrorIsWrittenOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER no_status_write BEFORE UPDATE OF status ON accounts
		BEGIN SELECT RAISE(ABORT, 'the status was written'); END`); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAccountLastError(ctx, "acc_01", "the site said no", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.LastError != "the site said no" {
		t.Fatalf("the last error is %q", a.LastError)
	}
	if a.Status != StatusActive {
		t.Fatalf("the status is %q", a.Status)
	}

	err = s.SetAccountLastError(ctx, "nope", "x", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found", err)
	}
}

// TestThePlanErrorIsKept says the plan's error is written down with
// the reading, so a plan whose title could not be read is not a plan
// without a title.
//
// 套餐名读不出来，要记下来。
func TestThePlanErrorIsKept(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01",
		Granted:   100, Consumed: 10, Remaining: 90,
		HasAmounts: true,
		PlanError:  "the plan title could not be read",
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d readings", len(snaps))
	}
	if snaps[0].PlanError != "the plan title could not be read" {
		t.Fatalf("the plan error is %q", snaps[0].PlanError)
	}

	// 没有错的读数，读回来也是空的，不是上一条留下的。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01",
		Granted:   100, Consumed: 20, Remaining: 80,
		HasAmounts: true,
		CreatedAt:  time.Now().Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	snaps, err = s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].PlanError != "" {
		t.Fatalf("got %+v", snaps)
	}
}

// TestTheFileAtVersionTwoGetsThePlanErrorColumn says a file that was
// made before the plan error column is brought forward by the third
// step.
//
// 版本二的库要补上这一列，行还在。
func TestTheFileAtVersionTwoGetsThePlanErrorColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")

	// 先做一个新库，再把那一列去掉、把版本写回二，就像上一个版本存下的那样。
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 600, Consumed: 100, Remaining: 500,
		HasAmounts: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE usage_snapshots DROP COLUMN plan_error"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 再打开就该被补上。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	cols, err := columnsOf(ctx, s2.db, "usage_snapshots")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cols["plan_error"]; !ok {
		t.Fatal("usage_snapshots has no column plan_error")
	}
	// 行还在，读数还是那一条。
	snaps, err := s2.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Remaining != 500 {
		t.Fatalf("got %+v", snaps)
	}
	if snaps[0].PlanError != "" {
		t.Fatalf("the plan error is %q", snaps[0].PlanError)
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

// The database's tables.
//
// A table is given as a name and a body, so that the migration can
// both make the table and tell whether the one in the file is the one
// it wants.
//
// The bodies are the shape a file ends up with. A file that was made
// by an older version is brought to this shape by the steps in
// store.go, so that a fresh file and an old one are the same.
//
// The children say which account they belong to, and the account is
// taken with them when it goes: the database does the cascade, so
// there is no list of children anywhere that can fall out of step
// with the schema.
type table struct {
	// name is the table's name.
	name string
	// body is the table: its columns, its constraints and the
	// accounts it belongs to.
	body string
	// copy says how the columns of the new table are filled from
	// the old one.
	copy []copyColumn
}

// copyColumn says how one column of a new table is filled from the
// old one.
type copyColumn struct {
	// name is the column's name.
	name string
	// notNull says the new column cannot be NULL, so a NULL in the
	// old one is replaced by the default.
	notNull bool
	// def is the value a column the old table does not have gets.
	def string
}

// notNull is a column that cannot be NULL.
func notNull(name, def string) copyColumn {
	return copyColumn{name: name, notNull: true, def: def}
}

// nullable is a column that may be NULL.
func nullable(name string) copyColumn {
	return copyColumn{name: name, def: "NULL"}
}

// The tables, in the order they are made.
//
// The accounts come first: the other tables point at them.
var tables = []table{
	{
		name: "accounts",
		body: `(
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  user_id      TEXT,
  email        TEXT,
  status       TEXT NOT NULL CHECK (status IN ('active','signed_out','error')),
  proxy_group  TEXT,
  proxy_url    TEXT,
  last_error   TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("id", "''"),
			notNull("name", "''"),
			nullable("user_id"),
			nullable("email"),
			notNull("status", "'active'"),
			nullable("proxy_group"),
			nullable("proxy_url"),
			nullable("last_error"),
			notNull("created_at", "0"),
			notNull("updated_at", "0"),
		},
	},
	{
		name: "tokens",
		body: `(
  account_id  TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  access      TEXT NOT NULL,
  refresh     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  subject     TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("account_id", "''"),
			notNull("access", "''"),
			notNull("refresh", "''"),
			notNull("expires_at", "0"),
			notNull("subject", "''"),
			notNull("updated_at", "0"),
		},
	},
	{
		name: "exhausted_windows",
		body: `(
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  window_key TEXT NOT NULL,
  until      INTEGER NOT NULL,
  reason     TEXT,
  PRIMARY KEY (account_id, window_key)
)`,
		copy: []copyColumn{
			notNull("account_id", "''"),
			notNull("window_key", "''"),
			notNull("until", "0"),
			nullable("reason"),
		},
	},
	{
		name: "sessions",
		body: `(
  session_key TEXT PRIMARY KEY,
  account_id  TEXT REFERENCES accounts(id) ON DELETE CASCADE,
  upstream_id TEXT UNIQUE,
  updated_at  INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("session_key", "''"),
			nullable("account_id"),
			nullable("upstream_id"),
			notNull("updated_at", "0"),
		},
	},
	{
		name: "checkins",
		body: `(
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  day        TEXT NOT NULL,
  outcome    TEXT NOT NULL,
  credit     REAL NOT NULL DEFAULT 0,
  streak     INTEGER NOT NULL DEFAULT 0,
  message    TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (account_id, day)
)`,
		copy: []copyColumn{
			notNull("id", "0"),
			notNull("account_id", "''"),
			notNull("day", "''"),
			notNull("outcome", "''"),
			notNull("credit", "0"),
			notNull("streak", "0"),
			nullable("message"),
			notNull("created_at", "0"),
		},
	},
	{
		name: "usage_snapshots",
		body: `(
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  granted     REAL,
  consumed    REAL,
  remaining   REAL,
  expires_at  INTEGER,
  plan        TEXT,
  plan_until  INTEGER,
  windows_json TEXT,
  balance     TEXT,
  membership_error TEXT,
  windows_error TEXT,
  plan_error TEXT,
  created_at  INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("id", "0"),
			notNull("account_id", "''"),
			nullable("granted"),
			nullable("consumed"),
			nullable("remaining"),
			nullable("expires_at"),
			nullable("plan"),
			nullable("plan_until"),
			nullable("windows_json"),
			nullable("balance"),
			nullable("membership_error"),
			nullable("windows_error"),
			nullable("plan_error"),
			notNull("created_at", "0"),
		},
	},
	{
		name: "egress_health",
		body: `(
  url        TEXT PRIMARY KEY,
  ok         INTEGER NOT NULL,
  error      TEXT,
  checked_at INTEGER NOT NULL
)`,
		copy: []copyColumn{
			notNull("url", "''"),
			notNull("ok", "0"),
			nullable("error"),
			notNull("checked_at", "0"),
		},
	},
}

// The indexes.
//
// The check-ins need none of their own: the unique (account_id, day)
// is already an index over the account and the day.
var indexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_account ON sessions(account_id)",
	"DROP INDEX IF EXISTS idx_checkins_account",
}

// The children, with the column that says which account they belong
// to.
//
// A row whose account is not there is refused, and the row goes when
// the account goes.
var childTables = []struct {
	name    string
	account string
}{
	{"tokens", "account_id"},
	{"exhausted_windows", "account_id"},
	{"sessions", "account_id"},
	{"checkins", "account_id"},
	{"usage_snapshots", "account_id"},
}
```

创建 `internal/store/store.go`：

```go
// Package store keeps the accounts, their tokens, the check-in history
// and the usage snapshots in one SQLite file.
//
// The file is opened with WAL on and with more than one connection, so
// a reader (the WebUI) and a writer (the scheduler) do not block each
// other. The pragmas are in the data source name, and a pragma is per
// connection, so every connection of the pool has them.
//
// The file holds the accounts' refresh tokens in clear, so the
// directory it is in is made for its owner only, and the file in it
// is made for its owner only as well.
//
// The sessions table grows by one row for every session key that is
// seen. Nothing prunes it: the rows are small, but they are not
// nothing.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// The account states.
const (
	StatusActive    = "active"
	StatusSignedOut = "signed_out"
	StatusError     = "error"
)

// ErrNotFound says the row is not there.
//
// It is the one way of saying so: a caller asks with errors.Is and
// never has to know that the database has a sentinel of its own.
var ErrNotFound = errors.New("store: not found")

// maxConns is how many connections the pool has.
//
// One connection would mean one statement at a time in the whole
// process, and the reader and the writer would take turns. A few
// connections let them work at the same time, which is what the WAL
// journal is for.
const maxConns = 4

// Store is the database.
type Store struct {
	db *sql.DB

	// Clock is the store's clock.
	//
	// It is there so that a test can say what time it is. A store
	// with no clock uses the wall clock.
	Clock func() time.Time
}

// now gives the store's time.
func (s *Store) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// stamp gives the time a row is written at.
//
// A caller that does not say when it is writing gets the store's
// clock. A row with no time in it says 1970, and 1970 is not a time
// anybody asked for.
func (s *Store) stamp(t time.Time) time.Time {
	if t.IsZero() {
		return s.now()
	}
	return t
}

// Account is one MiniMax account.
type Account struct {
	ID         string
	Name       string
	UserID     string
	Email      string
	Status     string
	ProxyGroup string
	ProxyURL   string
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
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
	ID         int64
	AccountID  string
	Granted    float64
	Consumed   float64
	Remaining  float64
	HasAmounts bool
	ExpiresAt  time.Time
	Plan       string
	PlanUntil  time.Time
	// WindowsJSON is the plan's windows as they were read.
	WindowsJSON string
	Balance     string
	// MembershipError says the membership could not be read.
	//
	// It is kept, because a plan that could not be read is not a plan
	// without a group: the windows are asked for under that group.
	MembershipError string
	// WindowsError says the plan's windows could not be read.
	//
	// It is kept, because a plan whose windows could not be read is
	// not a plan without windows.
	WindowsError string
	// PlanError says the plan's title could not be read.
	//
	// It is kept, because a plan whose title could not be read is not
	// a plan without a title: the field below would say "" and read
	// as "this account has no plan".
	PlanError string
	CreatedAt time.Time
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
//
// The directory and the file are made for their owner only: the file
// holds the refresh tokens of every account in clear. Windows has no
// such a mode, and there the file is left as Windows made it.
//
// A directory that is already there is left as it is: the store does
// not tighten the permissions of a directory the user picked. The file
// itself is always tightened, whoever made it.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	// The database is opened lazily, so a path that cannot be used
	// would only show up at the first query. Open it now: the caller
	// wants to know now.
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := protectFile(path); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Clock: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// dsn gives the data source name of a database file.
//
// The path is made a file: URI, so that a path with a question mark
// in it is the path and not the end of the file name. The pragmas
// come after the question mark; a pragma is per connection, so every
// connection of the pool gets them.
//
// The write lock is taken when a transaction starts and not in the
// middle of it. A transaction that finds the lock taken fails at once
// instead of failing at the end, after it has done its work.
func dsn(path string) string {
	p := filepath.ToSlash(path)
	prefix := "file:"
	if strings.HasPrefix(p, "//") {
		// A path that begins with two slashes is a network path, and
		// the two slashes after "file:" are the empty authority that
		// says so.
		prefix = "file://"
	}
	return prefix + escapePath(p) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
}

// escapePath makes a path safe to put in a file: URI.
//
// Every character that is not an unreserved one is escaped, so that a
// path with a space, a question mark, an ampersand, a hash or a
// percent sign in it is the path and not the URI's syntax. The
// slashes are left: they separate the parts of the path.
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' || unreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

// unreserved says whether a character may stand for itself in a URI.
//
// It is the set of RFC 3986: the letters, the digits, and the four
// marks that have no meaning of their own.
func unreserved(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~':
		return true
	}
	return false
}

// protectFile makes the database file readable by its owner only.
//
// The file holds the refresh tokens of every account in clear, so any
// other user on the machine could take the accounts over. Windows has
// no such a mode, and there the file is left as Windows made it.
func protectFile(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("protect the database file: %w", err)
	}
	return nil
}

// Migrate brings the database to the shape the code expects.
//
// The steps are numbered, and the file says which one it is at with
// PRAGMA user_version. A file that was made before the steps were
// numbered says 0, and the steps below bring it forward. A fresh file
// and an old one end up at the same version, with the same tables.
//
// Every step is one transaction: a step that fails leaves the file as
// it was, and the version is written in that same transaction, so a
// file never says it is at a version it did not reach.
func (s *Store) Migrate(ctx context.Context) error {
	// One connection for the whole migration: the pragmas are per
	// connection, and the steps must all see the same database.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Close()

	// A table is rebuilt by making a new one and copying the rows
	// over, and the old table's rows are still there while the copy
	// runs. The foreign keys would say that those rows belong to an
	// account that is not there, so they are turned off for the
	// steps and turned back on after.
	//
	// The pragmas are put back with a context that is not the caller's:
	// a caller's context that was cancelled would leave the
	// connection without its foreign keys.
	clean := context.WithoutCancel(ctx)
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=off"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.ExecContext(clean, "PRAGMA foreign_keys=on")

	// The old table is renamed out of the way before the new one
	// takes its name, and a rename writes the new name into the
	// tables that point at it. The old way of renaming is asked for
	// here, so that the other tables' names are left alone.
	if _, err := conn.ExecContext(ctx, "PRAGMA legacy_alter_table=on"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.ExecContext(clean, "PRAGMA legacy_alter_table=off")

	version, err := userVersion(ctx, conn)
	if err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("migrate: the database is at version %d and this program knows %d",
			version, schemaVersion)
	}
	for _, step := range migrations[version:] {
		if err := step(ctx, conn); err != nil {
			return err
		}
	}
	return nil
}

// schemaVersion is the version a file is at when the steps are done.
const schemaVersion = 3

// migrations are the steps, in order.
//
// The step at index n brings a file at version n to version n+1.
var migrations = []func(ctx context.Context, conn *sql.Conn) error{
	migrateMake,
	migrateConform,
	migratePlanError,
}

// migrateMake makes the tables that are not there.
//
// A file that was made by an older version has most of them already,
// and they are left as they are: the next step says what shape they
// should be.
func migrateMake(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 1, func(tx *sql.Tx) error {
		for _, t := range tables {
			if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+t.name+t.body); err != nil {
				return fmt.Errorf("migrate: make the table %s: %w", t.name, err)
			}
		}
		return nil
	})
}

// migrateConform gives the tables the shape the code wants.
//
// A table that is not the one the code has is made again: the rows
// are copied into a new table and the old one is taken away. A file
// that was made by an older version is brought to this shape here.
func migrateConform(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 2, func(tx *sql.Tx) error {
		for _, t := range tables {
			got, err := storedSQL(ctx, tx, t.name)
			if err != nil {
				return err
			}
			if got == "" {
				// The table is not there at all.
				if _, err := tx.ExecContext(ctx, "CREATE TABLE "+t.name+t.body); err != nil {
					return fmt.Errorf("migrate: make the table %s: %w", t.name, err)
				}
				continue
			}
			if sameSQL(got, "CREATE TABLE "+t.name+t.body) {
				continue
			}
			if err := rebuildTable(ctx, tx, t); err != nil {
				return err
			}
		}
		for _, q := range indexStatements {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
		}
		// A row whose account is not there is not allowed any more,
		// so the ones that are there are taken away.
		for _, c := range childTables {
			q := fmt.Sprintf("DELETE FROM %s WHERE %s NOT IN (SELECT id FROM accounts)",
				c.name, c.account)
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate: take away the rows of %s without an account: %w",
					c.name, err)
			}
		}
		return nil
	})
}

// migratePlanError gives the usage readings the plan error column.
//
// The column was added after the table was first made, so a file that
// was made before it does not have it. The table is rebuilt when the
// column is not there, and left alone when it is: a file that already
// has it must not have its readings copied about for nothing.
func migratePlanError(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 3, func(tx *sql.Tx) error {
		cols, err := columnsOf(ctx, tx, "usage_snapshots")
		if err != nil {
			return err
		}
		if _, ok := cols["plan_error"]; ok {
			return nil
		}
		for _, t := range tables {
			if t.name == "usage_snapshots" {
				return rebuildTable(ctx, tx, t)
			}
		}
		return nil
	})
}

// runStep does one step, in one transaction.
//
// The version is written at the end, in the same transaction, so that
// a file never says it is at a version it did not reach.
func runStep(ctx context.Context, conn *sql.Conn, version int, fn func(tx *sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		tx.Rollback()
		return fmt.Errorf("migrate: write the version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// rebuildTable makes the table again, with the rows of the old one.
//
// The rows are copied into a new table of the wanted shape, and the
// old table is taken away. A column the old table does not have gets
// its default, and a column that could be NULL and cannot any more
// gets the default instead of the NULL.
func rebuildTable(ctx context.Context, tx *sql.Tx, t table) error {
	exprs, err := copyExprs(ctx, tx, t)
	if err != nil {
		return err
	}
	cols := make([]string, 0, len(t.copy))
	for _, c := range t.copy {
		cols = append(cols, c.name)
	}
	old := t.name + "_old"
	for _, q := range []string{
		"CREATE TABLE " + t.name + "_new" + t.body,
		"INSERT INTO " + t.name + "_new (" + strings.Join(cols, ", ") + ") SELECT " +
			strings.Join(exprs, ", ") + " FROM " + t.name,
		"ALTER TABLE " + t.name + " RENAME TO " + old,
		"ALTER TABLE " + t.name + "_new RENAME TO " + t.name,
		"DROP TABLE " + old,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: rebuild the table %s: %w", t.name, err)
		}
	}
	return nil
}

// copyExprs gives the expressions the rows of the old table are read
// with.
func copyExprs(ctx context.Context, tx *sql.Tx, t table) ([]string, error) {
	old, err := columnsOf(ctx, tx, t.name)
	if err != nil {
		return nil, err
	}
	exprs := make([]string, 0, len(t.copy))
	for _, c := range t.copy {
		prev, ok := old[c.name]
		switch {
		case !ok:
			// The old table does not have the column.
			exprs = append(exprs, c.def)
		case !c.notNull:
			// The new column may be NULL, so a NULL is kept: a
			// reading that could not be made is not a reading of
			// zero.
			exprs = append(exprs, c.name)
		case prev.notNull:
			exprs = append(exprs, c.name)
		default:
			exprs = append(exprs, fmt.Sprintf("COALESCE(%s, %s)", c.name, c.def))
		}
	}
	return exprs, nil
}

// columnInfo says what is known about one column.
type columnInfo struct {
	// notNull says the column cannot be NULL.
	notNull bool
}

// querier is what can ask the database a question.
//
// The migration asks inside a transaction and the tests ask outside of
// one, so both have to do.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// columnsOf gives the columns of a table.
func columnsOf(ctx context.Context, q querier, name string) (map[string]columnInfo, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+name+")")
	if err != nil {
		return nil, fmt.Errorf("migrate: read the columns of %s: %w", name, err)
	}
	defer rows.Close()
	out := map[string]columnInfo{}
	for rows.Next() {
		var (
			cid     int
			colName string
			colType string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("migrate: read the columns of %s: %w", name, err)
		}
		out[colName] = columnInfo{notNull: notNull != 0}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read the columns of %s: %w", name, err)
	}
	return out, nil
}

// storedSQL gives the statement the table in the file was made with.
//
// It is empty when the table is not there.
func storedSQL(ctx context.Context, q querier, name string) (string, error) {
	var sqlText string
	err := q.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&sqlText)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("migrate: read the table %s: %w", name, err)
	}
	return sqlText, nil
}

// sameSQL says whether two statements are the same table.
//
// The statement in the file is not the one the code has: SQLite
// writes the table's name in quotes when the table is renamed, and
// the spacing is whatever the file was made with. So the two are
// compared with the whitespace put to one space and the quotes taken
// away.
func sameSQL(got, want string) bool {
	return normalizeSQL(got) == normalizeSQL(want)
}

// normalizeSQL puts a statement into the one shape two statements are
// compared in.
func normalizeSQL(s string) string {
	s = strings.ReplaceAll(s, `"`, "")
	return strings.Join(strings.Fields(s), " ")
}

// userVersion gives the version the file is at.
//
// It is 0 in a file that was made before the versions were kept.
func userVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var v int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("migrate: read the version: %w", err)
	}
	return v, nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromMS gives the time a millisecond count says.
//
// The time is in UTC: everything that is written in is in UTC, and a
// time that comes back in the machine's own zone is a time that says
// two different things depending on who reads it.
func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// CreateAccount inserts an account.
func (s *Store) CreateAccount(ctx context.Context, a Account) error {
	if err := checkStatus(a.Status); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (id, name, user_id, email, status, proxy_group, proxy_url,
			last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
		a.LastError, ms(s.stamp(a.CreatedAt)), ms(s.stamp(a.UpdatedAt)))
	if err != nil {
		return fmt.Errorf("create account: %w", err)
	}
	return nil
}

// UpdateAccount writes an account's fields.
//
// An account that is not there is an error, not a quiet success: the
// caller believes it wrote something.
//
// It writes every field, so a caller that wants to change one of them
// has to read the account first. The setters below change one field
// without that, and are the ones to use.
func (s *Store) UpdateAccount(ctx context.Context, a Account) error {
	if err := checkStatus(a.Status); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET name=?, user_id=?, email=?, status=?, proxy_group=?, proxy_url=?,
			last_error=?, updated_at=?
		WHERE id=?`,
		a.Name, a.UserID, a.Email, a.Status, a.ProxyGroup, a.ProxyURL,
		a.LastError, ms(s.stamp(a.UpdatedAt)), a.ID)
	if err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update account %s: %w", a.ID, ErrNotFound)
	}
	return nil
}

// UpdateAccountStatus writes an account's state and its last error.
//
// It is narrower than UpdateAccount on purpose: the caller does not
// have to read the account first, and a change that touches the state
// cannot lose a change that touched something else.
func (s *Store) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	if err := checkStatus(status); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET status=?, last_error=?, updated_at=? WHERE id=?`,
		status, lastError, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("update the status of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update the status of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetAccountProxy writes an account's outlet.
//
// It is narrow like UpdateAccountStatus: the group and the address
// are the only things it writes, so a caller that changes the outlet
// does not have to read the account first.
func (s *Store) SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET proxy_group=?, proxy_url=?, updated_at=? WHERE id=?`,
		group, url, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the outlet of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the outlet of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetAccountName writes the account's name.
//
// It is narrow like SetAccountProxy: the name is the only thing it
// writes, so a caller that renames an account does not have to read
// the account first, and cannot lose a change another writer made.
func (s *Store) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET name=?, updated_at=? WHERE id=?`,
		name, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the name of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the name of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetAccountLastError writes the account's last error.
//
// It writes the error and nothing else. It does not write the status
// either: an account that is healthy and an account whose last reading
// failed are two different things, and writing the status here would
// mean reading it first and writing back a value that could be stale.
func (s *Store) SetAccountLastError(ctx context.Context, id, lastError string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET last_error=?, updated_at=? WHERE id=?`,
		lastError, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the last error of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the last error of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// checkStatus says whether a status is one of ours.
//
// The rotator looks for the active accounts, so an account with a
// status nobody wrote is an account that is quietly never used.
func checkStatus(status string) error {
	switch status {
	case StatusActive, StatusSignedOut, StatusError:
		return nil
	}
	return fmt.Errorf("unknown account status %q: it is %q, %q or %q",
		status, StatusActive, StatusSignedOut, StatusError)
}

// DeleteAccount removes an account and everything under it.
//
// The children are taken away by the database: the tables say what
// belongs to an account, and ON DELETE CASCADE takes it away with the
// account. So there is no list of children here that can fall out of
// step with the schema.
//
// An account that is not there is an error: the caller believes
// something was removed.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM accounts WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete account %s: %w", id, ErrNotFound)
	}
	return nil
}

// ListAccounts gives every account, oldest first.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
			last_error, created_at, updated_at
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, nil
}

// GetAccount gives one account.
//
// An account that is not there is ErrNotFound.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, user_id, email, status, proxy_group, proxy_url,
			last_error, created_at, updated_at
		FROM accounts WHERE id=?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("get account %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account %s: %w", id, err)
	}
	return a, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanAccount(sc scanner) (Account, error) {
	var (
		a                     Account
		userID, email, pg, pu sql.NullString
		lastErr               sql.NullString
		createdAt, updatedAt  int64
	)
	if err := sc.Scan(&a.ID, &a.Name, &userID, &email, &a.Status, &pg, &pu,
		&lastErr, &createdAt, &updatedAt); err != nil {
		return Account{}, err
	}
	a.UserID = userID.String
	a.Email = email.String
	a.ProxyGroup = pg.String
	a.ProxyURL = pu.String
	a.LastError = lastErr.String
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
		t.AccountID, t.Access, t.Refresh, ms(t.ExpiresAt), t.Subject, ms(s.stamp(t.UpdatedAt)))
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

// GetToken gives an account's token.
//
// A token that is not there is ErrNotFound.
func (s *Store) GetToken(ctx context.Context, accountID string) (Token, error) {
	var (
		t         Token
		expiresAt int64
		updatedAt int64
		subject   sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT account_id, access, refresh, expires_at, subject, updated_at
		FROM tokens WHERE account_id=?`, accountID).
		Scan(&t.AccountID, &t.Access, &t.Refresh, &expiresAt, &subject, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, fmt.Errorf("get token %s: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return Token{}, fmt.Errorf("get token %s: %w", accountID, err)
	}
	t.Subject = subject.String
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
		c.AccountID, c.Day, c.Outcome, c.Credit, c.Streak, c.Message, ms(s.stamp(c.CreatedAt)))
	if err != nil {
		return fmt.Errorf("save checkin: %w", err)
	}
	return nil
}

// ListCheckins gives an account's check-ins, newest first.
//
// A limit of zero or less means every check-in: a limit is a limit,
// and a limit of zero is no limit at all.
func (s *Store) ListCheckins(ctx context.Context, accountID string, limit int) ([]Checkin, error) {
	q := `
		SELECT id, account_id, day, outcome, credit, streak, message, created_at
		FROM checkins WHERE account_id=? ORDER BY day DESC`
	args := []any{accountID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list checkins: %w", err)
	}
	defer rows.Close()
	var out []Checkin
	for rows.Next() {
		var (
			c         Checkin
			credit    sql.NullFloat64
			streak    sql.NullInt64
			message   sql.NullString
			createdAt int64
		)
		if err := rows.Scan(&c.ID, &c.AccountID, &c.Day, &c.Outcome, &credit, &streak, &message, &createdAt); err != nil {
			return nil, fmt.Errorf("list checkins: %w", err)
		}
		c.Credit = credit.Float64
		c.Streak = int(streak.Int64)
		c.Message = message.String
		c.CreatedAt = fromMS(createdAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list checkins: %w", err)
	}
	return out, nil
}

// SaveUsageSnapshot writes a usage reading.
//
// A reading that could not be made is not a reading of zero. Without
// the amounts the three numbers are NULL, so the reader can tell "no
// idea" from "nothing left" -- and an account with no idea is not an
// account with nothing left.
//
// The errors of the parts that could not be read are written down with
// it: a plan whose windows could not be read is not a plan without
// windows, and the reader has to be able to tell them apart.
//
// The amounts and the flag that says there are amounts are two ways
// of saying one thing, and a caller that forgets the flag would lose
// the numbers without a word. So the flag is worked out from the
// numbers: a reading with a number in it has amounts.
func (s *Store) SaveUsageSnapshot(ctx context.Context, u UsageSnapshot) error {
	has := u.HasAmounts
	if !has && (u.Granted != 0 || u.Consumed != 0 || u.Remaining != 0) {
		// There are numbers and nobody said so. The numbers are the
		// truth here: throwing them away would lose them without a
		// word.
		has = true
	}
	var granted, consumed, remaining any
	if has {
		granted, consumed, remaining = u.Granted, u.Consumed, u.Remaining
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_snapshots (account_id, granted, consumed, remaining,
			expires_at, plan, plan_until, windows_json, balance,
			membership_error, windows_error, plan_error, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.AccountID, granted, consumed, remaining, ms(u.ExpiresAt),
		u.Plan, ms(u.PlanUntil), u.WindowsJSON, u.Balance,
		u.MembershipError, u.WindowsError, u.PlanError, ms(s.stamp(u.CreatedAt)))
	if err != nil {
		return fmt.Errorf("save usage snapshot: %w", err)
	}
	return nil
}

// usageSnapshotColumns gives the columns of a usage snapshot, in the
// order scanUsageSnapshot reads them.
//
// The prefix names the table the columns come from: a join needs it,
// or the column names are ambiguous.
func usageSnapshotColumns(prefix string) string {
	cols := []string{
		"id", "account_id", "granted", "consumed", "remaining", "expires_at",
		"plan", "plan_until", "windows_json", "balance",
		"membership_error", "windows_error", "plan_error", "created_at",
	}
	if prefix != "" {
		for i := range cols {
			cols[i] = prefix + "." + cols[i]
		}
	}
	return strings.Join(cols, ", ")
}

// LatestUsageSnapshots gives each account's newest usage reading.
//
// The newest is the row with the greatest id. Two readings of one
// account can share a millisecond, and the timestamp alone would then
// name two rows -- and both would be summed, so the account's credits
// would be counted twice. The id grows with every row written, so it
// tells the readings apart, and there is one row per account, always.
func (s *Store) LatestUsageSnapshots(ctx context.Context) ([]UsageSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+usageSnapshotColumns("s")+`
		FROM usage_snapshots s
		JOIN (SELECT account_id, MAX(id) AS mid FROM usage_snapshots GROUP BY account_id)
			m ON m.account_id = s.account_id AND m.mid = s.id
		ORDER BY s.account_id`)
	if err != nil {
		return nil, fmt.Errorf("latest usage snapshots: %w", err)
	}
	defer rows.Close()
	var out []UsageSnapshot
	for rows.Next() {
		u, err := scanUsageSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("latest usage snapshots: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest usage snapshots: %w", err)
	}
	return out, nil
}

// LatestUsageSnapshot gives one account's newest usage reading.
//
// It is the row with the greatest id, for the same reason as in
// LatestUsageSnapshots: two readings of one account can share a
// millisecond.
//
// An account that was never read has no reading and no error.
func (s *Store) LatestUsageSnapshot(ctx context.Context, accountID string) (UsageSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+usageSnapshotColumns("")+`
		FROM usage_snapshots WHERE account_id=? ORDER BY id DESC LIMIT 1`, accountID)
	u, err := scanUsageSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UsageSnapshot{}, nil
	}
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("latest usage snapshot %s: %w", accountID, err)
	}
	return u, nil
}

// scanUsageSnapshot reads one usage snapshot.
func scanUsageSnapshot(sc scanner) (UsageSnapshot, error) {
	var (
		u                            UsageSnapshot
		granted, consumed, remaining sql.NullFloat64
		expiresAt                    sql.NullInt64
		plan, windowsJSON, balance   sql.NullString
		membershipErr, windowsErr    sql.NullString
		planErr                      sql.NullString
		planUntil                    sql.NullInt64
		createdAt                    int64
	)
	if err := sc.Scan(&u.ID, &u.AccountID, &granted, &consumed, &remaining, &expiresAt,
		&plan, &planUntil, &windowsJSON, &balance, &membershipErr, &windowsErr,
		&planErr, &createdAt); err != nil {
		return UsageSnapshot{}, err
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
	u.MembershipError = membershipErr.String
	u.WindowsError = windowsErr.String
	u.PlanError = planErr.String
	u.CreatedAt = fromMS(createdAt)
	return u, nil
}

// PruneUsageSnapshots throws away the readings that are not needed.
//
// The readings that are older than olderThan are thrown away, but the
// newest keepPerAccount readings of every account are kept whatever
// their age: the newest reading is the account's reading, and it is
// what the totals are made of.
//
// A keepPerAccount of zero or less keeps nothing: every reading that
// is older than olderThan goes.
func (s *Store) PruneUsageSnapshots(ctx context.Context, olderThan time.Time, keepPerAccount int) error {
	if keepPerAccount < 0 {
		keepPerAccount = 0
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM usage_snapshots WHERE id IN (
			SELECT id FROM (
				SELECT id, created_at,
					ROW_NUMBER() OVER (PARTITION BY account_id ORDER BY id DESC) AS rn
				FROM usage_snapshots
			) WHERE rn > ? AND created_at < ?)`,
		keepPerAccount, ms(olderThan))
	if err != nil {
		return fmt.Errorf("prune the usage snapshots: %w", err)
	}
	return nil
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
//
// The windows that have come back are in here as well: a row is there
// until it is pruned, and the row says when the window comes back.
// The reader decides which of them still count.
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
			e      Exhausted
			until  int64
			reason sql.NullString
		)
		if err := rows.Scan(&e.AccountID, &e.WindowKey, &until, &reason); err != nil {
			return nil, fmt.Errorf("list exhausted: %w", err)
		}
		e.Until = fromMS(until)
		e.Reason = reason.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list exhausted: %w", err)
	}
	return out, nil
}

// PruneExpiredWindows throws away the windows that have come back.
//
// A window that has run out is not a window that is spent, and it is
// of no use to anybody.
func (s *Store) PruneExpiredWindows(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM exhausted_windows WHERE until <= ?", ms(now))
	if err != nil {
		return fmt.Errorf("prune the expired windows: %w", err)
	}
	return nil
}

// BindSession points a session at an account.
//
// The session's upstream id is left alone: it belongs to the
// conversation and not to the account it is bound to.
func (s *Store) BindSession(ctx context.Context, sessionKey, accountID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_key, account_id, updated_at) VALUES (?,?,?)
		ON CONFLICT(session_key) DO UPDATE SET
			account_id=excluded.account_id, updated_at=excluded.updated_at`,
		sessionKey, accountID, ms(s.now()))
	if err != nil {
		return fmt.Errorf("bind session: %w", err)
	}
	return nil
}

// SessionAccount gives the account a session is bound to.
//
// A session that was never bound has no account and no error.
func (s *Store) SessionAccount(ctx context.Context, sessionKey string) (string, bool, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT account_id FROM sessions WHERE session_key=?", sessionKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("session account: %w", err)
	}
	if !id.Valid || id.String == "" {
		return "", false, nil
	}
	return id.String, true, nil
}

// UpstreamID gives the session's upstream id, making one if the
// session has none.
//
// The id is random: it is not derived from the session key, so the
// upstream never sees a hash of the words in the request. It is also
// kept: the same session has the same upstream id every time it is
// asked for.
//
// The key is what decides which id a conversation gets, and two
// conversations that arrive under the same key are one conversation as
// far as this store can tell. Telling them apart is the caller's job:
// it has to give them different keys.
//
// The session does not have to be bound to an account yet: a
// conversation has an upstream id before it has an account.
func (s *Store) UpstreamID(ctx context.Context, key string) (string, error) {
	id, ok, err := s.upstreamIDOf(ctx, key)
	if err != nil {
		return "", err
	}
	if ok {
		if id != "" {
			return id, nil
		}
		// The session is there but has no id yet: make one and write
		// it down.
		newID, err := randomID()
		if err != nil {
			return "", err
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET upstream_id=?
			WHERE session_key=? AND (upstream_id IS NULL OR upstream_id='')`, newID, key)
		if err != nil {
			return "", fmt.Errorf("set the upstream id of the session %s: %w", key, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return newID, nil
		}
		// Somebody else wrote one first, and theirs is the one.
		id, _, err := s.upstreamIDOf(ctx, key)
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", fmt.Errorf("the session %s has no upstream id", key)
		}
		return id, nil
	}
	// The session is not there yet: make it with an id of its own.
	newID, err := randomID()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_key, account_id, upstream_id, updated_at)
		VALUES (?, NULL, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET
			upstream_id=COALESCE(sessions.upstream_id, excluded.upstream_id)`,
		key, newID, ms(s.now()))
	if err != nil {
		return "", fmt.Errorf("make the session %s: %w", key, err)
	}
	id, _, err = s.upstreamIDOf(ctx, key)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("the session %s has no upstream id", key)
	}
	return id, nil
}

// upstreamIDOf gives the upstream id of a session.
//
// The second value says whether the session is there at all.
func (s *Store) upstreamIDOf(ctx context.Context, key string) (string, bool, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT upstream_id FROM sessions WHERE session_key=?", key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the upstream id of the session %s: %w", key, err)
	}
	return id.String, true, nil
}

// randomID gives an id that is not like any other.
//
// The id is random and not made from the session's key: two
// conversations that begin with the same words must not end up with
// the same upstream id.
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("make an id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// SetEgressHealth writes a proxy's last check.
func (s *Store) SetEgressHealth(ctx context.Context, url string, ok bool, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO egress_health (url, ok, error, checked_at) VALUES (?,?,?,?)
		ON CONFLICT(url) DO UPDATE SET
			ok=excluded.ok, error=excluded.error, checked_at=excluded.checked_at`,
		url, boolInt(ok), errMsg, ms(s.now()))
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
			return nil, fmt.Errorf("egress health: %w", err)
		}
		h.OK = ok != 0
		h.Error = errMsg.String
		h.CheckedAt = fromMS(checkedAt)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("egress health: %w", err)
	}
	return out, nil
}

// checkinZone is the timezone a check-in day is counted in.
//
// The site counts its days in China, so a day here is that zone's day.
// The zone is loaded by name, so the name is never ambiguous: "CST" is
// China Standard Time here and US Central somewhere else. A machine
// without the timezone database gets the same offset under a name that
// says what it is.
var checkinZone = loadCheckinZone()

// loadCheckinZone gives the timezone a check-in day is counted in.
func loadCheckinZone() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("UTC+8", 8*3600)
}

// DayKey is the day a check-in is counted for.
//
// The day is the site's day, so that the day rolls over at the same
// hour for every account, whatever the machine's own timezone is.
func DayKey(t time.Time) string {
	return t.In(checkinZone).Format("2006-01-02")
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

`go get` 会把 `go.mod` 的 go 指令顶到 `1.26.0` 并写上一行 `toolchain` —— sqlite v1.62.0 要求
`go >= 1.26.0`。这是预期的，别改回去。

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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
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
	// 另一个服务器一个请求都没有收到：跟随了就收到了。
	if got := hits.Load(); got != 0 {
		t.Fatalf("the other server got %d requests, want 0", got)
	}
}

func TestRefusesSchemeDowngrade(t *testing.T) {
	// https 转到同一个 host 的 http：Authorization 就会明文发出去。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "http://"+r.Host+"/x")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// The transport speaks plaintext whatever the scheme says, so a
	// downgrade can be tried without a certificate.
	base := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	c := New(Options{Timeout: 5 * time.Second, Transport: &plainTransport{base: base}})
	_, err := c.Get(context.Background(), "https://"+srv.Listener.Addr().String()+"/start")
	if err == nil {
		t.Fatal("expected an error")
	}
	var de *SchemeDowngradeError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v (%T), want SchemeDowngradeError", err, err)
	}
	if de.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d", de.Status)
	}
	if !strings.Contains(de.Error(), "http") {
		t.Fatalf("the error does not say the scheme was downgraded: %v", de)
	}
	// 只收到了第一个请求：降级后的地址没有被访问过。
	if got := hits.Load(); got != 1 {
		t.Fatalf("the server got %d requests, want 1", got)
	}
}

// plainTransport talks plaintext whatever the scheme of the request
// says.
type plainTransport struct {
	base *http.Transport
}

func (t *plainTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL.Scheme = "http"
	return t.base.RoundTrip(out)
}

func TestRefusesRedirectThatWouldReplayAStaleSignature(t *testing.T) {
	// 签名是按 path+query 算的。改了 path 还把旧的签名带过去，
	// 上游会拒签，那看起来就是一个死 token，白白花掉一个刷新令牌。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/other")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("yy", "deadbeef")
	req.Header.Set("x-signature", "cafebabe")
	_, err = c.Do(req)
	if err == nil {
		t.Fatal("expected an error")
	}
	var se *StaleSignatureError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want StaleSignatureError", err, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("the server got %d requests, want 1", got)
	}
}

func TestFollowsRedirectThatKeepsThePathAndTheSignature(t *testing.T) {
	// path 和 query 都没变，签名还是对的，这种重定向照跟。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if hits.Load() == 1 {
			w.Header().Set("Location", "/start?x=1")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("yy", "deadbeef")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Fatalf("the server got %d requests, want 2", got)
	}
}

func TestRefusesRedirectThatChangesTheQueryOfASignedRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if hits.Load() == 1 {
			w.Header().Set("Location", "/start?other=1")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-signature", "cafebabe")
	_, err = c.Do(req)
	if err == nil {
		t.Fatal("expected an error")
	}
	var se *StaleSignatureError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want StaleSignatureError", err, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("the server got %d requests, want 1", got)
	}
}

func TestTooManyRedirectsIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/loop")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Get(context.Background(), srv.URL+"/loop")
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v", err)
	}
}

func TestDoDoesNotTouchTheCallersRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})

	// 一个没有 header 的请求不能 panic。
	bare, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	bare.Header = nil
	resp, err := c.Do(bare)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 调用方的 header map 里不能被塞进 User-Agent。
	var got string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write([]byte("ok"))
	}))
	defer srv2.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv2.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "MiniMaxCode" {
		t.Fatalf("the server saw the user-agent %q", got)
	}
	if req.Header.Get("User-Agent") != "" {
		t.Fatalf("the caller's request was changed: %q", req.Header.Get("User-Agent"))
	}
}

func TestReadBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadBody(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("body = %q", b)
	}
}

func TestReadBodyWithNoResponse(t *testing.T) {
	if _, err := ReadBody(nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestReadBodyRefusesAVeryLongBody(t *testing.T) {
	// 一个不停往里写的上游不能把内存吃光。
	body := io.NopCloser(io.LimitReader(filler{}, maxBody+1024))
	resp := &http.Response{Body: body}
	if _, err := ReadBody(resp); err == nil {
		t.Fatal("expected an error")
	}
}

// filler is a reader that never ends.
type filler struct{}

func (filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestTheDefaultTransportIsTunedForManyAccounts(t *testing.T) {
	tr := defaultTransport()
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("http/2 is not attempted")
	}
	if tr.MaxIdleConnsPerHost != 32 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 32", tr.MaxIdleConnsPerHost)
	}
}

func TestCheckEnvelopeBusinessError(t *testing.T) {
	// HTTP 200 but the body says it failed. The message in the body is
	// not the one the 1000048 case makes up, so the two can be told
	// apart.
	raw := []byte(`{"statusInfo":{"code":1000048,"message":"account not found"}}`)
	err := CheckEnvelope("check-in", raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrSignInRefused) {
		t.Fatalf("err = %v, want ErrSignInRefused", err)
	}
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v (%T), want *BusinessError", err, err)
	}
	if be.Code != 1000048 {
		t.Fatalf("code = %d", be.Code)
	}
	if be.What != "check-in" {
		t.Fatalf("what = %q", be.What)
	}
	// The wording is the one the code has always produced.
	if err.Error() != "check-in: the sign-in was refused" {
		t.Fatalf("err = %q", err.Error())
	}
}

func TestCheckEnvelopeBusinessErrorCarriesTheCode(t *testing.T) {
	raw := []byte(`{"statusInfo":{"code":1001,"message":"something went wrong"}}`)
	err := CheckEnvelope("credits", raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v (%T), want *BusinessError", err, err)
	}
	if be.Code != 1001 {
		t.Fatalf("code = %d", be.Code)
	}
	if be.Message != "something went wrong" {
		t.Fatalf("message = %q", be.Message)
	}
	if err.Error() != "credits: something went wrong" {
		t.Fatalf("err = %q", err.Error())
	}
	// 不是签到被拒，就不是 ErrSignInRefused。
	if errors.Is(err, ErrSignInRefused) {
		t.Fatalf("err = %v, want not ErrSignInRefused", err)
	}
}

func TestCheckEnvelopeBaseResp(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":1001,"status_msg":"nope"}}`)
	err := CheckEnvelope("credits", raw)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v (%T), want *BusinessError", err, err)
	}
	if be.Code != 1001 {
		t.Fatalf("code = %d", be.Code)
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
		`{"error":{"message":"insufficient balance"}}`:   true,
		`{"message":"额度不足"}`:                             true,
		`{"message":"积分已用完"}`:                            true,
		`{"message":"model not available"}`:              false,
		`{"message":"quota exceeded"}`:                   true,
		`{"message":"rate limit exceeded"}`:              false,
		`{"message":"too many requests"}`:                false,
		`{"message":"rate-limited"}`:                     false,
		`{"message":"rate limited by upstream"}`:         false,
		`{"message":"rate_limit exceeded"}`:              false,
		`{"message":"throttled"}`:                        false,
		`{"message":"request throttled by the gateway"}`: false,
	}
	for body, want := range cases {
		if got := IsQuotaRefusal([]byte(body)); got != want {
			t.Errorf("IsQuotaRefusal(%s) = %v, want %v", body, got, want)
		}
	}
}
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
	"errors"
	"fmt"
	"regexp"
	"strings"
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

// SchemeDowngradeError is a redirect that took the request from https
// to http.
//
// The host may well be the same one, and that is exactly what makes
// it dangerous: the Authorization header would go out in cleartext, and
// whoever is on the way can read it and replay it. A redirect that
// downgrades the scheme is refused, never followed.
type SchemeDowngradeError struct {
	// From is the address the request was sent to.
	From string
	// To is the address it was sent on to.
	To string
	// Status is the redirect's status code.
	Status int
}

// Error is the error's text.
func (e *SchemeDowngradeError) Error() string {
	return fmt.Sprintf("%s sent the request on to %s (HTTP %d), which downgrades the scheme to http. The sign-in would go out in cleartext, so the redirect is refused.", e.From, e.To, e.Status)
}

// StaleSignatureError is a redirect that would have replayed a
// signature made for another path or query.
//
// MiniMax signs each account API request: "yy" is computed over the
// path and the query, so a signature made for one path is not a
// signature for another. Replaying the old one is refused by the
// upstream, and that refusal reads as a dead token — a false one that
// costs a refresh token. The redirect is refused instead.
type StaleSignatureError struct {
	// From is the address the request was sent to.
	From string
	// To is the address it was sent on to.
	To string
	// Status is the redirect's status code.
	Status int
}

// Error is the error's text.
func (e *StaleSignatureError) Error() string {
	return fmt.Sprintf("%s sent the request on to %s (HTTP %d), which changes the path or the query. The signature is made over the path and the query, so it cannot be replayed; the redirect is refused.", e.From, e.To, e.Status)
}

// signInRefusedCode is the business code MiniMax answers with when it
// refuses the sign-in.
const signInRefusedCode = 1000048

// ErrSignInRefused says the upstream refused the sign-in.
//
// It is what errors.Is is used with to tell a stale token from any
// other business error.
var ErrSignInRefused = errors.New("the sign-in was refused")

// BusinessError is a 200 answer whose body says it failed.
//
// The body of a successful HTTP status carries MiniMax's own code, and
// that code is what says what went wrong. Keeping it makes it
// possible to react to one code and not to another.
type BusinessError struct {
	// What is what was asked for.
	What string
	// Code is the business code the body carried.
	Code int
	// Message is the text the body carried.
	Message string
}

// Error is the error's text.
func (e *BusinessError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: status %d", e.What, e.Code)
	}
	return fmt.Sprintf("%s: %s", e.What, e.Message)
}

// Is says whether the error is the refusal of the sign-in.
func (e *BusinessError) Is(target error) bool {
	return target == ErrSignInRefused && e.Code == signInRefusedCode
}

// quotaPattern says a refusal is about the account's credits or plan
// running out.
var quotaPattern = regexp.MustCompile(`(?i)insufficient|balance|credit|quota|exhaust|limit|余额|积分|额度|不足|用完|上限`)

// rateLimitPattern says a body is about being sent too fast.
//
// It is taken out before the quota words are looked for: a rate limit
// is a different thing, the account is fine and the answer comes when
// the caller slows down. Rotating away from the account and marking
// its window spent would be the wrong answer.
var rateLimitPattern = regexp.MustCompile(`(?i)rate[\s_-]*limits?|too\s+many\s+requests|throttl\w*`)

// IsQuotaRefusal says whether a body is a refusal for running out of
// credits or plan.
//
// A rate limit is not: the account is fine and the answer comes when
// the caller slows down, so the rate-limit wording is taken out
// before the rest is looked for.
func IsQuotaRefusal(body []byte) bool {
	s := strings.ToLower(string(body))
	s = rateLimitPattern.ReplaceAllString(s, "")
	return quotaPattern.MatchString(s)
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
// The error that comes back is a *BusinessError, which carries the
// code; errors.Is(err, ErrSignInRefused) tells a refused sign-in from
// any other business error.
func CheckEnvelope(what string, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: not JSON", what)
	}
	if si := env.StatusInfo; si != nil && si.Code != 0 {
		if si.Code == signInRefusedCode {
			return &BusinessError{
				What:    what,
				Code:    si.Code,
				Message: "the sign-in was refused",
			}
		}
		msg := si.Message
		if msg == "" {
			msg = si.Msg
		}
		if msg == "" {
			msg = fmt.Sprintf("status %d", si.Code)
		}
		return &BusinessError{What: what, Code: si.Code, Message: msg}
	}
	if br := env.BaseResp; br != nil && br.StatusCode != 0 {
		msg := br.StatusMsg
		if msg == "" {
			msg = fmt.Sprintf("status %d", br.StatusCode)
		}
		return &BusinessError{What: what, Code: br.StatusCode, Message: msg}
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
//
// It refuses two more redirects for the same kind of reason: one that
// downgrades https to http, because the sign-in would go out in
// cleartext, and one that changes the path or the query of a signed
// request, because the signature is made over the path and the query
// and cannot be replayed.
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	timeout   time.Duration
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
		// The timeout is not on the shared client but on the request
		// this client makes: a chat streams for as long as it needs and
		// goes through its own client, while the account API and the
		// token endpoint answer in seconds and must not hang.
		http:      &http.Client{Transport: transport},
		timeout:   timeout,
		userAgent: ua,
	}
}

// defaultTransport is the transport used when the caller has none.
//
// It asks for no compression and does not add Accept-Encoding of its
// own: MiniMax's behaviour with compression is worse than useless for a
// stream.
//
// Many accounts share one transport, so it is tuned for that: HTTP/2 is
// negotiated, and up to 32 idle connections per host are kept. Without
// that, a third request to the same host would have its connection
// closed instead of pooled.
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
}

// redirectStatuses are the statuses a redirect can have.
var redirectStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// maxBody is the most a response body may be.
const maxBody = 32 << 20

// sameHost says whether two URLs are on the same host.
//
// The name is compared without the case, and the port by what it
// means: ":443" on https and no port at all are the same place, but
// another port is another service.
//
// The scheme has to be the same too. A redirect that keeps the host
// but changes https to http would send the Authorization header out
// in cleartext, and whoever is on the way could read it and replay it.
func sameHost(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

// effectivePort is the port a URL really uses.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// sameAddress says whether two URLs are on the same host and port,
// whatever the scheme is.
func sameAddress(a, b *url.URL) bool {
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return a.Port() == b.Port()
}

// sameRequest says whether a redirect keeps the path and the query.
//
// The signature of a signed request is computed over the path and the
// query, so a redirect that changes either of them cannot carry the
// old signature.
func sameRequest(a, b *url.URL) bool {
	return a.Path == b.Path && a.RawQuery == b.RawQuery
}

// signed says whether a request carries a signature.
//
// The two headers are the ones internal/sign makes: "yy" over the
// path, the query and the body, and "x-signature" over the time and
// the body.
func signed(h http.Header) bool {
	return h.Get("yy") != "" || h.Get("x-signature") != ""
}

// copyRequest makes the request the redirect loop sends.
//
// The caller's request is left as it is: the copy is what the
// User-Agent goes into, and a request with no header map at all is
// given an empty one so that writing into it cannot panic.
func copyRequest(req *http.Request) *http.Request {
	out := req.Clone(req.Context())
	if out.Header == nil {
		out.Header = http.Header{}
	}
	return out
}

// isDowngrade says whether a redirect takes the request from https to
// http.
func isDowngrade(from, to *url.URL) bool {
	return strings.EqualFold(from.Scheme, "https") && strings.EqualFold(to.Scheme, "http")
}

// schemeHost is what an address is called in an error: the scheme and
// the host, without the path.
func schemeHost(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// Do sends a request through the client's own transport.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return doWith(req, c.http.Transport, c.timeout, c.userAgent)
}

// DoWith sends a request through a given transport.
//
// A redirect that stays on the host is followed; one that leaves it is
// an OtherSiteError, and the request is not sent on.
//
// The transport is a parameter because the proxy is a property of the
// transport: an account with its own exit needs its own client, and
// the same client is shared by every account that shares an exit.
//
// There is no timeout here: a chat streams for as long as it needs,
// and the caller's context carries the deadline.
func DoWith(req *http.Request, transport http.RoundTripper) (*http.Response, error) {
	return doWith(req, transport, 0, "")
}

// doWith is the redirect loop both of them share.
//
// The caller's request is not touched: the User-Agent goes into a
// copy, and a request with no header map at all is given one, so
// writing into it cannot panic.
func doWith(req *http.Request, transport http.RoundTripper, timeout time.Duration, userAgent string) (*http.Response, error) {
	if transport == nil {
		transport = defaultTransport()
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	current := copyRequest(req)
	if current.Header.Get("User-Agent") == "" && userAgent != "" {
		current.Header.Set("User-Agent", userAgent)
	}
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
		if isDowngrade(current.URL, next) && sameAddress(current.URL, next) {
			// The sign-in would go out in cleartext, so the redirect is
			// refused and never followed.
			resp.Body.Close()
			return nil, &SchemeDowngradeError{
				From:   schemeHost(current.URL),
				To:     schemeHost(next),
				Status: resp.StatusCode,
			}
		}
		if !sameHost(next, current.URL) {
			resp.Body.Close()
			return nil, &OtherSiteError{
				From:   current.URL.Host,
				To:     next.Host,
				Status: resp.StatusCode,
			}
		}
		if signed(current.Header) && !sameRequest(current.URL, next) {
			// The signature is made over the path and the query, so the
			// old one cannot be replayed. Sending it would be refused by
			// the upstream, and that refusal reads as a dead token.
			resp.Body.Close()
			return nil, &StaleSignatureError{
				From:   schemeHost(current.URL),
				To:     schemeHost(next),
				Status: resp.StatusCode,
			}
		}
		if hop >= 5 {
			// Too many redirects is an error, not an answer: handing back
			// the redirect itself would leave the caller with a body
			// nobody read.
			resp.Body.Close()
			return nil, fmt.Errorf("too many redirects")
		}
		// The body has to be read again: http.Request.Body is a reader
		// the previous request already consumed, so replaying it as it
		// is would send an empty body — and a chat with an empty body is
		// not the chat that was asked for. GetBody is what reads it a
		// second time; without it the body cannot be replayed, so the
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
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// PostForm sends a form POST, as the OAuth endpoints want.
func (c *Client) PostForm(ctx context.Context, rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// ReadBody reads a response's body and closes it.
//
// The body is read through a limit: an upstream that streams without
// end must not be able to take all the memory. A body over the limit
// is an error, not a truncated answer.
func ReadBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("read body: no body")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("read body: the body is larger than %d bytes", maxBody)
	}
	return b, nil
}
```

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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestResolveAnEmptyGroupFallsBackToTheDefault(t *testing.T) {
	// 一个空的组值不是一条出路。空地址会被当成直连，请求就悄悄
	// 走掉了代理——这正是代理支持最不能犯的错。
	r := New(map[string]string{"g": "", "blank": "   "}, "http://127.0.0.1:7890")
	cases := []struct {
		name string
		acc  store.Account
		want string
	}{
		{"空组落到全局", store.Account{ProxyGroup: "g"}, "http://127.0.0.1:7890"},
		{"空白组落到全局", store.Account{ProxyGroup: "blank"}, "http://127.0.0.1:7890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.Resolve(c.acc)
			if got != c.want {
				t.Fatalf("Resolve = %q, want %q", got, c.want)
			}
			if got == Direct {
				t.Fatalf("Resolve = %q: the traffic would go out direct", got)
			}
		})
	}
}

func TestResolveNeverReturnsAnEmptyAddress(t *testing.T) {
	resolvers := []*Resolver{
		New(nil, ""),
		New(map[string]string{"g": ""}, ""),
		New(map[string]string{"g": " "}, "  "),
		New(nil, "direct"),
	}
	for _, r := range resolvers {
		got := r.Resolve(store.Account{ProxyGroup: "g"})
		if strings.TrimSpace(got) == "" {
			t.Fatalf("Resolve = %q: an empty address would go out direct", got)
		}
	}
}

func TestAnEmptyGroupDoesNotResolveToAProxyLessTransport(t *testing.T) {
	r := New(map[string]string{"g": ""}, "http://127.0.0.1:7890")
	addr := r.Resolve(store.Account{ProxyGroup: "g"})
	c, err := r.Client(addr)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", c.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("the transport has no proxy: the traffic would go out direct")
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

func TestTheSameAddressWrittenTwoWaysIsOneClient(t *testing.T) {
	r := New(nil, "")
	c1, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.Client("HTTP://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("expected the same client for the same address")
	}
	c3, err := r.Client("http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c4, err := r.Client("http://127.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	if c3 != c4 {
		t.Fatal("expected the same client for the same address with the default port")
	}
}

func TestInvalidateDropsTheClient(t *testing.T) {
	r := New(nil, "")
	c1, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	// 写成另一个样子也要能丢掉同一个。
	r.Invalidate("HTTP://127.0.0.1:7890")
	c2, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 {
		t.Fatal("expected a new client after Invalidate")
	}
}

func TestInvalidateClosesTheIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	defer srv.Close()

	r := New(nil, "")
	proxyURL := "http://" + srv.Listener.Addr().String()
	c, err := r.Client(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	r.Invalidate(proxyURL)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle connection of the dropped transport was not closed")
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

func TestDirectIsComparedWithoutTheCase(t *testing.T) {
	r := New(nil, "")
	for _, v := range []string{"direct", "Direct", "DIRECT", " direct "} {
		c, err := r.Client(v)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%q: transport = %T", v, c.Transport)
		}
		if tr.Proxy != nil {
			t.Fatalf("%q: the direct transport must not use a proxy", v)
		}
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

func TestTheTransportIsTunedForManyAccounts(t *testing.T) {
	r := New(nil, "")
	c, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*http.Transport)
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("http/2 is not attempted")
	}
	if tr.MaxIdleConnsPerHost != 32 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 32", tr.MaxIdleConnsPerHost)
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

func TestAnAddressWithoutAHostIsRefused(t *testing.T) {
	r := New(nil, "")
	if _, err := r.Client("http://"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := r.Client("socks5://"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestASocks5AddressWithoutAPortIsRefused(t *testing.T) {
	r := New(nil, "")
	if _, err := r.Client("socks5://host"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := r.Client("socks5h://host"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestTheCredentialsAreNotInTheErrorText(t *testing.T) {
	r := New(nil, "")
	_, err := r.Client("http://user:secret@")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user") {
		t.Fatalf("the credentials are in the error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("the error does not show the redacted address: %v", err)
	}
	_, err = r.Client("socks5://user:secret@host")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the credentials are in the error: %v", err)
	}
}

// TestRedacted says the credentials are taken out of an address that is
// shown, and the rest of it stays.
//
// 地址要给用户看，凭据是秘密：换掉凭据，scheme、主机和端口都还在。
func TestRedacted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://user:secret@host:1080", "http://***@host:1080"},
		{"socks5h://user:secret@host:1080", "socks5h://***@host:1080"},
		{"http://user@host:1080", "http://***@host:1080"},
		{"http://host:1080", "http://host:1080"},
		{"direct", "direct"},
		{"", ""},
	}
	for _, c := range cases {
		got := Redacted(c.in)
		if got != c.want {
			t.Fatalf("Redacted(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "user") {
			t.Fatalf("Redacted(%q) = %q, want no credentials", c.in, got)
		}
	}
}

func TestCheckTreatsA407AsAFailure(t *testing.T) {
	// 代理拒绝我们的凭据，就是不能用，不是健康。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer srv.Close()

	r := New(nil, "")
	proxyURL := "http://user:secret@" + srv.Listener.Addr().String()
	err := r.Check(context.Background(), proxyURL, "http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("err = %v, want the status in it", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the credentials are in the error: %v", err)
	}
}

func TestCheckDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("other"))
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()

	r := New(nil, "")
	err := r.Check(context.Background(), Direct, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("the other server got %d requests, want 0", got)
	}
}

func TestCheckHasADeadline(t *testing.T) {
	// 一个接了连接却不回答的代理不能把健康检查挂死。
	old := checkTimeout
	checkTimeout = 300 * time.Millisecond
	defer func() { checkTimeout = old }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	r := New(nil, "")
	start := time.Now()
	err = r.Check(context.Background(), Direct, "http://"+ln.Addr().String()+"/")
	if err == nil {
		t.Fatal("expected an error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the check took %s: it has no deadline of its own", took)
	}
}

func TestCheckPassesWhenTheTargetAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	r := New(nil, "")
	if err := r.Check(context.Background(), Direct, srv.URL); err != nil {
		t.Fatal(err)
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
//
// Both "socks5://" and "socks5h://" resolve the name at the proxy,
// not here: the address is sent to the proxy as it is written, and the
// proxy looks it up where the proxy is. That is what curl's "socks5h://"
// does; curl's "socks5://" resolves the name locally, and this package
// does not do that.
package egress

import (
	"context"
	"fmt"
	"io"
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

// checkTimeout is how long a health check waits for its answer.
//
// It is a variable so that the test can shorten it.
var checkTimeout = 10 * time.Second

// Resolver resolves an account's egress and hands out clients for it.
type Resolver struct {
	mu sync.Mutex
	// groups are the named ways out. They are set when the resolver is
	// made and are not written to afterwards.
	groups map[string]string
	// def is the global way out. Like the groups, it is set when the
	// resolver is made and is not written to afterwards.
	def string
	// clients are the HTTP clients, by way out.
	clients map[string]*http.Client
	// transports are the transports, by way out.
	transports map[string]http.RoundTripper
}

// New makes a resolver over the configured groups and the global
// default.
func New(groups map[string]string, def string) *Resolver {
	g := make(map[string]string, len(groups))
	for k, v := range groups {
		g[k] = v
	}
	return &Resolver{
		groups:     g,
		def:        def,
		clients:    map[string]*http.Client{},
		transports: map[string]http.RoundTripper{},
	}
}

// Resolve gives the proxy URL an account's requests go through.
//
// Nothing configured means direct: the account has no proxy of its
// own, no group, and there is no global default either.
//
// A group whose value is empty is not a way out: it falls through to
// the global default. An empty value that resolved to nothing would
// send the traffic out direct, which is the one mistake a proxy
// support must not make.
//
// The lock is taken, so that the maps are read the same way the caches
// are.
func (r *Resolver) Resolve(a store.Account) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := strings.TrimSpace(a.ProxyURL); u != "" {
		return u
	}
	if g := strings.TrimSpace(a.ProxyGroup); g != "" {
		if u, ok := r.groups[g]; ok {
			if v := strings.TrimSpace(u); v != "" {
				return v
			}
		}
	}
	v := strings.TrimSpace(r.def)
	if v == "" {
		return Direct
	}
	return v
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
// so one URL means one client, however many accounts use it. The key is
// the normalised URL, so the same address written two ways is one
// client.
func (r *Resolver) Client(proxyURL string) (*http.Client, error) {
	key := cacheKey(proxyURL)
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[key]; ok {
		return c, nil
	}
	tr, err := r.transportLocked(key)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: tr}
	r.clients[key] = c
	return c, nil
}

// Transport gives the transport for a proxy URL, making it if it is not
// there yet.
//
// This is what a caller that has its own client needs: the upstream
// client takes a transport, and the transport is what carries the
// proxy.
//
// Transports are cached by URL: the proxy is a property of the
// transport, so one URL means one transport, however many accounts use
// it.
func (r *Resolver) Transport(proxyURL string) (http.RoundTripper, error) {
	key := cacheKey(proxyURL)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.transportLocked(key)
}

// transportLocked is Transport, with the lock held.
func (r *Resolver) transportLocked(key string) (http.RoundTripper, error) {
	if tr, ok := r.transports[key]; ok {
		return tr, nil
	}
	tr, err := newTransport(key)
	if err != nil {
		return nil, err
	}
	r.transports[key] = tr
	return tr, nil
}

// Invalidate drops the cached client for a proxy URL, so a changed
// address is picked up.
//
// The dropped transport's idle connections are closed: they belong to
// the old address and would otherwise stay open until they time out.
func (r *Resolver) Invalidate(proxyURL string) {
	key := cacheKey(proxyURL)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, key)
	if tr, ok := r.transports[key]; ok {
		delete(r.transports, key)
		if t, ok := tr.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
}

// Groups gives the configured groups.
//
// The lock is taken, so that the maps are read the same way the caches
// are.
func (r *Resolver) Groups() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.groups))
	for k, v := range r.groups {
		out[k] = v
	}
	return out
}

// cacheKey is the key a proxy URL is cached under.
//
// The same address written two ways is the same address: the scheme is
// lowercased, the host is lowercased, the default port is dropped, and
// the rest is trimmed. Without this, "HTTP://h:1" and "http://h:1"
// would be two clients, and Invalidate would drop only one of them.
func cacheKey(proxyURL string) string {
	s := strings.TrimSpace(proxyURL)
	if s == "" {
		return Direct
	}
	if strings.EqualFold(s, Direct) {
		return Direct
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	return u.String()
}

// isDirect says whether a value means "no proxy".
//
// The comparison is without the case and without the space around it:
// "Direct" and "DIRECT" mean the same thing as "direct".
func isDirect(proxyURL string) bool {
	return strings.EqualFold(strings.TrimSpace(proxyURL), Direct)
}

// Redacted is a proxy URL as it may be shown.
//
// The credentials are taken out: they are a secret, and the health
// check's error is stored in the database. The scheme, the host and
// the port stay, so that one address can still be told from another.
//
// An address without a scheme has no credentials to take out of it, so
// it is left as it is.
func Redacted(proxyURL string) string {
	s := strings.TrimSpace(proxyURL)
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
	authority, tail := rest, ""
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		authority, tail = rest[:slash], rest[slash:]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return s
	}
	return s[:i+3] + "***@" + authority[at+1:] + tail
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
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	if isDirect(proxyURL) {
		return base, nil
	}
	u, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", Redacted(proxyURL), err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q: no host, write an address like http://host:port", Redacted(proxyURL))
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
		return base, nil
	case "socks5", "socks5h":
		if u.Port() == "" {
			return nil, fmt.Errorf("proxy %q: no port, write an address like socks5h://host:port", Redacted(proxyURL))
		}
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %q: %w", Redacted(proxyURL), err)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("socks5 proxy %q: no context dialer", Redacted(proxyURL))
		}
		base.DialContext = ctxDialer.DialContext
		base.Proxy = nil
		return base, nil
	case "":
		// 没有 scheme 就不是一个地址。默默直连是最坏的一种错：它
		// 看起来跑通了，其实请求根本没走代理。
		return nil, fmt.Errorf("proxy %q: no scheme, write http:// or socks5h://", Redacted(proxyURL))
	default:
		return nil, fmt.Errorf("proxy %q: unsupported scheme %q", Redacted(proxyURL), u.Scheme)
	}
}

// Check does one request through a proxy URL and says whether it worked.
//
// It is what the egress health check runs. The target is a small
// address that answers fast; a proxy that cannot reach it is not usable.
//
// The check has its own deadline, so a proxy that takes the connection
// and never answers fails the check instead of hanging it.
func (r *Resolver) Check(ctx context.Context, proxyURL, target string) error {
	c, err := r.Client(proxyURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	// A redirect means the target is not usable as a health probe, so it
	// is not followed: the answer has to come from the target itself.
	client := &http.Client{
		Transport: c.Transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The body is drained so the connection goes back into the pool.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// A redirect is not followed: the answer has to come from the target
	// itself, and a redirect says the target is somewhere else. It is
	// not a failure either — the proxy did carry the request through.
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("proxy %q: HTTP %d", Redacted(proxyURL), resp.StatusCode)
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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/upstream"
)

func testClient(t *testing.T) *upstream.Client {
	t.Helper()
	return upstream.New(upstream.Options{Timeout: 5 * time.Second})
}

// testDevice makes a device that polls at once, for a test that does
// not care about the waiting.
func testDevice() *Device {
	return &Device{
		UserCode:  "ABCD",
		verifier:  "v",
		pollField: "device_code",
		pollValue: "dev-1",
		deadline:  time.Now().Add(time.Minute),
		interval:  time.Millisecond,
		slowDown:  time.Millisecond,
	}
}

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

	c := testClient(t)
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

	c := testClient(t)
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

func TestStartDeviceUserCodeIntervalIsInMilliseconds(t *testing.T) {
	// 站点自己那套里的间隔是毫秒：500 是半秒，不是八分多钟。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"user_code":        "WXYZ-1234",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       time.Now().Add(10 * time.Minute).UnixMilli(),
			"interval":         500,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err != nil {
		t.Fatal(err)
	}
	if d.interval != 500*time.Millisecond {
		t.Fatalf("interval = %v, want 500ms", d.interval)
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

	c := testClient(t)
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

func TestStartDeviceWithADeadCode(t *testing.T) {
	// 站点说这个码已经过期了：不该再等十分钟。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-3",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       time.Now().Add(-time.Minute).UnixMilli(),
			"interval":         5,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "expired_token" {
		t.Fatalf("error = %+v", oe)
	}
}

func TestStartDeviceBadAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	c := testClient(t)
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

	c := testClient(t)
	d := testDevice()
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
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
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

	c := testClient(t)
	d := testDevice()
	tok, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("polled %d times, want 3", got)
	}
	if tok.Access != "acc-token" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestPollKeepsWaitingByStatus(t *testing.T) {
	// 另一种写法：200 加 status=pending。
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
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

	c := testClient(t)
	d := testDevice()
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("polled %d times, want 2", got)
	}
}

func TestPollSlowDown(t *testing.T) {
	// slow_down 是让我们等久一点，不是失败。
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
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

	c := testClient(t)
	d := testDevice()
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("polled %d times, want 2", got)
	}
}

func TestPollDeviceDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	}))
	defer srv.Close()

	c := testClient(t)
	d := testDevice()
	if _, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token"); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollNeedsARefreshToken(t *testing.T) {
	// 没有 refresh token 的账号活不过一小时：与其建一个这样的账号，
	// 不如让这次登录失败。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc-token",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	d := testDevice()
	_, err := d.Poll(context.Background(), c, srv.URL+"/oauth2/token")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "missing_refresh_token" {
		t.Fatalf("error = %+v", oe)
	}
}

func TestOAuthErrorCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	d := testDevice()
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

func TestDeviceTextKeepsTheVerifierSecret(t *testing.T) {
	d := &Device{
		UserCode: "ABCD-EFGH",
		URL:      "https://example.invalid/activate",
		verifier: "the-pkce-verifier",
	}
	texts := []string{
		d.String(),
		fmt.Sprintf("%+v", d),
		fmt.Sprintf("%v", d),
		fmt.Sprintf("%s", d),
	}
	for _, text := range texts {
		if strings.Contains(text, "the-pkce-verifier") {
			t.Fatalf("the verifier is in the text: %s", text)
		}
	}
	if !strings.Contains(d.String(), "ABCD-EFGH") {
		t.Fatalf("the text says nothing useful: %s", d.String())
	}
}

func TestClaims(t *testing.T) {
	payload := `{"sub":"user-1","account_id":"site-1"}`
	// 没有点号的东西不是 JWT。
	if _, _, ok := claims("not-a-jwt"); ok {
		t.Fatal("a string that is not a JWT was read as one")
	}
	// 补了填充的 payload 也是 JWT。
	padded := "x." + base64.URLEncoding.EncodeToString([]byte(payload)) + ".y"
	sub, account, ok := claims(padded)
	if !ok {
		t.Fatal("a padded payload was not read")
	}
	if sub != "user-1" || account != "site-1" {
		t.Fatalf("claims = %q/%q", sub, account)
	}
	// 没补填充的也是。
	raw := "x." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".y"
	sub, account, ok = claims(raw)
	if !ok || sub != "user-1" || account != "site-1" {
		t.Fatalf("claims = %q/%q, ok = %v", sub, account, ok)
	}
	// 读不出 payload 的不是 JWT。
	if _, _, ok := claims("x.!!!.y"); ok {
		t.Fatal("a payload that cannot be decoded was read as one")
	}
}

func TestTheUnitsOfTheSite(t *testing.T) {
	// expires_in 的单位
	for _, c := range []struct {
		name string
		in   int64
		want time.Duration
	}{
		{"an hour in seconds", 3600, time.Hour},
		{"an hour in milliseconds", 3600000, time.Hour},
		{"ten minutes in milliseconds", 600000, 10 * time.Minute},
		{"a day in seconds", 86400, 24 * time.Hour},
		{"nothing", 0, 0},
	} {
		if got := expiresDuration(c.in); got != c.want {
			t.Errorf("expiresDuration(%d) = %v, want %v (%s)", c.in, got, c.want, c.name)
		}
	}
	// 轮询间隔的单位
	for _, c := range []struct {
		name string
		in   int64
		want time.Duration
	}{
		{"five seconds", 5, 5 * time.Second},
		{"five minutes", 300, 5 * time.Minute},
		{"three seconds in milliseconds", 3000, 3 * time.Second},
		{"nothing", 0, 0},
	} {
		if got := pollInterval(c.in); got != c.want {
			t.Errorf("pollInterval(%d) = %v, want %v (%s)", c.in, got, c.want, c.name)
		}
	}
	// expired_in 的单位
	now := time.Now()
	for _, c := range []struct {
		name string
		in   int64
		want time.Time
	}{
		{"ten minutes left in seconds", 600, now.Add(10 * time.Minute)},
		{"ten minutes left in milliseconds", 600000, now.Add(10 * time.Minute)},
		{"an absolute time in milliseconds", now.Add(10 * time.Minute).UnixMilli(), now.Add(10 * time.Minute)},
		{"an absolute time in seconds", now.Add(10 * time.Minute).Unix(), now.Add(10 * time.Minute)},
	} {
		got, ok := siteDeadline(c.in)
		if !ok {
			t.Errorf("siteDeadline(%d) said nothing (%s)", c.in, c.name)
			continue
		}
		if diff := got.Sub(c.want); diff < -time.Minute || diff > time.Minute {
			t.Errorf("siteDeadline(%d) = %v, want about %v (%s)", c.in, got, c.want, c.name)
		}
	}
	if _, ok := siteDeadline(0); ok {
		t.Error("siteDeadline(0) said something")
	}
}

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

	c := testClient(t)
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

	c := testClient(t)
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

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if !oe.IsSignedOut() {
		t.Fatal("expected the error to be a sign-out")
	}
}

func TestRefreshSignOutInA200Body(t *testing.T) {
	// MiniMax 把业务错误放在 200 的 body 里。签出不是这么来的：
	// 参考实现读的是 invalid_grant 加 400。200 body 里的
	// invalid_grant 是别的事，不能拿来把账号登出。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.HTTPStatus != 200 {
		t.Fatalf("error = %+v", oe)
	}
	if oe.IsSignedOut() {
		t.Fatal("a sign-out in a 200 body was a sign-out")
	}
}

// TestSignedOutNeedsTheStatus says only invalid_grant with a 400 signs
// an account out.
//
// 登出是贵的事：账号的续期链断了，只能人再登一次。401 或者 500 带着
// 同样的 code 不是登出，是路过。
func TestSignedOutNeedsTheStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, false},
		{http.StatusInternalServerError, false},
		{http.StatusOK, false},
	}
	for _, c := range cases {
		e := &Error{Code: "invalid_grant", HTTPStatus: c.status}
		if got := e.IsSignedOut(); got != c.want {
			t.Errorf("invalid_grant with %d is a sign-out = %v, want %v", c.status, got, c.want)
		}
	}
	// 别的 code 不是登出，400 也不行。
	if (&Error{Code: "invalid_request", HTTPStatus: http.StatusBadRequest}).IsSignedOut() {
		t.Error("invalid_request with a 400 was a sign-out")
	}
}

// TestStartDeviceUserCodeWithAZeroExpiredIn says a zero in the
// expired_in field is not a field the site means.
//
// expired_in 是 0 的时候，参考实现把它当没有这个字段（它的 pos() 只
// 认大于零的值），于是这个答案不是拿 user code 去轮询的那种答案。
// 一个零的截止时间不是截止时间。
func TestStartDeviceUserCodeWithAZeroExpiredIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"user_code":        "WXYZ-1234",
			"verification_uri": "https://example.invalid/activate",
			"expired_in":       0,
		})
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "invalid_device_authorization_response" {
		t.Fatalf("code = %q, want the invalid answer", oe.Code)
	}
}

// TestStartDeviceWithoutExpiredIn says an answer without the
// expired_in field is not a user-code poll.
//
// 没有 expired_in 这个字段，就不是拿 user code 去轮询的那种答案。
func TestStartDeviceWithoutExpiredIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"user_code":        "WXYZ-1234",
			"verification_uri": "https://example.invalid/activate",
		})
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := StartDevice(context.Background(), c, srv.URL+"/oauth2/device/code")
	if err == nil {
		t.Fatal("expected an error")
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.Code != "invalid_device_authorization_response" {
		t.Fatalf("error = %+v", oe)
	}
}

func TestRefreshKeepsTheMessage(t *testing.T) {
	// 有的 4xx 只带 message：这个说明是账号的 last error。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_request","message":"the code is wrong"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "the code is wrong") {
		t.Fatalf("err = %v, want the message", err)
	}
	// 刷新的错误说的是 token 端点，不是登录服务器。
	if !strings.Contains(err.Error(), "token endpoint") {
		t.Fatalf("err = %v, want it to name the token endpoint", err)
	}
}

func TestRefreshWithALostAnswer(t *testing.T) {
	// 请求到了站点，答案没了：站点可能已经把 refresh token 用了。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
}

func TestRefreshWithARefusedConnection(t *testing.T) {
	// 连都没连上：请求肯定没到站点，同一个 token 再来一次是安全的。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := srv.URL + "/oauth2/token"
	srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, endpoint, "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

func TestRenewerSpendsOnceUnderConcurrency(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	current := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	var wg sync.WaitGroup
	var started sync.WaitGroup
	started.Add(8)
	results := make([]*Token, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started.Done()
			tok, err := r.Renew(context.Background(), "acc_01", current, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			results[i] = tok
		}(i)
	}
	started.Wait()
	// 让每个 goroutine 都进到 Renew 里去：它们都得等同一个刷新。
	time.Sleep(200 * time.Millisecond)
	close(release)
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

func TestRenewerWalksTheChainOnAfterALostStoreWrite(t *testing.T) {
	// 站点给了新 token，存库失败，库里还是那个已经用过的。
	// renewer 手上有新的：它得把它用掉，链才走得下去。
	var calls int32
	var mu sync.Mutex
	var spent []string
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		spent = append(spent, refresh)
		mu.Unlock()
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	first, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	// 存库失败：调用方手上还是那个旧 token。
	second, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the chain was walked %d times, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if spent[1] != "new-old-refresh" {
		t.Fatalf("the second spend used %q, want the token the renewer held", spent[1])
	}
	if second.Refresh == first.Refresh {
		t.Fatalf("the chain did not move on: %+v", second)
	}
}

func TestRenewerSpendsAgainWhenTheSiteDoesNotRotate(t *testing.T) {
	// 站点不给新的 refresh token：它没有把旧的用掉，
	// 所以下一次刷新还得再发一次。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{
			Access:  fmt.Sprintf("acc-%d", atomic.LoadInt32(&calls)),
			Refresh: refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "same-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was spent %d times, want 2", got)
	}
	if tok.Access == "old-access" {
		t.Fatalf("the token = %+v, want a new one", tok)
	}
}

func TestRenewerFollowsTheCallersNewerToken(t *testing.T) {
	// renewer 手上的 token 调用方不知道：调用方拿的是新的
	// （刚登录的，或者别的进程写进去的）。调用方说了算。
	var calls int32
	var mu sync.Mutex
	var spent []string
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		spent = append(spent, refresh)
		mu.Unlock()
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	newer := &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	tok, err := r.Renew(context.Background(), "acc_01", newer, spend)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spent) != 2 || spent[1] != "r2" {
		t.Fatalf("spent = %v, want the caller's token", spent)
	}
	if tok.Access != "acc-for-r2" {
		t.Fatalf("token = %+v, want the answer of the caller's own renewal", tok)
	}
}

func TestRenewerDoesNotMarkAFailedRefreshSpent(t *testing.T) {
	// 这个包的全部承诺：失败的刷新不把 token 记成用掉了。
	var calls int32
	var fail atomic.Bool
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return nil, errors.New("the network is down")
		}
		return &Token{
			Access:  "new-access",
			Refresh: "new-refresh",
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	fail.Store(true)
	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err == nil {
		t.Fatal("expected an error")
	}
	fail.Store(false)
	// 失败过的 token 还能再发：它多半没有到站点。
	tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was sent %d times, want 2", got)
	}
	if tok.Access != "new-access" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestRenewerDoesNotSendATokenOfUnknownOutcomeAgain(t *testing.T) {
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("the answer was lost: %w", ErrRefreshOutcomeUnknown)
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	_, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
	// 再来一次：答案不知道的 token 不再发。
	_, err = r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was sent %d times, want 1", got)
	}
}

func TestRenewerGivesEveryCallerItsOwnToken(t *testing.T) {
	// 两个调用方各拿一个 refresh token：各发各的，
	// 谁也不能拿别人的答案。
	var calls int32
	release := make(chan struct{})
	var mu sync.Mutex
	seen := map[string]bool{}
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		seen[refresh] = true
		mu.Unlock()
		<-release
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	tokens := map[string]*Token{
		"r1": {Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)},
		"r2": {Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)},
	}
	results := map[string]*Token{}
	var wg sync.WaitGroup
	var mu2 sync.Mutex
	for name, tok := range tokens {
		wg.Add(1)
		go func(name string, tok *Token) {
			defer wg.Done()
			got, err := r.Renew(context.Background(), "acc_01", tok, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			mu2.Lock()
			results[name] = got
			mu2.Unlock()
		}(name, tok)
	}
	// 等两个刷新都开始了。
	for i := 0; i < 200 && atomic.LoadInt32(&calls) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !seen["r1"] || !seen["r2"] {
		t.Fatalf("spent = %v, want both tokens", seen)
	}
	for name, tok := range results {
		if tok == nil {
			t.Fatalf("%s got nothing", name)
		}
		if want := "acc-for-" + name; tok.Access != want {
			t.Fatalf("%s got %+v, want the answer of its own renewal", name, tok)
		}
	}
}

func TestRenewerLeaderCancellationDoesNotKillTheFollowers(t *testing.T) {
	// 第一个来的调用方断线了，别的调用方还在等：
	// 刷新不该跟着死。
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := r.Renew(leaderCtx, "acc_01", stored, spend)
		leaderErr <- err
	}()
	// 等刷新真的开始了。
	for i := 0; i < 200 && atomic.LoadInt32(&calls) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	followerErr := make(chan error, 1)
	followerTok := make(chan *Token, 1)
	go func() {
		tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
		followerTok <- tok
		followerErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	// 第一个来的调用方走了。
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the leader got %v, want the cancellation", err)
	}
	close(release)
	if err := <-followerErr; err != nil {
		t.Fatalf("the follower got %v", err)
	}
	if tok := <-followerTok; tok == nil || tok.Access != "new-access" {
		t.Fatalf("the follower got %+v", tok)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
}

func TestRenewerForget(t *testing.T) {
	r := NewRenewer()
	var calls int32
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Newest("acc_01"); !ok {
		t.Fatal("the renewer holds nothing")
	}
	r.Forget("acc_01")
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token is still held after Forget")
	}
	// 已经用掉的记录也没了：这个账号签出了，它的东西都不作数了。
	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatalf("Renew after Forget: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was spent %d times, want 2", got)
	}
}

func TestRenewerForgetLeavesARunningRenewalAlone(t *testing.T) {
	// 账号正在签出时在跑的刷新，完了不能把 token 放回来。
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Renew(context.Background(), "acc_01", stored, spend)
	}()
	for i := 0; i < 200 && atomic.LoadInt32(&calls) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	r.Forget("acc_01")
	close(release)
	<-done
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token came back from the dead")
	}
}

func TestRenewerReset(t *testing.T) {
	// Reset 把链忘掉，"用掉了"的记录还在。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	r.Reset("acc_01")
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token is still held after Reset")
	}
	// 用掉的 token 不能再发：链断了，账号得重新登录。
	_, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
}

func TestRenewerChainBroken(t *testing.T) {
	// renewer 手上的 token 也用掉了：链断了，没有能走的路了。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	r.mu.Lock()
	r.latest["acc_01"] = &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	r.spent["acc_01"] = map[string]bool{
		tokenHash("r1"): true,
		tokenHash("r2"): true,
	}
	r.mu.Unlock()

	_, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}, spend)
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("a spent token was sent %d times", got)
	}
}

func TestRenewerRefusesAnotherAccountsToken(t *testing.T) {
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		return &Token{
			Access:  "new-access",
			Refresh: "new-refresh",
			Expires: time.Now().Add(time.Hour),
			Subject: "user-1",
		}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Subject: "user-1", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	_, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a9", Refresh: "r9", Subject: "user-2", Expires: time.Now().Add(time.Hour)}, spend)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "another account") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenewerNewestGivesACopy(t *testing.T) {
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Newest("acc_01")
	if !ok || got.Access != "new-access" {
		t.Fatalf("newest = %+v, ok = %v", got, ok)
	}
	got.Access = "changed"
	again, _ := r.Newest("acc_01")
	if again.Access == "changed" {
		t.Fatal("the caller changed what the renewer holds")
	}
	// 没有 token 时 Latest 给的是调用方自己的。
	empty := NewRenewer()
	cur := &Token{Access: "mine"}
	if got := empty.Latest("acc_02", cur); got != cur {
		t.Fatalf("latest = %+v, want the caller's own token", got)
	}
	if _, ok := empty.Newest("acc_02"); ok {
		t.Fatal("the renewer holds a token it does not have")
	}
}

func TestNoSecretIsInAnyErrorOrURL(t *testing.T) {
	const secret = "the-secret-refresh-token"
	const verifier = "the-secret-pkce-verifier"

	// 刷新的错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","message":"no"}`))
	}))
	defer srv.Close()
	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}

	// renewer 的错误。
	r := NewRenewer()
	_, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return nil, errors.New("nope")
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}
	// 链断了的错误也不带 token。
	// 先成功一次：那个 token 就用掉了。
	if _, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return &Token{Access: "b", Refresh: "other", Expires: time.Now().Add(time.Hour)}, nil
		}); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	r.Reset("acc_01")
	_, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return &Token{Access: "b", Refresh: "other", Expires: time.Now().Add(time.Hour)}, nil
		})
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}

	// 设备码的错误和地址。
	d := &Device{
		UserCode: "ABCD",
		URL:      "https://example.invalid/activate?code=ABCD",
		verifier: verifier,
	}
	for _, text := range []string{d.String(), fmt.Sprintf("%+v", d), d.URL} {
		if strings.Contains(text, verifier) {
			t.Fatalf("the verifier is in %q", text)
		}
	}
}
```

测试文件顶部还需要这个助手：

```go
func testClient(t *testing.T) *upstream.Client {
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

// What an answer came from.
//
// The text of an error says which endpoint answered, so a failure of
// the refresh does not read as a failure of the sign-in.
const (
	// WhatSignIn is the device flow: the sign-in server.
	WhatSignIn = "sign-in"
	// WhatRefresh is the token endpoint, where a refresh token is
	// traded in.
	WhatRefresh = "refresh"
)

// signInTimeout is how long a device code is waited for when the
// answer gives no end of its own.
const signInTimeout = 10 * time.Minute

// slowDownWait is how much longer to wait when the site says slow
// down.
//
// It is a constant and not a variable: a test hook that production
// behaviour depends on is one t.Parallel() away from a race, and a
// test that needs a shorter wait sets the field on its own device.
const slowDownWait = 5 * time.Second

// The units of the numbers the site gives.
//
// The site is not consistent about the unit: some of its answers are in
// seconds and some in milliseconds, and nothing in the answer says
// which. One rule is used for every number, and it lives in one place:
//
//	a number that is longer than the longest length of time the field
//	can plausibly hold is in milliseconds; every other number is in
//	seconds.
//
// The longest plausible length is given by the field that reads the
// number, so the rule is applied the same way everywhere and every
// field says what it believes.
const (
	// maxTokenSeconds is the longest an access token can live: a day.
	// A number longer than that is not a number of seconds.
	maxTokenSeconds = 24 * 60 * 60
	// maxPollSeconds is the longest a poll interval can be: ten
	// minutes. A longer wait is not a poll.
	maxPollSeconds = 10 * 60
	// epochSecondsFloor is the first Unix second of this century
	// (2001-09-09). A number above it is a timestamp, not a length of
	// time.
	epochSecondsFloor = 1_000_000_000
	// epochMillisFloor is the first Unix millisecond of a time in this
	// century (1973-03-03). A timestamp above it is in milliseconds.
	epochMillisFloor = 100_000_000_000
)

// siteDuration reads a length of time the site gave.
//
// The number is in seconds, unless it is longer than the longest
// length of time the field can plausibly hold, in which case it is in
// milliseconds. See the unit rules above.
func siteDuration(v int64, longestSeconds int64) time.Duration {
	if v <= 0 {
		return 0
	}
	if v > longestSeconds {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(v) * time.Second
}

// expiresDuration reads how long an access token lives.
//
// The site sends an hour as 3600 and an hour as 3600000; the rule in
// siteDuration tells the two apart. A ten-minute token sent as 600000
// is ten minutes and not a hundred and sixty-six hours.
func expiresDuration(v int64) time.Duration {
	return siteDuration(v, maxTokenSeconds)
}

// pollInterval reads how long to wait between two polls.
//
// A poll that waits more than ten minutes is not a poll, so a number
// that big is a number of milliseconds: 3000 is three seconds and not
// fifty minutes, and 300 is five minutes and not a third of a second.
func pollInterval(v int64) time.Duration {
	return siteDuration(v, maxPollSeconds)
}

// siteDeadline reads when a device code ends.
//
// The site gives either a remaining time or an absolute timestamp, and
// the timestamp comes in seconds or in milliseconds. The rule: a
// number at or below epochSecondsFloor is a remaining time — in
// seconds, or in milliseconds when it is too long to be seconds — and a
// bigger one is an absolute timestamp, in milliseconds when it is above
// epochMillisFloor and in seconds otherwise.
func siteDeadline(v int64) (time.Time, bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	if v > epochSecondsFloor {
		if v > epochMillisFloor {
			return time.UnixMilli(v), true
		}
		return time.Unix(v, 0), true
	}
	return time.Now().Add(siteDuration(v, maxTokenSeconds)), true
}

// Error is an answer from MiniMax.
type Error struct {
	// Code is the "error" the answer carried.
	Code string
	// HTTPStatus is the status the answer came with.
	HTTPStatus int
	// Message is the text to show.
	Message string
	// What says which endpoint answered: WhatSignIn for the device
	// flow, WhatRefresh for the token endpoint.
	What string
}

// Error is the error's text.
//
// The code comes first: it is what says what went wrong, and the
// message is the site's own words for it.
func (e *Error) Error() string {
	who := "MiniMax's sign-in server"
	if e.What == WhatRefresh {
		who = "MiniMax's token endpoint"
	}
	if e.Message != "" {
		return fmt.Sprintf("%s said %s: %s", who, e.Code, e.Message)
	}
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("%s said %s (HTTP %d)", who, e.Code, e.HTTPStatus)
	}
	return fmt.Sprintf("%s said %s", who, e.Code)
}

// IsSignedOut says the answer is the one that signs an account out.
//
// The sign-out is invalid_grant with a 400, the way MiniMax Code
// reads it. The status is part of the answer and not a detail: a 401
// or a 500 that carries the same code is passing, and the sign-in is
// kept.
//
// A sign-out is expensive to get wrong — the account loses its refresh
// token chain — so the answer has to be the one the site means.
func (e *Error) IsSignedOut() bool {
	return e.Code == "invalid_grant" && e.HTTPStatus == http.StatusBadRequest
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
	// AccountID is the account id the access token carries, as the
	// site says it.
	//
	// It is the site's own id and not the store's: the renewer is
	// keyed by the store's account id, and the two are not the same
	// thing. A caller that keyed the renewer by this id would walk a
	// second renewal chain for one account and spend two refresh
	// tokens, which signs the account out. The store's id is the one
	// to use.
	//
	// The name is kept as it is: renaming an exported field breaks
	// every caller that reads it, and nothing reads it today. Use
	// SiteAccountID to make the difference plain.
	AccountID string
	// HasClaims says the access token's claims could be read.
	//
	// It is false for a token that is not a JWT and for one whose
	// payload cannot be decoded. The claim names themselves are
	// guesses, so a true here does not mean they are the right ones.
	HasClaims bool
}

// SiteAccountID is the account id the site's own token carries.
//
// It is not the store's account id, which is what the renewer is keyed
// by. See Token.AccountID.
func (t *Token) SiteAccountID() string {
	if t == nil {
		return ""
	}
	return t.AccountID
}

// Device is a sign-in waiting for the user.
type Device struct {
	// UserCode is the code the user types.
	UserCode string
	// URL is where the user goes.
	URL string

	verifier  string
	pollField string
	pollValue string
	deadline  time.Time
	interval  time.Duration
	slowDown  time.Duration
}

// String is the device's text.
//
// The PKCE verifier is a secret: it is what lets anybody holding it
// finish this sign-in. So it is not in the text, and %+v prints this
// instead of the fields, so a debug log cannot leak it.
func (d Device) String() string {
	return fmt.Sprintf("Device{UserCode:%q URL:%q}", d.UserCode, d.URL)
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
		return nil, oauthError(resp.StatusCode, body, WhatSignIn)
	}
	var b struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURL         string `json:"verification_url"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		// ExpiredIn is a pointer, so that an answer without the field
		// is told from an answer whose field is zero. The two are not
		// the same: the field says the site is polling for the user
		// code, and a zero is a field that is there.
		//
		// A zero, though, is not a field the site means: the reference
		// reads it as absent (its pos() only takes a value above
		// zero), and so do we.
		ExpiredIn *int64 `json:"expired_in"`
		Interval  int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
	}
	userCode := strings.TrimSpace(b.UserCode)
	deviceCode := strings.TrimSpace(b.DeviceCode)
	uri := strings.TrimSpace(b.VerificationURI)
	if uri == "" {
		uri = strings.TrimSpace(b.VerificationURL)
	}
	if userCode == "" || uri == "" {
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
	}

	d := &Device{UserCode: userCode, verifier: verifier, slowDown: slowDownWait}
	// MiniMax's own variant has no device_code: the user code is polled
	// for, and the end may be an absolute time in milliseconds.
	//
	// The field being there is what says so, and the user code has to
	// be there to be polled for. A field that is not there at all is
	// not a user-code poll, however the answer looks. A field that is
	// there but says zero says nothing: the reference reads it as
	// absent, and a zero end is not an end.
	byUser := deviceCode == "" && userCode != "" && b.ExpiredIn != nil && *b.ExpiredIn > 0
	switch {
	case byUser:
		d.pollField, d.pollValue = "user_code", userCode
	case deviceCode != "":
		d.pollField, d.pollValue = "device_code", deviceCode
	default:
		return nil, &Error{Code: "invalid_device_authorization_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
	}

	// The end of the code comes in two shapes: a lifetime (expires_in)
	// and a deadline (expired_in). The fallback is only for the case
	// where the site said nothing at all: a deadline that is already
	// in the past is a dead code, and waiting ten minutes for it would
	// be waiting for nothing.
	var deadline time.Time
	switch {
	case b.ExpiresIn > 0:
		deadline = time.Now().Add(expiresDuration(b.ExpiresIn))
	case b.ExpiredIn != nil && *b.ExpiredIn > 0:
		deadline, _ = siteDeadline(*b.ExpiredIn)
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(signInTimeout)
	}
	if !deadline.After(time.Now()) {
		return nil, &Error{Code: "expired_token", What: WhatSignIn}
	}
	d.deadline = deadline

	// The wait between two polls. The site's own variant says the
	// interval in milliseconds and the ordinary one in seconds, and
	// the reference reads each one in its own unit: 3000 is three
	// seconds on one path and three thousand milliseconds on the
	// other.
	interval := 5 * time.Second
	switch {
	case b.Interval <= 0:
		// 没有这个字段（或者是零）：五秒一轮。
	case byUser:
		interval = time.Duration(b.Interval) * time.Millisecond
	default:
		interval = pollInterval(b.Interval)
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
			return nil, &Error{Code: "expired_token", What: WhatSignIn}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if time.Now().After(d.deadline) {
			return nil, &Error{Code: "expired_token", What: WhatSignIn}
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
			return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
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
			wait += d.slowDownWait()
			continue
		}
		if !httpOK {
			return nil, oauthError(resp.StatusCode, body, WhatSignIn)
		}
		switch status {
		case "pending":
			continue
		case "slow_down":
			wait += d.slowDownWait()
			continue
		case "denied", "access_denied":
			return nil, &Error{Code: "access_denied", What: WhatSignIn}
		case "expired", "expired_token":
			return nil, &Error{Code: "expired_token", What: WhatSignIn}
		}
		if b.Error != "" {
			return nil, oauthError(resp.StatusCode, body, WhatSignIn)
		}
		if b.AccessToken == "" || b.ExpiresIn <= 0 {
			return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode, What: WhatSignIn}
		}
		if b.RefreshToken == "" {
			// 没有 refresh token 的账号活不过一小时：它再也换不到
			// 新的 access token 了。与其建一个这样的账号，不如让这次
			// 登录失败，用户再登一次。
			return nil, &Error{Code: "missing_refresh_token", HTTPStatus: resp.StatusCode, What: WhatSignIn}
		}
		tok := &Token{
			Access:  b.AccessToken,
			Refresh: b.RefreshToken,
			Expires: time.Now().Add(expiresDuration(b.ExpiresIn)),
		}
		tok.Subject, tok.AccountID, tok.HasClaims = claims(b.AccessToken)
		return tok, nil
	}
}

// slowDownWait is how much longer to wait when the site says slow
// down.
func (d *Device) slowDownWait() time.Duration {
	if d.slowDown > 0 {
		return d.slowDown
	}
	return slowDownWait
}

// claims reads the JWT's payload, when the access token is one.
//
// The claim names are guesses: nobody has shown what MiniMax puts in
// its access tokens, so "sub" and "account_id" may be the wrong names,
// and the claim may be missing from a token that is a JWT all the
// same. The answer says whether the payload could be read at all, so a
// caller can tell "not a JWT" from "a JWT without that claim". The
// names have to be checked against a real token before they are
// trusted.
//
// The signature is not checked either: the token is only read, never
// trusted for anything.
func claims(access string) (subject, accountID string, ok bool) {
	parts := strings.Split(access, ".")
	if len(parts) < 2 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// A payload may be padded, and a padded one is a JWT all the
		// same.
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", "", false
		}
	}
	var p struct {
		Sub       string `json:"sub"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", false
	}
	return p.Sub, p.AccountID, true
}

// oauthError reads an error answer.
//
// The text is taken from error_description, then description, then
// message: a 4xx that carries only "message" would lose its
// explanation otherwise, and that explanation is what the account's
// last error says.
func oauthError(status int, body []byte, what string) *Error {
	var b struct {
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
		Description string `json:"description"`
		Message     string `json:"message"`
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
	if msg == "" {
		msg = b.Message
	}
	return &Error{Code: code, HTTPStatus: status, Message: msg, What: what}
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

在 `internal/oauth/oauth_test.go` 末尾追加（import 补上 `"mmxcode/internal/upstream"`、`"sync"` 与 `"sync/atomic"`）：

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

	c := testClient(t)
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

	c := testClient(t)
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

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if !oe.IsSignedOut() {
		t.Fatal("expected the error to be a sign-out")
	}
}

func TestRefreshSignOutInA200Body(t *testing.T) {
	// MiniMax 把业务错误放在 200 的 body 里。签出也是这么来的。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if oe.HTTPStatus != 200 {
		t.Fatalf("error = %+v", oe)
	}
	if !oe.IsSignedOut() {
		t.Fatal("a sign-out in a 200 body was not a sign-out")
	}
}

func TestRefreshKeepsTheMessage(t *testing.T) {
	// 有的 4xx 只带 message：这个说明是账号的 last error。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_request","message":"the code is wrong"}`))
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "the code is wrong") {
		t.Fatalf("err = %v, want the message", err)
	}
	// 刷新的错误说的是 token 端点，不是登录服务器。
	if !strings.Contains(err.Error(), "token endpoint") {
		t.Fatalf("err = %v, want it to name the token endpoint", err)
	}
}

func TestRefreshWithALostAnswer(t *testing.T) {
	// 请求到了站点，答案没了：站点可能已经把 refresh token 用了。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
}

func TestRefreshWithARefusedConnection(t *testing.T) {
	// 连都没连上：请求肯定没到站点，同一个 token 再来一次是安全的。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := srv.URL + "/oauth2/token"
	srv.Close()

	c := testClient(t)
	_, err := Refresh(context.Background(), c, endpoint, "old-refresh")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

func TestRenewerSpendsOnceUnderConcurrency(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	current := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	var wg sync.WaitGroup
	var started sync.WaitGroup
	started.Add(8)
	results := make([]*Token, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started.Done()
			tok, err := r.Renew(context.Background(), "acc_01", current, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			results[i] = tok
		}(i)
	}
	started.Wait()
	// 让每个 goroutine 都进到 Renew 里去：它们都得等同一个刷新。
	time.Sleep(200 * time.Millisecond)
	close(release)
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

func TestRenewerWalksTheChainOnAfterALostStoreWrite(t *testing.T) {
	// 站点给了新 token，存库失败，库里还是那个已经用过的。
	// renewer 手上有新的：它得把它用掉，链才走得下去。
	var calls int32
	var mu sync.Mutex
	var spent []string
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		spent = append(spent, refresh)
		mu.Unlock()
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	first, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	// 存库失败：调用方手上还是那个旧 token。
	second, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the chain was walked %d times, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if spent[1] != "new-old-refresh" {
		t.Fatalf("the second spend used %q, want the token the renewer held", spent[1])
	}
	if second.Refresh == first.Refresh {
		t.Fatalf("the chain did not move on: %+v", second)
	}
}

func TestRenewerSpendsAgainWhenTheSiteDoesNotRotate(t *testing.T) {
	// 站点不给新的 refresh token：它没有把旧的用掉，
	// 所以下一次刷新还得再发一次。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{
			Access:  fmt.Sprintf("acc-%d", atomic.LoadInt32(&calls)),
			Refresh: refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "same-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was spent %d times, want 2", got)
	}
	if tok.Access == "old-access" {
		t.Fatalf("the token = %+v, want a new one", tok)
	}
}

func TestRenewerFollowsTheCallersNewerToken(t *testing.T) {
	// renewer 手上的 token 调用方不知道：调用方拿的是新的
	// （刚登录的，或者别的进程写进去的）。调用方说了算。
	var calls int32
	var mu sync.Mutex
	var spent []string
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		spent = append(spent, refresh)
		mu.Unlock()
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	newer := &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	tok, err := r.Renew(context.Background(), "acc_01", newer, spend)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spent) != 2 || spent[1] != "r2" {
		t.Fatalf("spent = %v, want the caller's token", spent)
	}
	if tok.Access != "acc-for-r2" {
		t.Fatalf("token = %+v, want the answer of the caller's own renewal", tok)
	}
}

func TestRenewerDoesNotMarkAFailedRefreshSpent(t *testing.T) {
	// 这个包的全部承诺：失败的刷新不把 token 记成用掉了。
	var calls int32
	var fail atomic.Bool
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return nil, errors.New("the network is down")
		}
		return &Token{
			Access:  "new-access",
			Refresh: "new-refresh",
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	fail.Store(true)
	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err == nil {
		t.Fatal("expected an error")
	}
	fail.Store(false)
	// 失败过的 token 还能再发：它多半没有到站点。
	tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was sent %d times, want 2", got)
	}
	if tok.Access != "new-access" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestRenewerDoesNotSendATokenOfUnknownOutcomeAgain(t *testing.T) {
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("the answer was lost: %w", ErrRefreshOutcomeUnknown)
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	_, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
	// 再来一次：答案不知道的 token 不再发。
	_, err = r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("err = %v, want an unknown outcome", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was sent %d times, want 1", got)
	}
}

func TestRenewerGivesEveryCallerItsOwnToken(t *testing.T) {
	// 两个调用方各拿一个 refresh token：各发各的，
	// 谁也不能拿别人的答案。
	var calls int32
	release := make(chan struct{})
	var mu sync.Mutex
	seen := map[string]bool{}
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		seen[refresh] = true
		mu.Unlock()
		<-release
		return &Token{
			Access:  "acc-for-" + refresh,
			Refresh: "new-" + refresh,
			Expires: time.Now().Add(time.Hour),
		}, nil
	}
	tokens := map[string]*Token{
		"r1": {Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)},
		"r2": {Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)},
	}
	results := map[string]*Token{}
	var wg sync.WaitGroup
	var mu2 sync.Mutex
	for name, tok := range tokens {
		wg.Add(1)
		go func(name string, tok *Token) {
			defer wg.Done()
			got, err := r.Renew(context.Background(), "acc_01", tok, spend)
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			mu2.Lock()
			results[name] = got
			mu2.Unlock()
		}(name, tok)
	}
	// 等两个刷新都开始了。
	for i := 0; i < 200 && atomic.LoadInt32(&calls) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !seen["r1"] || !seen["r2"] {
		t.Fatalf("spent = %v, want both tokens", seen)
	}
	for name, tok := range results {
		if tok == nil {
			t.Fatalf("%s got nothing", name)
		}
		if want := "acc-for-" + name; tok.Access != want {
			t.Fatalf("%s got %+v, want the answer of its own renewal", name, tok)
		}
	}
}

func TestRenewerLeaderCancellationDoesNotKillTheFollowers(t *testing.T) {
	// 第一个来的调用方断线了，别的调用方还在等：
	// 刷新不该跟着死。
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := r.Renew(leaderCtx, "acc_01", stored, spend)
		leaderErr <- err
	}()
	// 等刷新真的开始了。
	for i := 0; i < 200 && atomic.LoadInt32(&calls) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	followerErr := make(chan error, 1)
	followerTok := make(chan *Token, 1)
	go func() {
		tok, err := r.Renew(context.Background(), "acc_01", stored, spend)
		followerTok <- tok
		followerErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	// 第一个来的调用方走了。
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the leader got %v, want the cancellation", err)
	}
	close(release)
	if err := <-followerErr; err != nil {
		t.Fatalf("the follower got %v", err)
	}
	if tok := <-followerTok; tok == nil || tok.Access != "new-access" {
		t.Fatalf("the follower got %+v", tok)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
}

func TestRenewerForget(t *testing.T) {
	r := NewRenewer()
	var calls int32
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Newest("acc_01"); !ok {
		t.Fatal("the renewer holds nothing")
	}
	r.Forget("acc_01")
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token is still held after Forget")
	}
	// 已经用掉的记录也没了：这个账号签出了，它的东西都不作数了。
	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatalf("Renew after Forget: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("the refresh token was spent %d times, want 2", got)
	}
}

func TestRenewerForgetLeavesARunningRenewalAlone(t *testing.T) {
	// 账号正在签出时在跑的刷新，完了不能把 token 放回来。
	var calls int32
	release := make(chan struct{})
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Renew(context.Background(), "acc_01", stored, spend)
	}()
	for i := 0; i < 200 && atomic.LoadInt32(&calls) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	r.Forget("acc_01")
	close(release)
	<-done
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token came back from the dead")
	}
}

func TestRenewerReset(t *testing.T) {
	// Reset 把链忘掉，"用掉了"的记录还在。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	stored := &Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}

	if _, err := r.Renew(context.Background(), "acc_01", stored, spend); err != nil {
		t.Fatal(err)
	}
	r.Reset("acc_01")
	if _, ok := r.Newest("acc_01"); ok {
		t.Fatal("the token is still held after Reset")
	}
	// 用掉的 token 不能再发：链断了，账号得重新登录。
	_, err := r.Renew(context.Background(), "acc_01", stored, spend)
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", got)
	}
}

func TestRenewerChainBroken(t *testing.T) {
	// renewer 手上的 token 也用掉了：链断了，没有能走的路了。
	var calls int32
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		atomic.AddInt32(&calls, 1)
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	r.mu.Lock()
	r.latest["acc_01"] = &Token{Access: "a2", Refresh: "r2", Expires: time.Now().Add(time.Hour)}
	r.spent["acc_01"] = map[string]bool{
		tokenHash("r1"): true,
		tokenHash("r2"): true,
	}
	r.mu.Unlock()

	_, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Expires: time.Now().Add(time.Hour)}, spend)
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("a spent token was sent %d times", got)
	}
}

func TestRenewerRefusesAnotherAccountsToken(t *testing.T) {
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		return &Token{
			Access:  "new-access",
			Refresh: "new-refresh",
			Expires: time.Now().Add(time.Hour),
			Subject: "user-1",
		}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a1", Refresh: "r1", Subject: "user-1", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	_, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "a9", Refresh: "r9", Subject: "user-2", Expires: time.Now().Add(time.Hour)}, spend)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "another account") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenewerNewestGivesACopy(t *testing.T) {
	r := NewRenewer()
	spend := func(ctx context.Context, refresh string) (*Token, error) {
		return &Token{Access: "new-access", Refresh: "new-refresh", Expires: time.Now().Add(time.Hour)}, nil
	}
	if _, err := r.Renew(context.Background(), "acc_01",
		&Token{Access: "old-access", Refresh: "old-refresh", Expires: time.Now().Add(time.Hour)}, spend); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Newest("acc_01")
	if !ok || got.Access != "new-access" {
		t.Fatalf("newest = %+v, ok = %v", got, ok)
	}
	got.Access = "changed"
	again, _ := r.Newest("acc_01")
	if again.Access == "changed" {
		t.Fatal("the caller changed what the renewer holds")
	}
	// 没有 token 时 Latest 给的是调用方自己的。
	empty := NewRenewer()
	cur := &Token{Access: "mine"}
	if got := empty.Latest("acc_02", cur); got != cur {
		t.Fatalf("latest = %+v, want the caller's own token", got)
	}
	if _, ok := empty.Newest("acc_02"); ok {
		t.Fatal("the renewer holds a token it does not have")
	}
}

func TestNoSecretIsInAnyErrorOrURL(t *testing.T) {
	const secret = "the-secret-refresh-token"
	const verifier = "the-secret-pkce-verifier"

	// 刷新的错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","message":"no"}`))
	}))
	defer srv.Close()
	c := testClient(t)
	_, err := Refresh(context.Background(), c, srv.URL+"/oauth2/token", secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}

	// renewer 的错误。
	r := NewRenewer()
	_, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return nil, errors.New("nope")
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}
	// 链断了的错误也不带 token。
	// 先成功一次：那个 token 就用掉了。
	if _, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return &Token{Access: "b", Refresh: "other", Expires: time.Now().Add(time.Hour)}, nil
		}); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	r.Reset("acc_01")
	_, err = r.Renew(context.Background(), "acc_01",
		&Token{Access: "a", Refresh: secret},
		func(ctx context.Context, refresh string) (*Token, error) {
			return &Token{Access: "b", Refresh: "other", Expires: time.Now().Add(time.Hour)}, nil
		})
	if !errors.Is(err, ErrRefreshChainBroken) {
		t.Fatalf("err = %v, want a broken chain", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}

	// 设备码的错误和地址。
	d := &Device{
		UserCode: "ABCD",
		URL:      "https://example.invalid/activate?code=ABCD",
		verifier: verifier,
	}
	for _, text := range []string{d.String(), fmt.Sprintf("%+v", d), d.URL} {
		if strings.Contains(text, verifier) {
			t.Fatalf("the verifier is in %q", text)
		}
	}
}
```

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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// ErrRefreshOutcomeUnknown says a refresh may have been spent.
//
// The request reached the site, or may have, and the answer was lost on
// the way back. The site may have spent the refresh token already, and
// sending it again would be answered with invalid_grant, which signs
// the account out. So a token whose outcome is unknown is not sent
// again.
var ErrRefreshOutcomeUnknown = errors.New("the outcome of the refresh is not known")

// ErrRefreshChainBroken says the renewal chain cannot go on.
//
// The chain is the run of refresh tokens one account walks: each token
// is spent once and buys the next. The chain breaks when the token at
// hand was spent already and there is no newer one to walk from, and
// when the account was signed out while a renewal was running. No
// token is left that the site would take, so the account has to be
// signed in again by hand.
var ErrRefreshChainBroken = errors.New("the refresh chain is broken")

// spendTimeout is how long a renewal may take.
//
// The renewal runs under a context of its own, so that the cancellation
// of one caller does not kill the renewal the other callers wait for.
// The deadline is what stops it from running forever.
const spendTimeout = 30 * time.Second

// Refresh trades a refresh token for a new access token.
//
// The caller's refresh token is kept when the answer gives no new one.
//
// The failure says which kind it is. A failure before the request left
// — a name that does not resolve, a connection that is refused — is
// safe to try again with the same token. A failure after the request
// was written — a read that times out, a connection that is cut —
// leaves the answer unknown: the site may have spent the token
// already. That error is ErrRefreshOutcomeUnknown, and the token is
// not sent again.
func Refresh(ctx context.Context, c *upstream.Client, endpoint, refresh string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", site.ClientID)
	form.Set("scope", site.Scope)
	form.Set("audience", site.Audience)

	resp, err := c.PostForm(ctx, endpoint, form)
	if err != nil {
		if requestNeverLeft(err) {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		return nil, &UnknownOutcomeError{Err: err}
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		// The site answered and the answer could not be read. The
		// token may have been spent all the same.
		return nil, &UnknownOutcomeError{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, oauthError(resp.StatusCode, body, WhatRefresh)
	}
	var b struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, &Error{Code: "invalid_json_response", HTTPStatus: resp.StatusCode, What: WhatRefresh}
	}
	if b.Error != "" {
		return nil, oauthError(resp.StatusCode, body, WhatRefresh)
	}
	if b.AccessToken == "" || b.ExpiresIn <= 0 {
		return nil, &Error{Code: "invalid_token_response", HTTPStatus: resp.StatusCode, What: WhatRefresh}
	}
	newRefresh := b.RefreshToken
	if newRefresh == "" {
		newRefresh = refresh
	}
	tok := &Token{
		Access:  b.AccessToken,
		Refresh: newRefresh,
		Expires: time.Now().Add(expiresDuration(b.ExpiresIn)),
	}
	tok.Subject, tok.AccountID, tok.HasClaims = claims(b.AccessToken)
	return tok, nil
}

// UnknownOutcomeError is a refresh whose answer was lost.
//
// The request reached the site, or may have, and the answer was lost on
// the way back. The site may have spent the refresh token, so the
// token must not be sent again. errors.Is says so: the error is
// ErrRefreshOutcomeUnknown.
type UnknownOutcomeError struct {
	// Err is what the transport said.
	Err error
}

// Error is the error's text.
func (e *UnknownOutcomeError) Error() string {
	return fmt.Sprintf("the refresh answer was lost: %v", e.Err)
}

// Unwrap gives the transport's error and ErrRefreshOutcomeUnknown.
func (e *UnknownOutcomeError) Unwrap() []error {
	return []error{e.Err, ErrRefreshOutcomeUnknown}
}

// requestNeverLeft says the request provably did not reach the site.
//
// A name that does not resolve and a connection that is refused are
// the two failures that happen before anything is written: the site
// cannot have spent a token it never heard of. Everything else — a
// read that times out, a connection that is cut halfway — leaves the
// answer unknown.
func requestNeverLeft(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

// SpendFunc spends a refresh token and gives what it bought.
type SpendFunc func(ctx context.Context, refresh string) (*Token, error)

// Renewer spends refresh tokens, once each.
//
// MiniMax spends a refresh token once: sent a second time it is
// invalid_grant, which signs the account out. So a refresh token that
// was already spent is never sent again from here, whatever the
// concurrency. A refresh that failed for a network reason is the one
// exception: it may not have reached the site, so it is sent again.
//
// The renewer keeps, per account, the newest token it got, the refresh
// tokens already spent, and the refresh tokens whose outcome is not
// known. A request that comes while a renewal is running waits for it
// and uses what it gave.
type Renewer struct {
	mu       sync.Mutex
	inflight map[string]*renewCall
	latest   map[string]*Token
	// spent holds, per account, the hashes of the refresh tokens
	// already spent. A token in here is never sent again.
	//
	// The tokens are remembered by their hash and not by themselves: a
	// refresh token is a secret, and a hash keeps no secret in memory
	// longer than it has to be there. The set grows with every
	// renewal, which is what makes it right: a set that kept only the
	// last few would let an old token be sent again.
	spent map[string]map[string]bool
	// unknown holds, per account, the hashes of the refresh tokens
	// whose outcome is not known. The site may have spent them, so
	// they are not sent again either.
	unknown map[string]map[string]bool
	// gen is the generation of each account's chain. A renewal takes
	// the generation it started in and writes nothing when it has
	// moved on, so a renewal that was running when the account was
	// signed out does not bring it back.
	gen map[string]uint64
}

// renewCall is one renewal in flight.
type renewCall struct {
	done chan struct{}
	// token and err are what the renewal gave.
	token *Token
	err   error
	// accountID is the account the renewal is for.
	accountID string
	// refresh is the refresh token the renewal spends.
	refresh string
	// gen is the generation the renewal started in.
	gen uint64
}

// NewRenewer makes a renewer.
func NewRenewer() *Renewer {
	return &Renewer{
		inflight: map[string]*renewCall{},
		latest:   map[string]*Token{},
		spent:    map[string]map[string]bool{},
		unknown:  map[string]map[string]bool{},
		gen:      map[string]uint64{},
	}
}

// Renew gives an account a fresh token, spending the refresh token at
// hand once.
//
// When the renewal is already running for the account and the token,
// the caller waits for it and takes what it gave. When the refresh
// token was already spent, the chain goes on from the newest token this
// renewer holds: that is the only way past a store that did not get
// the last token, and without it the chain never advances on its own.
//
// When the renewer does not know the token the caller holds, the
// caller has a newer one — a fresh sign-in, or a token another process
// wrote — so the caller is the one who is right and what this renewer
// holds is dropped.
//
// When the chain cannot go on at all, the error is
// ErrRefreshChainBroken, and the caller has to sign the account in
// again by hand.
func (r *Renewer) Renew(ctx context.Context, accountID string, current *Token, spend SpendFunc) (*Token, error) {
	if current == nil {
		return nil, fmt.Errorf("no token to renew")
	}
	if current.Refresh == "" {
		return nil, fmt.Errorf("no refresh token to renew")
	}
	if spend == nil {
		return nil, fmt.Errorf("no way to spend the refresh token")
	}

	key := tokenHash(current.Refresh)

	r.mu.Lock()
	// A renewal with this token is already running for this account:
	// wait for it rather than sending the token twice.
	if c, ok := r.inflight[flightKey(accountID, key)]; ok {
		r.mu.Unlock()
		return waitFor(ctx, c)
	}
	// The outcome of the last renewal with this token is not known:
	// the site may have spent it. It is not sent again.
	if r.unknown[accountID][key] {
		r.mu.Unlock()
		return nil, fmt.Errorf("the outcome of the last refresh with this token is not known, so it is not sent again: %w", ErrRefreshOutcomeUnknown)
	}
	head := r.latest[accountID]
	if head != nil && head.Subject != "" && current.Subject != "" && head.Subject != current.Subject {
		// The token is of another account: renewing it here would put
		// its token under this account.
		r.mu.Unlock()
		return nil, fmt.Errorf("the token belongs to another account")
	}
	switch {
	case r.spent[accountID][key]:
		// The caller's token was spent already. The chain goes on from
		// the newest token this renewer holds.
		if head == nil || head.Refresh == current.Refresh {
			r.mu.Unlock()
			return nil, fmt.Errorf("the refresh token was already spent and no newer token is held: %w", ErrRefreshChainBroken)
		}
		headKey := tokenHash(head.Refresh)
		if r.spent[accountID][headKey] || r.unknown[accountID][headKey] {
			r.mu.Unlock()
			return nil, fmt.Errorf("the refresh token was already spent and no newer token is held: %w", ErrRefreshChainBroken)
		}
		current, key = head, headKey
	case head == nil || head.Refresh != current.Refresh:
		// The renewer does not know the caller's token, so the caller
		// has a newer one. The caller is the one who is right, so what
		// this renewer holds is dropped.
		delete(r.latest, accountID)
	}

	// The flight takes the account's chain over: any renewal running
	// for this account is left behind, and its answer does not become
	// the chain's.
	r.gen[accountID]++
	c := &renewCall{
		done:      make(chan struct{}),
		accountID: accountID,
		refresh:   current.Refresh,
		gen:       r.gen[accountID],
	}
	r.inflight[flightKey(accountID, key)] = c
	r.mu.Unlock()

	go r.run(ctx, c, spend)
	return waitFor(ctx, c)
}

// run spends a refresh token and puts what it bought into the renewer.
//
// The spend runs under a context of its own: the caller's cancellation
// must not kill the renewal that every other caller is waiting for.
// The caller's context is only for waiting.
func (r *Renewer) run(ctx context.Context, c *renewCall, spend SpendFunc) {
	spendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spendTimeout)
	defer cancel()

	tok, err := spendSafely(spend, spendCtx, c.refresh)

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, flightKey(c.accountID, tokenHash(c.refresh)))
	if err == nil && tok != nil {
		// The site took the token, whatever happens to the chain now,
		// so it is never sent again. A site that does not rotate the
		// refresh token is the exception: it hands the same token
		// back, so it did not take it.
		if tok.Refresh != c.refresh {
			r.markSpent(c.accountID, c.refresh)
		}
		if r.gen[c.accountID] == c.gen {
			// A copy: the caller may change what it holds, and what
			// the renewer holds is not the caller's to change.
			held := *tok
			r.latest[c.accountID] = &held
		}
	}
	if err != nil && errors.Is(err, ErrRefreshOutcomeUnknown) {
		// The site may have taken the token. It is not sent again.
		r.markUnknown(c.accountID, c.refresh)
	}
	c.token, c.err = tok, err
	close(c.done)
}

// spendSafely spends a refresh token, and a panic is an error like any
// other.
//
// The spend runs in a goroutine of its own, and a panic there would
// take the whole process down with it.
func spendSafely(spend SpendFunc, ctx context.Context, refresh string) (tok *Token, err error) {
	defer func() {
		if p := recover(); p != nil {
			tok, err = nil, fmt.Errorf("refresh: %v", p)
		}
	}()
	return spend(ctx, refresh)
}

// waitFor waits for a renewal to finish.
//
// The caller's context is only for waiting: the renewal runs under a
// context of its own, so a caller that goes away does not kill the
// renewal the other callers are waiting for.
func waitFor(ctx context.Context, c *renewCall) (*Token, error) {
	select {
	case <-c.done:
		return c.token, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Latest gives the newest token this renewer holds for an account.
//
// The renewer keeps only the newest one per account, so this is that
// one, or the caller's own when there is none.
//
// Newest says the same thing without the guessing: it says whether
// there is one.
func (r *Renewer) Latest(accountID string, current *Token) *Token {
	tok, ok := r.Newest(accountID)
	if ok {
		return tok
	}
	return current
}

// Newest gives the newest token this renewer holds for an account.
//
// The second answer says whether there is one: without it a caller
// cannot tell "the renewer holds nothing" from "the renewer holds the
// token you already have". The token is a copy, so the caller cannot
// change what the renewer holds.
func (r *Renewer) Newest(accountID string) (*Token, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok := r.latest[accountID]
	if tok == nil {
		return nil, false
	}
	c := *tok
	return &c, true
}

// Forget drops an account's tokens, for when it is signed out.
//
// Everything about the account goes: the newest token, the record of
// the tokens already spent, and the renewal that may be running. A
// renewal that is running when the account is signed out is abandoned:
// its answer is not written back, so a token does not come back from
// the dead.
//
// A renewal that gets a token while the account is being signed out
// still has that token spent at the site, so the record of it stays:
// a token the site took is never sent again.
func (r *Renewer) Forget(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.latest, accountID)
	delete(r.spent, accountID)
	delete(r.unknown, accountID)
	r.gen[accountID]++
}

// Reset forgets an account's chain, so that the next renewal walks it
// again from the token the caller holds.
//
// The tokens already spent stay spent: a token the site took must never
// be sent again, whatever happens to the chain around it. So Reset is
// for "the chain I was walking is wrong, start again from here", and
// not for "this account is signed out" — that is Forget.
//
// A renewal that is running is abandoned, like in Forget: its answer
// belongs to the chain that was walked, not to the new one.
func (r *Renewer) Reset(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.latest, accountID)
	r.gen[accountID]++
}

// markSpent remembers that a refresh token was spent.
func (r *Renewer) markSpent(accountID, refresh string) {
	set := r.spent[accountID]
	if set == nil {
		set = map[string]bool{}
		r.spent[accountID] = set
	}
	set[tokenHash(refresh)] = true
}

// markUnknown remembers that the outcome of a refresh is not known.
func (r *Renewer) markUnknown(accountID, refresh string) {
	set := r.unknown[accountID]
	if set == nil {
		set = map[string]bool{}
		r.unknown[accountID] = set
	}
	set[tokenHash(refresh)] = true
}

// tokenHash is what a refresh token is remembered by.
//
// The tokens are remembered by their hash and not by themselves: a
// refresh token is a secret, and a hash keeps no secret in memory
// longer than it has to be there. The hash is of the token alone, so a
// hash of one account's token says nothing about another's.
func tokenHash(refresh string) string {
	sum := sha256.Sum256([]byte(refresh))
	return hex.EncodeToString(sum[:])
}

// flightKey says which renewal a request belongs to.
//
// The key is the account and the token, not the account alone: a
// caller that holds another token has another renewal to make, and
// giving it the answer of somebody else's renewal loses its token.
func flightKey(accountID, key string) string {
	return accountID + "\x00" + key
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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

func testClient(t *testing.T) *upstream.Client {
	t.Helper()
	return upstream.New(upstream.Options{Timeout: 5 * time.Second})
}

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

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
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

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	if _, err := c.Identity(context.Background(), "tok", ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLedgerPaging(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page_number")
		pages = append(pages, page)
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

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
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
	// 页面要一页一页地问，跳过第 2 页的 bug 不能混过去
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("pages = %v, want [1 2]", pages)
	}
}

// TestLedgerUnreadableAmount says the credits cannot be read.
//
// The reference does not answer a part of the ledger: a batch whose
// amount is not a number would tell a wrong share.
func TestLedgerUnreadableAmount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":1,"details":[
			{"credit_type":1,"granted_amount":"abc","consumed_amount":"0","remaining_amount":"1100.5","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if batches != nil {
		t.Fatalf("batches = %+v, want none", batches)
	}
}

// TestLedgerMissingExpiry says the time a batch runs out is not there.
//
// It is not the same as a batch that ran out: the credits of such a
// batch cannot be read, and the batch must not be called expired.
func TestLedgerMissingExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":1,"details":[
			{"credit_type":1,"granted_amount":"600","consumed_amount":"100","remaining_amount":"500","granted_at_ms":1763000000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("got %d batches", len(batches))
	}
	b := batches[0]
	if b.HasExpiresAt {
		t.Fatalf("batch = %+v, want no expiry", b)
	}
	if !b.ExpiresAt.IsZero() {
		t.Fatalf("expires at = %v, want the zero time", b.ExpiresAt)
	}
	if b.Granted != 600 || b.Remaining != 500 {
		t.Fatalf("batch = %+v", b)
	}
}

// TestLedgerExpireAtZero says a batch that expires at zero has expired.
//
// The time is there, and it is in the past: that is a spent batch, not
// an unreadable one.
func TestLedgerExpireAtZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":1,"details":[
			{"credit_type":1,"granted_amount":"600","consumed_amount":"0","remaining_amount":"600","expire_at_ms":0}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("got %d batches", len(batches))
	}
	b := batches[0]
	if !b.HasExpiresAt {
		t.Fatalf("batch = %+v, want an expiry", b)
	}
	if b.ExpiresAt.IsZero() {
		t.Fatalf("expires at = %v, want the epoch", b.ExpiresAt)
	}
}

// TestLedgerEmptyIsNotUnreadable says a ledger that was read and is
// empty is not a ledger that could not be read.
//
// 读到空账本是“没有积分”，读不到账本是“不知道”：两回事。
func TestLedgerEmptyIsNotUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":0,"details":[]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	batches, err := c.Ledger(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if batches == nil {
		t.Fatal("an empty ledger must not look like one that could not be read")
	}
	if len(batches) != 0 {
		t.Fatalf("got %d batches, want none", len(batches))
	}
}

func TestLedgerBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
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

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	// status 2 是用完了，所以是 100%
	if ws[0].Used != 100 {
		t.Fatalf("interval used = %v, want 100", ws[0].Used)
	}
	if ws[0].Amount != 150 || ws[0].Limit != 150 {
		t.Fatalf("window = %+v", ws[0])
	}
}

// TestPlanWindowsFallsBackToTheOpenPlatform says the open platform's
// answer is used when the console's own answer has no windows in it.
//
// 控制台自己的 remains_percent 在国际站上可能回一个空的 model_remains。
// 参考实现这时候去问 coding_plan/remains，Go 代码原来直接说"没有窗口"：
// 账号的额度用完了却看不出来，故障转移一直挑它。
func TestPlanWindowsFallsBackToTheOpenPlatform(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
		case site.CodingPlanPath:
			w.Write([]byte(`{"model_remains":[
				{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
				 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
				 "start_time":1763000000000,"end_time":1763010000000,
				 "current_weekly_status":2,"current_weekly_remaining_percent":80,
				 "current_weekly_total_percent":100,"weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if ws[0].Amount != 150 || ws[0].Limit != 150 {
		t.Fatalf("window = %+v", ws[0])
	}
	want := []string{site.PlanRemainsPath, site.CodingPlanPath}
	if len(asked) != len(want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
}

// TestPlanWindowsEmpty says a plan with no windows at all is no
// windows, and not an error.
//
// 两个端点都没有窗口的时候，答案是"没有窗口"，不是"读不出来"。
func TestPlanWindowsEmpty(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("got %+v", ws)
	}
	want := []string{site.PlanRemainsPath, site.CodingPlanPath}
	if len(asked) != len(want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
}

// TestTheWindowEndIsReadInSecondsToo says a window's end is read the
// way its length is read.
//
// 站点写时间不说是秒还是毫秒：一万亿以下是秒。窗口的结束时间要是按
// 毫秒读，一个秒的时间就变成了 1970 年 —— 窗口早就"回来了"，于是
// 用完的账号一次又一次被挑中。
func TestTheWindowEndIsReadInSecondsToo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
			{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
			 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
			 "start_time":1763000000,"end_time":1763010800,
			 "current_weekly_status":2,"current_weekly_remaining_percent":80,
			 "current_weekly_total_percent":100,"weekly_start_time":1762800000,"weekly_end_time":1763400000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if got := ws[0].ResetsAt.UnixMilli(); got != 1763010800000 {
		t.Fatalf("the window resets at %d, want 1763010800000", got)
	}
	// 长度也一样：三个小时（秒写和毫秒写都是三个小时）
	if ws[0].Name != "3 hours" {
		t.Fatalf("window = %+v, want the three hour window", ws[0])
	}
}

// TestPlanWindowsKeepsTheConsoleAnswerWhenItsListIsNotEmpty says a
// list that is not empty is the console's answer, even when every entry
// in it is a bucket the plan does not have.
//
// 参考实现看的是回答里有没有那个列表，不是我们能从列表里做出几个
// 窗口。列表在，答案就在：这时这个套餐就是没有窗口，那是答案，不是
// 再问一次的理由。
func TestPlanWindowsKeepsTheConsoleAnswerWhenItsListIsNotEmpty(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case site.PlanRemainsPath:
			// 一个套餐没有的桶：两个窗口都是无限的、两个都没有计数。
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":3,"current_weekly_status":3,
				 "current_interval_total_count":0,"current_weekly_total_count":0}
			]}`))
		default:
			t.Errorf("the open platform was asked: %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("got %+v, want no windows", ws)
	}
	if len(asked) != 1 || asked[0] != site.PlanRemainsPath {
		t.Fatalf("asked %v, want only the console", asked)
	}
}

// TestWindowName says a window is named after its own length.
//
// 窗口的名字是它自己的长度：三小时的窗口叫 "3 hours"，不是写死的
// "5 hours"。一周的窗口叫 "7 days"，没有长度的窗口叫 "Allowance"。
func TestWindowName(t *testing.T) {
	cases := []struct {
		name   string
		weekly bool
		span   time.Duration
		want   string
	}{
		{"three hours", false, 3 * time.Hour, "3 hours"},
		{"five hours", false, 5 * time.Hour, "5 hours"},
		{"one hour", false, time.Hour, "1 hours"},
		{"a day", false, 24 * time.Hour, "24 hours"},
		{"two days", false, 48 * time.Hour, "2 days"},
		{"forty five minutes", false, 45 * time.Minute, "45 minutes"},
		{"a week", true, 7 * 24 * time.Hour, "7 days"},
		{"a week without a span", true, 0, "7 days"},
		{"no span", false, 0, "Allowance"},
	}
	for _, c := range cases {
		got := windowName(c.weekly, c.span)
		if got != c.want {
			t.Errorf("windowName(%v, %v) = %q, want %q", c.weekly, c.span, got, c.want)
		}
	}
}

// TestPlanWindowsNameTheWindowByItsLength says the name of a window
// comes from the answer.
//
// 站点给三小时的窗口，名字就该是 "3 hours"。
func TestPlanWindowsNameTheWindowByItsLength(t *testing.T) {
	// 三小时：开始 1763000000000，结束 1763010800000。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case site.PlanRemainsPath:
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":10,
				 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24,
				 "start_time":1763000000000,"end_time":1763010800000,
				 "current_weekly_status":2,"current_weekly_remaining_percent":80,
				 "current_weekly_total_percent":100,"weekly_start_time":1762800000000,"weekly_end_time":1763400000000}
			]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if ws[0].Name != "3 hours" {
		t.Errorf("the window is called %q, want %q", ws[0].Name, "3 hours")
	}
	if ws[1].Name != "7 days" {
		t.Errorf("the weekly window is called %q, want %q", ws[1].Name, "7 days")
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

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
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

// TestMembership says the plan is read from the answer's data.
//
// 真的答案把字段放在 data 下面，只读顶层的话一个真的套餐会被说成没有。
func TestMembership(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/matrix/api/v1/commerce/get_membership_info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":{"has_token_plan":true,"token_plan_tier":"TokenPlanMax","op_group_id":"grp-1","op_credit_summary":{"total_remaining_amount":"1234.5"}}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Known {
		t.Fatal("the plan is unknown")
	}
	if !m.HasTokenPlan {
		t.Fatalf("membership = %+v, want a plan", m)
	}
	if m.Tier != "TokenPlanMax" {
		t.Fatalf("tier = %q", m.Tier)
	}
	if m.GroupID != "grp-1" {
		t.Fatalf("group = %q", m.GroupID)
	}
	if m.Balance != "1234.5" {
		t.Fatalf("balance = %q", m.Balance)
	}
}

// TestMembershipTopLevelWins says the top level wins over the data.
//
// 顶层的字段优先，data 里的同名字段只在顶层没有的时候用。
func TestMembershipTopLevelWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"has_token_plan":false,"token_plan_tier":"Top","op_group_id":"grp-top","data":{"has_token_plan":true,"token_plan_tier":"Under","op_group_id":"grp-under"}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if m.HasTokenPlan {
		t.Fatalf("membership = %+v, want no plan", m)
	}
	if m.Tier != "Top" || m.GroupID != "grp-top" {
		t.Fatalf("membership = %+v", m)
	}
}

// TestMembershipOpcreditBalance says the balance is the account's own.
//
// 没有 op_credit_summary 时用 opcredit_balance，字符串和数字都认。
func TestMembershipOpcreditBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"has_token_plan":true,"opcredit_balance":" 42.5 "}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if m.Balance != "42.5" {
		t.Fatalf("balance = %q, want 42.5", m.Balance)
	}
}

// TestMembershipNotFound says the plan is unknown, not that there is none.
//
// 国际站对会员接口回 404，那是“不知道”，不是“没有”。
func TestMembershipNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`404 page not found`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	m, err := c.Membership(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if m.Known {
		t.Fatalf("membership = %+v, want an unknown plan", m)
	}
	if m.HasTokenPlan {
		t.Fatalf("membership = %+v, want no plan", m)
	}
}

// TestPlanWindowsNegativeCount says a negative count is not a count.
//
// 负数的已用次数不是次数，不能拿它当金额。
func TestPlanWindowsNegativeCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
			{"model_name":"general","current_interval_status":1,"current_interval_remaining_percent":90,
			 "current_interval_total_percent":100,"current_interval_total_count":10,"current_interval_used_count":-5,
			 "start_time":1763000000000,"end_time":1763010000000}
		]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ws, err := c.PlanWindows(context.Background(), "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
	}
	if ws[0].Amount != 0 || ws[0].Limit != 0 {
		t.Fatalf("window = %+v, want no count", ws[0])
	}
}

// TestPlanWindowsEmptyStatusMsg says the error names the status code.
//
// 回答没有 status_msg 时，错误里至少要有一个状态码，不能是空字符串。
func TestPlanWindowsEmptyStatusMsg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base_resp":{"status_code":1001,"status_msg":""}}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	_, err := c.PlanWindows(context.Background(), "tok", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() == "" {
		t.Fatalf("error = %q, want a text", err.Error())
	}
	if !strings.Contains(err.Error(), "1001") {
		t.Fatalf("error = %q, want the status code", err.Error())
	}
}

// TestCallPostNilBody says the body on the wire is what is signed.
//
// 签名算的是 "{}"，发出去的也得是 "{}"，不然上游会拒。
func TestCallPostNilBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	if _, err := c.Call(context.Background(), "tok", "1234567890", http.MethodPost, "/v1/whatever", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "{}" {
		t.Fatalf("body = %q, want {}", got)
	}
}

// TestCallGetHasNoBody says a GET signs and sends nothing.
func TestCallGetHasNoBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	if _, err := c.Call(context.Background(), "tok", "1234567890", http.MethodGet, "/v1/whatever", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("body = %q, want nothing", got)
	}
}

// TestModelsLive says the live list is asked for with the token.
func TestModelsLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mavis/api/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q", got)
		}
		w.Write([]byte(`{"providers":[{"providerId":"minimax","config":{"models":{"MiniMax-M3.1":{"name":"M3.1","limit":{"context":1048576,"output":524288},"modalities":{"input":["text","image"]},"reasoning":true,"thinking":{"effortOptions":["low","high","none"]}}},"model_order":["MiniMax-M3.1"]}}]}`))
	}))
	defer srv.Close()

	c := New(testClient(t))
	c.SetBase(srv.URL, srv.URL)
	ms := c.Models("tok")
	if len(ms) != 1 {
		t.Fatalf("got %d models: %+v", len(ms), ms)
	}
	if ms[0].ID != "MiniMax-M3.1" || ms[0].Name != "M3.1" {
		t.Fatalf("model = %+v", ms[0])
	}
	if ms[0].Context != 1048576 || ms[0].Output != 524288 {
		t.Fatalf("model = %+v", ms[0])
	}
	if len(ms[0].Efforts) != 2 {
		t.Fatalf("efforts = %v, want two", ms[0].Efforts)
	}
}
```

测试文件的 import 补上 `"mmxcode/internal/upstream"`，并加助手：

```go
func testClient(t *testing.T) *upstream.Client {
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
	"bytes"
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

// modelsTimeout is how long the live model list is waited for.
//
// The reference gives the call ten seconds; the console's endpoints are
// not documented, so a stalled one gets little of that.
const modelsTimeout = 10 * time.Second

// The credit types the plan counts.
//
// The ledger writes them as plain numbers, and the plan counts only
// these two: what was bought and what a check-in gave. Another type is
// a bucket the plan does not count.
const (
	// CreditTypePurchase is a batch that was bought.
	CreditTypePurchase = 1
	// CreditTypeCheckin is a batch that a check-in gave.
	CreditTypeCheckin = 2
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
	// CreditType is CreditTypePurchase or CreditTypeCheckin for the
	// batches the plan counts. Another type is a bucket the plan does
	// not count.
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
	// HasGrantedAt says the answer carried the time the batch was
	// given. A batch without one has no GrantedAt, not one in 1970.
	HasGrantedAt bool `json:"-"`
	// HasExpiresAt says the answer carried the time the batch runs
	// out. A batch without one cannot be read: the credits of a batch
	// that never expires cannot be told from those of a spent one, and
	// a wrong share is worse than none.
	HasExpiresAt bool `json:"-"`
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

// Sender sends a request.
//
// It is an interface so that an account with its own proxy can have
// every one of its calls go out through it: the caller hands over the
// sender that belongs to the account.
type Sender interface {
	Do(req *http.Request) (*http.Response, error)
}

// nowOffset gives the timezone offset in seconds east of UTC.
//
// It is the value the reference sends as getTimezoneOffset() * -60.
// time.Time has no Offset() method: Zone()'s second value is it.
func nowOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// Client reads the account API.
type Client struct {
	up Sender
	// base is the host the account API is on.
	base string
	// platform is the host the plan console is on.
	platform string
	// now gives the time; a test can set it.
	now func() time.Time
	// tzOffset is the client's timezone offset in seconds.
	tzOffset int
	// osName is the client's platform name.
	osName string
}

// New makes a client.
func New(up Sender) *Client {
	return &Client{
		up:       up,
		base:     site.AgentHost,
		platform: site.PlatformHost,
		now:      time.Now,
		// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
		// 和 Zone() 给的偏移是同一个值。time.Time 没有 Offset()
		// 方法，Zone() 的第二个返回值就是它。
		tzOffset: nowOffset(),
		osName:   runtime.GOOS,
	}
}

// SetBase sends the calls to other hosts, for a test.
//
// The account API and the plan console live on two hosts, so both are
// given. Without this the tests talk to MiniMax itself, and fail
// wherever the site is out of reach.
func (c *Client) SetBase(base, platform string) {
	c.base = base
	c.platform = platform
}

// With gives a copy of the client that sends through another sender.
//
// Every fetch an account makes goes through the account's own way out,
// or the global one. An account behind a proxy that is not used for
// its usage reading is an account whose reading never arrives.
func (c *Client) With(up Sender) *Client {
	// The hosts go with it: a client made for an account's own exit
	// must still call the same hosts, or the request goes nowhere.
	return &Client{up: up, base: c.base, platform: c.platform, now: c.now, tzOffset: c.tzOffset, osName: c.osName}
}

// SetClock replaces the clock, for a test.
func (c *Client) SetClock(now func() time.Time, tzOffset int) {
	c.now = now
	c.tzOffset = tzOffset
}

// CheckinQuery is the query MiniMax Code's own check-in sends.
//
// It is the site's one, in the shape the signing package takes. The
// check-in and the credits ledger ask the same way, and the copy is
// kept in one place: the site's.
func CheckinQuery() []QueryKV {
	out := make([]QueryKV, 0, len(site.CheckinQuery))
	for _, kv := range site.CheckinQuery {
		out = append(out, QueryKV{K: kv.K, V: kv.V})
	}
	return out
}

// Identity reads who the account is.
func (c *Client) Identity(ctx context.Context, access, userID string) (Identity, error) {
	body, err := c.Call(ctx, access, userID, http.MethodGet, site.IdentityPath, nil, nil)
	if err != nil {
		return Identity{}, err
	}
	var env struct {
		Data struct {
			UserInfo struct {
				RealUserID  string `json:"realUserID"`
				RealUserID2 string `json:"real_user_id"`
				UserEmail   string `json:"userEmail"`
				Email       string `json:"email"`
				UserMail    string `json:"userMail"`
				UserEmail2  string `json:"user_email"`
				Name        string `json:"name"`
				UserName    string `json:"userName"`
				UserName2   string `json:"user_name"`
			} `json:"userInfo"`
		} `json:"data"`
		UserInfo *struct {
			RealUserID string `json:"realUserID"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Identity{}, fmt.Errorf("account: not JSON: %w", err)
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
//
// The answer's fields are read from the top level first and from the
// object under "data" second, the way the reference reads them: a
// field at the top level wins, and one that is only under "data" is
// used when the top level does not carry it. The real answer nests
// them under "data", so a plan that is only there is a plan.
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

func (c *Client) membershipOnce(ctx context.Context, access, userID string) (Membership, error) {
	body, err := c.Call(ctx, access, userID, http.MethodPost,
		site.MembershipPath, []byte("{}"), nil)
	if err != nil {
		return Membership{}, err
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return Membership{}, fmt.Errorf("membership: not JSON: %w", err)
	}
	// The fields are sometimes at the top level and sometimes under
	// "data". A "data" that is not an object carries no fields.
	var data map[string]json.RawMessage
	if raw, ok := env["data"]; ok {
		_ = json.Unmarshal(raw, &data)
	}
	pick := func(k string) json.RawMessage {
		if v, ok := env[k]; ok && !isJSONNull(v) {
			return v
		}
		if v, ok := data[k]; ok && !isJSONNull(v) {
			return v
		}
		return nil
	}
	m := Membership{Known: true}
	if raw := pick("has_token_plan"); raw != nil {
		var has bool
		if json.Unmarshal(raw, &has) == nil {
			m.HasTokenPlan = has
		}
	}
	m.Tier = jsonString(pick("token_plan_tier"))
	m.GroupID = jsonString(pick("op_group_id"))
	if f, ok := jsonNumber(pick("token_plan_expires_at")); ok && f > 0 {
		ms := int64(f)
		if ms < 1e12 {
			ms *= 1000
		}
		m.ExpiresAt = time.UnixMilli(ms)
	}
	m.Balance = membershipBalance(env, data, pick)
	return m, nil
}

// membershipBalance is the credits left, as the site writes them.
//
// The summary's total is the balance. When there is no total, the
// account's own opcredit_balance is it: as a string, trimmed, or as a
// number.
func membershipBalance(env, data map[string]json.RawMessage, pick func(string) json.RawMessage) string {
	sum := jsonObject(env["op_credit_summary"])
	if sum == nil {
		sum = jsonObject(data["op_credit_summary"])
	}
	if sum != nil {
		if v := jsonString(sum["total_remaining_amount"]); v != "" {
			return v
		}
	}
	return jsonBalance(pick("opcredit_balance"))
}

// isJSONNull says a value is the JSON null.
func isJSONNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return v == nil
}

// jsonObject is a value as an object, or nothing when it is not one.
func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// jsonString is a value as a trimmed string, or nothing when it is not
// a string or is empty.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// jsonBalance is a balance the site wrote as a string or as a number.
func jsonBalance(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

// jsonNumber is a number the site wrote as a number or as a string.
func jsonNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// ledgerBatch is a batch as the site writes it.
//
// The times are pointers, so that a batch without one is told from a
// batch with a zero one. The site writes the time the credits run out
// in, and a batch without it cannot be read.
type ledgerBatch struct {
	Batch
	GrantedAtMS *int64 `json:"granted_at_ms"`
	ExpiresAtMS *int64 `json:"expire_at_ms"`
}

// batch turns the answer's batch into the one the callers see.
//
// A time the answer did not carry stays unset: it is not the epoch,
// and saying so would make an unreadable batch look like a spent one.
func (l ledgerBatch) batch() Batch {
	b := l.Batch
	if l.GrantedAtMS != nil {
		b.GrantedAtMS = *l.GrantedAtMS
		b.HasGrantedAt = true
		b.GrantedAt = time.UnixMilli(*l.GrantedAtMS)
	}
	if l.ExpiresAtMS != nil {
		b.ExpiresAtMS = *l.ExpiresAtMS
		b.HasExpiresAt = true
		b.ExpiresAt = time.UnixMilli(*l.ExpiresAtMS)
	}
	return b
}

// Ledger reads every batch of credits.
//
// A ledger that was read and is empty gives an empty list: the account
// has no credits, which is not the same as a ledger that could not be
// read. A ledger that cannot be read gives nil: a part of the ledger
// would tell a wrong share. So does a batch whose amounts cannot be
// read: the credits cannot be told apart then, and none is better
// than a wrong share.
func (c *Client) Ledger(ctx context.Context, access, userID string) ([]Batch, error) {
	const pageSize = 20
	const maxPages = 10
	// The list starts empty and not nil: a ledger that was read and is
	// empty is an empty list, and a nil one would say the ledger could
	// not be read.
	out := []Batch{}
	for page := 1; page <= maxPages; page++ {
		query := append(append([]QueryKV{}, CheckinQuery()...),
			QueryKV{K: "page_number", V: strconv.Itoa(page)},
			QueryKV{K: "page_size", V: strconv.Itoa(pageSize)},
			QueryKV{K: "include_expired", V: "true"},
		)
		body, err := c.Call(ctx, access, userID, http.MethodGet, site.CreditPath, nil, query)
		if err != nil {
			return nil, err
		}
		var env struct {
			Details    []ledgerBatch `json:"details"`
			TotalCount *int          `json:"total_count"`
			Data       *struct {
				Details    []ledgerBatch `json:"details"`
				TotalCount *int          `json:"total_count"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("credits: not JSON: %w", err)
		}
		details, total := env.Details, env.TotalCount
		if len(details) == 0 && env.Data != nil {
			details, total = env.Data.Details, env.Data.TotalCount
		}
		if total == nil {
			// The answer is not a ledger: it cannot be read.
			return nil, nil
		}
		for _, lb := range details {
			b := lb.batch()
			if b.CreditType == CreditTypePurchase || b.CreditType == CreditTypeCheckin {
				granted, ok1 := ParseAmount(b.GrantedAmount, false)
				remaining, ok2 := ParseAmount(b.RemainingAmount, true)
				if !ok1 || !ok2 {
					// The amounts cannot be read: the credits of
					// this batch cannot be told from the ones of
					// a spent batch, and the ledger as a whole
					// cannot be read.
					return nil, nil
				}
				b.Granted = granted
				b.Remaining = math.Min(remaining, granted)
				b.Consumed = granted - b.Remaining
			}
			out = append(out, b)
		}
		if len(out) >= *total || len(details) == 0 {
			if len(out) < *total {
				// The answer says there is more than it gave.
				return nil, nil
			}
			return out, nil
		}
	}
	// Too many pages: the ledger was not read whole.
	return nil, nil
}

// PlanWindows reads the plan's windows.
//
// The console's own answer comes first; the open platform's is the
// fallback.
//
// The console's answer is used only when it has a list of windows in
// it. The reference uses it only when the answer is a list that is not
// empty, and asks the open platform otherwise: the international site's
// remains_percent answers 200 with an empty list, and the windows are
// at the other endpoint.
//
// A list that is not empty is the console's answer even when every
// entry in it is a bucket the plan does not have: then the plan has no
// windows, and that is an answer, not a reason to ask again.
//
// An answer the console could not give is not thrown away when the
// open platform has the windows: the two endpoints are two ways of
// asking the same question, and the one that answered is the one that
// counts.
func (c *Client) PlanWindows(ctx context.Context, access, groupID string) ([]Window, error) {
	ws, listed, err := c.planWindowsOnce(ctx, access, groupID, site.PlanRemainsPath)
	if err == nil && listed {
		return ws, nil
	}
	ws2, _, err2 := c.planWindowsOnce(ctx, access, groupID, site.CodingPlanPath)
	if err2 != nil {
		return nil, err2
	}
	return ws2, nil
}

func (c *Client) planWindowsOnce(ctx context.Context, access, groupID, path string) ([]Window, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.platform+path, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept-Encoding", "identity")
	if groupID != "" {
		req.Header.Set("X-Group-Id", groupID)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return nil, false, err
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, false, &RefusedError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, false, fmt.Errorf("M Plan: HTTP %d", resp.StatusCode)
	}
	return parsePlanWindows(body)
}

// PlanTitle reads the plan's name as the console says it.
//
// The reference gives up quietly when the name cannot be read and the
// usage card names the tier instead. This keeps the error, so that a
// caller can tell a plan without a name from a plan that could not be
// read; the manager treats a failure as no name, as the reference does.
func (c *Client) PlanTitle(ctx context.Context, access, groupID string) (string, error) {
	path := c.platform + site.PlanTitlePath + "?biz_line=2&cycle_type=1&resource_package_type=7"
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
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &RefusedError{Status: resp.StatusCode}
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
		return "", fmt.Errorf("M Plan: not JSON: %w", err)
	}
	if env.CurrentSubscribe == nil {
		return "", nil
	}
	return strings.TrimSpace(env.CurrentSubscribe.Title), nil
}

// Models gives the site's models.
//
// The live list is asked for with the account's own token, the way the
// reference asks for it: the site's own answer, the list without a
// token when the account's one is refused, and the built-in list when
// the live one cannot be read. The built-in list is what the site is
// known to have, so a fallback and a live read are not confused with
// each other.
func (c *Client) Models(access string) []site.Model {
	if c.up == nil {
		return site.FallbackModels()
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsTimeout)
	defer cancel()
	body, status, ok := c.modelsOnce(ctx, access)
	if !ok && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		// The account's own token was turned away: the site's list is
		// asked for without one.
		body, _, ok = c.modelsOnce(ctx, "")
	}
	if !ok {
		return site.FallbackModels()
	}
	ms := parseModels(body)
	if len(ms) == 0 {
		return site.FallbackModels()
	}
	return ms
}

// modelsOnce asks the site for its model list.
//
// The model list is on the chat host, which is the account host today:
// the two are the same host, and the path is the chat's.
func (c *Client) modelsOnce(ctx context.Context, access string) ([]byte, int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+site.ModelsPath+"?"+site.ModelsQuery, nil)
	if err != nil {
		return nil, 0, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	resp, err := c.up.Do(req)
	if err != nil {
		return nil, 0, false
	}
	body, err := upstream.ReadBody(resp)
	if err != nil {
		return nil, resp.StatusCode, false
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.StatusCode, false
	}
	return body, resp.StatusCode, true
}

// modelProvider is one provider of the model config.
type modelProvider struct {
	ProviderID string          `json:"providerId"`
	Config     json.RawMessage `json:"config"`
}

// modelConfig is the models of one provider.
//
// The models are kept as they were written: Go's maps have no order,
// and the answer's order is the order the site's models are listed in.
type modelConfig struct {
	Models     json.RawMessage `json:"models"`
	ModelOrder []string        `json:"model_order"`
}

// modelEntry is one model of the config.
type modelEntry struct {
	Name       string `json:"name"`
	Reasoning  *bool  `json:"reasoning"`
	Attachment *bool  `json:"attachment"`
	Limit      struct {
		Context json.Number `json:"context"`
		Output  json.Number `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	ThinkingConfig struct {
		Mode string `json:"mode"`
	} `json:"thinking_config"`
	Thinking *struct {
		EffortOptions    []string `json:"effortOptions"`
		EffortOptionsAlt []string `json:"effort_options"`
	} `json:"thinking"`
	EffortOptions []string `json:"effort_options"`
}

// parseModels reads MiniMax Code's model config as models.
//
// The answer is {providers:[{providerId:"minimax",config:{models:{<id>:{…}},
// model_order?}}]}. The models come in the order model_order gives
// them, the rest in the order the answer wrote them.
func parseModels(body []byte) []site.Model {
	var env struct {
		Providers []modelProvider `json:"providers"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	var cfg *modelConfig
	for i := range env.Providers {
		if env.Providers[i].ProviderID != "minimax" {
			continue
		}
		var c modelConfig
		if err := json.Unmarshal(env.Providers[i].Config, &c); err != nil {
			continue
		}
		cfg = &c
		break
	}
	if cfg == nil || len(cfg.Models) == 0 {
		return nil
	}
	models := map[string]json.RawMessage{}
	if json.Unmarshal(cfg.Models, &models) != nil {
		return nil
	}
	ids := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, id := range cfg.ModelOrder {
		if _, ok := models[id]; ok && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range jsonKeys(cfg.Models) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	out := make([]site.Model, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		var e modelEntry
		if err := json.Unmarshal(models[id], &e); err != nil {
			continue
		}
		m := site.Model{ID: id, Name: id}
		if name := strings.TrimSpace(e.Name); name != "" {
			m.Name = name
		}
		if f, err := e.Limit.Context.Float64(); err == nil && f > 0 {
			m.Context = int(f)
		}
		if f, err := e.Limit.Output.Float64(); err == nil && f > 0 {
			m.Output = int(f)
		}
		inputs := []string{"text"}
		for _, k := range e.Modalities.Input {
			k = strings.ToLower(k)
			if k == "" || k == "text" {
				continue
			}
			inputs = append(inputs, k)
		}
		// A model said to take attachments takes images, as before #985.
		if e.Attachment != nil && *e.Attachment && !hasInput(inputs, "image") {
			inputs = append(inputs, "image")
		}
		m.Inputs = inputs
		m.Thinking = modelThinking(e)
		m.Efforts = modelEfforts(e)
		out = append(out, m)
	}
	return out
}

// modelThinking says how a model handles thinking.
func modelThinking(e modelEntry) site.Thinking {
	switch strings.TrimSpace(e.ThinkingConfig.Mode) {
	case "switchable":
		return site.ThinkingSwitchable
	case "forced_off":
		return site.ThinkingOff
	case "forced_on":
		return site.ThinkingForcedOn
	}
	if e.Reasoning != nil && !*e.Reasoning {
		return site.ThinkingOff
	}
	return site.ThinkingOn
}

// modelEfforts are the reasoning levels the model takes.
func modelEfforts(e modelEntry) []string {
	var all []string
	if e.Thinking != nil {
		if len(e.Thinking.EffortOptions) > 0 {
			all = e.Thinking.EffortOptions
		} else {
			all = e.Thinking.EffortOptionsAlt
		}
	}
	if len(all) == 0 {
		all = e.EffortOptions
	}
	out := []string{}
	for _, v := range all {
		v = strings.TrimSpace(v)
		if v == "" || v == "default" || v == "none" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// hasInput says the kinds of input already name one.
func hasInput(inputs []string, k string) bool {
	for _, in := range inputs {
		if in == k {
			return true
		}
	}
	return false
}

// jsonKeys gives an object's keys in the order the answer wrote them.
//
// Go's maps have no order, and the answer's order is the order the
// site's models are listed in.
func jsonKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		key, ok := tok.(string)
		if !ok {
			return keys
		}
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
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
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/sign"
	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
)

// QueryKV is one query parameter for a signed call.
type QueryKV = sign.KV

// Call sends one signed account API request.
//
// The body is "{}" for a POST and nil for a GET, as MiniMax Code sends
// it. What is signed is exactly what goes on the wire: a POST whose
// body was not given sends "{}" and signs "{}". The answer's business
// codes are checked before it is handed back.
func (c *Client) Call(ctx context.Context, access, userID, method, path string, body []byte, query []QueryKV) ([]byte, error) {
	now := c.now()
	// The reference signs the bytes it sends: a POST's body, or "{}"
	// when it has none. A signature over anything else is refused.
	wire := wireBody(method, body)
	signed := sign.Sign(c.base, path, sign.Options{
		Access:   access,
		UserID:   userID,
		Body:     wire,
		Query:    query,
		Now:      now.UnixMilli(),
		TZOffset: c.tzOffset,
		OSName:   c.osName,
		Lang:     site.Lang,
	})
	var req *http.Request
	var err error
	if method == http.MethodPost {
		req, err = http.NewRequestWithContext(ctx, method, signed.URL, strings.NewReader(string(wire)))
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
		return nil, &statusError{status: resp.StatusCode}
	}
	if err := upstream.CheckEnvelope("account", raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// wireBody is the body that goes on the wire and into the signature.
//
// The reference's signing sends a GET without a body and a POST with
// the body it is given, an empty one as "{}". The two have to be the
// same bytes: a signature made over something else is refused.
func wireBody(method string, body []byte) []byte {
	if method != http.MethodPost {
		return nil
	}
	if len(body) == 0 {
		return []byte("{}")
	}
	return body
}

// RefusedError is a refusal of the sign-in.
type RefusedError struct {
	// Status is the HTTP status the refusal came with.
	Status int
}

// Error is the error's text.
func (e *RefusedError) Error() string {
	return fmt.Sprintf("the sign-in was refused (HTTP %d)", e.Status)
}

// Is says whether the error is the upstream's refusal of the sign-in.
//
// A refused sign-in is one event, whatever shape it arrives in: an
// HTTP 401 or 403, or MiniMax's own code 1000048 in the body of a
// 200. errors.Is(err, upstream.ErrSignInRefused) tells it apart from
// every other error.
func (e *RefusedError) Is(target error) bool {
	return target == upstream.ErrSignInRefused
}

// isNotFound says an error is a 404.
func isNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

type statusError struct{ status int }

func (e *statusError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

// parsePlanWindows reads the console's answer as windows.
//
// listed says the answer had a list of windows in it. The reference
// decides on that list and not on the windows it can make of it: an
// answer with a list is the console's answer, even when every entry in
// it is a bucket the plan does not have.
func parsePlanWindows(raw []byte) (windows []Window, listed bool, err error) {
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
		return nil, false, fmt.Errorf("no plan in the reply: %w", err)
	}
	if env.BaseResp != nil && env.BaseResp.StatusCode != 0 {
		// The reference names the status code when the answer has no
		// message of its own: an error with an empty text says nothing.
		msg := strings.TrimSpace(env.BaseResp.StatusMsg)
		if msg == "" {
			msg = fmt.Sprintf("MiniMax said %d", env.BaseResp.StatusCode)
		}
		return nil, false, errors.New(msg)
	}
	var out []Window
	for _, m := range env.ModelRemains {
		name := strings.TrimSpace(m.ModelName)
		if name == "" {
			continue
		}
		// A model whose two windows are both unlimited and both have no
		// count is a bucket the plan does not have.
		if isThree(m.CurrentIntervalStatus) && isThree(m.CurrentWeeklyStatus) &&
			isZeroCount(m.CurrentIntervalTotalCnt) && isZeroCount(m.CurrentWeeklyTotalCnt) {
			continue
		}
		general := strings.EqualFold(name, "general")
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
			// The window is named after its own length. A window of
			// three hours is "3 hours" and not "5 hours": the length is
			// the window's own, and the site does not always give
			// five.
			span := windowSpan(x.start, x.end, x.weekly)
			own := windowName(x.weekly, span)
			if status == 3 {
				// An unlimited window is said so on the general model.
				if general {
					out = append(out, Window{Name: own, ModelName: name, Unlimited: true})
				}
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
			w := Window{Name: own, ModelName: name, Used: used}
			if x.end > 0 {
				// The end of the window is a time the site wrote, and
				// the site does not say in which unit: a number
				// below a trillion is seconds. The reference reads it
				// the same way, and the window's own length is read
				// that way too — so the two agree on how long the
				// window is.
				w.ResetsAt = time.UnixMilli(planMS(x.end))
			}
			// The site counts the items a window has. A count that is
			// not there is zero, the way the reference reads it, so a
			// window with a limit and no count yet is "0 of 150" and
			// not "unknown". A count that is negative is not a count
			// at all: the reference wants the count to be at least
			// zero, or the window has no limit to show.
			items, of := 0.0, 0.0
			if x.usedCnt != nil {
				items = *x.usedCnt
			}
			if x.totalCnt != nil {
				of = *x.totalCnt
			}
			if items >= 0 && of > 0 && items <= of {
				w.Amount = items
				if status == 2 {
					// A spent window counts its whole limit.
					w.Amount = of
				}
				w.Limit = of
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
	return out, len(env.ModelRemains) > 0, nil
}

// isThree says whether a number is 3.
func isThree(v *float64) bool { return v != nil && *v == 3 }

// isZeroCount says whether a count is missing or zero.
func isZeroCount(v *float64) bool { return v == nil || *v == 0 }

// windowSpan is how long a plan window is.
//
// The site writes the two ends of a window in seconds or in
// milliseconds, and the reference reads them the same way: a number
// that is too long to be a time in seconds is in milliseconds.
//
// A weekly window without times of its own is a week long. Every other
// window without times is of no length at all, and says so.
func windowSpan(start, end int64, weekly bool) time.Duration {
	ms := int64(0)
	switch {
	case start > 0 && end > start:
		ms = planMS(end) - planMS(start)
	case weekly:
		ms = 7 * 86400_000
	}
	return time.Duration(ms) * time.Millisecond
}

// planMS reads a time the site wrote.
//
// A time in seconds is shorter than a time in milliseconds by a
// thousand, and the site does not say which it wrote. The reference
// tells them apart by their length: a number below a trillion is in
// seconds.
func planMS(v int64) int64 {
	if v < 1e12 {
		return v * 1000
	}
	return v
}

// windowName is the name of a plan window.
//
// The name says how long the window is, and the length is the
// window's own: a window of three hours is "3 hours", and a window
// that is a day long is "24 hours" and not "1 days", the way the
// reference reads it.
//
// A weekly window is "7 days" whatever its times say, and a window
// without a length is "Allowance".
func windowName(weekly bool, span time.Duration) string {
	if weekly {
		return "7 days"
	}
	ms := span.Milliseconds()
	switch {
	case ms > 86400_000 && ms%86400_000 == 0:
		return fmt.Sprintf("%d days", ms/86400_000)
	case ms >= 3600_000 && ms%3600_000 == 0:
		return fmt.Sprintf("%d hours", ms/3600_000)
	case ms > 0:
		// The reference floors the minutes, and Go's division rounds
		// towards zero. The span is never negative here, but the
		// floor is what the reference does.
		return fmt.Sprintf("%d minutes", int64(math.Floor(float64(ms)/60_000)))
	default:
		return "Allowance"
	}
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
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/upstream"
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

func testAccountClient(t *testing.T, srv *httptest.Server) *account.Client {
	t.Helper()
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	c := account.New(up)
	c.SetBase(srv.URL, srv.URL)
	return c
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

// TestRefusedByHTTP says a 401 is a failed check-in, not an error.
//
// 401 是“签到被拒”，是一个结果，不是一个意外。
func TestRefusedByHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid access token"}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", out.Outcome, OutcomeFailed)
	}
}

// TestRefusedByBusinessCode says MiniMax's own refusal is the same event.
//
// 200 的回答里带 1000048，和 401 是一件事：签到被拒。
func TestRefusedByBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`))
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", out.Outcome, OutcomeFailed)
	}
}

// TestUnknownClaimResult says a claim that says nothing is not a claim.
//
// 回答里没有 claim_result 时，不能当成“已经签到过了”——那会把一次
// 没有发生的签到记成完成了。
func TestUnknownClaimResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(panel(2, true)))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(testAccountClient(t, srv))
	out, err := c.Run(context.Background(), "tok", "1234567890")
	if err == nil {
		t.Fatalf("outcome = %+v, want an error", out)
	}
	if !strings.Contains(err.Error(), "claim_result") {
		t.Fatalf("error = %q", err.Error())
	}
}
```

测试文件的 import 补上 `"mmxcode/internal/account"` 与 `"mmxcode/internal/upstream"`，并加助手：

```go
func testAccountClient(t *testing.T, srv *httptest.Server) *account.Client {
	t.Helper()
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	c := account.New(up)
	c.SetBase(srv.URL, srv.URL)
	return c
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
	"errors"
	"fmt"
	"net/http"
	"sort"

	"mmxcode/internal/account"
	"mmxcode/internal/site"
	"mmxcode/internal/upstream"
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
// The sign-in is the caller's business: the caller renews the token
// before it calls, and Run neither renews nor tries again. What Run
// does with a refused sign-in — an HTTP 401 or 403, or MiniMax's own
// refusal code in the body of a 200 — is to answer OutcomeFailed
// instead of an error: a refused sign-in is one event, however it
// arrives.
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
	if errors.As(err, &refused) || errors.Is(err, upstream.ErrSignInRefused) {
		return Outcome{Outcome: OutcomeFailed, Message: err.Error()}, nil
	}
	return Outcome{}, err
}

func (c *Client) run(ctx context.Context, access, userID string) (Outcome, error) {
	raw, err := c.acc.Call(ctx, access, userID, http.MethodGet, site.SigninStatusPath, nil, account.CheckinQuery())
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
		claimRaw, err := c.acc.Call(ctx, access, userID, http.MethodPost, site.SigninClaimPath, []byte("{}"), account.CheckinQuery())
		if err != nil {
			return Outcome{}, err
		}
		var env struct {
			Data struct {
				ClaimResult json.RawMessage `json:"claim_result"`
				Points      *float64        `json:"points"`
			} `json:"data"`
		}
		if err := json.Unmarshal(claimRaw, &env); err != nil {
			return Outcome{}, fmt.Errorf("check-in: not JSON: %w", err)
		}
		out := Outcome{Outcome: OutcomeDone}
		if env.Data.Points != nil && *env.Data.Points > 0 {
			out.Credit = *env.Data.Points
		}
		// The claim answers 1 when it claimed now and 2 when the day
		// was already claimed. Anything else is not a claim: the
		// reference throws for it, and so does this. A check-in that
		// is not known is not a check-in that was done.
		result, ok := claimResult(env.Data.ClaimResult)
		if !ok {
			return Outcome{}, fmt.Errorf("check-in: unknown claim_result %s", claimText(env.Data.ClaimResult))
		}
		switch result {
		case 1:
			out.Outcome = OutcomeClaimed
			out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
		case 2:
			out.Streak = streakFrom(days, indexOf(days, today)-1) + 1
		default:
			return Outcome{}, fmt.Errorf("check-in: unknown claim_result %v", result)
		}
		return out, nil
	default:
		return Outcome{Outcome: OutcomeInactive}, nil
	}
}

// claimResult reads the claim's answer as the reference reads it: a
// number above zero, and nothing else.
func claimResult(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	if f <= 0 {
		return 0, false
	}
	return f, true
}

// claimText is what the answer said, for the error's text.
func claimText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "undefined"
	}
	return string(raw)
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
		return nil, fmt.Errorf("check-in: not JSON: %w", err)
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

// accountOf gives the entry of one account.
//
// The accounts are sorted by id, but a test that counts on the order
// breaks when an account is added: the account is asked for by name.
func accountOf(t *testing.T, s Summary, id string) AccountUsage {
	t.Helper()
	for _, a := range s.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no entry for %s in %+v", id, s.Accounts)
	return AccountUsage{}
}

func TestAggregate(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 600, Consumed: 120.5, Remaining: 479.5,
			ExpiresAt: now.Add(48 * time.Hour),
			CreatedAt: now.Add(-time.Minute),
		},
		{
			AccountID: "acc_02", HasAmounts: true,
			Granted: 300, Consumed: 0, Remaining: 300,
			ExpiresAt: now.Add(24 * time.Hour),
			CreatedAt: now.Add(-time.Minute),
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
	a1 := accountOf(t, s, "acc_01")
	if a1.Granted == nil || *a1.Granted != 600 {
		t.Fatalf("acc_01 = %+v", a1)
	}
	if a1.State != StateRead {
		t.Fatalf("acc_01 state = %q, want %q", a1.State, StateRead)
	}
	a2 := accountOf(t, s, "acc_02")
	if a2.Remaining == nil || *a2.Remaining != 300 {
		t.Fatalf("acc_02 = %+v", a2)
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
	a := accountOf(t, s, "acc_02")
	if a.Granted != nil {
		t.Fatal("an account without amounts must not carry a number")
	}
	if a.State != StateUnavailable {
		t.Fatalf("state = %q, want %q", a.State, StateUnavailable)
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

// TestAggregateCountsAnAccountOnce says two readings of one account are
// one account.
//
// 同一个账号的两行读数都被加起来，积分就翻倍：一个 100 积分的账号
// 会被报成 300。
func TestAggregateCountsAnAccountOnce(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			ID: 1, AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 0, Remaining: 100,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		},
		{
			// 同一毫秒里的第二次刷新
			ID: 2, AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 0, Remaining: 100,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		},
	}
	s := Aggregate(snaps, now)
	if s.TotalGranted != 100 {
		t.Fatalf("granted = %v, want 100: one account is one account", s.TotalGranted)
	}
	if s.TotalRemaining != 100 {
		t.Fatalf("remaining = %v, want 100", s.TotalRemaining)
	}
	if len(s.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(s.Accounts))
	}
}

// TestAggregateKeepsTheNewestReading says the newer reading of an
// account is the one that counts.
func TestAggregateKeepsTheNewestReading(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			ID: 1, AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 0, Remaining: 100,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now.Add(-time.Minute),
		},
		{
			ID: 2, AccountID: "acc_01", HasAmounts: true,
			Granted: 300, Consumed: 0, Remaining: 300,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		},
	}
	s := Aggregate(snaps, now)
	if s.TotalGranted != 300 {
		t.Fatalf("granted = %v, want 300: the newest reading counts", s.TotalGranted)
	}
	if len(s.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(s.Accounts))
	}
}

// TestAggregateTellsExpiredFromUnread says an account whose credits ran
// out and an account whose credits were never read are not the same.
//
// 两个都说成“读不到”，总数就短了一截，却没有人知道为什么。
func TestAggregateTellsExpiredFromUnread(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 100, Remaining: 0,
			ExpiresAt: now.Add(-time.Hour), CreatedAt: now,
		},
		{
			AccountID: "acc_02", HasAmounts: false, CreatedAt: now,
		},
	}
	s := Aggregate(snaps, now)
	if s.TotalGranted != 0 {
		t.Fatalf("granted = %v, want 0: expired credits are not counted", s.TotalGranted)
	}
	if s.AccountsExpired != 1 {
		t.Fatalf("expired = %d, want 1", s.AccountsExpired)
	}
	if s.AccountsUnavailable != 1 {
		t.Fatalf("unavailable = %d, want 1", s.AccountsUnavailable)
	}
	expired := accountOf(t, s, "acc_01")
	if expired.State != StateExpired {
		t.Fatalf("acc_01 state = %q, want %q", expired.State, StateExpired)
	}
	if expired.ExpiresAt == nil {
		t.Fatal("an expired reading has to say when it ran out")
	}
	unread := accountOf(t, s, "acc_02")
	if unread.State != StateUnavailable {
		t.Fatalf("acc_02 state = %q, want %q", unread.State, StateUnavailable)
	}
	if unread.Granted != nil {
		t.Fatal("a reading that never happened has no numbers")
	}
}

// TestAggregateTellsZeroFromUnread says an account with nothing left is
// not an account that was never read.
//
// 真的 0 和读不到不是一回事：读不到的账号不计入总数，真的 0 计入。
func TestAggregateTellsZeroFromUnread(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 0, Consumed: 0, Remaining: 0, CreatedAt: now,
		},
	}
	s := Aggregate(snaps, now)
	if s.AccountsUnavailable != 0 {
		t.Fatalf("unavailable = %d, want 0: a zero is not an unknown", s.AccountsUnavailable)
	}
	if s.AccountsExpired != 0 {
		t.Fatalf("expired = %d, want 0", s.AccountsExpired)
	}
	a := accountOf(t, s, "acc_01")
	if a.State != StateRead {
		t.Fatalf("state = %q, want %q", a.State, StateRead)
	}
	if a.Remaining == nil || *a.Remaining != 0 {
		t.Fatalf("acc_01 = %+v, want a zero that is there", a)
	}
}

// TestAggregateUpdatedAtIsTheReadingTime says the summary says when
// the numbers were read, not when the summary was made.
//
// 汇总时间当成读数时间，陈旧的数字看起来就是新的。
func TestAggregateUpdatedAtIsTheReadingTime(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	read := now.Add(-2 * time.Minute)
	s := Aggregate([]store.UsageSnapshot{
		{AccountID: "acc_01", HasAmounts: true, Granted: 10, Remaining: 10, CreatedAt: read},
	}, now)
	if !s.UpdatedAt.Equal(read) {
		t.Fatalf("updated_at = %v, want the reading time %v", s.UpdatedAt, read)
	}
	// 没有读数时退回汇总时间
	empty := Aggregate(nil, now)
	if !empty.UpdatedAt.Equal(now) {
		t.Fatalf("updated_at = %v, want %v", empty.UpdatedAt, now)
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
//
// The three states are kept apart: an account whose credits were read,
// an account whose credits have run out, and an account whose credits
// could not be read. A total that is short must be explainable.
package usage

import (
	"sort"
	"time"

	"mmxcode/internal/store"
)

// The states an account's credits can be in.
//
// They are told apart, because they mean different things: credits that
// were read are counted, credits that have run out are counted as
// nothing, and credits that could not be read are not counted at all.
const (
	// StateRead means the credits were read and some of them are live.
	StateRead = "read"
	// StateExpired means the credits were read and all of them have
	// run out.
	StateExpired = "expired"
	// StateUnavailable means the credits could not be read.
	StateUnavailable = "unavailable"
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
	// State says what the numbers are: StateRead, StateExpired or
	// StateUnavailable.
	//
	// The three are told apart, so that an account whose credits ran
	// out is not the same as an account whose credits were never read.
	State string `json:"state"`
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
	// UpdatedAt is when the numbers were read: the newest of the
	// readings that went into this summary. It is not the time the
	// summary was made, so a stale number is not made to look fresh.
	UpdatedAt time.Time `json:"updated_at"`
	// AccountsUnavailable counts the accounts whose ledger could not be
	// read, so a short total is not mistaken for a full one.
	AccountsUnavailable int `json:"accounts_unavailable"`
	// AccountsExpired counts the accounts whose credits were read and
	// have all run out, so a short total can say why it is short.
	AccountsExpired int `json:"accounts_expired"`
	// Accounts is each account's own numbers.
	Accounts []AccountUsage `json:"accounts"`
}

// Aggregate sums the accounts' snapshots.
//
// One account is one row. When two readings of one account are given,
// the newer one is the account's reading and the older one is left
// out: summing both would count the same credits twice.
//
// now decides which batches count: one that has run out is out. It is
// also what an expired reading is compared with, so that the same
// reading does not change its state from one call to the next.
func Aggregate(snaps []store.UsageSnapshot, now time.Time) Summary {
	s := Summary{UpdatedAt: now}
	var newest time.Time
	for _, sn := range newestPerAccount(snaps) {
		au := AccountUsage{ID: sn.AccountID}
		switch {
		case !sn.HasAmounts:
			// The credits could not be read: the account is left out
			// of the totals rather than counted as zero.
			au.State = StateUnavailable
			s.AccountsUnavailable++
		case !sn.ExpiresAt.IsZero() && !sn.ExpiresAt.After(now):
			// The credits were read and every one of them has run
			// out. The account has nothing left, which is not the
			// same as an account whose credits are unknown.
			au.State = StateExpired
			s.AccountsExpired++
			g, c, r := sn.Granted, sn.Consumed, sn.Remaining
			au.Granted, au.Consumed, au.Remaining = &g, &c, &r
			t := sn.ExpiresAt
			au.ExpiresAt = &t
		default:
			au.State = StateRead
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
		if sn.CreatedAt.After(newest) {
			newest = sn.CreatedAt
		}
		s.Accounts = append(s.Accounts, au)
	}
	if !newest.IsZero() {
		s.UpdatedAt = newest
	}
	sort.Slice(s.Accounts, func(i, j int) bool { return s.Accounts[i].ID < s.Accounts[j].ID })
	return s
}

// newestPerAccount gives each account's newest snapshot.
//
// The newest is the one that was read last: the later reading wins,
// and a reading that shares its millisecond with another is told from
// it by the id, which grows with every row written.
func newestPerAccount(snaps []store.UsageSnapshot) []store.UsageSnapshot {
	byAccount := make(map[string]store.UsageSnapshot, len(snaps))
	at := make(map[string]int, len(snaps))
	order := make([]string, 0, len(snaps))
	for i, sn := range snaps {
		cur, ok := byAccount[sn.AccountID]
		if !ok {
			byAccount[sn.AccountID] = sn
			at[sn.AccountID] = i
			order = append(order, sn.AccountID)
			continue
		}
		// Two readings that cannot be told apart are the later one's:
		// they are the same reading, so either will do.
		if newer(sn, cur) || (sn.ID == cur.ID && i > at[sn.AccountID]) {
			byAccount[sn.AccountID] = sn
			at[sn.AccountID] = i
		}
	}
	out := make([]store.UsageSnapshot, 0, len(order))
	for _, id := range order {
		out = append(out, byAccount[id])
	}
	return out
}

// newer says a reading is newer than another.
func newer(a, b store.UsageSnapshot) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
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

import "os"

func main() {
	os.Exit(Execute())
}
```

创建 `cmd/mmxcode/root.go`：

```go
// This file holds the mmxcode command line.
//
// Everything in this directory is package main: the directory is the
// binary, and a directory holds one package.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
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
	mgr   *manager.Manager
}

func newRoot() *cobra.Command {
	var (
		configPath string
		dbPath     string
	)
	root := &cobra.Command{
		Use:          "mmxcode",
		Short:        "MiniMax Code 多账号网关",
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
		e.egr = egress.New(cfg.Proxy.Groups, cfg.Proxy.Default)
		// 登录和账户 API 也走代理。配了不用等于没配：国内的网络
		// 连不上国际站，登录就停在第一步。
		tr, err := e.egr.Transport(e.egr.Resolve(store.Account{}))
		if err != nil {
			return err
		}
		e.up = upstream.New(upstream.Options{Timeout: 30 * time.Second, Transport: tr})
		e.acc = account.New(e.up)
		e.mgr = manager.New(s, e.up, e.acc, e.egr, oauth.NewRenewer())
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
	root.AddCommand(newServeCmd(e))
	return root
}

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

- [ ] **Step 2: 写 login 子命令**

创建 `cmd/mmxcode/login.go`：

```go
package main

import (
	"context"
	"fmt"
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
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"mmxcode/internal/egress"
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
				// 代理地址里的账号和密码是秘密，而这份输出会进到终端的
				// 回滚里，也会进到 CI 的日志里。egress.Redacted 就是
				// 为了这个。组名不是秘密，照原样打印。
				proxy := egress.Redacted(a.ProxyURL)
				if proxy == "" {
					proxy = a.ProxyGroup
				}
				if proxy == "" {
					proxy = egress.Redacted(e.cfg.Proxy.Default)
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
			ctx := context.Background()
			err := e.store.DeleteAccount(ctx, args[0])
			if errors.Is(err, store.ErrNotFound) {
				// 存储层说的是"没有这一行"，人说的是"没有这个账号"。
				return fmt.Errorf("no such account: %s", args[0])
			}
			return err
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
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no such account: %s", args[0])
				}
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
			// 只写出口那两列。整行写回去会把别的写者在这中间改的东西抹掉：
			// 登出刚写下的状态，会被这一次改代理抹回 active。
			return e.store.SetAccountProxy(ctx, a.ID, a.ProxyGroup, a.ProxyURL, nowUTC())
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "出口组名")
	cmd.Flags().StringVar(&url, "url", "", "单独的代理地址")
	return cmd
}
```

在 `cmd/mmxcode/root.go` 加一个 `nowUTC` 助手（import 补上 `"sort"` 与 `"strings"`）：

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
package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"mmxcode/internal/checkin"
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
			failed := false
			for _, a := range list {
				if accountID != "" && a.ID != accountID {
					continue
				}
				// 走 manager 那条路：它先续期，再用账号自己的出口，
				// 签到记录也由它写（日子是站点的那一天）。
				//
				// 命令自己读库里的令牌是不续期的。一个 90 分钟没续过
				// 的令牌发过去，站点回 401，签到根本没发生，而命令
				// 还会说成功。
				out, err := e.mgr.Checkin(ctx, a.ID)
				if err != nil {
					fmt.Printf("%s\t失败：%v\n", a.Name, err)
					failed = true
					continue
				}
				fmt.Printf("%s\t%s\t积分 %g\t连续 %d 天\n", a.Name, out.Outcome, out.Credit, out.Streak)
				if out.Outcome == checkin.OutcomeFailed {
					// 被拒的签到不是签到成功。命令要失败，cron 才
					// 知道今天什么都没领到。
					msg := out.Message
					if msg == "" {
						msg = "the sign-in was refused"
					}
					fmt.Printf("%s\t失败：%s\n", a.Name, msg)
					failed = true
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
package main

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
package main

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

## 快速开始

```bash
# 编译
go build -o mmxcode ./cmd/mmxcode

# 配置文件：照着 config.example.yaml 改一份
cp config.example.yaml config.yaml
# 把 server.listen 改成 "127.0.0.1:8080"（本机用），
# 或者写上 server.api_keys（给别的机器用）

# 登录一个账号：命令会打印一个链接和一个验证码
./mmxcode --config config.yaml --db ./mmxcode.db login

# 看看账号、签到、看积分
./mmxcode --config config.yaml --db ./mmxcode.db accounts list
./mmxcode --config config.yaml --db ./mmxcode.db checkin
./mmxcode --config config.yaml --db ./mmxcode.db usage

# 起网关
./mmxcode --config config.yaml --db ./mmxcode.db serve
```

数据库路径由 `--db` 覆盖（示例配置里的 `db: /data/mmxcode.db` 是容器里的路径，本机跑要换掉）。

起完网关之后，客户端指过来就行：

```bash
curl -s http://127.0.0.1:8080/v1/messages \
  -H "content-type: application/json" \
  -d '{"model":"MiniMax-M3","max_tokens":64,"messages":[{"role":"user","content":"say hi"}]}'
```

配了 `server.api_keys` 的话，请求要带上 key：

```bash
curl -s http://127.0.0.1:8080/v1/messages \
  -H "content-type: application/json" \
  -H "x-api-key: sk-local-xxxx" \
  -d '{"model":"MiniMax-M3","max_tokens":64,"messages":[{"role":"user","content":"say hi"}]}'
```

## 命令

```
mmxcode serve                     # 起 HTTP 服务、WebUI 与定时任务
mmxcode login                    # 登录一个账号
mmxcode accounts list            # 列出账号
mmxcode accounts proxy <id> --group warp
mmxcode checkin                  # 给所有账号签到
mmxcode usage                    # 积分汇总
mmxcode models                   # 模型列表
```

全局开关，对所有子命令有效：

- `--config <path>`：配置文件路径。默认取环境变量 `MMXCODE_CONFIG`；都不给就用内置默认值。
- `--db <path>`：数据库路径，覆盖配置文件里的 `db`。

`serve` 自己没有别的开关：监听地址、key、轮转参数都在配置文件里。

其余子命令的开关：`login --name`（账号标签）、`accounts proxy --group/--url`（出口组名 / 单独的代理地址）、`checkin --account`（只签这一个账号）、`usage --json`（输出 JSON）。

## 对外接口

聊天协议：

| 路由 | 用途 |
|---|---|
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | Codex Responses |
| `GET /v1/models` | 模型列表 |

管理接口，都在 `/api/` 下：

| 路由 | 方法 | 用途 |
|---|---|---|
| `/api/accounts` | GET / POST | 列出账号 / 发起一次登录 |
| `/api/accounts/poll` | POST | 查登录进度 |
| `/api/accounts/{id}` | PATCH / DELETE | 改账号（名字、出口组、代理地址）/ 删除账号 |
| `/api/accounts/{id}/checkin` | POST | 给这个账号签到 |
| `/api/accounts/{id}/usage` | GET | 读这个账号的积分 |
| `/api/usage` | GET | 所有账号的积分汇总 |
| `/api/egress` | GET | 出口组和它们的状态 |
| `/api/egress/test` | POST | 立刻测一个出口组 |
| `/api/sessions` | GET | 会话绑定（目前是空列表） |
| `/api/events` | GET | SSE，每 30 秒一个心跳 |
| `/api/session` | POST / DELETE | 登录换 cookie / 注销 |

WebUI 页面：

| 路由 | 页面 |
|---|---|
| `/` | 账号 |
| `/login` | 登录 |
| `/egress` | 出口 |
| `/usage` | 用量 |
| `/signin` | 交 key 的登录页 |
| `/static/app.js` | 页面上的脚本 |

### 鉴权

- 配了 `server.api_keys` 时，每个请求都要带 `x-api-key` 或 `Authorization: Bearer <key>`；两个头都没有、或者 key 不对，一律 401。
- 一个 key 都不配时，鉴权整体跳过。所以 `mmxcode serve` 只允许听本机地址：`server.listen` 不是回环地址（`:8080`、`0.0.0.0:8080` 都不算）就直接报错退出：

  ```
  no api key is configured, so the gateway would listen on :8080 for everyone; put api_keys in the config or listen on 127.0.0.1
  ```

- 鉴权对所有路径生效。带 key 的请求（`x-api-key` 头或 `Authorization: Bearer`）一律放行；不带 key 的请求分两种：`/api/` 下的接口回 401，浏览器要开的页面回登录页 `/signin`（200），登录页上填 key 换来的 cookie 之后就能进管理界面。
- cookie 的值是拿 key 算出来的 HMAC，服务器自己不留东西；`HttpOnly`、`SameSite=Strict`，走 TLS 时才加 `Secure`。

### 轮转

**会话怎么定。** 请求的会话优先看客户端给的头：`X-Session-Id`（也认 `X-Conversation-Id`、`X-Session-Id-Codex`、`Session-Id`）。有就用它的哈希当账号绑定键。客户端没带头，就用 system prompt 加第一条 user 消息的哈希当绑定键。两样都没有就没有会话，按轮转挑账号。

**发给上游的会话 id。** 上游拿到的是数据库里存着的一个随机 id：一个会话一个，存下来不变，重启之后还是它，换号也不变。客户端自己那串字符串不外发，请求里的话的哈希也不外发。

这个改不了有一种情况：两段 system prompt 和第一条 user 消息都一样的对话，客户端又不带会话 id 的时候，按上面的规则它们本来就是同一个会话，于是共用一个上游 id、一个上游会话。服务器分不出来 —— 请求里没有能分开它们的东西；要分开只能让客户端带上 `X-Session-Id`。

**换号。** 账号的配额用完时，上游的拒绝被判定成配额耗尽，网关把这个账号的这个 plan 组标记成耗尽，换一个账号把请求重放一遍。重放只发生在还没往客户端写出任何字节的时候：流式响应会先读第一个 SSE 事件，判断它是错误还是回答；一旦写出去就不再换号，流到一半断了只给客户端补一个 error 事件收尾。

- 耗尽的账号在 `rotation.exhausted_cooldown`（默认 5m）之内不再被挑中；能从上游读到窗口重置时间就按那个时间算，读不到才按 cooldown 算。
- 这个请求里已经试过并失败的账号会被跳过：另一个账号来答这一次，但会话绑定留在原处 —— 一次 5xx 不会把整个会话搬到别的账号上去。

## 配置

见 `config.example.yaml`（容器里见 `config.docker.yaml`）。文件只需要写自己要改的键，剩下的走默认值；认不出的键直接报错。

| 键 | 默认值 | 作用 |
|---|---|---|
| `server.listen` | `":8080"` | 监听地址 |
| `server.api_keys` | 空 | 网关接受的 key 列表。空 = 不鉴权，只允许听本机 |
| `db` | `"/data/mmxcode.db"` | SQLite 数据库路径 |
| `upstream.timeout` | `120s` | 上游请求时限。流式对话也吃这份预算，想聊更久就调大 |
| `upstream.site` | `"global"` | 上游站点，只支持 `global` |
| `rotation.max_attempts` | `3` | 一个请求最多换几个账号 |
| `rotation.strategy` | `"sticky"` | `sticky`（会话粘账号）或 `round-robin`（每次都转）。写错的值当 `sticky` 处理 |
| `rotation.exhausted_cooldown` | `5m` | 配额耗尽后暂时不用这个账号的时间。读不到窗口重置时间时按它算 |
| `proxy.default` | 空 | 全局默认出口 |
| `proxy.groups` | 空 | 出口组：组名 → 地址。账号可以指向其中一组，一组共用一个出口 |
| `schedule.checkin_at` | `"09:00"` | 每天签到的时间，本地时区，`HH:MM` |
| `schedule.usage_refresh` | `15m` | 积分刷新间隔 |
| `schedule.token_refresh` | `5m` | token 续期检查间隔 |
| `schedule.egress_health` | `2m` | 出口健康检查间隔 |

环境变量：`MMXCODE_CONFIG` 是 `--config` 的默认值；`MMXCODE_PROXY_DEFAULT` 覆盖 `proxy.default`（不设就不覆盖）。

两个值写错了 `mmxcode serve` 会拒绝启动，而不是悄悄按别的来：`schedule.checkin_at` 不是 `HH:MM` 会报 `schedule.checkin_at = "..."; want HH:MM`，`upstream.site` 不是 `global` 会报 `upstream.site = "..."; only "global" is supported`。

### 代理模型

出口按这个顺序挑：

1. 账号自己的代理地址：`mmxcode accounts proxy <id> --url <地址>`；
2. 账号指向的出口组：`mmxcode accounts proxy <id> --group <组名>`，取组里的地址；
3. 全局 `proxy.default`。

组的地址写成空字符串时不算出口：它会落到全局默认，而不是变成直连。全局默认也是空（或明确写 `direct`）时才是直连。

- 支持的 scheme：`http`、`https`、`socks5`、`socks5h`。没有 scheme 的地址会报错，不会默默直连。
- `socks5://` 和 `socks5h://` 都把域名交给代理去解析（远端 DNS），跟 curl 的 `socks5://`（本地解析）不一样。

## 部署

```bash
cp .env.example .env
docker compose up -d
```

compose 做的事：

- 用仓库里的 Dockerfile 构建，镜像名 `mmxcode-gateway:latest`。
- 端口只绑本机：`127.0.0.1:8080:8080`，管理界面不暴露到公网。
- 挂配置文件：默认挂仓库里的 `config.docker.yaml` 到 `/app/config.yaml`（只读）。要用自己的副本就设 `MMXCODE_CONFIG_FILE=./config.yaml`。
- 挂数据目录：默认是 Docker 命名卷 `mmxcode-data`，容器里是 `/data`，数据库落在 `/data/mmxcode.db`。
- 容器以非 root 用户 `mmxcode`（uid 10001）运行，`/data` 归它所有。
- 环境变量：`MMXCODE_CONFIG=/app/config.yaml`、`TZ=Asia/Shanghai`、`MMXCODE_PROXY_DEFAULT`（默认 `http://host.docker.internal:7890`；容器里访问宿主机的代理要写 `host.docker.internal`，compose 里已经配好）。

数据目录：

- 默认 `MMXCODE_DATA_DIR=mmxcode-data`，是 Docker 命名卷。Docker 按镜像里 `/data` 的属主（uid 10001）创建它，容器里的用户直接能读写。
- 想把数据库留在宿主机目录上就写 `MMXCODE_DATA_DIR=./data`，但那个目录必须对 uid 10001 可写，否则容器建不了数据库文件、启动即失败：

  ```bash
  mkdir -p data && chown 10001:10001 data
  ```

### Cloudflare WARP

WARP 是额外的出口选项，跟按账号单独指定的代理并列。

- 默认是关的：`.env.example` 里 `COMPOSE_PROFILES=warp` 那行是注释掉的，所以普通的 `docker compose up` 不会起 WARP。
- 打开：把 `.env` 里那行的注释去掉。宿主机的 `/dev/net/tun` 要可用，容器需要 `NET_ADMIN` 权限（compose 里已经写好）。
- 起了以后，出口组 `warp` 指向 `socks5h://warp:4000`，账号用 `mmxcode accounts proxy <id> --group warp` 就走 WARP。

## 协议对齐

上游行为对齐社区插件 [`@magpie-community/opencode-minimax-auth`](https://github.com/magpie-community/plugins/tree/main/packages/minimax) 的 `packages/minimax/index.mjs`：端点、请求签名、刷新语义、签到语义、配额拒绝的判定，都以它为准。

### 上游只认 Anthropic 那条路

参考插件只会打 Anthropic 的 `/messages`（它接的是 `@ai-sdk/anthropic`），它从来没走过 OpenAI 的 `/chat/completions`，也没走过 Codex 的 `/responses`。所以这两种客户端协议都备着本地转换：

- **OpenAI**：先按原样发给上游的 `/chat/completions`；上游回 404（就是没有这个接口）时，改成本地转成 Anthropic 的 `/messages` 再发一次，回来的答案转回 OpenAI 的形状。这个判断只做一次，之后都走本地转换。
- **Codex**：同样的办法，先试 `/responses`，404 就转成 Anthropic 再发。

转换出来的形状是客户端真的能用的：Codex 的非流式回答里，工具调用是 `function_call` 条目（`call_id`、`name`、`arguments` 都在），不是一段空文本。

## 计划

两个计划都已实现：

- 计划 A：核心库与 CLI（`docs/superpowers/plans/2026-11-13-mmxcode-core.md`）
- 计划 B：HTTP 网关（Anthropic / OpenAI / Codex 三协议）、WebUI、Docker 部署（`docs/superpowers/plans/2026-11-13-mmxcode-gateway.md`）

### 不做的

图片生成、视频生成、语音、web search 这些**不做**。参考插件通过 MiniMax 的 Matrix 连接器（`/minimax-cloud/api/v1/connectors/*`）用得上它们，但那些是另一个量级的工作量，要做的时候单开一个计划 —— 设计文档 §1.5 记着调研到的东西。
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

### Task 13: 实现期新增的文件（CLI 的测试）

计划里原来只有 CLI 的实现。实现的时候给它补上了测试：命令行的参数、
账号的出口、签到、serve 的开关，各有各的坑，测试把它们钉住。

**Files:**
- Create: `cmd/mmxcode/root_test.go`
- Create: `cmd/mmxcode/accounts_test.go`
- Create: `cmd/mmxcode/checkin_test.go`
- Create: `cmd/mmxcode/serve_test.go`

- [ ] **Step 1: 写测试**

创建 `cmd/mmxcode/root_test.go`：

```go
package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// captureStdout runs fn and gives back what it wrote to the standard
// output.
//
// The commands write with fmt.Print, not with the command's own writer,
// so the only way to see what they printed is to catch the file
// descriptor.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

func TestRootHasEveryCommand(t *testing.T) {
	root := newRoot()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"login", "accounts", "checkin", "usage", "models"} {
		if !have[want] {
			t.Errorf("the root command has no %q command", want)
		}
	}
}

func TestModelsPrintsTheFallbackList(t *testing.T) {
	cmd := newModelsCmd(&env{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Errorf("models: %v", err)
		}
	})
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines++
	}
	if lines != 4 {
		t.Errorf("models printed %d lines, want 4:\n%s", lines, out)
	}
	if !strings.Contains(out, "MiniMax-M3") {
		t.Errorf("models did not mention MiniMax-M3:\n%s", out)
	}
}

func TestAccountsListWithNoAccounts(t *testing.T) {
	e := testEnv(t)
	out := captureStdout(t, func() {
		cmd := newAccountsListCmd(e)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err != nil {
			t.Errorf("accounts list: %v", err)
		}
	})
	if !strings.Contains(out, "还没有账号") {
		t.Errorf("accounts list said nothing about having no accounts:\n%s", out)
	}
}

func TestAccountsListShowsTheAccount(t *testing.T) {
	e := testEnv(t)
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", UserID: "u1", Email: "one@example.com",
		Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	out := captureStdout(t, func() {
		cmd := newAccountsListCmd(e)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err != nil {
			t.Errorf("accounts list: %v", err)
		}
	})
	for _, want := range []string{"acc_1", "一号", "one@example.com", "active"} {
		if !strings.Contains(out, want) {
			t.Errorf("accounts list did not show %q:\n%s", want, out)
		}
	}
}

func TestAccountsProxyRefusesAnUnknownGroup(t *testing.T) {
	e := testEnv(t)
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	cmd := newAccountsProxyCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"acc_1", "--group", "nope"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("accounts proxy took an unknown group")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the error does not name the group: %v", err)
	}
}

func TestAccountsProxySetsTheGroup(t *testing.T) {
	e := testEnv(t)
	e.cfg.Proxy.Groups = map[string]string{"hk": "http://127.0.0.1:1:8080"}
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	cmd := newAccountsProxyCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"acc_1", "--group", "hk"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy: %v", err)
	}
	a, err := e.store.GetAccount(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if a.ProxyGroup != "hk" {
		t.Errorf("the account's group is %q, want %q", a.ProxyGroup, "hk")
	}
}

func TestUsageWithNothingReadYet(t *testing.T) {
	e := testEnv(t)
	out := captureStdout(t, func() {
		cmd := newUsageCmd(e)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err != nil {
			t.Errorf("usage: %v", err)
		}
	})
	if !strings.Contains(out, "总积分") {
		t.Errorf("usage printed no totals:\n%s", out)
	}
}

func TestUsageJSON(t *testing.T) {
	e := testEnv(t)
	out := captureStdout(t, func() {
		cmd := newUsageCmd(e)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"--json"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("usage: %v", err)
		}
	})
	if !strings.Contains(out, "\"total_granted\"") {
		t.Errorf("usage --json printed no totals:\n%s", out)
	}
}

func TestNewAccountIDIsNotEmpty(t *testing.T) {
	if !strings.HasPrefix(newAccountID(), "acc_") {
		t.Errorf("the account id does not start with acc_: %q", newAccountID())
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "c"); got != "c" {
		t.Errorf("firstNonEmpty gave %q, want %q", got, "c")
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty of nothing gave %q, want %q", got, "")
	}
}

func TestGroupNames(t *testing.T) {
	got := groupNames(map[string]string{"b": "", "a": ""})
	if got != "a, b" {
		t.Errorf("groupNames gave %q, want %q", got, "a, b")
	}
}

// testEnv gives an environment with a store in a temporary file.
func testEnv(t *testing.T) *env {
	t.Helper()
	path := t.TempDir() + "/mmxcode.db"
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &env{cfg: config.Default(), store: s}
}

// TestAccountsRemoveSaysThereIsNoSuchAccount says removing an account
// that is not there says so in words a person can act on.
//
// 删一个没有的账号，要说"没有这个账号"，不是"store: not found"。
func TestAccountsRemoveSaysThereIsNoSuchAccount(t *testing.T) {
	e := testEnv(t)
	cmd := newAccountsRemoveCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"acc_nope"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("accounts remove took an account that is not there")
	}
	if !strings.Contains(err.Error(), "no such account") {
		t.Errorf("the error does not say there is no such account: %v", err)
	}
	if !strings.Contains(err.Error(), "acc_nope") {
		t.Errorf("the error does not name the account: %v", err)
	}
}

// TestAccountsProxyKeepsTheOtherField says changing the group leaves
// the address alone, and the other way round.
//
// 改组不动地址，改地址不动组。
func TestAccountsProxyKeepsTheOtherField(t *testing.T) {
	e := testEnv(t)
	e.cfg.Proxy.Groups = map[string]string{"hk": "http://127.0.0.1:1:8080"}
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	cmd := newAccountsProxyCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"acc_1", "--url", "http://127.0.0.1:2:8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy --url: %v", err)
	}

	cmd = newAccountsProxyCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"acc_1", "--group", "hk"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy --group: %v", err)
	}

	a, err := e.store.GetAccount(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if a.ProxyGroup != "hk" {
		t.Errorf("the group is %q, want %q", a.ProxyGroup, "hk")
	}
	if a.ProxyURL != "http://127.0.0.1:2:8080" {
		t.Errorf("the address is %q, want it kept", a.ProxyURL)
	}
}

// TestAccountsProxyDoesNotUndoASignOut says changing an account's
// outlet does not put a signed-out account back to active.
//
// 改出口不能把登出的账号改回 active。
func TestAccountsProxyDoesNotUndoASignOut(t *testing.T) {
	e := testEnv(t)
	e.cfg.Proxy.Groups = map[string]string{"hk": "http://127.0.0.1:1:8080"}
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := e.store.UpdateAccountStatus(context.Background(), "acc_1",
		store.StatusSignedOut, "the site refused the token", now); err != nil {
		t.Fatalf("sign out: %v", err)
	}

	cmd := newAccountsProxyCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"acc_1", "--group", "hk"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy: %v", err)
	}

	a, err := e.store.GetAccount(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if a.Status != store.StatusSignedOut {
		t.Errorf("the status is %q, want %q", a.Status, store.StatusSignedOut)
	}
	if a.LastError != "the site refused the token" {
		t.Errorf("the last error is %q", a.LastError)
	}
}
```

创建 `cmd/mmxcode/accounts_test.go`：

```go
package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// TestAccountsListKeepsTheProxySecret says the proxy's credentials are
// not printed.
//
// 代理地址里的账号密码是秘密。`mmxcode accounts list` 的输出会留在终端
// 的回滚里，也会被 CI 收进日志里。
func TestAccountsListKeepsTheProxySecret(t *testing.T) {
	e := testEnv(t)
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		ProxyURL:  "socks5h://bob:hunter2@127.0.0.1:1080",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	out := captureStdout(t, func() {
		cmd := newAccountsListCmd(e)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		if err := cmd.Execute(); err != nil {
			t.Errorf("accounts list: %v", err)
		}
	})
	if strings.Contains(out, "hunter2") {
		t.Errorf("the proxy's password is in the output:\n%s", out)
	}
	if strings.Contains(out, "bob") {
		t.Errorf("the proxy's name is in the output:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:1080") {
		t.Errorf("the proxy's host is not in the output:\n%s", out)
	}
}

// TestAccountsListKeepsTheDefaultProxySecret says the global default
// proxy's credentials are not printed either.
//
// 全局默认的代理地址也一样：账号没有自己的地址时打印的就是它。
func TestAccountsListKeepsTheDefaultProxySecret(t *testing.T) {
	e := testEnv(t)
	e.cfg.Proxy.Default = "http://carol:s3cret@proxy.example:8080"
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	out := captureStdout(t, func() {
		cmd := newAccountsListCmd(e)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		if err := cmd.Execute(); err != nil {
			t.Errorf("accounts list: %v", err)
		}
	})
	if strings.Contains(out, "s3cret") {
		t.Errorf("the default proxy's password is in the output:\n%s", out)
	}
	if !strings.Contains(out, "proxy.example:8080") {
		t.Errorf("the default proxy's host is not in the output:\n%s", out)
	}
}

// TestAccountsListShowsTheGroupName says a group's name is printed as
// it is: a name is not a secret.
//
// 组名不是秘密，照原样打印，才看得出这个账号走的是哪一组。
func TestAccountsListShowsTheGroupName(t *testing.T) {
	e := testEnv(t)
	e.cfg.Proxy.Groups = map[string]string{"hk": "socks5h://bob:hunter2@127.0.0.1:1080"}
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		ProxyGroup: "hk", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	out := captureStdout(t, func() {
		cmd := newAccountsListCmd(e)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		if err := cmd.Execute(); err != nil {
			t.Errorf("accounts list: %v", err)
		}
	})
	if strings.Contains(out, "hunter2") {
		t.Errorf("the group's password is in the output:\n%s", out)
	}
	if !strings.Contains(out, "hk") {
		t.Errorf("the group is not in the output:\n%s", out)
	}
}
```

创建 `cmd/mmxcode/checkin_test.go`：

```go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// checkinServer is a stand-in for MiniMax: the token endpoint, the
// sign-in panel and the claim.
//
// 签到的测试不能碰真的站点：一次续期花掉一个 refresh token。
func checkinServer(t *testing.T, refuse bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var refreshes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			refreshes.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-access",
				"refresh_token": "new-refresh",
				"expires_in":    3600,
			})
		case "/minimax-cloud/api/v1/signin/status":
			if refuse {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"base_resp":{"status_code":1000048}}`))
				return
			}
			w.Write([]byte(`{"data":{"days":[
				{"day_no":1,"status":3,"is_today":false,"points":25},
				{"day_no":2,"status":3,"is_today":false,"points":25},
				{"day_no":3,"status":3,"is_today":false,"points":25},
				{"day_no":4,"status":2,"is_today":true,"points":25},
				{"day_no":5,"status":1,"is_today":false,"points":25},
				{"day_no":6,"status":1,"is_today":false,"points":25},
				{"day_no":7,"status":1,"is_today":false,"points":25}]}}`))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &refreshes
}

// testEnvWithServer gives an environment whose accounts are served by
// the stand-in.
func testEnvWithServer(t *testing.T, srv *httptest.Server) *env {
	t.Helper()
	e := testEnv(t)
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	// 账号的请求要走到那台假服务器上，不然测试就问真的站点去了。
	acc.SetBase(srv.URL, srv.URL)
	e.up = up
	e.acc = acc
	e.egr = egress.New(nil, "")
	e.mgr = manager.New(e.store, up, acc, e.egr, oauth.NewRenewer())
	e.mgr.TokenURL = srv.URL + "/oauth2/token"
	return e
}

// seedAccount gives an account whose token is old.
func seedAccount(t *testing.T, e *env, expires time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := e.store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "一号", UserID: "u1", Email: "one@example.com",
		Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := e.store.SaveToken(ctx, store.Token{
		AccountID: "acc_1", Access: "old-access", Refresh: "old-refresh",
		ExpiresAt: expires, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save token: %v", err)
	}
}

// TestCheckinRenewsTheToken says the command renews the token before
// it checks in.
//
// 90 分钟没人续期，命令自己拿库里那份令牌去签到，站点回 401，签到
// 根本没发生，命令却说是成功的。
func TestCheckinRenewsTheToken(t *testing.T) {
	srv, refreshes := checkinServer(t, false)
	e := testEnvWithServer(t, srv)
	// 一个半小时以前就签到的账号：令牌早就过期了。
	seedAccount(t, e, time.Now().Add(-90*time.Minute))

	var err error
	out := captureStdout(t, func() {
		cmd := newCheckinCmd(e)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("checkin: %v\n%s", err, out)
	}
	if refreshes.Load() != 1 {
		t.Errorf("the token was renewed %d times, want 1", refreshes.Load())
	}
	if !strings.Contains(out, "claimed") {
		t.Errorf("the check-in was not claimed:\n%s", out)
	}
	// 记录是 manager 写的，日子是站点的那一天。
	rows, err := e.store.ListCheckins(context.Background(), "acc_1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the check-in table has %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Day != store.DayKey(time.Now()) {
		t.Errorf("the record is for %q, want %q", rows[0].Day, store.DayKey(time.Now()))
	}
	if rows[0].Outcome != "claimed" {
		t.Errorf("the record says %q, want %q", rows[0].Outcome, "claimed")
	}
}

// TestCheckinFailsWhenTheSignInIsRefused says a refused check-in makes
// the command fail.
//
// 被拒的签到不是签到：命令要报错，cron 才不会每天都看着是成功。
func TestCheckinFailsWhenTheSignInIsRefused(t *testing.T) {
	srv, _ := checkinServer(t, true)
	e := testEnvWithServer(t, srv)
	seedAccount(t, e, time.Now().Add(-90*time.Minute))

	var err error
	out := captureStdout(t, func() {
		cmd := newCheckinCmd(e)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	if err == nil {
		t.Fatalf("the command succeeded although the sign-in was refused:\n%s", out)
	}
	if !strings.Contains(out, "failed") {
		t.Errorf("the command did not say the check-in failed:\n%s", out)
	}
}

// TestCheckinOnlyOneAccount says the --account flag picks one.
func TestCheckinOnlyOneAccount(t *testing.T) {
	srv, refreshes := checkinServer(t, false)
	e := testEnvWithServer(t, srv)
	seedAccount(t, e, time.Now().Add(-90*time.Minute))
	ctx := context.Background()
	now := time.Now()
	if err := e.store.CreateAccount(ctx, store.Account{
		ID: "acc_2", Name: "二号", UserID: "u2", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := e.store.SaveToken(ctx, store.Token{
		AccountID: "acc_2", Access: "old-access", Refresh: "old-refresh",
		ExpiresAt: now.Add(-90 * time.Minute), UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save token: %v", err)
	}

	cmd := newCheckinCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--account", "acc_2"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Errorf("checkin: %v", err)
		}
	})
	if strings.Contains(out, "一号") {
		t.Errorf("the other account was checked in too:\n%s", out)
	}
	if refreshes.Load() != 1 {
		t.Errorf("the token was renewed %d times, want 1", refreshes.Load())
	}
}
```

创建 `cmd/mmxcode/serve_test.go`：

```go
package main

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// TestIsLoopback says which listen addresses stay on this machine.
//
// ":8080" and "0.0.0.0:8080" mean every interface of the machine, so
// they are not loopback: with no api key they would hand the models to
// whoever can reach the machine.
func TestIsLoopback(t *testing.T) {
	cases := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8080", true},
		{"127.0.0.5:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"192.168.1.5:8080", false},
	}
	for _, c := range cases {
		if got := isLoopback(c.listen); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.listen, got, c.want)
		}
	}
}

// serveCmdFor gives a serve command that says nothing but its error.
func serveCmdFor(t *testing.T, cfg config.Config) *cobra.Command {
	t.Helper()
	cmd := newServeCmd(&env{cfg: cfg})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd
}

// TestServeRefusesWithoutAnAPIKeyOnAWideAddress says the gateway does
// not come up for everyone when nobody has to give a key.
func TestServeRefusesWithoutAnAPIKeyOnAWideAddress(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "192.168.1.5:8080"} {
		cfg := config.Default()
		cfg.Server.Listen = listen
		cfg.Server.APIKeys = nil
		err := serveCmdFor(t, cfg).Execute()
		if err == nil {
			t.Fatalf("serve started on %s with no api key", listen)
		}
		if !strings.Contains(err.Error(), "no api key is configured") {
			t.Errorf("the error for %s does not say the key is missing: %v", listen, err)
		}
	}
}

// TestServeRefusesANonGlobalSite says the Chinese site is refused
// rather than served with the wrong token.
func TestServeRefusesANonGlobalSite(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.Site = "cn"
	err := serveCmdFor(t, cfg).Execute()
	if err == nil {
		t.Fatal("serve started with upstream.site = cn")
	}
	if !strings.Contains(err.Error(), "only \"global\" is supported") {
		t.Errorf("the error does not name the site: %v", err)
	}
}

// TestServeRefusesABadCheckinTime says a check-in time that cannot be
// read is refused instead of quietly run at the default time.
func TestServeRefusesABadCheckinTime(t *testing.T) {
	cfg := config.Default()
	cfg.Schedule.CheckinAt = "nine o'clock"
	err := serveCmdFor(t, cfg).Execute()
	if err == nil {
		t.Fatal("serve started with a check-in time that cannot be read")
	}
	if !strings.Contains(err.Error(), "want HH:MM") {
		t.Errorf("the error does not say the format: %v", err)
	}
}

// TestServeStartsOnLoopbackWithoutAnAPIKey says a loopback address
// with no api key is fine: only this machine can reach it.
//
// The port is taken first, so the server cannot come up and the run
// ends at once. What matters here is which error comes back: not the
// one about the missing key.
func TestServeStartsOnLoopbackWithoutAnAPIKey(t *testing.T) {
	err := serveUntilThePortIsTaken(t, "127.0.0.1", nil)
	if err == nil {
		t.Fatal("serve came up on a port that was already taken")
	}
	if strings.Contains(err.Error(), "no api key is configured") {
		t.Errorf("serve refused a loopback address with no api key: %v", err)
	}
}

// TestServeAllowsAWideAddressWithAnAPIKey says a wide address is fine
// when a key is configured: the key is what keeps the gateway closed.
func TestServeAllowsAWideAddressWithAnAPIKey(t *testing.T) {
	err := serveUntilThePortIsTaken(t, "0.0.0.0", []string{"sk-local-test"})
	if err == nil {
		t.Fatal("serve came up on a port that was already taken")
	}
	if strings.Contains(err.Error(), "no api key is configured") {
		t.Errorf("serve refused a wide address that has an api key: %v", err)
	}
}

// serveUntilThePortIsTaken runs serve on an address whose port is
// already taken, so the run ends at once, and gives back the error.
func serveUntilThePortIsTaken(t *testing.T, host string, keys []string) error {
	t.Helper()
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	cfg := config.Default()
	cfg.Server.Listen = addr
	cfg.Server.APIKeys = keys

	cmd := newServeCmd(serveEnv(t, cfg))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

// serveEnv gives an environment with a store and a manager, the way
// the root command builds them.
func serveEnv(t *testing.T, cfg config.Config) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mmxcode.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	egr := egress.New(cfg.Proxy.Groups, cfg.Proxy.Default)
	tr, err := egr.Transport(egr.Resolve(store.Account{}))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	up := upstream.New(upstream.Options{Timeout: 30 * time.Second, Transport: tr})
	acc := account.New(up)
	return &env{
		cfg:   cfg,
		store: s,
		up:    up,
		egr:   egr,
		acc:   acc,
		mgr:   manager.New(s, up, acc, egr, oauth.NewRenewer()),
	}
}
```




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
