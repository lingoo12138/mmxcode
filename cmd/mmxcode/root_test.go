package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	root, _ := newRoot()
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
	if lines != 5 {
		t.Errorf("models printed %d lines, want 5:\n%s", lines, out)
	}
	if !strings.Contains(out, "MiniMax-M3") {
		t.Errorf("models did not mention MiniMax-M3:\n%s", out)
	}
	if !strings.Contains(out, "MiniMax-M3.1-Flash") {
		t.Errorf("models did not mention MiniMax-M3.1-Flash:\n%s", out)
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

func TestAccountsProxyRefusesAnUnknownAccount(t *testing.T) {
	e := testEnv(t)
	cmd := newAccountsProxyCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"acc_nope", "--url", "http://127.0.0.1:8080"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("accounts proxy took an account that is not there")
	}
	if !strings.Contains(err.Error(), "acc_nope") {
		t.Errorf("the error does not name the account: %v", err)
	}
}

func TestAccountsProxySetsTheAddress(t *testing.T) {
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
	cmd.SetArgs([]string{"acc_1", "--url", "http://127.0.0.1:8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy: %v", err)
	}
	a, err := e.store.GetAccount(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if a.ProxyURL != "http://127.0.0.1:8080" {
		t.Errorf("the account's address is %q, want %q", a.ProxyURL, "http://127.0.0.1:8080")
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

// TestUsageRefreshReadsTheCreditsNow says the command can ask for a
// reading now.
//
// 命令行里的数也是上一次读数时的数。要看现在的数，就得能说一声「现在
// 读一遍」，而不是等着下一个周期。
func TestUsageRefreshReadsTheCreditsNow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"100","remaining_amount":"500","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		case "/backend/account/token_plan/remains_percent":
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	e := testEnvWithServer(t, srv)
	seedAccount(t, e, time.Now().Add(time.Hour))

	out := captureStdout(t, func() {
		cmd := newUsageCmd(e)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"--refresh"})
		if err := cmd.Execute(); err != nil {
			t.Errorf("usage: %v", err)
		}
	})
	if !strings.Contains(out, "600") {
		t.Errorf("the command did not read the credits:\n%s", out)
	}
	if !strings.Contains(out, "读了") {
		t.Errorf("the command did not say how many it read:\n%s", out)
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
// TestAccountsProxyKeepsTheOtherField says changing the outlet does
// not put a signed-out account back to active.
//
// 改出口不能把登出的账号改回 active。
func TestAccountsProxyKeepsTheOtherField(t *testing.T) {
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
	cmd.SetArgs([]string{"acc_1", "--url", "http://127.0.0.1:2:8080"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy --url: %v", err)
	}

	a, err := e.store.GetAccount(context.Background(), "acc_1")
	if err != nil {
		t.Fatalf("get account: %v", err)
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
	cmd.SetArgs([]string{"acc_1", "--url", "http://127.0.0.1:8080"})
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

// TestTheApiKeysComeFromTheEnvironment says the gateway's api keys can
// come from the environment.
//
// 镜像里不带密钥：密钥从环境变量进来，配置文件里可以一把都没有。密钥
// 是申请来的，写进镜像就等于把它摊在别人眼前。
func TestTheApiKeysComeFromTheEnvironment(t *testing.T) {
	t.Setenv(APIKeysEnv, "sk-one, sk-two")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen: \"127.0.0.1:8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, e := newRoot()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--config", path, "--db", filepath.Join(dir, "t.db"), "usage"})
	if err := root.Execute(); err != nil {
		t.Fatalf("usage: %v", err)
	}
	keys := e.cfg.Server.APIKeys
	if len(keys) != 2 || keys[0] != "sk-one" || keys[1] != "sk-two" {
		t.Fatalf("api keys = %q, want the two from the environment", keys)
	}
}

// TestAnEmptyApiKeyInTheEnvironmentIsDropped says a key that is only a
// space is not a key.
//
// 空的一把 key 等于没有 key：一个不带 key 的请求带来的也是空串，两边
// 一比就"相等"了——于是所有请求都过了这道门。
func TestAnEmptyApiKeyInTheEnvironmentIsDropped(t *testing.T) {
	t.Setenv(APIKeysEnv, "sk-one, ,  ,sk-two")
	keys := apiKeysFromEnv(os.Getenv(APIKeysEnv))
	if len(keys) != 2 {
		t.Fatalf("keys = %q, want the two real ones", keys)
	}
}
