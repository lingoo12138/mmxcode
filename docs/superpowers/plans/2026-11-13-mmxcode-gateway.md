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
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/checkin"
	"mmxcode/internal/egress"
	"mmxcode/internal/oauth"
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
// The layers above it — the gateway, the WebUI, the scheduler — come
// here for an account's token and for its readings, but they do reach
// the store on their own, so this package is not the only way into the
// database. What it does hold is that a token is renewed in one place
// and a refresh token is never spent twice.
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
	"mmxcode/internal/secret"
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
	// tokenNotWrittenReason says a renewal worked and its token could
	// not be written down.
	tokenNotWrittenReason = "the token was renewed but it could not be written down, so the token in the database is older than the token in use"
	// checkinNotWrittenReason says a check-in worked and its record
	// could not be written down.
	checkinNotWrittenReason = "the check-in was claimed but its record could not be written down, so the credit and the streak are recorded nowhere"
	// renewalFailedReason says a renewal failed but the token in hand
	// is still good.
	renewalFailedReason = "the renewal failed but the token in hand is still good"
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
	// Key opens the passwords the accounts were brought in with.
	//
	// It is empty when there is no key: then an account cannot be
	// signed in again, and saying so is better than pretending.
	Key secret.Key
	// ReloginFn signs an account in again. It is a field so that a
	// test can stand in for the network.
	ReloginFn ReloginFunc
	// reloginLocks serialises the relogin of one account.
	reloginLocks reloginLocks
}

