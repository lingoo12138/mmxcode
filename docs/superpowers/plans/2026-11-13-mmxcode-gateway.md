# mmxcode HTTP 网关 + WebUI + 部署 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在计划 A 的核心库之上建一个 HTTP 网关：Anthropic / OpenAI / Codex 三种协议直通 MiniMax Code 的聊天端点，账号按会话粘性绑定并在配额耗尽时自动换号，配一个服务端渲染的 WebUI 管理界面，以及 Docker（含 Cloudflare WARP profile）部署。

**Architecture:** `manager`（账号状态、token 续期、按账号取出口 client、用量刷新、签到）→ `rotator`（纯逻辑选号与故障转移判定）→ `gateway`（三协议路由、鉴权、错误改写、重放边界）→ `webui`（管理 API + 服务端渲染页面 + SSE）→ `scheduler`（定时签到 / 用量刷新 / token 续期 / 出口健康检查）。

**Tech Stack:** 计划 A 的全部依赖，加 `net/http`（标准库路由）、`html/template`（服务端渲染）、`golang.org/x/net/proxy`（SOCKS5，已在计划 A 引入）。

**设计文档：** `docs/superpowers/specs/2026-11-13-mmxcode-gateway-design.md`
**前置：** 计划 A（`docs/superpowers/plans/2026-11-13-mmxcode-core.md`）已实现并通过测试。本计划直接建在它的包之上。

---

## 本计划与计划 A 的关系

计划 A 已经交付：登录、多账号、签到、积分汇总、出口解析、SQLite 存储、CLI。

本计划交付剩下的：

| 能力 | 落在哪 |
|---|---|
| 账号轮转（会话粘性 + 故障转移） | `internal/rotator` |
| 三协议网关 | `internal/gateway` |
| Codex 协议本地转换 | `internal/convert` |
| WebUI 与管理 API | `internal/webui` |
| 定时任务 | `internal/scheduler` |
| Docker 与 WARP 部署 | `Dockerfile` / `docker-compose.yml` |

计划 A 的 `internal/oauth`、`internal/account`、`internal/checkin`、`internal/usage`、`internal/egress`、`internal/store`、`internal/upstream`、`internal/sign`、`internal/site`、`internal/config` 在本计划中被复用，个别文件按需修改（下面每个 Task 都写明改哪里、改成什么）。

---

## 三条必须守住的不变量

这三条是参考插件踩过的坑，写错了会浪费掉一次性的 refresh token，或者把流式响应变成一整块：

1. **跨站重定向不换号、不消耗 refresh token。** MiniMax 国际站从中国大陆网络访问会 307 到中国站；两个站的 token 不通用，跟着跳会被误判成"登录被拒"。对外返回 502 + 明确提示。

2. **只有在响应头还没写给客户端之前才允许换号重放。** SSE 一旦吐了字节就不能再切账号。

3. **上游请求必须显式带 `accept-encoding: identity`。** MiniMax 会把被要求压缩的响应压成单个 gzip member 并在结束时 flush，流式响应的所有事件会一次性到达，首个 token 等到最后一个。Go 的 `http.Transport` 默认自动加 `Accept-Encoding: gzip`，必须覆盖。

---

## 文件结构

```
cmd/mmxcode/
  serve.go             serve 子命令（新增）
internal/manager/
  manager.go           账号状态、token 续期、按账号取 client、用量刷新、签到
  manager_test.go
internal/rotator/
  rotator.go           选号、会话亲和、耗尽判定
  decide.go            故障转移判定（纯函数）
  session.go           会话键推导
  rotator_test.go
internal/convert/
  requests.go          Responses 请求 → Anthropic Messages 请求
  stream.go            Anthropic SSE → Responses SSE
  convert_test.go
internal/upstream/
  client.go            改：Do 支持按请求指定 transport
  chat.go              聊天请求的构造与上游调用
  chat_test.go
internal/gateway/
  gateway.go           路由与聊天处理
  auth.go              api key 鉴权
  errors.go            错误改写
  gateway_test.go
internal/webui/
  webui.go             管理 API 与页面
  templates/
    layout.html
    accounts.html
    egress.html
    usage.html
    login.html
  static/app.js
internal/scheduler/
  scheduler.go         定时任务
  scheduler_test.go
Dockerfile
docker-compose.yml
.env.example
```

---

### Task 1: manager 包

**Files:**
- Create: `internal/manager/manager.go`
- Create: `internal/manager/manager_test.go`

manager 是 gateway、webui、scheduler 共同依赖的那一层：它知道一个账号现在用什么 token、从哪个出口出去、它的积分是多少。

- [ ] **Step 1: 写失败的测试**

创建 `internal/manager/manager_test.go`：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/manager/...
```

Expected: FAIL —— `undefined: New`、`undefined: Manager`

- [ ] **Step 3: 实现 manager**

创建 `internal/manager/manager.go`：

```go
// Package manager owns the accounts' running state: which token each
// one has, which way out its requests go, what its credits are and
// whether it has checked in.
//
// Everything above it — the gateway, the WebUI, the scheduler — goes
// through this package rather than touching the store and the upstream
// client on its own, so that a token is renewed in one place and a
// refresh token is never spent twice.
//
// There are two account ids, and they are not the same thing. The
// store's id is the manager's own key for an account, and the renewer
// is keyed by it. The site's own id is the one an access token carries,
// in oauth.Token.AccountID. No account id goes into a token in this
// package: the store's id there would have the renewer walk a second
// chain for one account and spend two refresh tokens, which signs the
// account out.
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/checkin"
	"mmxcode/internal/egress"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// renewWindow is how long before a token ends it is renewed.
//
// The reference plugin renews ten minutes early; the site hands out
// access tokens that live an hour.
const renewWindow = 10 * time.Minute

// accountTimeout is how long the account API is waited for.
//
// The account API answers in a moment: the membership, the ledger and
// the plan's windows are three small answers. A stalled one gets
// little of that, so that one account's reading does not hold up the
// others'.
const accountTimeout = 5 * time.Second

// The reasons an account's last error says.
//
// They say which case it is, because the two are mended in different
// ways: a broken chain needs a human, an unknown outcome does not.
const (
	// chainBrokenReason says the renewal chain cannot go on.
	chainBrokenReason = "the refresh chain is broken: the refresh token was spent already and there is no newer one, so the account has to be signed in again"
	// unknownOutcomeReason says a refresh was sent and its answer was lost.
	unknownOutcomeReason = "the outcome of the last refresh is not known: the site may have spent the refresh token, so it was not sent again"
)

// Manager holds the gateway's accounts.
type Manager struct {
	// Store is the database.
	Store *store.Store
	// Up is the client for MiniMax's hosts.
	Up *upstream.Client
	// Acc reads the account API.
	Acc *account.Client
	// Egress resolves the way out and hands out clients.
	Egress *egress.Resolver
	// Renewer spends refresh tokens, once each.
	Renewer *oauth.Renewer
	// TokenURL is where a refresh token is traded in.
	TokenURL string
}

// New makes a manager.
func New(s *store.Store, up *upstream.Client, acc *account.Client, egr *egress.Resolver, rw *oauth.Renewer) *Manager {
	return &Manager{
		Store: s, Up: up, Acc: acc, Egress: egr, Renewer: rw,
		TokenURL: site.AccountURL(site.TokenPath),
	}
}

// Accounts gives every account.
func (m *Manager) Accounts(ctx context.Context) ([]store.Account, error) {
	return m.Store.ListAccounts(ctx)
}

// Account gives one account.
func (m *Manager) Account(ctx context.Context, id string) (store.Account, error) {
	return m.Store.GetAccount(ctx, id)
}

// Token gives an account's access token, renewing it when it is about
// to end.
//
// The renewal goes through the renewer, so a burst of requests finds
// one renewal in flight rather than several, and the one-time refresh
// token is sent once.
func (m *Manager) Token(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account %s is not signed in", accountID)
	}
	if stored.Access == "" && stored.Refresh == "" {
		// The sign-in was taken away: the site has refused it. There
		// is nothing to send, so the account fails here instead of
		// paying for a renewal that cannot work.
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	cur := tokenFromStore(stored)
	if cur.Expires.After(time.Now().Add(renewWindow)) {
		return cur, nil
	}
	return m.Renew(ctx, accountID, cur)
}

// tokenFromStore gives the token the store holds.
//
// The site's own account id is not in the store, so the field stays
// empty: the store's id is the manager's key for the account, the
// renewer is keyed by it, and a token that carried it would be a token
// whose account id is the wrong one. The site puts its own id in the
// tokens it hands out, and that one is kept.
func tokenFromStore(stored store.Token) *oauth.Token {
	return &oauth.Token{
		Access:  stored.Access,
		Refresh: stored.Refresh,
		Expires: stored.ExpiresAt,
		Subject: stored.Subject,
	}
}

// Renew trades an account's refresh token for a new one.
//
// The failures are told apart, because they are mended in different
// ways:
//
//   - The site refuses the refresh token (invalid_grant). The account
//     is signed out — unless the renewer holds a newer token: a token
//     that is spent already is refused the same way a revoked sign-in
//     is, and a stale token must not sign out an account that is alive.
//   - The chain cannot go on (oauth.ErrRefreshChainBroken). No token
//     is left that the site would take, so the account has to be signed
//     in again by hand.
//   - The answer was lost (oauth.ErrRefreshOutcomeUnknown). The site
//     may have spent the token, so it is not sent again, but the site
//     never said the sign-in is no good: the account is not signed out.
func (m *Manager) Renew(ctx context.Context, accountID string, cur *oauth.Token) (*oauth.Token, error) {
	if cur == nil {
		return nil, fmt.Errorf("no token to renew")
	}
	// What the renewer holds is asked for before the renewal: a
	// renewal that walks the chain drops what the renewer holds, so
	// afterwards there may be nothing left to ask about.
	held, _ := m.Renewer.Newest(accountID)
	// The token that really goes to the site is remembered: the
	// renewer may walk the chain and spend a token other than the
	// caller's, and a refusal is about the token that was sent.
	var sent spentRefresh
	tok, err := m.Renewer.Renew(ctx, accountID, cur, func(ctx context.Context, refresh string) (*oauth.Token, error) {
		sent.set(refresh)
		return oauth.Refresh(ctx, m.Up, m.TokenURL, refresh)
	})
	if err != nil {
		return m.renewFailed(ctx, accountID, cur, sent.get(), held, err)
	}
	if err := m.saveToken(ctx, accountID, tok); err != nil {
		return nil, err
	}
	return tok, nil
}

// renewFailed says what a failed renewal means for the account.
//
// It gives the token to use when there is one — a refused token that
// was only stale is not the end of the account — and the error to give
// back when there is not.
func (m *Manager) renewFailed(ctx context.Context, accountID string, cur *oauth.Token,
	refused string, held *oauth.Token, err error) (*oauth.Token, error) {
	switch {
	case errors.Is(err, oauth.ErrRefreshChainBroken):
		// The chain cannot go on: the token in hand is spent and there
		// is no newer one the site would take. Only a human can mend
		// this, by signing the account in again.
		if mErr := m.markSignedOut(ctx, accountID, chainBrokenReason); mErr != nil {
			return nil, errors.Join(err, fmt.Errorf("the broken chain could not be saved: %w", mErr))
		}
		return nil, err
	case errors.Is(err, oauth.ErrRefreshOutcomeUnknown):
		// The request went out and the answer was lost: the site may
		// have spent the token, so it is not sent again — the renewer
		// knows that — but the site never said the sign-in is no good,
		// so the account is not signed out. The error is written down
		// all the same, so that the WebUI shows that this account is
		// in trouble.
		if mErr := m.markError(ctx, accountID, unknownOutcomeReason); mErr != nil {
			return nil, errors.Join(err, fmt.Errorf("the error could not be saved: %w", mErr))
		}
		return nil, err
	}
	var oe *oauth.Error
	if !asOAuthError(err, &oe) || !oe.IsSignedOut() {
		return nil, err
	}
	// The site refused a refresh token. The account is signed out —
	// unless the refused token was stale and the renewer holds a newer
	// one: a token that is spent already is refused the same way a
	// revoked sign-in is, and an account that is alive must not be
	// signed out over a token nobody uses any more.
	//
	// What the renewer holds is asked for again: a renewal that walks
	// the chain may have left it with nothing, and what it held before
	// is then the only thing there is to say.
	newest, ok := m.Renewer.Newest(accountID)
	if !ok {
		newest = held
	}
	if newer := m.newerToken(cur, refused, newest); newer != nil {
		if sErr := m.saveToken(ctx, accountID, newer); sErr != nil {
			return nil, errors.Join(err, sErr)
		}
		return newer, nil
	}
	// The sign-out has to be written down. An account that stays active
	// with a token the site has refused is handed out again and again,
	// and every request pays for a renewal that cannot work.
	if mErr := m.markSignedOut(ctx, accountID, err.Error()); mErr != nil {
		return nil, errors.Join(err, fmt.Errorf("the sign-out could not be saved: %w", mErr))
	}
	return nil, err
}

// newerToken gives the token to use when the token that was refused was
// only stale.
//
// The site answers invalid_grant for a refresh token that is spent
// already, and that is what a revoked sign-in looks like too. So a
// refusal is only the end of the account when there is nothing newer
// to try: the renewer may hold a token from a sign-in or from another
// process, and then the refused token was merely stale.
//
// The token the renewer holds has to be a different one — the refused
// token is the one it holds too when the chain was walked, and that one
// is dead — and a newer one: the renewer may hold the older of the two,
// and a fresh sign-in in the store is newer than what the renewer has.
// The expiry says which one is newer, the same way it says it for the
// store.
func (m *Manager) newerToken(cur *oauth.Token, refused string, held *oauth.Token) *oauth.Token {
	if held == nil || held.Refresh == "" {
		return nil
	}
	if held.Refresh == refused {
		return nil
	}
	if !held.Expires.IsZero() && !cur.Expires.IsZero() && held.Expires.Before(cur.Expires) {
		return nil
	}
	return held
}

// saveToken writes an account's token to the store.
//
// The token is written only when it is newer than the one the store
// holds. The chain advances on its own, so two renewals can answer
// with two different tokens, and the older one can arrive second: the
// older token's refresh has been spent already — the newer one was
// bought with it — and writing it down would leave the store with a
// refresh token the site refuses, which signs the account out at the
// next renewal.
//
// The expiry says which token is newer: the site hands out access
// tokens that live an hour, so the token of the later renewal ends
// later. Two tokens that end at the same time are the same step of the
// chain, and the one in the store is kept: writing the other one down
// buys nothing and risks a refresh token that is spent already.
func (m *Manager) saveToken(ctx context.Context, accountID string, tok *oauth.Token) error {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err == nil && !stored.ExpiresAt.Before(tok.Expires) {
		// The store already holds a token that is at least as new.
		return nil
	}
	// A token that could not be read is written over: the store is
	// the only place the token lives, and a renewal that is not
	// written down is a renewal that is lost.
	return m.Store.SaveToken(ctx, store.Token{
		AccountID: accountID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: time.Now(),
	})
}

// ForceRenew renews an account's token without waiting for it to be
// near its end.
//
// It is what the gateway does when the site answers 401.
//
// The renewer is not told to forget the account, and it is not told to
// reset the chain either. The token it holds may be newer than the one
// in the store -- a renewal that worked but whose writing failed leaves
// the store behind -- and it is the only way out of that: the refresh
// token in the store is spent, and a spent refresh token must not be
// sent again. A reset would drop the token that is the only way out, and
// a forget would drop the record of the tokens already spent, so that
// one of them would be sent again.
func (m *Manager) ForceRenew(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account %s is not signed in", accountID)
	}
	if stored.Access == "" && stored.Refresh == "" {
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	return m.Renew(ctx, accountID, tokenFromStore(stored))
}

// markSignedOut records that an account's sign-in is gone.
//
// The sign-in is taken away as well: an account whose sign-in the site
// has refused must not keep a token that every request would try to
// renew.
//
// The error is not swallowed: a sign-out that could not be written
// down is an account that stays active with a token the site refuses.
func (m *Manager) markSignedOut(ctx context.Context, accountID, reason string) error {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	a.Status = store.StatusSignedOut
	a.LastError = reason
	a.UpdatedAt = time.Now()
	if err := m.Store.UpdateAccount(ctx, a); err != nil {
		return err
	}
	// The token row is blanked: the sign-in is gone, so the token is
	// of no use, and keeping it would send the account back to the
	// site for a renewal that was already refused.
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: accountID,
		UpdatedAt: time.Now(),
	}); err != nil {
		return err
	}
	// The renewer forgets the account too: a signed-out account must
	// not be able to renew, and a token of the old sign-in must not be
	// waiting when the account signs in again. This is the one place
	// where forgetting is right: the account is signed out for good,
	// so nothing of the old chain is of use any more.
	m.Renewer.Forget(accountID)
	return nil
}

// markError writes an account's last error down, without signing it
// out.
//
// An account whose refresh answer was lost is not a signed-out
// account: the site never said the sign-in is no good, and the access
// token may work until it ends. The error is written down all the same
// and the account is put in the error state, so that the WebUI shows
// that this account is in trouble instead of showing it as a normal
// one.
//
// The renewer is not told anything: it has to keep the record of the
// token whose outcome is not known, because that token must not be sent
// again.
func (m *Manager) markError(ctx context.Context, accountID, reason string) error {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	a.Status = store.StatusError
	a.LastError = reason
	a.UpdatedAt = time.Now()
	return m.Store.UpdateAccount(ctx, a)
}

// spentRefresh remembers the refresh token that really went to the
// site.
//
// The renewer walks the chain, so the token that is spent is not always
// the one the caller asked about. The renewal runs in a goroutine of
// its own and the caller may stop waiting for it, so the remembering
// has to be safe.
type spentRefresh struct {
	mu  sync.Mutex
	tok string
}

// set remembers the refresh token that was sent.
func (s *spentRefresh) set(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tok = tok
}

// get gives the refresh token that was sent.
func (s *spentRefresh) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tok
}

// accFor gives the account API client an account's calls go out with.
//
// Every fetch an account makes goes through the account's own way out,
// or the global one. An account behind a proxy that is not used for
// its usage reading is an account whose reading never arrives.
//
// The transport is cached by the resolver, so making a client for it is
// cheap.
func (m *Manager) accFor(a store.Account) (*account.Client, error) {
	tr, err := m.Egress.Transport(m.Egress.Resolve(a))
	if err != nil {
		return nil, err
	}
	return m.Acc.With(upstream.New(upstream.Options{Timeout: accountTimeout, Transport: tr})), nil
}

// Client gives the HTTP client an account's requests go out with, and
// the address of that way out.
//
// The address is redacted: it is shown to the user, and the proxy's
// credentials are a secret. The client itself carries the real
// address, so nothing is lost by hiding it here.
func (m *Manager) Client(ctx context.Context, accountID string) (*http.Client, string, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, "", err
	}
	url := m.Egress.Resolve(a)
	c, err := m.Egress.Client(url)
	if err != nil {
		return nil, egress.Redacted(url), err
	}
	return c, egress.Redacted(url), nil
}

// RefreshUsage reads an account's credits and windows and saves them.
//
// The membership comes first: it names the group the plan's windows
// are under, and the console asks for the windows of that group.
//
// A refused sign-in is not a missing reading: the account is marked
// signed out, so that a plan that could not be read is not mistaken
// for a plan without windows.
//
// A reading that failed is written down as well: the snapshot of the
// failed reading says so, so the numbers of the reading before it are
// not passed off as current.
func (m *Manager) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return store.UsageSnapshot{}, err
	}
	tok, err := m.Token(ctx, accountID)
	if err != nil {
		return store.UsageSnapshot{}, err
	}
	snap := store.UsageSnapshot{AccountID: accountID, CreatedAt: time.Now()}

	acc, err := m.accFor(a)
	if err != nil {
		return store.UsageSnapshot{}, err
	}

	// The membership is read before the plan, because it names the
	// group the plan's windows are under.
	group := ""
	if mem, err := acc.Membership(ctx, tok.Access, a.UserID); err == nil {
		snap.Balance = mem.Balance
		snap.PlanUntil = mem.ExpiresAt
		group = mem.GroupID
	} else {
		// The error is kept: a membership that could not be read is
		// not a membership without a group, and the windows are
		// asked for under that group.
		snap.MembershipError = err.Error()
		if isRefusal(err) {
			if err := m.markSignedOut(ctx, accountID, err.Error()); err != nil {
				return store.UsageSnapshot{}, err
			}
		}
	}

	batches, err := acc.Ledger(ctx, tok.Access, a.UserID)
	if err != nil {
		// The reading failed. It is written down, so that a consumer
		// can say "the last reading of this account failed" instead
		// of showing the numbers of the reading before it as if they
		// were current.
		snap.HasAmounts = false
		if sErr := m.Store.SaveUsageSnapshot(ctx, snap); sErr != nil {
			return store.UsageSnapshot{}, errors.Join(err, sErr)
		}
		return snap, err
	}
	if batches == nil {
		// The ledger could not be read. The credits are unknown, and
		// an unknown share is not a share of zero. The reading is
		// written down all the same, so that it is known to have
		// failed.
		snap.HasAmounts = false
		if sErr := m.Store.SaveUsageSnapshot(ctx, snap); sErr != nil {
			return store.UsageSnapshot{}, sErr
		}
		return snap, nil
	}
	now := time.Now()
	var granted, consumed, remaining float64
	live := false
	unknown := false
	expired := false
	var soonest time.Time
	var lastExpiry time.Time
	for _, b := range batches {
		if b.CreditType != account.CreditTypePurchase && b.CreditType != account.CreditTypeCheckin {
			// A bucket the plan does not count.
			continue
		}
		if !b.HasExpiresAt {
			// The credits cannot be read: the batch does not say
			// when it runs out, so a live batch cannot be told
			// from a spent one, and the balance would be wrong.
			unknown = true
			break
		}
		if !b.ExpiresAt.After(now) {
			// The batch has run out. That is not a loss: the
			// account's credits ran out, which is not the same as
			// the credits being unknown.
			expired = true
			if b.ExpiresAt.After(lastExpiry) {
				lastExpiry = b.ExpiresAt
			}
			continue
		}
		granted += b.Granted
		consumed += b.Consumed
		remaining += b.Remaining
		live = true
		if soonest.IsZero() || b.ExpiresAt.Before(soonest) {
			soonest = b.ExpiresAt
		}
	}
	switch {
	case unknown:
		// A part of the ledger would tell a wrong share, so no
		// share is told at all.
		snap.HasAmounts = false
	default:
		// The ledger was read: the account has what it has. A ledger
		// with nothing live in it means the account has no credits
		// left, which is not the same as a ledger that could not be
		// read.
		snap.HasAmounts = true
		snap.Granted, snap.Consumed, snap.Remaining = granted, consumed, remaining
		switch {
		case live:
			snap.ExpiresAt = soonest
		case expired:
			// Every batch had run out: the account ran out at this
			// time, and the summary can say so.
			snap.ExpiresAt = lastExpiry
		}
	}

	if ws, err := acc.PlanWindows(ctx, tok.Access, group); err == nil {
		if len(ws) > 0 {
			if b, err := json.Marshal(ws); err == nil {
				snap.WindowsJSON = string(b)
			}
		}
	} else {
		// The error is kept: a plan whose windows could not be read
		// is not a plan without windows, and the caller decides what
		// to do with the account.
		snap.WindowsError = err.Error()
		if isRefusal(err) {
			if err := m.markSignedOut(ctx, accountID, err.Error()); err != nil {
				return store.UsageSnapshot{}, err
			}
		}
	}
	if title, err := acc.PlanTitle(ctx, tok.Access, group); err == nil {
		snap.Plan = title
	} else if isRefusal(err) {
		if err := m.markSignedOut(ctx, accountID, err.Error()); err != nil {
			return store.UsageSnapshot{}, err
		}
	}

	if err := m.Store.SaveUsageSnapshot(ctx, snap); err != nil {
		return store.UsageSnapshot{}, err
	}
	return snap, nil
}

