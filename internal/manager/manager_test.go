package manager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/checkin"
	"mmxcode/internal/egress"
	"mmxcode/internal/oauth"
	"mmxcode/internal/secret"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"

	_ "modernc.org/sqlite"
)

func newTestManager(t *testing.T, srv *httptest.Server) *Manager {
	t.Helper()
	m, _ := newTestManagerWithPath(t, srv)
	return m
}

// newTestManagerWithPath makes a manager and says where its database
// is.
//
// A test that wants the database to misbehave — a write that is
// refused, a write that touches more than it should — opens the file
// itself and puts a trigger in it.
func newTestManagerWithPath(t *testing.T, srv *httptest.Server) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := store.Open(path)
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
	return m, path
}

// rawDB opens the test's database file with plain SQL.
//
// The store keeps its own handle to itself, so this is the way a test
// gets at the file the store is using.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// refuseWrites makes the writes to a table fail.
//
// The trigger stands in for a full disk or a read-only file: the
// writes are refused and the reads go on, which is what such a fault
// looks like to the store.
func refuseWrites(t *testing.T, path, table, event string) {
	t.Helper()
	db := rawDB(t, path)
	q := `CREATE TRIGGER refuse_writes BEFORE ` + event + ` ON ` + table +
		` BEGIN SELECT RAISE(ABORT, 'the disk is full'); END;`
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
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
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
	if n := calls.Load(); n != 1 {
		t.Fatalf("the refresh token was spent %d times, want 1", n)
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

// TestRefreshAllUsageReadsEveryAccount says one round of readings
// covers every account.
//
// 一次刷新要把所有账号都读一遍：页面上那个「刷新」按钮按一下，人要
// 看到的是所有账号现在的数，不是其中一个的数。
func TestRefreshAllUsageReadsEveryAccount(t *testing.T) {
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
	for _, id := range []string{"acc_01", "acc_02"} {
		m.Store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "1234567890",
			CreatedAt: now, UpdatedAt: now,
		})
		m.Store.SaveToken(ctx, store.Token{
			AccountID: id, Access: "acc", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}

	res, err := m.RefreshAllUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Refreshed != 2 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	snaps, err := m.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want one per account", len(snaps))
	}
}

// TestRefreshAllUsageSaysWhichAccountFailed says the round says which
// account could not be read.
//
// 一个账号读不到，不能把整趟说成失败了，也不能什么都不说：读不到的那
// 一个要说得出来，人才知道去看哪一个。
func TestRefreshAllUsageSaysWhichAccountFailed(t *testing.T) {
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
		ID: "acc_01", Name: "读得到的", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	// 这个账号没有令牌：读不到。
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "读不到的", Status: store.StatusActive, UserID: "0987654321",
		CreatedAt: now, UpdatedAt: now,
	})

	res, err := m.RefreshAllUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Refreshed != 1 || res.Failed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Reasons) != 1 {
		t.Fatalf("reasons = %v, want one", res.Reasons)
	}
	if !strings.Contains(res.Reasons[0], "acc_02") {
		t.Fatalf("reason = %q, want it to name the account", res.Reasons[0])
	}
}

