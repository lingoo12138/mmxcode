package manager

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/checkin"
	"mmxcode/internal/egress"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

func newTestManager(t *testing.T, srv *httptest.Server) *Manager {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	m := New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())
	if srv != nil {
		// The token endpoint and the account API are pointed at the
		// test server. Without this the test talks to MiniMax itself,
		// and a refresh token it invents is spent for real.
		m.TokenURL = srv.URL + site.TokenPath
		acc.SetBase(srv.URL, srv.URL)
	}
	return m
}

func TestTokenReturnsStoredToken(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, nil)
	now := time.Now()
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	tok, err := m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc" {
		t.Fatalf("access = %q", tok.Access)
	}
}

func TestTokenRefreshesWhenAboutToExpire(t *testing.T) {
	var refreshed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshed++
		w.Write([]byte(`{"access_token":"new","refresh_token":"newref","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "old", Refresh: "ref",
		// 两分钟后过期，落在续期窗口内
		ExpiresAt: now.Add(2 * time.Minute), UpdatedAt: now,
	})

	tok, err := m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new" {
		t.Fatalf("access = %q, want the refreshed one", tok.Access)
	}
	if refreshed != 1 {
		t.Fatalf("refreshed %d times", refreshed)
	}
	// 新 token 要落库
	stored, err := m.Store.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Access != "new" || stored.Refresh != "newref" {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestTokenRefreshOnlyOnceUnderConcurrency(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(`{"access_token":"new","refresh_token":"newref","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "old", Refresh: "ref",
		ExpiresAt: now.Add(time.Minute), UpdatedAt: now,
	})

	done := make(chan struct{}, 6)
	for i := 0; i < 6; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := m.Token(ctx, "acc_01"); err != nil {
				t.Errorf("Token: %v", err)
			}
		}()
	}
	for i := 0; i < 6; i++ {
		<-done
	}
	if calls != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", calls)
	}
}

func TestTokenMarksAccountSignedOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "old", Refresh: "ref",
		ExpiresAt: now.Add(time.Minute), UpdatedAt: now,
	})

	if _, err := m.Token(ctx, "acc_01"); err == nil {
		t.Fatal("expected an error")
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
}

func TestClientUsesAccountEgress(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, nil)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive,
		ProxyURL: "http://127.0.0.1:7890", CreatedAt: now, UpdatedAt: now,
	})
	c, url, err := m.Client(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://127.0.0.1:7890" {
		t.Fatalf("url = %q", url)
	}
	if c == nil {
		t.Fatal("no client")
	}
}