// Checkin claims an account's daily credits.
func (m *Manager) Checkin(ctx context.Context, accountID string) (checkin.Outcome, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return checkin.Outcome{}, err
	}
	tok, err := m.Token(ctx, accountID)
	if err != nil {
		return checkin.Outcome{}, err
	}
	acc, err := m.accFor(a)
	if err != nil {
		return checkin.Outcome{}, err
	}
	out, err := checkin.New(acc).Run(ctx, tok.Access, a.UserID)
	if err != nil {
		return checkin.Outcome{}, err
	}
	day := store.DayKey(time.Now())
	_ = m.Store.SaveCheckin(ctx, store.Checkin{
		AccountID: accountID,
		Day:       day,
		Outcome:   out.Outcome,
		Credit:    out.Credit,
		Streak:    out.Streak,
		Message:   out.Message,
		CreatedAt: time.Now(),
	})
	return out, nil
}

// LatestWindows gives an account's saved windows.
//
// It is the windows of the account's newest reading: an earlier
// reading's windows are not looked at. When that reading could not
// read the windows, the error is given: a plan whose windows could
// not be read is not a plan without windows, and the caller decides
// what to do with the account.
func (m *Manager) LatestWindows(ctx context.Context, accountID string) ([]account.Window, error) {
	snap, err := m.Store.LatestUsageSnapshot(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if snap.WindowsError != "" {
		return nil, fmt.Errorf("the plan's windows could not be read: %s", snap.WindowsError)
	}
	if snap.WindowsJSON == "" {
		return nil, nil
	}
	var ws []account.Window
	if err := json.Unmarshal([]byte(snap.WindowsJSON), &ws); err != nil {
		return nil, err
	}
	return ws, nil
}

// asOAuthError unwraps an error into an OAuth error.
func asOAuthError(err error, target **oauth.Error) bool {
	return errors.As(err, target)
}

// isRefusal says an error is a refusal of the sign-in.
//
// A refused sign-in is one event, however it arrives: an HTTP 401 or
// 403, or MiniMax's own code 1000048 in the body of a 200.
func isRefusal(err error) bool {
	if err == nil {
		return false
	}
	var refused *account.RefusedError
	return errors.As(err, &refused) || errors.Is(err, upstream.ErrSignInRefused)
}

// WindowGroup says which plan window a model spends from.
//
// The plan's windows are per model group: the chat models spend the
// "general" group, the video models spend another.
func WindowGroup(model string) string {
	if strings.Contains(strings.ToLower(model), "video") {
		return "video"
	}
	return "general"
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/manager/...
```

Expected: PASS —— 6 个并发请求只花掉 1 次 refresh token

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/manager/
git add internal/manager
git commit -m "feat: manager 包

账号状态、token 续期、按账号取出口、用量刷新与签到。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: upstream 聊天客户端

**Files:**
- Modify: `internal/upstream/client.go`（把重定向循环提出来，支持按请求指定 transport）
- Create: `internal/upstream/chat.go`
- Create: `internal/upstream/chat_test.go`

- [ ] **Step 1: 改写 client.go 的 Do**

把 `internal/upstream/client.go` 里的 `Client.Do` 整个换成下面这段，并把 `DoWith` 加进去。改完后 `Client.Do` 就是 `DoWith` 的一个包装：

```go
// Do sends a request through the client's own transport.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return doWith(req, c.http.Transport, c.timeout, c.userAgent)
}

// DoWith sends a request through a given transport.
//
// A redirect that stays on the host is followed; one that leaves it is
// an OtherSiteError, and the request is not sent on.
//
// The transport is a parameter because the proxy is a property of the
// transport: an account with its own exit needs its own client, and
// the same client is shared by every account that shares an exit.
//
// There is no timeout here: a chat streams for as long as it needs,
// and the caller's context carries the deadline.
func DoWith(req *http.Request, transport http.RoundTripper) (*http.Response, error) {
	return doWith(req, transport, 0, "")
}

// doWith is the redirect loop both of them share.
//
// The caller's request is not touched: the User-Agent goes into a
// copy, and a request with no header map at all is given one, so
// writing into it cannot panic.
func doWith(req *http.Request, transport http.RoundTripper, timeout time.Duration, userAgent string) (*http.Response, error) {
	if transport == nil {
		transport = defaultTransport()
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	current := copyRequest(req)
	if current.Header.Get("User-Agent") == "" && userAgent != "" {
		current.Header.Set("User-Agent", userAgent)
	}
	for hop := 0; ; hop++ {
		resp, err := client.Do(current)
		if err != nil {
			return nil, err
		}
		loc := resp.Header.Get("Location")
		if loc == "" || !redirectStatuses[resp.StatusCode] {
			return resp, nil
		}
		next, err := current.URL.Parse(loc)
		if err != nil {
			return resp, nil
		}
		if isDowngrade(current.URL, next) && sameAddress(current.URL, next) {
			// The sign-in would go out in cleartext, so the redirect is
			// refused and never followed.
			resp.Body.Close()
			return nil, &SchemeDowngradeError{
				From:   schemeHost(current.URL),
				To:     schemeHost(next),
				Status: resp.StatusCode,
			}
		}
		if !sameHost(next, current.URL) {
			resp.Body.Close()
			return nil, &OtherSiteError{
				From:   current.URL.Host,
				To:     next.Host,
				Status: resp.StatusCode,
			}
		}
		if signed(current.Header) && !sameRequest(current.URL, next) {
			// The signature is made over the path and the query, so the
			// old one cannot be replayed. Sending it would be refused by
			// the upstream, and that refusal reads as a dead token.
			resp.Body.Close()
			return nil, &StaleSignatureError{
				From:   schemeHost(current.URL),
				To:     schemeHost(next),
				Status: resp.StatusCode,
			}
		}
		if hop >= 5 {
			// Too many redirects is an error, not an answer: handing back
			// the redirect itself would leave the caller with a body
			// nobody read.
			resp.Body.Close()
			return nil, fmt.Errorf("too many redirects")
		}
		// The body has to be read again: http.Request.Body is a reader
		// the previous request already consumed, so replaying it as it
		// is would send an empty body — and a chat with an empty body is
		// not the chat that was asked for. GetBody is what reads it a
		// second time; without it the body cannot be replayed, so the
		// answer is handed back as it came.
		method, body, length := current.Method, current.Body, current.ContentLength
		replay := false
		if resp.StatusCode != 307 && resp.StatusCode != 308 {
			if method != http.MethodGet && method != http.MethodHead {
				method, body, length = http.MethodGet, nil, 0
			}
		} else if body != nil {
			if current.GetBody == nil {
				return resp, nil
			}
			fresh, err := current.GetBody()
			if err != nil {
				return resp, nil
			}
			body = fresh
			replay = true
		}
		resp.Body.Close()
		nextReq, err := http.NewRequestWithContext(current.Context(), method, next.String(), body)
		if err != nil {
			return nil, err
		}
		nextReq.Header = current.Header.Clone()
		if replay {
			// The body came out of GetBody, which hands out a plain
			// reader, and http.NewRequest only recognises bytes and
			// strings readers. Without the length the replay would go
			// out with no Content-Length at all, chunked instead of
			// what the first request had.
			if length != 0 {
				nextReq.ContentLength = length
			}
			// The body is a reader now, so the next hop would not know
			// how to read it again. Keep the way to do it.
			nextReq.GetBody = current.GetBody
		}
		current = nextReq
	}
}
```

`DoWith` 里 `client.Do(current)` 的第二个返回值在 `err != nil` 时，Go 会把它包进 `*url.Error`，所以 `OtherSiteError` 会被包一层。用 `errors.As` 取。

`sameHost`、`effectivePort`、`copyRequest`、`isDowngrade`、`sameAddress`、`signed`、
`sameRequest`、`schemeHost` 和 `time` 在 `client.go` 里已经有了（计划 A 的 Task 4
写的），这里直接用。

同时删掉 `client.go` 里旧的 `Do` 函数体（保留 `defaultTransport`、`redirectStatuses`、
`sameHost` 和上面那几个小函数）。

- [ ] **Step 2: 写失败的测试**

创建 `internal/upstream/chat_test.go`：

```go
package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatSendsTheRightHeaders(t *testing.T) {
	var got http.Header
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotURL = r.URL.String()
		w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{
		BaseURL:   srv.URL,
		Path:      "/mavis/api/v1/llm/v1/messages",
		Access:    "acc-token",
		SessionID: "sess-1",
		TZOffset:  -480,
		Body:      []byte(`{"model":"MiniMax-M3"}`),
		Stream:    false,
		Anthropic: true,
	}
	resp, err := c.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if !strings.HasSuffix(gotURL, "/mavis/api/v1/llm/v1/messages") {
		t.Fatalf("url = %s", gotURL)
	}
	if got.Get("Authorization") != "Bearer acc-token" {
		t.Fatalf("authorization = %q", got.Get("Authorization"))
	}
	if got.Get("x-api-key") != "sk-xxx" {
		t.Fatalf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("User-Agent") != "MiniMaxAgent" {
		t.Fatalf("user-agent = %q", got.Get("User-Agent"))
	}
	if got.Get("X-Mavis-Agent-Id") != "main" {
		t.Fatalf("agent id = %q", got.Get("X-Mavis-Agent-Id"))
	}
	if got.Get("X-Mavis-Session-Id") != "sess-1" {
		t.Fatalf("session id = %q", got.Get("X-Mavis-Session-Id"))
	}
	if got.Get("X-Mavis-Timezone-Offset") != "-480" {
		t.Fatalf("tz = %q", got.Get("X-Mavis-Timezone-Offset"))
	}
	if got.Get("accept-encoding") != "identity" {
		t.Fatalf("accept-encoding = %q, want identity", got.Get("accept-encoding"))
	}
	if got.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got.Get("anthropic-version"))
	}
}

func TestChatKeepsTheClientAnthropicVersion(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{
		BaseURL: srv.URL, Path: "/mavis/api/v1/llm/v1/messages",
		Access: "a", Body: []byte(`{}`), Anthropic: true,
		Extra: map[string]string{"anthropic-version": "2099-01-01"},
	}
	resp, err := c.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("anthropic-version") != "2099-01-01" {
		t.Fatalf("anthropic-version = %q", got.Get("anthropic-version"))
	}
}

func TestChatSendsTheBody(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{
		BaseURL: srv.URL, Path: "/p", Access: "a",
		Body: []byte(`{"model":"x"}`),
	}
	resp, err := c.Chat(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if body != `{"model":"x"}` {
		t.Fatalf("body = %q", body)
	}
}

func TestChatUsesTheGivenTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	tr := &http.Transport{}
	req := ChatRequest{BaseURL: srv.URL, Path: "/p", Access: "a", Body: []byte(`{}`)}
	resp, err := c.Chat(context.Background(), req, tr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestChatReportsOtherSite(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req := ChatRequest{BaseURL: srv.URL, Path: "/p", Access: "a", Body: []byte(`{}`)}
	_, err := c.Chat(context.Background(), req, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var oe *OtherSiteError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v (%T)", err, err)
	}
}
```

import 补上 `"errors"`。

- [ ] **Step 3: 运行测试确认失败**

```bash
go test ./internal/upstream/...
```

Expected: FAIL —— `undefined: ChatRequest`

- [ ] **Step 4: 实现聊天客户端**

创建 `internal/upstream/chat.go`：

```go
package upstream

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"mmxcode/internal/site"
)

// ChatRequest is one chat request to MiniMax.
//
// The body is the client's own, unchanged: MiniMax Code speaks the
// Anthropic and the OpenAI format natively, so nothing is rewritten
// on the way in.
type ChatRequest struct {
	// BaseURL is the host to talk to.
	BaseURL string
	// Path is the endpoint's path.
	Path string
	// Access is the account's access token.
	Access string
	// SessionID is the session the upstream should treat this as.
	SessionID string
	// TZOffset is the client's timezone offset in seconds.
	TZOffset int
	// Body is the request body.
	Body []byte
	// Stream says the answer is a stream.
	Stream bool
	// Anthropic says the Anthropic protocol is in use, so the version
	// header is filled in when the client did not send one.
	Anthropic bool
	// Extra holds headers the client set, copied over.
	Extra map[string]string
}

// Chat sends a chat request.
//
// transport may be nil, in which case the client's own is used. The
// account's exit is a property of the transport, so an account with
// its own proxy passes its own client here.
func (c *Client) Chat(ctx context.Context, req ChatRequest, transport http.RoundTripper) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(req.BaseURL, "/")+req.Path, strings.NewReader(string(req.Body)))
	if err != nil {
		return nil, err
	}
	for k, v := range req.Extra {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Access)
	// MiniMax Code gives its SDK a placeholder key; the real one is in
	// Authorization.
	httpReq.Header.Set("x-api-key", site.PlaceholderKey)
	httpReq.Header.Set("User-Agent", site.UserAgent)
	httpReq.Header.Set("X-Mavis-Agent-Id", site.AgentID)
	httpReq.Header.Set("X-Mavis-Timezone-Offset", strconv.Itoa(req.TZOffset))
	if req.SessionID != "" {
		httpReq.Header.Set("X-Mavis-Session-Id", req.SessionID)
	}
	if req.Anthropic && httpReq.Header.Get("anthropic-version") == "" {
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	}
	// 必须：不带压缩时 MiniMax 才会一个事件一个事件地吐
	httpReq.Header.Set("accept-encoding", "identity")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	// A nil transport means the client's own: the client was made with
	// the transport of the account's exit, and falling back to a fresh
	// default one would quietly drop the proxy.
	if transport == nil {
		transport = c.http.Transport
	}
	// A nil transport means the client's own: the client was made with
	// the transport of the account's exit, and falling back to a fresh
	// default one would quietly drop the proxy.
	if transport == nil {
		transport = c.http.Transport
	}
	// A nil transport means the client's own: the client was made with
	// the transport of the account's exit, and falling back to a fresh
	// default one would quietly drop the proxy.
	if transport == nil {
		transport = c.http.Transport
	}
	// A nil transport means the client's own: the client was made with
	// the transport of the account's exit, and falling back to a fresh
	// default one would quietly drop the proxy.
	if transport == nil {
		transport = c.http.Transport
	}
	resp, err := DoWith(httpReq, transport)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Models asks the site for its models.
//
// The token is used when there is one; the site also answers without.
func (c *Client) Models(ctx context.Context, baseURL, access string) ([]byte, error) {
	u := strings.TrimRight(baseURL, "/") + site.ModelsPath + "?" + site.ModelsQuery
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", site.UserAgent)
	httpReq.Header.Set("accept-encoding", "identity")
	if access != "" {
		httpReq.Header.Set("Authorization", "Bearer "+access)
	}
	resp, err := DoWith(httpReq, c.http.Transport)
	if err != nil {
		return nil, err
	}
	body, err := ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("models: HTTP %d", resp.StatusCode)
	}
	return body, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./internal/upstream/...
```

Expected: PASS

- [ ] **Step 6: 提交**

```bash
gofmt -w internal/upstream/
git add internal/upstream
git commit -m "feat: 聊天请求上游客户端

按账号出口走各自的 transport，强制 accept-encoding: identity。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: rotator 包

**Files:**
- Create: `internal/rotator/session.go`
- Create: `internal/rotator/decide.go`
- Create: `internal/rotator/rotator.go`
- Create: `internal/rotator/rotator_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/rotator/rotator_test.go`：

```go
package rotator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// fakePool is an account pool that does not touch a database.
type fakePool struct {
	accounts  []store.Account
	exhausted []store.Exhausted
	sessions  map[string]string
	bound     []string
	marked    []markedWindow
	remaining map[string]float64

	// sessionErr is what SessionAccount fails with.
	sessionErr error
	// bindErr is what BindSession fails with.
	bindErr error
	// remainingCalls counts the questions asked for one account's
	// credits.
	remainingCalls int
}

// markedWindow is one window the pool was told to mark spent.
type markedWindow struct {
	accountID string
	windowKey string
	until     time.Time
	reason    string
}

func newFakePool(accounts ...store.Account) *fakePool {
	return &fakePool{accounts: accounts, sessions: map[string]string{}}
}

func (p *fakePool) Accounts(context.Context) ([]store.Account, error) { return p.accounts, nil }

func (p *fakePool) Exhausted(context.Context) ([]store.Exhausted, error) { return p.exhausted, nil }

func (p *fakePool) BindSession(_ context.Context, key, accountID string) error {
	if p.bindErr != nil {
		return p.bindErr
	}
	p.sessions[key] = accountID
	p.bound = append(p.bound, key+"="+accountID)
	return nil
}

func (p *fakePool) SessionAccount(_ context.Context, key string) (string, bool, error) {
	if p.sessionErr != nil {
		return "", false, p.sessionErr
	}
	id, ok := p.sessions[key]
	return id, ok, nil
}

func (p *fakePool) Remaining(_ context.Context, accountID string) (float64, bool) {
	p.remainingCalls++
	left, ok := p.remaining[accountID]
	return left, ok
}

func (p *fakePool) MarkExhausted(_ context.Context, accountID, windowKey string, until time.Time, reason string) error {
	p.marked = append(p.marked, markedWindow{
		accountID: accountID, windowKey: windowKey, until: until, reason: reason,
	})
	p.exhausted = append(p.exhausted, store.Exhausted{
		AccountID: accountID, WindowKey: windowKey, Until: until, Reason: reason,
	})
	return nil
}

// batchPool is a pool that answers for a whole list of accounts at
// once.
type batchPool struct {
	*fakePool
	calls int
	ids   []string
}

func (p *batchPool) RemainingAll(_ context.Context, ids []string) map[string]Remaining {
	p.calls++
	p.ids = ids
	out := map[string]Remaining{}
	for _, id := range ids {
		if left, ok := p.remaining[id]; ok {
			out[id] = Remaining{Left: left, Known: true}
		}
	}
	return out
}

func acc(id string) store.Account {
	return store.Account{ID: id, Name: id, Status: store.StatusActive}
}

func TestPickBindsSessionToAnAccount(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})

	first, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != second.AccountID {
		t.Fatalf("the session moved: %s then %s", first.AccountID, second.AccountID)
	}
	if len(p.bound) != 1 {
		t.Fatalf("bound %v", p.bound)
	}
}

func TestPickSkipsExhaustedAccounts(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
}

func TestPickIgnoresExpiredExhaustion(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(-time.Minute), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_01" {
		t.Fatalf("picked %s", got.AccountID)
	}
}

func TestPickSkipsSignedOutAccounts(t *testing.T) {
	a := acc("acc_01")
	a.Status = store.StatusSignedOut
	p := newFakePool(a, acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
}

func TestPickFailsOverToAnotherAccount(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})

	first, _ := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	// 第一个账号在这个请求里已经试过了，重放时不能再选它
	used := map[string]bool{first.AccountID: true}
	second, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", used)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccountID == first.AccountID {
		t.Fatal("the account that was tried again was picked again")
	}
	// 换的只是这个请求的账号：会话还绑在原来那个上
	if p.sessions["sess-1"] != first.AccountID {
		t.Fatalf("the binding moved to %s", p.sessions["sess-1"])
	}
}

func TestPickKeepsTheBindingWhenTheBoundAccountWasAlreadyTried(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})

	// acc_01 已经试过了：换个账号回答这次请求，但不动绑定
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", map[string]bool{"acc_01": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_01" {
		t.Fatalf("the binding moved to %s", p.sessions["sess-1"])
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
	if got.Rebound {
		t.Fatal("the plan says the session was rebound")
	}
	if !got.Retried {
		t.Fatal("the plan does not say the request was answered by another account")
	}
}

func TestPickRebindsWhenTheBoundAccountIsExhausted(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_02" {
		t.Fatalf("the binding stayed on %s", p.sessions["sess-1"])
	}
	if !got.Rebound {
		t.Fatal("the plan does not say the session was rebound")
	}
}

func TestPickRebindsWhenTheBoundAccountIsSignedOut(t *testing.T) {
	a := acc("acc_01")
	a.Status = store.StatusSignedOut
	p := newFakePool(a, acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_02" {
		t.Fatalf("the binding stayed on %s", p.sessions["sess-1"])
	}
}

func TestPickRebindsWhenTheBoundAccountIsGone(t *testing.T) {
	p := newFakePool(acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.sessions["sess-1"] != "acc_02" {
		t.Fatalf("the binding stayed on %s", p.sessions["sess-1"])
	}
}

func TestPickFailsWhenTheSessionCannotBeRead(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	p.sessionErr = errors.New("the database is down")
	r := New(p, Options{MaxAttempts: 3})

	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err == nil {
		t.Fatal("expected an error")
	}
	// 读不到绑定不是"没有绑定"：原来那个不能被改写
	if p.sessions["sess-1"] != "acc_01" {
		t.Fatalf("the binding moved to %s", p.sessions["sess-1"])
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
}

func TestPickFailsWhenTheBindingCannotBeWritten(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.bindErr = errors.New("the database is down")
	r := New(p, Options{MaxAttempts: 3})

	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err == nil {
		t.Fatal("expected an error")
	}
	if len(p.bound) != 0 {
		t.Fatalf("bound %v", p.bound)
	}
}

func TestPickFailsWhenEveryAccountIsExhausted(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})
	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNoSessionRotates(t *testing.T) {
	// 没有会话就没有可粘的东西，退化为轮询
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})
	a, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("both requests went to %s", a.AccountID)
	}
}

func TestRoundRobinIgnoresTheBinding(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})
	a, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID == b.AccountID {
		t.Fatalf("both went to %s", a.AccountID)
	}
}

func TestRoundRobinWalksOverTheAccountsThatCannotAnswer(t *testing.T) {
	// acc_01 用过了，acc_02 退出了：轮询跳过它们
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	p.accounts[1].Status = store.StatusSignedOut
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})

	first, err := r.Pick(context.Background(), "", "MiniMax-M3", map[string]bool{"acc_01": true})
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID != "acc_03" {
		t.Fatalf("picked %s", first.AccountID)
	}
}

func TestRoundRobinTakesTheLeastRecentlyUsedAccount(t *testing.T) {
	// 轮询用的也是"最久没用过的先来"这条规则
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		got, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[got.AccountID] {
			t.Fatalf("%s was picked twice in three picks", got.AccountID)
		}
		seen[got.AccountID] = true
	}
}

func TestPrefersTheAccountWithCredits(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.remaining = map[string]float64{"acc_01": 10, "acc_02": 500}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s, want the one with credits", got.AccountID)
	}
}

func TestTheCreditsAreAskedForTheWholeListAtOnce(t *testing.T) {
	p := &batchPool{fakePool: newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))}
	p.remaining = map[string]float64{"acc_02": 500}
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s", got.AccountID)
	}
	if p.calls != 1 {
		t.Fatalf("the pool was asked %d times, want 1", p.calls)
	}
	if len(p.ids) != 3 {
		t.Fatalf("the pool was asked about %v", p.ids)
	}
}

func TestTheCreditsAreAskedForOneAccountAtATimeWithoutTheBatch(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	r := New(p, Options{MaxAttempts: 3})
	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err != nil {
		t.Fatal(err)
	}
	if p.remainingCalls != 2 {
		t.Fatalf("the pool was asked %d times, want 2", p.remainingCalls)
	}
}

func TestWindowKey(t *testing.T) {
	r := New(newFakePool(), Options{})
	if got := r.WindowKey("MiniMax-M3"); got != "general" {
		t.Fatalf("window = %q", got)
	}
	if got := r.WindowKey("MiniMax-Video-01"); got != "video" {
		t.Fatalf("window = %q", got)
	}
}

func TestGroupKey(t *testing.T) {
	// 键是计划窗口的那一组，不是一个窗口
	r := New(newFakePool(), Options{})
	if got := r.GroupKey("MiniMax-M3"); got != "general" {
		t.Fatalf("group = %q", got)
	}
	if got := r.GroupKey("MiniMax-Video-01"); got != "video" {
		t.Fatalf("group = %q", got)
	}
}

func TestTheExhaustionIsPerGroup(t *testing.T) {
	// general 这一组用完了，别的组不受影响
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.exhausted = []store.Exhausted{{
		AccountID: "acc_01", WindowKey: "general",
		Until: time.Now().Add(time.Hour), Reason: "quota",
	}}
	r := New(p, Options{MaxAttempts: 3})

	general, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if general.AccountID != "acc_02" {
		t.Fatalf("picked %s", general.AccountID)
	}
	video, err := r.Pick(context.Background(), "sess-2", "MiniMax-Video-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if video.AccountID != "acc_01" {
		t.Fatalf("picked %s", video.AccountID)
	}
}

func TestMarkExhaustedReachesThePool(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3, Cooldown: time.Minute})
	until := time.Now().Add(time.Hour).Truncate(time.Second)

	if err := r.MarkExhausted(context.Background(), "acc_01", "general", "quota", until); err != nil {
		t.Fatal(err)
	}
	if len(p.marked) != 1 {
		t.Fatalf("marked %v", p.marked)
	}
	m := p.marked[0]
	if m.accountID != "acc_01" {
		t.Fatalf("account = %q", m.accountID)
	}
	if m.windowKey != "general" {
		t.Fatalf("window = %q", m.windowKey)
	}
	if m.reason != "quota" {
		t.Fatalf("reason = %q", m.reason)
	}
	if !m.until.Equal(until) {
		t.Fatalf("until = %v, want %v", m.until, until)
	}
}

func TestMarkWindowExhaustedHasThePoolOrder(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3, Cooldown: time.Minute})
	until := time.Now().Add(time.Hour).Truncate(time.Second)

	if err := r.MarkWindowExhausted(context.Background(), "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	if len(p.marked) != 1 {
		t.Fatalf("marked %v", p.marked)
	}
	m := p.marked[0]
	if m.accountID != "acc_01" || m.windowKey != "general" {
		t.Fatalf("marked %+v", m)
	}
	if m.reason != "quota" {
		t.Fatalf("reason = %q", m.reason)
	}
	if !m.until.Equal(until) {
		t.Fatalf("until = %v, want %v", m.until, until)
	}
}

func TestMarkExhaustedUsesTheCooldownWhenThereIsNoTime(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3, Cooldown: 90 * time.Second})
	if err := r.MarkWindowExhausted(context.Background(), "acc_01", "general", time.Time{}, "quota"); err != nil {
		t.Fatal(err)
	}
	if len(p.marked) != 1 {
		t.Fatalf("marked %v", p.marked)
	}
	until := p.marked[0].until
	if until.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("until = %v, want the cooldown", until)
	}
}

func TestTheAttemptsAreCounted(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	r := New(p, Options{MaxAttempts: 2})

	used := map[string]bool{"acc_01": true}
	if _, err := r.Pick(context.Background(), "", "MiniMax-M3", used); err != nil {
		t.Fatalf("one account tried, one attempt left: %v", err)
	}
	used["acc_02"] = true
	if _, err := r.Pick(context.Background(), "", "MiniMax-M3", used); err == nil {
		t.Fatal("expected an error: the budget is used up")
	}
}

func TestTheStrategyIsChecked(t *testing.T) {
	if err := (Options{Strategy: StrategySticky}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Options{Strategy: StrategyRoundRobin}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Options{Strategy: "sticky-ish"}).Validate(); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := NewChecked(newFakePool(), Options{Strategy: "sticky-ish"}); err == nil {
		t.Fatal("expected an error")
	}
	// New 不会因为策略写错就失败：它退回粘性的那个
	if r := New(newFakePool(), Options{Strategy: "sticky-ish"}); r == nil {
		t.Fatal("no rotator")
	}
}
```


- [ ] **Step 2: 写会话键与判定的测试**

在同一个文件末尾追加：

```go

func TestSessionFromHeaders(t *testing.T) {
	h := map[string][]string{
		"X-Session-Id":      {"abc"},
		"X-Conversation-Id": {"def"},
	}
	s := SessionOf(h, "sys", "hello")
	if s.UpstreamID != "abc" {
		t.Fatalf("upstream id = %q", s.UpstreamID)
	}
	h2 := map[string][]string{"X-Conversation-Id": {"def"}}
	if got := SessionOf(h2, "sys", "hello"); got.UpstreamID != "def" {
		t.Fatalf("upstream id = %q", got.UpstreamID)
	}
}

func TestSessionKeyIsHashed(t *testing.T) {
	// 客户端自己那串字符串不进数据库键
	h := map[string][]string{"X-Session-Id": {"abc"}}
	s := SessionOf(h, "sys", "hello")
	if s.Key == "" {
		t.Fatal("no key")
	}
	if strings.Contains(s.Key, "abc") {
		t.Fatalf("key = %q, it holds the client's own string", s.Key)
	}
	if SessionOf(h, "sys", "hello").Key != s.Key {
		t.Fatal("the same conversation gave two keys")
	}
	other := SessionOf(map[string][]string{"X-Session-Id": {"def"}}, "sys", "hello")
	if other.Key == s.Key {
		t.Fatal("two conversations gave the same key")
	}
}

func TestSessionFromHash(t *testing.T) {
	a := SessionOf(nil, "sys", "hello")
	b := SessionOf(nil, "sys", "hello")
	if a.Key == "" {
		t.Fatal("expected a key")
	}
	if a.Key != b.Key {
		t.Fatal("the same conversation gave two keys")
	}
	c := SessionOf(nil, "sys", "other")
	if a.Key == c.Key {
		t.Fatal("two conversations gave the same key")
	}
	if a.UpstreamID == "" {
		t.Fatal("expected an upstream session id")
	}
}

func TestTwoIdenticalFirstMessagesShareTheUpstreamID(t *testing.T) {
	// 这是已知的碰撞：两个用户的第一条消息一样，就共用一个上游会话，
	// 上游分不出是两个会话。给每个会话一个随机的上游 id 需要 sessions
	// 表多一列，现在没有。
	a := SessionOf(nil, "sys", "hello")
	b := SessionOf(nil, "sys", "hello")
	if a.UpstreamID != b.UpstreamID {
		t.Fatal("the two conversations got different upstream ids")
	}
}

func TestSessionEmpty(t *testing.T) {
	s := SessionOf(nil, "", "")
	if s.Key != "" || s.UpstreamID != "" {
		t.Fatalf("session = %+v", s)
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		f    Failure
		want Action
	}{
		{"配额拒绝", Failure{Status: 429, Body: []byte(`{"message":"insufficient balance"}`)}, ActionFailover},
		{"403 配额", Failure{Status: 403, Body: []byte(`{"message":"额度不足"}`)}, ActionFailover},
		{"402 配额", Failure{Status: 402, Body: []byte(`{"message":"quota exceeded"}`)}, ActionFailover},
		{"429 限流", Failure{Status: 429, Body: []byte(`{"message":"too many requests"}`)}, ActionRetrySame},
		{"500", Failure{Status: 500}, ActionRetrySame},
		{"网络错误", Failure{Network: true}, ActionRetrySame},
		{"跨站重定向", Failure{OtherSite: true}, ActionGiveUp},
		{"401", Failure{Status: 401}, ActionRenew},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.f); got != c.want {
				t.Fatalf("Decide = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecideRenewsOnARefusedSignIn(t *testing.T) {
	cases := []struct {
		name string
		f    Failure
	}{
		{"调用方说的", Failure{Status: 200, Refused: true}},
		{"200 里的 1000048", Failure{Status: 200, Body: []byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`)}},
		{"base_resp 里的 1000048", Failure{Status: 200, Body: []byte(`{"base_resp":{"status_code":1000048,"status_msg":"the sign-in was refused"}}`)}},
		{"403 里的 1000048", Failure{Status: 403, Body: []byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.f); got != ActionRenew {
				t.Fatalf("Decide = %v, want %v", got, ActionRenew)
			}
		})
	}
}

