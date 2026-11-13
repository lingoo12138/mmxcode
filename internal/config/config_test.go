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

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