func TestRefreshUsage(t *testing.T) {
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

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	snap, err := m.RefreshUsage(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.HasAmounts || snap.Granted != 600 || snap.Remaining != 500 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Consumed != 100 {
		t.Fatalf("consumed = %v, want 100", snap.Consumed)
	}
}

func TestCheckinDelegates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
			w.Write([]byte(`{"data":{"days":[
				{"day_no":1,"status":3,"is_today":false,"points":25},
				{"day_no":2,"status":3,"is_today":false,"points":25},
				{"day_no":3,"status":2,"is_today":true,"points":25},
				{"day_no":4,"status":1,"is_today":false,"points":25},
				{"day_no":5,"status":1,"is_today":false,"points":25},
				{"day_no":6,"status":1,"is_today":false,"points":25},
				{"day_no":7,"status":1,"is_today":false,"points":25}]}}`))
		case "/minimax-cloud/api/v1/signin/claim":
			w.Write([]byte(`{"data":{"claim_result":1,"points":25}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	out, err := m.Checkin(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome != checkin.OutcomeClaimed {
		t.Fatalf("outcome = %q", out.Outcome)
	}
}

// TestRefreshUsageSendsTheGroup says the plan is asked for the group's
// windows.
//
// 会员回答里有 op_group_id，控制台要带上 X-Group-Id 才会给那一组的窗口。
func TestRefreshUsageSendsTheGroup(t *testing.T) {
	var groups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matrix/api/v1/commerce/get_membership_info":
			w.Write([]byte(`{"data":{"has_token_plan":true,"op_group_id":"grp-1"}}`))
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"100","remaining_amount":"500","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		case "/backend/account/token_plan/remains_percent":
			groups = append(groups, r.Header.Get("X-Group-Id"))
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[
				{"model_name":"general","current_interval_status":2,"current_interval_remaining_percent":0,
				 "current_interval_total_percent":100,"current_interval_total_count":150,"current_interval_used_count":24}
			]}`))
		case "/v1/api/openplatform/charge/combo/cycle_audio_resource_package":
			groups = append(groups, r.Header.Get("X-Group-Id"))
			w.Write([]byte(`{"current_subscribe":{"current_subscribe_title":"TokenPlanMax"}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	snap, err := m.RefreshUsage(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("the plan was asked %d times, want 2", len(groups))
	}
	for _, g := range groups {
		if g != "grp-1" {
			t.Fatalf("X-Group-Id = %q, want grp-1", g)
		}
	}
	if snap.Plan != "TokenPlanMax" {
		t.Fatalf("plan = %q", snap.Plan)
	}
	if snap.WindowsJSON == "" {
		t.Fatal("no windows")
	}
}

// TestRefreshUsageWithoutAGroup says no group, no header.
//
// 会员不知道的时候就没有组，控制台也不该收到一个空的组。
func TestRefreshUsageWithoutAGroup(t *testing.T) {
	var groups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matrix/api/v1/commerce/get_membership_info":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`404 page not found`))
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"100","remaining_amount":"500","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		case "/backend/account/token_plan/remains_percent":
			groups = append(groups, r.Header.Get("X-Group-Id"))
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	if _, err := m.RefreshUsage(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != "" {
			t.Fatalf("X-Group-Id = %q, want nothing", g)
		}
	}
}

// TestRefreshUsageMissingExpiry says the credits cannot be read.
//
// 账本里有一批没有到期时间，这批积分读不出来，整个余额就不给。
func TestRefreshUsageMissingExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matrix/api/v1/commerce/get_membership_info":
			w.Write([]byte(`{"data":{"has_token_plan":true}}`))
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"100","remaining_amount":"500","granted_at_ms":1763000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	snap, err := m.RefreshUsage(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if snap.HasAmounts {
		t.Fatalf("snapshot = %+v, want no amounts", snap)
	}
	if snap.Granted != 0 || snap.Remaining != 0 {
		t.Fatalf("snapshot = %+v, want no amounts", snap)
	}
}

// TestRefreshUsageRefusedSignIn says a refused sign-in is not a plan
// without windows.
//
// 签到被拒的时候账号要标成登出，不能把读不到窗口说成没有窗口。
func TestRefreshUsageRefusedSignIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matrix/api/v1/commerce/get_membership_info":
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid access token"}`))
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":0,"details":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	if _, err := m.RefreshUsage(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
}

// TestRefreshUsageExpiredBatch says a ledger that was read and has
// nothing live in it is not a ledger that could not be read.
//
// 账本读到了，但里面的批次都已经过期：这是“用完了”，不是“读不到”。
func TestRefreshUsageExpiredBatch(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"0","remaining_amount":"600","granted_at_ms":1763000000000,"expire_at_ms":` +
				fmt.Sprint(expired) + `}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	snap, err := m.RefreshUsage(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.HasAmounts {
		t.Fatalf("snapshot = %+v, want the reading that was made", snap)
	}
	if snap.Remaining != 0 || snap.Granted != 0 {
		t.Fatalf("snapshot = %+v, want nothing left", snap)
	}
	if snap.ExpiresAt.IsZero() {
		t.Fatal("the reading has to say when the credits ran out")
	}
	if snap.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at = %v, want a time in the past", snap.ExpiresAt)
	}
}

// TestRefreshUsageEmptyLedger says a ledger that was read and is empty
// is an account with nothing left, not an account that was never read.
//
// 读到空账本是 0，读不到账本是不知道：两回事。
func TestRefreshUsageEmptyLedger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":0,"details":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	snap, err := m.RefreshUsage(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.HasAmounts {
		t.Fatalf("snapshot = %+v, want a reading of zero", snap)
	}
	if snap.Remaining != 0 {
		t.Fatalf("snapshot = %+v, want nothing left", snap)
	}
}

