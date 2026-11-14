package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/secret"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// aReloginEnv gives an environment whose re-login never touches the
// network.
func aReloginEnv(t *testing.T) (*env, secret.Key) {
	t.Helper()
	e := testEnv(t)
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	e.up = up
	e.acc = account.New(up)
	e.egr = egress.New(nil, "")
	e.mgr = manager.New(e.store, up, e.acc, e.egr, oauth.NewRenewer())
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	e.mgr.Key = key
	e.mgr.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		if password != "hunter2" {
			t.Errorf("password = %q", password)
		}
		return e.store.SaveToken(ctx, store.Token{
			AccountID: acc.ID, Access: "a", Refresh: "r",
			ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
		})
	}
	return e, key
}

// seedSignedOut gives an account that is signed out and has a password.
func seedSignedOut(t *testing.T, e *env, key secret.Key, id string) {
	t.Helper()
	now := time.Now()
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateAccount(context.Background(), store.Account{
		ID: id, Name: id, Email: id + "@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetPassword(context.Background(), id, cipher, now); err != nil {
		t.Fatal(err)
	}
}

func TestReloginWantsAllOrAnAccount(t *testing.T) {
	// 什么都不说就重登，是一个惊喜等着人：说清楚要哪一个。
	var out bytes.Buffer
	err := runRelogin(context.Background(), &env{}, &out, "", "", false)
	if err == nil {
		t.Fatal("relogin without --all and without --account said nothing")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("the error does not mention --all: %v", err)
	}
}

func TestReloginSignsOneAccountInAgain(t *testing.T) {
	e, key := aReloginEnv(t)
	seedSignedOut(t, e, key, "acc_1")
	var out bytes.Buffer
	if err := runRelogin(context.Background(), e, &out, "acc_1", "", false); err != nil {
		t.Fatal(err)
	}
	a, err := e.store.GetAccount(context.Background(), "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusActive {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusActive)
	}
	if !strings.Contains(out.String(), "acc_1") {
		t.Errorf("the output does not name the account: %q", out.String())
	}
}

func TestReloginAllSignsThemAllInAgain(t *testing.T) {
	e, key := aReloginEnv(t)
	seedSignedOut(t, e, key, "acc_1")
	seedSignedOut(t, e, key, "acc_2")
	var out bytes.Buffer
	if err := runRelogin(context.Background(), e, &out, "", "", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "成功 2") {
		t.Errorf("the output does not count the accounts: %q", out.String())
	}
	for _, id := range []string{"acc_1", "acc_2"} {
		a, err := e.store.GetAccount(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if a.Status != store.StatusActive {
			t.Errorf("%s: status = %q, want %q", id, a.Status, store.StatusActive)
		}
	}
}

func TestReloginSaysWhenAnAccountCouldNotBeSignedIn(t *testing.T) {
	e, key := aReloginEnv(t)
	seedSignedOut(t, e, key, "acc_1")
	e.mgr.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return errors.New("the site refused the password")
	}
	var out bytes.Buffer
	err := runRelogin(context.Background(), e, &out, "", "", true)
	if err == nil {
		t.Fatal("relogin said nothing about the account that could not be signed in")
	}
	a, err := e.store.GetAccount(context.Background(), "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Errorf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
}

func TestImportHasATimeoutFlag(t *testing.T) {
	// 整趟导入得有个时限：清单很大的时候，人不想等到天亮。
	cmd := newImportCmd(&env{})
	f := cmd.Flags().Lookup("timeout")
	if f == nil {
		t.Fatal("the import command has no --timeout flag")
	}
	if f.DefValue != "0s" {
		t.Errorf("the timeout defaults to %q, want %q", f.DefValue, "0s")
	}
}

func TestReloginRefusesAnUnknownGroup(t *testing.T) {
	// 组名写错而默默走全局默认出口，是最坏的一种错：看着跑通了，
	// 其实请求根本没走要去的那条路。
	e := testEnv(t)
	e.cfg.Proxy.Groups = map[string]string{"hk": "http://127.0.0.1:1:8080"}
	err := e.reloginFn("nope")(context.Background(), store.Account{ID: "acc_1"}, "pw")
	if err == nil {
		t.Fatal("relogin took an outlet group that is not there")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the error does not name the group: %v", err)
	}
}

// TestTheReloginHelpSaysTheQueueIsOneProcess says the relogin command
// says that its queue only counts inside one process.
//
// 排队是进程内的：这条命令跟一个正在跑的 serve 用同一个库的时候，两个
// 进程可能同时登同一个账号。这件事得在命令的说明里写着，不然人以为
// 「同一个账号不会同时登两遍」在任何时候都成立。
func TestTheReloginHelpSaysTheQueueIsOneProcess(t *testing.T) {
	e := testEnv(t)
	cmd := newReloginCmd(e)
	if !strings.Contains(cmd.Long, "进程") {
		t.Fatalf("the help does not say the queue is one process: %q", cmd.Long)
	}
	if !strings.Contains(cmd.Long, "serve") {
		t.Fatalf("the help does not mention the other process: %q", cmd.Long)
	}
}