// TestTheRefreshSkipsAnAccountThatIsBeingRead says the round skips an
// account that another round is reading right now.
//
// 同一个账号同一时刻只读一次（规格 §3.3）：别的这一趟正在读它，这一趟
// 就跳过它 —— 它没有坏，它正在被读，所以不算读失败，但要说得出来。
func TestTheRefreshSkipsAnAccountThatIsBeingRead(t *testing.T) {
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
	for _, id := range []string{"acc_01", "acc_02"} {
		m.Store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "1234567890",
			CreatedAt: now, UpdatedAt: now,
		})
		m.Store.SaveToken(ctx, store.Token{
			AccountID: id, Access: "acc", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	// 别的这一趟正在读 acc_01。
	if !m.BeginReading("acc_01") {
		t.Fatal("the account could not be claimed")
	}

	res, err := m.RefreshAllUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Refreshed != 1 {
		t.Errorf("refreshed = %d, want 1", res.Refreshed)
	}
	if res.Failed != 0 {
		t.Errorf("failed = %d, want 0: it is being read, not broken", res.Failed)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", res.Skipped)
	}
}

// TestTheRefreshGivesTheAccountBack says the round gives the account
// back when the reading is done.
//
// 读失败了也要还回来：不然那个账号再也读不到了。
func TestTheRefreshGivesTheAccountBack(t *testing.T) {
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
	// 这个账号读得到。
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	// 这个账号读不到：没有令牌。
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, UserID: "0987654321",
		CreatedAt: now, UpdatedAt: now,
	})

	if _, err := m.RefreshAllUsage(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acc_01", "acc_02"} {
		if !m.BeginReading(id) {
			t.Errorf("the account %s is still locked", id)
		}
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
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
	if n := calls.Load(); n != 1 {
		t.Fatalf("the refresh was sent %d times, want 1", n)
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
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
	if n := calls.Load(); n != 0 {
		t.Fatalf("the site was asked %d times, want 0", n)
	}
}

// TestUnknownOutcomeIsNotASignOut says a refresh whose answer was lost
// is not a sign-out.
//
// 刷新请求发出去了，回答没回来：站点可能已经把 refresh token 花掉了，
// 所以它不能再发一次；但站点没有说这个登录失效了，账号不能写成登出。
// 错误要记到账号上，WebUI 才看得出这个账号出了什么事。
func TestUnknownOutcomeIsNotASignOut(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
	if n := calls.Load(); n != 1 {
		t.Fatalf("the refresh token was sent %d times, want 1", n)
	}
}

// TestAStaleRefusalDoesNotSignTheAccountOut says a refused token that is
// only stale must not sign the account out.
//
// 站点对已经花掉的 refresh token 也回 invalid_grant，跟登录失效长得
// 一模一样。renewer 手里还有更新的令牌时，被拒的那份只是旧了：账号还
// 活着，不能把它登出，要用更新的那份。
//
// 这里走的是强制的续期（网关收到 401 时做的事）：平常那条路根本不会
// 把旧的那份发出去——renewer 手里有更新的，就用那一份。
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

	tok, err := m.ForceRenew(ctx, "acc_01")
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
	// 库里那份换成了更新的：renewer 手里那份比它还新，manager 会用
	// renewer 那份，所以这里要给库里那份一个更晚的到期时间。
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc9", Refresh: "ref9",
		ExpiresAt: now.Add(2 * time.Hour), UpdatedAt: now,
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
	// 计数器是给处理请求的那条线程写的：普通变量在竞态检测器眼里就是
	// 一个数据竞争。
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
			// renewer 手里那份就是库里那份，到期时间也一样
			return &oauth.Token{Access: "acc3", Refresh: "ref3", Expires: now.Add(time.Minute)}, nil
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
	if n := calls.Load(); n != 1 {
		t.Fatalf("the refresh token was sent %d times, want 1", n)
	}
}