func TestDecideIgnoresTheQuotaWordsOutsideTheMessage(t *testing.T) {
	// 身体里有 "credit" 和 "limit"，但那不是上游说的拒绝，
	// 不能因此把这个账号的窗口记成用完。
	f := Failure{Status: 429, Body: []byte(`{"message":"the request failed","detail":{"credit":5,"limit":100}}`)}
	if got := Decide(f); got != ActionRetrySame {
		t.Fatalf("Decide = %v, want %v", got, ActionRetrySame)
	}
}

func TestDecideReadsTheQuotaWordsFromTheNestedMessage(t *testing.T) {
	f := Failure{Status: 429, Body: []byte(`{"error":{"message":"insufficient balance"}}`)}
	if got := Decide(f); got != ActionFailover {
		t.Fatalf("Decide = %v, want %v", got, ActionFailover)
	}
}

func TestDecideUsesTheMessageTheCallerParsed(t *testing.T) {
	f := Failure{Status: 429, Message: "insufficient balance"}
	if got := Decide(f); got != ActionFailover {
		t.Fatalf("Decide = %v, want %v", got, ActionFailover)
	}
}
```


- [ ] **Step 3: 运行测试确认失败**

```bash
go test ./internal/rotator/...
```

Expected: FAIL —— `undefined: New`、`undefined: SessionKey`、`undefined: Decide`

- [ ] **Step 4: 实现会话键**

创建 `internal/rotator/session.go`：

```go
package rotator

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// The headers a client can name its session with, in the order they
// are looked for.
var sessionHeaders = []string{
	"X-Session-Id",
	"X-Conversation-Id",
	"X-Session-Id-Codex",
	"Session-Id",
}

// Session is what a request's session is made of.
//
// The two things it holds are different on purpose. The key says which
// account the conversation belongs to, and it is ours alone. The
// upstream id says which conversation it is, and MiniMax sees it.
type Session struct {
	// Key is the account affinity key. Always a hash.
	Key string
	// UpstreamID is the session id sent to MiniMax.
	UpstreamID string
}

// SessionOf reads the session out of a request.
//
// The client's own session id is used when it has one. Otherwise the
// session is a hash of the system prompt and the first user message:
// the first message of a conversation does not change while the
// conversation goes on, so the hash holds the conversation to one
// account.
//
// The key is a hash either way. A client's own string is not ours to
// keep, and it has no business becoming a database key as it came.
//
// The upstream id is the client's own when it sent one: it names the
// conversation, not the account, so it stays the same when the
// conversation moves to another account.
//
// When there is neither a header nor a message to hash, the session is
// empty and the request is handled by round robin.
//
// The upstream id of a conversation that brought no header of its own
// is the hash of its first message. Two users whose first messages
// happen to be the same therefore share one upstream id, and one
// account: the upstream sees one conversation as two, and one user's
// messages can turn up in another user's conversation. A random id per
// conversation would not, but the id has to be kept from one turn to
// the next and there is nowhere to keep it: the sessions table holds
// the key and the account and nothing else. It needs a column of its
// own before this can be done.
func SessionOf(headers map[string][]string, system, firstUser string) Session {
	for _, h := range sessionHeaders {
		for _, v := range headers[h] {
			if s := strings.TrimSpace(v); s != "" {
				return Session{Key: "h:" + digest("hdr:"+s), UpstreamID: s}
			}
		}
	}
	if strings.TrimSpace(system) == "" && strings.TrimSpace(firstUser) == "" {
		return Session{}
	}
	d := digest(system + "\x00" + firstUser)
	return Session{Key: "m:" + d, UpstreamID: d}
}

// digest is the short hash a session key is made of.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}
```


- [ ] **Step 5: 实现故障转移判定**

创建 `internal/rotator/decide.go`：

```go
package rotator

import (
	"encoding/json"
	"errors"
	"strings"

	"mmxcode/internal/upstream"
)

// Action is what to do about a failed upstream answer.
type Action int

// The actions.
const (
	// ActionRetrySame waits a little and asks the same account again.
	ActionRetrySame Action = iota
	// ActionFailover marks the account's window spent and asks another
	// account.
	ActionFailover
	// ActionRenew renews the token and asks the same account again.
	ActionRenew
	// ActionGiveUp gives up: the answer goes to the client as it is.
	ActionGiveUp
)

// String is the action's name.
func (a Action) String() string {
	switch a {
	case ActionRetrySame:
		return "retry-same"
	case ActionFailover:
		return "failover"
	case ActionRenew:
		return "renew"
	default:
		return "give-up"
	}
}

// Failure is what went wrong upstream.
type Failure struct {
	// Status is the HTTP status, when there was one.
	Status int
	// Body is the answer's body.
	Body []byte
	// Network says the request never got an answer.
	Network bool
	// OtherSite says the request was redirected to the other site.
	OtherSite bool
	// Refused says the sign-in was refused: the token is stale and
	// the account itself is fine. It is what upstream.ErrSignInRefused
	// says.
	//
	// When it is not set, the body is read to find out: MiniMax's own
	// code 1000048 in the body of a 200 means the same thing.
	Refused bool
	// Message is the message the upstream's business error carried.
	//
	// The words that say "the account is spent" are looked for in it
	// and not in the raw body: a body that says anything about a
	// limit is not a refusal to spend. When it is empty the body is
	// read to find the message.
	Message string
	// Code is the business code the answer carried.
	//
	// When it is zero the body is read to find the code.
	Code int
}

// Decide says what to do about a failure.
//
// The rules come from what the site actually does:
//
//   - a refused sign-in means the token is stale: it is renewed first,
//     and the same account is asked again;
//   - a refusal that names the credits or the quota means the account
//     is spent: another account is tried;
//   - a 429 that does not name the credits is a rate limit, and the
//     same account is asked again after a pause;
//   - a 5xx or a network error is the same: the same account, again;
//   - a 401 means the token is stale: it is renewed first;
//   - a redirect to the other site is neither of these: the account is
//     fine and the network is not, so nothing is changed and no
//     refresh token is spent.
//
// The words that say the account is spent are looked for in the
// upstream's own message and not in the raw body: a body that says
// anything about a limit is not a refusal to spend, and marking the
// account's window spent on one is a mistake that lasts until the
// window comes back.
//
// Decide reads the body when the caller has not filled in the
// message, the code or the refusal. It does no I/O: the body is read,
// not the network.
func Decide(f Failure) Action {
	if f.OtherSite {
		return ActionGiveUp
	}
	if f.Network {
		return ActionRetrySame
	}
	_, msg, refused := f.business()
	if f.Refused || refused {
		// 签到被拒：令牌旧了，续签之后还是这个账号。
		return ActionRenew
	}
	if f.Status == 401 {
		return ActionRenew
	}
	spent := isQuotaMessage(msg)
	switch f.Status {
	case 200:
		// 一个 200 里装着业务错误。只有确认了那是错误才会走到
		// 这里，所以按错误来判。
		if spent {
			return ActionFailover
		}
		return ActionGiveUp
	case 402, 403, 429:
		if spent {
			return ActionFailover
		}
		return ActionRetrySame
	}
	if f.Status >= 500 {
		return ActionRetrySame
	}
	return ActionGiveUp
}

// business gives the business error the answer carried: its code, its
// message, and whether the sign-in was refused.
//
// What the caller filled in comes first; the body is read when it is
// not there, so that a caller that has the parsed error does not have
// to hand it over twice.
func (f Failure) business() (code int, message string, refused bool) {
	code, message, refused = f.Code, f.Message, f.Refused
	if code != 0 && message != "" {
		return code, message, refused
	}
	if len(f.Body) == 0 {
		return code, message, refused
	}
	c, m, r := parseBusiness(f.Body)
	if code == 0 {
		code = c
	}
	if message == "" {
		message = m
	}
	if !refused {
		refused = r
	}
	return code, message, refused
}

// parseBusiness reads the business error out of a body.
//
// MiniMax's own shape is read first: statusInfo and base_resp carry
// the code and the message. A body that carries a plain message is
// read next, for the shape the site uses when it answers with a plain
// error.
//
// A body that says nothing at all — not JSON, or JSON without a
// message — says nothing here either: there is no message to read,
// and the words are not looked for in the raw bytes.
func parseBusiness(body []byte) (code int, message string, refused bool) {
	if err := upstream.CheckEnvelope("chat", body); err != nil {
		var be *upstream.BusinessError
		if errors.As(err, &be) {
			return be.Code, be.Message, errors.Is(err, upstream.ErrSignInRefused)
		}
	}
	return 0, plainMessage(body), false
}

// plainMessage reads the message out of a body that is not in
// MiniMax's own shape.
//
// The message is looked for at the top level and inside an error
// object, which are the shapes the site uses. A field that is not a
// string says nothing.
func plainMessage(body []byte) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	if msg := messageOf(obj); msg != "" {
		return msg
	}
	for _, key := range []string{"error", "err"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var sub map[string]json.RawMessage
		if json.Unmarshal(raw, &sub) != nil {
			continue
		}
		if msg := messageOf(sub); msg != "" {
			return msg
		}
	}
	return ""
}

// messageOf reads the message out of a JSON object.
func messageOf(obj map[string]json.RawMessage) string {
	for _, key := range []string{"message", "msg"} {
		if s := rawString(obj[key]); s != "" {
			return s
		}
	}
	return ""
}

// rawString reads a JSON string. A field that is not a string says
// nothing.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// isQuotaMessage says whether the upstream's message is a refusal for
// running out of credits or plan.
//
// A rate limit is not: the account is fine and the answer comes when
// the caller slows down.
func isQuotaMessage(message string) bool {
	if strings.TrimSpace(message) == "" {
		return false
	}
	return upstream.IsQuotaRefusal([]byte(message))
}
```


- [ ] **Step 6: 实现选号**

创建 `internal/rotator/rotator.go`：

```go
// Package rotator picks the account a request goes to, and says what
// to do when the account cannot answer.
//
// A conversation is bound to one account and stays there. The binding
// is by session key: the client's own session id when it has one, and
// otherwise a hash of the system prompt and the first user message.
// When the bound account is spent or gone, another is picked and the
// conversation is rebound.
//
// A binding is moved only when the bound account cannot answer at all:
// it is spent, it is signed out, or it is gone. An account that has
// already been tried in this request and failed is another thing: a
// different account answers that one request, and the conversation
// stays where it is. One 5xx does not move a conversation to another
// account for good.
//
// The exhaustion is per account and per plan group. The site's plan
// windows ("5 hours", "7 days") belong to a group, and the models of a
// group spend from the same windows: a group that is spent answers
// none of its models until the window comes back, and a model of
// another group is not touched by it.
//
// The upstream conversation id of a conversation that brought no
// session id of its own is derived from the content of its first
// message. Two users whose first messages happen to be the same
// therefore share one upstream conversation id, and one account: the
// upstream sees one conversation as two. The sessions table has no
// column for an upstream id of its own, so a random one cannot be
// kept; giving every conversation an id of its own needs that column.
package rotator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/store"
)

// Pool is the account pool the rotator picks from.
type Pool interface {
	// Accounts gives every account.
	Accounts(ctx context.Context) ([]store.Account, error)
	// Exhausted gives the windows that are spent.
	Exhausted(ctx context.Context) ([]store.Exhausted, error)
	// BindSession points a session at an account.
	BindSession(ctx context.Context, sessionKey, accountID string) error
	// SessionAccount gives the account a session is bound to.
	SessionAccount(ctx context.Context, sessionKey string) (string, bool, error)
	// MarkExhausted marks an account's window spent.
	//
	// The order is the account, the window key, the time it comes
	// back and the reason. It is the order of
	// Rotator.MarkWindowExhausted.
	MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
	// Remaining gives an account's credits left. The second value is
	// false when the credits have not been read yet.
	Remaining(ctx context.Context, accountID string) (float64, bool)
}

// RemainingFor is an optional part of a Pool: it gives the credits
// left for a whole list of accounts at once.
//
// A pool that has it is asked for all the accounts in one question, so
// a pick costs one question and not one per account. A pool without
// it is asked account by account.
//
// The map holds only the accounts whose credits are known: an account
// that is not in it has not been read yet.
type RemainingFor interface {
	RemainingAll(ctx context.Context, accountIDs []string) map[string]Remaining
}

// Remaining is what is left of an account's credits.
type Remaining struct {
	// Left is how many credits are left.
	Left float64
	// Known says the credits have been read. When it is false, Left
	// says nothing.
	Known bool
}

// The strategies.
const (
	// StrategySticky keeps a conversation on the account it was bound
	// to, and only rotates when there is no conversation to keep.
	StrategySticky = "sticky"
	// StrategyRoundRobin rotates every request, and the binding is
	// not used.
	StrategyRoundRobin = "round-robin"
)

// Options configures a rotator.
type Options struct {
	// MaxAttempts is how many accounts one request may be tried on.
	//
	// Pick counts the accounts the caller has already tried and gives
	// up once there are as many as this, so the budget is the
	// rotator's own and not only the caller's.
	MaxAttempts int
	// Cooldown is how long an account is given up on when the spent
	// window cannot be told apart.
	Cooldown time.Duration
	// WindowGroup says which plan group a model spends from. It
	// defaults to the chat models' group.
	WindowGroup func(model string) string
	// Strategy says how an account is picked. The values are
	// StrategySticky and StrategyRoundRobin.
	//
	// An empty strategy is StrategySticky. Any other value is taken
	// for StrategySticky as well, which is the safe answer but hides
	// the mistake: Validate is where the mistake is seen.
	Strategy string
}