// reloginLocks serialises the relogin of one account.
//
// 调度器的一轮和人在命令行里的那一次可能撞上。同一个账号被登两次，
// 站点那边就是两个设备码、两个会话、两条新链；后失败的那一次还会把
// 先成功那一次存好的令牌抹掉。
type reloginLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock takes the lock of one account and gives back the way to let it
// go.
func (l *reloginLocks) lock(id string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	mu, ok := l.m[id]
	if !ok {
		mu = &sync.Mutex{}
		l.m[id] = mu
	}
	l.mu.Unlock()
	mu.Lock()
	return mu.Unlock
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
//
// The token the renewer holds is asked for before the renewal: a
// renewal that worked but whose writing failed leaves the renewer with
// a newer token than the store has, and renewing again would spend
// another refresh token — one the site may have taken already.
func (m *Manager) Token(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, tokenReadError(accountID, err)
	}
	if stored.Access == "" && stored.Refresh == "" {
		// The sign-in was taken away: the site has refused it. There
		// is nothing to send, so the account fails here instead of
		// paying for a renewal that cannot work.
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	cur := tokenFromStore(stored)
	if held, ok := m.Renewer.Newest(accountID); ok && newerThanStored(held, stored) {
		// The renewer holds a token that is newer than the one in the
		// store, so it is the one to use: renewing again would spend
		// a refresh token that the site may have taken already.
		cur = held
		// It is written down as well, so that the next reader of the
		// store finds the same token.
		if err := m.saveToken(ctx, accountID, cur); err != nil {
			m.note(ctx, accountID, tokenNotWrittenReason+": "+err.Error())
		}
	}
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

// newerThanStored says the token is newer than the one the store has.
//
// The expiry says which one is newer, the same way saveToken says it:
// the site hands out access tokens that live an hour, so the token of
// the later renewal ends later. Two tokens that end at the same time
// are the same step of the chain, and the one in the store is kept.
func newerThanStored(tok *oauth.Token, stored store.Token) bool {
	if tok == nil || tok.Expires.IsZero() {
		return false
	}
	return stored.ExpiresAt.Before(tok.Expires)
}

// tokenReadError says what it is that the token could not be read.
//
// A token that is not there is an account that is not signed in, and
// that is one thing. Anything else is the database in trouble, and
// saying "not signed in" about it hides the cause: the account looks
// signed out in the WebUI while the truth is that the file could not
// be read at all.
func tokenReadError(accountID string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("account %s is not signed in", accountID)
	}
	return fmt.Errorf("account %s: the token could not be read: %w", accountID, err)
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
//   - Anything else — the site is having a bad moment, or the network
//     is. The token in hand may still do, so it is kept when it has
//     not ended.
func (m *Manager) Renew(ctx context.Context, accountID string, cur *oauth.Token) (*oauth.Token, error) {
	return m.renew(ctx, accountID, cur, false)
}

// renew trades a refresh token for a new one.
//
// force says whether the renewal was forced. A forced renewal is what
// the gateway does when the site has refused the access token, and then
// the token in hand is no good whatever its expiry says.
func (m *Manager) renew(ctx context.Context, accountID string, cur *oauth.Token, force bool) (*oauth.Token, error) {
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
		return m.renewFailed(ctx, accountID, cur, sent.get(), held, err, force)
	}
	if err := m.saveToken(ctx, accountID, tok); err != nil {
		// The site has just handed out a token that is good for an
		// hour. Throwing it away because the database would not take
		// it burns the quota of another account for nothing: the
		// caller fails the request over, and the refresh token in the
		// store is spent already, so every request after this one
		// renews again and spends another one.
		//
		// So the token is kept and the caller gets it, and no error
		// comes with it. An error would send the request over to
		// another account, and the gateway cannot make use of a token
		// that arrives with one: it fails the request over and drops
		// the token.
		//
		// The failed write is written down on the account instead, so
		// that the WebUI shows why the token in the database is older
		// than the token in use.
		m.note(ctx, accountID, tokenNotWrittenReason+": "+err.Error())
	}
	return tok, nil
}

// renewFailed says what a failed renewal means for the account.
//
// It gives the token to use when there is one — a refused token that
// was only stale is not the end of the account — and the error to give
// back when there is not.
func (m *Manager) renewFailed(ctx context.Context, accountID string, cur *oauth.Token,
	refused string, held *oauth.Token, err error, force bool) (*oauth.Token, error) {
	switch {
	case errors.Is(err, oauth.ErrRefreshChainBroken):
		// The chain cannot go on: the token in hand is spent and there
		// is no newer one the site would take. Only a human can mend
		// this, by signing the account in again.
		if mErr := m.markSignedOut(ctx, accountID, cur.Access, chainBrokenReason); mErr != nil {
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
		//
		// The cause goes with it: "the outcome is not known" says
		// which case it is, but the transport's own words say what
		// happened, and the sign-out branch keeps those too.
		if mErr := m.markError(ctx, accountID, cur.Access, unknownOutcomeReason+": "+err.Error()); mErr != nil {
			return nil, errors.Join(err, fmt.Errorf("the error could not be saved: %w", mErr))
		}
		return nil, err
	}
	var oe *oauth.Error
	if !asOAuthError(err, &oe) || !oe.IsSignedOut() {
		// Not a refusal: the site is having a bad moment, or the
		// network is. The token in hand may still do — the reference
		// keeps it — so it is kept when it has not ended yet.
		//
		// A forced renewal does not keep it: that is what the gateway
		// does when the site has refused the access token, and then
		// the token in hand is no good whatever its expiry says.
		if !force && cur != nil && cur.Expires.After(time.Now()) {
			m.note(ctx, accountID, renewalFailedReason+": "+err.Error())
			return cur, nil
		}
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
	if mErr := m.markSignedOut(ctx, accountID, cur.Access, err.Error()); mErr != nil {
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
//
// A forced renewal does not keep the token in hand when it fails: the
// site has refused the token that is in hand, so it is no good
// whatever its expiry says.
func (m *Manager) ForceRenew(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, tokenReadError(accountID, err)
	}
	if stored.Access == "" && stored.Refresh == "" {
		return nil, fmt.Errorf("account %s is signed out", accountID)
	}
	return m.renew(ctx, accountID, tokenFromStore(stored), true)
}

// markSignedOut records that an account's sign-in is gone.
//
// The sign-in is taken away as well: an account whose sign-in the site
// has refused must not keep a token that every request would try to
// renew.
//
// The write is **conditional**: it happens only while the token row
// still holds the access token that was refused. Another process may
// have signed the account in again in the meantime, and writing the
// sign-out then would undo that sign-in — the fresh token would be
// taken away, and the account would be shown as signed out while it
// holds a token that works.
//
// access is the access token of the chain in hand: the one the refusal
// is about. The state is written with the narrow setter, and not with
// the one that writes every field: the name, the proxy and the rest of
// the account are not this method's business, and writing them back from a
// copy that was read a moment ago would undo a change another writer
// made in between.
//
// The error is not swallowed: a sign-out that could not be written
// down is an account that stays active with a token the site refuses.
func (m *Manager) markSignedOut(ctx context.Context, accountID, access, reason string) error {
	written, err := m.Store.SignOutIfTheTokenIsStillThere(ctx, accountID, access, reason, time.Now())
	if err != nil {
		return err
	}
	if !written {
		// 有人在这中间把账号救回来了：库里那份令牌已经不是手里这一份
		// 了，这次失败说的已经不是它了。这一边不动它。
		return nil
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
// The write is conditional, for the same reason as in markSignedOut: the
// error is about the chain in hand, and a chain that has moved on since
// is somebody else's business.
//
// The renewer is not told anything: it has to keep the record of the
// token whose outcome is not known, because that token must not be sent
// again.
func (m *Manager) markError(ctx context.Context, accountID, access, reason string) error {
	_, err := m.Store.MarkErrorIfTheTokenIsStillThere(ctx, accountID, access, reason, time.Now())
	return err
}

// note writes a note on the account without changing its state.
//
// The note is about something that went wrong that the account is not
// broken by: a renewal that could not be written down, a check-in
// whose record was lost. The account itself is still good, so the
// state it has is kept and only the note is written.
//
// The note is written with a setter that writes the note alone: the
// status column is not touched, and the account is not read back to be
// written again. A sign-out that happens while the note is being
// written keeps its status — the two writers share the note column,
// and that is all they share.
//
// The note is best effort. A note that could not be written down must
// not make a working token unusable, and the fault that stopped the
// note is the very fault that stopped the thing the note is about.
func (m *Manager) note(ctx context.Context, accountID, reason string) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	if a.Status == store.StatusSignedOut {
		// The sign-out has its own reason, and that is the one that
		// matters: it says why the site does not take this account
		// any more.
		return
	}
	// The error is written on its own: the status is not written, and
	// not read back to be written again. A sign-out that happened
	// while this was being written must not be undone by it.
	_ = m.Store.SetAccountLastError(ctx, accountID, reason, time.Now())
}

// ErrNotSignedOut says the account is not signed out.
//
// 重登是给签出的账号准备的。状态不是签出的账号，替它登录没有意义：
// 它手里那条链还能用，重登一次只会把还能用的令牌换掉。
var ErrNotSignedOut = errors.New("这个账号不是签出状态，不用重登")

// ReloginFunc signs an account in again with the password it was
// brought in with.
type ReloginFunc func(ctx context.Context, acc store.Account, password string) error

// Relogin signs an account in again with the password it was brought
// in with.
//
// An account without a password cannot be signed in again: there is
// nothing to sign in with. That is an error, not a silent skip — the
// person who asked wants to know.
//
// Only a signed-out account is signed in again. An account that is
// still active has a token that works, and signing it in again would
// replace that token with a new one for no reason.
func (m *Manager) Relogin(ctx context.Context, accountID string) error {
	acc, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Status != store.StatusSignedOut {
		return ErrNotSignedOut
	}
	// 同一个账号的重登要一个一个来：调度器的一轮和命令行里的那一次
	// 可能撞上，两个都登的话，站点那边就是两个设备码、两个会话。
	unlock := m.reloginLocks.lock(accountID)
	defer unlock()
	// 等锁的时候别人可能已经把这个账号登回去了。再登一次是白登，
	// 而且会把刚换好的令牌再换一次。
	acc, err = m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Status != store.StatusSignedOut {
		return ErrNotSignedOut
	}
	cipher, err := m.Store.Password(ctx, accountID)
	if err != nil {
		return err
	}
	if cipher == "" {
		return errors.New("这个账号没有存密码，没法自己登回去")
	}
	if len(m.Key) == 0 {
		// 没有密钥，密文就开不了。密文在，说明当初是存过的，是密钥
		// 现在没了——说清楚，别让人以为是账号的问题。
		return errors.New("没有密码的密钥，存着的密文开不了")
	}
	password, err := m.Key.Open(cipher)
	if err != nil {
		return err
	}
	if m.ReloginFn == nil {
		return errors.New("没有地方能重新登录")
	}
	if err := m.ReloginFn(ctx, acc, password); err != nil {
		// 登不回去说的是这次登录失败，不是手里那条链的事。所以令牌
		// 不动：清空它只能毁掉一个还能用的凭据，什么也救不回来。
		// 这里写下的只是失败的原因，账号还是签出的。
		//
		// 写是**有条件**的：另一个进程可能刚把这个账号登回去了，那一
		// 行现在是 active 了，这时候把它写回 signed_out，就是把别人刚
		// 换好的令牌作废掉，还让这个账号显示成登出的状态——手里却握着
		// 一份能用的凭据，轮转池不用它，下一轮又要登一次。
		written, mErr := m.Store.MarkSignedOut(ctx, accountID, err.Error(), time.Now())
		if mErr != nil {
			return errors.Join(err, fmt.Errorf("the failure could not be saved: %w", mErr))
		}
		if !written {
			// 别人已经把它登回去了：原因照样要说给听着的人，但状态
			// 是别人的，不能由这一边改回去。
			return errors.Join(err, ErrNotSignedOut)
		}
		return err
	}
	// 旧的那条链就此作废：这次登录换来的令牌是新的链，旧的刷新令牌
	// 再也不会被发出去了。
	m.Renewer.Forget(accountID)
	return m.Store.UpdateAccountStatus(ctx, accountID, store.StatusActive, "", time.Now())
}

// ReloginResult says what one round of re-logins came to.
type ReloginResult struct {
	// Fixed is how many accounts were signed in again.
	Fixed int
	// Failed is how many could not be signed in again.
	Failed int
	// Reasons says why the ones that failed failed: one line per account.
	Reasons []string
}

// ReloginAll signs in again every account that is signed out and has
// a password.
//
// It gives back how many it fixed, how many it did not, and why.
//
// 理由是要的：只说「失败 2」，人得自己去一个一个看是哪个账号、为什么。
// 而「一个都没试成」（连账号都没列出来）和「都试了，都登回去了」在只
// 看着两个数字的时候是一样的——上一次那个错就是这样被藏起来的。
//
// An account without a password is skipped and is not a failure: it
// was never brought in with one, so there is nothing to sign in with,
// and that is not this round's fault.
func (m *Manager) ReloginAll(ctx context.Context, limit int) (ReloginResult, error) {
	var out ReloginResult
	accounts, err := m.Store.ListAccounts(ctx)
	if err != nil {
		// 账号都没列出来，这一趟就根本没跑。把它说成「成功 0，失败 0」，
		// 人会以为没什么要登的。
		return out, fmt.Errorf("the accounts could not be listed: %w", err)
	}
	for _, acc := range accounts {
		if acc.Status != store.StatusSignedOut {
			continue
		}
		if limit > 0 && out.Fixed+out.Failed >= limit {
			break
		}
		cipher, err := m.Store.Password(ctx, acc.ID)
		if err != nil || cipher == "" {
			continue
		}
		if err := m.Relogin(ctx, acc.ID); err != nil {
			if errors.Is(err, ErrNotSignedOut) {
				// 别人刚把它登回去了：这不算失败。
				continue
			}
			out.Failed++
			out.Reasons = append(out.Reasons, accountReason(acc, err))
			continue
		}
		out.Fixed++
	}
	return out, nil
}

// accountReason says which account it was, and what went wrong.
//
// 谁和为什么都要说：一个账号读不到，人得知道是哪一个，才知道去看哪
// 一行。
func accountReason(acc store.Account, err error) string {
	who := acc.Name
	if who == "" {
		who = acc.ID
	}
	return who + "（" + acc.ID + "）：" + err.Error()
}

// UsageRefreshResult says what one round of usage readings came to.
type UsageRefreshResult struct {
	// Refreshed counts the accounts whose credits were read.
	Refreshed int
	// Failed counts the accounts whose credits could not be read.
	Failed int
	// Reasons says why each of those failed, one line per account.
	Reasons []string
}

// RefreshAllUsage reads every account's credits now.
//
// 这是页面上那个「刷新」按钮按下去以后做的事：所有账号的账本现读一
// 遍，不等调度器下一趟。一个账号读不到不影响别的——但要说出来是哪一
// 个，人才知道去看哪一行。
//
// 账号都没列出来的时候，这一趟就根本没跑：把它说成「读了 0 个」，人
// 会以为账本是空的。
func (m *Manager) RefreshAllUsage(ctx context.Context) (UsageRefreshResult, error) {
	var out UsageRefreshResult
	accounts, err := m.Store.ListAccounts(ctx)
	if err != nil {
		return out, fmt.Errorf("the accounts could not be listed: %w", err)
	}
	for _, acc := range accounts {
		if _, err := m.RefreshUsage(ctx, acc.ID); err != nil {
			out.Failed++
			out.Reasons = append(out.Reasons, accountReason(acc, err))
			continue
		}
		out.Refreshed++
	}
	return out, nil
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
			if err := m.markSignedOut(ctx, accountID, tok.Access, err.Error()); err != nil {
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
			if err := m.markSignedOut(ctx, accountID, tok.Access, err.Error()); err != nil {
				return store.UsageSnapshot{}, err
			}
		}
	}

	title, err := acc.PlanTitle(ctx, tok.Access, group)
	switch {
	case err == nil:
		snap.Plan = title
	case isRefusal(err):
		if err := m.markSignedOut(ctx, accountID, tok.Access, err.Error()); err != nil {
			return store.UsageSnapshot{}, err
		}
	default:
		// The plan's name could not be read. The failure is kept:
		// a plan whose name could not be read is not a plan without
		// a name, and Plan: "" would read as "this account has no
		// plan" rather than "the plan could not be read".
		snap.PlanError = err.Error()
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
	if err := m.Store.SaveCheckin(ctx, store.Checkin{
		AccountID: accountID,
		Day:       day,
		Outcome:   out.Outcome,
		Credit:    out.Credit,
		Streak:    out.Streak,
		Message:   out.Message,
		CreatedAt: time.Now(),
	}); err != nil {
		// The check-in worked: the credits are claimed and the day
		// cannot be claimed again. The record of it is lost, so the
		// credit and the streak are recorded nowhere.
		//
		// The failure is written down on the account rather than
		// returned: the caller would tell the user that the check-in
		// failed, and it did not.
		m.note(ctx, accountID, checkinNotWrittenReason+": "+err.Error())
	}
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
	"errors"
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
		Access: "a", Body: []byte(`{}`),
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
	// MiniMax Code's own requests all carry the version header, so
	// every request here carries it too — the site reads the request
	// as MiniMax Code wrote it, not as the client's protocol wrote it.
	if httpReq.Header.Get("anthropic-version") == "" {
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

// TestTheCursorBreaksTheTie says the cursor is what decides between
// the accounts that were used at the same moment.
//
// 最久没用过的时刻一样的账号有好几个时，光看时刻分不出来。游标指着
// 谁就给谁：轮转才转得起来。
func TestTheCursorBreaksTheTie(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"), acc("acc_03"))
	r := New(p, Options{MaxAttempts: 3, Strategy: StrategyRoundRobin})
	// 中间那个用过，另外两个一次也没用过：时刻一样，谁也不比谁老。
	r.lastUsed["acc_02"] = time.Now()
	// 游标停在 acc_02 上：它用过，不在候选里；下一个候选是 acc_03。
	r.cursor = 1

	got, err := r.Pick(context.Background(), "", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_03" {
		t.Fatalf("picked %s, want the one the cursor points at", got.AccountID)
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
	// 这是已知的碰撞，而且是定义上的碰撞：会话键就是 system prompt 加
	// 第一条 user 消息的哈希，两段一样的开场白是同一个会话。数据库里
	// 存着的上游 id 也是按会话键给的，所以这两个会话共用一个上游会话，
	// 上游分不出是两个会话。服务器分不出来：请求里没有能分开它们的东西，
	// 要分开只能让客户端带上 X-Session-Id。
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

// idPool is a pool that keeps an upstream id for every session.
//
// 有上游 id 的池子：id 是它自己给的，不是请求算出来的。
type idPool struct {
	*fakePool
	ids map[string]string
	// asked holds the keys the pool was asked for.
	asked []string
	// err is what UpstreamID fails with.
	err error
}

func (p *idPool) UpstreamID(_ context.Context, key string) (string, error) {
	p.asked = append(p.asked, key)
	if p.err != nil {
		return "", p.err
	}
	if id, ok := p.ids[key]; ok {
		return id, nil
	}
	id := "up-" + key
	p.ids[key] = id
	return id, nil
}

func newIDPool(accounts ...store.Account) *idPool {
	return &idPool{fakePool: newFakePool(accounts...), ids: map[string]string{}}
}

// TestTheUpstreamIDComesFromThePool says the id the upstream is told
// about is the one the pool keeps.
//
// 上游看到的是数据库里存着的那个 id，不是请求算出来的那个。
func TestTheUpstreamIDComesFromThePool(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})
	sess := SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "sys", "hello")

	got, err := r.UpstreamID(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if got != "up-"+sess.Key {
		t.Fatalf("upstream id = %q", got)
	}
	if len(p.asked) != 1 || p.asked[0] != sess.Key {
		t.Fatalf("the pool was asked %v", p.asked)
	}
}

// TestTheUpstreamIDFallsBackToTheDerivedOne says a pool that keeps no
// id leaves the derived one in place.
//
// 没有上游 id 的池子：退回请求自己算出来的那个，规则和从前一样。
func TestTheUpstreamIDFallsBackToTheDerivedOne(t *testing.T) {
	// fakePool 没有 UpstreamID 方法：它就是那种给不出 id 的池子。
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})

	// 客户端带了会话 id：退回的就是客户端自己那串。
	withHeader := SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "sys", "hello")
	got, err := r.UpstreamID(context.Background(), withHeader)
	if err != nil {
		t.Fatal(err)
	}
	if got != "client-1" {
		t.Fatalf("upstream id = %q, want the client's own", got)
	}

	// 客户端没带：退回的是第一条消息的哈希。
	fromMessage := SessionOf(nil, "sys", "hello")
	got, err = r.UpstreamID(context.Background(), fromMessage)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || got != fromMessage.UpstreamID {
		t.Fatalf("upstream id = %q, want the hash of the first message", got)
	}
}

// TestTwoSessionsGetDifferentUpstreamIDs says two conversations do not
// share the id the upstream is told about.
//
// 两个会话拿到两个不同的上游 id。
func TestTwoSessionsGetDifferentUpstreamIDs(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})

	a, err := r.UpstreamID(context.Background(),
		SessionOf(map[string][]string{"X-Session-Id": {"a"}}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.UpstreamID(context.Background(),
		SessionOf(map[string][]string{"X-Session-Id": {"b"}}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatalf("upstream ids = %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("the two sessions got the same upstream id %q", a)
	}
}

// TestASessionWithNoKeyHasNoUpstreamID says a request with no
// conversation at all names nothing.
//
// 没有会话就没有对话要命名：池子也不问。
func TestASessionWithNoKeyHasNoUpstreamID(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})

	got, err := r.UpstreamID(context.Background(), SessionOf(nil, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("upstream id = %q", got)
	}
	if len(p.asked) != 0 {
		t.Fatalf("the pool was asked %v", p.asked)
	}
}

// TestTheUpstreamIDFailsWhenThePoolCannotBeAsked says a pool that has
// the id but cannot read it is an error.
//
// 读不到存着的 id 就不能装作没有：上游会把这段对话当成新的一段。
func TestTheUpstreamIDFailsWhenThePoolCannotBeAsked(t *testing.T) {
	p := newIDPool(acc("acc_01"))
	p.err = errors.New("the database is down")
	r := New(p, Options{MaxAttempts: 3})

	sess := SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "sys", "hello")
	if _, err := r.UpstreamID(context.Background(), sess); err == nil {
		t.Fatal("expected an error")
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

func TestDecideReadsTheQuotaWordsFromAPlainErrorField(t *testing.T) {
	// 站点也会这样答：error 是一个字符串，不是对象。那句话就是
	// 上游说的拒绝，配额的话要认。
	f := Failure{Status: 403, Body: []byte(`{"error":"insufficient_balance"}`)}
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

// TestDecideGivesUpOnAForbiddenThatIsNotAboutTheCredits says a 403
// that does not name the credits is the site's own no.
//
// 同一个账号、同一个令牌再问一次，问出来的还是这个答案。参考实现
// 是原样交给客户端的。
func TestDecideGivesUpOnAForbiddenThatIsNotAboutTheCredits(t *testing.T) {
	f := Failure{Status: 403, Body: []byte(`{"message":"forbidden"}`)}
	if got := Decide(f); got != ActionGiveUp {
		t.Fatalf("Decide = %v, want %v", got, ActionGiveUp)
	}
}

// TestDecideRetriesTheSameAccountOnARateLimit says a 429 that does not
// name the credits is a rate limit: wait, then ask the same account.
func TestDecideRetriesTheSameAccountOnARateLimit(t *testing.T) {
	f := Failure{Status: 429, Body: []byte(`{"message":"too many requests"}`)}
	if got := Decide(f); got != ActionRetrySame {
		t.Fatalf("Decide = %v, want %v", got, ActionRetrySame)
	}
}

// TestTheUpstreamMessageIsReadTheSameWay says the message the client
// is given is the message the decision was made on.
func TestTheUpstreamMessageIsReadTheSameWay(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"message":"insufficient balance"}`, "insufficient balance"},
		{`{"error":{"message":"insufficient balance"}}`, "insufficient balance"},
		{`{"error":"insufficient_balance"}`, "insufficient_balance"},
		{`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`, "insufficient balance"},
		{`{"nothing":"here"}`, ""},
	}
	for _, c := range cases {
		if got := UpstreamMessage([]byte(c.body)); got != c.want {
			t.Errorf("UpstreamMessage(%s) = %q, want %q", c.body, got, c.want)
		}
	}
}
```




- [ ] **Step 2: 写会话键与判定的测试**

这一步的测试已经在上面那份 `internal/rotator/rotator_test.go` 的整文件里——那份文件是最后的样子。照着整文件写一次即可，不用在这里再追加一遍。




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
//
// The upstream id in here is the derived one: the client's own session
// id when it sent one, and a hash of the first message when it did not.
// It is the fallback, used when the pool keeps no id of its own.
type Session struct {
	// Key is the account affinity key. Always a hash.
	Key string
	// UpstreamID is the derived session id, used when the pool keeps
	// none.
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
// The derived upstream id is the client's own when it sent one: it
// names the conversation, not the account, so it stays the same when
// the conversation moves to another account. It is what the upstream
// is told when the pool keeps no id of its own.
//
// When there is neither a header nor a message to hash, the session is
// empty and the request is handled by round robin.
//
// The derived id of a conversation that brought no header of its own is
// the hash of its first message. Two users whose first messages
// happen to be the same are one session by this design's own
// definition -- the session key is a hash of the same two things -- so
// they share one upstream id, and one upstream conversation, and one
// user's messages can turn up in another user's conversation. A kept
// id does not change that: it is kept per session key, and the two
// conversations have one key. Nothing in the request tells them
// apart, so no server can: the client has to send X-Session-Id. What
// a kept id does fix is that the upstream stops seeing the client's
// own string and a hash of the request's words, and that the mapping
// from the session to the upstream's conversation survives a restart.
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
//   - a 402 or a 403 that does not name the credits is the site saying
//     no: the answer goes to the client as it is, because asking the
//     same account with the same token again gives the same answer;
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
	case 402, 403:
		// 站点自己说不行：不是限流，也不是令牌旧了。同一个账号、
		// 同一个令牌再问一次，问出来的还是这个答案 —— 参考实现
		// 也是原样交给客户端，不重试。
		if spent {
			return ActionFailover
		}
		return ActionGiveUp
	case 429:
		if spent {
			return ActionFailover
		}
		// 限流：等一等，还是这个账号。
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

// UpstreamMessage reads the message out of an upstream body.
//
// It reads it the way the decision reads it: MiniMax's own shape
// first, then the plain shapes the site uses when it answers with a
// plain error. A caller that shows the upstream's words to the client
// reads them here, so that the words the client sees and the words the
// decision was made on are the same words.
func UpstreamMessage(body []byte) string {
	_, msg, _ := parseBusiness(body)
	return msg
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
// object, which are the shapes the site uses. An error field that is a
// string and not an object is the message itself: the site answers
// with a plain error that way. A field that is not a string says
// nothing.
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
		if s := rawString(raw); s != "" {
			return s
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
// Pool is the account pool the rotator picks from.
type Pool interface {
	// Accounts gives every account.
	Accounts(ctx context.Context) ([]store.Account, error)
	// Exhausted gives the windows that are spent.
	Exhausted(ctx context.Context) ([]store.Exhausted, error)
	// BindSession points a session at an account.
	BindSession(ctx context.Context, sessionKey, accountID string) error
	// SessionState gives the session's row in one read.
	//
	// 那一行的两列（钉在哪个号上、上游的 id）一起拿：分开问就是两次往返。
	SessionState(ctx context.Context, sessionKey string) (store.SessionState, error)
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

// UpstreamIDs is an optional part of a Pool: it gives the upstream id
// of a session.
//
// The id is the one MiniMax is told about. It belongs to the
// conversation and not to the account the conversation is bound to, so
// it does not change when the conversation moves to another account.
//
// A pool that has it is asked for the id it keeps; a pool without it is
// asked for nothing, and the id the request derives is used instead.
type UpstreamIDs interface {
	UpstreamID(ctx context.Context, sessionKey string) (string, error)
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
	// UpstreamID is the id the upstream knows this conversation by.
	//
	// 它是跟着那一行一起读出来的：那一行每个请求都要问一次，所以问出来的
	// 东西就跟着走，不用再问一遍。
	UpstreamID string
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
//   - a conversation bound to a busy account is answered by another
//     account, and the binding is left alone as well: a busy account is
//     not one that cannot answer;
//   - a conversation with no binding takes the least recently used
//     account, and the one with the most credits left when two were
//     used the same, and is bound to it;
//   - a request with no conversation at all rotates: the next account
//     in turn, so the load is spread.
//
// The accounts that are busy right now are not picked. When every
// account that could answer is busy, the error is ErrAllBusy: the
// caller waits for one of them to come free.
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
	usable, group := r.usableAccounts(accounts, exhausted, model, used)
	if len(usable) == 0 {
		return Plan{}, fmt.Errorf("no account can answer: every account is spent for the %s group or has already been tried", group)
	}
	// The accounts that are not busy: a request is in the air on the
	// other ones, and they are not picked.
	free, err := r.freeAccounts(ctx, usable)
	if err != nil {
		return Plan{}, err
	}

	sticky := r.opts.Strategy != StrategyRoundRobin

	// 这一段对话在库里那一行：钉在哪个号上、上游的 id。
	//
	// 那一行以前是分开读的（两次往返），而每个请求都要问它一次。
	var state store.SessionState
	if sessionKey != "" {
		var err error
		state, err = r.pool.SessionState(ctx, sessionKey)
		if err != nil {
			// 读不出来不是「没绑定」：当成没绑定的话，库里那个绑定会被覆
			// 盖掉。
			return Plan{}, fmt.Errorf("the session row could not be read: %w", err)
		}
	}

	// 上游的 id：那一行里的那个。
	//
	// 那一行没有 id（新的对话）：问池子要一个 —— 它会编一个写下去。
	//
	// 只有「那一行还没有 id」的时候才问：问一次就是一次写，而热路径上那一
	// 行已经有 id 了（那一行每个请求都要读一次，所以问出来的东西就跟着
	// 走，不用再问一遍）。
	upstreamID := state.UpstreamID
	if upstreamID == "" && sessionKey != "" {
		if ids, ok := r.pool.(UpstreamIDs); ok {
			var err error
			upstreamID, err = ids.UpstreamID(ctx, sessionKey)
			if err != nil {
				return Plan{}, fmt.Errorf("the upstream id could not be read: %w", err)
			}
		}
	}

	if sticky && sessionKey != "" {
		if state.AccountID != "" {
			boundID := state.AccountID
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
				a, err := r.bestAccount(ctx, free)
				if err != nil {
					return Plan{}, err
				}
				r.markUsed(a.ID)
				return r.plan(a, group, false, true, upstreamID), nil
			case r.isBusy(ctx, boundID):
				// The bound account is busy: another account answers
				// this request, and the conversation stays where it
				// is. A busy account is not one that cannot answer.
				a, err := r.bestAccount(ctx, free)
				if err != nil {
					return Plan{}, err
				}
				r.markUsed(a.ID)
				return r.plan(a, group, false, true, upstreamID), nil
			default:
				// The bound account can answer: it answers.
				r.markUsed(boundID)
				return r.plan(store.Account{ID: boundID}, group, false, false, upstreamID), nil
			}
		}
	}

	// No binding, or the bound account cannot answer: another account
	// is picked and the conversation is bound to it.
	if sticky && sessionKey != "" {
		a, err := r.bestAccount(ctx, free)
		if err != nil {
			return Plan{}, err
		}
		if err := r.pool.BindSession(ctx, sessionKey, a.ID); err != nil {
			// The binding was not written. The caller must not be
			// told the conversation has moved: it has not.
			return Plan{}, fmt.Errorf("the session binding could not be written: %w", err)
		}
		r.markUsed(a.ID)
		return r.plan(a, group, true, false, upstreamID), nil
	}

	// No conversation, or the round-robin strategy: the next account
	// in the rotation.
	usableIDs := make(map[string]bool, len(free))
	for _, a := range free {
		usableIDs[a.ID] = true
	}
	a, err := r.nextAccount(accounts, usableIDs)
	if err != nil {
		return Plan{}, err
	}
	r.markUsed(a.ID)
	return r.plan(a, group, false, false, upstreamID), nil
}

// plan gives the plan of a request that goes to an account.
func (r *Rotator) plan(a store.Account, group string, rebound, retried bool, upstreamID string) Plan {
	return Plan{
		AccountID:  a.ID,
		WindowKey:  group,
		GroupKey:   group,
		Rebound:    rebound,
		Retried:    retried,
		UpstreamID: upstreamID,
	}
}

// UpstreamID gives the id the upstream is told about for a session.
//
// The pool's id is used when the pool has one: it is kept in the
// database, so it is the same for every request of the conversation,
// and it survives a restart.
//
// A pool that cannot supply one is asked for nothing, and the derived
// id the session carries is used: the client's own session id when it
// sent one, and the hash of the first message when it did not.
//
// A session with no key names no conversation: the id is empty and the
// upstream is told nothing.
//
// A pool that has the id but cannot read it is an error. The derived id
// would be a working id, but it is not the one the conversation was
// sent with before, and the upstream would take the conversation for a
// new one. Saying so is better than that.
func (r *Rotator) UpstreamID(ctx context.Context, sess Session) (string, error) {
	if sess.Key == "" {
		// 没有会话就没有对话要命名：池子也不问，空的会话行不该有。
		return "", nil
	}
	pool, ok := r.pool.(UpstreamIDs)
	if !ok {
		// 这个池子没有存着的上游 id：退回请求自己算出来的那个。
		return sess.UpstreamID, nil
	}
	id, err := pool.UpstreamID(ctx, sess.Key)
	if err != nil {
		return "", fmt.Errorf("the upstream id could not be read: %w", err)
	}
	if id == "" {
		return sess.UpstreamID, nil
	}
	return id, nil
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

// bestAccount gives the account that was used least recently, and the
// one with the most credits left when two were used the same.
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
//
// 挑号的规矩，从要紧的到不要紧的：
//
//  1. 读到余额的排前面 —— 那是能拿来比较的数字。没读到的不算零，它排在
//     有数的号后面（规格不变量 2）。
//  2. 读到「0」的排到有数的号后面 —— 它接不了活了，挑它就是撞一次空，
//     撞空要一个上游请求，还占着并发。
//  3. 最久没用过的排前面 —— 把用量摊开：别让一个号被别的号挤在后面一直
//     闲着，到头来被一口气烧光（规格 §6.5）。
//  4. 一样的话余额多的排前面。
//  5. 再一样就按 ID 排 —— 稳定：同样的情况挑出同一个号来。
func better(a, b scored) bool {
	if a.known != b.known {
		// 读到余额的排前面：那是能拿来比较的数字
		return a.known
	}
	if a.known {
		aEmpty := a.left <= 0
		bEmpty := b.left <= 0
		if aEmpty != bEmpty {
			// 读到 0 的排后面：它撞空是必然的。
			return bEmpty
		}
	}
	if !a.usedAt.Equal(b.usedAt) {
		// 最久没用过的排前面
		return a.usedAt.Before(b.usedAt)
	}
	if a.known && a.left != b.left {
		return a.left > b.left
	}
	return a.acc.ID < b.acc.ID
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
			bestAt = at
			candidates = []int{i}
			found = true
		case at.Equal(bestAt):
			candidates = append(candidates, i)
		}
	}
	if !found {
		return store.Account{}, fmt.Errorf("no account can answer")
	}

	idx := candidates[0]
	if len(candidates) > 1 {
		start := r.cursor % len(all)
		for _, c := range candidates {
			if c >= start {
				idx = c
				break
			}
		}
	}
	r.cursor = idx + 1
	// 给出去的就是游标挑的那个：候选都是同一个时刻，差别只在游标。
	return all[idx], nil
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
	"fmt"
	"reflect"
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

// TestAnEmptyTextBlockStillHasItsText says a text block keeps its
// text field even when the text is empty.
//
// Anthropic wants the field: a block that says only "type":"text" is
// refused. And a block that carries a field it does not know is
// refused too, so the text block carries nothing else.
func TestAnEmptyTextBlockStillHasItsText(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	block := m["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if _, ok := block["text"]; !ok {
		t.Fatalf("block = %v, want a text field", block)
	}
	if block["text"] != "" {
		t.Fatalf("block = %v, want an empty text", block)
	}
	if len(block) != 2 {
		t.Fatalf("block = %v, want only the type and the text", block)
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

// TestStreamEmpty: an empty stream is a stream that ended before the
// answer was done. The client has to be able to tell that from a
// whole answer, so the conversion fails and no response.completed is
// written.
func TestStreamEmpty(t *testing.T) {
	var out strings.Builder
	err := StreamResponses(strings.NewReader(""), &out, "m")
	if err == nil {
		t.Fatal("expected an error: the stream ended early")
	}
	got := out.String()
	if !strings.Contains(got, "response.created") {
		t.Fatalf("the stream has no created event:\n%s", got)
	}
	if strings.Contains(got, "response.completed") {
		t.Fatalf("the stream says it completed:\n%s", got)
	}
}

// events reads the events out of a Responses stream.
func events(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(s, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("the event's data is not JSON: %v", err)
			}
			out = append(out, m)
		}
	}
	return out
}

// eventTypes gives the type of every event of a stream.
func eventTypes(s string) []string {
	var out []string
	for _, block := range strings.Split(s, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var m map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
					out = append(out, fmt.Sprint(m["type"]))
				}
			}
		}
	}
	return out
}

// Finding 1: an upstream error must not be swallowed. The client gets
// a failure event and the conversion gives an error back, so that the
// gateway's own handling of a stream that ended early fires too.
func TestStreamUpstreamErrorFails(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "an error event",
			sse: "event: error\n" +
				`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n",
		},
		{
			name: "an error field",
			sse: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}` + "\n\n" +
				"event: error\n" +
				`data: {"type":"error","error":{"message":"the account is not usable"}}` + "\n\n",
		},
		{
			name: "a statusInfo code",
			sse: "event: message_start\n" +
				`data: {"type":"message_start","statusInfo":{"code":1004,"message":"account error"}}` + "\n\n",
		},
		{
			name: "a base_resp code",
			sse: "event: message_start\n" +
				`data: {"type":"message_start","base_resp":{"status_code":1002,"status_msg":"bad request"}}` + "\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			err := StreamResponses(strings.NewReader(tc.sse), &out, "m")
			if err == nil {
				t.Fatal("expected an error: the upstream said the answer failed")
			}
			got := out.String()
			if strings.Contains(got, "response.completed") {
				t.Fatalf("the stream says it completed:\n%s", got)
			}
			if !strings.Contains(got, "response.failed") {
				t.Fatalf("the stream has no failure event:\n%s", got)
			}
			// The message of the upstream has to reach the client.
			for _, ev := range events(t, got) {
				if ev["type"] != "response.failed" {
					continue
				}
				resp := ev["response"].(map[string]any)
				errObj, ok := resp["error"].(map[string]any)
				if !ok {
					t.Fatalf("the failure has no error: %v", resp)
				}
				if msg := fmt.Sprint(errObj["message"]); msg == "" {
					t.Fatalf("the failure has no message: %v", errObj)
				}
			}
		})
	}
}

// Finding 2: a stream that ends early is not a whole answer.
func TestStreamTruncatedIsNotComplete(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half an ans"}}` + "\n\n"

	var out strings.Builder
	err := StreamResponses(strings.NewReader(in), &out, "m")
	if err == nil {
		t.Fatal("expected an error: the stream ended before the answer was done")
	}
	got := out.String()
	if strings.Contains(got, "response.completed") {
		t.Fatalf("the stream says it completed:\n%s", got)
	}
	if !strings.Contains(got, "response.failed") {
		t.Fatalf("the stream has no failure event:\n%s", got)
	}
	// The text that did arrive is still there.
	if !strings.Contains(got, `"delta":"half an ans"`) {
		t.Fatalf("the text is missing:\n%s", got)
	}
}

// Finding 3: the response id belongs to the Responses protocol, is
// stable inside one response and is not the upstream's message id.
func TestStreamResponseID(t *testing.T) {
	run := func() string {
		var out strings.Builder
		if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first := run()
	second := run()

	idOf := func(s string) string {
		for _, ev := range events(t, s) {
			if ev["type"] == "response.created" {
				return fmt.Sprint(ev["response"].(map[string]any)["id"])
			}
		}
		t.Fatalf("the stream has no created event:\n%s", s)
		return ""
	}
	id, other := idOf(first), idOf(second)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("the id is not a Responses id: %q", id)
	}
	if id == other {
		t.Fatalf("two responses have the same id: %q", id)
	}
	// The upstream's message id must not be used.
	if strings.Contains(id, "msg_") {
		t.Fatalf("the id is the upstream's message id: %q", id)
	}
	// One response keeps one id.
	for _, ev := range events(t, first) {
		resp, ok := ev["response"].(map[string]any)
		if !ok {
			continue
		}
		if got := fmt.Sprint(resp["id"]); got != id {
			t.Fatalf("the events of one response have different ids: %q and %q", id, got)
		}
	}
}

// Finding 4: a block the Responses protocol cannot show does not
// become an empty message, and it leaves no item without an end.
func TestStreamSkipsUnknownContentBlock(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"image","source":{"type":"base64","data":"xx"}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// Every item that was added is also done.
	added := map[string]bool{}
	done := map[string]bool{}
	for _, ev := range events(t, got) {
		switch ev["type"] {
		case "response.output_item.added":
			added[fmt.Sprint(ev["item"].(map[string]any)["id"])] = true
		case "response.output_item.done":
			done[fmt.Sprint(ev["item"].(map[string]any)["id"])] = true
		}
	}
	if len(added) == 0 {
		t.Fatalf("the stream has no item:\n%s", got)
	}
	for id := range added {
		if !done[id] {
			t.Fatalf("the item %s was added but never done:\n%s", id, got)
		}
	}
	// The picture did not become an empty message.
	for _, ev := range events(t, got) {
		if ev["type"] != "response.output_text.done" {
			continue
		}
		if fmt.Sprint(ev["text"]) == "" {
			t.Fatalf("an empty text was written:\n%s", got)
		}
	}
	// The completed answer has only the message.
	for _, ev := range events(t, got) {
		if ev["type"] != "response.completed" {
			continue
		}
		out := ev["response"].(map[string]any)["output"].([]any)
		if len(out) != 1 {
			t.Fatalf("the answer has %d items: %v", len(out), out)
		}
		if fmt.Sprint(out[0].(map[string]any)["type"]) != "message" {
			t.Fatalf("the item is not a message: %v", out[0])
		}
	}
}

// Finding 5: the tool choice comes across.
func TestToAnthropicToolChoice(t *testing.T) {
	cases := []struct {
		name string
		give string
		want string // the Anthropic tool_choice, "" for none
	}{
		{name: "auto", give: `"auto"`, want: `{"type":"auto"}`},
		{name: "none", give: `"none"`, want: `{"type":"none"}`},
		{name: "required becomes any", give: `"required"`, want: `{"type":"any"}`},
		{name: "a named function", give: `{"type":"function","name":"get_weather"}`, want: `{"type":"tool","name":"get_weather"}`},
		{name: "a name that is not known", give: `"maybe"`, want: ""},
		{name: "a function without a name", give: `{"type":"function"}`, want: ""},
		{name: "a shape that is not known", give: `{"type":"allowed_tools","mode":"auto"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"tool_choice":` + tc.give + `}`)
			out, err := ToAnthropic(in)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			got, ok := m["tool_choice"]
			if tc.want == "" {
				if ok {
					t.Fatalf("the tool choice should have been left out: %v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("the tool choice is missing: %s", out)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			// The names of the fields are not what matters here, only
			// what they say.
			var have, want map[string]any
			if err := json.Unmarshal(b, &have); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(have, want) {
				t.Fatalf("tool_choice = %s, want %s", b, tc.want)
			}
		})
	}
}

// Finding 6, request side: the arguments of a tool call have to be a
// JSON object on the Anthropic side.
func TestToAnthropicToolArguments(t *testing.T) {
	cases := []struct {
		name    string
		give    string
		want    string
		wantErr bool
	}{
		{name: "a JSON object as a string", give: `"{\"city\":\"SF\"}"`, want: `{"city":"SF"}`},
		{name: "an object", give: `{"city":"SF"}`, want: `{"city":"SF"}`},
		{name: "no arguments", give: `""`, want: `{}`},
		{name: "text that is not JSON", give: `"SF"`, wantErr: true},
		{name: "JSON that is not an object", give: `"[1,2]"`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"model":"m","input":[{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":` + tc.give + `}]}`)
			out, err := ToAnthropic(in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %s", out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			msgs := m["messages"].([]any)
			block := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			input, ok := block["input"].(map[string]any)
			if !ok {
				t.Fatalf("the tool input is not an object: %v", block)
			}
			b, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Fatalf("input = %s, want %s", b, tc.want)
			}
		})
	}
}

// Finding 6, stream side: the arguments of the finished tool call are
// one JSON string.
func TestStreamFunctionCallArgumentsAreOneString(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":":\"SF\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	var args string
	for _, ev := range events(t, got) {
		if ev["type"] != "response.output_item.done" {
			continue
		}
		item := ev["item"].(map[string]any)
		if fmt.Sprint(item["type"]) != "function_call" {
			continue
		}
		s, ok := item["arguments"].(string)
		if !ok {
			t.Fatalf("the arguments are not a string: %v", item)
		}
		args = s
	}
	if args == "" {
		t.Fatalf("the finished tool call has no arguments:\n%s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("the arguments are not one JSON string: %q: %v", args, err)
	}
	if parsed["city"] != "SF" {
		t.Fatalf("the arguments are wrong: %q", args)
	}
}

// Finding 7: the completed answer carries the items that were sent and
// uses the names of the Responses protocol.
func TestStreamCompletedPayload(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, ev := range events(t, got) {
		if ev["type"] != "response.completed" {
			continue
		}
		resp := ev["response"].(map[string]any)
		if _, ok := resp["output"].([]any); !ok {
			t.Fatalf("the completed answer has no output: %v", resp)
		}
		items := resp["output"].([]any)
		if len(items) != 2 {
			t.Fatalf("the answer has %d items: %v", len(items), items)
		}
		if fmt.Sprint(items[0].(map[string]any)["type"]) != "reasoning" {
			t.Fatalf("the first item is not a reasoning: %v", items[0])
		}
		if fmt.Sprint(items[1].(map[string]any)["type"]) != "message" {
			t.Fatalf("the second item is not a message: %v", items[1])
		}
		if resp["output_text"] != "Hello" {
			t.Fatalf("output_text = %v", resp["output_text"])
		}
		// The names of the Anthropic protocol have no place here.
		for _, key := range []string{"stop_reason", "content", "role"} {
			if _, ok := resp[key]; ok {
				t.Fatalf("the completed answer has the Anthropic field %q: %v", key, resp)
			}
		}
		return
	}
	t.Fatalf("the stream has no completed event:\n%s", got)
}

// Finding 8: every event says where it stands in the stream.
func TestStreamSequenceNumbers(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for i, ev := range events(t, got) {
		seq, ok := ev["sequence_number"].(float64)
		if !ok {
			t.Fatalf("the event %d has no sequence_number: %v", i, ev)
		}
		if int(seq) != i {
			t.Fatalf("the event %d has the sequence number %v", i, seq)
		}
	}
}

// Finding 9: a data field may be continued over several lines.
func TestStreamMultiLineData(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{
			// The shape the SSE specification asks for: every line
			// carries the data field, and the lines belong to one
			// payload.
			name: "the data field is written again",
			in: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,` + "\n" +
				`data: "content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,` + "\n" +
				`data: "delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		},
		{
			// The shape some upstreams send: only the first line of
			// the payload carries the data field.
			name: "the payload is spread over the lines",
			in: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,` + "\n" +
				`"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,` + "\n" +
				`"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if err := StreamResponses(strings.NewReader(tc.in), &out, "m"); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, `"delta":"Hello"`) {
				t.Fatalf("the text is missing:\n%s", got)
			}
			if !strings.Contains(got, "response.completed") {
				t.Fatalf("the stream did not finish:\n%s", got)
			}
		})
	}
}

// Finding 9: a piece of the stream that cannot be read must not leave
// the client with a false "completed".
func TestStreamMalformedPayloadIsNotComplete(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	err := StreamResponses(strings.NewReader(in), &out, "m")
	if err == nil {
		t.Fatal("expected an error: a piece of the stream could not be read")
	}
	got := out.String()
	if strings.Contains(got, "response.completed") {
		t.Fatalf("the stream says it completed:\n%s", got)
	}
	if !strings.Contains(got, "response.failed") {
		t.Fatalf("the stream has no failure event:\n%s", got)
	}
}

// Finding 10: a message with no text in it is not an empty answer.
func TestStreamEmptyTextBlockIsNotAMessage(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, ev := range events(t, got) {
		switch ev["type"] {
		case "response.output_text.done", "response.content_part.done":
			if fmt.Sprint(ev["text"]) == "" {
				t.Fatalf("an empty text was written:\n%s", got)
			}
		case "response.output_item.added", "response.output_item.done":
			item := ev["item"].(map[string]any)
			if fmt.Sprint(item["type"]) != "message" {
				continue
			}
			content, ok := item["content"].([]any)
			if !ok {
				continue
			}
			for _, part := range content {
				if fmt.Sprint(part.(map[string]any)["text"]) == "" {
					t.Fatalf("an empty message was written:\n%s", got)
				}
			}
		}
	}
	// The answer has no item at all: the model said nothing.
	for _, ev := range events(t, got) {
		if ev["type"] != "response.completed" {
			continue
		}
		items := ev["response"].(map[string]any)["output"].([]any)
		if len(items) != 0 {
			t.Fatalf("the answer has %d items: %v", len(items), items)
		}
	}
}

// The events of a stream keep their order.
func TestStreamEventOrder(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := eventTypes(out.String())
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(got) != len(want) {
		t.Fatalf("the events are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the events are %v, want %v", got, want)
		}
	}
}
```

这一步的测试已经在上面那份 `internal/convert/responses_test.go` 的整文件里——那份文件是最后的样子。照着整文件写一次即可，不用在这里再追加一遍。

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
//
// Two rules hold for the whole package:
//
//   - A piece of the answer that the Responses protocol has no shape
//     for is left out, not faked. An Anthropic image block, a document
//     or a tool result has no output item of its own in the Responses
//     protocol, so it gets no item at all. Turning it into an empty
//     message would tell the client that the model said nothing,
//     which is not what happened.
//
//   - A tool call's arguments have to be a JSON object on the
//     Anthropic side and a JSON string on the Responses side.
//     Arguments that are neither are refused rather than turned into
//     an empty object: an empty object is a tool call with arguments
//     the caller never wrote.
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responsesRequest is the part of a Responses request that is read.
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
	Tools           []responsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
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
	Model      string               `json:"model"`
	System     string               `json:"system,omitempty"`
	Messages   []anthropicMessage   `json:"messages"`
	MaxTokens  int                  `json:"max_tokens"`
	Stream     bool                 `json:"stream,omitempty"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking   *thinking            `json:"thinking,omitempty"`
	// StopSequences are the sequences that end the answer. The
	// Responses protocol has no such field, so it is only filled in
	// when the request came from the OpenAI protocol.
	StopSequences []string `json:"stop_sequences,omitempty"`
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

// MarshalJSON writes the block the way Anthropic wants it.
//
// Anthropic wants the field every block has, even when the field is
// empty: a text block without a "text" is refused. And a block that
// carries a field it does not know is refused too, so each kind writes
// only its own fields — a text block never carries an id.
func (b anthropicBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "tool_use":
		input := b.Input
		if len(input) == 0 {
			// Anthropic wants an object here, not nothing.
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{b.Type, b.ID, b.Name, input})
	case "tool_result":
		return json.Marshal(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
		}{b.Type, b.ToolUseID, b.Content})
	default:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text})
	}
}

// anthropicTool is one tool of an Anthropic request.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice is Anthropic's tool_choice.
//
// Anthropic always wants an object, even for the choices that the
// Responses protocol writes as a plain string.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// thinking is the Anthropic thinking setting.
type thinking struct {
	Type string `json:"type"`
}

// ToAnthropic turns a Responses request into an Anthropic Messages
// request.
//
// The model, the messages, the tools, the tool choice and the
// streaming flag all come across. The Responses protocol's tool calls
// and tool results become Anthropic's tool_use and tool_result blocks.
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
	out.ToolChoice = toolChoice(in.ToolChoice)
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

// toolChoice turns the Responses tool_choice into Anthropic's.
//
// The Responses protocol says "required" where Anthropic says "any",
// and it names a function where Anthropic names a tool. A choice that
// has no counterpart is left out: a different choice would silently
// change what the model is allowed to do.
func toolChoice(raw json.RawMessage) *anthropicToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &anthropicToolChoice{Type: "auto"}
		case "none":
			return &anthropicToolChoice{Type: "none"}
		case "required":
			return &anthropicToolChoice{Type: "any"}
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Type == "function" && obj.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: obj.Name}
	}
	return nil
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
				CallID    string          `json:"call_id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(mustMarshal(item), &fc); err != nil {
				return nil, err
			}
			input, err := toolInput(fc.Arguments)
			if err != nil {
				return nil, err
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

// toolInput turns the arguments of a Responses tool call into the
// input of an Anthropic tool_use.
//
// Anthropic wants an object and the Responses protocol carries the
// arguments as a JSON string. Arguments that are not a JSON object
// cannot be used, so they are refused: an empty object would be a
// tool call with arguments the caller never wrote.
func toolInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			// 没有参数的工具就是这样写的。
			return json.RawMessage(`{}`), nil
		}
		if _, err := jsonObject([]byte(s)); err != nil {
			return nil, err
		}
		return json.RawMessage(s), nil
	}
	if _, err := jsonObject(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// jsonObject says whether a piece of JSON is an object.
func jsonObject(raw []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("responses: the tool call's arguments are not a JSON object")
	}
	if obj == nil {
		// "null" reads as an object with nothing in it, but it is
		// not one.
		return nil, fmt.Errorf("responses: the tool call's arguments are not a JSON object")
	}
	return obj, nil
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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
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
	// Error is the error an error event carries.
	Error json.RawMessage `json:"error"`
	// StatusInfo and BaseResp are MiniMax's own ways of saying that
	// the answer failed inside a 200.
	StatusInfo *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	} `json:"statusInfo"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// blockState is what is known about one upstream content block.
type blockState struct {
	// outputIndex is the item's index in the Responses output.
	outputIndex int
	// kind is "message", "reasoning", "function_call" or "skipped".
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
	// input is the tool input the upstream gave with the block, for
	// the case that it gives the whole arguments at once.
	input json.RawMessage
	// started says whether the item's events have gone out. A block
	// that says nothing gets no item at all: an empty message is
	// worse than no message.
	started bool
	// finished says whether the item's done event has gone out.
	finished bool
}

// converter turns the upstream's stream into a Responses stream.
type converter struct {
	bw    *bufio.Writer
	model string

	// responseID is the id of this response. It is made here and not
	// taken from the upstream: the upstream's message id is not a
	// response id, and two requests must not get the same one.
	responseID string

	// seq counts the events, so that every one of them can say where
	// it stands in the stream.
	seq int

	// blocks is what is known about each upstream content block.
	blocks map[int]*blockState
	// nextIndex is the index the next output item gets.
	nextIndex int
	// items holds the finished output items by their index.
	items map[int]any

	text      strings.Builder
	reasoning strings.Builder

	inputTokens  int
	outputTokens int

	// sawStop says the upstream's stream ended the way it should.
	sawStop bool

	// failed says the answer has already been reported as failed.
	failed bool
}

// StreamResponses turns the upstream's Anthropic stream into a
// Responses stream.
//
// The events are written as they come, so a client that is waiting for
// the first token gets it as soon as the upstream sends it.
//
// A stream that fails, or that ends before the answer is done, gets a
// response.failed and no response.completed: the client has to be able
// to tell a whole answer from a half one.
func StreamResponses(r io.Reader, w io.Writer, model string) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	c := &converter{
		bw:         bw,
		model:      model,
		responseID: randomID("resp"),
		blocks:     map[int]*blockState{},
		items:      map[int]any{},
	}
	return c.run(r)
}

// run reads the upstream's stream and writes the Responses stream.
func (c *converter) run(r io.Reader) error {
	if err := c.emit("response.created", map[string]any{
		"response": map[string]any{
			"id": c.responseID, "object": "response", "model": c.model,
			"status": "in_progress", "output": []any{},
		},
	}); err != nil {
		return err
	}

	// broken holds the first piece of the stream that could not be
	// read. The stream goes on, but it does not end as a whole answer
	// any more.
	var broken error
	err := readSSE(r, func(name, payload string) error {
		payload = strings.TrimSpace(payload)
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			// 有些上游用 [DONE] 收尾，它也是一个像样的结尾。
			c.sawStop = true
			return nil
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			if name == "error" {
				// 上游说了它自己错了，哪怕这一块不是 JSON。
				return c.fail("upstream_error", payload)
			}
			// 认不出的那一块不掐断整条流，但它也不会被当作
			// 什么都没发生：这一条流不再是完整的答案。
			if broken == nil {
				broken = fmt.Errorf("the upstream sent a piece of the stream that could not be read: %w", err)
			}
			return nil
		}
		if code, msg, bad := upstreamError(name, &ev); bad {
			return c.fail(code, msg)
		}
		if ev.Type == "" && name != "" {
			// 有些上游在 event: 那一行写了名字，数据里没有写。
			ev.Type = name
		}
		return c.event(&ev)
	})
	if err != nil {
		var fe *failedError
		if errors.As(err, &fe) {
			// 已经报过了
			return err
		}
		return c.fail("upstream_error", err.Error())
	}
	if broken != nil {
		return c.fail("upstream_error", broken.Error())
	}
	if !c.sawStop {
		return c.fail("upstream_error", "the upstream's stream ended before the answer was done")
	}
	return c.complete()
}

// event takes one event of the upstream's stream.
func (c *converter) event(ev *anthropicEvent) error {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			c.inputTokens = ev.Message.Usage.InputTokens
		}
	case "content_block_start":
		return c.startBlock(ev)
	case "content_block_delta":
		return c.blockDelta(ev)
	case "content_block_stop":
		return c.stopBlock(ev)
	case "message_delta":
		if ev.Usage != nil {
			c.outputTokens = ev.Usage.OutputTokens
		}
	case "message_stop":
		c.sawStop = true
	}
	return nil
}

// startBlock takes the opening of one upstream content block.
//
// The blocks the Responses protocol cannot show are left out. They get
// no item at all: an empty message in their place would tell the
// client that the model said nothing, which is not what happened.
func (c *converter) startBlock(ev *anthropicEvent) error {
	if ev.ContentBlock == nil {
		return nil
	}
	st := &blockState{input: ev.ContentBlock.Input}
	switch ev.ContentBlock.Type {
	case "text":
		st.kind = "message"
	case "thinking":
		st.kind = "reasoning"
	case "tool_use":
		st.kind = "function_call"
		st.callID = ev.ContentBlock.ID
		st.name = ev.ContentBlock.Name
	default:
		// 图片、文档、工具结果之类的块在 Responses 的输出里没有
		// 对应的形状，跳过：它们不会变成一个空的消息块。
		st.kind = "skipped"
		c.blocks[ev.Index] = st
		return nil
	}
	c.blocks[ev.Index] = st
	switch st.kind {
	case "function_call":
		// 一次工具调用本身就是一件事，不用等它的参数。
		return c.startItem(st)
	case "message":
		// 有些上游把第一段话直接放在块的开头。
		if ev.ContentBlock.Text != "" {
			return c.addText(st, ev.ContentBlock.Text)
		}
	case "reasoning":
		if ev.ContentBlock.Thinking != "" {
			return c.addThinking(st, ev.ContentBlock.Thinking)
		}
	}
	return nil
}

// startItem writes the opening of one output item.
//
// The item is only started when the block actually says something: an
// item with no text in it would be an empty assistant message, and a
// client cannot tell that from a real one.
func (c *converter) startItem(st *blockState) error {
	if st.started || st.kind == "skipped" {
		return nil
	}
	st.started = true
	st.outputIndex = c.nextIndex
	c.nextIndex++
	switch st.kind {
	case "message":
		st.itemID = randomID("msg")
		if err := c.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIndex,
			"item": map[string]any{
				"type": "message", "id": st.itemID, "role": "assistant",
				"status": "in_progress", "content": []any{},
			},
		}); err != nil {
			return err
		}
		return c.emit("response.content_part.added", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	case "reasoning":
		st.itemID = randomID("rs")
		if err := c.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIndex,
			"item": map[string]any{
				"type": "reasoning", "id": st.itemID, "summary": []any{},
			},
		}); err != nil {
			return err
		}
		return c.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	case "function_call":
		st.itemID = randomID("fc")
		return c.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIndex,
			"item": map[string]any{
				"type": "function_call", "id": st.itemID,
				"call_id": st.callID, "name": st.name, "arguments": "",
				"status": "in_progress",
			},
		})
	}
	return nil
}

// addText adds a piece of text to a message item.
func (c *converter) addText(st *blockState, text string) error {
	if err := c.startItem(st); err != nil {
		return err
	}
	st.text += text
	c.text.WriteString(text)
	return c.emit("response.output_text.delta", map[string]any{
		"item_id": st.itemID, "output_index": st.outputIndex,
		"content_index": 0, "delta": text,
	})
}

// addThinking adds a piece of thinking to a reasoning item.
func (c *converter) addThinking(st *blockState, thinking string) error {
	if err := c.startItem(st); err != nil {
		return err
	}
	st.text += thinking
	c.reasoning.WriteString(thinking)
	return c.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": st.itemID, "output_index": st.outputIndex,
		"summary_index": 0, "delta": thinking,
	})
}

// blockDelta takes one piece of an upstream content block.
func (c *converter) blockDelta(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok || ev.Delta == nil {
		return nil
	}
	switch ev.Delta.Type {
	case "text_delta":
		if st.kind != "message" || ev.Delta.Text == "" {
			return nil
		}
		return c.addText(st, ev.Delta.Text)
	case "thinking_delta":
		if st.kind != "reasoning" || ev.Delta.Thinking == "" {
			return nil
		}
		return c.addThinking(st, ev.Delta.Thinking)
	case "input_json_delta":
		if st.kind != "function_call" {
			return nil
		}
		if err := c.startItem(st); err != nil {
			return err
		}
		st.args += ev.Delta.PartialJSON
		return c.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"delta": ev.Delta.PartialJSON,
		})
	}
	return nil
}

// stopBlock takes the end of one upstream content block.
func (c *converter) stopBlock(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok {
		return nil
	}
	return c.finishBlock(st, "completed")
}

// finishBlock writes the end of one output item.
//
// A block that was never started has no item and gets no end either:
// there is nothing to close.
func (c *converter) finishBlock(st *blockState, status string) error {
	if st.finished {
		return nil
	}
	st.finished = true
	if !st.started {
		return nil
	}
	switch st.kind {
	case "message":
		if err := c.emit("response.output_text.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"content_index": 0, "text": st.text,
		}); err != nil {
			return err
		}
		if err := c.emit("response.content_part.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": st.text, "annotations": []any{}},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "message", "id": st.itemID, "role": "assistant",
			"status": status,
			"content": []any{map[string]any{
				"type": "output_text", "text": st.text, "annotations": []any{},
			}},
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"output_index": st.outputIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items[st.outputIndex] = item
	case "function_call":
		args := st.args
		if args == "" {
			// 参数是一次给全的，或者根本没有参数。
			args = toolArguments(st.input)
		}
		if err := c.emit("response.function_call_arguments.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"arguments": args,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "function_call", "id": st.itemID,
			"call_id": st.callID, "name": st.name, "arguments": args,
			"status": status,
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"output_index": st.outputIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items[st.outputIndex] = item
	case "reasoning":
		if err := c.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": st.text},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "reasoning", "id": st.itemID,
			"summary": []any{map[string]any{"type": "summary_text", "text": st.text}},
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"output_index": st.outputIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items[st.outputIndex] = item
	}
	return nil
}

// closeBlocks ends the blocks that were still open.
//
// A stream that breaks in the middle leaves items that were started
// but never ended. They are closed here, so that the client does not
// wait for a done event that will never come.
func (c *converter) closeBlocks() error {
	idx := make([]int, 0, len(c.blocks))
	for i := range c.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		if err := c.finishBlock(c.blocks[i], "incomplete"); err != nil {
			return err
		}
	}
	return nil
}

// output gives the finished items, in the order of the output.
func (c *converter) output() []any {
	idx := make([]int, 0, len(c.items))
	for i := range c.items {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]any, 0, len(idx))
	for _, i := range idx {
		out = append(out, c.items[i])
	}
	return out
}

// complete writes the end of a whole answer.
func (c *converter) complete() error {
	return c.emit("response.completed", map[string]any{
		"response": map[string]any{
			"id": c.responseID, "object": "response", "model": c.model,
			"status":                 "completed",
			"output":                 c.output(),
			"output_text":            c.text.String(),
			"reasoning_summary_text": c.reasoning.String(),
			"usage": map[string]any{
				"input_tokens": c.inputTokens, "output_tokens": c.outputTokens,
				"total_tokens": c.inputTokens + c.outputTokens,
			},
		},
	})
}

// failedError is the error of a stream that could not be finished.
type failedError struct {
	code string
	msg  string
}

func (e *failedError) Error() string { return e.msg }

// fail writes the failure of the answer and gives the error back.
//
// A failed answer gets a response.failed and no response.completed:
// the client has to know that the answer is not there.
func (c *converter) fail(code, msg string) error {
	if code == "" {
		code = "upstream_error"
	}
	if c.failed {
		// 答案已经报过一次失败了，不再报第二次。
		return &failedError{code: code, msg: msg}
	}
	c.failed = true
	if err := c.closeBlocks(); err != nil {
		return err
	}
	if err := c.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id": c.responseID, "object": "response", "model": c.model,
			"status": "failed",
			"error":  map[string]any{"code": code, "message": msg},
			"output": c.output(),
			"usage": map[string]any{
				"input_tokens": c.inputTokens, "output_tokens": c.outputTokens,
				"total_tokens": c.inputTokens + c.outputTokens,
			},
		},
	}); err != nil {
		return err
	}
	return &failedError{code: code, msg: msg}
}

// emit writes one Responses event.
func (c *converter) emit(event string, payload map[string]any) error {
	payload["type"] = event
	payload["sequence_number"] = c.seq
	c.seq++
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.bw, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	return c.bw.Flush()
}

// upstreamError says what is wrong with an upstream event, if anything.
//
// MiniMax can put a business error inside a 200 answer, and it can be
// in the stream as well: as an error event, as an error field, or in
// one of MiniMax's own status fields.
func upstreamError(name string, ev *anthropicEvent) (code, msg string, bad bool) {
	if name == "error" || ev.Type == "error" {
		return "upstream_error", ev.errorMessage("the upstream said the answer failed"), true
	}
	if len(ev.Error) > 0 {
		raw := strings.TrimSpace(string(ev.Error))
		if raw != "" && raw != "null" && raw != "{}" {
			return "upstream_error", ev.errorMessage("the upstream said the answer failed"), true
		}
	}
	if si := ev.StatusInfo; si != nil && si.Code != 0 {
		m := si.Message
		if m == "" {
			m = si.Msg
		}
		if m == "" {
			m = fmt.Sprintf("status %d", si.Code)
		}
		return fmt.Sprintf("status_%d", si.Code), m, true
	}
	if br := ev.BaseResp; br != nil && br.StatusCode != 0 {
		m := br.StatusMsg
		if m == "" {
			m = fmt.Sprintf("status %d", br.StatusCode)
		}
		return fmt.Sprintf("status_%d", br.StatusCode), m, true
	}
	return "", "", false
}

// errorMessage reads the message out of an error field.
func (ev *anthropicEvent) errorMessage(fallback string) string {
	if len(ev.Error) == 0 {
		return fallback
	}
	var s string
	if json.Unmarshal(ev.Error, &s) == nil && s != "" {
		return s
	}
	var obj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(ev.Error, &obj) == nil {
		if obj.Message != "" {
			return obj.Message
		}
		if obj.Msg != "" {
			return obj.Msg
		}
		if obj.Type != "" {
			return obj.Type
		}
	}
	return fallback
}

// toolArguments gives the arguments of a tool call as one JSON string.
//
// The Responses protocol carries the arguments as a string, so the
// pieces the upstream sends add up to one JSON string.
func toolArguments(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}
	s := strings.TrimSpace(string(input))
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}

// readSSE reads the upstream's SSE stream and gives each event to fn.
//
// A data field may be continued over several lines: the lines belong to
// the same payload and are joined with a newline. An event ends at the
// blank line.
func readSSE(r io.Reader, fn func(name, payload string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var name string
	var data []string
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			// 事件到这里为止
			if len(data) > 0 {
				if err := fn(name, strings.Join(data, "\n")); err != nil {
					return err
				}
			}
			name = ""
			data = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			// 注释
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		switch field {
		case "event":
			name = strings.TrimPrefix(value, " ")
			continue
		case "data":
			data = append(data, strings.TrimPrefix(value, " "))
			continue
		case "id", "retry":
			// 跟答案没有关系
			continue
		}
		if len(data) > 0 {
			// 有些上游把一段 JSON 摊在几行上，只有第一行写了
			// data:。剩下的行是同一段话的后半截。
			data = append(data, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// 最后一块可能没有空行收尾
	if len(data) > 0 {
		return fn(name, strings.Join(data, "\n"))
	}
	return nil
}

// counter stands in for the random numbers if they cannot be had.
var counter atomic.Uint64

// randomID gives an id that belongs to this answer alone.
//
// The ids of the Responses protocol have to be unique: two requests
// for the same model must not answer with the same id.
func randomID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// 随机数拿不到时也不能给出重复的 id
		return fmt.Sprintf("%s_%d", prefix, counter.Add(1))
	}
	return prefix + "_" + hex.EncodeToString(b)
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/apikey"
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
	srv   *httptest.Server
	gw    *Server
	store *store.Store
	// mu guards the counters below: a test may send many requests at
	// once, and the fake upstream counts them from the server's own
	// goroutines.
	mu     sync.Mutex
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
			h.mu.Lock()
			h.renewals++
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok2","refresh_token":"ref2","expires_in":3600}`))
			return
		}
		h.mu.Lock()
		h.hits++
		h.tokens = append(h.tokens, r.Header.Get("Authorization"))
		h.mu.Unlock()
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
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[` +
			`{"type":"text","text":"ok"},` +
			`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Beijing"}}` +
			`]}`))
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
	var resp struct {
		Object     string `json:"object"`
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("the answer = %s", w.Body.String())
	}
	if resp.OutputText != "ok" {
		t.Fatalf("output_text = %q", resp.OutputText)
	}
	// 工具调用不能被丢掉：模型要调工具，客户端得看得见它调的是哪个
	// 工具、拿什么参数去调。非流式这条路以前只留 text 块，tool_use
	// 块一个都没有，模型永远调不了工具。
	var call *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	for i := range resp.Output {
		if resp.Output[i].Type == "function_call" {
			call = &resp.Output[i]
		}
	}
	if call == nil {
		t.Fatalf("the tool call was thrown away: %s", w.Body.String())
	}
	if call.CallID != "toolu_1" {
		t.Fatalf("call_id = %q, want the tool_use block's id", call.CallID)
	}
	if call.Name != "get_weather" {
		t.Fatalf("name = %q", call.Name)
	}
	if call.Arguments != `{"city":"Beijing"}` {
		t.Fatalf("arguments = %q", call.Arguments)
	}
	if call.Status != "completed" {
		t.Fatalf("status = %q", call.Status)
	}
	if call.ID == "" {
		t.Fatal("the tool call has no id of its own")
	}
	// 文字块还是那一个文字块：两条路对同一个上游答案要一样。
	var text *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	for i := range resp.Output {
		if resp.Output[i].Type == "message" {
			text = &resp.Output[i]
		}
	}
	if text == nil {
		t.Fatalf("the text block was thrown away: %s", w.Body.String())
	}
	if len(text.Content) != 1 || text.Content[0].Text != "ok" {
		t.Fatalf("the message item = %+v", text.Content)
	}
	if text.ID == "" {
		t.Fatal("the message item has no id of its own")
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

	// 错的 key 也是进不去的。
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", "sk-wrong")
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 for the wrong key", w.Code)
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

// TestForbiddenDoesNotRenewTheToken says a 403 does not renew the
// token.
//
// 聊天这条路只有 401 才续签：403 是站点自己不答，不是令牌旧了。
// 续签一次就是花掉一个只能用一次的 refresh token，不该花在每个
// 403 上。
// TestTheForbiddenIsNotAskedAgain says a 403 that does not name the
// credits is not asked again.
//
// 同一个账号、同一个令牌再问一次，问出来的还是这个答案。参考实现
// 是原样交给客户端的。
func TestTheForbiddenIsNotAskedAgain(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	h.mu.Lock()
	hits := h.hits
	h.mu.Unlock()
	if hits != 1 {
		t.Fatalf("the upstream was asked %d times, want 1", hits)
	}
}

// TestTheClientIsToldTheUpstreamsOwnWords says the words the client is
// given are the words the upstream said, even when the error is a
// plain string.
//
// 站点答 {"error":"insufficient_balance"} 时，那句话就是上游说的拒绝，
// 客户端该看到它，而不是一句"额度用完了"。
func TestTheClientIsToldTheUpstreamsOwnWords(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"insufficient_balance"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "usage limit reached: insufficient_balance") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestForbiddenDoesNotRenewTheToken(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if h.renewals != 0 {
		t.Fatalf("the token was renewed %d times, want 0", h.renewals)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
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

// TestAScriptGetsJSONWhenItSignsIn says a script that posts JSON gets
// JSON and the cookie, not a page.
//
// 脚本要的是一张票，不是一页 HTML：跟着重定向走会拿到一页它用不了
// 的东西。
func TestAScriptGetsJSONWhenItSignsIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"key":"sk-1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type = %q, want json", ct)
	}
	if body := w.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %s, want the ok answer", body)
	}
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no cookie was given: %v", w.Header()["Set-Cookie"])
	}
}

// TestAScriptIsToldInJSONThatItsKeyWasRefused says a refused key is
// refused in JSON when the caller asked in JSON.
//
// 脚本拿到一页 HTML 的话，它读不出那句"key 不对"。
func TestAScriptIsToldInJSONThatItsKeyWasRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"key":"sk-wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type = %q, want json", ct)
	}
	body := w.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("the answer is a page: %s", body)
	}
	if !strings.Contains(body, "refused") {
		t.Fatalf("body = %s, want the refusal", body)
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

// TestTheNativeFlagIsSafeWhileTheFallbackHappens says the flag that
// says the upstream speaks Responses is read and written by many
// requests at once without a race.
//
// 上游第一次说它没有 /responses 的时候，网关要把这条路关掉：那是一根
// 所有人都读的杆，一根被写一根被读，就是数据竞争。
//
// 这台机器上没有 race detector（-race 要 cgo，这里没有 gcc），所以在
// 这里看不出差别：在有 gcc 的机器上跑
// go test -race ./internal/gateway/
// 才会报出竞争来。
func TestTheNativeFlagIsSafeWhileTheFallbackHappens(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == site.ResponsesPath {
			// 上游没有 Responses：网关要改走本地转换。
			// 慢一点再答，让所有请求都已经在读那根杆了。
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	})

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := `{"model":"m","input":"hi ` + strconv.Itoa(i) + `"}`
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			w := httptest.NewRecorder()
			h.gw.ServeHTTP(w, req)
		}(i)
	}
	close(start)
	wg.Wait()

	// 第一个 404 把这条路关掉之后，它就一直是关着的。
	if h.gw.NativeResponses.Load() {
		t.Fatal("the native path is still on")
	}

	// 关掉之后，Codex 的请求就走本地转换那条路了：上游再也收不到
	// /responses。
	mu.Lock()
	before := len(paths)
	mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"again"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths[before:] {
		if p == site.ResponsesPath {
			t.Fatalf("the native path was used again: %v", paths[before:])
		}
	}
}