// TestTheRenewerForgetsAnAccountThatIsSignedOut says a signed-out
// account leaves nothing behind in the renewer.
//
// 登出的时候 renewer 要忘掉这个账号：忘掉之后这个账号再登进来，renewer
// 手里不能还留着老令牌，那会把新登进来的令牌当成旧链上的一段。
//
// 这里走的是强制的续期（网关收到 401 时做的事）：平常那条路会用
// renewer 手里那份更新的令牌，根本不会走到登出。
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

	if _, err := m.ForceRenew(ctx, "acc_01"); err == nil {
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

// TestTheSignOutWritesOnlyTheStatus says the sign-out does not write
// back the account's other columns.
//
// 管理员把账号登出时只写状态、最后一条错误和时间。别的写者（WebUI）改
// 过的名字、代理不能被管理员手里那份旧拷贝抹掉——那份拷贝是登出之前读
// 的，别人的改动就在这中间进来的。
func TestTheSignOutWritesOnlyTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m, path := newTestManagerWithPath(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))

	// 一个看火的触发器：只有写了账号别的列的语句才会让它动。
	db := rawDB(t, path)
	if _, err := db.Exec(`CREATE TABLE watched (id TEXT, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER watch_account
		AFTER UPDATE OF name, user_id, email, proxy_group, proxy_url ON accounts
		BEGIN INSERT INTO watched (id, name) VALUES (OLD.id, NEW.name); END;`); err != nil {
		t.Fatal(err)
	}

	// 另一个写者改了名字
	if err := m.Store.UpdateAccount(ctx, store.Account{
		ID: "acc_01", Name: "renamed", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 从这里开始看
	if _, err := db.Exec(`DELETE FROM watched`); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Token(ctx, "acc_01"); err == nil {
		t.Fatal("expected an error")
	}
	var watched int
	if err := db.QueryRow(`SELECT count(*) FROM watched`).Scan(&watched); err != nil {
		t.Fatal(err)
	}
	if watched != 0 {
		t.Fatalf("the sign-out wrote %d rows of the other columns", watched)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
	if a.Name != "renamed" {
		t.Fatalf("name = %q, want the name the other writer set", a.Name)
	}
}

// TestTheSignOutSurvivesAnotherWriter says a sign-out is not undone by
// a writer that does not touch the status.
//
// 登出之后另一个写者改了账号的代理：代理的改动不该把登出抹掉，账号是
// 站点已经不认的账号，不能又变回能用的。
func TestTheSignOutSurvivesAnotherWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))

	if _, err := m.Token(ctx, "acc_01"); err == nil {
		t.Fatal("expected an error")
	}
	// 另一个写者改了出口
	if err := m.Store.SetAccountProxy(ctx, "acc_01", "grp", "http://127.0.0.1:7890", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusSignedOut)
	}
	if a.ProxyGroup != "grp" || a.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("the account = %+v, want the outlet the other writer set", a)
	}
}

// TestTheSignOutDoesNotUndoAFreshSignIn says a refusal does not sign
// out an account that somebody else has signed in again.
//
// 手里这份令牌的续期被站点拒了（站点说这个 refresh token 不认），可是
// 这期间另一个进程把这账号登回来了：库里换成了新令牌。这时候把签出
// 写下去，就是把别人刚登好的账号作废掉，还把那份新令牌收走——页面会
// 说这个账号登出了，轮转池也不用它，而它手里握着一份能用的凭据。
func TestTheSignOutDoesNotUndoAFreshSignIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))

	// 另一个进程把这账号登回来了：库里换成了新令牌。
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "fresh", Refresh: "freshref",
		ExpiresAt: now.Add(2 * time.Hour), Subject: "sub-1", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// 这一边拿着旧令牌去续期，被拒了。
	cur := &oauth.Token{Access: "old", Refresh: "ref", Expires: now.Add(time.Minute)}
	if _, err := m.Renew(ctx, "acc_01", cur); err == nil {
		t.Fatal("expected an error")
	}

	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusActive {
		t.Fatalf("status = %q, want %q", a.Status, store.StatusActive)
	}
	tok, err := m.Store.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "fresh" || tok.Refresh != "freshref" {
		t.Fatalf("the token = %+v, want the one the other sign-in wrote", tok)
	}
}