// Validate says whether the options make sense.
//
// A strategy that is neither StrategySticky nor StrategyRoundRobin is
// taken for StrategySticky, so a misspelled strategy does not fail
// loudly on its own. This is where it is caught.
func (o Options) Validate() error {
	switch o.Strategy {
	case "", StrategySticky, StrategyRoundRobin:
		return nil
	}
	return fmt.Errorf("unknown rotation strategy %q: it is %q or %q", o.Strategy, StrategySticky, StrategyRoundRobin)
}

// Plan is the account a request goes to.
type Plan struct {
	// AccountID is the account.
	AccountID string
	// WindowKey is the plan group the request spends.
	//
	// Deprecated: the key is the group of plan windows, not one
	// window. GroupKey says the same thing and is the name to use.
	WindowKey string
	// GroupKey is the plan group the request spends. It is the same
	// key as WindowKey.
	GroupKey string
	// Rebound says the session was moved to another account.
	Rebound bool
	// Retried says another account answered this request because the
	// bound one had already been tried, and the session stayed where
	// it was.
	Retried bool
}

// Rotator picks accounts.
type Rotator struct {
	pool Pool
	opts Options

	// mu guards the two fields below.
	mu sync.Mutex
	// cursor is where the rotation stands, so two accounts that are
	// equally good are not always taken in the same order.
	cursor int
	// lastUsed says when each account was last handed out, for the
	// "least recently used first" rule.
	lastUsed map[string]time.Time
}

// New makes a rotator.
//
// A strategy that is not one of the known ones is taken for
// StrategySticky. NewChecked is where that is caught.
func New(pool Pool, opts Options) *Rotator {
	r, err := NewChecked(pool, opts)
	if err != nil {
		// The strategy is not one of the known ones. The safe answer is
		// the sticky one; NewChecked is where the mistake is caught.
		opts.Strategy = StrategySticky
		r = newRotator(pool, opts)
	}
	return r
}

// NewChecked makes a rotator and says whether the options make sense.
//
// It is New with the mistake in the options told about instead of
// hidden.
func NewChecked(pool Pool, opts Options) (*Rotator, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return newRotator(pool, opts), nil
}

// newRotator makes a rotator with the options filled in.
func newRotator(pool Pool, opts Options) *Rotator {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = 5 * time.Minute
	}
	if opts.WindowGroup == nil {
		opts.WindowGroup = defaultWindowGroup
	}
	if opts.Strategy == "" {
		opts.Strategy = StrategySticky
	}
	return &Rotator{
		pool:     pool,
		opts:     opts,
		lastUsed: map[string]time.Time{},
	}
}

// defaultWindowGroup is the chat models' group.
//
// It is the same rule as manager.WindowGroup, and the two have to be
// kept in step: the manager is the one that reads the site's plan, and
// the key here has to be the one the manager writes down.
func defaultWindowGroup(model string) string {
	if strings.Contains(strings.ToLower(model), "video") {
		return "video"
	}
	return "general"
}

// GroupKey gives the plan group a model spends from.
//
// The key is the group of plan windows and not a single window: the
// windows "5 hours" and "7 days" belong to a group, and the models of
// the group spend from the same windows.
func (r *Rotator) GroupKey(model string) string {
	return r.opts.WindowGroup(model)
}

// WindowKey gives the plan window a model spends from.
//
// Deprecated: the key is the plan group, not one window. Use GroupKey.
func (r *Rotator) WindowKey(model string) string {
	return r.GroupKey(model)
}

// Pick gives the account a request goes to.
//
// used holds the accounts already tried for this request, so a replay
// does not pick the same one again.
//
// Which account it is comes down to these cases:
//
//   - a conversation that is bound to an account stays on it, as long as
//     the account can still answer;
//   - a conversation bound to an account that has already been tried in
//     this request is answered by another account, and the binding is
//     left alone: one failure does not move the conversation;
//   - a conversation with no binding takes the account with the most
//     credits left, and the least recently used one when those are
//     equal, and is bound to it;
//   - a request with no conversation at all rotates: the next account
//     in turn, so the load is spread.
//
// The "round-robin" strategy skips the first three cases and rotates
// always.
func (r *Rotator) Pick(ctx context.Context, sessionKey, model string, used map[string]bool) (Plan, error) {
	// The budget of attempts is the rotator's: a request is tried on
	// as many accounts as Options.MaxAttempts says, and not one more.
	if r.opts.MaxAttempts > 0 && len(used) >= r.opts.MaxAttempts {
		return Plan{}, fmt.Errorf("the %d attempts are used up", r.opts.MaxAttempts)
	}

	accounts, err := r.pool.Accounts(ctx)
	if err != nil {
		return Plan{}, err
	}
	exhausted, err := r.pool.Exhausted(ctx)
	if err != nil {
		return Plan{}, err
	}
	group := r.GroupKey(model)

	// The accounts that can answer: they are signed in, their group
	// is not spent, and they have not been tried yet.
	usable := make([]store.Account, 0, len(accounts))
	usableIDs := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			continue
		}
		if used[a.ID] || r.isExhausted(exhausted, a.ID, group) {
			continue
		}
		usable = append(usable, a)
		usableIDs[a.ID] = true
	}
	if len(usable) == 0 {
		return Plan{}, fmt.Errorf("no account can answer: every account is spent for the %s group or has already been tried", group)
	}

	sticky := r.opts.Strategy != StrategyRoundRobin

	if sticky && sessionKey != "" {
		boundID, ok, err := r.pool.SessionAccount(ctx, sessionKey)
		if err != nil {
			// A read error is not an unbound session. Were it taken
			// for one, the binding that is there would be overwritten.
			return Plan{}, fmt.Errorf("the session binding could not be read: %w", err)
		}
		if ok {
			bound, found := accountByID(accounts, boundID)
			switch {
			case !found, bound.Status != store.StatusActive:
				// The account is gone or signed out: the binding may
				// be replaced.
			case r.isExhausted(exhausted, boundID, group):
				// The account's group is spent: the binding may be
				// replaced.
			case used[boundID]:
				// The bound account has already been tried in this
				// request and failed. Another account answers this
				// request, and the conversation stays where it is.
				a, err := r.bestAccount(ctx, usable)
				if err != nil {
					return Plan{}, err
				}
				r.markUsed(a.ID)
				return r.plan(a, group, false, true), nil
			default:
				// The bound account can answer: it answers.
				r.markUsed(boundID)
				return r.plan(store.Account{ID: boundID}, group, false, false), nil
			}
		}
	}

	// No binding, or the bound account cannot answer: another account
	// is picked and the conversation is bound to it.
	if sticky && sessionKey != "" {
		a, err := r.bestAccount(ctx, usable)
		if err != nil {
			return Plan{}, err
		}
		if err := r.pool.BindSession(ctx, sessionKey, a.ID); err != nil {
			// The binding was not written. The caller must not be
			// told the conversation has moved: it has not.
			return Plan{}, fmt.Errorf("the session binding could not be written: %w", err)
		}
		r.markUsed(a.ID)
		return r.plan(a, group, true, false), nil
	}

	// No conversation, or the round-robin strategy: the next account
	// in the rotation.
	a, err := r.nextAccount(accounts, usableIDs)
	if err != nil {
		return Plan{}, err
	}
	r.markUsed(a.ID)
	return r.plan(a, group, false, false), nil
}

// plan gives the plan of a request that goes to an account.
func (r *Rotator) plan(a store.Account, group string, rebound, retried bool) Plan {
	return Plan{
		AccountID: a.ID,
		WindowKey: group,
		GroupKey:  group,
		Rebound:   rebound,
		Retried:   retried,
	}
}

// accountByID finds an account in a list.
func accountByID(accounts []store.Account, id string) (store.Account, bool) {
	for _, a := range accounts {
		if a.ID == id {
			return a, true
		}
	}
	return store.Account{}, false
}

// bestAccount gives the account with the most credits left, and the
// least recently used one when two have the same.
//
// An account whose credits have not been read is taken after the ones
// whose credits are known: a known balance is the better guide, and
// the account is still tried, unlike a spent one.
func (r *Rotator) bestAccount(ctx context.Context, usable []store.Account) (store.Account, error) {
	left := r.remaining(ctx, usable)
	var best *scored
	for _, a := range usable {
		rem := left[a.ID]
		s := scored{acc: a, known: rem.Known, left: rem.Left, usedAt: r.usedAt(a.ID)}
		if best == nil {
			best = &s
			continue
		}
		if better(s, *best) {
			best = &s
		}
	}
	if best == nil {
		return store.Account{}, fmt.Errorf("no account is usable")
	}
	return best.acc, nil
}

// remaining gives the credits left of every account in the list.
//
// A pool that can answer for the whole list in one go is asked to; a
// pool that cannot is asked account by account.
func (r *Rotator) remaining(ctx context.Context, accounts []store.Account) map[string]Remaining {
	out := make(map[string]Remaining, len(accounts))
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	if batch, ok := r.pool.(RemainingFor); ok {
		for id, rem := range batch.RemainingAll(ctx, ids) {
			out[id] = rem
		}
		return out
	}
	for _, id := range ids {
		left, known := r.pool.Remaining(ctx, id)
		out[id] = Remaining{Left: left, Known: known}
	}
	return out
}

// scored is an account with the numbers the choice is made on.
type scored struct {
	acc    store.Account
	known  bool
	left   float64
	usedAt time.Time
}

// better says one account is a better pick than another.
func better(a, b scored) bool {
	if a.known != b.known {
		// 读到余额的排前面：那是能拿来比较的数字
		return a.known
	}
	if a.known && a.left != b.left {
		return a.left > b.left
	}
	// 一样就用最久没用的那个
	return a.usedAt.Before(b.usedAt)
}

// nextAccount gives the next account in the rotation.
//
// The rule is the one the other path uses as well: the account that
// was handed out least recently comes next. An account that has never
// been handed out has never been used, so it comes before one that
// has.
//
// When several accounts share the oldest moment, the one the cursor
// points at is taken, so the rotation keeps moving.
func (r *Rotator) nextAccount(all []store.Account, usable map[string]bool) (store.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var best store.Account
	var bestAt time.Time
	var candidates []int
	found := false
	for i, a := range all {
		if !usable[a.ID] {
			continue
		}
		at := r.lastUsed[a.ID]
		switch {
		case !found, at.Before(bestAt):
			best, bestAt, found = a, at, true
			candidates = []int{i}
		case at.Equal(bestAt):
			candidates = append(candidates, i)
		}
	}
	if !found {
		return store.Account{}, fmt.Errorf("no account can answer")
	}

	idx := candidates[0]
	if len(candidates) > 1 && len(all) > 0 {
		start := r.cursor % len(all)
		for _, c := range candidates {
			if c >= start {
				idx = c
				break
			}
		}
	}
	r.cursor = idx + 1
	return best, nil
}

// markUsed records that an account was handed out.
func (r *Rotator) markUsed(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastUsed[accountID] = time.Now()
}

// usedAt says when an account was last handed out.
func (r *Rotator) usedAt(accountID string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastUsed[accountID]
}

// MarkExhausted marks an account's group spent.
//
// The arguments are the account, the group, the reason and the time
// the group comes back.
//
// Deprecated: the last two are the other way round from the Pool's
// MarkExhausted, and the two are easy to mix up. MarkWindowExhausted
// has the Pool's order.
func (r *Rotator) MarkExhausted(ctx context.Context, accountID, windowKey, reason string, until time.Time) error {
	return r.MarkWindowExhausted(ctx, accountID, windowKey, until, reason)
}

// MarkWindowExhausted marks an account's group spent.
//
// The arguments are the account, the group, the time the group comes
// back and the reason: the same order as the Pool's MarkExhausted.
//
// A time of zero means the group comes back after the cooldown.
func (r *Rotator) MarkWindowExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	if until.IsZero() {
		until = time.Now().Add(r.opts.Cooldown)
	}
	return r.pool.MarkExhausted(ctx, accountID, windowKey, until, reason)
}

// isExhausted says an account's group is spent.
func (r *Rotator) isExhausted(exhausted []store.Exhausted, accountID, windowKey string) bool {
	now := time.Now()
	for _, e := range exhausted {
		if e.AccountID != accountID {
			continue
		}
		if !e.Until.After(now) {
			continue
		}
		// 账号级耗尽（窗口键为空）对所有窗口都算
		if e.WindowKey == "" || e.WindowKey == windowKey {
			return true
		}
	}
	return false
}
```


- [ ] **Step 7: 运行测试确认通过**

```bash
go test ./internal/rotator/...
```

Expected: PASS

- [ ] **Step 8: 提交**

```bash
gofmt -w internal/rotator/
git add internal/rotator
git commit -m "feat: rotator 包

会话粘性绑定、按窗口的耗尽判定、故障转移判定。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: Codex 协议转换

**Files:**
- Create: `internal/convert/requests.go`
- Create: `internal/convert/stream.go`
- Create: `internal/convert/convert_test.go`

Codex 用的是 Responses 协议。上游可能原生支持，也可能不支持；不支持时本地转成 Anthropic 的 `/messages` 请求，再把上游的 SSE 转回 Responses 的事件。

- [ ] **Step 1: 写失败的测试**

创建 `internal/convert/convert_test.go`：

```go
package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToAnthropicPlainMessage(t *testing.T) {
	in := []byte(`{
		"model":"MiniMax-M3",
		"instructions":"you are helpful",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"max_output_tokens":100
	}`)
	out, err := ToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "MiniMax-M3" {
		t.Fatalf("model = %v", m["model"])
	}
	if m["system"] != "you are helpful" {
		t.Fatalf("system = %v", m["system"])
	}
	if m["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens = %v", m["max_tokens"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("role = %v", msg["role"])
	}
	content := msg["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi" {
		t.Fatalf("block = %v", block)
	}
}

func TestToAnthropicStringInput(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	msg := msgs[0].(map[string]any)
	block := msg["content"].([]any)[0].(map[string]any)
	if block["text"] != "hello" {
		t.Fatalf("block = %v", block)
	}
}

func TestToAnthropicToolCalls(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		],
		"tools":[{"type":"function","name":"get_weather","description":"the weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`)
	out, err := ToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	// 第二条是 assistant 的 tool_use
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("role = %v", assistant["role"])
	}
	block := assistant["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "get_weather" {
		t.Fatalf("block = %v", block)
	}
	if block["id"] != "call_1" {
		t.Fatalf("id = %v", block["id"])
	}
	// 第三条是 user 的 tool_result
	user := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("role = %v", user["role"])
	}
	rblock := user["content"].([]any)[0].(map[string]any)
	if rblock["type"] != "tool_result" || rblock["tool_use_id"] != "call_1" {
		t.Fatalf("block = %v", rblock)
	}
	if rblock["content"] != "sunny" {
		t.Fatalf("content = %v", rblock["content"])
	}
	// 工具的形状换了
	tools := m["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Fatalf("tool = %v", tool)
	}
	if tool["input_schema"] == nil {
		t.Fatalf("tool has no input_schema: %v", tool)
	}
	if _, ok := tool["parameters"]; ok {
		t.Fatalf("the tool kept its Responses shape: %v", tool)
	}
}

func TestToAnthropicKeepsStream(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","input":"x","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("out = %s", out)
	}
}

func TestToAnthropicRejectsBadJSON(t *testing.T) {
	if _, err := ToAnthropic([]byte(`not json`)); err == nil {
		t.Fatal("expected an error")
	}
}
```

再追加 SSE 转换的测试：

```go
// anthropicSSE is a fixed sample of the upstream's stream.
const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestStreamProducesResponsesEvents(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "MiniMax-M3"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.completed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the stream has no %s:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `"delta":"Hello"`) {
		t.Fatalf("the text delta is missing:\n%s", got)
	}
	if !strings.Contains(got, `"delta":"let me think"`) {
		t.Fatalf("the thinking delta is missing:\n%s", got)
	}
}

func TestStreamProducesFunctionCallEvents(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		`"type":"function_call"`,
		`"name":"get_weather"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the stream has no %s:\n%s", want, got)
		}
	}
}

func TestStreamEmpty(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(""), &out, "m"); err != nil {
		t.Fatal(err)
	}
	if out.String() == "" {
		t.Fatal("expected at least the created event")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/convert/...
```

Expected: FAIL —— `undefined: ToAnthropic`

- [ ] **Step 3: 实现请求转换**

创建 `internal/convert/requests.go`：

```go
// Package convert turns the Codex Responses protocol into what
// MiniMax speaks, and the answer back.
//
// MiniMax Code speaks the Anthropic Messages protocol natively. The
// Responses protocol is turned into it here: the request is rewritten,
// and the upstream's stream is rewritten back into Responses events.
package convert

import (
	"encoding/json"
	"fmt"
)

// responsesRequest is the part of a Responses request that is read.
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
	Tools           []responsesTool `json:"tools"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

// responsesTool is one tool of a Responses request.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// anthropicRequest is the Anthropic Messages request that comes out.
type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Thinking  *thinking          `json:"thinking,omitempty"`
}

// anthropicMessage is one message.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one block of a message.
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

// anthropicTool is one tool of an Anthropic request.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// thinking is the Anthropic thinking setting.
type thinking struct {
	Type string `json:"type"`
}

// ToAnthropic turns a Responses request into an Anthropic Messages
// request.
//
// The model, the messages, the tools and the streaming flag all come
// across. The Responses protocol's tool calls and tool results become
// Anthropic's tool_use and tool_result blocks.
func ToAnthropic(body []byte) ([]byte, error) {
	var in responsesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("responses: %w", err)
	}
	out := anthropicRequest{
		Model:     in.Model,
		System:    in.Instructions,
		MaxTokens: in.MaxOutputTokens,
		Stream:    in.Stream,
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = 4096
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	if in.Reasoning != nil && in.Reasoning.Effort != "" {
		out.Thinking = &thinking{Type: "adaptive"}
	}
	msgs, err := messages(in.Input)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	return json.Marshal(out)
}

// messages turns the Responses input into Anthropic messages.
func messages(raw json.RawMessage) ([]anthropicMessage, error) {
	if len(raw) == 0 {
		return []anthropicMessage{}, nil
	}
	// 一个字符串就是一条 user 消息
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []anthropicMessage{{
			Role:    "user",
			Content: []anthropicBlock{{Type: "text", Text: s}},
		}}, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses: input is neither a string nor a list")
	}
	var out []anthropicMessage
	for _, item := range items {
		typ := stringField(item["type"])
		switch typ {
		case "function_call":
			var fc struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(mustMarshal(item), &fc); err != nil {
				return nil, err
			}
			input := json.RawMessage(fc.Arguments)
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, anthropicMessage{
				Role: "assistant",
				Content: []anthropicBlock{{
					Type: "tool_use", ID: fc.CallID, Name: fc.Name, Input: input,
				}},
			})
		case "function_call_output":
			var fo struct {
				CallID string `json:"call_id"`
				Output string `json:"output"`
			}
			if err := json.Unmarshal(mustMarshal(item), &fo); err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{
				Role: "user",
				Content: []anthropicBlock{{
					Type: "tool_result", ToolUseID: fo.CallID, Content: fo.Output,
				}},
			})
		case "message", "":
			var mi struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(mustMarshal(item), &mi); err != nil {
				return nil, err
			}
			role := mi.Role
			if role == "" {
				role = "user"
			}
			if role == "system" {
				role = "user"
			}
			blocks, err := blocks(mi.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{Role: role, Content: blocks})
		default:
			// 别的类型不认，跳过
		}
	}
	if len(out) == 0 {
		out = []anthropicMessage{{
			Role:    "user",
			Content: []anthropicBlock{{Type: "text", Text: ""}},
		}}
	}
	return out, nil
}