// TestRefreshUsageUnreadableLedger says a ledger that cannot be read
// leaves the credits unknown.
//
// 账本读不出来的时候没有数：没有数不是 0。
func TestRefreshUsageUnreadableLedger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			// 这一批的数额读不出来，整个账本就读不出来
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"abc","consumed_amount":"0","remaining_amount":"600","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	snap, err := m.RefreshUsage(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if snap.HasAmounts {
		t.Fatalf("snapshot = %+v, want no amounts", snap)
	}
	// 存下来的也是没有数的那一份
	stored, err := m.Store.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.HasAmounts {
		t.Fatalf("stored = %+v, want no amounts", stored)
	}
}

// TestRefreshUsageRecordsAFailedReading says a failed reading is
// written down.
//
// 读失败时什么都不写，上一份读数就还是这个账号当前的读数，陈旧的数字
// 看起来是新的。
func TestRefreshUsageRecordsAFailedReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"the ledger is down"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	if _, err := m.RefreshUsage(ctx, "acc_01"); err == nil {
		t.Fatal("expected an error")
	}
	stored, err := m.Store.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.HasAmounts {
		t.Fatalf("stored = %+v, want the failed reading", stored)
	}
	if stored.CreatedAt.IsZero() {
		t.Fatal("the failed reading has to say when it was made")
	}
}

// TestRefreshUsageKeepsThePlanWindowError says a plan whose windows
// could not be read is not a plan without windows.
//
// 读不到窗口和没有窗口不是一回事：调用方要能分清，不然就会把账号放上
// 错误的冷却。
func TestRefreshUsageKeepsThePlanWindowError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend/account/token_plan/remains_percent",
			"/v1/api/openplatform/coding_plan/remains":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"the console is down"}`))
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"0","remaining_amount":"600","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	snap, err := m.RefreshUsage(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if snap.WindowsError == "" {
		t.Fatalf("snapshot = %+v, want the error of the windows", snap)
	}
	if snap.WindowsJSON != "" {
		t.Fatalf("snapshot = %+v, want no windows", snap)
	}
	// 读数存下来了，调用方也能问出来
	stored, err := m.Store.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.WindowsError == "" {
		t.Fatalf("stored = %+v, want the error of the windows", stored)
	}
	if _, err := m.LatestWindows(ctx, "acc_01"); err == nil {
		t.Fatal("the caller has to be told that the windows are unknown")
	}
}

// TestForceRenew says a forced renewal gives a new token.
//
// 这是网关收到 401 时做的事。
func TestForceRenew(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sent = append(sent, r.Form.Get("refresh_token"))
		w.Write([]byte(`{"access_token":"acc2","refresh_token":"ref2","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc1", Refresh: "ref1",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	tok, err := m.ForceRenew(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc2" {
		t.Fatalf("token = %+v, want the renewed one", tok)
	}
	if len(sent) != 1 || sent[0] != "ref1" {
		t.Fatalf("the refresh tokens sent = %v, want [ref1]", sent)
	}
	stored, err := m.Store.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Access != "acc2" || stored.Refresh != "ref2" {
		t.Fatalf("stored = %+v, want the new token", stored)
	}
}