// TestASignedOutAccountIsStillFoundByItsSubject says signing an
// account out does not lose the key that says which site account it is.
//
// 签出是要收走令牌的：站点不认这个登录了，这份凭据没有用。可是令牌行
// 里的 subject 得留着——那是站点那边账号自己的 id，是「这个站点账号在
// 库里是哪一行」的键。签出的账号恰恰是最要紧的那一个：操作的人看见它
// 掉线了，第一件事就是重新导入它。键要是被抹掉，那一次导入就会当成
// 新账号，再插一行——两行都是活的，轮转会挑到两次。
func TestASignedOutAccountIsStillFoundByItsSubject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "old", Refresh: "ref",
		ExpiresAt: now.Add(time.Minute), Subject: "sub-1", UpdatedAt: now,
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
	acc, err := m.Store.AccountBySubject(ctx, "sub-1")
	if err != nil {
		t.Fatalf("the signed-out account is not found by its subject: %v", err)
	}
	if acc.ID != "acc_01" {
		t.Fatalf("account = %q, want acc_01", acc.ID)
	}
}

// TestARenewalThatWorkedIsKeptWhenTheWriteFails says a renewal that
// worked is not thrown away because the database would not take it.
//
// 站点刚给了新令牌，写库却失败了：令牌不能丢。丢了这次请求要白换一个
// 账号，而库里那份 refresh 已经花掉了，下一次请求又要再花一次 refresh
// token。
func TestARenewalThatWorkedIsKeptWhenTheWriteFails(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"access_token":"new","refresh_token":"newref","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m, path := newTestManagerWithPath(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))
	// 写库失败：磁盘满了
	refuseWrites(t, path, "tokens", "UPDATE")

	tok, err := m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatalf("the renewal was thrown away: %v", err)
	}
	if tok == nil || tok.Access != "new" {
		t.Fatalf("token = %+v, want the token the site handed out", tok)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the site was asked %d times, want 1", n)
	}
	// 写库失败这件事要写在账号上，WebUI 才看得出库里那份是旧的
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.LastError, "written down") {
		t.Fatalf("last error = %q, want the failed write", a.LastError)
	}
}

// TestAFailedRenewalKeepsTheTokenInHand says a renewal that failed for
// a reason that is not a refusal leaves the caller with the token it
// has.
//
// 令牌端点回 500：令牌还在有效期内，还有几分钟能用。为了几分钟后的
// 一次续期失败就把现在能用的令牌扔掉，是白扔。
func TestAFailedRenewalKeepsTheTokenInHand(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"the token endpoint is down"}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	// 令牌还有一分钟就过期，落在续期窗口里，但它还是好的
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))

	tok, err := m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatalf("the token in hand was thrown away: %v", err)
	}
	if tok == nil || tok.Access != "old" {
		t.Fatalf("token = %+v, want the token in hand", tok)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the site was asked %d times, want 1", n)
	}
}

// TestATokenThatCannotBeReadIsNotASignOut says a database that is in
// trouble is not an account that is signed out.
//
// 数据库坏了和账号没登录是两回事。写成“账号没登录”，真正的原因就没
// 了，WebUI 也会说错。
func TestATokenThatCannotBeReadIsNotASignOut(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, nil)
	now := time.Now()
	seedAccount(t, m, "acc_01", "acc", "ref", now.Add(time.Hour))

	// 数据库读不了了
	if err := m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := m.Token(ctx, "acc_01")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "is not signed in") {
		t.Fatalf("error = %q, want the fault of the database", err)
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("error = %q, want the cause of the failure", err)
	}
}

// TestAnAccountWithoutATokenIsNotSignedIn says a missing token is a
// sign-in that is not there.
//
// 没有令牌行就是没登录，这个说法要留着。
func TestAnAccountWithoutATokenIsNotSignedIn(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, nil)
	now := time.Now()
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Token(ctx, "acc_01")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "is not signed in") {
		t.Fatalf("error = %q, want the missing sign-in", err)
	}
}

