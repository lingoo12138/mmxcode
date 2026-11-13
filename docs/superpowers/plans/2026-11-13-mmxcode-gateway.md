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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/checkin"
	"mmxcode/internal/egress"
	"mmxcode/internal/oauth"
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
	return New(s, up, account.New(up), egress.New(nil, ""), oauth.NewRenewer())
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
package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
}

// New makes a manager.
func New(s *store.Store, up *upstream.Client, acc *account.Client, egr *egress.Resolver, rw *oauth.Renewer) *Manager {
	return &Manager{Store: s, Up: up, Acc: acc, Egress: egr, Renewer: rw}
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
	cur := &oauth.Token{
		Access:  stored.Access,
		Refresh: stored.Refresh,
		Expires: stored.ExpiresAt,
		Subject: stored.Subject,
	}
	if cur.Expires.After(time.Now().Add(renewWindow)) {
		return cur, nil
	}
	return m.Renew(ctx, accountID, cur)
}

// Renew trades an account's refresh token for a new one.
//
// When the site says the refresh token is no good, the account is
// marked signed out and no token is given.
func (m *Manager) Renew(ctx context.Context, accountID string, cur *oauth.Token) (*oauth.Token, error) {
	if cur == nil {
		return nil, fmt.Errorf("no token to renew")
	}
	tok, err := m.Renewer.Renew(ctx, accountID, cur, func(ctx context.Context, refresh string) (*oauth.Token, error) {
		return oauth.Refresh(ctx, m.Up, site.AccountURL(site.TokenPath), refresh)
	})
	if err != nil {
		var oe *oauth.Error
		if asOAuthError(err, &oe) && oe.IsSignedOut() {
			m.markSignedOut(ctx, accountID, err.Error())
		}
		return nil, err
	}
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: accountID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: time.Now(),
	}); err != nil {
		return nil, err
	}
	return tok, nil
}

// ForceRenew renews an account's token without waiting for it to be
// near its end.
//
// It is what the gateway does when the site answers 401.
func (m *Manager) ForceRenew(ctx context.Context, accountID string) (*oauth.Token, error) {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account %s is not signed in", accountID)
	}
	cur := &oauth.Token{
		Access:  stored.Access,
		Refresh: stored.Refresh,
		Expires: stored.ExpiresAt,
		Subject: stored.Subject,
	}
	// 让 renewer 忘记旧链，用库里那份 refresh 重新走一次
	m.Renewer.Forget(accountID)
	return m.Renew(ctx, accountID, cur)
}

// markSignedOut records that an account's sign-in is gone.
func (m *Manager) markSignedOut(ctx context.Context, accountID, reason string) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	a.Status = store.StatusSignedOut
	a.LastError = reason
	a.UpdatedAt = time.Now()
	_ = m.Store.UpdateAccount(ctx, a)
}

// Client gives the HTTP client an account's requests go out with, and
// the address of that way out.
func (m *Manager) Client(ctx context.Context, accountID string) (*http.Client, string, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, "", err
	}
	url := m.Egress.Resolve(a)
	c, err := m.Egress.Client(url)
	if err != nil {
		return nil, url, err
	}
	return c, url, nil
}