// blocks turns a Responses message's content into Anthropic blocks.
func blocks(raw json.RawMessage) ([]anthropicBlock, error) {
	if len(raw) == 0 {
		return []anthropicBlock{{Type: "text", Text: ""}}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("responses: content is neither a string nor a list")
	}
	out := make([]anthropicBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "":
			out = append(out, anthropicBlock{Type: "text", Text: p.Text})
		default:
			// 图片之类的原样丢掉：Anthropic 的图片块形状不同，
			// 这里只保证纯文本的对话能用。
			out = append(out, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
	if len(out) == 0 {
		out = []anthropicBlock{{Type: "text", Text: ""}}
	}
	return out, nil
}

func stringField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
```

- [ ] **Step 4: 实现流转换**

创建 `internal/convert/stream.go`：

```go
package convert

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// anthropicEvent is one event of the upstream's stream.
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// blockState is what is known about one upstream content block.
type blockState struct {
	// outputIndex is the item's index in the Responses output.
	outputIndex int
	// kind is "reasoning", "message" or "function_call".
	kind string
	// itemID is the Responses item id.
	itemID string
	// callID is the tool call id, for a function call.
	callID string
	// name is the tool's name, for a function call.
	name string
	// text is what has been said so far.
	text string
	// args is the tool arguments so far.
	args string
}

// StreamResponses turns the upstream's Anthropic stream into a
// Responses stream.
//
// The events are written as they come, so a client that is waiting for
// the first token gets it as soon as the upstream sends it.
func StreamResponses(r io.Reader, w io.Writer, model string) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	blocks := map[int]*blockState{}
	nextIndex := 0
	responseID := "resp_" + model
	var text strings.Builder
	var reasoning strings.Builder
	stopReason := ""
	inputTokens, outputTokens := 0, 0

	emit := func(event string, payload map[string]any) error {
		payload["type"] = event
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return bw.Flush()
	}

	// 第一个事件：response.created
	if err := emit("response.created", map[string]any{
		"response": map[string]any{
			"id": responseID, "object": "response", "model": model,
			"status": "in_progress", "output": []any{},
		},
	}); err != nil {
		return err
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// 认不出的行跳过，不让一行坏数据掐断整条流
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				responseID = ev.Message.ID
				inputTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			st := &blockState{outputIndex: nextIndex}
			nextIndex++
			switch ev.ContentBlock.Type {
			case "thinking":
				st.kind = "reasoning"
				st.itemID = fmt.Sprintf("rs_%d", st.outputIndex)
				if err := emit("response.output_item.added", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "reasoning", "id": st.itemID, "summary": []any{},
					},
				}); err != nil {
					return err
				}
				if err := emit("response.reasoning_summary_part.added", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": ""},
				}); err != nil {
					return err
				}
			case "tool_use":
				st.kind = "function_call"
				st.itemID = "fc_" + fmt.Sprint(st.outputIndex)
				st.callID = ev.ContentBlock.ID
				st.name = ev.ContentBlock.Name
				if err := emit("response.output_item.added", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": st.itemID,
						"call_id": st.callID, "name": st.name, "arguments": "",
					},
				}); err != nil {
					return err
				}
			default:
				st.kind = "message"
				st.itemID = "msg_" + fmt.Sprint(st.outputIndex)
				if err := emit("response.output_item.added", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "message", "id": st.itemID, "role": "assistant",
						"status": "in_progress", "content": []any{},
					},
				}); err != nil {
					return err
				}
				if err := emit("response.content_part.added", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				}); err != nil {
					return err
				}
			}
			blocks[ev.Index] = st
		case "content_block_delta":
			st, ok := blocks[ev.Index]
			if !ok || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				st.text += ev.Delta.Text
				text.WriteString(ev.Delta.Text)
				if err := emit("response.output_text.delta", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0, "delta": ev.Delta.Text,
				}); err != nil {
					return err
				}
			case "thinking_delta":
				st.text += ev.Delta.Thinking
				reasoning.WriteString(ev.Delta.Thinking)
				if err := emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"summary_index": 0, "delta": ev.Delta.Thinking,
				}); err != nil {
					return err
				}
			case "input_json_delta":
				st.args += ev.Delta.PartialJSON
				if err := emit("response.function_call_arguments.delta", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"delta": ev.Delta.PartialJSON,
				}); err != nil {
					return err
				}
			}
		case "content_block_stop":
			st, ok := blocks[ev.Index]
			if !ok {
				continue
			}
			switch st.kind {
			case "message":
				if err := emit("response.output_text.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0, "text": st.text,
				}); err != nil {
					return err
				}
				if err := emit("response.content_part.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": st.text, "annotations": []any{}},
				}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "message", "id": st.itemID, "role": "assistant",
						"status": "completed",
						"content": []any{map[string]any{
							"type": "output_text", "text": st.text, "annotations": []any{},
						}},
					},
				}); err != nil {
					return err
				}
			case "function_call":
				if err := emit("response.function_call_arguments.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"arguments": st.args,
				}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": st.itemID,
						"call_id": st.callID, "name": st.name, "arguments": st.args,
					},
				}); err != nil {
					return err
				}
			case "reasoning":
				if err := emit("response.reasoning_summary_part.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": st.text},
				}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "reasoning", "id": st.itemID,
						"summary": []any{map[string]any{"type": "summary_text", "text": st.text}},
					},
				}); err != nil {
					return err
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				outputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			// 收尾在循环外统一做
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	return emit("response.completed", map[string]any{
		"response": map[string]any{
			"id": responseID, "object": "response", "model": model,
			"status": "completed", "stop_reason": stopReason,
			"output_text":            text.String(),
			"reasoning_summary_text": reasoning.String(),
			"usage": map[string]any{
				"input_tokens": inputTokens, "output_tokens": outputTokens,
			},
		},
	})
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./internal/convert/...
```

Expected: PASS

- [ ] **Step 6: 提交**

```bash
gofmt -w internal/convert/
git add internal/convert
git commit -m "feat: Codex Responses 协议转换

请求转 Anthropic Messages，上游 SSE 转回 Responses 事件。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: gateway 包

**Files:**
- Create: `internal/gateway/auth.go`
- Create: `internal/gateway/errors.go`
- Create: `internal/gateway/gateway.go`
- Create: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/gateway/gateway_test.go`：

```go
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/rotator"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// harness is a gateway with one account and a fake upstream.
type harness struct {
	srv    *httptest.Server
	gw     *Server
	store  *store.Store
	hits   int
	tokens []string
	// renewals counts the token renewals the gateway asked for.
	renewals int
}

func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.TokenPath {
			// 续签也走假服务器：测试永远不碰 MiniMax 自己。
			h.renewals++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok2","refresh_token":"ref2","expires_in":3600}`))
			return
		}
		h.hits++
		h.tokens = append(h.tokens, r.Header.Get("Authorization"))
		handler(w, r)
	}))
	t.Cleanup(h.srv.Close)

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
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(context.Background(), store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	mgr := manager.New(s, up, account.New(up), egress.New(nil, ""), oauth.NewRenewer())
	// 续签的地址指到假服务器：一个测试碰到续签的时候，也不该去
	// MiniMax 自己那里。
	mgr.TokenURL = h.srv.URL + site.TokenPath
	cfg := config.Default()
	cfg.Server.APIKeys = nil
	h.store = s
	h.gw = New(mgr, cfg, s, nil)
	// 上游地址指到假服务器
	h.gw.UpstreamBase = h.srv.URL
	return h
}

func TestAnthropicPassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mavis/api/v1/llm/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	})
	body := `{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"hi"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestOpenAIPassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mavis/api/v1/llm/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestCodexUsesAnthropicFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 上游没有 /responses，返回 404
		if r.URL.Path == "/mavis/api/v1/llm/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/mavis/api/v1/llm/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	// 回退路径非流式时返回 Responses 的形状
	if !strings.Contains(w.Body.String(), `"object":"response"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestQuotaRefusalFailsOverToAnotherAccount(t *testing.T) {
	// The count is a local one: the handler is given before the
	// harness that holds it exists.
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"insufficient balance"}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 第二个账号
	now := time.Now()
	h.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	h.store.SaveToken(context.Background(), store.Token{
		AccountID: "acc_02", Access: "tok2", Refresh: "ref2",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.hits < 2 {
		t.Fatalf("the request was tried %d times", h.hits)
	}
	// 第一个账号被标记耗尽
	ex, err := h.store.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].AccountID != "acc_01" {
		t.Fatalf("exhausted = %+v", ex)
	}
}

func TestOtherSiteRedirectIsNotFailover(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", w.Code)
	}
	if h.hits != 1 {
		t.Fatalf("the request was tried %d times, want 1", h.hits)
	}
	if !strings.Contains(w.Body.String(), "proxy") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", "sk-1")
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("the key was refused")
	}
}

func TestModels(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"MiniMax-M3"}]}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Object string `json:"object"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Data) == 0 || body.Data[0].ID == "" {
		t.Fatalf("body = %s", w.Body.String())
	}
	if body.Object != "list" {
		t.Fatalf("object = %q", body.Object)
	}
}

func TestModelsFallsBackToBuiltIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "MiniMax-M3") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestStreamIsForwarded(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "message_start") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
}

// countingStore counts the questions the pool adapter asks the store.
//
// 包一层是为了数问题，不是为了改 store。
type countingStore struct {
	*store.Store
	readings int
}

func (c *countingStore) LatestUsageSnapshots(ctx context.Context) ([]store.UsageSnapshot, error) {
	c.readings++
	return c.Store.LatestUsageSnapshots(ctx)
}

// TestRemainingAllReadsTheCreditsOfEveryAccountAtOnce says the credits
// of a whole list of accounts come from one read.
//
// 一次挑选要问所有候选账号的余额。一次读就够了：每个账号读一次，
// N 个账号就是 N 次读。
func TestRemainingAllReadsTheCreditsOfEveryAccountAtOnce(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_02", "acc_03"} {
		h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		})
	}
	// acc_02 的余额读到了，acc_01 没有
	h.store.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_02", HasAmounts: true, Granted: 600, Consumed: 100, Remaining: 500,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	})

	c := &countingStore{Store: h.store}
	p := poolAdapter{c}
	got := p.RemainingAll(ctx, []string{"acc_01", "acc_02", "acc_03"})
	if c.readings != 1 {
		t.Fatalf("the readings were read %d times, want 1", c.readings)
	}
	rem, ok := got["acc_02"]
	if !ok {
		t.Fatalf("the account whose credits are known is not in the map: %+v", got)
	}
	if !rem.Known || rem.Left != 500 {
		t.Fatalf("remaining = %+v", rem)
	}
	if _, ok := got["acc_01"]; ok {
		t.Fatalf("an account whose credits are not known is in the map: %+v", got)
	}

	// 单个账号那条路走的是同一份数据，两边不能对不上
	left, known := p.Remaining(ctx, "acc_02")
	if !known || left != 500 {
		t.Fatalf("remaining = %v, known = %v", left, known)
	}
	if _, known := p.Remaining(ctx, "acc_01"); known {
		t.Fatal("the account without a reading says it has credits")
	}
}

// TestPickReadsTheCreditsOfTheAccountsAtOnce says one pick asks for the
// credits of every account it may choose in one question.
//
// 一个请求要挑账号时，余额是一次读完的，不是一个账号一次。
func TestPickReadsTheCreditsOfTheAccountsAtOnce(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_02", "acc_03"} {
		h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		})
		h.store.SaveUsageSnapshot(ctx, store.UsageSnapshot{
			AccountID: id, HasAmounts: true, Granted: 600, Remaining: 400,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		})
	}
	c := &countingStore{Store: h.store}
	rot := rotator.New(poolAdapter{c}, rotator.Options{MaxAttempts: 3, WindowGroup: manager.WindowGroup})
	if _, err := rot.Pick(ctx, "sess-1", "m", map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if c.readings != 1 {
		t.Fatalf("the credits were read %d times for one pick, want 1", c.readings)
	}
}

// TestQuotaRefusalThatNamesTheCreditsSpendsTheWindow says a refusal
// that names the credits spends the account's window.
//
// 说了额度用完就是额度用完：这个账号的窗口记成用完，换一个账号。
func TestQuotaRefusalThatNamesTheCreditsSpendsTheWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)

	ex, err := h.store.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].AccountID != "acc_01" {
		t.Fatalf("exhausted = %+v", ex)
	}
	if ex[0].WindowKey != "general" {
		t.Fatalf("window key = %q, want the group", ex[0].WindowKey)
	}
	if ex[0].Reason != "quota" {
		t.Fatalf("reason = %q", ex[0].Reason)
	}
}

// TestForbiddenThatDoesNotNameTheCreditsDoesNotSpendTheWindow says a
// 403 that only mentions a limit is not a spent account.
//
// 说了"限制"不等于说了"额度用完"。把窗口记成用完，这个账号就白等
// 一整个窗口。
func TestForbiddenThatDoesNotNameTheCreditsDoesNotSpendTheWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden","detail":"the limit of the requests"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)

	ex, err := h.store.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 0 {
		t.Fatalf("the window was spent: %+v", ex)
	}
}

// TestTheClientIsToldTheLimitWasReached says the 429 the client gets
// when the credits are gone says the limit was reached.
//
// 上游自己说额度用完了，客户端就该看到这句话：等一会儿再来，而不是
// 以为哪里坏了。
func TestTheClientIsToldTheLimitWasReached(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "usage limit reached: insufficient balance") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestUnauthorizedRenewsTheToken says a 401 renews the token and asks
// the same account again.
//
// 401 是令牌旧了：续签之后还是这个账号。
func TestUnauthorizedRenewsTheToken(t *testing.T) {
	// 次数是本地的一份：handler 是先给的，那时 h还不存在。
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"the sign-in was refused"}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.renewals != 1 {
		t.Fatalf("the token was renewed %d times, want 1", h.renewals)
	}
	if len(h.tokens) < 2 || h.tokens[0] == h.tokens[1] {
		t.Fatalf("the account was asked again with the same token: %v", h.tokens)
	}
}

// TestForbiddenRenewsTheToken says a 403 renews the token too.
//
// 站点用 403 拒签到，账号这一边也是这么读的：令牌旧了，续签。
func TestForbiddenRenewsTheToken(t *testing.T) {
	// 次数是本地的一份：handler 是先给的，那时 h还不存在。
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"forbidden"}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.renewals != 1 {
		t.Fatalf("the token was renewed %d times, want 1", h.renewals)
	}
	if len(h.tokens) < 2 || h.tokens[0] == h.tokens[1] {
		t.Fatalf("the account was asked again with the same token: %v", h.tokens)
	}
}

// TestRefusedSignInInTheBodyRenewsTheToken says the site's own code for
// a refused sign-in renews the token.
//
// 200 的身体里带着 1000048：站点说签到被拒了。令牌旧了，续签。
func TestRefusedSignInInTheBodyRenewsTheToken(t *testing.T) {
	// 次数是本地的一份：handler 是先给的，那时 h还不存在。
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Write([]byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.renewals != 1 {
		t.Fatalf("the token was renewed %d times, want 1", h.renewals)
	}
}

// brokenStream writes one event and then breaks the connection.
//
// 上游断了：客户端要看到一个收尾的事件，而不是一条断掉的流。
func brokenStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("event: message_start\ndata: {}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	panic(http.ErrAbortHandler)
}

// brokenResponsesStream is a Responses stream that breaks.
func brokenResponsesStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	panic(http.ErrAbortHandler)
}

// TestBrokenStreamEndsWithTheProtocolsOwnFailure says a stream that
// breaks ends with one failure event, in the shape of the protocol the
// client speaks.
//
// 断了的流要给一个收尾事件，形状得是客户端认得的那个：OpenAI 的
// 客户端不认 Anthropic 的 error 事件，Codex 的客户端认 response.failed。
func TestBrokenStreamEndsWithTheProtocolsOwnFailure(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		h := newHarness(t, brokenStream)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		body := w.Body.String()
		if n := strings.Count(body, "event: error"); n != 1 {
			t.Fatalf("the stream has %d error events, want 1: %s", n, body)
		}
		if strings.Contains(body, "response.failed") {
			t.Fatalf("the stream has a Responses event in it: %s", body)
		}
	})
	t.Run("openai", func(t *testing.T) {
		h := newHarness(t, brokenStream)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		body := w.Body.String()
		if n := strings.Count(body, `"error"`); n != 1 {
			t.Fatalf("the stream has %d errors, want 1: %s", n, body)
		}
		if !strings.Contains(body, `data: {"error"`) {
			t.Fatalf("the error is not an OpenAI one: %s", body)
		}
		if strings.Contains(body, "event: error") {
			t.Fatalf("the stream has an Anthropic event in it: %s", body)
		}
	})
	t.Run("codex", func(t *testing.T) {
		h := newHarness(t, brokenResponsesStream)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true}`))
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		body := w.Body.String()
		if n := strings.Count(body, "event: response.failed"); n != 1 {
			t.Fatalf("the stream has %d failures, want 1: %s", n, body)
		}
		if strings.Contains(body, "event: error") {
			t.Fatalf("the stream has an Anthropic event in it: %s", body)
		}
		if !strings.Contains(body, `"id":"resp_1"`) {
			t.Fatalf("the failure does not name the response: %s", body)
		}
	})
}

// TestCodexFallbackStreamHasOneFailure says the Codex fallback does not
// end the stream twice.
//
// 上游说不了 Responses 的时候，本地把 Anthropic 的流转成 Responses 的。
// 那一层断了的时候自己会写 response.failed，网关再写一个 Anthropic 的
// error 事件，客户端就看到两次失败，形状还不一样。
func TestCodexFallbackStreamHasOneFailure(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.ResponsesPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		brokenStream(w, r)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	body := w.Body.String()
	if n := strings.Count(body, "event: response.failed"); n != 1 {
		t.Fatalf("the stream has %d failures, want 1: %s", n, body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("the stream has an Anthropic event in it: %s", body)
	}
}

// signIn signs the browser in and gives the cookie it got.
func signIn(t *testing.T, h *harness, key string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader("key="+url.QueryEscape(key)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no cookie was given: %v", w.Header()["Set-Cookie"])
	return nil
}

// TestTheCookieIsAccepted says the browser's cookie opens the gateway.
//
// 浏览器不会替页面带上 x-api-key，cookie 是它能带的那张门票。
func TestTheCookieIsAccepted(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	c := signIn(t, h, "sk-1")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.AddCookie(c)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestTheSignInPageIsReachableWithoutASession says the page that signs
// the browser in does not need a session.
//
// 没有会话的人正要来这里登录。
func TestTheSignInPageIsReachableWithoutASession(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodGet, "/signin", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), `<form`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	// 页面不回显 key，也不该被缓存
	if strings.Contains(w.Body.String(), "sk-1") {
		t.Fatalf("the key is in the page: %s", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("cache control = %q", cc)
	}
}

// TestAWrongKeySetsNoCookie says a wrong key is refused and leaves no
// cookie behind.
func TestAWrongKeySetsNoCookie(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader("key=sk-wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatalf("a cookie was given for a wrong key: %+v", c)
		}
	}
}

// TestACookieOfARotatedKeyIsRefused says a cookie stops working when
// the key it was made of is not configured any more.
//
// 换一把 key，旧的 cookie 自己就作废了：服务端不用存任何东西。
func TestACookieOfARotatedKeyIsRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	c := signIn(t, h, "sk-1")
	h.gw.Cfg.Server.APIKeys = []string{"sk-2"}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.AddCookie(c)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestTheCookieIsOnlyForTheBrowser says the cookie is HttpOnly and
// Strict.
//
// cookie 本身就是一张门票：谁拿到它谁就能用这个网关。所以脚本读不到
// 它（HttpOnly），别的站点的表单也带不上它（SameSite=Strict）。
func TestTheCookieIsOnlyForTheBrowser(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	c := signIn(t, h, "sk-1")
	if !c.HttpOnly {
		t.Fatal("a script can read the cookie")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("same site = %v, want strict", c.SameSite)
	}
	if c.Path != "/" {
		t.Fatalf("path = %q", c.Path)
	}
	if c.Value == "" || c.Value == "sk-1" {
		t.Fatalf("the cookie carries the key itself: %q", c.Value)
	}
}

// TestAPageWithoutASessionGivesTheSignInPage says a browser that opens
// a page is given the sign-in page.
//
// 浏览器打开的是页面，给它一段 JSON 等于什么也做不了。
func TestAPageWithoutASessionGivesTheSignInPage(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content type = %q, body = %s", ct, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<form`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestAnAPIWithoutASessionGivesJSON says the API clients keep their
// JSON answer.
//
// 脚本和模型客户端读得懂 JSON，读不懂一个登录页。
func TestAnAPIWithoutASessionGivesJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	for _, path := range []string{"/api/usage", "/v1/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: code = %d", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s: content type = %q", path, ct)
		}
	}
}

// TestWithoutAKeyNothingIsChecked says a gateway without keys lets
// everything through.
//
// 没配 key 的时候网关不查：那时候它只该听在本机，serve 那一条命令管
// 这件事。
func TestWithoutAKeyNothingIsChecked(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the request was refused")
	}
}

// TestSignOutTakesTheCookieAway says there is a way out.
//
// 签出要能删掉 cookie：不然浏览器里那张票一直在。
func TestSignOutTakesTheCookieAway(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodDelete, "/api/session", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("the cookie was not taken away: %v", w.Result().Cookies())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/gateway/...
```

Expected: FAIL —— `undefined: New`

- [ ] **Step 3: 实现鉴权**

创建 `internal/gateway/auth.go`：

```go
package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
)

// sessionCookie is the name of the cookie the browser carries.
//
// 网关的页面要过 auth，而浏览器不会替页面带上 x-api-key。这个 cookie
// 就是浏览器能带的那张票。
const sessionCookie = "mmxcode_session"

// sessionContext is what the cookie's value is computed over.
//
// 它是固定的一串字：同一把 key 永远算出同一个值，换一把 key 就换一个
// 值。
const sessionContext = "mmxcode-webui-session-v1"

// sessionValue gives the cookie's value for one key.
//
// 值是 key 的带钥匙的哈希：HMAC-SHA256(key = API key, message = 上面
// 那串固定的字)。
//
// 这么做的取舍是：服务端不用存任何东西，转一把 key 旧的 cookie 自己
// 就作废；代价是这个 cookie 本身就是一张票，谁拿到它谁就能用这个网关。
// 所以它是 HttpOnly（脚本读不到它），也是 SameSite=Strict（别的站点
// 的表单带不上它）。
func sessionValue(apiKey string) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(sessionContext))
	return hex.EncodeToString(mac.Sum(nil))
}

// auth checks the request's key.
//
// The key comes in the header, the way it always did, or in the cookie
// the browser carries.
//
// 没有配 key 的时候网关不查：那时候它只该听在本机，serve 那一条命令
// 管这件事。
func (s *Server) auth(r *http.Request) bool {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 {
		return true
	}
	given := r.Header.Get("x-api-key")
	if given == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			given = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(given), []byte(k)) == 1 {
			return true
		}
	}
	// 浏览器带来的 cookie：值是某一把配好的 key 算出来的
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionValue(k))) == 1 {
			return true
		}
	}
	return false
}

// session handles the sign-in cookie.
//
// POST gives the cookie, DELETE takes it away. Neither needs a session:
// signing in is how one gets one, and signing out has to work even when
// the cookie is already gone.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.signIn(w, r)
	case http.MethodDelete:
		s.signOut(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// signIn takes the key and gives the cookie.
//
// The key comes from the form the browser posts, or from a JSON body a
// script sends. The page that comes back never carries the key: it is a
// secret, and the page might sit in a cache.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 {
		writeError(w, http.StatusBadRequest, "this gateway has no api key configured")
		return
	}
	key := submittedKey(r)
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
			s.setSessionCookie(w, r, k)
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	// 错的 key：不发 cookie
	s.signInPage(w, r, http.StatusUnauthorized, "the key was refused")
}

// signOut takes the cookie away.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// setSessionCookie gives the browser the cookie.
//
// The value is computed from the key, so the server keeps nothing.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, apiKey string) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionValue(apiKey),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	// 只有走 TLS 的连接才标 Secure：网关常常是局域网里的 http，标了
	// Secure 的 cookie 浏览器根本不会带回来。
	if r.TLS != nil {
		c.Secure = true
	}
	http.SetCookie(w, c)
}