// TestAnEmptyConfiguredKeyOpensNothing says a configured key that is
// not a key never matches.
//
// 配置里写了空串的时候，一个不带 key 的请求带来的也是空串，两边一比
// 就"相等"了：等于是没有认证。配置这一头要挡住，配置说什么都不能
// 让空 key 进门。
func TestAnEmptyConfiguredKeyOpensNothing(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{""}

	headers := []string{"", " ", "  \t "}
	for _, key := range headers {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("the key %q was accepted: code = %d", key, w.Code)
		}
	}
}

// TestAnEmptyKeyInBearerOpensNothing says the same of the other place
// a key can come from.
//
// Bearer 后面空着，带来的也是空串。
func TestAnEmptyKeyInBearerOpensNothing(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{""}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("the empty bearer key was accepted: code = %d", w.Code)
	}
}

// TestAnEmptyKeySignsNobodyIn says the sign-in page does not give a
// cookie to an empty key either.
//
// 登录是另一扇门：空 key 配在那里的时候，空 key 也要进不去。
func TestAnEmptyKeySignsNobodyIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{""}

	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader("key="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatalf("a cookie was given for an empty key: %+v", c)
		}
	}
}

// sessionRecorder keeps the session ids the upstream was told about.
//
// 记下发给上游的会话 id：这一节要看的就是它。
type sessionRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (s *sessionRecorder) record(r *http.Request) {
	s.mu.Lock()
	s.ids = append(s.ids, r.Header.Get("X-Mavis-Session-Id"))
	s.mu.Unlock()
}

