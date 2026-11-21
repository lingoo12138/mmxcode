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

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/egress"
	"github.com/lingoo12138/mmxcode/internal/manager"
	"github.com/lingoo12138/mmxcode/internal/oauth"
	"github.com/lingoo12138/mmxcode/internal/store"
	"github.com/lingoo12138/mmxcode/internal/upstream"
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
	e.egr = egress.New("")
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