// TestForceRenewWalksTheChainWhenTheStoreIsBehind says an account
// whose store did not get the last renewal can still be renewed.
//
// 续期成功了但写库失败时，库里那份 refresh 已经用掉了，renewer 手里
// 的新 token 是唯一的出路。
func TestForceRenewWalksTheChainWhenTheStoreIsBehind(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sent = append(sent, r.Form.Get("refresh_token"))
		w.Write([]byte(`{"access_token":"acc3","refresh_token":"ref3","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	// 库里还是老的那份：写库失败了
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc1", Refresh: "ref1",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	// renewer 手里有更新的一份
	// 令牌里的 AccountID 是站点自己的账号 id，这里不知道，就空着
	if _, err := m.Renewer.Renew(ctx, "acc_01",
		&oauth.Token{Access: "acc1", Refresh: "ref1"},
		func(ctx context.Context, refresh string) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc2", Refresh: "ref2",
				Expires: time.Now().Add(time.Hour)}, nil
		}); err != nil {
		t.Fatal(err)
	}

	tok, err := m.ForceRenew(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc3" {
		t.Fatalf("token = %+v, want the renewed one", tok)
	}
	if len(sent) != 1 || sent[0] != "ref2" {
		t.Fatalf("the refresh tokens sent = %v, want the newer one [ref2]", sent)
	}
}

// TestSignedOutAccountFailsFast says a signed-out account is not
// handed out with a token the site has refused.
//
// 登出的账号要把令牌抹掉，不然每次请求都要白白付一次续期。
func TestSignedOutAccountFailsFast(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "old", Refresh: "ref",
		ExpiresAt: now.Add(time.Minute), UpdatedAt: now,
	})

	if _, err := m.Token(ctx, "acc_01"); err == nil {
		t.Fatal("expected an error")
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
	stored, err := m.Store.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Access != "" || stored.Refresh != "" {
		t.Fatalf("stored = %+v, want the token taken away", stored)
	}
	// 再要一次令牌也是错，不会再去续期
	if _, err := m.Token(ctx, "acc_01"); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("the refresh was sent %d times, want 1", calls)
	}
}

// TestClientHidesTheProxyCredentials says the proxy address the manager
// hands out has no credentials in it.
func TestClientHidesTheProxyCredentials(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, nil)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive,
		ProxyURL: "http://user:secret@127.0.0.1:7890", CreatedAt: now, UpdatedAt: now,
	})
	_, url, err := m.Client(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(url, "secret") {
		t.Fatalf("url = %q, want no credentials", url)
	}
	if !strings.Contains(url, "127.0.0.1:7890") {
		t.Fatalf("url = %q, want the host and the port", url)
	}
}

// seedAccount makes an account with a token in the store.
func seedAccount(t *testing.T, m *Manager, id, access, refresh string, expires time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: id, Access: access, Refresh: refresh,
		ExpiresAt: expires, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// jwtWithAccountID makes an access token that carries an account id.
//
// The site hands out JWTs, and the claims are what the manager reads.
func jwtWithAccountID(accountID string) string {
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"sub":"user1","account_id":"` + accountID + `"}`))
	return "e30." + payload + ".sig"
}

// TestChainBrokenSignsTheAccountOut says a chain that cannot go on ends
// the account's sign-in.
//
// 续期的链断了：库里那份 refresh 已经花掉了，renewer 手里也没有更新的
// 一份，没有令牌可以再换。这个账号只能重新登录，状态要写成登出，最后
// 一条错误要说明是链断了，而不是让它接着重试。
func TestChainBrokenSignsTheAccountOut(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))
	// renewer 手里那份是上一次续期买来的，"ref" 已经花掉了
	if _, err := m.Renewer.Renew(ctx, "acc_01", &oauth.Token{Access: "old", Refresh: "ref"},
		func(ctx context.Context, refresh string) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc2", Refresh: "ref2", Expires: now.Add(time.Hour)}, nil
		}); err != nil {
		t.Fatal(err)
	}
	// 链被清掉了：没有更新的令牌可走，链就断了
	m.Renewer.Reset("acc_01")

	if _, err := m.Token(ctx, "acc_01"); !errors.Is(err, oauth.ErrRefreshChainBroken) {
		t.Fatalf("error = %v, want the broken chain", err)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
	if !strings.Contains(a.LastError, "chain") {
		t.Fatalf("last error = %q, want the broken chain", a.LastError)
	}
	if calls != 0 {
		t.Fatalf("the site was asked %d times, want 0", calls)
	}
}