func (s *sessionRecorder) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

// plainPool is a pool that cannot supply an upstream id.
//
// 没有 UpstreamID 方法的池子：rotator 退回自己算出来的那个 id。
type plainPool struct {
	accounts []store.Account
	sessions map[string]string
}

func (p *plainPool) Accounts(context.Context) ([]store.Account, error) { return p.accounts, nil }

func (p *plainPool) Exhausted(context.Context) ([]store.Exhausted, error) { return nil, nil }

func (p *plainPool) BindSession(_ context.Context, key, accountID string) error {
	if p.sessions == nil {
		p.sessions = map[string]string{}
	}
	p.sessions[key] = accountID
	return nil
}

func (p *plainPool) SessionAccount(_ context.Context, key string) (string, bool, error) {
	id, ok := p.sessions[key]
	return id, ok, nil
}

func (p *plainPool) MarkExhausted(context.Context, string, string, time.Time, string) error {
	return nil
}

func (p *plainPool) Remaining(context.Context, string) (float64, bool) { return 0, false }

// chat sends one chat request and gives the answer back.
func chat(t *testing.T, h *harness, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	return w
}

// TestTheUpstreamIDIsTheOneTheStoreKept says the upstream is told the
// id the database keeps, and not the client's own string.
//
// 上游看到的是数据库里存着的那个 id。客户端自己那串字符串不外发。
func TestTheUpstreamIDIsTheOneTheStoreKept(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	chat(t, h, "client-1", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	seen := rec.seen()
	if len(seen) != 1 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	if seen[0] == "client-1" {
		t.Fatal("the client's own session id went upstream")
	}
	key := rotator.SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "", "").Key
	want, err := h.store.UpstreamID(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if seen[0] != want {
		t.Fatalf("upstream id = %q, want the one the store keeps %q", seen[0], want)
	}
}

// TestOneConversationKeepsOneUpstreamID says the two requests of one
// conversation go upstream with the same id.
//
// 一段对话的两次请求，上游看到的是同一个会话 id：重启之后也是这个。
func TestOneConversationKeepsOneUpstreamID(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chat(t, h, "client-1", body)
	chat(t, h, "client-1", body)

	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	if seen[0] == "" {
		t.Fatal("no upstream id was sent")
	}
	if seen[0] != seen[1] {
		t.Fatalf("the two requests got %q and %q", seen[0], seen[1])
	}
}

// TestTwoConversationsGetDifferentUpstreamIDs says two conversations do
// not share the id the upstream is told about.
//
// 两段对话拿到两个不同的上游 id。
func TestTwoConversationsGetDifferentUpstreamIDs(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chat(t, h, "client-1", body)
	chat(t, h, "client-2", body)

	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	if seen[0] == "" || seen[1] == "" {
		t.Fatalf("upstream ids = %q and %q", seen[0], seen[1])
	}
	if seen[0] == seen[1] {
		t.Fatalf("the two conversations got the same upstream id %q", seen[0])
	}
}

// TestAPoolWithoutAnUpstreamIDFallsBackToTheDerivedOne says a pool that
// cannot supply an id leaves the derived one in place, and that it is
// stable.
//
// 给不出上游 id 的池子：退回请求自己算出来的那个，两次请求是同一个。
func TestAPoolWithoutAnUpstreamIDFallsBackToTheDerivedOne(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 换成一个给不出上游 id 的池子。
	h.gw.Rot = rotator.New(&plainPool{accounts: []store.Account{{
		ID: "acc_01", Name: "a", Status: store.StatusActive,
	}}}, rotator.Options{MaxAttempts: 3, WindowGroup: manager.WindowGroup})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chat(t, h, "client-1", body)
	chat(t, h, "client-1", body)

	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	// 退回的规则没有变：客户端带了 id，就是客户端自己那串。
	if seen[0] != "client-1" {
		t.Fatalf("upstream id = %q, want the client's own", seen[0])
	}
	if seen[0] != seen[1] {
		t.Fatalf("the two requests got %q and %q", seen[0], seen[1])
	}

	// 客户端没带 id：退回的是第一条消息的哈希。
	chat(t, h, "", body)
	seen = rec.seen()
	want := rotator.SessionOf(nil, "", "hi").UpstreamID
	if seen[len(seen)-1] != want {
		t.Fatalf("upstream id = %q, want the hash %q", seen[len(seen)-1], want)
	}
}

// TestAKeyMadeInTheConsoleOpensTheChatAPI says a key made in the
// console opens the chat API.
//
// 后台申请的那把 key 是给别的机器用的：它得能聊天。
func TestAKeyMadeInTheConsoleOpensTheChatAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 配置里有 key，所以门是关着的：这时候进来的那把才真是靠它开的门。
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the key made in the console was refused")
	}
}

// TestARevokedKeyDoesNotOpenTheChatAPI says a revoked key opens
// nothing.
//
// 撤销是把门关上：撤了的那一把不该再能进门。
func TestARevokedKeyDoesNotOpenTheChatAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)
	if err := h.store.RevokeAPIKey(context.Background(), "key_1", time.Now()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 for a revoked key", w.Code)
	}
}

// TestAKeyMadeInTheConsoleDoesNotOpenTheConsole says a chat key is not
// a console key.
//
// 后台登录用的是另一把 key（配置里那把）：聊天用的 key 不该也能打开
// 管理界面。
func TestAKeyMadeInTheConsoleDoesNotOpenTheConsole(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)

	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: a chat key opened the console", w.Code)
	}

	// 后台那把 key 还是能进后台。
	req = httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("x-api-key", "sk-console")
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the console key was refused")
	}
}