// submittedKey reads the key out of the request.
//
// 表单和 JSON 都认：浏览器发的是表单，脚本发的多半是 JSON。
func submittedKey(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Key string `json:"key"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			return strings.TrimSpace(body.Key)
		}
		return ""
	}
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return strings.TrimSpace(r.PostFormValue("key"))
}

// signInPage is the page the browser signs in on.
//
// It is a page and not a JSON answer: the browser asked for a page. The
// key never comes back into it, and it is not to be cached: a key left in
// a shared machine's cache is a key left behind.
//
// The status says whether the page is the one the browser was after (200)
// or the answer to a key that was refused (401).
func (s *Server) signInPage(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	signInTemplate.Execute(w, map[string]any{
		"Error":   errMsg,
		"HasKeys": len(s.Cfg.Server.APIKeys) > 0,
	})
}

// signInTemplate is the page the browser signs in on.
var signInTemplate = template.Must(template.New("signin").Parse(signInHTML))

// signInHTML is the sign-in page.
//
// 表单里只有一个框：把 key 填进去。没有 key 的网关不需要登录，所以
// 那时候页面只说一句。
const signInHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>登录 · mmxcode</title>
<style>
:root { color-scheme: light dark; }
body { font: 14px/1.6 system-ui, sans-serif; margin: 0; padding: 2rem 1rem; }
h1 { font-size: 1.1rem; }
form { display: flex; gap: .5rem; align-items: center; max-width: 32rem; }
input { flex: 1; padding: .4rem; }
button { padding: .25rem .8rem; cursor: pointer; }
.bad { color: #c0392b; }
</style>
</head>
<body>
<h1>mmxcode</h1>
{{if .HasKeys}}
<p>这个网关要一把 API key。填进去就能进管理页。</p>
<form method="post" action="/api/session">
<input type="password" name="key" placeholder="API key" autocomplete="current-password" autofocus>
<button type="submit">进入</button>
</form>
{{if .Error}}<p class="bad">{{.Error}}</p>{{end}}
{{else}}
<p>这个网关没有配 API key，不用登录。</p>
{{end}}
</body>
</html>
`

// isAPIPath says the path is one the API clients use.
//
// 脚本和模型客户端读得懂 JSON，读不懂一个登录页；浏览器打开的是页面，
// 给它登录页才对。
func isAPIPath(path string) bool {
	for _, prefix := range []string{"/v1", "/api"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 实现错误改写**

创建 `internal/gateway/errors.go`：

```go
package gateway

import (
	"encoding/json"
	"net/http"
)

// writeError writes a plain error answer.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
}

// writeRateLimit writes the answer for a refusal that is about the
// credits.
//
// The client is told the limit was reached, so it knows to wait and
// try again rather than that something broke.
func writeRateLimit(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "rate_limit_error",
			"message": "usage limit reached: " + message,
		},
	})
}
```

- [ ] **Step 5: 实现网关**

创建 `internal/gateway/gateway.go`：

```go
// Package gateway serves the chat protocols and the management API.
//
// A chat request is bound to an account by its session, and stays
// there. When the account is spent, another is tried and the request
// is replayed — but only while nothing has been written to the client
// yet.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/convert"
	"mmxcode/internal/manager"
	"mmxcode/internal/rotator"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// nowOffset gives the timezone offset in seconds east of UTC.
//
// It is the value the reference sends as getTimezoneOffset() * -60.
// time.Time has no Offset() method: Zone()'s second value is it.
func nowOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// Server is the gateway.
type Server struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store
	// Rot picks the account.
	Rot *rotator.Rotator
	// UpstreamBase is the host the chats go to. It is a field so a
	// test can point it at a stand-in.
	UpstreamBase string
	// NativeResponses says the upstream speaks the Responses protocol
	// itself. It is tried and turned off when the upstream says no.
	NativeResponses bool
	// WebUI serves the management pages, when there is one.
	WebUI http.Handler
}

// New makes a gateway.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store, webui http.Handler) *Server {
	g := &Server{
		Mgr:             mgr,
		Cfg:             cfg,
		Store:           s,
		UpstreamBase:    site.LLMHost,
		NativeResponses: true,
		WebUI:           webui,
	}
	g.Rot = rotator.New(poolAdapter{s}, rotator.Options{
		MaxAttempts: cfg.Rotation.MaxAttempts,
		Cooldown:    cfg.Rotation.ExhaustedCooldown.Duration(),
		WindowGroup: manager.WindowGroup,
		Strategy:    cfg.Rotation.Strategy,
	})
	return g
}

// poolStore is what the pool adapter reads from the store.
//
// It is an interface and not the store itself, so that a test can count
// the questions the adapter asks without the store knowing about
// counting.
type poolStore interface {
	ListAccounts(ctx context.Context) ([]store.Account, error)
	ListExhausted(ctx context.Context) ([]store.Exhausted, error)
	BindSession(ctx context.Context, sessionKey, accountID string) error
	SessionAccount(ctx context.Context, sessionKey string) (string, bool, error)
	SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
	LatestUsageSnapshots(ctx context.Context) ([]store.UsageSnapshot, error)
}

// poolAdapter makes the store into a rotator pool.
type poolAdapter struct{ s poolStore }

func (p poolAdapter) Accounts(ctx context.Context) ([]store.Account, error) {
	return p.s.ListAccounts(ctx)
}

func (p poolAdapter) Exhausted(ctx context.Context) ([]store.Exhausted, error) {
	return p.s.ListExhausted(ctx)
}

func (p poolAdapter) BindSession(ctx context.Context, key, accountID string) error {
	return p.s.BindSession(ctx, key, accountID)
}

func (p poolAdapter) SessionAccount(ctx context.Context, key string) (string, bool, error) {
	return p.s.SessionAccount(ctx, key)
}

func (p poolAdapter) MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	return p.s.SetExhausted(ctx, accountID, windowKey, until, reason)
}

// RemainingAll gives the credits left of a whole list of accounts.
//
// The rotator asks for every account it may pick in one question, so
// the answer comes out of one read of the readings and not one read
// per account.
//
// The map holds only the accounts whose credits are known: an account
// that is not in it has not been read yet, and the rotator takes it
// after the ones whose balance it knows.
func (p poolAdapter) RemainingAll(ctx context.Context, accountIDs []string) map[string]rotator.Remaining {
	out := map[string]rotator.Remaining{}
	if len(accountIDs) == 0 {
		return out
	}
	wanted := make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		wanted[id] = true
	}
	snaps, err := p.s.LatestUsageSnapshots(ctx)
	if err != nil {
		return out
	}
	for _, s := range snaps {
		if !wanted[s.AccountID] {
			continue
		}
		if !s.HasAmounts {
			// 没读到余额的账号不算有余额：留空，让 rotator 把它排在后面。
			continue
		}
		out[s.AccountID] = rotator.Remaining{Left: s.Remaining, Known: true}
	}
	return out
}

// Remaining gives one account's credits left.
//
// It goes through the same read as RemainingAll, so that the two
// cannot disagree about one account.
func (p poolAdapter) Remaining(ctx context.Context, accountID string) (float64, bool) {
	rem, ok := p.RemainingAll(ctx, []string{accountID})[accountID]
	if !ok {
		return 0, false
	}
	return rem.Left, true
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/signin":
		// 登录页不用会话：没有会话的人正要来这里登录。
		s.signInPage(w, r, http.StatusOK, "")
		return
	case "/api/session":
		// 发 cookie 和销 cookie 都不用会话。
		s.session(w, r)
		return
	}
	if !s.auth(r) {
		if isAPIPath(r.URL.Path) {
			writeError(w, http.StatusUnauthorized, "the key was refused")
			return
		}
		// 浏览器来的是页面：给它登录页，不是一段 JSON。
		s.signInPage(w, r, http.StatusOK, "")
		return
	}
	switch r.URL.Path {
	case "/v1/messages":
		s.handleChat(w, r, protocolAnthropic)
		return
	case "/v1/chat/completions":
		s.handleChat(w, r, protocolOpenAI)
		return
	case "/v1/responses":
		s.handleChat(w, r, protocolCodex)
		return
	case "/v1/models":
		s.handleModels(w, r)
		return
	}
	if s.WebUI != nil && strings.HasPrefix(r.URL.Path, "/api/") {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	if s.WebUI != nil {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "no such endpoint")
}

// protocol is the protocol the client speaks.
type protocol int

const (
	protocolAnthropic protocol = iota
	protocolOpenAI
	protocolCodex
)

// handleChat serves a chat request.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, proto protocol) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the request body could not be read")
		return
	}
	// 上游请求的时限就是配的那一个。流式对话也吃这份预算：想要更长的
	// 就把 upstream.timeout 调大。
	ctx := r.Context()
	if d := s.Cfg.Upstream.Timeout.Duration(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	model := modelOf(body)
	stream := isStream(body)
	sess := rotator.SessionOf(r.Header, systemOf(body), firstUserOf(body))

	used := map[string]bool{}
	var lastErr error
	// quota 说最后一次失败是“额度用完了”，quotaMsg 是上游自己说的那句。
	// 客户端最后看到的是上游的话，不是网关自己编的一句。
	var quota bool
	var quotaMsg string
	attempts := s.Cfg.Rotation.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
accountLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if lastErr != nil {
				s.writeFailure(w, quota, quotaMsg, lastErr)
				return
			}
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		used[plan.AccountID] = true

		tok, err := s.Mgr.Token(ctx, plan.AccountID)
		if err != nil {
			lastErr = err
			continue
		}
		client, _, err := s.Mgr.Client(ctx, plan.AccountID)
		if err != nil {
			lastErr = err
			continue
		}

		upBody, upPath, convertBack := s.upstreamShape(body, proto)
		chatReq := upstream.ChatRequest{
			BaseURL:   s.UpstreamBase,
			Path:      upPath,
			Access:    tok.Access,
			SessionID: sess.UpstreamID,
			// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
			// 和 Zone() 给的偏移是同一个值。time.Time 没有 Offset()
			// 方法，Zone() 的第二个返回值就是它。
			TZOffset:  nowOffset(),
			Body:      upBody,
			Stream:    stream,
			Anthropic: upPath == site.MessagesPath,
			Extra:     clientHeaders(r),
		}

		// renewed 说这个账号的令牌已经续过一次：MiniMax 的 refresh token
		// 只能用一次，续第二次会把账号签死。
		renewed := false
		// 同一个账号可以再问几次：限流、5xx、续签之后，都是这个账号
		// 再来一次就好。
		for try := 0; try < attempts; try++ {
			resp, raw, err := s.attempt(ctx, chatReq, client.Transport, stream)
			if err != nil {
				var oe *upstream.OtherSiteError
				if errors.As(err, &oe) {
					// 跨站：不换号、不消耗 refresh token
					writeError(w, http.StatusBadGateway, oe.Error())
					return
				}
				// 网络没通：还是这个账号，等一等再来。
				lastErr = err
				time.Sleep(backoff(try))
				continue
			}

			// 上游没有 /responses：改用本地转换，同一个账号再来一次
			if resp.StatusCode == http.StatusNotFound && proto == protocolCodex && s.NativeResponses {
				s.NativeResponses = false
				upBody, upPath, convertBack = s.upstreamShape(body, proto)
				chatReq.Body = upBody
				chatReq.Path = upPath
				chatReq.Anthropic = upPath == site.MessagesPath
				resp, raw, err = s.attempt(ctx, chatReq, client.Transport, stream)
				if err != nil {
					var oe *upstream.OtherSiteError
					if errors.As(err, &oe) {
						writeError(w, http.StatusBadGateway, oe.Error())
						return
					}
					lastErr = err
					time.Sleep(backoff(try))
					continue
				}
			}

			// 200 且是流：直接转出去
			if resp.StatusCode == http.StatusOK && raw == nil {
				s.writeStream(w, resp, proto, convertBack, model)
				return
			}

			// 200 但身体里有业务错误
			if resp.StatusCode == http.StatusOK {
				if err := upstream.CheckEnvelope("chat", raw); err == nil {
					s.writeJSON(w, resp, raw, proto, convertBack)
					return
				}
			}

			failure := s.failure(resp.StatusCode, raw)
			switch s.action(failure) {
			case rotator.ActionFailover:
				// 额度用完了：这个账号的窗口记成用完，换一个账号。
				until := s.windowReset(ctx, plan.AccountID, plan.GroupKey)
				_ = s.Rot.MarkWindowExhausted(ctx, plan.AccountID, plan.GroupKey, until, "quota")
				lastErr = fmt.Errorf("the account's credits or plan are used up")
				quota = true
				quotaMsg = failure.Message
				continue accountLoop
			case rotator.ActionRenew:
				if renewed {
					// 续过一次还是被拒：这个账号先放着，换一个。
					lastErr = fmt.Errorf("the sign-in was refused (HTTP %d)", resp.StatusCode)
					quota = false
					quotaMsg = ""
					continue accountLoop
				}
				newTok, rerr := s.Mgr.ForceRenew(ctx, plan.AccountID)
				if rerr != nil {
					// 跨站：不换号、不消耗 refresh token
					var oe *upstream.OtherSiteError
					if errors.As(rerr, &oe) {
						writeError(w, http.StatusBadGateway, oe.Error())
						return
					}
					lastErr = rerr
					quota = false
					quotaMsg = ""
					continue accountLoop
				}
				// 用新 token 再来一次，还是这个账号
				renewed = true
				chatReq.Access = newTok.Access
				quota = false
				quotaMsg = ""
				continue
			case rotator.ActionRetrySame:
				// 限流、5xx：这个账号等一等，再问它一次。
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				quota = false
				quotaMsg = ""
				time.Sleep(backoff(try))
				continue
			default:
				// 给不出别的说法：上游的答案原样给客户端。
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				w.Write(raw)
				return
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("the request could not be served")
	}
	s.writeFailure(w, quota, quotaMsg, lastErr)
}

// failure says what went wrong upstream.
//
// The code and the message come out of the parsed answer, so that the
// decision the rotator makes and the words the client is given are read
// from one thing and not from two.
func (s *Server) failure(status int, raw []byte) rotator.Failure {
	f := rotator.Failure{Status: status, Body: raw}
	err := upstream.CheckEnvelope("chat", raw)
	if err == nil {
		return f
	}
	var be *upstream.BusinessError
	if !errors.As(err, &be) {
		return f
	}
	f.Code = be.Code
	f.Message = be.Message
	// 1000048：站点说签到被拒了。令牌照旧了，账号本身没问题。
	if errors.Is(err, upstream.ErrSignInRefused) {
		f.Refused = true
	}
	return f
}

// action says what to do about the answer.
//
// The rotator owns the rule. The one thing it cannot know is that the
// site refuses a sign-in with a 403 as well as with a 401 — the account
// package reads both that way — so the gateway says the sign-in was
// refused.
//
// It says it only when the rotator did not read the answer as a
// refusal to spend: an answer that names the credits is about the
// credits, and spending the window is the right answer for it.
func (s *Server) action(failure rotator.Failure) rotator.Action {
	action := rotator.Decide(failure)
	if action == rotator.ActionFailover {
		return action
	}
	if failure.Status != http.StatusUnauthorized && failure.Status != http.StatusForbidden {
		return action
	}
	failure.Refused = true
	return rotator.Decide(failure)
}

// writeFailure writes the answer of a request that could not be served.
//
// The words the client is given are the upstream's own when it said the
// credits are gone: "usage limit reached: …" tells the client to wait
// and try again, which a plain error does not.
func (s *Server) writeFailure(w http.ResponseWriter, quota bool, quotaMsg string, err error) {
	if quota {
		message := quotaMsg
		if message == "" {
			message = "the account's credits or plan are used up"
		}
		writeRateLimit(w, message)
		return
	}
	writeError(w, http.StatusTooManyRequests, err.Error())
}

// attempt sends the request upstream and gives the answer back.
//
// A stream is read no further than its first event: enough to tell an
// error from an answer, little enough that the client still gets the
// first token as soon as the upstream sends it. The rest of the stream
// is left on the response's body.
//
// raw is the whole body when the answer was not a stream, and nil when
// it was.
func (s *Server) attempt(ctx context.Context, req upstream.ChatRequest, transport http.RoundTripper, wantStream bool) (*http.Response, []byte, error) {
	resp, err := s.Mgr.Up.Chat(ctx, req, transport)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, raw, nil
	}
	if !wantStream {
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return resp, nil, err
		}
		return resp, raw, nil
	}
	br := bufio.NewReader(resp.Body)
	head, _ := peekEvent(br)
	if sseHasError(head) {
		// 200 里装着错误：整条读完，当成失败处理
		rest, _ := io.ReadAll(br)
		resp.Body.Close()
		return resp, append(head, rest...), nil
	}
	// 把读走的第一个事件放回去，客户端一个字节都不会少
	resp.Body = &rejoinedBody{
		Reader: io.MultiReader(bytes.NewReader(head), br),
		Closer: resp.Body,
	}
	return resp, nil, nil
}

// rejoinedBody is a body whose first event was read ahead of time.
type rejoinedBody struct {
	io.Reader
	io.Closer
}

// peekEvent reads the first event of a stream.
//
// An SSE event ends at the blank line, so that is what is read.
func peekEvent(br *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		line, err := br.ReadString('\n')
		head = append(head, line...)
		if err != nil {
			return head, err
		}
		if line == "\n" {
			return head, nil
		}
	}
}

// sseHasError says a stream's first event carries an error.
//
// MiniMax can answer 200 with an error in the stream. By the time the
// client has seen a byte the account cannot be changed any more, so
// the error is looked for before anything is written.
func sseHasError(head []byte) bool {
	for _, line := range strings.Split(string(head), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type       string          `json:"type"`
			Error      json.RawMessage `json:"error"`
			StatusInfo *struct {
				Code int `json:"code"`
			} `json:"statusInfo"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.StatusInfo != nil && ev.StatusInfo.Code != 0 {
			return true
		}
		if len(ev.Error) > 0 && string(ev.Error) != "null" && string(ev.Error) != "{}" {
			return true
		}
		if ev.Type == "error" {
			return true
		}
	}
	return false
}

// upstreamShape gives the request the upstream is to get.
//
// Anthropic and OpenAI go through untouched: the site speaks both. The
// Responses protocol is turned into the Anthropic one, unless the site
// has shown it speaks Responses itself.
func (s *Server) upstreamShape(body []byte, proto protocol) (out []byte, path string, convertBack bool) {
	switch proto {
	case protocolAnthropic:
		return body, site.MessagesPath, false
	case protocolOpenAI:
		return body, site.ChatCompletionsPath, false
	default:
		if s.NativeResponses {
			return body, site.ResponsesPath, false
		}
		b, err := convert.ToAnthropic(body)
		if err != nil {
			return body, site.ResponsesPath, false
		}
		return b, site.MessagesPath, true
	}
}

// copyHeaders gives the client's the upstream's headers.
//
// The encoding is left out: the client is told what it gets, not that
// it gets something compressed.
func copyHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, v := range resp.Header {
		switch strings.ToLower(k) {
		case "content-encoding", "content-length", "transfer-encoding":
			continue
		}
		w.Header()[k] = v
	}
}

// writeStream writes the upstream's stream to the client.
//
// The bytes go out as they arrive, so the client gets the first token
// as soon as the upstream sends it. Once the first byte is out the
// account cannot be changed any more; if the stream then breaks, the
// client is told the stream ended early rather than being sent a
// second, different answer.
//
// The event that says so has the shape of the protocol the client
// speaks: an OpenAI client does not read Anthropic's error event, and a
// Codex client reads the Responses one.
func (s *Server) writeStream(w http.ResponseWriter, resp *http.Response, proto protocol, convertBack bool, model string) {
	copyHeaders(w, resp)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var err error
	if convertBack {
		// 转换器自己收尾：它失败的时候已经写了一个 response.failed，
		// 网关再写一个就是第二次失败，形状还不一样。
		err = convert.StreamResponses(resp.Body, w, model)
	} else {
		sniffer := &idSniffer{w: w}
		_, err = io.Copy(sniffer, resp.Body)
		if err != nil {
			writeStreamError(sniffer, proto, err, model, sniffer.responseID())
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	resp.Body.Close()
}

// writeStreamError writes the event that says the stream broke.
//
// The shape is the one of the protocol the client speaks.
func writeStreamError(w io.Writer, proto protocol, err error, model, responseID string) {
	message := "the stream ended early: " + err.Error()
	switch proto {
	case protocolOpenAI:
		// OpenAI 的流里，错误也是一行 data。
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": "api_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
	case protocolCodex:
		// Codex 读的是 Responses 的形状，和本地转换写出来的一样。
		response := map[string]any{
			"object": "response",
			"model":  model,
			"status": "failed",
			"error":  map[string]any{"code": "upstream_error", "message": message},
		}
		if responseID != "" {
			response["id"] = responseID
		}
		b, _ := json.Marshal(map[string]any{
			"type":     "response.failed",
			"response": response,
		})
		fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", b)
	default:
		b, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": message},
		})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
	}
}

// idSniffer keeps the first bytes of a stream, to find the response id.
//
// The Responses protocol names the response in its first event, and the
// failure at the end of a broken stream has to carry the same id: the
// client matches the two.
type idSniffer struct {
	w   io.Writer
	buf []byte
}

// maxSniff is how many bytes are looked at for the response id.
const maxSniff = 8 << 10

func (s *idSniffer) Write(p []byte) (int, error) {
	if len(s.buf) < maxSniff {
		n := maxSniff - len(s.buf)
		if n > len(p) {
			n = len(p)
		}
		s.buf = append(s.buf, p[:n]...)
	}
	return s.w.Write(p)
}

