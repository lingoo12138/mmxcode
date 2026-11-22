package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/account"
	"github.com/lingoo12138/mmxcode/internal/config"
	"github.com/lingoo12138/mmxcode/internal/egress"
	"github.com/lingoo12138/mmxcode/internal/manager"
	"github.com/lingoo12138/mmxcode/internal/oauth"
	"github.com/lingoo12138/mmxcode/internal/provision"
	"github.com/lingoo12138/mmxcode/internal/store"
	"github.com/lingoo12138/mmxcode/internal/upstream"
)

// newTestWebUIWithConfig makes a WebUI with a given configuration.
//
// The import page is only there when no API key is set, so a test that
// wants to see it there needs a configuration of its own.
func newTestWebUIWithConfig(t *testing.T, cfg config.Config) (*WebUI, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now()
	s.CreateAccount(context.Background(), store.Account{
		ID: "acc_01", Name: "主号", Email: "a@example.com", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(context.Background(), store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	// The account API is pointed at a stand-in server: the check-in
	// route reaches the account API, and a test must never talk to
	// MiniMax itself.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no upstream in the test", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(cfg.Proxy.Default), oauth.NewRenewer())
	w := New(mgr, cfg, s)
	// The import and the relogin talk to the site, and a test must
	// never talk to MiniMax itself: they are fields so that the test
	// can stand in for the network.
	w.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		out := make([]provision.Result, 0, len(entries))
		for _, e := range entries {
			out = append(out, provision.Result{Email: e.Email, OK: true, AccountID: "acc_02"})
		}
		return out, nil
	}
	w.Relogin = func(ctx context.Context) (manager.ReloginResult, error) {
		return manager.ReloginResult{Fixed: 1}, nil
	}
	return w, s
}

func newTestWebUI(t *testing.T) (*WebUI, *store.Store) {
	t.Helper()
	return newTestWebUIWithConfig(t, config.Default())
}

func TestAccountsPage(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "主号") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestTheOverviewPageSaysHowTheGatewayLooks says the front page says how
// many accounts are there and which of them need a hand.
//
// 一进来就要看见「现在怎么样」：账号多少、剩多少积分、哪里出了事。别的
// 页面是去做事的，这一页是看情况的。
func TestTheOverviewPageSaysHowTheGatewayLooks(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	// 一个签出去的号：它要人来管，所以得摆在概览上。
	s.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "签出的号", Status: store.StatusSignedOut,
		LastError: "the refresh token can no longer be used",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "概览") {
		t.Fatalf("the front page is not the overview: %s", body)
	}
	if !strings.Contains(body, "签出的号") {
		t.Fatalf("the account that needs a hand is not on the front page: %s", body)
	}
	if !strings.Contains(body, "需要处理") {
		t.Fatalf("the front page does not say what needs a hand: %s", body)
	}
}

// TestTheAccountsPageHasTheTools says the accounts page has the things
// that make a hundred accounts manageable.
//
// 一百个账号：筛状态、排一下、勾几个一起办。这三样没有的话，一百个
// 账号就只能一个一个点。
func TestTheAccountsPageHasTheTools(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"account-filter", "status-chips", "pick-all", "bulkbar"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the accounts page has no %s: %s", want, body)
		}
	}
}

// TestTheIconsAreServed says the icon sheet is served.
//
// 图标是 /static/icons.svg 里的一个 <symbol>：页面用 <use> 引它。没有
// 这个文件，页面上每一个图标都是空的。
func TestTheIconsAreServed(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/static/icons.svg", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<symbol") {
		t.Fatalf("the icon sheet has no symbols: %s", rec.Body.String())
	}
}