// makeAKey writes a key down and gives it back in the clear.
func makeAKey(t *testing.T, s *store.Store) string {
	t.Helper()
	plain, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIKey(context.Background(), store.APIKey{
		ID: "key_1", Name: "给我的笔记本", KeyHash: apikey.Hash(plain),
		Prefix: apikey.Display(plain), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return plain
}

// TestTheUsageCardIsMagpiesShape says the gateway has an endpoint that
// says the usage in magpie's card shape.
//
// magpie 的 provider 插件有一个 usage() 钩子，卡片就是从那里来的。我们
// 没有自己端点，所以这个钩子得由网关自己顶上来：给 magpie 一个 magpie
// 认得的形状。
func TestTheUsageCardIsMagpiesShape(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	if err := h.store.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 500, Consumed: 62.5, Remaining: 437.5,
		Plan: "Token Plan M3", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Plan    string `json:"plan"`
		Balance string `json:"balance"`
		Windows []struct {
			Name string `json:"name"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Plan != "Token Plan M3" {
		t.Errorf("plan = %q, want %q", out.Plan, "Token Plan M3")
	}
	if out.Balance != "437.5 credits" {
		t.Errorf("balance = %q, want %q", out.Balance, "437.5 credits")
	}
	if len(out.Windows) == 0 {
		t.Error("the card has no windows")
	}
}

// TestTheUsageCardNeedsAKey says the card is behind the key.
//
// 卡片说的是这个网关还剩多少积分：那不是给路人看的。
func TestTheUsageCardNeedsAKey(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	h.gw.Cfg.Server.APIKeys = []string{"sk-admin"}
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// TestAKeyMadeInTheConsoleOpensTheUsageCard says a chat key opens the
// card.
//
// magpie 是拿着聊天那把 key 来问卡片的：它问的时候手里有的就是这一把。
func TestAKeyMadeInTheConsoleOpensTheUsageCard(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	h.gw.Cfg.Server.APIKeys = []string{"sk-admin"}
	plain := makeAKey(t, h.store)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("x-api-key", plain)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"mmxcode/internal/apikey"
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
	return s.authWith(r, keys)
}

// authWith checks the request's key against the given keys.
//
// 头里的 key 和浏览器带的 cookie 都认。
func (s *Server) authWith(r *http.Request, keys []string) bool {
	if _, ok := matchingKey(givenKey(r), keys); ok {
		return true
	}
	// 浏览器带来的 cookie：值是某一把配好的 key 算出来的
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			// 空的 key 算不出票来：它不该是谁的票。
			continue
		}
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionValue(k))) == 1 {
			return true
		}
	}
	return false
}

// givenKey gives the key the request carries.
//
// 两个头都认：x-api-key，和 Authorization: Bearer。
func givenKey(r *http.Request) string {
	given := strings.TrimSpace(r.Header.Get("x-api-key"))
	if given != "" {
		return given
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// authChat checks the request's key for the chat API.
//
// 两把 key 都认：配置里那把（后台登录用的），和在后台申请的那把（给
// 别的机器聊天用的）。
//
// 一把都没有的时候不鉴权：那时候网关只该听本机。申请过就不一样了：
// 那是有人打算从别的机器用它。
func (s *Server) authChat(r *http.Request) bool {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 && !s.hasMadeKeys(r.Context()) {
		return true
	}
	// 配置里那把，和浏览器带的 cookie。
	if s.authWith(r, keys) {
		return true
	}
	given := givenKey(r)
	if given == "" || s.Store == nil {
		return false
	}
	// 库里存的是哈希：请求带来的 key 算一次哈希，对上了就是那一把。
	// 撤销过的不算——它已经不能开门了。
	_, err := s.Store.APIKeyByHash(r.Context(), apikey.Hash(given))
	return err == nil
}

// hasMadeKeys says whether any key has been made in the console.
func (s *Server) hasMadeKeys(ctx context.Context) bool {
	if s.Store == nil {
		return false
	}
	have, err := s.Store.HasAPIKeys(ctx)
	return err == nil && have
}

// matchingKey gives the configured key the given one matches.
//
// 空白的 key 不算一把 key。配置里有一把空的时候，一个不带 key 的请求
// 带来的也是空串，两边一比就是"相等"——那等于没有认证，一份配错的配置
// 就把门打开了。所以空的配置项和空的请求都挡在这里：配置说什么都不
// 算数。
func matchingKey(given string, keys []string) (string, bool) {
	if strings.TrimSpace(given) == "" {
		return "", false
	}
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(given), []byte(k)) == 1 {
			return k, true
		}
	}
	return "", false
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
//
// A script gets JSON and the cookie, not a page: a redirect would send
// it to a page it cannot use. The browser gets the page.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	keys := s.Cfg.Server.APIKeys
	if len(keys) == 0 {
		writeError(w, protocolOf(r.URL.Path), http.StatusBadRequest, "this gateway has no api key configured")
		return
	}
	key := submittedKey(r)
	// 空 key 进不来：登录是另一扇门，空的那把不该把门打开。
	if k, ok := matchingKey(key, keys); ok {
		s.setSessionCookie(w, r, k)
		w.Header().Set("Cache-Control", "no-store")
		if wantsJSON(r) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// 错的 key：不发 cookie
	if wantsJSON(r) {
		writeError(w, protocolOf(r.URL.Path), http.StatusUnauthorized, "the key was refused")
		return
	}
	s.signInPage(w, r, http.StatusUnauthorized, "the key was refused")
}

// wantsJSON says the caller wants an answer in JSON rather than a page.
//
// 浏览器交表单，脚本交 JSON。分清楚了，脚本才拿得到 JSON：它要是
// 跟着重定向走，拿回来的是一页 HTML，那对它没有用。
func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
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

// isChatPath says the path is one of the chat API's.
//
// 聊天那扇门和管理页那扇门不一样：聊天认两把 key，管理页只认一把。
func isChatPath(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

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
//
// The shape is the one of the protocol the client speaks: an OpenAI
// client reads the error out of "error", and an Anthropic or a Codex
// client reads the type out of it as well.
func writeError(w http.ResponseWriter, proto protocol, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody(proto, "api_error", msg))
}

// writeRateLimit writes the answer for a refusal that is about the
// credits.
//
// The client is told the limit was reached, so it knows to wait and
// try again rather than that something broke.
func writeRateLimit(w http.ResponseWriter, proto protocol, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(errorBody(proto, "rate_limit_error", "usage limit reached: "+message))
}

// errorBody gives the body of an error.
//
// The Anthropic and the Codex protocols carry the type beside the
// error; the OpenAI one carries the error on its own.
func errorBody(proto protocol, kind, msg string) map[string]any {
	errObj := map[string]any{
		"type":    kind,
		"message": msg,
	}
	if proto == protocolOpenAI {
		return map[string]any{"error": errObj}
	}
	return map[string]any{
		"type":  "error",
		"error": errObj,
	}
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
//
// The session id the upstream is told about is the one the database
// keeps: an opaque id, made once and kept, so the upstream sees one
// conversation and the mapping from the session to the upstream's
// conversation survives a restart. The client's own session id does not
// go upstream, and neither does a hash of the request's words.
//
// What that does not fix: two conversations whose system prompt and
// first user message are the same, sent by a client that sends no
// session id of its own, are one session by the session key's own
// definition. They share one upstream id, and one upstream
// conversation, however random that id is, and no server can tell them
// apart: there is nothing in the request that does. The fix for that
// case is the client sending X-Session-Id.
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
	"sync"
	"sync/atomic"
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
	//
	// 每个请求都读它，而第一个 404 会把它写掉：一个字段被一个请求写、
	// 被别的请求读，就是数据竞争。所以它是 atomic 的。
	NativeResponses atomic.Bool
	// NativeChatCompletions says the upstream speaks the OpenAI chat
	// completions protocol itself. It is tried and turned off when the
	// upstream says no.
	//
	// 参考实现只走 Anthropic 的 /messages，这条路是照着同目录下的
	// /messages 推的：站点也许有，也许没有。它和 NativeResponses 是
	// 同一根杆，同一个理由才用 atomic。
	NativeChatCompletions atomic.Bool
	// WebUI serves the management pages, when there is one.
	WebUI http.Handler
	// modelsMu guards the model list that was read.
	modelsMu sync.Mutex
	// models is the model list, the site's own or the built-in one.
	models []site.Model
	// modelsAt is when it was read.
	modelsAt time.Time
	// modelsLive says the list is the site's own, and not the built-in
	// one that is served when the site cannot be reached.
	modelsLive bool
}

// New makes a gateway.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store, webui http.Handler) *Server {
	g := &Server{
		Mgr:          mgr,
		Cfg:          cfg,
		Store:        s,
		UpstreamBase: site.LLMHost,
		WebUI:        webui,
	}
	// 原生路径先开着：上游说它没有 /responses 的时候再关。
	g.NativeResponses.Store(true)
	g.NativeChatCompletions.Store(true)
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
	UpstreamID(ctx context.Context, sessionKey string) (string, error)
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

// UpstreamID gives the upstream id of a session.
//
// The id is the one the database keeps: an opaque id, made once and
// kept, so the upstream sees one conversation and the mapping survives
// a restart. The client's own session id does not go upstream.
func (p poolAdapter) UpstreamID(ctx context.Context, key string) (string, error) {
	return p.s.UpstreamID(ctx, key)
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
	// 两扇门：聊天那扇认两把 key（配置里的和后台申请的），管理页那扇只
	// 认配置里的那把。
	gate := s.auth
	if isChatPath(r.URL.Path) {
		gate = s.authChat
	}
	if !gate(r) {
		if isAPIPath(r.URL.Path) {
			writeError(w, protocolOf(r.URL.Path), http.StatusUnauthorized, "the key was refused")
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
	case "/v1/usage":
		// magpie 的卡片：这个网关还剩多少额度。
		s.usageCard(w, r)
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
	writeError(w, protocolOf(r.URL.Path), http.StatusNotFound, "no such endpoint")
}

// protocol is the protocol the client speaks.
type protocol int

const (
	protocolAnthropic protocol = iota
	protocolOpenAI
	protocolCodex
)

// protocolOf says which protocol a path speaks.
//
// 一个路径一个协议：错误的样子也跟着协议走，所以路由和错误都要问
// 同一句。
func protocolOf(path string) protocol {
	switch path {
	case "/v1/chat/completions":
		return protocolOpenAI
	case "/v1/responses":
		return protocolCodex
	default:
		return protocolAnthropic
	}
}

// maxBodyBytes is how large a request body may be.
//
// 64 MiB：用户是可能传图片的 —— 站点的模型收 text、image、video（参考插件
// 的模型名单就是这么写的），而图片是 base64 写在身体里的，一张图几 MB 是常
// 态，用户传几张就上三十兆。
//
// 1M token 的对话是 4 MB 上下（带上 JSON 外壳 4.5–5 MB），64 MiB 是它的十几
// 倍，够宽裕。
//
// 再大的请求不是对话，是要把它这个网关的内存吃光的东西：没有上限的时候，一
// 个请求就能让网关没有内存可用。
const maxBodyBytes = 64 << 20

// handleChat serves a chat request.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, proto protocol) {
	// 请求体有个上限：不然一个客户端发来一大块东西，网关就把它整个
	// 读进内存里，然后就没有内存了。
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, proto, http.StatusRequestEntityTooLarge, "the request body is too large")
			return
		}
		writeError(w, proto, http.StatusBadRequest, "the request body could not be read")
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
	// 发给上游的会话 id 是数据库里存着的那个：不重算，也不把客户端
	// 自己那串字符串发出去。取不到就是取不到，不能拿别的顶上：顶上来的
	// 那个上游没见过，会把这段对话当成新的一段。
	upstreamID, err := s.Rot.UpstreamID(ctx, sess)
	if err != nil {
		writeError(w, proto, http.StatusServiceUnavailable, err.Error())
		return
	}

	used := map[string]bool{}
	var lastErr error
	// quota 说最后一次失败是“额度用完了”，quotaMsg 是上游自己说的那句。
	// 客户端最后看到的是上游的话，不是网关自己编的一句。
	var quota bool
	var quotaMsg string
	// lastStatus 是上游最后一次给的状态，lastMsg 是它自己说的那句。
	// 客户端最后看到的是上游的状态，不是网关编出来的一个 429：上游说
	// 503 就是 503，客户端才知道是上游坏了，而不是自己被限流了。
	var lastStatus int
	var lastMsg string
	attempts := s.Cfg.Rotation.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
accountLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if lastErr != nil {
				s.writeFailure(w, proto, quota, quotaMsg, lastStatus, lastMsg, lastErr)
				return
			}
			writeError(w, proto, http.StatusServiceUnavailable, err.Error())
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

		upBody, upPath, convertBack, err := s.upstreamShape(body, proto)
		if err != nil {
			writeError(w, proto, http.StatusBadRequest, err.Error())
			return
		}
		chatReq := upstream.ChatRequest{
			BaseURL:   s.UpstreamBase,
			Path:      upPath,
			Access:    tok.Access,
			SessionID: upstreamID,
			// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
			// 和 Zone() 给的偏移是同一个值。time.Time 没有 Offset()
			// 方法，Zone() 的第二个返回值就是它。
			TZOffset: nowOffset(),
			Body:     upBody,
			Stream:   stream,
			Extra:    clientHeaders(r),
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
					writeError(w, proto, http.StatusBadGateway, oe.Error())
					return
				}
				// 网络没通：还是这个账号，等一等再来。上游没有给出
				// 状态：这是网关自己的问题。
				lastErr = err
				lastStatus = 0
				lastMsg = ""
				if !sleepCtx(ctx, backoff(try)) {
					break accountLoop
				}
				continue
			}

			// 上游没有这条路：改用本地转换，同一个账号再来一次。
			//
			// 开放平台对 MiniMax Code 的账号答的也是「没有这条路」：503
			// 加一句 direct_route_not_configured。那不是站点坏了，是那条
			// 路对这个账号不通 —— 该走 Anthropic 那条。
			if resp.StatusCode == http.StatusNotFound || openPlatformNotConfigured(raw) {
				fb, fp, fback, ok := s.fallbackShape(body, proto, upPath)
				if ok {
					upBody, upPath, convertBack = fb, fp, fback
					chatReq.Body = upBody
					chatReq.Path = upPath
					resp, raw, err = s.attempt(ctx, chatReq, client.Transport, stream)
					if err != nil {
						var oe *upstream.OtherSiteError
						if errors.As(err, &oe) {
							writeError(w, proto, http.StatusBadGateway, oe.Error())
							return
						}
						lastErr = err
						lastStatus = 0
						lastMsg = ""
						if !sleepCtx(ctx, backoff(try)) {
							break accountLoop
						}
						continue
					}
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
					s.writeJSON(w, resp, raw, proto, convertBack, model)
					return
				}
			}

			failure := s.failure(resp.StatusCode, raw)
			lastStatus = resp.StatusCode
			lastMsg = failure.Message
			if lastMsg == "" {
				// 上游自己说的话要给客户端和运维看：只知道一个状态码，
				// 谁也说不清到底是哪一件事坏了。
				lastMsg = upstreamMessage(raw)
			}
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
					// 续签自己的错要看 rerr：err 是上一次尝试的，
					// 那时候它是空的，看它永远看不出来。
					if errors.As(rerr, &oe) {
						writeError(w, proto, http.StatusBadGateway, oe.Error())
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
				if !sleepCtx(ctx, backoff(try)) {
					break accountLoop
				}
				continue
			default:
				// 给不出别的说法：上游的状态原样给客户端。
				s.writeFailure(w, proto, false, "", resp.StatusCode, lastMsg, lastErr)
				return
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("the request could not be served")
	}
	s.writeFailure(w, proto, quota, quotaMsg, lastStatus, lastMsg, lastErr)
}

// failure says what went wrong upstream.
//
// The code and the message come out of the parsed answer, so that the
// decision the rotator makes and the words the client is given are read
// from one thing and not from two.
func (s *Server) failure(status int, raw []byte) rotator.Failure {
	f := rotator.Failure{Status: status, Body: raw}
	err := upstream.CheckEnvelope("chat", raw)
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		f.Code = be.Code
		f.Message = be.Message
		// 1000048：站点说签到被拒了。令牌照旧了，账号本身没问题。
		if errors.Is(err, upstream.ErrSignInRefused) {
			f.Refused = true
		}
		return f
	}
	// 没有 MiniMax 自己的形状：消息按它平常的样子读，rotator 读的
	// 也是同一份 —— 客户端看到的和它做决定用的是同一句话。
	f.Message = rotator.UpstreamMessage(raw)
	return f
}

// upstreamMessage reads the message out of an upstream body.
//
// 上游的话要看得见：客户端和运维都要知道上游说了什么，而不是只知道
// 一个状态码。这和 rotator 读消息的是同一份。
func upstreamMessage(raw []byte) string {
	return rotator.UpstreamMessage(raw)
}

// action says what to do about the answer.
//
// The rotator owns the rule, and on the chat path the rule is the
// site's own: a 401 means the token was turned away before its end, so
// the token is renewed and the same account is asked again. A 403 is
// the site's refusal to answer and not a stale token — renewing on it
// would spend a refresh token (which MiniMax gives only once) on
// every refusal.
//
// The account API is a different story: there the site refuses a
// sign-in with a 403 as well as with a 401, and the account package
// reads both that way.
func (s *Server) action(failure rotator.Failure) rotator.Action {
	return rotator.Decide(failure)
}

// writeFailure writes the answer of a request that could not be served.
//
// The status is the upstream's own, when the upstream gave one: a 503
// is a 503 and a 400 is a 400. Calling both of them a 429 tells the
// client it was sent too fast, and it backs off from a gateway that is
// not the problem. The one that stays a 429 is the refusal about the
// credits: that is the one the client can do something about.
//
// A failure with no upstream status at all is the gateway's own: the
// request never got through, and that is a 502.
//
// The words the client is given are the upstream's own when it said the
// credits are gone: "usage limit reached: …" tells the client to wait
// and try again, which a plain error does not.
func (s *Server) writeFailure(w http.ResponseWriter, proto protocol, quota bool, quotaMsg string, status int, message string, err error) {
	if quota {
		msg := quotaMsg
		if msg == "" {
			msg = "the account's credits or plan are used up"
		}
		writeRateLimit(w, proto, msg)
		return
	}
	if status == 0 {
		// 上游没有给出状态：网络没通。这是网关自己的问题。
		msg := "the request did not get through"
		if err != nil {
			msg = err.Error()
		}
		writeError(w, proto, http.StatusBadGateway, msg)
		return
	}
	if message == "" {
		message = fmt.Sprintf("the upstream answered with HTTP %d", status)
	}
	writeError(w, proto, status, message)
}

// openPlatformNotConfigured says the upstream's answer is "the open
// platform has no route for this account".
//
// MiniMax 的开放平台是给 API key 用的：MiniMax Code 的账号在那条路上没
// 有配好的路由，站点答的是 503、errorCode 50115、direct_route_not_configured。
// 这不是站点坏了，是这条路对这个账号不通：跟「没有这条路」是一回事。
func openPlatformNotConfigured(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var env struct {
		ErrorCode   int    `json:"errorCode"`
		ErrorReason string `json:"errorReason"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false
	}
	return env.ErrorCode == 50115 || env.ErrorReason == "direct_route_not_configured"
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
// Anthropic goes through untouched: the site speaks it. The OpenAI and
// the Responses protocols go through untouched as long as the site is
// thought to speak them; when the site says it does not have them (a
// 404), the request is turned into an Anthropic one and the answer is
// turned back afterwards.
func (s *Server) upstreamShape(body []byte, proto protocol) (out []byte, path string, convertBack bool, err error) {
	switch proto {
	case protocolAnthropic:
		return body, site.MessagesPath, false, nil
	case protocolOpenAI:
		if s.NativeChatCompletions.Load() {
			return body, site.ChatCompletionsPath, false, nil
		}
		b, cerr := convert.ToAnthropicOpenAI(body)
		if cerr != nil {
			return nil, "", false, fmt.Errorf("the request could not be turned into the upstream's format: %w", cerr)
		}
		return b, site.MessagesPath, true, nil
	default:
		if s.NativeResponses.Load() {
			return body, site.ResponsesPath, false, nil
		}
		b, cerr := convert.ToAnthropic(body)
		if cerr != nil {
			return nil, "", false, fmt.Errorf("the request could not be turned into the upstream's format: %w", cerr)
		}
		return b, site.MessagesPath, true, nil
	}
}

// fallbackShape gives the shape to use when the upstream does not have
// the path the request went out to.
//
// The site may well serve /chat/completions and /responses itself, and
// that is the better way: nothing is converted and nothing is lost.
// When it answers a 404 instead, the path is closed for everybody and
// the request is turned into an Anthropic one; the answer is turned
// back afterwards.
//
// The path the request went out to is what decides: a request that
// already went out as an Anthropic one has no fallback left, and its
// 404 is the upstream's own answer.
func (s *Server) fallbackShape(body []byte, proto protocol, path string) (out []byte, newPath string, convertBack bool, ok bool) {
	switch proto {
	case protocolCodex:
		if path != site.ResponsesPath {
			return nil, "", false, false
		}
		// 上游说它没有 /responses：这条路就关掉了。
		s.NativeResponses.Store(false)
	case protocolOpenAI:
		if path != site.ChatCompletionsPath {
			return nil, "", false, false
		}
		// 上游说它没有 /chat/completions：这条路就关掉了。
		s.NativeChatCompletions.Store(false)
	default:
		return nil, "", false, false
	}
	b, p, back, err := s.upstreamShape(body, proto)
	if err != nil {
		return nil, "", false, false
	}
	return b, p, back, true
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
		// 转换器自己收尾：它失败的时候已经写了一个失败事件，
		// 网关再写一个就是第二次失败，形状还不一样。
		switch proto {
		case protocolCodex:
			err = convert.StreamResponses(resp.Body, w, model)
		case protocolOpenAI:
			err = convert.StreamChatCompletions(resp.Body, w, model)
		}
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
func (s *Server) writeJSON(w http.ResponseWriter, resp *http.Response, body []byte, proto protocol, convertBack bool, model string) {
	if convertBack {
		out, err := backConvert(body, proto, model)
		if err != nil {
			writeError(w, proto, http.StatusBadGateway, err.Error())
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

// backConvert turns the upstream's Anthropic answer into the shape the
// client speaks.
//
// The upstream answered in Anthropic because the site has no endpoint
// of the client's own: the Codex protocol gets a Responses answer, the
// OpenAI protocol a chat completion.
func backConvert(body []byte, proto protocol, model string) ([]byte, error) {
	switch proto {
	case protocolCodex:
		return convert.ToResponses(body, model)
	case protocolOpenAI:
		return convert.ToChatCompletion(body, model)
	default:
		return body, nil
	}
}

// handleModels serves the model list.
//
// The list is the site's own: it is read from MiniMax and kept for a
// while, because the site's list changes a few times a year and a
// client asking for it should not be made to wait for a trip to
// MiniMax every time.
//
// 站点答不上来的时候摆的是内置的那一份：那是站点已知有的东西，比一
// 份空名单好。内置的那一份也会旧（站点改过模型的名字），所以它只
// 是兜底，不是主路。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, m := range s.modelsFor(r.Context()) {
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "owned_by": "minimax",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
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

// sleepCtx waits, unless the request is over.
//
// 客户端走了就不必再等：等着只是白占着上游的次数和时间，一个请求能
// 让九次上游调用和三十秒的睡觉一起吊着。
//
// It says whether the wait was over: false means the request is over
// and nothing more should be asked upstream.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
亲和键（永远是哈希，客户端自己那串字符串不会变成数据库键）和推导出来的
上游会话 id（客户端带了就用客户端的，换号时不变）。

发给上游的会话 id 不是这个推导出来的，而是 `rotator.Rotator.UpstreamID` 给的：
池子（网关这边是 `store.UpstreamID`）存着的那个随机 id，一个会话一个，存下来
不变，重启之后还是它。池子给不出的时候（测试里的替身）才退回推导出来的那个。

这个改不了两段一样的开场白：system prompt 和第一条 user 消息都一样、客户端又不
带会话 id 的两段对话，按会话键的定义本来就是同一个会话，于是共用一个上游 id
和一个上游会话。服务器分不出来 —— 请求里没有能分开它们的东西，要分开只能让
客户端带上 `X-Session-Id`。

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

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/provision"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
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
	// The outlet groups are the configuration's; the resolver is built
	// from the same map, so the page and the resolver agree on which
	// groups there are.
	if len(cfg.Proxy.Groups) == 0 {
		cfg.Proxy.Groups = groups
	}
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

	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
		strings.NewReader(`{"name":"新名字","proxy_group":"warp"}`))
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
	if a.Name != "新名字" || a.ProxyGroup != "warp" {
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
/* 上一次的错是一整句：给它一块地方，别让它撑着整张表。 */
.err { color: #c0392b; max-width: 28rem; overflow-wrap: anywhere; }
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
  <td>{{if eq $acc.Status "active"}}<span class="good">正常</span>{{else if eq $acc.Status "signed_out"}}<span class="bad">已登出</span>{{else}}<span class="bad">{{$acc.Status}}</span>{{end}}{{if $acc.LastError}}<div class="err">{{$acc.LastError}}</div>{{end}}</td>
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
{{else}}
<button id="start-login">开始登录</button>
{{end}}
{{/* 验证码和结果各有一个地方：页面一开始没有等待中的登录时，
     失败的话也得让人看见那句话。 */}}
<div id="login-result"></div>
<p id="poll-result" class="muted">{{if .Pending}}正在等……{{end}}</p>
{{end}}
```

- [ ] **Step 4: 写静态脚本**

创建 `internal/webui/static/app.js`：

```javascript
// mmxcode 的 WebUI 脚本。
//
// 做几件事：改账号的出口、触发签到、测出口、登录。
// 页面本身是服务端渲染的，这里不重新渲染。
//
// 这里不用带 API key：浏览器同源的 fetch 会自己带上 mmxcode_session
// 那张票，页面的按钮因此不会 401。
//
// 上游答上来的东西（验证码、地址、错误）一律当字看：它们进页面的时候
// 只走 textContent 和 URL，不拼 HTML。

// readAnswer 读一个回答。
//
// 回答不一定是 JSON：网关拒绝了一把 key 的时候给的是登录页，那是
// HTML。所以回答先当字读下来，再看它是什么。
async function readAnswer(res) {
  const text = await res.text();
  const type = (res.headers.get('Content-Type') || '').toLowerCase();
  let json = null;
  // 没有 Content-Type 的回答也试一下：中间的一层代理可能把它拿掉了。
  if (type.indexOf('json') >= 0 || type === '') {
    try {
      json = JSON.parse(text);
    } catch (e) {
      json = null;
    }
  }
  return { ok: res.ok, status: res.status, json: json, text: text };
}

// messageOf 说一个回答说了什么。
//
// JSON 的错误在 error.message 里，登录页的那句话在 .bad 里。两样都读，
// 人看到的都是一句话，不是一段 HTML。
function messageOf(answer) {
  if (answer.json) {
    const e = answer.json.error;
    if (typeof e === 'string' && e) return e;
    if (e && e.message) return e.message;
    if (answer.json.message) return answer.json.message;
  }
  const page = pageMessage(answer.text);
  if (page) return page;
  return answer.text.trim() || ('HTTP ' + answer.status);
}

// pageMessage 读登录页上那句话。
//
// 页面是 HTML，所以它被解析成文档，只取 .bad 那一行的字。它从来不
// 被写回页面里去。
function pageMessage(html) {
  if (!html) return '';
  try {
    const doc = new DOMParser().parseFromString(html, 'text/html');
    const bad = doc.querySelector('.bad');
    return bad ? bad.textContent.trim() : '';
  } catch (e) {
    return '';
  }
}

// safeURL 给一个能点的地址，或者不给。
//
// 地址是上游答上来的，javascript: 那种点一下就跑。只有 http 和 https
// 才是链接，其余的不是链接。
function safeURL(raw) {
  try {
    const u = new URL(String(raw));
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
    return u.href;
  } catch (e) {
    return null;
  }
}

async function post(url, body) {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const answer = await readAnswer(res);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
  return answer.json;
}

async function patch(url, body) {
  const res = await fetch(url, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  const answer = await readAnswer(res);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
}

// signIn 拿一张票。
//
// 回答可能是登录页（HTML），也可能是 JSON：网关现在给的是前者。两样
// 都读，人看到的是同一句话。
async function signIn(key) {
  const res = await fetch('/api/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ key: key }),
  });
  const answer = await readAnswer(res);
  if (!answer.ok) throw new Error(messageOf(answer));
  return answer;
}

// askForKey 要一把 key。
//
// 会话没了的时候，页面上的按钮都答 401。这里问一次 key，交给网关换
// 一张票，换到了就把页面重新加载。
let keyBox = null;
function askForKey() {
  if (keyBox) return;
  keyBox = document.createElement('div');
  keyBox.className = 'card';
  const label = document.createElement('p');
  label.textContent = '这个网关要一把 API key。填进去就能继续。';
  const form = document.createElement('form');
  const input = document.createElement('input');
  input.type = 'password';
  input.placeholder = 'API key';
  input.autocomplete = 'current-password';
  const button = document.createElement('button');
  button.type = 'submit';
  button.textContent = '进入';
  const msg = document.createElement('p');
  form.append(input, button);
  keyBox.append(label, form, msg);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    button.disabled = true;
    try {
      await signIn(input.value);
      location.reload();
    } catch (e) {
      msg.textContent = e.message;
      button.disabled = false;
    }
  });
  document.body.appendChild(keyBox);
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
      // 验证码和地址是上游给的：它们只作为字和地址进页面，不作为
      // 标记。地址还得是 http 或者 https 才配当链接。
      box.textContent = '';
      const code = document.createElement('strong');
      code.textContent = out.user_code;
      const link = document.createElement('a');
      link.textContent = '这个页面';
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      const href = safeURL(out.verification_url);
      if (href) link.href = href;
      box.append('把验证码 ', code, ' 填到 ', link, '。');
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
      const box = document.getElementById('poll-result');
      if (out.done) {
        clearInterval(timer);
        if (box) box.textContent = '登录成功，正在刷新……';
        location.reload();
      } else if (out.error) {
        clearInterval(timer);
        if (box) box.textContent = '失败：' + out.error;
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
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/provision"
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
	// Err says why the sign-in did not work. It is empty while the
	// sign-in is still going on.
	//
	// It is written and read under the WebUI's lock. A string is two
	// words, and reading one of them while the other one is being
	// written gives a string that is half of one thing and half of
	// another: a pointer to nowhere with a length that is not there.
	Err string
	// FailedAt is when the sign-in failed. It is zero while the sign-in
	// is still going on.
	FailedAt time.Time
}

// defaultPendingTTL is how long a sign-in that has ended is kept.
//
// A sign-in that failed has to be kept long enough for the browser to
// be told why: the page asks about it every few seconds. After that it
// goes, so that a server that is up for a long time does not keep every
// sign-in it ever had.
const defaultPendingTTL = 10 * time.Minute

// WebUI serves the pages and the API.
type WebUI struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store
	// Import brings a batch of accounts in. It is a field so that the
	// page and the command share one task, and so that a test can
	// stand in for the network.
	Import func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error)
	// Relogin signs the accounts that are signed out in again.
	Relogin func(ctx context.Context) (manager.ReloginResult, error)
	// PasswordNote says what happens to the passwords of the accounts
	// that are brought in.
	//
	// It is empty when the passwords are kept. When they are not, the
	// page has to say so: an account whose password was thrown away
	// cannot sign itself back in, and the person has to know that
	// before the account drops out.
	PasswordNote string

	tmpl    *template.Template
	mu      sync.Mutex
	pending map[string]*pendingLogin
	// pendingTTL is how long a sign-in that has ended is kept. It is a
	// field so that a test can make it short.
	pendingTTL time.Duration
}

// New makes the WebUI.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store) *WebUI {
	// Only the layout is parsed here. Every page defines "content",
	// and one set of templates for all of them would keep only the
	// last page's content, so every page would render the last one.
	tmpl := template.Must(template.New("mmxcode").Funcs(template.FuncMap{"num": num}).ParseFS(templateFS, "templates/layout.html"))
	return &WebUI{
		Mgr:        mgr,
		Cfg:        cfg,
		Store:      s,
		tmpl:       tmpl,
		pending:    map[string]*pendingLogin{},
		pendingTTL: defaultPendingTTL,
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
	case "/api/usage/refresh":
		w.apiUsageRefresh(rw, r)
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
	case "/api/import":
		w.apiImport(rw, r)
		return
	case "/api/relogin":
		w.apiRelogin(rw, r)
		return
	case "/api/keys":
		w.apiKeys(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/keys/") {
		w.apiKey(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
		w.apiAccount(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/import/") {
		w.apiImportRun(rw, r)
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
	case "/import":
		// 上传不再因为配了 key 就关着：那把 key 就是后台登录的那把，
		// 能登录的人就是管这个网关的人，明文密码走不走内网是他自己
		// 的选择。上传这个口子跟后台的其它页面一样，钥匙管着。
		w.page(rw, r, "import.html", w.importData)
	case "/relogin":
		// 重登页不接触明文密码：它登的是已经签出的账号，用的是库里
		// 存着的那一份。
		w.page(rw, r, "relogin.html", w.reloginData)
	case "/keys":
		w.page(rw, r, "keys.html", w.keysData)
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
			// 账号在 error 或者 signed_out 的时候，光有状态是没用的：
			// 上一次为什么失败，才是那个人要做的事。
			"LastError": a.LastError,
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
	s := usage.Aggregate(snaps, accounts, time.Now())
	return map[string]any{"Summary": s}, nil
}

// loginData is the login page's data.
func (w *WebUI) loginData(ctx context.Context) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pending {
		// A sign-in that failed is not waiting for anybody any more. It
		// is kept a while so that the page can say why it failed, and
		// it must not be shown as one that is still going on.
		if p.Err != "" {
			continue
		}
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
	if err != nil {
		w.failLogin(id, err.Error())
		return
	}
	now := time.Now()
	acc := store.Account{
		ID:        newAccountID(),
		Status:    store.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 账户是谁，要问站点本人：令牌里的那个 sub 是猜的，签到和积分
	// 明细要的是真的那个 user id。命令行就是这么做的，这里也一样。
	who, ierr := w.identity(ctx, tok)
	if ierr != nil {
		// 登录成功了，只是读不到账户信息：账号还是建起来，用令牌
		// 自己带着的东西填。
		fmt.Fprintf(os.Stderr, "webui: signed in, but the account's identity could not be read: %v\n", ierr)
	}
	// 名字不给：库里已经有这一行的时候，名字是上次那人自己写的标签，
	// 一次登录不该把它换掉。新账号的名字由库来起（地址、用户 id、令牌
	// 里带的那个 id）。
	acc.UserID = firstNonEmpty(who.RealUserID, tok.Subject)
	acc.Email = who.Email
	// 库里已经有这个站点账号了（多半是批量导入进来的）：改那一行，不再
	// 另起一行——同一个站点账号两行都是活的，轮转会挑到两次。
	acc, err = w.Store.SignIn(ctx, acc, store.Token{
		Access: tok.Access, Refresh: tok.Refresh,
		ExpiresAt: tok.Expires, Subject: tok.Subject,
	})
	if err != nil {
		// 建账号失败要说出来：不然这一次登录就悄悄没了，人只看到
		// 页面在转圈。
		w.failLogin(id, "the account could not be written down: "+err.Error())
		return
	}
	// 刚登进来的账号马上读一次账本：不等调度器那一趟
	// （usage.refresh.scan_every，默认 30 秒），人才看得到它有多少
	// 积分。读不到不算登录失败——账号是进来了的。
	w.refreshUsage(ctx, acc.ID)
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
}

// identity reads who the account is.
//
// The token's own claims are guesses, so the site is asked. A failure
// is not fatal: the sign-in worked, and the caller falls back to what
// the token carries.
func (w *WebUI) identity(ctx context.Context, tok *oauth.Token) (account.Identity, error) {
	if w.Mgr == nil || w.Mgr.Acc == nil {
		return account.Identity{}, errors.New("no account client")
	}
	return w.Mgr.Acc.Identity(ctx, tok.Access, tok.Subject)
}

// failLogin says the sign-in did not work.
//
// The reason is kept on the entry, so that the page can be told why.
// The entry itself goes away after a while: a server that is up for a
// long time must not keep every sign-in it ever had.
//
// The entry is not taken away at once, because the browser has to be
// able to read the reason. The page asks every few seconds, and it
// stops asking once it has the answer.
func (w *WebUI) failLogin(id string, reason string) {
	w.mu.Lock()
	p := w.pending[id]
	if p != nil {
		p.Err = reason
		p.FailedAt = time.Now()
	}
	ttl := w.pendingTTL
	w.mu.Unlock()
	if p == nil {
		return
	}
	time.AfterFunc(ttl, func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	})
}

// newAccountID makes an id for a new account.
//
// The time alone is not enough: two sign-ins that end in the same
// millisecond get the same id, and the second one is refused by the
// primary key. So the id has a random part too.
func newAccountID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 没有随机数的时候只能用时间：撞车的机会很小，撞上了也
		// 只是这一次登录白做。
		return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
	}
	return "acc_" + hex.EncodeToString(b)
}

// firstNonEmpty gives the first value that is not empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiPoll says how a sign-in is going.
func (w *WebUI) apiPoll(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	// The reason is read under the lock and copied out of it. It is
	// written under the same lock, and reading it outside would read
	// half of one string and half of another.
	w.mu.Lock()
	p := w.pending[body.ID]
	reason := ""
	if p != nil {
		reason = p.Err
	}
	w.mu.Unlock()
	if reason != "" {
		writeJSON(rw, map[string]any{"error": reason})
		return
	}
	if p == nil {
		writeJSON(rw, map[string]any{"done": true})
		return
	}
	writeJSON(rw, map[string]any{"done": false})
}

// apiUsageRefresh reads every account's credits now.
//
// 页面上那些数是上一次读数时的数。这一趟是现读的：不等调度器，也不
// 等下一次。读不到的账号要说得出来，人才知道去看哪一个。正在被别的
// 那一趟读着的账号被跳过了 —— 跳过的也要说得出来。
func (w *WebUI) apiUsageRefresh(rw http.ResponseWriter, r *http.Request) {
	res, err := w.Mgr.RefreshAllUsage(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, reason := range res.Reasons {
		log.Printf("usage: %s", reason)
	}
	if res.Skipped > 0 {
		log.Printf("usage: %d account(s) are being read by another round and were skipped", res.Skipped)
	}
	writeJSON(rw, map[string]any{
		"refreshed": res.Refreshed,
		"failed":    res.Failed,
		"skipped":   res.Skipped,
		"reasons":   res.Reasons,
	})
}

// apiUsage gives the credits summary.
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
	s := usage.Aggregate(snaps, accounts, time.Now())
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
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "no such account: "+id, http.StatusNotFound)
				return
			}
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
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
		// 名字和出口分开写。整行写回去会把别的写者在这中间改的东西抹掉：
		// 管理器刚写下的登出，会被这一次改名字抹回 active。
		if body.Name != nil && *body.Name != a.Name {
			if err := w.Store.SetAccountName(r.Context(), a.ID, *body.Name, time.Now()); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if body.ProxyGroup != nil || body.ProxyURL != nil {
			if err := w.Store.SetAccountProxy(r.Context(), a.ID, a.ProxyGroup, a.ProxyURL, time.Now()); err != nil {
				http.Error(rw, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		writeJSON(rw, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := w.Store.DeleteAccount(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(rw, "no such account: "+id, http.StatusNotFound)
				return
			}
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
	// 签到给的积分是账本池里的：读完这一账号的额度，卡上的余额才是
	// 签到之后的数。人在等，所以只读这一个。
	round := usage.NewRound(w.Store, w.Mgr, w.Mgr.HotAccounts, w.Cfg)
	if _, err := round.Settle(r.Context(), id); err != nil {
		fmt.Printf("checkin %s: the credits could not be read: %v\n", id, err)
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
	writeJSONStatus(rw, http.StatusOK, v)
}

// writeJSONStatus writes a JSON answer with a status code.
//
// The header is set before the status is written: after WriteHeader the
// header map is frozen, and a Content-Type set afterwards never reaches
// the client. Go sniffs the body instead and says text/plain, and the
// page's script then cannot read the answer — it reads nothing, and the
// upload looks like it failed even though it went through.
func writeJSONStatus(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

// maxImportBytes is how big an uploaded list may be.
//
// 一行一个账号，八兆字节已经是很长的一份清单了。
const maxImportBytes = 8 << 20

// importData is the import page's data.
func (w *WebUI) importData(ctx context.Context) (map[string]any, error) {
	runs, err := w.Store.ListImportRuns(ctx, 10)
	if err != nil {
		return nil, err
	}
	views := make([]importRunView, 0, len(runs))
	for _, r := range runs {
		views = append(views, importRunView{ImportRun: r, Whole: wholeReason(r.Results)})
	}
	return map[string]any{
		"Runs":         views,
		"PasswordNote": w.PasswordNote,
		"TextLines":    maxImportTextLines,
	}, nil
}

// importRunView is one import run as the page shows it.
//
// 整趟没成的导入，成功和失败都是零：光看那两个数，看不出这一趟出了什
// 么事，像是这一趟什么都没干。理由得摆在眼前。
type importRunView struct {
	store.ImportRun
	// Whole is why the run failed as a whole, when it did.
	Whole string
}

// wholeReason gives the reason a run failed as a whole.
//
// 整趟没成的时候，结果里有一行是整趟的（whole），它说的就是这一趟为什
// 么没成。那一行不是账号：失败的账号数是零，可这一趟确实没成，得说。
func wholeReason(results string) string {
	if strings.TrimSpace(results) == "" {
		return ""
	}
	var list []provision.Result
	if err := json.Unmarshal([]byte(results), &list); err != nil {
		return ""
	}
	for _, r := range list {
		if r.Whole {
			return r.Message
		}
	}
	return ""
}

// apiImport takes a list of accounts and brings them in.
//
// 两种交法，交的是同一份清单（每行 邮箱|密码）：
//
//   - 上传一个文件（multipart 的 file 那一栏）
//   - 贴一段清单（JSON 的 {"list": "…"}）
//
// The work goes on in the background, and the answer says which run to
// watch.
//
// The list is read into memory and thrown away afterwards: it holds
// passwords in the clear, and it must not land on the disk.
//
// 配了 key 也不关这个口子了：那把 key 就是后台登录的那把，能进来的人
// 就是管这个网关的人。明文密码走不走内网，是他自己的选择。
func (w *WebUI) apiImport(rw http.ResponseWriter, r *http.Request) {
	if w.Import == nil {
		http.Error(rw, "这个网关没有批量导入", http.StatusNotImplemented)
		return
	}
	var (
		entries []provision.Entry
		bad     []provision.BadLine
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		// 贴过来的一段清单。
		var body struct {
			List string `json:"list"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(rw, "这个请求不是一段清单", http.StatusBadRequest)
			return
		}
		if n := countLines(body.List); n > maxImportTextLines {
			http.Error(rw, fmt.Sprintf("这段清单有 %d 行，最多 %d 行", n, maxImportTextLines),
				http.StatusBadRequest)
			return
		}
		entries, bad = provision.Parse(strings.NewReader(body.List))
	} else {
		// 上传的一个文件。
		//
		// The request has a size limit, and the memory that
		// ParseMultipartForm is given is bigger than it: the list
		// therefore never reaches a temporary file, it stays in
		// memory.
		r.Body = http.MaxBytesReader(rw, r.Body, maxImportBytes)
		if err := r.ParseMultipartForm(maxImportBytes * 2); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(rw, "这个清单太大了", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(rw, "这个请求不是文件上传", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(rw, "没有文件", http.StatusBadRequest)
			return
		}
		defer file.Close()
		entries, bad = provision.Parse(file)
	}
	for _, b := range bad {
		// 坏行的那句话里没有密码：清单的解析器把密码摘掉了。
		log.Printf("import: 第 %d 行：%s", b.Line, b.Reason)
	}
	if len(entries) == 0 {
		http.Error(rw, "清单里没有能用的账号", http.StatusBadRequest)
		return
	}
	runID := newImportRunID()
	now := time.Now()
	if err := w.Store.CreateImportRun(r.Context(), store.ImportRun{
		ID: runID, Source: "web", StartedAt: now, Total: len(entries),
	}); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	go w.runImport(runID, entries)
	// 202：导入在后台跑着，这个回答只说这一次导入的号。
	writeJSONStatus(rw, http.StatusAccepted, map[string]any{"id": runID})
}

// maxImportTextLines is how many lines a pasted list may have.
//
// 贴的那一段是给人手写的那一份：一百行够写一百个账号，再多就该上传一
// 个文件了。上传的那一份是批量那一份，它自己的大小上限是
// maxImportBytes，行数不管。
const maxImportTextLines = 100

// countLines counts the lines of a pasted list.
//
// 末尾那几个空行不算：粘贴的时候多带一个换行是常事，那不该把一行算
// 成两行。
func countLines(text string) int {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// newImportRunID makes the name of one import run.
//
// 时间还不够：两次上传可能落在同一毫秒里，那样的第二个会被主键挡下。
func newImportRunID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run_%d", time.Now().UnixMilli())
	}
	return fmt.Sprintf("run_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// runImport brings a batch in and writes down what came of it.
//
// A run that fails as a whole says why, in two places: in the log, and
// in the results the page shows. The run has no column of its own for
// it, so the reason goes in as a row of its own — otherwise the page
// says only "failed 1" and the operator never learns that the key was
// wrong.
//
// 那一行不是账号，所以它不算进失败的账号数里：一个账号都没处理的这一趟，
// 失败的账号数是零。整趟为什么没成，由那一行自己说。
func (w *WebUI) runImport(runID string, entries []provision.Entry) {
	ctx := context.Background()
	results, err := w.Import(ctx, entries)
	if err != nil {
		log.Printf("import %s: %v", runID, err)
		results = append(results, provision.Result{
			Email:   "（整趟）",
			OK:      false,
			Whole:   true,
			Stage:   stageOf(err),
			Message: "整趟导入没能完成：" + err.Error(),
		})
	}
	ok := 0
	failed := 0
	for _, r := range results {
		switch {
		case r.OK:
			ok++
			// 刚进来的账号马上读一次账本：不等调度器那一趟（15 分
			// 钟），人才看得到它有多少积分。
			w.refreshUsage(ctx, r.AccountID)
		case !r.Whole:
			// 整趟没成的那一行不是账号：它不算进失败的账号数里。
			failed++
		}
	}
	if err := w.Store.FinishImportRun(ctx, runID, time.Now(), ok, failed, provision.ResultsJSON(results)); err != nil {
		log.Printf("import %s: %v", runID, err)
	}
}

// refreshUsage reads one account's credits now.
//
// 读不到不算导入失败：账号是进来了的，账本读不到是另一件事。它要
// 记下来，别让它悄悄没掉。
func (w *WebUI) refreshUsage(ctx context.Context, accountID string) {
	if w.Mgr == nil || accountID == "" {
		return
	}
	if _, err := w.Mgr.RefreshUsage(ctx, accountID); err != nil {
		log.Printf("import: %s 的积分读不到：%v", accountID, err)
	}
}

// stageOf says where an import failed, when it says.
//
// 整趟失败的时候，失败在哪儿也是话的一部分：错在哪一步，人要看得见。
func stageOf(err error) provision.Stage {
	var pe *provision.Error
	if errors.As(err, &pe) {
		return pe.Stage
	}
	return ""
}

// apiRelogin signs the accounts that are signed out in again.
//
// 理由要带回去：只说「失败 2」，人得自己去一个一个看是哪个账号、为什么。
// 这一趟根本没跑的时候也要说，不能让它看着像「没什么要登的」。
func (w *WebUI) apiRelogin(rw http.ResponseWriter, r *http.Request) {
	if w.Relogin == nil {
		http.Error(rw, "这个网关没有重登", http.StatusNotImplemented)
		return
	}
	res, err := w.Relogin(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{
		"fixed":   res.Fixed,
		"failed":  res.Failed,
		"reasons": res.Reasons,
	})
}

// apiImportRun says how one batch import is going.
func (w *WebUI) apiImportRun(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/import/")
	run, err := w.Store.GetImportRun(r.Context(), id)
	if err != nil {
		http.Error(rw, "没有这一次导入", http.StatusNotFound)
		return
	}
	writeJSON(rw, map[string]any{
		"id":          run.ID,
		"source":      run.Source,
		"total":       run.Total,
		"ok":          run.OK,
		"failed":      run.Failed,
		"results":     run.Results,
		"finished_at": !run.FinishedAt.IsZero(),
	})
}

// reloginData is the relogin page's data.
//
// Only the accounts that are signed out and still have a password are
// listed: the others cannot be signed in again, and saying so is
// better than a button that cannot work.
func (w *WebUI) reloginData(ctx context.Context) (map[string]any, error) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	for _, a := range accounts {
		if a.Status != store.StatusSignedOut {
			continue
		}
		cipher, err := w.Store.Password(ctx, a.ID)
		if err != nil || cipher == "" {
			continue
		}
		// 存着的密码是加密的，可它也不该到页面上：这里只说这个账号
		// 还能登回去。
		out = append(out, map[string]any{
			"ID": a.ID, "Name": a.Name, "Email": a.Email, "Error": a.LastError,
		})
	}
	return map[string]any{"SignedOut": out}, nil
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
	"bytes"
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
	"mmxcode/internal/secret"
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

// TestCheckEgressWritesTheHealth says the result of the outlet check
// reaches the database.
//
// 出口的健康检查写不进数据库，WebUI 的出口页就永远是"没查过"：
// 控制台说两个出口都是好的，表里一行也没有。
func TestCheckEgressWritesTheHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	cfg := config.Default()
	// "direct" 不走代理，请求直接到那台假服务器上。
	cfg.Proxy.Groups = map[string]string{"local": "direct"}
	sc := New(mgr, cfg)
	sc.target = srv.URL

	sc.CheckEgress(ctx)

	rows, err := s.EgressHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the health table has %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].URL != "direct" {
		t.Errorf("the row is for %q, want %q", rows[0].URL, "direct")
	}
	if !rows[0].OK {
		t.Errorf("the row says the outlet is not usable: %+v", rows[0])
	}
	if rows[0].Error != "" {
		t.Errorf("the row carries an error: %q", rows[0].Error)
	}
}

// TestCheckEgressWritesAFailedHealth says a broken outlet is written
// down as broken.
//
// 出口不通的时候，表里要有那一行，而且要写清楚为什么不通。
func TestCheckEgressWritesAFailedHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s.Migrate(ctx)
	defer s.Close()

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	cfg := config.Default()
	cfg.Proxy.Groups = map[string]string{"local": "direct"}
	sc := New(mgr, cfg)
	sc.target = srv.URL

	sc.CheckEgress(ctx)

	rows, err := s.EgressHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the health table has %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].OK {
		t.Errorf("the row says the outlet is usable: %+v", rows[0])
	}
	if rows[0].Error == "" {
		t.Error("the row does not say why the outlet is broken")
	}
}

func TestTheReloginJobSignsTheSignedOutAccountsInAgain(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusSignedOut,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "acc_01", cipher, now); err != nil {
		t.Fatal(err)
	}
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())
	mgr.Key = key
	mgr.ReloginFn = func(ctx context.Context, a store.Account, password string) error {
		return s.SaveToken(ctx, store.Token{
			AccountID: a.ID, Access: "a", Refresh: "r",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}

	sc := New(mgr, config.Default())
	sc.Relogin(ctx)

	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusActive {
		t.Fatalf("status = %q", got.Status)
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

// egressTarget is the address the outlets are checked against.
//
// It is MiniMax's own agent host: a proxy that cannot reach it cannot
// carry a request to MiniMax either.
const egressTarget = "https://agent.minimax.io/"

// egressWriteTimeout is how long the health row may take to be
// written.
//
// The write is a small one, and it must not hold the scheduler up.
const egressWriteTimeout = 5 * time.Second

// Scheduler runs the recurring work.
type Scheduler struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// target is the address the outlets are checked against. A test
	// points it at a stand-in, so that the check never leaves the
	// machine.
	target string
}

// New makes a scheduler.
func New(mgr *manager.Manager, cfg config.Config) *Scheduler {
	return &Scheduler{Mgr: mgr, Cfg: cfg, target: egressTarget}
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

// Relogin signs in again the accounts that are signed out.
//
// It runs on its own clock: the cooldown is how long the scheduler
// waits between two rounds, so a sign-in that failed is not retried
// every minute.
func (s *Scheduler) Relogin(ctx context.Context) {
	res, err := s.Mgr.ReloginAll(ctx, s.Cfg.Batch.Relogin.MaxAccounts)
	if err != nil {
		// 这一趟根本没跑：说清楚，不然看着像「没什么要登的」。
		fmt.Printf("relogin: %v\n", err)
		return
	}
	if res.Fixed > 0 || res.Failed > 0 {
		fmt.Printf("relogin: %d fixed, %d failed\n", res.Fixed, res.Failed)
	}
	for _, r := range res.Reasons {
		fmt.Printf("relogin: %s\n", r)
	}
}

// CheckEgress tests every outlet and records the result.
//
// The probe and the write have a context each. The probe's timeout is
// over when the answer is in, and a write under that context would be
// thrown away before it reaches the database: the health table would
// stay empty and the WebUI would say "not checked" for ever.
func (s *Scheduler) CheckEgress(ctx context.Context) {
	target := s.target
	if target == "" {
		target = egressTarget
	}
	for name, url := range s.Cfg.Proxy.Groups {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.Mgr.Egress.Check(probeCtx, url, target)
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		// The write runs under a context of its own: the caller's may
		// be over already, and the health of the outlet has to be
		// written down all the same.
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), egressWriteTimeout)
		_ = s.Mgr.Store.SetEgressHealth(writeCtx, url, err == nil, msg)
		writeCancel()
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

	// 重登是单独的活儿，不在续期器那条热路径上：续期器照旧把链断的
	// 账号标成 signed_out，签回来的活儿交给这里。
	if s.Cfg.Batch.Relogin.Enabled {
		every := s.Cfg.Batch.Relogin.Cooldown.Duration()
		if every <= 0 {
			every = time.Hour
		}
		go s.loop(ctx, every, s.Relogin)
	}

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
	"mmxcode/internal/manager"
	"mmxcode/internal/provision"
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
				return fmt.Errorf("no api key is configured, so the gateway would listen on %s for everyone; "+
					"put api_keys in the config or %s, or listen on 127.0.0.1",
					listen, APIKeysEnv)
			}

			ui := webui.New(e.mgr, e.cfg, e.store)
			e.wireImport(ui)
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

// wireImport puts the batch import and the re-login on the page.
//
// 页面上传和命令行导入走的是同一件事：批量入号和重登。
//
// 密钥有问题的时候导入要停下来：悄悄把密码丢掉，会留下一库谁也登不
// 回去的账号。
//
// 没有密钥，密码就不入库。页面上得说清楚：账号掉线以后没有人能自己登
// 回去，人得知道这件事。
//
// 这一段单独拿出来，是为了能测它：那句提醒是接线上的，不是模板里的。
func (e *env) wireImport(ui *webui.WebUI) {
	keeper, keeperErr := passwordKeeper(false)
	ui.PasswordNote = passwordNote(keeper, keeperErr)
	ui.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		if keeperErr != nil {
			return nil, keeperErr
		}
		runner, err := e.runner(e.cfg.Batch.Group, e.cfg.Batch.Interval.Duration(), keeper)
		if err != nil {
			return nil, err
		}
		return runner.Run(ctx, entries)
	}
	ui.Relogin = func(ctx context.Context) (manager.ReloginResult, error) {
		return e.mgr.ReloginAll(ctx, e.cfg.Batch.Relogin.MaxAccounts)
	}
}

// passwordNote says what happens to the passwords, when they are not
// kept.
//
// 没有密钥的时候密码就不入库，页面上得说清楚：账号掉线以后没有人能
// 自己登回去。密钥有问题的时候导入自己就停了，页面不用多说。
func passwordNote(keeper func(string) (string, error), keeperErr error) string {
	if keeperErr != nil || keeper != nil {
		return ""
	}
	return "密码没有存进库里（没有 " + PasswordKeyEnv + "）：账号掉线以后没有人能自己登回去。"
}

// isLoopback says a listen address is only on this machine.
//
// ":8080" and "0.0.0.0:8080" are not: they mean every interface of the
// machine, so anyone who can reach it can use the accounts.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	if host == "localhost" {
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
// This file holds the mmxcode command line.
//
// Everything in this directory is package main: the directory is the
// binary, and a directory holds one package.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/provision"
	"mmxcode/internal/secret"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// Execute runs the command line and gives the process its exit code.
func Execute() int {
	root, _ := newRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// APIKeysEnv is the environment variable that holds the api keys.
//
// 镜像里不带密钥：密钥是申请来的，写进镜像就等于把它摊在别人眼前。
// 它从环境变量进来，配置文件里可以一把都没有。
const APIKeysEnv = "MMXCODE_API_KEYS"

// apiKeysFromEnv gives the api keys the environment brings.
//
// 逗号分隔。一把空的 key 等于没有 key：它会被去掉。
func apiKeysFromEnv(v string) []string {
	var out []string
	for _, k := range strings.Split(v, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// env is what the commands share.
type env struct {
	cfg   config.Config
	store *store.Store
	up    *upstream.Client
	egr   *egress.Resolver
	acc   *account.Client
	mgr   *manager.Manager

	// makeRunner makes the batch driver. It is a field so that a test
	// can run a batch without a network.
	makeRunner func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner
}

func newRoot() (*cobra.Command, *env) {
	var (
		configPath string
		dbPath     string
	)
	root := &cobra.Command{
		Use:          "mmxcode",
		Short:        "MiniMax Code 多账号网关",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", os.Getenv("MMXCODE_CONFIG"), "配置文件路径")
	root.PersistentFlags().StringVar(&dbPath, "db", "", "数据库路径（覆盖配置）")

	e := &env{}
	ctx := context.Background()

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if dbPath != "" {
			cfg.DB = dbPath
		}
		if v := os.Getenv("MMXCODE_PROXY_DEFAULT"); v != "" {
			cfg.Proxy.Default = v
		}
		// 密钥也可以从环境变量来：镜像里不带密钥，密钥是申请来的。
		if v := os.Getenv(APIKeysEnv); v != "" {
			cfg.Server.APIKeys = append(cfg.Server.APIKeys, apiKeysFromEnv(v)...)
		}
		e.cfg = cfg
		s, err := store.Open(cfg.DB)
		if err != nil {
			return err
		}
		if err := s.Migrate(ctx); err != nil {
			return err
		}
		e.store = s
		e.egr = egress.New(cfg.Proxy.Groups, cfg.Proxy.Default)
		// 登录和账户 API 也走代理。配了不用等于没配：国内的网络
		// 连不上国际站，登录就停在第一步。
		tr, err := e.egr.Transport(e.egr.Resolve(store.Account{}))
		if err != nil {
			return err
		}
		e.up = upstream.New(upstream.Options{Timeout: 30 * time.Second, Transport: tr})
		e.acc = account.New(e.up)
		e.mgr = manager.New(s, e.up, e.acc, e.egr, oauth.NewRenewer())
		// 重登用的密钥：没有它，账号掉线了就只能自己再登一次。
		if key, err := secret.FromEnv(PasswordKeyEnv); err == nil {
			e.mgr.Key = key
		} else if strings.TrimSpace(os.Getenv(PasswordKeyEnv)) != "" {
			// 密钥设了但是不对：说一声，别让人以为密码存下了。
			fmt.Fprintf(os.Stderr, "warning: %s is set but unusable: %v\n", PasswordKeyEnv, err)
		}
		e.mgr.ReloginFn = e.reloginFn("")
		return nil
	}
	root.PersistentPostRun = func(cmd *cobra.Command, args []string) {
		if e.store != nil {
			e.store.Close()
		}
	}

	root.AddCommand(newLoginCmd(e))
	root.AddCommand(newImportCmd(e))
	root.AddCommand(newReloginCmd(e))
	root.AddCommand(newAccountsCmd(e))
	root.AddCommand(newCheckinCmd(e))
	root.AddCommand(newUsageCmd(e))
	root.AddCommand(newModelsCmd(e))
	root.AddCommand(newServeCmd(e))
	return root, e
}

func nowUTC() time.Time { return time.Now().UTC() }

// groupNames lists the outlet groups, for an error message.
func groupNames(groups map[string]string) string {
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
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

# 受限网络下构建时要给出出口：docker build --build-arg GOPROXY=...
# 默认值就是 go 自己的默认值，不传也不影响。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

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

# 默认配置打进镜像：没有 bind mount 时镜像自己也能跑。
# 想改配置就挂载自己的文件盖住它。
COPY config.docker.yaml /app/config.yaml

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

# 本地的东西不进构建上下文：配置文件里有 api key，.env 里也是。
config.yaml
config.local.yaml
.env
.env.*
!.env.example

# 计划同步脚本和别的工作文件
.tmp-review
.worktrees
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
      # 数据目录。默认用 Docker 命名卷：Docker 会按镜像里 /data 的属主
      # （uid 10001）创建它，容器里的 mmxcode 用户可以直接写。
      # 想把数据库留在宿主机目录上就设 MMXCODE_DATA_DIR=./data，
      # 但宿主机那个目录必须对 uid 10001 可写，否则容器建不了数据库文件：
      #   mkdir -p data && chown 10001:10001 data
      - ${MMXCODE_DATA_DIR:-mmxcode-data}:/data
      # 默认挂仓库里自带的 config.docker.yaml；要用自己的副本就设
      # MMXCODE_CONFIG_FILE=./config.yaml（相对路径按 compose 文件所在目录解析）。
      - ${MMXCODE_CONFIG_FILE:-./config.docker.yaml}:/app/config.yaml:ro
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
  # 数据库数据目录（见上面 MMXCODE_DATA_DIR 的说明）。
  mmxcode-data:
  warp-state:
```

- [ ] **Step 4: 写 .env.example**

创建 `.env.example`：

```bash
# 复制成 .env 后按需改。

# 容器内走宿主机的代理。要用别的出口就改这里。
MMXCODE_PROXY_DEFAULT=http://host.docker.internal:7890

# 挂进容器的配置文件（宿主机路径）。
# 默认用仓库自带的 config.docker.yaml；要改成自己的副本就写绝对路径，
# 例如 MMXCODE_CONFIG_FILE=./config.yaml（相对路径按 compose 文件所在目录解析）。
MMXCODE_CONFIG_FILE=./config.docker.yaml

# 数据目录的挂载源（宿主机侧）。
# 默认 mmxcode-data = Docker 命名卷：Docker 按镜像里 /data 的属主（uid 10001）
# 创建它，容器里的 mmxcode 用户（uid 10001）能直接读写，数据库落在
# /data/mmxcode.db。
# 想把数据库留在宿主机目录上就写 MMXCODE_DATA_DIR=./data（相对路径按 compose
# 文件所在目录解析）。注意：宿主机那个目录必须对 uid 10001 可写，否则容器
# 建不了数据库文件、启动即失败：
#   mkdir -p data && chown 10001:10001 data
MMXCODE_DATA_DIR=mmxcode-data

# 开启 Cloudflare WARP 出口：把这行的值写成 warp。
# 默认是关的 —— 下面这行保持注释，容器就不会启动 WARP。
# 打开时（取消注释）宿主机的 /dev/net/tun 要可用，容器需要 NET_ADMIN。
#COMPOSE_PROFILES=warp
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

### Task 11: 实现期新增的文件

计划里原来没有这些文件，实现的时候加上了。

上游只认 Anthropic 那一条路：参考实现只打过 `/messages`，OpenAI 的
`/chat/completions` 和 Codex 的 `/responses` 是我们本地转出来的。
`internal/convert/openai.go` 与 `internal/convert/responses.go` 就是
这两个转换，其余是给它们的测试，以及网关的失败形状和 WebUI 登录页
的测试。

**Files:**
- Create: `internal/convert/openai.go`
- Create: `internal/convert/responses.go`
- Create: `internal/convert/openai_test.go`
- Create: `internal/gateway/failures_test.go`
- Create: `internal/gateway/openai_test.go`
- Create: `internal/webui/login_test.go`

- [ ] **Step 1: 写转换与测试**

创建 `internal/convert/openai.go`：

```go
package convert

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// This file is the OpenAI side of the conversion skeleton.
//
// The reference plugin only ever calls the Anthropic endpoint. When the
// site does not serve /chat/completions, an OpenAI request is turned
// into an Anthropic one here, and the Anthropic answer is turned back
// into the OpenAI shape — the same skeleton the Codex path rides on,
// with another shape at each end.

// chatRequest is the part of an OpenAI chat request that is read.
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Stream              bool            `json:"stream"`
	Tools               []chatTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	Stop                json.RawMessage `json:"stop"`
}

// chatMessage is one message of an OpenAI request.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// chatToolCall is one tool call of an assistant message.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatTool is one tool of an OpenAI request.
type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ToAnthropicOpenAI turns an OpenAI chat request into an Anthropic
// Messages request.
//
// The model, the messages, the tools, the tool choice and the
// streaming flag all come across. The OpenAI tool calls and tool
// results become Anthropic's tool_use and tool_result blocks.
//
// The system prompt is put in Anthropic's own place: an OpenAI request
// carries it as a message of its own, and Anthropic has a field for
// it.
func ToAnthropicOpenAI(body []byte) ([]byte, error) {
	var in chatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	out := anthropicRequest{
		Model:     in.Model,
		Stream:    in.Stream,
		MaxTokens: in.MaxTokens,
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = in.MaxCompletionTokens
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = 4096
	}
	var system []string
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			// Anthropic's system prompt is its own field: the same
			// words in a different place.
			if t := chatText(m.Content); t != "" {
				system = append(system, t)
			}
			continue
		case "user":
			out.Messages = append(out.Messages, anthropicMessage{
				Role: "user", Content: chatTextBlocks(m.Content),
			})
		case "assistant":
			blocks := chatBlocks(m.Content)
			for _, tc := range m.ToolCalls {
				input, err := toolInput(json.RawMessage(tc.Function.Arguments))
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.ID,
					Name: tc.Function.Name, Input: input,
				})
			}
			if len(blocks) == 0 {
				// 一条什么也没有的助手消息在 Anthropic 那边不成立。
				blocks = []anthropicBlock{{Type: "text", Text: ""}}
			}
			out.Messages = append(out.Messages, anthropicMessage{
				Role: "assistant", Content: blocks,
			})
		case "tool", "function":
			// Anthropic puts the result of a tool in a user message.
			out.Messages = append(out.Messages, anthropicMessage{
				Role: "user", Content: []anthropicBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID,
					Content: chatText(m.Content),
				}},
			})
		default:
			// 别的角色不认，跳过。
		}
	}
	out.System = strings.Join(system, "\n\n")
	if len(out.Messages) == 0 {
		out.Messages = []anthropicMessage{{
			Role: "user", Content: []anthropicBlock{{Type: "text", Text: ""}},
		}}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	out.ToolChoice = openAIToolChoice(in.ToolChoice)
	out.StopSequences = chatStop(in.Stop)
	return json.Marshal(out)
}

// openAIToolChoice turns the OpenAI tool_choice into Anthropic's.
//
// The same words as the Responses protocol: "required" is Anthropic's
// "any", and a named function is a named tool.
func openAIToolChoice(raw json.RawMessage) *anthropicToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &anthropicToolChoice{Type: "auto"}
		case "none":
			return &anthropicToolChoice{Type: "none"}
		case "required":
			return &anthropicToolChoice{Type: "any"}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Type == "function" && obj.Function.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: obj.Function.Name}
	}
	return nil
}

// chatStop reads the stop sequences out of an OpenAI request.
//
// The field is a string or a list of strings; both are read.
func chatStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// chatText reads the text out of an OpenAI message's content.
func chatText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// chatBlocks turns an OpenAI message's content into Anthropic blocks.
//
// The parts that carry text come across; the pictures and the files do
// not: their shapes differ, and a picture turned into a piece of text
// would be a lie.
func chatBlocks(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]anthropicBlock, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			out = append(out, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
	return out
}

// chatTextBlocks is the blocks of a message that has to say something.
//
// Anthropic 的消息不能是空的：一条没有内容的消息不成立，所以这里
// 补一个空的文字块。
func chatTextBlocks(raw json.RawMessage) []anthropicBlock {
	blocks := chatBlocks(raw)
	if len(blocks) == 0 {
		return []anthropicBlock{{Type: "text", Text: ""}}
	}
	return blocks
}

// ToChatCompletion turns an Anthropic answer into an OpenAI chat
// completion.
//
// A tool_use block becomes a tool_calls entry and the reason for the
// end becomes "tool_calls": a client that is told "stop" while the
// answer holds a tool call waits for nothing.
func ToChatCompletion(body []byte, model string) ([]byte, error) {
	var msg anthropicAnswer
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("chat: the upstream's answer could not be read")
	}
	if model == "" {
		model = msg.Model
	}
	id := msg.ID
	if id == "" {
		id = randomID("chatcmpl")
	}
	var content strings.Builder
	var toolCalls []any
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			content.WriteString(c.Text)
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{
					"name": c.Name, "arguments": toolArguments(c.Input),
				},
			})
		}
	}
	finish := finishReason(msg.StopReason)
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	message := map[string]any{
		"role": "assistant", "content": content.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
	}
	if msg.Usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     msg.Usage.InputTokens,
			"completion_tokens": msg.Usage.OutputTokens,
			"total_tokens":      msg.Usage.InputTokens + msg.Usage.OutputTokens,
		}
	}
	return json.Marshal(out)
}