// responseID gives the id of the response the stream is about.
func (s *idSniffer) responseID() string {
	m := responseIDPattern.FindSubmatch(s.buf)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// responseIDPattern finds the id of the response in the first events.
var responseIDPattern = regexp.MustCompile(`"id"\s*:\s*"(resp[^"]*)"`)

// writeJSON writes a whole answer to the client.
func (s *Server) writeJSON(w http.ResponseWriter, resp *http.Response, body []byte, proto protocol, convertBack bool) {
	if convertBack {
		out, err := convertResponses(body, proto)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		body = out
	}
	copyHeaders(w, resp)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// convertResponses turns an Anthropic answer into a Responses answer.
func convertResponses(body []byte, proto protocol) ([]byte, error) {
	if proto != protocolCodex {
		return body, nil
	}
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("the upstream's answer could not be read")
	}
	text := ""
	for _, c := range msg.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	out := map[string]any{
		"id":     msg.ID,
		"object": "response",
		"status": "completed",
		"output": []any{map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"output_text": text,
	}
	return json.Marshal(out)
}

// handleModels serves the model list.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var access string
	accounts, err := s.Store.ListAccounts(r.Context())
	if err == nil && len(accounts) > 0 {
		if tok, err := s.Mgr.Token(r.Context(), accounts[0].ID); err == nil {
			access = tok.Access
		}
	}
	raw, err := s.Mgr.Up.Models(r.Context(), s.UpstreamBase, access)
	if err != nil || len(raw) == 0 {
		raw = nil
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if raw != nil {
		json.Unmarshal(raw, &out)
	}
	if len(out.Data) == 0 {
		for _, m := range site.FallbackModels() {
			out.Data = append(out.Data, map[string]any{
				"id": m.ID, "object": "model", "owned_by": "minimax",
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   out.Data,
	})
}

// windowReset gives the moment a spent window comes back.
//
// The key is the plan group, not one window: the windows "5 hours" and
// "7 days" belong to a group, and the group comes back as a whole.
func (s *Server) windowReset(ctx context.Context, accountID, groupKey string) time.Time {
	ws, err := s.Mgr.LatestWindows(ctx, accountID)
	if err != nil {
		return time.Time{}
	}
	for _, w := range ws {
		if w.ModelName == groupKey && !w.ResetsAt.IsZero() {
			return w.ResetsAt
		}
	}
	return time.Time{}
}

// backoff is how long to wait before asking again.
//
// The wait grows with the attempt and is short enough that a request
// does not hang for long.
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

// clientHeaders gives the headers the client set that are worth
// passing on.
func clientHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"anthropic-version", "anthropic-beta", "x-stainless-lang"} {
		if v := r.Header.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// modelOf reads the model out of a request body.
func modelOf(body []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &b)
	return b.Model
}

// isStream says the request wants a stream.
func isStream(body []byte) bool {
	var b struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &b)
	return b.Stream
}

// systemOf reads the system prompt out of a request body.
func systemOf(body []byte) string {
	var b struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	return textOf(b.System)
}

// firstUserOf reads the first user message out of a request body.
//
// The three protocols put it in different places, so all of them are
// looked at.
func firstUserOf(body []byte) string {
	var b struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	for _, m := range b.Messages {
		if m.Role == "user" {
			return textOf(m.Content)
		}
	}
	if len(b.Input) > 0 {
		var s string
		if json.Unmarshal(b.Input, &s) == nil {
			return s
		}
	}
	return ""
}

// textOf reads the text out of a content field, which may be a string
// or a list of parts.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
```

`rotator.SessionOf` 已在 Task 3 里导出，gateway 直接用。它给出两个东西：
亲和键（永远是哈希，客户端自己那串字符串不会变成数据库键）和发给上游的
会话 id（客户端带了就用客户端的，换号时不变）。

- [ ] **Step 6: 运行测试确认通过**

```bash
go test ./internal/gateway/...
```

Expected: PASS

- [ ] **Step 7: 提交**

```bash
gofmt -w internal/gateway/ internal/rotator/
git add internal/gateway internal/rotator
git commit -m "feat: gateway 包

三协议路由、鉴权、配额拒绝改写、跨站 502、失败换号重放。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: webui 包

**Files:**
- Create: `internal/webui/webui.go`
- Create: `internal/webui/templates/layout.html`
- Create: `internal/webui/templates/accounts.html`
- Create: `internal/webui/templates/egress.html`
- Create: `internal/webui/templates/usage.html`
- Create: `internal/webui/templates/login.html`
- Create: `internal/webui/static/app.js`

- [ ] **Step 1: 写失败的测试**

创建 `internal/webui/webui_test.go`：

```go
package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

func newTestWebUI(t *testing.T) (*WebUI, *store.Store) {
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
	groups := map[string]string{"warp": "socks5h://warp:4000"}
	// The account API is pointed at a stand-in server: the check-in
	// route reaches the account API, and a test must never talk to
	// MiniMax itself.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no upstream in the test", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(groups, ""), oauth.NewRenewer())
	cfg := config.Default()
	// The outlet groups are the configuration's; the resolver is built
	// from the same map, so the page and the resolver agree on which
	// groups there are.
	cfg.Proxy.Groups = groups
	return New(mgr, cfg, s), s
}

func TestAccountsPage(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "主号") {
		t.Fatalf("body = %s", rec.Body.String())
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

func TestAPIEgress(t *testing.T) {
	w, _ := newTestWebUI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/egress", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "warp") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAPIPatchAccount(t *testing.T) {
	w, s := newTestWebUI(t)
	body := strings.NewReader(`{"proxy_group":"warp","name":"新名字"}`)
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
	if a.ProxyGroup != "warp" || a.Name != "新名字" {
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

	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/webui/...
```

Expected: FAIL —— `undefined: New`

- [ ] **Step 3: 写模板**

创建 `internal/webui/templates/layout.html`：

```html
{{define "layout"}}<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>{{.Title}} · mmxcode</title>
<style>
:root { color-scheme: light dark; }
body { font: 14px/1.6 system-ui, sans-serif; margin: 0; padding: 0 1rem 3rem; }
header { display: flex; align-items: baseline; gap: 1rem; padding: 1rem 0; border-bottom: 1px solid #8884; }
header h1 { font-size: 1.1rem; margin: 0; }
nav a { margin-right: 1rem; text-decoration: none; }
table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #8883; }
th { font-weight: 600; }
.bad { color: #c0392b; }
.good { color: #27ae60; }
.muted { color: #888; }
button { padding: .25rem .6rem; cursor: pointer; }
input, select { padding: .25rem; }
.cards { display: flex; gap: 1rem; flex-wrap: wrap; margin: 1rem 0; }
.card { border: 1px solid #8884; border-radius: 6px; padding: .8rem 1.2rem; }
.card .n { font-size: 1.6rem; }
</style>
</head>
<body>
<header>
  <h1>mmxcode</h1>
  <nav>
    <a href="/">账号</a>
    <a href="/egress">出口</a>
    <a href="/usage">用量</a>
    <a href="/login">登录</a>
    {{if .HasKeys}}<a href="/signin" id="sign-out">退出</a>{{end}}
  </nav>
  <span class="muted" id="status"></span>
</header>
<main>
{{template "content" .}}
</main>
<script src="/static/app.js"></script>
{{if .HasKeys}}
<script>
// 退出：把浏览器里那张票删掉，再回到登录页。
// 没有 key 的网关没有这张票，也就不用退出。
document.addEventListener('click', async (ev) => {
  const link = ev.target.closest('#sign-out');
  if (!link) return;
  ev.preventDefault();
  try {
    await fetch('/api/session', { method: 'DELETE' });
  } catch (e) {
    /* 签不出去也就还是那样 */
  }
  location.href = '/signin';
});
</script>
{{end}}
</body>
</html>{{end}}
```

创建 `internal/webui/templates/accounts.html`：

```html
{{define "content"}}
<h2>账号</h2>
<table>
<tr><th>标签</th><th>邮箱</th><th>状态</th><th>积分</th><th>套餐</th><th>出口</th><th>今日签到</th><th></th></tr>
{{range $acc := .Accounts}}
<tr>
  <td>{{$acc.Name}}<div class="muted">{{$acc.ID}}</div></td>
  <td>{{$acc.Email}}</td>
  <td>{{if eq $acc.Status "active"}}<span class="good">正常</span>{{else if eq $acc.Status "signed_out"}}<span class="bad">已登出</span>{{else}}<span class="bad">{{$acc.Status}}</span>{{end}}</td>
  <td>{{if $acc.HasAmounts}}{{printf "%.2f / %.2f" $acc.Remaining $acc.Granted}}{{else}}<span class="muted">读不到</span>{{end}}</td>
  <td>{{$acc.Plan}}<div class="muted">{{$acc.WindowSummary}}</div></td>
  <td>
    <select data-account="{{$acc.ID}}" data-field="proxy_group">
      <option value="">（跟随全局）</option>
      {{range $.Groups}}<option value="{{.Name}}"{{if eq .Name $acc.ProxyGroup}} selected{{end}}>{{.Name}}</option>{{end}}
    </select>
    <input size="18" placeholder="单独代理地址" data-account="{{$acc.ID}}" data-field="proxy_url" value="{{$acc.ProxyURL}}">
  </td>
  <td>{{if $acc.CheckedIn}}<span class="good">已签 {{$acc.Streak}} 天</span>{{else}}<span class="muted">未签</span>{{end}}</td>
  <td><button data-checkin="{{$acc.ID}}">立即签到</button></td>
</tr>
{{end}}
</table>
{{if not .Accounts}}<p class="muted">还没有账号。<a href="/login">去登录</a>。</p>{{end}}
{{end}}
```

模板里 `range` 的块内 `.` 是循环变量，`$acc` 是账号，`$.Groups` 是根上的出口组名单。每一行的下拉框要选中的是**这一行**账号的组，所以比的是 `$acc.ProxyGroup`。

创建 `internal/webui/templates/egress.html`：

```html
{{define "content"}}
<h2>出口</h2>
<table>
<tr><th>名字</th><th>地址</th><th>健康</th><th></th></tr>
{{range .Egress}}
<tr>
  <td>{{.Name}}</td>
  <td class="muted">{{.URL}}</td>
  <td>{{if .OK}}<span class="good">可用</span>{{else if .Checked}}<span class="bad">不可用：{{.Error}}</span>{{else}}<span class="muted">未检查</span>{{end}}</td>
  <td><button data-egress="{{.Name}}">测试</button></td>
</tr>
{{end}}
</table>
{{end}}
```

创建 `internal/webui/templates/usage.html`：

```html
{{define "content"}}
<h2>用量</h2>
<div class="cards">
  <div class="card"><div class="muted">总积分</div><div class="n">{{printf "%.2f" .Summary.TotalGranted}}</div></div>
  <div class="card"><div class="muted">总消耗</div><div class="n">{{printf "%.2f" .Summary.TotalConsumed}}</div></div>
  <div class="card"><div class="muted">总剩余</div><div class="n">{{printf "%.2f" .Summary.TotalRemaining}}</div></div>
</div>
{{if .Summary.ExpiresAt}}<p class="muted">最近过期：{{.Summary.ExpiresAt.Format "2006-01-02 15:04"}}</p>{{end}}
<p class="muted">读数时间：{{.Summary.UpdatedAt.Format "2006-01-02 15:04"}}</p>
{{if .Summary.AccountsUnavailable}}<p class="bad">有 {{.Summary.AccountsUnavailable}} 个账号的积分读不到。</p>{{end}}
{{if .Summary.AccountsExpired}}<p class="muted">有 {{.Summary.AccountsExpired}} 个账号的积分已经过期，没有算进总数。</p>{{end}}
<table>
<tr><th>账号</th><th>积分</th><th>消耗</th><th>剩余</th><th>过期</th></tr>
{{range .Summary.Accounts}}
<tr>
  <td>{{if .Email}}{{.Email}}{{else}}{{.ID}}{{end}}{{if eq .State "expired"}} <span class="muted">已过期</span>{{end}}</td>
  <td>{{if .Granted}}{{printf "%.2f" (num .Granted)}}{{else}}<span class="muted">读不到</span>{{end}}</td>
  <td>{{if .Consumed}}{{printf "%.2f" (num .Consumed)}}{{else}}0{{end}}</td>
  <td>{{if .Remaining}}{{printf "%.2f" (num .Remaining)}}{{else}}0{{end}}</td>
  <td class="muted">{{if .ExpiresAt}}{{.ExpiresAt.Format "2006-01-02"}}{{end}}</td>
</tr>
{{end}}
</table>
{{end}}
```

创建 `internal/webui/templates/login.html`：

```html
{{define "content"}}
<h2>登录</h2>
{{if .Pending}}
<p>把验证码 <strong>{{.Pending.UserCode}}</strong> 填到
   <a href="{{.Pending.URL}}" target="_blank">这个页面</a>。</p>
<p id="poll-result" class="muted">正在等……</p>
{{else}}
<button id="start-login">开始登录</button>
<div id="login-result"></div>
{{end}}
{{end}}
```

- [ ] **Step 4: 写静态脚本**

创建 `internal/webui/static/app.js`：

```javascript
// mmxcode 的 WebUI 脚本。
//
// 只做三件事：改账号的出口、触发签到、测出口。
// 页面本身是服务端渲染的，这里不重新渲染。
//
// 这里不用带 API key：浏览器同源的 fetch 会自己带上 mmxcode_session
// 那张票，页面的按钮因此不会 401。

async function post(url, body) {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(text || res.statusText);
  return text ? JSON.parse(text) : null;
}

async function patch(url, body) {
  const res = await fetch(url, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(await res.text());
}

document.addEventListener('click', async (ev) => {
  const checkin = ev.target.closest('[data-checkin]');
  if (checkin) {
    const id = checkin.dataset.checkin;
    checkin.disabled = true;
    checkin.textContent = '签到中……';
    try {
      const out = await post(`/api/accounts/${id}/checkin`);
      checkin.textContent = out.outcome === 'claimed' ? `领到 ${out.credit}` : out.outcome;
    } catch (e) {
      checkin.textContent = '失败';
      checkin.title = String(e.message);
    }
    return;
  }

  const egress = ev.target.closest('[data-egress]');
  if (egress) {
    const name = egress.dataset.egress;
    egress.disabled = true;
    egress.textContent = '测试中……';
    try {
      const out = await post('/api/egress/test', { name });
      egress.textContent = out.ok ? '可用' : '不可用';
      egress.className = out.ok ? 'good' : 'bad';
    } catch (e) {
      egress.textContent = '失败';
      egress.className = 'bad';
    }
    return;
  }

  if (ev.target.id === 'start-login') {
    const box = document.getElementById('login-result');
    box.textContent = '请稍候……';
    try {
      const out = await post('/api/accounts');
      box.innerHTML =
        `把验证码 <strong>${out.user_code}</strong> 填到 ` +
        `<a href="${out.verification_url}" target="_blank">这个页面</a>。`;
      poll(out.id);
    } catch (e) {
      box.textContent = '失败：' + e.message;
    }
  }
});

document.addEventListener('change', async (ev) => {
  const sel = ev.target.closest('[data-field]');
  if (!sel) return;
  const body = {};
  body[sel.dataset.field] = sel.value;
  try {
    await patch(`/api/accounts/${sel.dataset.account}`, body);
    document.getElementById('status').textContent = '已保存';
  } catch (e) {
    document.getElementById('status').textContent = '保存失败：' + e.message;
  }
});

function poll(id) {
  const timer = setInterval(async () => {
    try {
      const out = await post(`/api/accounts/poll`, { id });
      if (out.done) {
        clearInterval(timer);
        document.getElementById('poll-result').textContent = '登录成功，正在刷新……';
        location.reload();
      } else if (out.error) {
        clearInterval(timer);
        document.getElementById('poll-result').textContent = '失败：' + out.error;
      }
    } catch (e) {
      /* 继续等 */
    }
  }, 3000);
}
```

- [ ] **Step 5: 实现 webui**

创建 `internal/webui/webui.go`：

```go
// Package webui serves the management pages and the management API.
//
// The pages are rendered on the server; the script on them only
// changes what has to be changed. Nothing is rebuilt in the browser.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/usage"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// pendingLogin is a sign-in that is waiting for the user.
type pendingLogin struct {
	ID        string
	UserCode  string
	URL       string
	Device    *oauth.Device
	CreatedAt time.Time
	Err       string
}

// WebUI serves the pages and the API.
type WebUI struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store

	tmpl    *template.Template
	mu      sync.Mutex
	pending map[string]*pendingLogin
}

// New makes the WebUI.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store) *WebUI {
	// Only the layout is parsed here. Every page defines "content",
	// and one set of templates for all of them would keep only the
	// last page's content, so every page would render the last one.
	tmpl := template.Must(template.New("mmxcode").Funcs(template.FuncMap{"num": num}).ParseFS(templateFS, "templates/layout.html"))
	return &WebUI{
		Mgr:     mgr,
		Cfg:     cfg,
		Store:   s,
		tmpl:    tmpl,
		pending: map[string]*pendingLogin{},
	}
}

// ServeHTTP routes a request.
func (w *WebUI) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/static/app.js":
		w.serveStatic(rw, r)
		return
	case "/api/accounts":
		w.apiAccounts(rw, r)
		return
	case "/api/accounts/poll":
		w.apiPoll(rw, r)
		return
	case "/api/usage":
		w.apiUsage(rw, r)
		return
	case "/api/egress":
		w.apiEgress(rw, r)
		return
	case "/api/egress/test":
		w.apiEgressTest(rw, r)
		return
	case "/api/sessions":
		w.apiSessions(rw, r)
		return
	case "/api/events":
		w.apiEvents(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
		w.apiAccount(rw, r)
		return
	}
	switch r.URL.Path {
	case "/":
		w.page(rw, r, "accounts.html", w.accountsData)
	case "/egress":
		w.page(rw, r, "egress.html", w.egressData)
	case "/usage":
		w.page(rw, r, "usage.html", w.usageData)
	case "/login":
		w.page(rw, r, "login.html", w.loginData)
	default:
		http.NotFound(rw, r)
	}
}

// num is a number that may not be there.
//
// The credits are pointers, so that an account whose ledger could not
// be read is left out rather than counted as zero. A page has to show
// a number, so the pointer is read here.
func num(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// serveStatic gives the script.
func (w *WebUI) serveStatic(rw http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	rw.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	rw.Write(b)
}

// page renders one page.
func (w *WebUI) page(rw http.ResponseWriter, r *http.Request, name string, data func(context.Context) (map[string]any, error)) {
	d, err := data(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	d["Title"] = strings.TrimSuffix(name, ".html")
	// 配了 key 才有登录和退出可做：没配 key 的网关不用登录。
	d["HasKeys"] = len(w.Cfg.Server.APIKeys) > 0
	// The page brings its own "content", so it is parsed into a copy
	// of the layout.
	tmpl, err := w.tmpl.Clone()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tmpl.ParseFS(templateFS, "templates/"+name); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "layout", d); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Write([]byte(buf.String()))
}

// accountsData is the accounts page's data.
func (w *WebUI) accountsData(ctx context.Context) (map[string]any, error) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := w.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	byAccount := map[string]store.UsageSnapshot{}
	for _, s := range snaps {
		byAccount[s.AccountID] = s
	}
	groups := w.groupNames()
	out := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		snap := byAccount[a.ID]
		checkedIn, streak := w.todayCheckin(ctx, a.ID)
		out = append(out, map[string]any{
			"ID":            a.ID,
			"Name":          a.Name,
			"Email":         a.Email,
			"Status":        a.Status,
			"ProxyGroup":    a.ProxyGroup,
			"ProxyURL":      egress.Redacted(a.ProxyURL),
			"Current":       a.ProxyGroup,
			"HasAmounts":    snap.HasAmounts,
			"Granted":       snap.Granted,
			"Remaining":     snap.Remaining,
			"Plan":          snap.Plan,
			"WindowsError":  snap.WindowsError,
			"WindowSummary": w.windowSummary(ctx, a.ID),
			"CheckedIn":     checkedIn,
			"Streak":        streak,
		})
	}
	return map[string]any{"Accounts": out, "Groups": groups}, nil
}

// groupNames gives the outlet groups for the drop-down.
func (w *WebUI) groupNames() []map[string]any {
	var out []map[string]any
	for name := range w.Cfg.Proxy.Groups {
		out = append(out, map[string]any{"Name": name})
	}
	out = append(out, map[string]any{"Name": "direct"})
	return out
}

// windowSummary gives a one-line summary of the account's windows.
func (w *WebUI) windowSummary(ctx context.Context, accountID string) string {
	ws, err := w.Mgr.LatestWindows(ctx, accountID)
	if err != nil || len(ws) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ws))
	for _, x := range ws {
		if x.Unlimited {
			parts = append(parts, fmt.Sprintf("%s 不限", x.ModelName))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", x.ModelName, x.Used))
	}
	return strings.Join(parts, " · ")
}

// todayCheckin says whether the account has checked in today.
func (w *WebUI) todayCheckin(ctx context.Context, accountID string) (bool, int) {
	day := store.DayKey(time.Now())
	list, err := w.Store.ListCheckins(ctx, accountID, 5)
	if err != nil {
		return false, 0
	}
	for _, c := range list {
		if c.Day == day {
			return c.Outcome == "claimed" || c.Outcome == "done", c.Streak
		}
	}
	return false, 0
}

// egressData is the outlets page's data.
func (w *WebUI) egressData(ctx context.Context) (map[string]any, error) {
	health, err := w.Store.EgressHealth(ctx)
	if err != nil {
		return nil, err
	}
	byURL := map[string]store.EgressHealth{}
	for _, h := range health {
		byURL[h.URL] = h
	}
	var out []map[string]any
	for name, url := range w.Cfg.Proxy.Groups {
		h, ok := byURL[url]
		out = append(out, map[string]any{
			"Name": name, "URL": egress.Redacted(url),
			"OK": ok && h.OK, "Checked": ok, "Error": h.Error,
		})
	}
	return map[string]any{"Egress": out}, nil
}

// usageData is the usage page's data.
func (w *WebUI) usageData(ctx context.Context) (map[string]any, error) {
	snaps, err := w.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	emails := map[string]string{}
	for _, a := range accounts {
		emails[a.ID] = a.Email
	}
	s := usage.Aggregate(snaps, time.Now())
	for i := range s.Accounts {
		s.Accounts[i].Email = emails[s.Accounts[i].ID]
	}
	return map[string]any{"Summary": s}, nil
}

// loginData is the login page's data.
func (w *WebUI) loginData(ctx context.Context) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pending {
		return map[string]any{"Pending": map[string]any{
			"UserCode": p.UserCode, "URL": p.URL,
		}}, nil
	}
	return map[string]any{"Pending": nil}, nil
}

// apiAccounts starts a sign-in, or lists the accounts.
func (w *WebUI) apiAccounts(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d, err := w.accountsData(r.Context())
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"accounts": d["Accounts"]})
	case http.MethodPost:
		d, err := w.startLogin(r.Context())
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, d)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// startLogin asks the site for a sign-in code.
func (w *WebUI) startLogin(ctx context.Context) (map[string]any, error) {
	d, err := oauth.StartDevice(ctx, w.Mgr.Up, site.AccountURL(site.DeviceCodePath))
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("pending_%d", time.Now().UnixNano())
	w.mu.Lock()
	w.pending[id] = &pendingLogin{
		ID: id, UserCode: d.UserCode, URL: d.URL, Device: d, CreatedAt: time.Now(),
	}
	w.mu.Unlock()
	go w.waitLogin(id, d)
	return map[string]any{
		"id": id, "user_code": d.UserCode, "verification_url": d.URL,
	}, nil
}

// waitLogin waits for the sign-in to finish.
func (w *WebUI) waitLogin(id string, d *oauth.Device) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	tok, err := d.Poll(ctx, w.Mgr.Up, site.AccountURL(site.TokenPath))
	w.mu.Lock()
	p := w.pending[id]
	w.mu.Unlock()
	if err != nil {
		w.mu.Lock()
		if p != nil {
			p.Err = err.Error()
		}
		w.mu.Unlock()
		return
	}
	now := time.Now()
	acc := store.Account{
		ID:        fmt.Sprintf("acc_%d", now.UnixMilli()),
		Name:      tok.Subject,
		UserID:    tok.Subject,
		Status:    store.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := w.Store.CreateAccount(ctx, acc); err != nil {
		return
	}
	_ = w.Store.SaveToken(ctx, store.Token{
		AccountID: acc.ID, Access: tok.Access, Refresh: tok.Refresh,
		ExpiresAt: tok.Expires, Subject: tok.Subject, UpdatedAt: now,
	})
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
}

// apiPoll says how a sign-in is going.
func (w *WebUI) apiPoll(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	w.mu.Lock()
	p := w.pending[body.ID]
	w.mu.Unlock()
	if p == nil {
		writeJSON(rw, map[string]any{"done": true})
		return
	}
	if p.Err != "" {
		writeJSON(rw, map[string]any{"error": p.Err})
		return
	}
	writeJSON(rw, map[string]any{"done": false})
}

// apiUsage gives the credits of every account.
func (w *WebUI) apiUsage(rw http.ResponseWriter, r *http.Request) {
	snaps, err := w.Store.LatestUsageSnapshots(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	accounts, err := w.Store.ListAccounts(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	emails := map[string]string{}
	for _, a := range accounts {
		emails[a.ID] = a.Email
	}
	s := usage.Aggregate(snaps, time.Now())
	for i := range s.Accounts {
		s.Accounts[i].Email = emails[s.Accounts[i].ID]
	}
	writeJSON(rw, s)
}

// apiEgress gives the outlets and their health.
func (w *WebUI) apiEgress(rw http.ResponseWriter, r *http.Request) {
	d, err := w.egressData(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, d)
}

// apiEgressTest tests one outlet now.
func (w *WebUI) apiEgressTest(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	url, ok := w.Cfg.Proxy.Groups[body.Name]
	if !ok {
		http.Error(rw, "no such outlet", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := w.Mgr.Egress.Check(ctx, url, "https://agent.minimax.io/")
	ok2 := err == nil
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = w.Store.SetEgressHealth(r.Context(), url, ok2, msg)
	writeJSON(rw, map[string]any{"ok": ok2, "error": msg})
}

// apiAccount handles one account: patch, delete, usage, check-in.
func (w *WebUI) apiAccount(rw http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		http.NotFound(rw, r)
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "checkin":
			w.apiCheckin(rw, r, id)
			return
		case "usage":
			w.apiAccountUsage(rw, r, id)
			return
		}
		http.NotFound(rw, r)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name       *string `json:"name"`
			ProxyGroup *string `json:"proxy_group"`
			ProxyURL   *string `json:"proxy_url"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		a, err := w.Store.GetAccount(r.Context(), id)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusNotFound)
			return
		}
		if body.Name != nil {
			a.Name = *body.Name
		}
		if body.ProxyGroup != nil {
			// 组名写错会让账号悄悄改走全局默认。挡在这里。
			if *body.ProxyGroup != "" {
				if _, ok := w.Cfg.Proxy.Groups[*body.ProxyGroup]; !ok {
					http.Error(rw, "no such outlet group: "+*body.ProxyGroup, http.StatusBadRequest)
					return
				}
			}
			a.ProxyGroup = *body.ProxyGroup
		}
		if body.ProxyURL != nil {
			// The page shows the address without its credentials, so
			// the address that comes back unchanged is the one that
			// is stored, and not the redacted one. An address the
			// user really changed is written as it is.
			shown := egress.Redacted(a.ProxyURL)
			if *body.ProxyURL != shown || shown == a.ProxyURL {
				a.ProxyURL = *body.ProxyURL
			}
		}
		a.UpdatedAt = time.Now()
		if err := w.Store.UpdateAccount(r.Context(), a); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := w.Store.DeleteAccount(r.Context(), id); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"ok": true})
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// apiCheckin claims one account's credits.
func (w *WebUI) apiCheckin(rw http.ResponseWriter, r *http.Request, id string) {
	out, err := w.Mgr.Checkin(r.Context(), id)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, map[string]any{
		"outcome": out.Outcome, "credit": out.Credit,
		"streak": out.Streak, "message": out.Message,
	})
}