func TestAPIAccounts(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		Accounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].ID != "acc_01" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPIUsage(t *testing.T) {
	w, s := newTestWebUI(t)
	s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 600, Consumed: 100, Remaining: 500,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["total_granted"] != float64(600) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestAPIUsageCarriesTheState says the usage API says what state every
// account is in, and how many have run out.
//
// 新的 state 和 accounts_expired 要到客户端那里：页面才知道哪个账号的
// 读数过期了，短掉的总数才有话说。
func TestAPIUsageCarriesTheState(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 600, Consumed: 100, Remaining: 500,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	})
	s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_02", HasAmounts: true, Granted: 600, Consumed: 100, Remaining: 500,
		ExpiresAt: now.Add(-time.Hour), CreatedAt: now,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		AccountsExpired int `json:"accounts_expired"`
		Accounts        []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.AccountsExpired != 1 {
		t.Fatalf("accounts_expired = %d, body = %s", out.AccountsExpired, rec.Body.String())
	}
	states := map[string]string{}
	for _, a := range out.Accounts {
		states[a.ID] = a.State
	}
	if states["acc_01"] != "read" || states["acc_02"] != "expired" {
		t.Fatalf("states = %v, body = %s", states, rec.Body.String())
	}
}

// TestTheUsagePageSaysAnAccountThatWasNeverRead says the page says an
// account whose credits were never read.
//
// 没读过跟读不到是两回事。页面上写「读不到」，人会以为是出了什么
// 事，要去查站点；其实只是还没轮到读它。
func TestTheUsagePageSaysAnAccountThatWasNeverRead(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "还没读") {
		t.Fatalf("the page does not say the account was never read: %s", body)
	}
	if strings.Contains(body, "读不到") {
		t.Fatalf("the page says the account cannot be read: %s", body)
	}
}

// TestTheUsageCanBeRefreshedNow says a reading can be asked for now.
//
// 页面上那些数是上一次读数时的数。要看现在的数，就得能说一声「现在
// 读一遍」——不等调度器那一趟。
func TestTheUsageCanBeRefreshedNow(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/usage/refresh", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Refreshed int      `json:"refreshed"`
		Failed    int      `json:"failed"`
		Reasons   []string `json:"reasons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// 测试里的站点是个 404 的替身：这个账号的账本读不到。但这一趟
	// 跑了，失败的也要说出来。
	if out.Failed != 1 {
		t.Fatalf("failed = %d, body = %s", out.Failed, rec.Body.String())
	}
	if len(out.Reasons) != 1 {
		t.Fatalf("reasons = %v, want one", out.Reasons)
	}
}

// TestTheUsageRefreshSaysHowManyWereSkipped says the answer says how
// many accounts were skipped because another round was reading them.
//
// 跳过的不是读失败：那个账号没有坏，它正在被读。但人要看得见这一趟
// 少读了几个。
func TestTheUsageRefreshSaysHowManyWereSkipped(t *testing.T) {
	w, _ := newTestWebUI(t)
	// 别的这一趟正在读这个账号。
	if !w.Mgr.BeginReading("acc_01") {
		t.Fatal("the account could not be claimed")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/usage/refresh", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Refreshed int `json:"refreshed"`
		Failed    int `json:"failed"`
		Skipped   int `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Skipped != 1 {
		t.Errorf("skipped = %d, body = %s", out.Skipped, rec.Body.String())
	}
	if out.Failed != 0 {
		t.Errorf("failed = %d, want 0: it is being read, not broken", out.Failed)
	}
}

// TestTheUsagePageHasARefreshButton says the page offers the refresh.
func TestTheUsagePageHasARefreshButton(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "usage-refresh") {
		t.Fatalf("the page has no refresh button: %s", rec.Body.String())
	}
}

// TestTheScriptRefreshesTheUsage says the page's script asks for the
// reading.
func TestTheScriptRefreshesTheUsage(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/static/app.js", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/api/usage/refresh") {
		t.Fatalf("the script does not ask for a reading: %s", rec.Body.String())
	}
}

// TestThePageHasAWayOut says the page offers a way out when the gateway
// has a key to sign in with.
//
// 浏览器里那张票得能删掉，不然退出不了。
func TestThePageHasAWayOut(t *testing.T) {
	w, _ := newTestWebUI(t)
	w.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/api/session") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestThePageHasNoWayOutWithoutAKey says a gateway without a key does
// not offer a sign-out it cannot do.
//
// 没配 key 的网关不用登录，也就没有那张票可删。
func TestThePageHasNoWayOutWithoutAKey(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "/api/session") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPICheckin(t *testing.T) {
	w, _ := newTestWebUI(t)
	// 签到会打上游；这里只看路由认不认这个路径
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/acc_01/checkin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("the route is missing")
	}
}