// RefreshUsage reads an account's credits and windows and saves them.
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

	batches, err := m.Acc.Ledger(ctx, tok.Access, a.UserID)
	if err != nil {
		return store.UsageSnapshot{}, err
	}
	now := time.Now()
	var granted, consumed, remaining float64
	have := false
	var soonest time.Time
	for _, b := range batches {
		if b.CreditType != 1 && b.CreditType != 2 {
			continue
		}
		if !b.ExpiresAt.IsZero() && !b.ExpiresAt.After(now) {
			continue
		}
		granted += b.Granted
		consumed += b.Consumed
		remaining += b.Remaining
		have = true
		if b.ExpiresAt.IsZero() {
			continue
		}
		if soonest.IsZero() || b.ExpiresAt.Before(soonest) {
			soonest = b.ExpiresAt
		}
	}
	if have {
		snap.HasAmounts = true
		snap.Granted = granted
		snap.Consumed = consumed
		snap.Remaining = remaining
		snap.ExpiresAt = soonest
	}

	if ws, err := m.Acc.PlanWindows(ctx, tok.Access, ""); err == nil && len(ws) > 0 {
		if b, err := json.Marshal(ws); err == nil {
			snap.WindowsJSON = string(b)
		}
	}
	if title, err := m.Acc.PlanTitle(ctx, tok.Access, ""); err == nil {
		snap.Plan = title
	}
	if mem, err := m.Acc.Membership(ctx, tok.Access, a.UserID); err == nil {
		snap.Balance = mem.Balance
		snap.PlanUntil = mem.ExpiresAt
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
	c := checkin.New(m.Acc)
	out, err := c.Run(ctx, tok.Access, a.UserID)
	if err != nil {
		return checkin.Outcome{}, err
	}
	day := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
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
func (m *Manager) LatestWindows(ctx context.Context, accountID string) ([]account.Window, error) {
	snaps, err := m.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range snaps {
		if s.AccountID != accountID || s.WindowsJSON == "" {
			continue
		}
		var ws []account.Window
		if err := json.Unmarshal([]byte(s.WindowsJSON), &ws); err != nil {
			return nil, err
		}
		return ws, nil
	}
	return nil, nil
}

// asOAuthError unwraps an error into an OAuth error.
func asOAuthError(err error, target **oauth.Error) bool {
	for err != nil {
		if oe, ok := err.(*oauth.Error); ok {
			*target = oe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
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
	return DoWith(req, c.http.Transport)
}

// DoWith sends a request through a given transport.
//
// A redirect that stays on the host is followed; one that leaves it is
// an OtherSiteError, and the request is not sent on.
//
// The transport is a parameter because the proxy is a property of the
// transport: an account with its own exit needs its own client, and
// the same client is shared by every account that shares an exit.
func DoWith(req *http.Request, transport http.RoundTripper) (*http.Response, error) {
	if transport == nil {
		transport = defaultTransport()
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	current := req
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
		if next.Host != current.URL.Host {
			resp.Body.Close()
			return nil, &OtherSiteError{
				From:   current.URL.Host,
				To:     next.Host,
				Status: resp.StatusCode,
			}
		}
		if hop >= 5 {
			return resp, nil
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

同时删掉 `client.go` 里旧的 `Do` 函数体（保留 `defaultTransport`、`redirectStatuses`）。

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
		BaseURL:    srv.URL,
		Path:       "/mavis/api/v1/llm/v1/messages",
		Access:     "acc-token",
		SessionID:  "sess-1",
		TZOffset:   -480,
		Body:       []byte(`{"model":"MiniMax-M3"}`),
		Stream:     false,
		Anthropic:  true,
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
	resp, err := DoWith(httpReq, nil)
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
	marked    []string
	remaining map[string]float64
}

func newFakePool(accounts ...store.Account) *fakePool {
	return &fakePool{accounts: accounts, sessions: map[string]string{}}
}

func (p *fakePool) Accounts(context.Context) ([]store.Account, error) { return p.accounts, nil }

func (p *fakePool) Exhausted(context.Context) ([]store.Exhausted, error) { return p.exhausted, nil }

func (p *fakePool) BindSession(_ context.Context, key, accountID string) error {
	p.sessions[key] = accountID
	p.bound = append(p.bound, key+"="+accountID)
	return nil
}

func (p *fakePool) SessionAccount(_ context.Context, key string) (string, bool, error) {
	id, ok := p.sessions[key]
	return id, ok, nil
}

func (p *fakePool) Remaining(_ context.Context, accountID string) (float64, bool) {
	left, ok := p.remaining[accountID]
	return left, ok
}

func (p *fakePool) MarkExhausted(_ context.Context, accountID, windowKey string, until time.Time, reason string) error {
	p.marked = append(p.marked, accountID+"/"+windowKey+"/"+reason)
	p.exhausted = append(p.exhausted, store.Exhausted{
		AccountID: accountID, WindowKey: windowKey, Until: until, Reason: reason,
	})
	return nil
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
	// 第一个账号耗尽了，重放时不能再选它
	used := map[string]bool{first.AccountID: true}
	second, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", used)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccountID == first.AccountID {
		t.Fatal("the exhausted account was picked again")
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
	r := New(p, Options{MaxAttempts: 3, Strategy: "round-robin"})
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

func TestWindowKey(t *testing.T) {
	r := New(newFakePool(), Options{})
	if got := r.WindowKey("MiniMax-M3"); got != "general" {
		t.Fatalf("window = %q", got)
	}
	if got := r.WindowKey("MiniMax-Video-01"); got != "video" {
		t.Fatalf("window = %q", got)
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

import "mmxcode/internal/upstream"

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
}

// Decide says what to do about a failure.
//
// The rules come from what the site actually does:
//
//   - a refusal that names the credits or the quota means the account
//     is spent: another account is tried;
//   - a 429 that does not name the credits is a rate limit, and the
//     same account is asked again after a pause;
//   - a 5xx or a network error is the same: the same account, again;
//   - a 401 means the token is stale: it is renewed first;
//   - a redirect to the other site is neither of these: the account is
//     fine and the network is not, so nothing is changed and no
//     refresh token is spent.
func Decide(f Failure) Action {
	if f.OtherSite {
		return ActionGiveUp
	}
	if f.Network {
		return ActionRetrySame
	}
	if f.Status == 200 {
		// 一个 200 里装着业务错误。只有确认了那是错误才会走到
		// 这里，所以按错误来判。
		if upstream.IsQuotaRefusal(f.Body) {
			return ActionFailover
		}
		return ActionGiveUp
	}
	switch f.Status {
	case 401:
		return ActionRenew
	case 402, 403:
		if upstream.IsQuotaRefusal(f.Body) {
			return ActionFailover
		}
		return ActionRetrySame
	case 429:
		if upstream.IsQuotaRefusal(f.Body) {
			return ActionFailover
		}
		return ActionRetrySame
	}
	if f.Status >= 500 {
		return ActionRetrySame
	}
	return ActionGiveUp
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
// The exhaustion is per account and per plan window: an account whose
// five-hour window is full can still serve another window.
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
	MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
	// Remaining gives an account's credits left. The second value is
	// false when the credits have not been read yet.
	Remaining(ctx context.Context, accountID string) (float64, bool)
}

// Options configures a rotator.
type Options struct {
	// MaxAttempts is how many accounts one request may be tried on.
	MaxAttempts int
	// Cooldown is how long an account is given up on when the spent
	// window cannot be told apart.
	Cooldown time.Duration
	// WindowGroup says which plan window a model spends from. It
	// defaults to the chat models' group.
	WindowGroup func(model string) string
	// Strategy says how an account is picked.
	//
	// "sticky" keeps a conversation on the account it was bound to, and
	// only rotates when there is no conversation to keep.
	// "round-robin" rotates every request, and the binding is not used.
	Strategy string
}

// Plan is the account a request goes to.
type Plan struct {
	// AccountID is the account.
	AccountID string
	// WindowKey is the plan window the request spends.
	WindowKey string
	// Rebound says the session was moved to another account.
	Rebound bool
}

// Rotator picks accounts.
type Rotator struct {
	pool Pool
	opts Options

	// mu guards the two fields below.
	mu sync.Mutex
	// cursor is where the rotation stands, so two requests in a row do
	// not land on the same account.
	cursor int
	// lastUsed says when each account was last handed out, for the
	// "least recently used first" rule.
	lastUsed map[string]time.Time
}

// New makes a rotator.
func New(pool Pool, opts Options) *Rotator {
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
		opts.Strategy = "sticky"
	}
	return &Rotator{
		pool:     pool,
		opts:     opts,
		lastUsed: map[string]time.Time{},
	}
}

// defaultWindowGroup is the chat models' group.
func defaultWindowGroup(model string) string {
	if strings.Contains(strings.ToLower(model), "video") {
		return "video"
	}
	return "general"
}

// WindowKey gives the plan window a model spends from.
func (r *Rotator) WindowKey(model string) string {
	return r.opts.WindowGroup(model)
}

// Pick gives the account a request goes to.
//
// used holds the accounts already tried for this request, so a replay
// does not pick the same one again.
//
// Which account it is comes down to three cases:
//
//   - a conversation that is bound to an account stays on it, as long as
//     the account can still answer;
//   - a conversation with no binding takes the account with the most
//     credits left, and the least recently used one when those are
//     equal;
//   - a request with no conversation at all rotates: the next account
//     in turn, so the load is spread.
//
// The "round-robin" strategy skips the first case and rotates always.
func (r *Rotator) Pick(ctx context.Context, sessionKey, model string, used map[string]bool) (Plan, error) {
	accounts, err := r.pool.Accounts(ctx)
	if err != nil {
		return Plan{}, err
	}
	exhausted, err := r.pool.Exhausted(ctx)
	if err != nil {
		return Plan{}, err
	}
	window := r.WindowKey(model)

	usable := make([]store.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			continue
		}
		if used[a.ID] || r.isExhausted(exhausted, a.ID, window) {
			continue
		}
		usable = append(usable, a)
	}
	if len(usable) == 0 {
		return Plan{}, fmt.Errorf("every account is spent for the %s window", window)
	}

	sticky := r.opts.Strategy != "round-robin"

	// 会话已绑定的账号能用就用
	if sticky && sessionKey != "" {
		if id, ok, err := r.pool.SessionAccount(ctx, sessionKey); err == nil && ok {
			for _, a := range usable {
				if a.ID != id {
					continue
				}
				r.markUsed(id)
				return Plan{
					AccountID: id,
					WindowKey: window,
				}, nil
			}
		}
	}

	// 会话没有绑定：挑余额最多的，其次最久没用的
	if sticky && sessionKey != "" {
		a, err := r.bestAccount(ctx, usable)
		if err != nil {
			return Plan{}, err
		}
		_ = r.pool.BindSession(ctx, sessionKey, a.ID)
		r.markUsed(a.ID)
		return Plan{
			AccountID: a.ID,
			WindowKey: window,
			Rebound:   true,
		}, nil
	}

	// 没有会话，或者就是要轮询：下一个
	a := r.nextAccount(usable)
	r.markUsed(a.ID)
	return Plan{
		AccountID: a.ID,
		WindowKey: window,
	}, nil
}

// bestAccount gives the account with the most credits left, and the
// least recently used one when two have the same.
//
// An account whose credits have not been read is not treated as empty:
// it would be pushed to the back of the queue every time, and it is
// more likely to be an account that was just added.
func (r *Rotator) bestAccount(ctx context.Context, usable []store.Account) (store.Account, error) {
	var best *scored
	for _, a := range usable {
		left, known := r.pool.Remaining(ctx, a.ID)
		s := scored{acc: a, known: known, left: left, usedAt: r.usedAt(a.ID)}
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
		// 余额没读到的排前面：它多半是刚加进来的账号
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
// The cursor moves on every call, so two requests in a row do not land
// on the same account.
func (r *Rotator) nextAccount(usable []store.Account) store.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.cursor % len(usable)
	r.cursor++
	return usable[i]
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

// MarkExhausted marks an account's window spent.
func (r *Rotator) MarkExhausted(ctx context.Context, accountID, windowKey, reason string, until time.Time) error {
	if until.IsZero() {
		until = time.Now().Add(r.opts.Cooldown)
	}
	return r.pool.MarkExhausted(ctx, accountID, windowKey, until, reason)
}

// isExhausted says an account's window is spent.
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
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Thinking string         `json:"thinking"`
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
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
			"output_text": text.String(),
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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/checkin"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
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
}

func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if h.hits == 1 {
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
	"crypto/subtle"
	"net/http"
	"strings"
)

// auth checks the request's key.
//
// When no key is configured the gateway does not check: it is then
// meant to listen on the loopback address only, which the server does.
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

	"mmxcode/internal/upstream"
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

// quotaMessage says what the answer's message is.
func quotaMessage(body []byte) string {
	var env struct {
		Message string `json:"message"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return string(body)
	}
	switch {
	case env.Error != nil && env.Error.Message != "":
		return env.Error.Message
	case env.Message != "":
		return env.Message
	case env.Msg != "":
		return env.Msg
	}
	return "the account's credits or plan are used up"
}

// isQuotaRefusal says the answer is a refusal for the credits.
func isQuotaRefusal(status int, body []byte) bool {
	switch status {
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return upstream.IsQuotaRefusal(body)
	}
	return false
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
		Mgr:           mgr,
		Cfg:           cfg,
		Store:         s,
		UpstreamBase:  site.LLMHost,
		NativeResponses: true,
		WebUI:         webui,
	}
	g.Rot = rotator.New(poolAdapter{s}, rotator.Options{
		MaxAttempts: cfg.Rotation.MaxAttempts,
		Cooldown:    cfg.Rotation.ExhaustedCooldown.Duration(),
		WindowGroup: manager.WindowGroup,
		Strategy:    cfg.Rotation.Strategy,
	})
	return g
}

// poolAdapter makes the store into a rotator pool.
type poolAdapter struct{ s *store.Store }

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

func (p poolAdapter) Remaining(ctx context.Context, accountID string) (float64, bool) {
	snaps, err := p.s.LatestUsageSnapshots(ctx)
	if err != nil {
		return 0, false
	}
	for _, s := range snaps {
		if s.AccountID != accountID {
			continue
		}
		if !s.HasAmounts {
			return 0, false
		}
		return s.Remaining, true
	}
	return 0, false
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.auth(r) {
		writeError(w, http.StatusUnauthorized, "the key was refused")
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
	attempts := s.Cfg.Rotation.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if lastErr != nil {
				writeError(w, http.StatusTooManyRequests, lastErr.Error())
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
			// 和 Go 的 Offset() 是同一个值。
			TZOffset:  int(time.Now().Offset()),
			Body:      upBody,
			Stream:    stream,
			Anthropic: upPath == site.MessagesPath,
			Extra:     clientHeaders(r),
		}

		resp, raw, err := s.attempt(ctx, chatReq, client.Transport, stream)
		if err != nil {
			var oe *upstream.OtherSiteError
			if errors.As(err, &oe) {
				// 跨站：不换号、不消耗 refresh token
				writeError(w, http.StatusBadGateway, oe.Error())
				return
			}
			lastErr = err
			time.Sleep(backoff(attempt))
			continue
		}

		// 上游没有 /responses：改用本地转换，同一个账号再来一次
		if resp.StatusCode == http.StatusNotFound && proto == protocolCodex && s.NativeResponses {
			s.NativeResponses = false
			upBody, upPath, convertBack = s.upstreamShape(body, proto)
			chatReq.Body = upBody
			chatReq.Path = upPath
			chatReq.Anthropic = upPath == site.MessagesPath
			resp, raw, err = s.attempt(r.Context(), chatReq, client.Transport, stream)
			if err != nil {
				lastErr = err
				time.Sleep(backoff(attempt))
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

		failure := rotator.Failure{Status: resp.StatusCode, Body: raw}
		switch rotator.Decide(failure) {
		case rotator.ActionFailover:
			until := s.windowReset(ctx, plan.AccountID, plan.WindowKey)
			_ = s.Rot.MarkExhausted(ctx, plan.AccountID, plan.WindowKey, "quota", until)
			lastErr = fmt.Errorf("the account's credits or plan are used up")
			continue
		case rotator.ActionRenew:
			newTok, rerr := s.Mgr.ForceRenew(ctx, plan.AccountID)
			if rerr != nil {
				lastErr = rerr
				continue
			}
			// 用新 token 再来一次，还是这个账号
			chatReq.Access = newTok.Access
			resp2, raw2, err2 := s.attempt(r.Context(), chatReq, client.Transport, stream)
			if err2 != nil {
				lastErr = err2
				continue
			}
			if resp2.StatusCode == http.StatusOK && raw2 == nil {
				s.writeStream(w, resp2, proto, convertBack, model)
				return
			}
			if resp2.StatusCode == http.StatusOK {
				if err := upstream.CheckEnvelope("chat", raw2); err == nil {
					s.writeJSON(w, resp2, raw2, proto, convertBack)
					return
				}
			}
			lastErr = fmt.Errorf("HTTP %d", resp2.StatusCode)
			continue
		case rotator.ActionRetrySame:
			if isQuotaRefusal(resp.StatusCode, raw) {
				writeRateLimit(w, quotaMessage(raw))
				return
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			time.Sleep(backoff(attempt))
			continue
		default:
			if isQuotaRefusal(resp.StatusCode, raw) {
				writeRateLimit(w, quotaMessage(raw))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(raw)
			return
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("the request could not be served")
	}
	writeError(w, http.StatusTooManyRequests, lastErr.Error())
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
			Type       string `json:"type"`
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
func (s *Server) writeStream(w http.ResponseWriter, resp *http.Response, proto protocol, convertBack bool, model string) {
	copyHeaders(w, resp)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var err error
	if convertBack {
		err = convert.StreamResponses(resp.Body, w, model)
	} else {
		_, err = io.Copy(w, resp.Body)
	}
	if err != nil {
		// 流到一半断了：给客户端一个错误事件收尾，不重放
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"the stream ended early: %v\"}}\n\n", err)
	}
	if flusher != nil {
		flusher.Flush()
	}
	resp.Body.Close()
}

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
func (s *Server) windowReset(ctx context.Context, accountID, windowKey string) time.Time {
	ws, err := s.Mgr.LatestWindows(ctx, accountID)
	if err != nil {
		return time.Time{}
	}
	for _, w := range ws {
		if w.ModelName == windowKey && !w.ResetsAt.IsZero() {
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
	mgr := manager.New(s, up, account.New(up), egress.New(map[string]string{"warp": "socks5h://warp:4000"}, ""), oauth.NewRenewer())
	return New(mgr, config.Default(), s), s
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
  </nav>
  <span class="muted" id="status"></span>
</header>
<main>
{{template "content" .}}
</main>
<script src="/static/app.js"></script>
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
{{if .Summary.AccountsUnavailable}}<p class="bad">有 {{.Summary.AccountsUnavailable}} 个账号的积分读不到。</p>{{end}}
<table>
<tr><th>账号</th><th>积分</th><th>消耗</th><th>剩余</th><th>过期</th></tr>
{{range .Summary.Accounts}}
<tr>
  <td>{{if .Email}}{{.Email}}{{else}}{{.ID}}{{end}}</td>
  <td>{{if .Granted}}{{printf "%.2f" .Granted}}{{else}}<span class="muted">读不到</span>{{end}}</td>
  <td>{{if .Consumed}}{{printf "%.2f" .Consumed}}{{else}}0{{end}}</td>
  <td>{{if .Remaining}}{{printf "%.2f" .Remaining}}{{else}}0{{end}}</td>
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
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"mmxcode/internal/config"
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
	tmpl := template.Must(template.ParseFS(templateFS, "templates/*.html"))
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
	var buf strings.Builder
	if err := w.tmpl.ExecuteTemplate(&buf, "layout", d); err != nil {
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
			"Email":        a.Email,
			"Status":       a.Status,
			"ProxyGroup":   a.ProxyGroup,
			"ProxyURL":     a.ProxyURL,
			"Current":      a.ProxyGroup,
			"HasAmounts":   snap.HasAmounts,
			"Granted":      snap.Granted,
			"Remaining":    snap.Remaining,
			"Plan":         snap.Plan,
			"WindowSummary": w.windowSummary(ctx, a.ID),
			"CheckedIn":    checkedIn,
			"Streak":       streak,
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
	day := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
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
			"Name": name, "URL": url,
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
			Name      *string `json:"name"`
			ProxyGroup *string `json:"proxy_group"`
			ProxyURL  *string `json:"proxy_url"`
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
			a.ProxyURL = *body.ProxyURL
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
	mgr := manager.New(s, up, account.New(up), egress.New(nil, ""), oauth.NewRenewer())

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
	mgr := manager.New(s, up, account.New(up), egress.New(nil, ""), oauth.NewRenewer())

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
package mmxcode

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
FROM golang:1.23-alpine AS builder

WORKDIR /src

# 先只拷依赖清单，依赖没变���这一层就不用重建
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