// apiAccountUsage gives one account's credits.
func (w *WebUI) apiAccountUsage(rw http.ResponseWriter, r *http.Request, id string) {
	snap, err := w.Mgr.RefreshUsage(r.Context(), id)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(rw, snap)
}

// apiSessions gives the session bindings.
func (w *WebUI) apiSessions(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, map[string]any{"sessions": []any{}})
}

// apiEvents pushes the state changes as they happen.
func (w *WebUI) apiEvents(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(http.StatusOK)
	f, ok := rw.(http.Flusher)
	if !ok {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(rw, ": ping\n\n")
			f.Flush()
		}
	}
}

// writeJSON writes a JSON answer.
func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(rw).Encode(v)
}
```


- [ ] **Step 6: 运行测试确认通过**

```bash
go test ./internal/webui/...
```

Expected: PASS

- [ ] **Step 7: 提交**

```bash
gofmt -w internal/webui/
git add internal/webui
git commit -m "feat: WebUI

服务端渲染的账号、出口、用量、登录四个页面，加上管理 API。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 7: scheduler 包

**Files:**
- Create: `internal/scheduler/scheduler.go`
- Create: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/scheduler/scheduler_test.go`：

```go
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

func TestCheckinDelayIsJittered(t *testing.T) {
	seen := map[string]time.Duration{}
	for i := 0; i < 40; i++ {
		d := CheckinDelay(0, 30*time.Minute)
		seen[d.String()] = d
	}
	if len(seen) < 5 {
		t.Fatalf("the delay did not vary: %v", seen)
	}
	for d := range seen {
		if d == "" {
			t.Fatal("empty delay")
		}
	}
}

func TestCheckinDelayFits(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := CheckinDelay(0, 30*time.Minute)
		if d < 0 || d > 30*time.Minute {
			t.Fatalf("delay = %v", d)
		}
	}
}

func TestNextCheckin(t *testing.T) {
	at, err := time.Parse("15:04", "09:00")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	got := NextCheckin(now, at, 30*time.Minute)
	// 今天 09:00 还没到，应该是今天
	if got.Day() != 13 {
		t.Fatalf("next = %v", got)
	}
	now = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	got = NextCheckin(now, at, 30*time.Minute)
	if got.Day() != 14 {
		t.Fatalf("next = %v, want tomorrow", got)
	}
}

func TestUsageRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"10","consumed_amount":"0","remaining_amount":"10","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	// The account API is pointed at the test server. Without this the
	// test asks MiniMax for the numbers and waits for an answer that
	// never comes.
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	sc.RefreshUsage(ctx)
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Granted != 10 {
		t.Fatalf("snaps = %+v", snaps)
	}
}

func TestCheckinAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/signin/status":
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
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()
	now := time.Now()
	s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	// The account API is pointed at the test server. Without this the
	// test asks MiniMax for the numbers and waits for an answer that
	// never comes.
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	results := sc.CheckinAll(ctx)
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Outcome != "claimed" {
		t.Fatalf("outcome = %q", results[0].Outcome)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./internal/scheduler/...
```

Expected: FAIL —— `undefined: New`

- [ ] **Step 3: 实现 scheduler**

创建 `internal/scheduler/scheduler.go`：

```go
// Package scheduler runs the recurring work: the daily check-in, the
// credits refresh, the token renewal and the outlet health check.
package scheduler

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"mmxcode/internal/checkin"
	"mmxcode/internal/config"
	"mmxcode/internal/manager"
)

// Scheduler runs the recurring work.
type Scheduler struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
}

// New makes a scheduler.
func New(mgr *manager.Manager, cfg config.Config) *Scheduler {
	return &Scheduler{Mgr: mgr, Cfg: cfg}
}

// CheckinResult is one account's check-in.
type CheckinResult struct {
	// AccountID is the account.
	AccountID string
	// Outcome is what happened.
	Outcome string
	// Credit is what it paid.
	Credit float64
	// Streak is the days in a row.
	Streak int
	// Error is what went wrong, when something did.
	Error string
}

// CheckinDelay gives the wait before one account's check-in.
//
// The delay is random inside the window, so a hundred accounts do not
// all hit the site at the same second.
func CheckinDelay(base time.Duration, window time.Duration) time.Duration {
	if window <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(window)))
}

// NextCheckin gives the moment the daily check-in runs.
//
// The time is a wall-clock time in the local zone. When the moment has
// passed, it is tomorrow's.
func NextCheckin(now time.Time, at time.Time, jitter time.Duration) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Add(CheckinDelay(0, jitter))
}

// CheckinAll claims every account's daily credits.
func (s *Scheduler) CheckinAll(ctx context.Context) []CheckinResult {
	accounts, err := s.Mgr.Accounts(ctx)
	if err != nil {
		return nil
	}
	out := make([]CheckinResult, 0, len(accounts))
	for _, a := range accounts {
		r := CheckinResult{AccountID: a.ID}
		res, err := s.Mgr.Checkin(ctx, a.ID)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Outcome = res.Outcome
			r.Credit = res.Credit
			r.Streak = res.Streak
		}
		out = append(out, r)
	}
	return out
}

// RefreshUsage reads every account's credits.
func (s *Scheduler) RefreshUsage(ctx context.Context) {
	accounts, err := s.Mgr.Accounts(ctx)
	if err != nil {
		return
	}
	for _, a := range accounts {
		if _, err := s.Mgr.RefreshUsage(ctx, a.ID); err != nil {
			// 一个账号读不到不影响别的
			continue
		}
	}
}

// RenewTokens renews the tokens that are about to end.
func (s *Scheduler) RenewTokens(ctx context.Context) {
	accounts, err := s.Mgr.Accounts(ctx)
	if err != nil {
		return
	}
	for _, a := range accounts {
		_, _ = s.Mgr.Token(ctx, a.ID)
	}
}

// CheckEgress tests every outlet and records the result.
func (s *Scheduler) CheckEgress(ctx context.Context) {
	groups := s.Cfg.Proxy.Groups
	for name, url := range groups {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.Mgr.Egress.Check(ctx, url, "https://agent.minimax.io/")
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		_ = s.Mgr.Store.SetEgressHealth(ctx, url, err == nil, msg)
		_ = name
	}
}

// Run runs the recurring work until the context ends.
func (s *Scheduler) Run(ctx context.Context) {
	at, err := time.Parse("15:04", s.Cfg.Schedule.CheckinAt)
	if err != nil {
		at = time.Date(0, 1, 1, 9, 0, 0, 0, time.Local)
	}
	usageEvery := s.Cfg.Schedule.UsageRefresh.Duration()
	if usageEvery <= 0 {
		usageEvery = 15 * time.Minute
	}
	tokenEvery := s.Cfg.Schedule.TokenRefresh.Duration()
	if tokenEvery <= 0 {
		tokenEvery = 5 * time.Minute
	}
	egressEvery := s.Cfg.Schedule.EgressHealth.Duration()
	if egressEvery <= 0 {
		egressEvery = 2 * time.Minute
	}

	go s.loop(ctx, usageEvery, s.RefreshUsage)
	go s.loop(ctx, tokenEvery, s.RenewTokens)
	go s.loop(ctx, egressEvery, s.CheckEgress)

	for {
		next := NextCheckin(time.Now(), at, 30*time.Minute)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			for _, r := range s.CheckinAll(ctx) {
				if r.Error != "" {
					fmt.Printf("checkin %s: %s\n", r.AccountID, r.Error)
					continue
				}
				fmt.Printf("checkin %s: %s\n", r.AccountID, r.Outcome)
			}
		}
	}
}

// loop runs a piece of work every so often.
func (s *Scheduler) loop(ctx context.Context, every time.Duration, work func(context.Context)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			work(ctx)
		}
	}
}

// OutcomeNames gives the outcome names, for the WebUI.
func OutcomeNames() []string {
	return []string{
		checkin.OutcomeClaimed,
		checkin.OutcomeDone,
		checkin.OutcomeIneligible,
		checkin.OutcomeInactive,
		checkin.OutcomeFailed,
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./internal/scheduler/...
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
gofmt -w internal/scheduler/
git add internal/scheduler
git commit -m "feat: scheduler 包

定时签到、用量刷新、token 续期、出口健康检查。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 8: serve 子命令

**Files:**
- Create: `cmd/mmxcode/serve.go`
- Modify: `cmd/mmxcode/root.go`（挂上 serve）

- [ ] **Step 1: 写 serve**

创建 `cmd/mmxcode/serve.go`：

```go
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/gateway"
	"mmxcode/internal/scheduler"
	"mmxcode/internal/webui"
)

func newServeCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "起 HTTP 服务、WebUI 与定时任务",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// 只有国际站。中国站是另一套主机，token 不通用。
			if s := e.cfg.Upstream.Site; s != "" && s != "global" {
				return fmt.Errorf("upstream.site = %q; only \"global\" is supported", s)
			}

			// 签到时间写错的话，调度器会默默按 09:00 跑。用户以为自己
			// 配了别的点，就永远等不到那个点的签到。起不来比悄悄跑错好。
			if _, err := time.Parse("15:04", e.cfg.Schedule.CheckinAt); err != nil {
				return fmt.Errorf("schedule.checkin_at = %q; want HH:MM", e.cfg.Schedule.CheckinAt)
			}

			// 没配 key 时只听本机，不然就是把模型白送出去
			listen := e.cfg.Server.Listen
			if len(e.cfg.Server.APIKeys) == 0 && !isLoopback(listen) {
				return fmt.Errorf("no api key is configured, so the gateway would listen on %s for everyone; put api_keys in the config or listen on 127.0.0.1", listen)
			}

			ui := webui.New(e.mgr, e.cfg, e.store)
			gw := gateway.New(e.mgr, e.cfg, e.store, ui)
			sc := scheduler.New(e.mgr, e.cfg)

			srv := &http.Server{
				Addr:              listen,
				Handler:           gw,
				ReadHeaderTimeout: 10 * time.Second,
				// 聊天会流很久，不给写超时
			}

			go sc.Run(ctx)

			errc := make(chan error, 1)
			go func() {
				fmt.Printf("网关在 %s 上\n", listen)
				errc <- srv.ListenAndServe()
			}()

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			select {
			case err := <-errc:
				if err != nil && err != http.ErrServerClosed {
					return err
				}
			case <-sig:
				fmt.Println("正在关掉……")
			}
			cancel()
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutCancel()
			return srv.Shutdown(shutCtx)
		},
	}
}

// isLoopback says a listen address is only on this machine.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
```

import 补上 `"github.com/spf13/cobra"`。

- [ ] **Step 2: 把 serve 挂上，并给 env 加 mgr**

修改 `cmd/mmxcode/root.go`：

把 `env` 结构体改成：

```go
type env struct {
	cfg   config.Config
	store *store.Store
	up    *upstream.Client
	egr   *egress.Resolver
	acc   *account.Client
	mgr   *manager.Manager
}
```

在 `PersistentPreRunE` 里，`e.egr` 与 `e.acc` 建好之后加上：

```go
		e.mgr = manager.New(s, e.up, e.acc, e.egr, oauth.NewRenewer())
```

import 加上 `"mmxcode/internal/manager"` 与 `"mmxcode/internal/oauth"`。

在 `root.AddCommand` 那一行加上：

```go
	root.AddCommand(newServeCmd(e))
```

- [ ] **Step 3: 编译并跑一遍**

```bash
go build ./...
go run ./cmd/mmxcode --db ./tmp.db serve --help
```

Expected: 打印 serve 的帮助

- [ ] **Step 4: 手工验证**

```bash
MMXCODE_CONFIG= go run ./cmd/mmxcode --db ./tmp.db serve --config ""
```

换个终端：

```bash
curl -s http://127.0.0.1:8080/v1/models | head -c 400
curl -s http://127.0.0.1:8080/api/usage
```

Expected: 第一个返回模型列表，第二个返回 `{"total_granted":0,...}`

验证完停掉服务，删掉临时库：

```bash
rm -f tmp.db tmp.db-wal tmp.db-shm
```

- [ ] **Step 5: 提交**

```bash
gofmt -w cmd/
git add cmd
git commit -m "feat: serve 子命令

起网关、WebUI 与定时任务。没配 key 时不许对外监听。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 9: Docker 与 WARP 部署

**Files:**
- Create: `Dockerfile`
- Create: `docker-compose.yml`
- Create: `.env.example`
- Create: `.dockerignore`

- [ ] **Step 1: 写 Dockerfile**

创建 `Dockerfile`：

```dockerfile
# 构建
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 先只拷依赖清单，依赖没变这一层就不用重建
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 静态编译，镜像里不需要 libc
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mmxcode ./cmd/mmxcode

# 运行
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 mmxcode \
    && mkdir -p /data \
    && chown mmxcode:mmxcode /data

COPY --from=builder /out/mmxcode /usr/local/bin/mmxcode

USER mmxcode
WORKDIR /app

ENV MMXCODE_CONFIG=/app/config.yaml
ENV TZ=Asia/Shanghai

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/mmxcode"]
CMD ["serve"]
```

- [ ] **Step 2: 写 .dockerignore**

创建 `.dockerignore`：

```
.git
docs
data
tmp.db
tmp.db-wal
tmp.db-shm
*.md
```

- [ ] **Step 3: 写 docker-compose.yml**

创建 `docker-compose.yml`：

```yaml
services:
  mmxcode:
    build: .
    image: mmxcode-gateway:latest
    restart: unless-stopped
    ports:
      # 只绑本机，管理界面不暴露到公网
      - "127.0.0.1:8080:8080"
    volumes:
      - ./data:/data
      - ./config.yaml:/app/config.yaml:ro
    environment:
      MMXCODE_CONFIG: /app/config.yaml
      # 容器内访问宿主机的代理要写 host.docker.internal
      MMXCODE_PROXY_DEFAULT: "${MMXCODE_PROXY_DEFAULT:-http://host.docker.internal:7890}"
      TZ: Asia/Shanghai
    extra_hosts:
      - "host.docker.internal:host-gateway"

  # WARP 作为额外的出口，默认不启动。
  # 开启：.env 里写 COMPOSE_PROFILES=warp
  warp:
    image: cloudflare/warp:latest
    profiles: ["warp"]
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    sysctls:
      net.ipv4.conf.all.src_valid_mark: "1"
    volumes:
      - warp-state:/var/lib/cloudflare-warp
    command:
      - /bin/sh
      - -c
      - |
        warp-cli --accept-tos registration new || true
        warp-cli mode proxy
        warp-cli proxy port 4000
        warp-cli proxy bind 0.0.0.0
        warp-cli connect
        tail -f /dev/null
    expose:
      - "4000"

volumes:
  warp-state:
```

- [ ] **Step 4: 写 .env.example**

创建 `.env.example`：

```bash
# 复制成 .env 后按需改。

# 容器内走宿主机的代理。要用别的出口就改这里。
MMXCODE_PROXY_DEFAULT=http://host.docker.internal:7890

# 开启 Cloudflare WARP 出口：把这行的值写成 warp。
# 删掉这一行 = 不启动 WARP。
COMPOSE_PROFILES=warp
```

- [ ] **Step 5: 写一份容器里用的配置**

创建 `config.docker.yaml`：

```yaml
server:
  listen: ":8080"
  api_keys: ["sk-local-change-me"]

proxy:
  default: "http://host.docker.internal:7890"
  groups:
    clash:  "http://host.docker.internal:7890"
    warp:   "socks5h://warp:4000"
    direct: "direct"

upstream:
  timeout: 120s
  site: global

rotation:
  strategy: sticky
  max_attempts: 3
  exhausted_cooldown: 5m

schedule:
  checkin_at: "09:00"
  usage_refresh: 15m
  token_refresh: 5m
  egress_health: 2m

db: "/data/mmxcode.db"
```

- [ ] **Step 6: 构建镜像并验证**

```bash
docker build -t mmxcode-gateway:latest .
docker run --rm mmxcode-gateway:latest models
```

Expected: 打印模型表格

```bash
docker run --rm -v "$PWD/data:/data" -v "$PWD/config.docker.yaml:/app/config.yaml:ro" \
  -p 127.0.0.1:8080:8080 mmxcode-gateway:latest
```

另一个终端：

```bash
curl -s -H "x-api-key: sk-local-change-me" http://127.0.0.1:8080/v1/models | head -c 300
```

Expected: 模型列表

- [ ] **Step 7: 验证 WARP profile（可选）**

```bash
cp .env.example .env
docker compose --profile warp up -d
docker compose logs -f warp
```

Expected: WARP 的日志里出现注册与连接成功；网关的出口页上 `warp` 组变绿

```bash
docker compose down
rm .env
```

- [ ] **Step 8: 提交**

```bash
git add Dockerfile docker-compose.yml .dockerignore .env.example config.docker.yaml
git commit -m "build: Docker 镜像与 WARP 部署

WARP 作为 SOCKS5 出口，默认关闭，用 COMPOSE_PROFILES=warp 开启。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 10: 收口

**Files:**
- Modify: `README.md`（补上网关与部署）

- [ ] **Step 1: 改 README**

把 `README.md` 的「命令」一节扩成：

```markdown
## 命令

```
mmxcode serve                    # 起 HTTP 服务 + WebUI + 定时任务
mmxcode login                    # 登录一个账号
mmxcode accounts list            # 列出账号
mmxcode accounts proxy <id> --group warp
mmxcode checkin                  # 给所有账号签到
mmxcode usage                    # 积分汇总
mmxcode models                   # 模型列表
```

## 对外接口

| 路由 | 用途 |
|---|---|
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | Codex Responses |
| `GET /v1/models` | 模型列表 |

配了 `api_keys` 时请求要带 `x-api-key` 或 `Authorization: Bearer <key>`。没配的时候不许对外监听，只走 `127.0.0.1`。

管理接口在 `/api/` 下：`/api/accounts`、`/api/accounts/{id}/checkin`、`/api/usage`、`/api/egress` 等。WebUI 在 `/`。

## 部署

```bash
cp .env.example .env
docker compose up -d
```

WARP 出口默认关闭。在 `.env` 里写 `COMPOSE_PROFILES=warp` 才启动，此时 `proxy.groups.warp` 指向 `socks5h://warp:4000`。
```

- [ ] **Step 2: 全量测试**

```bash
go vet ./...
go test ./... -count=1
```

Expected: 全部 PASS

- [ ] **Step 3: 端到端手工验证（需要真实账号）**

```bash
rm -f tmp.db*
go run ./cmd/mmxcode --db ./tmp.db login
go run ./cmd/mmxcode --db ./tmp.db accounts list
go run ./cmd/mmxcode --db ./tmp.db checkin
go run ./cmd/mmxcode --db ./tmp.db usage
```

再起网关，试一次真实对话：

```bash
go run ./cmd/mmxcode --db ./tmp.db serve --config ""
```

另一个终端：

```bash
curl -s http://127.0.0.1:8080/v1/messages \
  -H "content-type: application/json" \
  -d '{"model":"MiniMax-M3","max_tokens":64,"messages":[{"role":"user","content":"say hi"}]}'
```

Expected: 一段回答

**还要验证的两件事**：

1. **会话粘性**：连发两次带同一个 `X-Session-Id` 的请求，看网关日志里是不是同一个账号。
2. **换号**：把第一个账号的积分用光（或在 WebUI 上把它指到一个用不上的出口组），再发一次，看是不是换到了另一个账号。

验证完：

```bash
rm -f tmp.db tmp.db-wal tmp.db-shm
```

- [ ] **Step 4: 提交**

```bash
git add README.md
git commit -m "docs: README 补上网关与部署

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 完成标准

- `go vet ./...` 无输出
- `go test ./... -count=1` 全绿
- `mmxcode serve` 起得来，`/v1/models` 返回模型列表，`/api/usage` 返回汇总
- 三种协议各能发一次请求
- 配额拒绝会换号重放，跨站重定向返回 502 且不换号
- 签名的六个金标准向量仍然通过（计划 A 的）
- `docker build` 出的镜像能跑，`docker compose --profile warp up` 能起 WARP

## 不在本计划里

- 中国站（`minimax.cn` / `minimaxi.com`）：另一套主机，token 不通用
- 全局透明代理模式（`network_mode: service:warp`）：与按账号选代理互斥
- 上游模型列表的实时抓取（`/v1/models` 用的是内置列表兜底）

## 待实测

1. **上游是否原生支持 `/responses`**：实现里默认先试原生路径（`NativeResponses = true`）。要确认的是国际站 `/mavis/api/v1/llm/v1/responses` 到底在不在。在不在，两条路径的输出行为要一致。
2. **国际站的 `/credit/details` 能不能用**：不能用的话 `/api/usage` 的 `total_*` 只算有数据的账号，并带 `accounts_unavailable` 计数。
3. **`token_plan/remains_percent` 在国际站返不返数据**：不返的话耗尽标记退化成账号级 + 冷却。
4. **WARP 在当前网络能不能连通 `agent.minimax.io`**：不能的话 `warp` 组在出口页上就是红的，不用选它。