// 出口页列的是在用的那些：全局默认，加上账号自己写的地址。
func TestAPIEgress(t *testing.T) {
	w, s := newTestWebUI(t)
	if err := s.SetAccountOutlet(context.Background(), "acc_01",
		"http://10.0.0.5:1080", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/egress", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "http://10.0.0.5:1080") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPIPatchAccount(t *testing.T) {
	w, s := newTestWebUI(t)
	body := strings.NewReader(`{"proxy_url":"http://10.0.0.5:1080","name":"新名字"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/accounts/acc_01", body)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	a, err := s.GetAccount(context.Background(), "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.ProxyURL != "http://10.0.0.5:1080" || a.Name != "新名字" {
		t.Fatalf("account = %+v", a)
	}
}

func TestAPIDeleteAccount(t *testing.T) {
	w, s := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/acc_01", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if _, err := s.GetAccount(context.Background(), "acc_01"); err == nil {
		t.Fatal("the account is still there")
	}
}

// TestAPIAccountsHideTheProxyCredentials says the account list does not
// carry the proxy's credentials.
//
// 账号列表是要给人看的，代理的账号密码不能跟着出去。
func TestAPIAccountsHideTheProxyCredentials(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	a.ProxyURL = "http://user:secret@127.0.0.1:7890"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret") {
		t.Fatalf("the credentials are in the answer: %s", body)
	}
	if !strings.Contains(body, "127.0.0.1:7890") {
		t.Fatalf("the address is not there either: %s", body)
	}
}

// TestAccountsPageHideTheProxyCredentials says the page does not carry
// the proxy's credentials either.
func TestAccountsPageHideTheProxyCredentials(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	a.ProxyURL = "http://user:secret@127.0.0.1:7890"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("the credentials are in the page: %s", rec.Body.String())
	}
}

// TestPatchKeepsTheProxyBehindTheShownAddress says the address the page
// shows is not written over the one that is stored.
//
// 页面上看到的是没有凭据的地址，它原样回去的时候不能把存着的凭据抹掉。
func TestPatchKeepsTheProxyBehindTheShownAddress(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	a.ProxyURL = "http://user:secret@127.0.0.1:7890"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"proxy_url":"http://***@127.0.0.1:7890"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/accounts/acc_01", body)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyURL != "http://user:secret@127.0.0.1:7890" {
		t.Fatalf("proxy_url = %q, want the one that is stored", got.ProxyURL)
	}
}

// TestTheAccountListCarriesTheLastError says the accounts API says why
// an account is in trouble.
//
// 账号在 error 或者 signed_out 的时候，光有状态是没用的：人要知道
// 该做什么。
func TestTheAccountListCarriesTheLastError(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	a.Status = store.StatusError
	a.LastError = "the refresh chain is broken: the account has to be signed in again"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		Accounts []struct {
			ID        string `json:"id"`
			LastError string `json:"lasterror"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if out.Accounts[0].LastError != a.LastError {
		t.Fatalf("last error = %q, want %q", out.Accounts[0].LastError, a.LastError)
	}
}

// TestTheAccountPageShowsTheLastError says the page says it too.
func TestTheAccountPageShowsTheLastError(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	a.Status = store.StatusError
	a.LastError = "the outcome of the last refresh is not known: the account has to be signed in again"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), a.LastError) {
		t.Fatalf("the last error is not on the page: %s", rec.Body.String())
	}
}

// TestTheAccountPageSaysNothingWhenThereIsNoError says a healthy
// account does not get a place for an error that is not there.
//
// 没有错就别占地方：一页账号，一半是空的说明，就成了一堵墙。
func TestTheAccountPageSaysNothingWhenThereIsNoError(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `class="err"`) {
		t.Fatalf("there is an error box for an account that is fine: %s", rec.Body.String())
	}
}

