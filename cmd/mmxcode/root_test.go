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
