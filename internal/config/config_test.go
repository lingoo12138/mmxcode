package config

import (
	"os"
	"path/filepath"
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
	// Go 把 90 分钟写成 "1h30m0s"，yaml 也就不必加引号。
	if string(b) != "1h30m0s\n" {
		t.Fatalf("marshalled = %q", b)
	}
	// 写出去的东西要能读回来。
	var back Duration
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Duration() != 90*time.Minute {
		t.Fatalf("round trip = %v, want 90m", back.Duration())
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

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	// 键名写错时必须报错。拼错一个出口组名，静默走全局默认是很坏的失败方式。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "server:\n  listenn: \":9090\"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for the unknown key")
	}
}

func TestLoadEmptyFile(t *testing.T) {
	// 空文件不是错误：默认值就是完整配置。
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, ""); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" {
		t.Fatalf("listen = %q, want :8080", c.Server.Listen)
	}
}

func TestLoadCommentOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, "# 只有注释\n"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" {
		t.Fatalf("listen = %q, want :8080", c.Server.Listen)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