// TestTheLastErrorIsNotMarkup says the last error is shown as text, not
// as markup.
//
// 那句话是上游答上来的，里面可能有 <、& 和引号。它是给人看的，不是
// 给人执行的。
func TestTheLastErrorIsNotMarkup(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	a.Status = store.StatusError
	a.LastError = `<script>alert("x")</script>`
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `<script>alert`) {
		t.Fatalf("the last error was written as markup: %s", body)
	}
	if !strings.Contains(body, `&lt;script&gt;`) {
		t.Fatalf("the last error is not escaped: %s", body)
	}
}

// TestAPIPatchDoesNotUndoASignOut says changing an account through
// the API does not put a signed-out account back to active.
//
// 改账号不能把登出的账号改回 active。
func TestAPIPatchDoesNotUndoASignOut(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	if err := s.UpdateAccountStatus(ctx, "acc_01", store.StatusSignedOut,
		"the site refused the token", time.Now()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/accounts/acc_01",
		strings.NewReader(`{"name":"新名字","proxy_url":"http://10.0.0.5:1080"}`))
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}

	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusSignedOut {
		t.Errorf("the status is %q, want %q", a.Status, store.StatusSignedOut)
	}
	if a.LastError != "the site refused the token" {
		t.Errorf("the last error is %q", a.LastError)
	}
	// 改的还是要改。
	if a.Name != "新名字" || a.ProxyURL != "http://10.0.0.5:1080" {
		t.Errorf("account = %+v", a)
	}
}