// TestUnknownOutcomeIsNotASignOut says a refresh whose answer was lost
// is not a sign-out.
//
// 刷新请求发出去了，回答没回来：站点可能已经把 refresh token 花掉了，
// 所以它不能再发一次；但站点没有说这个登录失效了，账号不能写成登出。
// 错误要记到账号上，WebUI 才看得出这个账号出了什么事。
func TestUnknownOutcomeIsNotASignOut(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// 回答还没写出来连接就断了：请求到了站点，回答没回来
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))

	if _, err := m.Token(ctx, "acc_01"); !errors.Is(err, oauth.ErrRefreshOutcomeUnknown) {
		t.Fatalf("error = %v, want the unknown outcome", err)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status == store.StatusSignedOut {
		t.Fatalf("status = %q, the site never said the sign-in is no good", a.Status)
	}
	if !strings.Contains(a.LastError, "not known") {
		t.Fatalf("last error = %q, want the unknown outcome", a.LastError)
	}
	// 再要一次令牌，那个 refresh token 不能再发一次
	if _, err := m.Token(ctx, "acc_01"); !errors.Is(err, oauth.ErrRefreshOutcomeUnknown) {
		t.Fatalf("error = %v, want the unknown outcome", err)
	}
	if calls != 1 {
		t.Fatalf("the refresh token was sent %d times, want 1", calls)
	}
}

// TestAStaleRefusalDoesNotSignTheAccountOut says a refused token that is
// only stale must not sign the account out.
//
// 站点对已经花掉的 refresh token 也回 invalid_grant，跟登录失效长得
// 一模一样。renewer 手里还有更新的令牌时，被拒的那份只是旧了：账号还
// 活着，不能把它登出，要用更新的那份。
func TestAStaleRefusalDoesNotSignTheAccountOut(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sent = append(sent, r.Form.Get("refresh_token"))
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	// 库里还是老的那份：写库失败了
	seedAccount(t, m, "acc_01", "old", "ref-old", now.Add(time.Minute))
	// renewer 手里有更新的一份
	if _, err := m.Renewer.Renew(ctx, "acc_01", &oauth.Token{Access: "acc2", Refresh: "ref-new"},
		func(ctx context.Context, refresh string) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc3", Refresh: "ref-newer", Expires: now.Add(time.Hour)}, nil
		}); err != nil {
		t.Fatal(err)
	}

	tok, err := m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatalf("the account was signed out over a stale token: %v", err)
	}
	if tok.Access != "acc3" {
		t.Fatalf("token = %+v, want the newer one", tok)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusActive {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusActive)
	}
	// 库里换成了更新的那份
	stored, err := m.Store.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Access != "acc3" {
		t.Fatalf("stored = %+v, want the newer token", stored)
	}
	if len(sent) != 1 || sent[0] != "ref-old" {
		t.Fatalf("the refresh tokens sent = %v, want [ref-old]", sent)
	}
}

