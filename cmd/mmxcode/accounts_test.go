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

// TestAccountsListShowsTheAddress says the account's own address is
// printed, with the credentials taken out.
//
// 地址里的账号和密码是秘密，而这份输出会进到终端的回滚里，也会进到 CI
// 的日志里。
func TestAccountsListShowsTheAddress(t *testing.T) {
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
		t.Errorf("the address's password is in the output:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:1080") {
		t.Errorf("the address is not in the output:\n%s", out)
	}
}