// TestAPIDeleteOfAnAccountThatIsNotThere says deleting an account that
// is not there is a 404, not a 500.
//
// 删一个没有的账号是 404。
func TestAPIDeleteOfAnAccountThatIsNotThere(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/acc_nope", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestAPIPatchOfAnAccountThatIsNotThere says the same for a change.
//
// 改一个没有的账号也是 404。
func TestAPIPatchOfAnAccountThatIsNotThere(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/accounts/acc_nope",
		strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestTheImportPageIsThereWhenNoAPIKeysAreSet says the import page is
// there when the gateway has no key.
//
// 没有 key 的网关只听本机，明文密码不出这台机器，所以上传是开的。
func TestTheImportPageIsThereWhenNoAPIKeysAreSet(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "批量导入") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestTheImportPageIsThereWhenAPIKeysAreSet says the import page is
// there even when the gateway has a key.
//
// 配了 key 就把上传关掉，是从前的规矩，现在不关了：那把 key 就是后台
// 登录的那把，能登录的人就是管这个网关的人。明文密码走不走内网，是他
// 自己的选择。
func TestTheImportPageIsThereWhenAPIKeysAreSet(t *testing.T) {
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-x"}
	w, _ := newTestWebUIWithConfig(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "批量导入") {
		t.Fatalf("body = %q", body)
	}
	// 上传的口子得在页面上：只剩一句「这里不开放」不算数。
	if !strings.Contains(body, "import-form") {
		t.Fatalf("the upload form is not on the page: %q", body)
	}
	if strings.Contains(body, "不开放上传") {
		t.Fatalf("the page still says the upload is shut: %q", body)
	}
}

// TestTheImportLinkIsInTheNavWhenAPIKeysAreSet says the nav has the
// import link even when the gateway has a key.
//
// 导航里没有那个链接，页面上就等于没有这个功能：人得自己敲地址。
func TestTheImportLinkIsInTheNavWhenAPIKeysAreSet(t *testing.T) {
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-x"}
	w, _ := newTestWebUIWithConfig(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `href="/import"`) {
		t.Fatalf("the nav has no import link: %q", rec.Body.String())
	}
}

// TestTheImportIsTakenWithAPIKeysAreSet says the upload itself is
// taken even when the gateway has a key.
//
// 页面开着而接口关着，那个功能等于没有。
func TestTheImportIsTakenWithAPIKeysAreSet(t *testing.T) {
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-x"}
	w, _ := newTestWebUIWithConfig(t, cfg)
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// TestUploadingAFileAnswersWithAJobID says an upload gets an answer
// that says which run to watch.
//
// 导入在后台跑，回答里给的是那一次的编号。
func TestUploadingAFileAnswersWithAJobID(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"id\"") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestTheImportIsWrittenDown says the run is written down, so that the
// page can say how it went.
//
// 跑完了要有痕迹：不然人只能盯着一个转圈的页面等。
func TestTheImportIsWrittenDown(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// The import goes on in the background, so the run is waited for.
	var run store.ImportRun
	for i := 0; i < 100; i++ {
		run, err = s.GetImportRun(context.Background(), out.ID)
		if err == nil && !run.FinishedAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.FinishedAt.IsZero() {
		t.Fatalf("the run did not finish: %+v", run)
	}
	if run.OK != 1 || run.Failed != 0 {
		t.Fatalf("run = %+v", run)
	}
}

// TestTheAnswerToTheUploadHasNoPassword says the password of the list
// does not go back out in the answer.
//
// 清单里有明文密码，回答里不能有。
func TestTheAnswerToTheUploadHasNoPassword(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|secretpassword\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secretpassword") {
		t.Fatalf("the password is in the answer: %s", rec.Body.String())
	}
}

// TestTheBadLinesAreLoggedWithoutTheirPasswords says the log of a
// list that cannot be read has no password in it.
//
// 清单里的密码是明文，日志是会被人看见的东西。
func TestTheBadLinesAreLoggedWithoutTheirPasswords(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	var logged bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(old) })

	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("notanemail|secretpassword\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(logged.String(), "secretpassword") {
		t.Fatalf("the password is in the log: %s", logged.String())
	}
}

// TestTheImportTakesAPastedList says a list that is pasted in is
// taken as well as one that is uploaded.
//
// 页面上有两种交法：上传一个文件，或者贴一段清单。交的是同一件事：
// 每行 邮箱|密码。
func TestTheImportTakesAPastedList(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	body := strings.NewReader(`{"list":"a@example.com|pw\nb@example.com|pw\n"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// The import goes on in the background, so the run is waited for.
	var run store.ImportRun
	var err error
	for i := 0; i < 100; i++ {
		run, err = s.GetImportRun(context.Background(), out.ID)
		if err == nil && !run.FinishedAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.FinishedAt.IsZero() {
		t.Fatalf("the run did not finish: %+v", run)
	}
	if run.Total != 2 || run.OK != 2 {
		t.Fatalf("run = %+v", run)
	}
}

// TestTheImportTakesAHundredPastedLines says a list of exactly a
// hundred lines is taken.
//
// 上限是「最多 100 行」，100 行本身得收。
func TestTheImportTakesAHundredPastedLines(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	list := strings.Repeat("a@example.com|pw\n", 100)
	body := strings.NewReader(`{"list":` + strconv.Quote(list) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// TestTheImportRefusesALongerPastedList says a pasted list that is
// longer than a hundred lines is refused.
//
// 贴的那段是给人手写的那一份：超过 100 行就说不清了，宁可拒掉。
func TestTheImportRefusesALongerPastedList(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	list := strings.Repeat("a@example.com|pw\n", 101)
	body := strings.NewReader(`{"list":` + strconv.Quote(list) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "100") {
		t.Fatalf("the body does not say the limit: %q", rec.Body.String())
	}
}

// TestTheImportPageHasAPlaceToPasteTheList says the page has a box to
// paste the list into.
//
// 两种交法都要在页面上看得见：上传文件那一个口子，和贴清单那一个。
func TestTheImportPageHasAPlaceToPasteTheList(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "import-text") {
		t.Fatalf("the page has no box to paste the list into: %q", body)
	}
	if !strings.Contains(body, "100") {
		t.Fatalf("the page does not say the limit: %q", body)
	}
}

// TestTheImportPageShowsTheRunsBefore says the import page says what
// the last imports came to.
//
// 跑过的导入要在页面上看得见：人得知道刚才那趟成了几个。
func TestTheImportPageShowsTheRunsBefore(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	now := time.Now()
	if err := s.CreateImportRun(context.Background(), store.ImportRun{
		ID: "run_1", Source: "web", StartedAt: now, Total: 3, OK: 2, Failed: 1,
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "最近的导入") {
		t.Fatalf("the runs are not on the page: %q", body)
	}
	if !strings.Contains(body, "web") {
		t.Fatalf("the source of the run is not on the page: %q", body)
	}
}

// TestAListThatIsTooBigIsRefused says a list that is too big is turned
// away.
//
// 清单是有上限的：请求的大小是有限的，而清单进内存的地方比这个上限
// 大，所以它不会落到磁盘上的临时文件里去。
func TestAListThatIsTooBigIsRefused(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a@example.com|pw\n", 700000)
	fw.Write([]byte(big))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// TestAnUploadWithoutAFileIsRefused says an upload that has no file in
// it is turned away.
func TestAnUploadWithoutAFileIsRefused(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("other", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// TestTheUploadedListNeverLandsOnTheDisk says the uploaded list is not
// written to a temporary file.
//
// 清单里有明文密码：它只该在内存里走一趟，不该落到磁盘上。
// 清单比解析器的内存上限还大的时候，它本来会写进临时文件。
func TestTheUploadedListNeverLandsOnTheDisk(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	// 临时目录换成一个空的：请求之后它里面不该多出东西来。
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)

	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	// 五兆字节：比解析器给的内存还多，比请求的上限还少。
	fw.Write([]byte(strings.Repeat("a@example.com|pw\n", 300000)))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("the list is on the disk: %v", names)
	}
}

// TestTheReloginIsAskedFor says the relogin can be asked for.
//
// 重登是页面上一个按钮点出来的。
// 重登是页面上一个按钮点出来的。
func TestTheReloginIsAskedFor(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "/api/relogin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fixed") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestTheReloginPageListsTheSignedOutAccounts says the relogin page
// says which accounts are signed out.
//
// 签出了的账号要看得见，才知道该重登谁。
func TestTheReloginPageListsTheSignedOutAccounts(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: "acc_09", Name: "掉了的", Email: "b@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 没有密码就登不回去：这样的账号才拿得出手。
	if err := s.SetPassword(context.Background(), "acc_09", "cipher", now); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/relogin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "b@example.com") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestTheReloginPageHasNoAccountWithoutAPassword says an account that
// has no password is not on the relogin page.
//
// 没有密码就没有能登回去的东西，把它摆在那儿只会让人白按一次。
func TestTheReloginPageHasNoAccountWithoutAPassword(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: "acc_10", Name: "没有密码的", Email: "c@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/relogin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "c@example.com") {
		t.Fatalf("an account without a password is on the page: %q", rec.Body.String())
	}
}

// TestTheReloginPageHasNoCipherText says the page does not carry the
// password that is stored.
//
// 存的密码是加密的，可它也不该到页面上。
func TestTheReloginPageHasNoCipherText(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: "acc_11", Name: "掉了的", Email: "d@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(context.Background(), "acc_11", "ciphertext", now); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/relogin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ciphertext") {
		t.Fatalf("the stored password is on the page: %q", rec.Body.String())
	}
}

// TestTheImportPageSaysThePasswordsAreNotKept says the import page
// says so when the passwords are not kept.
//
// 没有密钥的时候密码不入库。页面上一个字都不说，人就以为每个账号以
// 后都能自己登回去——账号掉线的时候才知道，那时已经晚了。
func TestTheImportPageSaysThePasswordsAreNotKept(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	w.PasswordNote = "密码没有存进库里（没有 MMXCODE_PASSWORD_KEY）。"
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "密码没有存进库里") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestTheUploadAnswerIsJSONOnTheWire says the upload's answer reaches
// the browser as JSON.
//
// 这个测试要用真的服务器，不能用 httptest.NewRecorder：Recorder 的
// 头不会像真的服务器那样在 WriteHeader 的时候被定下来，所以头丢了
// 它也看不见。真的服务器上丢了这个头，浏览器拿到的就是
// text/plain，页面上的脚本读不出那个 id，上传明明成功了，页面上却是
// 「失败」。
func TestTheUploadAnswerIsJSONOnTheWire(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)

	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	resp, err := http.Post(srv.URL+"/api/import", part.FormDataContentType(), body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "json") {
		t.Fatalf("content type = %q, want json", ct)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" {
		t.Fatal("the answer has no run id")
	}
}

// TestTheWebImportReadsTheCreditsOfTheNewAccount says an account that
// just came in through the page has its credits read.
//
// 导入完了要等上一趟扫描（usage.refresh.scan_every，默认 30 秒）才看
// 得到积分，是不让人话的：刚进来的账号马上就读一次账本。
func TestTheWebImportReadsTheCreditsOfTheNewAccount(t *testing.T) {
	w, s := newTestWebUI(t)
	ctx := context.Background()
	w.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		return []provision.Result{{Email: "a@example.com", OK: true, AccountID: "acc_01"}}, nil
	}
	w.runImport("run_1", []provision.Entry{{Email: "a@example.com", Password: "pw"}})
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("the credits of the new account were not read: %d readings", len(snaps))
	}
}

// TestARunThatFailedAsAWholeStillHasAnArrayOfResults says a run that
// failed as a whole keeps a results that the page can read.
//
// 一整趟导入失败的时候（比如密钥配错了），没有一条结果。存下来的那
// 一串以前是 "null"：页面上那个脚本 JSON.parse('null') 解析出来是
// null，接着遍历就炸了，页面上写着「失败」，结果表也不出来。
func TestARunThatFailedAsAWholeStillHasAnArrayOfResults(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	w.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		return nil, errors.New("the key is not right")
	}
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// The import goes on in the background, so the run is waited for.
	var run store.ImportRun
	for i := 0; i < 100; i++ {
		run, err = s.GetImportRun(context.Background(), out.ID)
		if err == nil && !run.FinishedAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.FinishedAt.IsZero() {
		t.Fatalf("the run did not finish: %+v", run)
	}
	if run.Results == "" || !strings.HasPrefix(run.Results, "[") {
		t.Fatalf("results = %q, want an array", run.Results)
	}
	// The stored text has to be an array the page can walk through.
	var results []map[string]any
	if err := json.Unmarshal([]byte(run.Results), &results); err != nil {
		t.Fatal(err)
	}
	// "null" parses all right and then comes out as nothing: the page
	// walks through it and falls over.
	if results == nil {
		t.Fatalf("results = %q, want an array, not a null", run.Results)
	}
}

// TestTheReasonARunFailedAsAWholeIsNotLost says the reason a whole
// run failed does not disappear.
//
// 整趟失败的时候，原因原来既没进日志，也没进结果表：页面上只有「失
// 败 1」，人永远不知道是钥匙写错了还是站点在限流。钥匙写错这一件
// 事，页面上得说得出来。
func TestTheReasonARunFailedAsAWholeIsNotLost(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	w.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		return nil, errors.New("the key is not right")
	}
	var logged bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(old) })

	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// The import goes on in the background, so the run is waited for.
	var run store.ImportRun
	for i := 0; i < 100; i++ {
		run, err = s.GetImportRun(context.Background(), out.ID)
		if err == nil && !run.FinishedAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.FinishedAt.IsZero() {
		t.Fatalf("the run did not finish: %+v", run)
	}
	// The reason has to be in the results: that is what the page shows.
	if !strings.Contains(run.Results, "the key is not right") {
		t.Fatalf("results = %q, want the reason in them", run.Results)
	}
	// 那一行不是账号：这一趟一个账号都没处理成，失败的账号数是零。
	// 把整趟的失败算成账号的失败，页面上就会说「失败 1」，而清单上有
	// 一个账号——那一个其实不是失败在这一趟上的。
	if run.Failed != 0 {
		t.Fatalf("run = %+v, want no account counted as failed", run)
	}
	if run.OK != 0 {
		t.Fatalf("run = %+v, want no account counted as done", run)
	}
	// And in the log: the run has no column of its own for the reason.
	if !strings.Contains(logged.String(), "the key is not right") {
		t.Fatalf("the log does not say why the run failed: %q", logged.String())
	}
}

// TestTheHistorySaysWhyARunFailedAsAWhole says the page says why a run
// failed as a whole, after a reload.
//
// 整趟没成的导入，成功和失败都是零。刷新一次页面，光看那两个数，看不
// 出这一趟出了什么事——像是这一趟什么都没干。理由得摆在眼前：那一行
// 不是账号，可这一趟确实没成。
func TestTheHistorySaysWhyARunFailedAsAWhole(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	now := time.Now()
	if err := s.CreateImportRun(context.Background(), store.ImportRun{
		ID: "run_1", Source: "web", StartedAt: now, Total: 1,
	}); err != nil {
		t.Fatal(err)
	}
	results := `[{"email":"（整趟）","ok":false,"whole":true,"message":"整趟导入没能完成：the key is not right"}]`
	if err := s.FinishImportRun(context.Background(), "run_1", now, 0, 0, results); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "the key is not right") {
		t.Fatalf("the page does not say why the run failed: %q", body)
	}
}

// TestTheImportResultsScriptSurvivesAResultsThatIsNotAnArray says the
// page's script does not throw on a run whose results are not an array.
//
// 脚本那一半也守着：它拿到一个不是数组的东西时不许炸，也不许把结果表
// 藏着。这一半是 node 跑的：这个仓库没有别的跑法，node 不在的时候
// 这一条就跳过去——Go 那一半（存下来的结果得是 "[]"）还是照样守着。
func TestTheImportResultsScriptSurvivesAResultsThatIsNotAnArray(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node in this environment")
	}
	p := exec.Command(node, filepath.Join("testdata", "import_results_guard.js"))
	if out, err := p.CombinedOutput(); err != nil {
		t.Fatalf("the script guard failed: %v\n%s", err, out)
	}
}

// do asks the web UI a question and gives back the answer.
//
// 组的那几个接口要问的种类多：一个请求走一遍 recorder，比抄一遍四行强。
func (w *WebUI) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, r)
	return rec
}

// TestTheGroupIsMadeAndChanged says a group can be made and changed
// through the API.
//
// 组在库里，页面上能改 —— 这是这一版跟上一版最不一样的地方。
func TestTheGroupIsMadeAndChanged(t *testing.T) {
	w, _ := newTestWebUI(t)
	body := `{"name":"clash","urls":["http://a:8080","http://b:8080"]}`
	rec := w.do(t, http.MethodPost, "/api/proxy-groups", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	g, err := w.Store.GetProxyGroup(context.Background(), "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 2 {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
	// 改组：地址换掉。
	rec = w.do(t, http.MethodPost, "/api/proxy-groups", `{"name":"clash","urls":["http://c:8080"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	g, err = w.Store.GetProxyGroup(context.Background(), "clash")
	if err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if len(g.URLs) != 1 || g.URLs[0] != "http://c:8080" {
		t.Errorf("the group's addresses are %v", g.URLs)
	}
}

// TestTheGroupIsTakenAway says a group can be taken away, and that the
// accounts that pointed at it go back to the global group.
func TestTheGroupIsTakenAway(t *testing.T) {
	w, _ := newTestWebUI(t)
	ctx := context.Background()
	now := time.Now()
	if err := w.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "one", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create the account: %v", err)
	}
	if err := w.Store.SetAccountOutlet(ctx, "acc_1", "", "clash", now); err != nil {
		t.Fatalf("set the outlet: %v", err)
	}
	rec := w.do(t, http.MethodPost, "/api/proxy-groups", `{"name":"clash","urls":["http://a:8080"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	rec = w.do(t, http.MethodDelete, "/api/proxy-groups?name=clash", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	a, err := w.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "" {
		t.Errorf("the account still points at the group %q", a.ProxyGroup)
	}
}

// TestTheAccountPointsAtAGroup says an account can be pointed at a group
// through the API, and that the address and the group are one thing.
func TestTheAccountPointsAtAGroup(t *testing.T) {
	w, _ := newTestWebUI(t)
	ctx := context.Background()
	now := time.Now()
	if err := w.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "one", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create the account: %v", err)
	}
	rec := w.do(t, http.MethodPatch, "/api/accounts/acc_1", `{"proxy_group":"clash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
	a, err := w.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatalf("get the account: %v", err)
	}
	if a.ProxyGroup != "clash" {
		t.Errorf("the account's group is %q", a.ProxyGroup)
	}
	// 两个一起给是不行的：账号只有一条出口。
	rec = w.do(t, http.MethodPatch, "/api/accounts/acc_1",
		`{"proxy_group":"clash","proxy_url":"http://a:8080"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("the answer is %d: %s", rec.Code, rec.Body.String())
	}
}