// TestTheRenewersTokenIsUsedWhenItIsNewer says the token the renewer
// holds is used when it is newer than the one in the store.
//
// 续期成功了但写库失败：renewer 手里那份是新的，库里那份是旧的。再续
// 一次就是再花一个 refresh token，而那一枚可能已经没了。
func TestTheRenewersTokenIsUsedWhenItIsNewer(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"access_token":"other","refresh_token":"otherref","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))
	// renewer 手里有一份更新的：上一次续期成功了，但没写进库
	if _, err := m.Renewer.Renew(ctx, "acc_01",
		&oauth.Token{Access: "old", Refresh: "ref"},
		func(ctx context.Context, refresh string) (*oauth.Token, error) {
			return &oauth.Token{Access: "new", Refresh: "newref",
				Expires: now.Add(time.Hour)}, nil
		}); err != nil {
		t.Fatal(err)
	}

	tok, err := m.Token(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new" {
		t.Fatalf("access = %q, want the newer one", tok.Access)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the site was asked %d times, want 0", n)
	}
	// 新的一份也写进库里
	stored, err := m.Store.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Access != "new" {
		t.Fatalf("stored = %+v, want the newer token", stored)
	}
}

// TestACheckinWhoseRecordFailsIsWrittenDown says a check-in that
// worked is not dropped because its record could not be written.
//
// 签到成功了，记录写不进去：这一天不能再签，积分和连续天数就都没有
// 记下来。写不进去这件事要写在账号上。
func TestACheckinWhoseRecordFailsIsWrittenDown(t *testing.T) {
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
	m, path := newTestManagerWithPath(t, srv)
	now := time.Now()
	// 签到要账号的 user id
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "acc", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 签到记录写不进去
	refuseWrites(t, path, "checkins", "INSERT")

	out, err := m.Checkin(ctx, "acc_01")
	if err != nil {
		t.Fatalf("the check-in worked: %v", err)
	}
	if out.Outcome != checkin.OutcomeClaimed {
		t.Fatalf("outcome = %q, want the claimed one", out.Outcome)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.LastError, "written down") {
		t.Fatalf("last error = %q, want the failed record", a.LastError)
	}
}

// TestTheUnknownOutcomeKeepsTheCause says the account's last error
// says what happened, and not only which case it was.
//
// 刷新请求发出去了、回答没回来：账号上要写的不只是“结果不明”，还有那
// 个传输上的原因，WebUI 才看得出出了什么事。
func TestTheUnknownOutcomeKeepsTheCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 回答还没写出来连接就断了：请求到了站点，回答没回来
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	ctx := context.Background()
	m := newTestManager(t, srv)
	now := time.Now()
	seedAccount(t, m, "acc_01", "old", "ref", now.Add(time.Minute))

	_, tokErr := m.Token(ctx, "acc_01")
	if tokErr == nil {
		t.Fatal("expected an error")
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.LastError, "not known") {
		t.Fatalf("last error = %q, want the unknown outcome", a.LastError)
	}
	if !strings.Contains(a.LastError, tokErr.Error()) {
		t.Fatalf("last error = %q, want the cause %q", a.LastError, tokErr)
	}
}

func TestReloginSignsTheAccountInAgain(t *testing.T) {
	// 账号签出了，而且它有密码：拿密码重新登一次，状态回到 active。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		if password != "hunter2" {
			t.Fatalf("password = %q", password)
		}
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: "acc_1", Access: "new-access", Refresh: "new-refresh",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	if err := m.Relogin(ctx, "acc_1"); err != nil {
		t.Fatal(err)
	}
	acc, err := m.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusActive {
		t.Fatalf("status = %q", acc.Status)
	}
	if acc.LastError != "" {
		t.Fatalf("last error = %q, want none", acc.LastError)
	}
	tok, err := m.Store.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new-access" || tok.Refresh != "new-refresh" {
		t.Fatalf("token = %+v, want the new one", tok)
	}
}

func TestReloginWithoutAPasswordSaysSo(t *testing.T) {
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Relogin(ctx, "acc_1"); err == nil {
		t.Fatal("no error for an account without a password")
	}
}

