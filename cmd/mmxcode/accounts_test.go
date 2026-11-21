package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/store"
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

// TestTheAccountCanPointAtAGroup says an account's outlet can be a
// group, and that the address and the group are one thing.
//
// 地址和组是二选一：账号只有一条出口。给了组，账号自己的地址就不算了。
func TestTheAccountCanPointAtAGroup(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	now := time.Now()
	if err := e.store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		ProxyURL:  "http://old:8080",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	cmd := newAccountsProxyCmd(e)
	cmd.SetArgs([]string{"acc_1", "--group", "clash"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accounts proxy: %v", err)
	}
	a, err := e.store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if a.ProxyGroup != "clash" {
		t.Errorf("the account's group is %q, want %q", a.ProxyGroup, "clash")
	}
	// 给了组，账号自己的地址就不算了。
	if a.ProxyURL != "" {
		t.Errorf("the account still has an address of its own: %q", a.ProxyURL)
	}

	// 两个一起给是不行的。
	cmd = newAccountsProxyCmd(e)
	cmd.SetArgs([]string{"acc_1", "--url", "http://a:8080", "--group", "clash"})
	if err := cmd.Execute(); err == nil {
		t.Error("the address and the group were both taken")
	}
}

// TestTheAccountListSaysTheGroup says the list says which group an
// account is on.
//
// 一列「出口组」：人不用去猜这个号走的是哪条路。
func TestTheAccountListSaysTheGroup(t *testing.T) {
	e := testEnv(t)
	now := time.Now()
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_1", Name: "一号", Status: store.StatusActive,
		ProxyGroup: "clash", CreatedAt: now, UpdatedAt: now,
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
	if !strings.Contains(out, "clash") {
		t.Errorf("the group is not in the output:\n%s", out)
	}
}