// chatBlock is what is known about one upstream content block.
type chatBlock struct {
	// kind is "text" or "tool".
	kind string
	// index is the tool call's index in the chunk's tool_calls.
	index int
	// id is the tool call's id.
	id string
	// name is the tool's name.
	name string
	// args are the tool's arguments so far.
	args string
	// input is the whole input the upstream gave with the block.
	input json.RawMessage
}

// chatConverter turns the upstream's Anthropic stream into an OpenAI
// chat completion stream.
type chatConverter struct {
	bw    *bufio.Writer
	model string
	// id is the id of this completion. It is made here and not taken
	// from the upstream: two requests must not answer with the same
	// one.
	id string
	// created is when the answer was made.
	created int64

	blocks map[int]*chatBlock
	// nextIndex is the index the next tool call gets.
	nextIndex int
	// toolCalls says whether the answer holds a tool call.
	toolCalls bool

	// stopReason is why the answer ended.
	stopReason string
	// sawStop says the upstream's stream ended the way it should.
	sawStop bool
}

// StreamChatCompletions turns the upstream's Anthropic stream into an
// OpenAI chat completion stream.
//
// The pieces go out as they come, so a client that is waiting for the
// first token gets it as soon as the upstream sends it. The stream
// ends with a chunk that carries the reason for the end and with
// "[DONE]", which is how an OpenAI client knows the answer is whole.
func StreamChatCompletions(r io.Reader, w io.Writer, model string) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	c := &chatConverter{
		bw:      bw,
		model:   model,
		id:      randomID("chatcmpl"),
		created: time.Now().Unix(),
		blocks:  map[int]*chatBlock{},
	}
	return c.run(r)
}