// TestTwoRenewalsKeepTheNewerTokenInTheStore says the store keeps the
// newer of two tokens.
//
// 续期的链自己会往前走，两次续期可能带回两个不同的令牌，旧的那个还
// 可能后到。旧令牌里的 refresh 已经被新令牌花掉了，写进库里，下一次
// 续期就会拿到 invalid_grant，账号就被登出了。所以只有更新的令牌才
// 写库。
func TestTwoRenewalsKeepTheNewerTokenInTheStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("refresh_token") == "ref-old" {
			// 旧的那次慢，旧令牌后到
			time.Sleep(200 * time.Millisecond)
			w.Write([]byte(`{"access_token":"acc-old","refresh_token":"ref-old2","expires_in":60}`))
			return
		}
		w.Write([]byte(`{"access_token":"acc-new","refresh_token":"ref-new2","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "acc0", "ref-old", now.Add(time.Minute))

	old := make(chan *oauth.Token, 1)
	go func() {
		tok, err := m.Renew(ctx, "acc_01", &oauth.Token{
			Access: "acc0", Refresh: "ref-old", Expires: now.Add(time.Minute),
		})
		if err != nil {
			t.Error(err)
		}
		old <- tok
	}()
	time.Sleep(20 * time.Millisecond)
	newer, err := m.Renew(ctx, "acc_01", &oauth.Token{
		Access: "acc1", Refresh: "ref-new", Expires: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if newer.Access != "acc-new" {
		t.Fatalf("token = %+v, want the newer one", newer)
	}
	<-old

	stored, err := m.Store.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Access != "acc-new" || stored.Refresh != "ref-new2" {
		t.Fatalf("stored = %+v, want the newer token", stored)
	}
}

// TestTheTokenCarriesTheSitesAccountID says the store's id is not put
// where the site's id belongs.
//
// oauth.Token.AccountID 是站点自己的账号 id，renewer 是按库里的账号 id
// 记链的，两个不是一个东西。把库里的 id 填进去，走链的人就会给一个账号
// 走两条链，两个 refresh token 都花掉，账号就白登出了。
func TestTheTokenCarriesTheSitesAccountID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"` + jwtWithAccountID("site-42") +
			`","refresh_token":"ref2","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "acc1", "ref1", now.Add(time.Minute))

	tok, err := m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != "site-42" {
		t.Fatalf("account id = %q, want the site's one", tok.AccountID)
	}
	// 库里那份没有站点的账号 id：它就空着，不能拿库里的 id 顶上去
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc9", Refresh: "ref9",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	tok, err = m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != "" {
		t.Fatalf("account id = %q, want nothing: the store does not keep the site's id", tok.AccountID)
	}
}

// TestTheRenewerKeepsItsChainWhenTheOutcomeIsUnknown says the renewer
// is not told to forget an account that is not signed out.
//
// 答案没回来的时候不能把 renewer 忘干净：忘了它手里那份，令牌就走不
// 了；更要紧的是忘了"已经花掉"的记录，那个 refresh token 就会被再发
// 一次，站点会回 invalid_grant，账号就白登出了。
func TestTheRenewerKeepsItsChainWhenTheOutcomeIsUnknown(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	// 库里那份和 renewer 手里那份是同一个：走链不会动它
	seedAccount(t, m, "acc_01", "acc3", "ref3", now.Add(time.Minute))
	if _, err := m.Renewer.Renew(ctx, "acc_01", &oauth.Token{Access: "acc2", Refresh: "ref2"},
		func(ctx context.Context, refresh string) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc3", Refresh: "ref3", Expires: now.Add(time.Hour)}, nil
		}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Token(ctx, "acc_01"); !errors.Is(err, oauth.ErrRefreshOutcomeUnknown) {
		t.Fatalf("error = %v, want the unknown outcome", err)
	}
	// renewer 手里那份还在
	if held, ok := m.Renewer.Newest("acc_01"); !ok || held.Access != "acc3" {
		t.Fatalf("the renewer holds %+v, want the token it had", held)
	}
	// 那份答案不明的令牌不会再发一次
	if _, err := m.Token(ctx, "acc_01"); !errors.Is(err, oauth.ErrRefreshOutcomeUnknown) {
		t.Fatalf("error = %v, want the unknown outcome", err)
	}
	if calls != 1 {
		t.Fatalf("the refresh token was sent %d times, want 1", calls)
	}
}

// TestTheRenewerForgetsAnAccountThatIsSignedOut says a signed-out
// account leaves nothing behind in the renewer.
//
// 登出的时候 renewer 要忘掉这个账号：忘掉之后这个账号再登进来，renewer
// 手里不能还留着老令牌，那会把新登进来的令牌当成旧链上的一段。
func TestTheRenewerForgetsAnAccountThatIsSignedOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "acc1", "ref1", now.Add(time.Minute))
	// renewer 手里有一份，"ref1" 已经花掉了
	if _, err := m.Renewer.Renew(ctx, "acc_01", &oauth.Token{Access: "acc1", Refresh: "ref1"},
		func(ctx context.Context, refresh string) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc2", Refresh: "ref2", Expires: now.Add(time.Hour)}, nil
		}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Token(ctx, "acc_01"); err == nil {
		t.Fatal("expected an error")
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
	if _, ok := m.Renewer.Newest("acc_01"); ok {
		t.Fatal("the renewer still holds a token of a signed-out account")
	}
}