func TestReloginWithoutAKeySaysSo(t *testing.T) {
	// 密文在、密钥没了：说的是密钥的事，不是账号的事。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	err = m.Relogin(ctx, "acc_1")
	if err == nil {
		t.Fatal("no error without a key")
	}
	if !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("error = %q, want it to name the key", err)
	}
}

func TestAFailedReloginLeavesTheAccountSignedOutWithTheReason(t *testing.T) {
	// 登不回去的账号还是签出的，而且要说清楚为什么。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return errors.New("the site refused the password")
	}
	if err := m.Relogin(ctx, "acc_1"); err == nil {
		t.Fatal("no error for a failed sign-in")
	}
	acc, err := m.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want signed out", acc.Status)
	}
	if !strings.Contains(acc.LastError, "refused the password") {
		t.Fatalf("last error = %q, want the reason", acc.LastError)
	}
}

func TestReloginAllOnlyTouchesTheAccountsThatHaveAPassword(t *testing.T) {
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	m.Key = key
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acc_1", "acc_2"} {
		if err := m.Store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: acc.ID, Access: "a", Refresh: "r",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	res, err := m.ReloginAll(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed != 1 || res.Failed != 0 {
		t.Fatalf("fixed = %d, failed = %d", res.Fixed, res.Failed)
	}
	acc, err := m.Store.GetAccount(ctx, "acc_2")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusSignedOut {
		t.Fatalf("the account without a password was touched: %q", acc.Status)
	}
}

func TestReloginAllStopsAtTheLimit(t *testing.T) {
	// 一轮只动几个号：剩下的留给下一轮。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	m.Key = key
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acc_1", "acc_2", "acc_3"} {
		if err := m.Store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.Store.SetPassword(ctx, id, cipher, now); err != nil {
			t.Fatal(err)
		}
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: acc.ID, Access: "a", Refresh: "r",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	res, err := m.ReloginAll(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed != 2 || res.Failed != 0 {
		t.Fatalf("fixed = %d, failed = %d", res.Fixed, res.Failed)
	}
}

// TestReloginAllSaysWhyTheOnesThatFailedFailed says the reasons are not
// thrown away.
//
// 只说「失败 2」，人得自己去一个一个看是哪个账号、为什么。而「一个都
// 没试成」（连账号都没列出来）和「都登回去了」在只看着两个数字的时候
// 是一样的。
func TestReloginAllSaysWhyTheOnesThatFailedFailed(t *testing.T) {
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	m.Key = key
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "the first one", Status: store.StatusSignedOut,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return errors.New("the site refused it")
	}

	res, err := m.ReloginAll(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Fixed != 0 {
		t.Fatalf("res = %+v, want one failure", res)
	}
	if len(res.Reasons) != 1 {
		t.Fatalf("reasons = %v, want one line", res.Reasons)
	}
	if !strings.Contains(res.Reasons[0], "the site refused it") {
		t.Fatalf("reason = %q, want the cause in it", res.Reasons[0])
	}
	if !strings.Contains(res.Reasons[0], "acc_1") {
		t.Fatalf("reason = %q, want it to name the account", res.Reasons[0])
	}
}

// TestReloginAllSaysWhenItCouldNotStart says a round that could not even
// start says so.
//
// 账号都列不出来，这一趟就根本没跑。把它说成「成功 0，失败 0」，人会
// 以为没什么要登的。
func TestReloginAllSaysWhenItCouldNotStart(t *testing.T) {
	m, path := newTestManagerWithPath(t, nil)
	ctx := context.Background()
	// 让读账号这一句炸掉：像磁盘坏了那样。
	db := rawDB(t, path)
	if _, err := db.Exec(`DROP TABLE accounts`); err != nil {
		t.Fatal(err)
	}

	res, err := m.ReloginAll(ctx, 0)
	if err == nil {
		t.Fatalf("res = %+v, want an error", res)
	}
	if res.Fixed != 0 || res.Failed != 0 {
		t.Fatalf("res = %+v, want nothing counted", res)
	}
}

func TestReloginAllLeavesTheActiveAccountsAlone(t *testing.T) {
	// 好着的账号不动：重登是给签出的号用的。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		called = true
		return nil
	}
	res, err := m.ReloginAll(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed != 0 || res.Failed != 0 {
		t.Fatalf("fixed = %d, failed = %d", res.Fixed, res.Failed)
	}
	if called {
		t.Fatal("an active account was signed in again")
	}
}

func TestTheReloginDropsTheOldChain(t *testing.T) {
	// 新的登录换来的是新的链：旧的刷新令牌就此作废，不会再被发出去。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	// 旧的那条链：renewer 手里有一份。
	if _, err := m.Renewer.Renew(ctx, "acc_1", &oauth.Token{
		Access: "old-access", Refresh: "old-refresh", Expires: now.Add(time.Hour),
	}, func(ctx context.Context, refresh string) (*oauth.Token, error) {
		return &oauth.Token{
			Access: "old-access-2", Refresh: "old-refresh-2", Expires: now.Add(2 * time.Hour),
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Renewer.Newest("acc_1"); !ok {
		t.Fatal("the renewer does not hold the old chain")
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: "acc_1", Access: "new-access", Refresh: "new-refresh",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	if err := m.Relogin(ctx, "acc_1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Renewer.Newest("acc_1"); ok {
		t.Fatal("the old chain is still there: its refresh token could be sent again")
	}
}

func TestAReloginOfAnActiveAccountIsRefused(t *testing.T) {
	// 还能用的账号不需要重登：替它登一次只会把还能用的令牌换掉。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_1", Access: "good-access", Refresh: "good-refresh",
		ExpiresAt: now.Add(50 * time.Minute), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	signedIn := false
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		signedIn = true
		return errors.New("the site refused the password")
	}
	err = m.Relogin(ctx, "acc_1")
	if err == nil {
		t.Fatal("no error for an account that is not signed out")
	}
	if !errors.Is(err, ErrNotSignedOut) {
		t.Fatalf("error = %q, want it to say the account is not signed out", err)
	}
	if signedIn {
		t.Fatal("an active account was signed in again")
	}
	// 令牌还在，状态也还在：一个还能用的账号不能因为一次重登而掉出去。
	tok, err := m.Store.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "good-access" || tok.Refresh != "good-refresh" {
		t.Fatalf("token = %+v, want the one that was there", tok)
	}
	acc, err := m.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusActive {
		t.Fatalf("status = %q, want active", acc.Status)
	}
}

// TestTheAccountAnotherOneIsSigningInIsNotAFailure says the loser of
// the race is not counted as a failure.
//
// 调度器的一轮和命令行里的那一次可能撞上：一个正在登，另一个排在后
// 面。排着的那个醒来的时候，账号已经登好了——它不是失败，是没活干。
// 算成失败的话，页面上写着「失败 1」，而账号其实好好的。
func TestTheAccountAnotherOneIsSigningInIsNotAFailure(t *testing.T) {
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	holding := make(chan struct{})
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		close(holding)
		// 慢一点：让排在那后面的一趟挤在锁上。
		time.Sleep(200 * time.Millisecond)
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: "acc_1", Access: "new-access", Refresh: "new-refresh",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	// 一个重登占着这个账号。
	done := make(chan error, 1)
	go func() { done <- m.Relogin(ctx, "acc_1") }()
	<-holding
	// 这一趟排在后面：它醒来的时候，账号已经登好了。
	res, err := m.ReloginAll(ctx, 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 0 {
		t.Fatalf("failed = %d, want 0: the account was signed in by the other one", res.Failed)
	}
	if res.Fixed != 0 {
		t.Fatalf("fixed = %d, want 0: this run signed nothing in", res.Fixed)
	}
}

func TestAFailedReloginKeepsTheTokenThatIsThere(t *testing.T) {
	// 这次重登失败，什么都没说手里那条链的事：令牌不能动。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_1", Access: "good-access", Refresh: "good-refresh",
		ExpiresAt: now.Add(50 * time.Minute), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return errors.New("the site refused the password")
	}
	if err := m.Relogin(ctx, "acc_1"); err == nil {
		t.Fatal("no error for a failed sign-in")
	}
	tok, err := m.Store.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "good-access" || tok.Refresh != "good-refresh" {
		t.Fatalf("token = %+v, want the one that was there", tok)
	}
	acc, err := m.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusSignedOut {
		t.Fatalf("status = %q, want signed out", acc.Status)
	}
	if !strings.Contains(acc.LastError, "refused the password") {
		t.Fatalf("last error = %q, want the reason", acc.LastError)
	}
}

func TestTwoReloginsOfOneAccountAreOneSignIn(t *testing.T) {
	// 调度器的一轮和命令行里的那一次可能撞上。同一个账号只能登一次：
	// 登两次就是站点那边两个设备码、两个会话。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	var calls int32
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		atomic.AddInt32(&calls, 1)
		// 慢一点，让另一个那一次挤在锁上。
		time.Sleep(50 * time.Millisecond)
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: "acc_1", Access: "new-access", Refresh: "new-refresh",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_ = m.Relogin(ctx, "acc_1")
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("signed in %d times, want 1", got)
	}
	tok, err := m.Store.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new-access" {
		t.Fatalf("token = %+v", tok)
	}
}

// TestAFailedReloginDoesNotUndoTheOneThatWorked says a sign-in that
// failed does not put the account back to signed out when somebody else
// has already signed it in.
//
// 两个进程各跑一次重登（命令行一次、serve 里一次）：一个成了，另一个
// 失败了。失败的那一个要是把状态写回 signed_out，就是把赢家刚换好的
// 令牌作废掉，还让这个账号显示成登出的状态——手里却握着一份能用的
// 凭据：轮转池不用它，下一轮又要登一次。
//
// 那个锁是每个进程自己的一份（同一个进程里两个 Manager 也是各拿各的），
// 所以这件事挡不住，只能靠写的时候有条件。
func TestAFailedReloginDoesNotUndoTheOneThatWorked(t *testing.T) {
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	// 这一次的登录没成，而另一个进程刚把这个账号登回去了。
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		if err := m.Store.UpdateAccountStatus(ctx, "acc_1", store.StatusActive, "", time.Now()); err != nil {
			return err
		}
		return errors.New("the site refused the password")
	}

	if err := m.Relogin(ctx, "acc_1"); err == nil {
		t.Fatal("no error for a sign-in that failed")
	}
	acc, err := m.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusActive {
		t.Fatalf("status = %q, want active: the one that worked must not be undone", acc.Status)
	}
}

// TestTheSameAccountIsNotReadTwiceAtTheSameTime says the manager keeps
// one account from being read twice at once.
//
// 同一个账号同一时刻只读一次（规格 §3.3）：签到那一趟和热的那一趟可能
// 撞上，两边都去读，读的是同一次。
func TestTheSameAccountIsNotReadTwiceAtTheSameTime(t *testing.T) {
	m := New(nil, nil, nil, nil, nil)

	if !m.BeginReading("acc_1") {
		t.Fatal("the account was not claimed")
	}
	if m.BeginReading("acc_1") {
		t.Fatal("the account was claimed twice")
	}
	// 别的账号不受影响。
	if !m.BeginReading("acc_2") {
		t.Fatal("the other account was not claimed")
	}
	m.EndReading("acc_1")
	if !m.BeginReading("acc_1") {
		t.Fatal("the account was not claimed again")
	}
	m.EndReading("acc_1")
	m.EndReading("acc_2")
}