// run reads the upstream's stream and writes the OpenAI stream.
func (c *chatConverter) run(r io.Reader) error {
	// 第一块带上角色：OpenAI 的客户端从这一块开始读。
	if err := c.chunk(map[string]any{"role": "assistant", "content": ""}); err != nil {
		return err
	}
	err := readSSE(r, func(name, payload string) error {
		payload = strings.TrimSpace(payload)
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			// 有些上游用 [DONE] 收尾，它也是一个像样的结尾。
			c.sawStop = true
			return nil
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			if name == "error" {
				return c.fail(payload)
			}
			return nil
		}
		if _, msg, bad := upstreamError(name, &ev); bad {
			return c.fail(msg)
		}
		if ev.Type == "" && name != "" {
			ev.Type = name
		}
		return c.event(&ev)
	})
	if err != nil {
		var ce *chatFailedError
		if errors.As(err, &ce) {
			// 已经报过了
			return err
		}
		return c.fail(err.Error())
	}
	if !c.sawStop {
		// 断了的流不是一个完整的答案：客户端得知道它没有拿到整个
		// 答案，而不是以为答案就这样。
		return c.fail("the upstream's stream ended before the answer was done")
	}
	return c.finish()
}

// event takes one event of the upstream's stream.
func (c *chatConverter) event(ev *anthropicEvent) error {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && c.model == "" {
			c.model = ev.Message.Model
		}
	case "content_block_start":
		return c.startBlock(ev)
	case "content_block_delta":
		return c.blockDelta(ev)
	case "content_block_stop":
		return c.stopBlock(ev)
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			c.stopReason = ev.Delta.StopReason
		}
	case "message_stop":
		c.sawStop = true
	}
	return nil
}

// startBlock takes the opening of one upstream content block.
func (c *chatConverter) startBlock(ev *anthropicEvent) error {
	if ev.ContentBlock == nil {
		return nil
	}
	switch ev.ContentBlock.Type {
	case "text", "thinking":
		c.blocks[ev.Index] = &chatBlock{kind: "text"}
		return nil
	case "tool_use":
		st := &chatBlock{
			kind: "tool", index: c.nextIndex,
			id: ev.ContentBlock.ID, name: ev.ContentBlock.Name,
			input: ev.ContentBlock.Input,
		}
		c.nextIndex++
		c.toolCalls = true
		c.blocks[ev.Index] = st
		// 一次工具调用本身就是一件事：它的名字和 id 马上就来。
		return c.toolChunk(map[string]any{
			"index": st.index, "id": st.id, "type": "function",
			"function": map[string]any{"name": st.name, "arguments": ""},
		})
	default:
		// 图片、文档之类的块在 OpenAI 的回答里没有形状，跳过。
		c.blocks[ev.Index] = &chatBlock{kind: "skipped"}
		return nil
	}
}

// blockDelta takes one piece of an upstream content block.
func (c *chatConverter) blockDelta(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok || ev.Delta == nil {
		return nil
	}
	switch ev.Delta.Type {
	case "text_delta":
		if st.kind != "text" || ev.Delta.Text == "" {
			return nil
		}
		return c.chunk(map[string]any{"content": ev.Delta.Text})
	case "input_json_delta":
		if st.kind != "tool" {
			return nil
		}
		st.args += ev.Delta.PartialJSON
		return c.toolChunk(map[string]any{
			"index":    st.index,
			"function": map[string]any{"arguments": ev.Delta.PartialJSON},
		})
	}
	return nil
}

// stopBlock takes the end of one upstream content block.
func (c *chatConverter) stopBlock(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok || st.kind != "tool" {
		return nil
	}
	if st.args == "" {
		// 参数是一次给全的，或者根本没有参数。
		st.args = toolArguments(st.input)
		if st.args == "" {
			st.args = "{}"
		}
		return c.toolChunk(map[string]any{
			"index":    st.index,
			"function": map[string]any{"arguments": st.args},
		})
	}
	return nil
}

// finish writes the end of the answer.
func (c *chatConverter) finish() error {
	reason := finishReason(c.stopReason)
	if c.toolCalls {
		reason = "tool_calls"
	}
	if err := c.write(map[string]any{
		"index": 0, "delta": map[string]any{}, "finish_reason": reason,
	}); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.bw, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return c.bw.Flush()
}

// chunk writes one chunk of the OpenAI stream.
func (c *chatConverter) chunk(delta map[string]any) error {
	return c.write(map[string]any{
		"index": 0, "delta": delta, "finish_reason": nil,
	})
}

// toolChunk writes one chunk that carries a piece of a tool call.
func (c *chatConverter) toolChunk(call map[string]any) error {
	return c.write(map[string]any{
		"index":         0,
		"delta":         map[string]any{"tool_calls": []any{call}},
		"finish_reason": nil,
	})
}

// write writes one chunk of the OpenAI stream.
func (c *chatConverter) write(choice map[string]any) error {
	b, err := json.Marshal(map[string]any{
		"id": c.id, "object": "chat.completion.chunk",
		"created": c.created, "model": c.model,
		"choices": []any{choice},
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.bw, "data: %s\n\n", b); err != nil {
		return err
	}
	return c.bw.Flush()
}

// chatFailedError is the error of a stream that could not be finished.
type chatFailedError struct {
	msg string
}

func (e *chatFailedError) Error() string { return e.msg }

// fail writes the failure of the answer.
//
// An OpenAI client reads the failure out of a chunk like any other
// one, so that is where it goes.
func (c *chatConverter) fail(msg string) error {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error"},
	})
	fmt.Fprintf(c.bw, "data: %s\n\n", b)
	fmt.Fprintf(c.bw, "data: [DONE]\n\n")
	c.bw.Flush()
	return &chatFailedError{msg: msg}
}
```

创建 `internal/convert/responses.go`：

```go
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicAnswer is the shape of an Anthropic message.
//
// It is the answer of the Anthropic protocol, whole or in pieces: the
// same fields are in the message of a whole answer and in the
// content_block of a stream.
type anthropicAnswer struct {
	// ID is the message's id.
	ID string `json:"id"`
	// Model is the model that answered.
	Model string `json:"model"`
	// StopReason says why the answer ended.
	StopReason string `json:"stop_reason"`
	// Content is the answer's blocks.
	Content []anthropicContent `json:"content"`
	// Usage says how many tokens were used.
	Usage *anthropicUsage `json:"usage"`
}

// anthropicContent is one block of an answer.
type anthropicContent struct {
	// Type is the block's kind: text, thinking, tool_use.
	Type string `json:"type"`
	// Text is the text of a text block.
	Text string `json:"text"`
	// Thinking is the text of a thinking block.
	Thinking string `json:"thinking"`
	// ID is the id of a tool_use block.
	ID string `json:"id"`
	// Name is the tool's name, for a tool_use block.
	Name string `json:"name"`
	// Input is the tool's arguments, for a tool_use block.
	Input json.RawMessage `json:"input"`
}

// anthropicUsage is what an answer says about its tokens.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ToResponses turns an Anthropic answer into a Responses answer.
//
// The items are the ones the streaming converter writes for the very
// same answer: a text block is a message item, a thinking block is a
// reasoning item, a tool_use block is a function call item. The two
// paths have to agree — an answer must not look different depending
// on whether the client asked for a stream.
//
// A block the Responses protocol has no shape for is left out, not
// faked: it gets no item at all.
func ToResponses(body []byte, model string) ([]byte, error) {
	var msg anthropicAnswer
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("responses: the upstream's answer could not be read")
	}
	if model == "" {
		model = msg.Model
	}
	items := []any{}
	var text, reasoning strings.Builder
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
			items = append(items, map[string]any{
				"type": "message", "id": randomID("msg"),
				"role": "assistant", "status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": c.Text, "annotations": []any{},
				}},
			})
		case "thinking":
			reasoning.WriteString(c.Thinking)
			items = append(items, map[string]any{
				"type": "reasoning", "id": randomID("rs"),
				"summary": []any{map[string]any{
					"type": "summary_text", "text": c.Thinking,
				}},
			})
		case "tool_use":
			items = append(items, map[string]any{
				"type": "function_call", "id": randomID("fc"),
				"call_id": c.ID, "name": c.Name,
				"arguments": toolArguments(c.Input),
				"status":    "completed",
			})
		}
	}
	out := map[string]any{
		"id":     randomID("resp"),
		"object": "response",
		"status": "completed",
		"model":  model,
		"output": items,
		// output_text 是给不读 output 的客户端准备的：它要的只是那段话。
		"output_text": text.String(),
	}
	if reasoning.Len() > 0 {
		out["reasoning_summary_text"] = reasoning.String()
	}
	if msg.Usage != nil {
		out["usage"] = map[string]any{
			"input_tokens":  msg.Usage.InputTokens,
			"output_tokens": msg.Usage.OutputTokens,
			"total_tokens":  msg.Usage.InputTokens + msg.Usage.OutputTokens,
		}
	}
	return json.Marshal(out)
}

// finishReason says what an Anthropic stop reason means to a client
// that only knows the OpenAI wording.
func finishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
```

创建 `internal/convert/openai_test.go`：

```go
package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToResponsesKeepsTheToolCall says the tool call of an Anthropic
// answer is a function call item in the Responses answer.
//
// 回答里的工具调用块在 Responses 里有一个自己的样子。把它丢掉，模型
// 就永远调不了工具：客户端收到的是一个说完话了的回答。
func TestToResponsesKeepsTheToolCall(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","model":"m","content":[
		{"type":"text","text":"ok"},
		{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Beijing"}}
	]}`)
	out, err := ToResponses(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText string `json:"output_text"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("the answer = %s", out)
	}
	if resp.OutputText != "ok" {
		t.Fatalf("output_text = %q", resp.OutputText)
	}
	var call *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	for i := range resp.Output {
		if resp.Output[i].Type == "function_call" {
			call = &resp.Output[i]
		}
	}
	if call == nil {
		t.Fatalf("the tool call was thrown away: %s", out)
	}
	if call.CallID != "toolu_1" || call.Name != "get_weather" {
		t.Fatalf("the tool call = %+v", call)
	}
	if call.Arguments != `{"city":"Beijing"}` {
		t.Fatalf("arguments = %q", call.Arguments)
	}
	if call.Status != "completed" {
		t.Fatalf("status = %q", call.Status)
	}
	if call.ID == "" {
		t.Fatal("the tool call has no id of its own")
	}
}

// TestToResponsesHasTheSameItemsAsTheStream says the whole answer and
// the stream agree on what the items look like.
//
// 同一个上游答案，两条路要给客户端看一样的形状。
func TestToResponsesHasTheSameItemsAsTheStream(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","content":[
		{"type":"text","text":"ok"},
		{"type":"thinking","thinking":"hmm"},
		{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}
	]}`)
	out, err := ToResponses(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, item := range resp.Output {
		kinds = append(kinds, item.Type)
	}
	want := []string{"message", "reasoning", "function_call"}
	if len(kinds) != len(want) {
		t.Fatalf("the items are %v, want %v", kinds, want)
	}
	for i, k := range kinds {
		if k != want[i] {
			t.Fatalf("the items are %v, want %v", kinds, want)
		}
	}
	if len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Type != "output_text" {
		t.Fatalf("the message item = %+v", resp.Output[0])
	}
	if len(resp.Output[1].Summary) != 1 || resp.Output[1].Summary[0].Text != "hmm" {
		t.Fatalf("the reasoning item = %+v", resp.Output[1])
	}
}

// TestToResponsesCarriesTheUsage says the upstream's usage comes along.
func TestToResponsesCarriesTheUsage(t *testing.T) {
	answer := []byte(`{"type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	out, err := ToResponses(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Usage == nil {
		t.Fatalf("the usage was thrown away: %s", out)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 || resp.Usage.TotalTokens != 15 {
		t.Fatalf("the usage = %+v", resp.Usage)
	}
}

// TestToAnthropicOpenAITurnsAMessage says an OpenAI request becomes an
// Anthropic one.
func TestToAnthropicOpenAITurnsAMessage(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"you are helpful"},
			{"role":"user","content":"hi"}
		],
		"max_completion_tokens":32
	}`)
	out, err := ToAnthropicOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "m" {
		t.Fatalf("model = %q", req.Model)
	}
	if req.System != "you are helpful" {
		t.Fatalf("system = %q", req.System)
	}
	if req.MaxTokens != 32 {
		t.Fatalf("max_tokens = %d", req.MaxTokens)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[0].Role != "user" {
		t.Fatalf("role = %q", req.Messages[0].Role)
	}
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("content = %+v", req.Messages[0].Content)
	}
}

// TestToAnthropicOpenAITurnsTheTools says the tools and the tool
// choice come across.
func TestToAnthropicOpenAITurnsTheTools(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}
	}`)
	out, err := ToAnthropicOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "f" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if len(req.Tools[0].InputSchema) == 0 {
		t.Fatalf("the schema was thrown away: %s", out)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != "tool" || req.ToolChoice.Name != "f" {
		t.Fatalf("tool_choice = %+v", req.ToolChoice)
	}
}

// TestToAnthropicOpenAITurnsTheToolCalls says the tool calls of a
// conversation come across as tool_use blocks.
func TestToAnthropicOpenAITurnsTheToolCalls(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"42"}
		]
	}`)
	out, err := ToAnthropicOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   string          `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 {
		t.Fatalf("the assistant message = %+v", assistant)
	}
	block := assistant.Content[0]
	if block.Type != "tool_use" || block.ID != "call_1" || block.Name != "f" {
		t.Fatalf("the tool_use block = %+v", block)
	}
	if string(block.Input) != `{"a":1}` {
		t.Fatalf("the tool's input = %s", block.Input)
	}
	result := req.Messages[2]
	if result.Role != "user" || len(result.Content) != 1 {
		t.Fatalf("the tool result message = %+v", result)
	}
	if result.Content[0].Type != "tool_result" || result.Content[0].ToolUseID != "call_1" {
		t.Fatalf("the tool_result block = %+v", result.Content[0])
	}
	if result.Content[0].Content != "42" {
		t.Fatalf("the result = %q", result.Content[0].Content)
	}
}

// TestToChatCompletionSaysWhatWasSaid says an Anthropic answer becomes
// an OpenAI chat completion.
func TestToChatCompletionSaysWhatWasSaid(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	out, err := ToChatCompletion(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q", completion.Object)
	}
	if completion.Created == 0 {
		t.Fatal("the completion has no time")
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %+v", completion.Choices)
	}
	if completion.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
	if completion.Usage.PromptTokens != 3 || completion.Usage.CompletionTokens != 2 || completion.Usage.TotalTokens != 5 {
		t.Fatalf("the usage = %+v", completion.Usage)
	}
}

// TestToChatCompletionTurnsTheToolCall says the tool call of an
// Anthropic answer is an OpenAI tool call.
func TestToChatCompletionTurnsTheToolCall(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}]}`)
	out, err := ToChatCompletion(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatal(err)
	}
	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %+v", calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Type != "function" {
		t.Fatalf("the tool call = %+v", calls[0])
	}
	if calls[0].Function.Name != "f" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
}

// TestStreamChatCompletionsGivesTheChunks says the Anthropic stream
// becomes an OpenAI stream.
func TestStreamChatCompletionsGivesTheChunks(t *testing.T) {
	up := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":5}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	var out strings.Builder
	if err := StreamChatCompletions(strings.NewReader(up), &out, "m"); err != nil {
		t.Fatal(err)
	}
	stream := out.String()
	if !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
		t.Fatalf("the stream does not end with [DONE]: %s", stream)
	}
	var text strings.Builder
	var finish string
	chunks := 0
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("object = %q", chunk.Object)
		}
		chunks++
		text.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if chunks < 2 {
		t.Fatalf("the stream has %d chunks: %s", chunks, stream)
	}
	if text.String() != "hi" {
		t.Fatalf("the text = %q", text.String())
	}
	if finish != "stop" {
		t.Fatalf("finish_reason = %q", finish)
	}
}

// TestStreamChatCompletionsGivesTheToolCall says the tool call of a
// stream is an OpenAI tool call.
func TestStreamChatCompletionsGivesTheToolCall(t *testing.T) {
	up := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	var out strings.Builder
	if err := StreamChatCompletions(strings.NewReader(up), &out, "m"); err != nil {
		t.Fatal(err)
	}
	stream := out.String()
	var name, args, id string
	var finish string
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		for _, call := range chunk.Choices[0].Delta.ToolCalls {
			if call.Function.Name != "" {
				name = call.Function.Name
			}
			if call.ID != "" {
				id = call.ID
			}
			args += call.Function.Arguments
		}
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if name != "f" {
		t.Fatalf("the tool's name = %q", name)
	}
	if id != "toolu_1" {
		t.Fatalf("the tool call's id = %q", id)
	}
	if args != `{"a":1}` {
		t.Fatalf("the arguments = %q", args)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q", finish)
	}
}
```

创建 `internal/gateway/failures_test.go`：

```go
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAnUpstreamThatIsDownIsNotARateLimit says a 503 from the upstream
// reaches the client as a 503.
//
// 上游说它自己坏了，客户端就该看到上游坏了。把它说成 429，客户端会
// 当成自己被限流：退避，过一会儿再来，再问一个还是坏了的上游。运维
// 看到的也是"被限流了"，而不是"上游挂了"。
func TestAnUpstreamThatIsDownIsNotARateLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"the upstream is down"}`))
	})
	// 一次就够：这里要看的不是重试，是最后给客户端的那个状态。
	h.gw.Cfg.Rotation.MaxAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503, body = %s", w.Code, w.Body.String())
	}
}

// TestAnUpstreamBadRequestIsNotARateLimit says a 400 from the upstream
// reaches the client as a 400.
//
// 400 是上游说"这个请求不对"。说成 429 是骗客户端：它会把这个请求
// 一直重试下去，而重试一个不对的请求没有用。
func TestAnUpstreamBadRequestIsNotARateLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"the request is not right"}}`))
	})
	h.gw.Cfg.Rotation.MaxAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

// TestAQuotaRefusalIsStillARateLimit says the one refusal that stays a
// 429 is the one about the credits.
//
// 额度用完了就是限流：客户端等一会儿再来是对的，所以这一条还是 429，
// 而且要带上限流的样子，让客户端知道该等而不是该换账号。
func TestAQuotaRefusalIsStillARateLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if body.Error.Type != "rate_limit_error" {
		t.Fatalf("the error's type = %q, want rate_limit_error", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "usage limit reached") {
		t.Fatalf("the error's message = %q", body.Error.Message)
	}
}

// TestATransportErrorIsABadGateway says a request that never got
// through is the gateway's own problem.
//
// 网络没通的时候上游没有给过任何状态。这时候没有上游的话可以转述，
// 那是网关这一边的事：502。把它说成 429，客户端会以为是自己被限流
// 了，而真正坏着的是网关到上游那一段路。
func TestATransportErrorIsABadGateway(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 上游指到一个已经关掉的地方：连都连不上。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	h.gw.UpstreamBase = deadURL
	h.gw.Cfg.Rotation.MaxAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502, body = %s", w.Code, w.Body.String())
	}
}

// TestABodyOverTheLimitIsRefused says a body that is too big is turned
// away instead of being read into memory.
//
// 没有上限的时候，一个请求就能把它这个网关的内存吃光：这里读的是整
// 个身子，读进来就一直拿着。
func TestABodyOverTheLimitIsRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	big := strings.Repeat("x", maxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(big))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413, body = %s", w.Code, w.Body.String())
	}
	if h.hits != 0 {
		t.Fatalf("the upstream was asked %d times, want 0", h.hits)
	}
}

// TestTheModelListIsReadOnceFromTheSite says the model list is the
// site's own, and that asking for it costs one request.
//
// 站点的模型接口给的是 provider config 的样子，不是模型列表的样子：
// 要读它得照着那个样子读（internal/account 的 ParseModels）。问的时候
// 不带令牌 —— 带上有可能在路上把令牌续了，一份模型名单不值得换一把
// 新令牌。
//
// 名单一年也换不了几回，所以读到的那份留着：客户端问十回，站点被问一
// 回。答得看不懂的时候（这里答的是模型列表的样子，不是 provider
// config 的样子）摆的还是内置的那一份。
func TestTheModelListIsReadOnceFromTheSite(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"whatever"}]}`))
	})
	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("code = %d", w.Code)
		}
		last = w
	}
	if h.hits != 1 {
		t.Fatalf("the upstream was asked %d times, want 1", h.hits)
	}
	w := last
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if body.Object != "list" {
		t.Fatalf("object = %q", body.Object)
	}
	if len(body.Data) == 0 {
		t.Fatalf("the list is empty: %s", w.Body.String())
	}
	for _, m := range body.Data {
		if m.ID == "whatever" {
			t.Fatalf("the upstream's answer was used: %s", w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), "MiniMax-M3") {
		t.Fatalf("the built-in list is not in the answer: %s", w.Body.String())
	}
}

// TestTheBackoffStopsWhenTheClientIsGone says the wait between two
// attempts ends when the client goes away.
//
// 客户端走了之后再睡上半分钟是白等：上游的次数是白花的，网关的连接
// 是白占的。退避要看着请求的命。
func TestTheBackoffStopsWhenTheClientIsGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 客户端走了：上游还在答，答了也没人听。
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	start := time.Now()
	h.gw.ServeHTTP(w, req)
	elapsed := time.Since(start)
	if h.hits != 1 {
		t.Fatalf("the upstream was asked %d times, want 1", h.hits)
	}
	// 第一次退避是半秒：没有停住的话，这里至少要等半秒。
	if elapsed > 400*time.Millisecond {
		t.Fatalf("the request took %s: the backoff did not stop", elapsed)
	}
}
```

创建 `internal/gateway/openai_test.go`：

```go
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mmxcode/internal/site"
)

// upstreamRecorder keeps what the upstream was asked.
//
// 记下发给上游的东西：上游是在自己的 goroutine 里 answering 的，所以
// 要一把锁。
type upstreamRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
}

func (u *upstreamRecorder) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
}

// seen gives the paths and the bodies the upstream was asked with.
func (u *upstreamRecorder) seen() ([]string, [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...), append([][]byte(nil), u.bodies...)
}

// TestTheOpenAIAnswerComesBackInTheOpenAIShape says an OpenAI request
// that the site cannot answer itself is answered in the OpenAI shape.
//
// 参考实现只走 Anthropic 的 /messages，站点多半没有 /chat/completions。
// 那条路是本地转的：请求转成 Anthropic 的，回答再转回 OpenAI 的样子。
// 客户端认的是 OpenAI 的形状，不能给它一段 Anthropic 的。
func TestTheOpenAIAnswerComesBackInTheOpenAIShape(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path == site.ChatCompletionsPath {
			// 站点没有这条路
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	})

	body := `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}

	// 上游收到的是 Anthropic 的请求
	paths, bodies := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("the upstream was asked %d times: %v", len(paths), paths)
	}
	if paths[0] != site.ChatCompletionsPath {
		t.Fatalf("the first path = %q, want the native one", paths[0])
	}
	if paths[1] != site.MessagesPath {
		t.Fatalf("the second path = %q, want the Anthropic one", paths[1])
	}
	var up map[string]any
	if err := json.Unmarshal(bodies[1], &up); err != nil {
		t.Fatalf("the upstream's request is not JSON: %v (%s)", err, bodies[1])
	}
	if up["max_tokens"] != float64(32) {
		t.Fatalf("the upstream's request = %s", bodies[1])
	}

	// 客户端收到的是 OpenAI 的回答
	var answer struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if answer.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", answer.Object)
	}
	if len(answer.Choices) != 1 {
		t.Fatalf("choices = %+v", answer.Choices)
	}
	if answer.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q", answer.Choices[0].Message.Role)
	}
	if answer.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q", answer.Choices[0].Message.Content)
	}
	if answer.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", answer.Choices[0].FinishReason)
	}
	// 第一次 404 把这条路关掉了，后面的请求不再去撞它。
	if h.gw.NativeChatCompletions.Load() {
		t.Fatal("the native path is still on")
	}
}

// TestTheOpenAIToolCallSurvivesTheFallback says a tool call the
// upstream made comes back as an OpenAI tool call.
//
// 模型要调工具，客户端得看得见它调的是哪个工具、拿什么参数去调。
func TestTheOpenAIToolCallSurvivesTheFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.ChatCompletionsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","stop_reason":"tool_use","content":[` +
			`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Beijing"}}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var answer struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if len(answer.Choices) != 1 {
		t.Fatalf("choices = %+v", answer.Choices)
	}
	calls := answer.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %+v", calls)
	}
	if calls[0].ID != "toolu_1" {
		t.Fatalf("id = %q, want the tool_use block's id", calls[0].ID)
	}
	if calls[0].Type != "function" {
		t.Fatalf("type = %q", calls[0].Type)
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"city":"Beijing"}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
	if answer.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", answer.Choices[0].FinishReason)
	}
}

// TestTheOpenAIRequestIsUntouchedWhenTheSiteHasThePath says the
// native path is the default: nothing is converted when the site
// serves /chat/completions.
//
// 站点也许真的有这条路。有的时候网关就该原样把请求发过去：转来转去
// 只会把东西弄丢。
func TestTheOpenAIRequestIsUntouchedWhenTheSiteHasThePath(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	})

	body := `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	paths, bodies := rec.seen()
	if len(paths) != 1 || paths[0] != site.ChatCompletionsPath {
		t.Fatalf("the upstream was asked %v", paths)
	}
	if string(bodies[0]) != body {
		t.Fatalf("the upstream's request = %s, want it unchanged", bodies[0])
	}
	// 客户端拿到的就是上游的那一段
	if !strings.Contains(w.Body.String(), "chatcmpl-1") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if !h.gw.NativeChatCompletions.Load() {
		t.Fatal("the native path was turned off without a 404")
	}
}

// TestTheOpenAIStreamFallbackEndsWithDone says the streaming fallback
// gives the client an OpenAI stream.
//
// OpenAI 的流是一段 data，每一段是一块，最后是 [DONE]。少了最后那一
// 段，客户端会一直等一个不会来的答案。
func TestTheOpenAIStreamFallbackEndsWithDone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.ChatCompletionsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n"))
		w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	stream := w.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
		t.Fatalf("the stream does not end with [DONE]: %s", stream)
	}

	var text strings.Builder
	var finish string
	chunks := 0
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("object = %q, want chat.completion.chunk", chunk.Object)
		}
		if chunk.Model != "m" {
			t.Fatalf("model = %q", chunk.Model)
		}
		chunks++
		if len(chunk.Choices) != 1 {
			t.Fatalf("choices = %+v", chunk.Choices)
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if chunks < 2 {
		t.Fatalf("the stream has %d chunks: %s", chunks, stream)
	}
	if text.String() != "hi" {
		t.Fatalf("the text = %q", text.String())
	}
	if finish != "stop" {
		t.Fatalf("finish_reason = %q", finish)
	}
}
```

创建 `internal/webui/login_test.go`：

```go
package webui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// fakeSite is the site itself, answered here.
//
// 一个测试不许碰网络：令牌的端点、账号的 API 都在这里答。没有哪一条
// 请求能出去。
type fakeSite struct {
	mu       sync.Mutex
	device   string
	token    string
	identity string
	seen     []string
	// tokens counts the token requests that came in.
	tokens int
	// tokenFor gives the answer to the n-th token request.
	//
	// 它是给「每一次登录都是一个不一样的站点账号」那种测试用的：同一
	// 个站点账号登三次，库里只有一行。
	tokenFor func(n int) string
	// identityFor gives the answer to the n-th identity request.
	//
	// 站点给的那个真的 user id 是账号最靠得住的钥匙：两个登录要是那
	// 一个一样，那是同一个站点账号，库里只有一行。
	identityFor func(n int) string
	// identities counts the identity requests that came in.
	identities int
}

// RoundTrip answers one request.
func (f *fakeSite) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	body, status := "", http.StatusOK
	switch r.URL.Path {
	case site.DeviceCodePath:
		body = f.device
	case site.TokenPath:
		if f.tokenFor != nil {
			body = f.tokenFor(f.tokens)
			f.tokens++
		} else {
			body = f.token
		}
	case site.IdentityPath:
		if f.identityFor != nil {
			body = f.identityFor(f.identities)
			f.identities++
		} else {
			body = f.identity
		}
	}
	if body == "" {
		status, body = http.StatusNotFound, `{"error":"no such endpoint"}`
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// creditAnswer is what the site answers to a request for the ledger.
//
// 一笔还没有花掉的额度：发了 600，花了 0，还剩 600。
const creditAnswer = `{"total_count":1,"details":[{"credit_type":1,` +
	`"granted_amount":"600","consumed_amount":"0","remaining_amount":"600",` +
	`"granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`

// serve answers the account API for the stand-in server.
//
// 账号的 API 走的是账号自己的出口（直连），所以要有一台真的服务器接着
// 它：直连出去就是问真的站点去了。答的是同一份 stand-in 的答案。
func (f *fakeSite) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	switch r.URL.Path {
	case site.CreditPath:
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(creditAnswer))
	default:
		http.NotFound(w, r)
	}
}

// paths gives the paths that were asked for.
func (f *fakeSite) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// newFakeSiteWebUI makes a WebUI whose site is a stand-in.
//
// 账号的 API 和令牌的端点都在 stand-in 后面，所以登录这条路整条都能走
// 一遍，而没有一条请求出得去。
func newFakeSiteWebUI(t *testing.T, f *fakeSite) (*WebUI, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second, Transport: f})
	acc := account.New(up)
	// 账号的 API 要有一台服务器接着：账号自己的出口是直连的，直连出去
	// 就是问真的站点去了。令牌的端点还在 stand-in 后面。
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(map[string]string{}, ""), oauth.NewRenewer())
	return New(mgr, config.Default(), s), s
}

// deviceAnswer is what the site answers to a sign-in code.
const deviceAnswer = `{"device_code":"dc","user_code":"ABCD-EFGH",` +
	`"verification_uri":"https://account.minimax.io/activate","expires_in":600,"interval":1}`

// tokenWithSubject makes a token that carries a subject claim.
//
// 令牌里的 sub 是猜的：这个令牌带一个，好让测试看得见"猜的那个"和
// "真的那个"不是一回事。
func tokenWithSubject(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"sub":%q}`, sub)))
	return "header." + payload + ".signature"
}

// waitForAccounts waits until the store holds n accounts.
func waitForAccounts(t *testing.T, s *store.Store, n int) []store.Account {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		accs, err := s.ListAccounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(accs) >= n {
			return accs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("there are not %d accounts yet", n)
	return nil
}

// TestTheWebUIAcquiresTheRealUserIDAndEmail says the account the WebUI
// makes is the account the site knows.
//
// 账号是谁要问站点本人：令牌里的那个 sub 是猜的，签到和积分明细要的
// 是真的那个 user id。猜出来的那个号，签到签不上，积分明细也是空的。
func TestTheWebUIAcquiresTheRealUserIDAndEmail(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token: `{"access_token":"` + tokenWithSubject("jwt-subject") +
			`","refresh_token":"rt","expires_in":3600}`,
		identity: `{"data":{"userInfo":{"realUserID":"1234567890",` +
			`"userEmail":"who@example.com"}}}`,
	}
	w, s := newFakeSiteWebUI(t, f)

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	accs := waitForAccounts(t, s, 1)
	acc := accs[0]
	if acc.UserID != "1234567890" {
		t.Errorf("user id = %q, want the one the site says", acc.UserID)
	}
	if acc.Email != "who@example.com" {
		t.Errorf("email = %q, want the one the site says", acc.Email)
	}
	if acc.Name != "who@example.com" {
		t.Errorf("name = %q, want the email the site says", acc.Name)
	}
	if !strings.HasPrefix(acc.ID, "acc_") {
		t.Errorf("id = %q, want the acc_ prefix", acc.ID)
	}
	// 站点本人被问过了：那个 user id 和邮箱是从它那里来的。
	asked := false
	for _, p := range f.paths() {
		if p == "GET "+site.IdentityPath {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the identity API was never asked: %v", f.paths())
	}
}

// TestTheWebLoginReadsTheCreditsOfTheNewAccount says an account signed
// in through the page has its credits read.
//
// 刚登进来的账号马上读一次账本：不等调度器那一趟（15 分钟），人才看
// 得到它有多少积分。
func TestTheWebLoginReadsTheCreditsOfTheNewAccount(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token: `{"access_token":"` + tokenWithSubject("jwt-subject") +
			`","refresh_token":"rt","expires_in":3600}`,
		identity: `{"data":{"userInfo":{"realUserID":"1234567890",` +
			`"userEmail":"who@example.com"}}}`,
	}
	w, s := newFakeSiteWebUI(t, f)

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	waitForAccounts(t, s, 1)
	waitForPath(t, f, site.CreditPath)
}

// waitForPath waits until the site was asked for a path.
//
// 登录是在后台做的：账号进了库，读账本是在它之后。测试不能靠运气。
func waitForPath(t *testing.T, f *fakeSite, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range f.paths() {
			if strings.HasSuffix(p, path) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the site was never asked for %s: %v", path, f.paths())
}

// TestTheWebUIKeepsTheLoginWhenTheIdentityIsUnreadable says a sign-in
// that worked is not thrown away because the identity could not be read.
//
// 登录成功了就是登录成功了：读不到账户信息只是少知道一点，账号照样建
// 起来，用令牌自己带着的东西填。
func TestTheWebUIKeepsTheLoginWhenTheIdentityIsUnreadable(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token: `{"access_token":"` + tokenWithSubject("jwt-subject") +
			`","refresh_token":"rt","expires_in":3600}`,
		// 身份读不到：没有回答就是没有回答。
		identity: "",
	}
	w, s := newFakeSiteWebUI(t, f)

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	accs := waitForAccounts(t, s, 1)
	acc := accs[0]
	if acc.UserID != "jwt-subject" {
		t.Errorf("user id = %q, want the one the token carries", acc.UserID)
	}
	if acc.Name != "jwt-subject" {
		t.Errorf("name = %q, want the one the token carries", acc.Name)
	}
	if acc.Email != "" {
		t.Errorf("email = %q, want none", acc.Email)
	}
}

// TestTwoSignInsDoNotShareAnAccountID says two sign-ins that end at the
// same moment do not fight over one id.
//
// 账号的 id 是主键：两个登录在同一毫秒里结束，第二个会被数据库挡下
// 来。挡下来的那一个要是没人说，这一次登录就悄悄没了。
//
// 这三次登录是三个不一样的站点账号（站点给的那个 user id 不一样）：同
// 一个站点账号登三次，库里只有一行，那是另一条规则。
func TestTwoSignInsDoNotShareAnAccountID(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		tokenFor: func(n int) string {
			return `{"access_token":"` + tokenWithSubject(fmt.Sprintf("jwt-subject-%d", n)) +
				`","refresh_token":"rt","expires_in":3600}`
		},
		identityFor: func(n int) string {
			return `{"data":{"userInfo":{"realUserID":"` + fmt.Sprintf("user-%d", n) + `"}}}`
		},
	}
	w, s := newFakeSiteWebUI(t, f)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
		if rec.Code != 200 {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	accs := waitForAccounts(t, s, 3)
	ids := map[string]bool{}
	for _, a := range accs {
		if ids[a.ID] {
			t.Fatalf("two accounts have the id %q", a.ID)
		}
		ids[a.ID] = true
		if !strings.HasPrefix(a.ID, "acc_") {
			t.Errorf("id = %q, want the acc_ prefix", a.ID)
		}
	}
}

// TestTheSameAccountSignedInTwiceIsOneRow says signing the same site
// account in twice does not make two live rows.
//
// 一个站点账号在库里只能有一行。这一行多半是批量导入进来的：登录又
// 登了一遍，是把那一行改好，不是再插一行——同一个站点账号两行都是活
// 的，轮转会挑到两次，页面把它列两遍，而且其中一个的令牌迟早被另一个
// 的重登作废。
func TestTheSameAccountSignedInTwiceIsOneRow(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token: `{"access_token":"` + tokenWithSubject("jwt-subject") +
			`","refresh_token":"rt","expires_in":3600}`,
		identity: `{"data":{"userInfo":{"realUserID":"1234567890"}}}`,
	}
	w, s := newFakeSiteWebUI(t, f)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
		if rec.Code != 200 {
			t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	accs := waitForAccounts(t, s, 1)
	// 两个登录都要走完：再等一会儿，看有没有多出第二行。
	time.Sleep(200 * time.Millisecond)
	accs, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 {
		t.Fatalf("accounts = %v, want one row for one site account", accs)
	}
}

// TestTheAccountIDIsNotOnlyTheTime says the id is not the time alone.
//
// 光是时间不够：同一毫秒里结束的两个登录会拿到同一个 id。
func TestTheAccountIDIsNotOnlyTheTime(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newAccountID()
		if !strings.HasPrefix(id, "acc_") {
			t.Fatalf("id = %q, want the acc_ prefix", id)
		}
		if seen[id] {
			t.Fatalf("the same id twice: %q", id)
		}
		seen[id] = true
	}
}

// TestARefusedSignInIsToldInTheBrowser says the page is told why the
// sign-in did not work.
//
// 失败要说出来：页面只看到"还在等"，人只当是网慢。
func TestARefusedSignInIsToldInTheBrowser(t *testing.T) {
	f := &fakeSite{
		device: deviceAnswer,
		token:  `{"error":"access_denied","error_description":"the user refused"}`,
	}
	w, s := newFakeSiteWebUI(t, f)

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		body := strings.NewReader(fmt.Sprintf(`{"id":%q}`, started.ID))
		w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/poll", body))
		var out struct {
			Error string `json:"error"`
			Done  bool   `json:"done"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Error != "" {
			if !strings.Contains(out.Error, "access_denied") {
				t.Fatalf("the error is %q, want the site's own words", out.Error)
			}
			accs, err := s.ListAccounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(accs) != 0 {
				t.Fatalf("the refused sign-in made an account: %+v", accs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the refusal never came")
}

// TestAFailedSignInIsForgotten says a sign-in that failed does not stay
// forever.
//
// 说完失败，那次登录就该忘掉：服务器开得久，不能把每一次登录都留着。
func TestAFailedSignInIsForgotten(t *testing.T) {
	w, _ := newTestWebUI(t)
	w.mu.Lock()
	w.pendingTTL = 50 * time.Millisecond
	w.pending["pending_1"] = &pendingLogin{ID: "pending_1"}
	w.mu.Unlock()
	w.failLogin("pending_1", "the site refused the sign-in")

	// 页面先问到那句话。
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"id":"pending_1"}`)
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/poll", body))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != "the site refused the sign-in" {
		t.Fatalf("body = %s, want the reason", rec.Body.String())
	}

	// 过了一会儿，那次登录不在了。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.pending)
		w.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the failed sign-in is still there")
}

// TestThePollDoesNotRaceWithAFailedSignIn says the page can ask about a
// sign-in while it fails.
//
// 页面每隔几秒问一次登录怎么了，登录失败的时候那个失败是写进去的。
// 读它的时候得拿着锁：字符串是两个词，读到一半就是半个指针加一个
// 不在那里的长度。
func TestThePollDoesNotRaceWithAFailedSignIn(t *testing.T) {
	w, _ := newTestWebUI(t)
	w.mu.Lock()
	w.pending["pending_1"] = &pendingLogin{ID: "pending_1"}
	w.mu.Unlock()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	// 登录失败：那个失败是写在锁里的。
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			w.mu.Lock()
			p := w.pending["pending_1"]
			if p != nil {
				p.Err = "the site refused the sign-in"
			}
			w.mu.Unlock()
		}
	}()
	// 页面在问。
	for i := 0; i < 500; i++ {
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"id":"pending_1"}`)
		w.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/poll", body))
	}
	close(done)
	wg.Wait()
}

// TestTheDeviceCodeIsNeverMarkup says the code the site gives is never
// written as markup.
//
// 上游答上来的验证码和地址是给人看的，不是给浏览器执行的。Go 这边拼
// 出来的每一段页面都是模板里的，脚本那边也不许有 innerHTML。
func TestTheDeviceCodeIsNeverMarkup(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"innerHTML", "template.HTML"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s has %s in it", name, bad)
			}
		}
	}
	js, err := os.ReadFile(filepath.Join("static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", `href="${`, "href='${"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("static/app.js has %s in it", bad)
		}
	}
}

// TestTheLoginPageEscapesTheDeviceCode says the page shows the code as
// text, and the address as an address.
//
// 验证码是上游给的，地址也是。一个 <script> 进去就是脚本，一个
// javascript: 的地址进去就是点一下就跑。
func TestTheLoginPageEscapesTheDeviceCode(t *testing.T) {
	w, _ := newTestWebUI(t)
	w.mu.Lock()
	w.pending["pending_1"] = &pendingLogin{
		ID:       "pending_1",
		UserCode: `<script>alert("x")</script>`,
		URL:      "javascript:alert(1)",
	}
	w.mu.Unlock()

	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert") {
		t.Fatalf("the code was written as markup: %s", body)
	}
	if strings.Contains(body, "javascript:alert") {
		t.Fatalf("the address was written as a link: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("the code is not escaped: %s", body)
	}
}
```




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

1. **上游是否原生支持 `/responses`**：实现里默认先试原生路径（`NativeResponses.Store(true)`）。要确认的是国际站 `/mavis/api/v1/llm/v1/responses` 到底在不在。在不在，两条路径的输出行为要一致。
2. **国际站的 `/credit/details` 能不能用**：不能用的话 `/api/usage` 的 `total_*` 只算有数据的账号，并带 `accounts_unavailable` 计数。
3. **`token_plan/remains_percent` 在国际站返不返数据**：不返的话耗尽标记退化成账号级 + 冷却。
4. **WARP 在当前网络能不能连通 `agent.minimax.io`**：不能的话 `warp` 组在出口页上就是红的，不用选它。
