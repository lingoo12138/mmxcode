# 每个请求少打几次库 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一个聊天请求打在库上的次数从 5 条（热）/ 9 条（冷）降到 3 条 / 5 条。

**Architecture:** 四个改动 —— 量具（数一个请求打了几条语句）、后台那一趟的两条读合成一条、`HasAPIKeys` 的答案缓存在内存里（谁改了谁通知）、会话那一行读一次就够（顺带把冷路径的两次写合成一条）。缓存会话行、token、账号行**不做** —— 调查里实测它们会错。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`（纯 Go）、`database/sql`。

**规格：** `docs/superpowers/specs/2026-11-19-mmxcode-request-db-round-trips-design.md`

---

## 现在的账（规格 §2）

热路径（池里有号、会话已绑定、令牌没过期）：**5 条语句，254 µs**。

| 顺序 | 方法 | 目的 | 一次 |
|---|---|---|---|
| 1 | `Store.HasAPIKeys` | 「后台有没有申请过 key」 | 63 µs |
| 2 | `Store.UpstreamID` → `upstreamIDOf` | 取这轮对话发给上游的固定 id | 56 µs |
| 3 | `Store.SessionAccount` | 轮转要知道这轮对话钉在哪个号上 | 30 µs |
| 4 | `Store.GetToken` | 取 access token 去问上游 | 51 µs |
| 5 | `Store.GetAccount` | 只为了解析代理地址 | 54 µs |

冷路径（新会话第一趟）：**9 条语句，20.3 ms**（`synchronous=full` 下量的；上一轮把
`normal` 做成了默认，所以现在是 1.28 ms —— 不过那两次写还是两次）。

100 个并发冷请求：**874 条语句（8.74 条/请求），库时间 9.37 s**，其中写占 9.30 s。
**最大的那笔钱不在往返上，在写锁排队上。**

机器：Windows 11 / i7-7700 3.6GHz。**时钟粒度约 500 µs** —— 「每次调用多少微秒」
都是 200~500 次的平均。

## 这一版不做的

| 不做 | 为什么 |
|---|---|
| 缓存会话行（`sessionKey → (account_id, upstream_id)`） | **实测会错**：库里改绑了，缓存还拿着旧的号，请求被送到旧账号上。改绑有三条路，其中一条是别的进程改的库 —— TTL 也兜不住 |
| 缓存 token | **实测会错**：读额度那一趟会续签并把新 token 写进库，聊天那条路同时在读同一个号 —— 缓存拿着旧 token 去问上游，上游 401，**而 MiniMax 的 refresh token 只能用一次** |
| 缓存账号行（只为代理地址） | 改代理以后旧代理还在用 |
| `CountUsable` 换成数池里的号 | 池空的时候它返回 0 → 总并发算成 0 → **所有请求 429** |
| 把新会话的写合进 `TouchSessions` 那一批 | 省 15.3 s / 100 并发冷请求，可是绑定最多晚 20 秒 —— 同一会话的两个并发请求可能落到不同的号上。只在 round_robin 策略下安全 |

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/store/counting.go`（新） | 量具：给 SQLite 套一层计数的连接，数一个请求打了几条语句 |
| `internal/store/store.go` | `options` 多一个 `connector`（量具从这儿进来）、`HasAPIKeys` 的缓存、`ListAccounts` 带上耗尽标记 |
| `internal/store/sessions.go`（新） | 会话那几个方法（`store.go` 已经 2500 多行了）：`SessionState`、合成一条的 INSERT |
| `internal/rotator/rotator.go` | `Pool` 接口的 `SessionAccount` 换成 `SessionState`；`Pick` 用带着的那一份，不再自己读 |
| `internal/gateway/gateway.go` | `poolAdapter` 跟着换；请求那一路把带着的状态递下去 |
| `internal/webui/keys.go` | 不用改 —— 失效在 store 自己的写方法里（见 Task 3） |
| `internal/usage/round.go` | `Targets` 里那两条读合成一条 |

---

### Task 1: 量具 —— 数一个请求打了几条语句

**Files:**
- Create: `internal/store/counting.go`
- Modify: `internal/store/store.go`（`options` 多一个 `connector`；`Open` 和 `openDurable` 用它）
- Test: `internal/store/counting_test.go`

**为什么先做它：** 这一版要量的是「一个请求打在库上的次数」。数库的**方法**说不了
往返了几次 —— 一个方法里可能有三条语句。要数的是语句，那就在连接上数。

量具要能装在**生产的连接**上：`store.Open` 是用 `sql.Open("sqlite", dsn)` 开的，
所以得给它一条缝 —— 一个能换掉连接的钩子。生产的那个钩子是空（还是 SQLite 自己
的连接），量的时候才塞一个计数的进去。

- [ ] **Step 1: 写失败的测试**

`internal/store/counting_test.go`：

```go
// TestTheCounterCountsTheStatements says the counter counts the
// statements that went to the database.
//
// 量具本身要有牙：它数出来的得是真的语句，而且一条不多一条不少。
func TestTheCounterCountsTheStatements(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	s := openTestOpts(t, WithConnector(counter.Connector()))
	makeAccount(t, s, "one")

	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if got := counter.CountOf("INSERT INTO checkins"); got != 1 {
		t.Errorf("the statements that inserted a check-in = %d, want 1", got)
	}
	if got := counter.Count(); got == 0 {
		t.Error("nothing was counted at all")
	}
}

// TestTheCounterCountsEveryConnection says the statements of every
// connection are counted.
//
// 丢不得的那两笔走的是另一条连接：那条上的语句也得算进去，否则量出来的
// 条数是少的。
func TestTheCounterCountsEveryConnection(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	s := openTestOpts(t, WithConnector(counter.Connector()))
	makeAccount(t, s, "one")

	// 这一笔走的是那条 fsync 的连接。
	if err := s.SaveToken(ctx, Token{
		AccountID: "one", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := counter.CountOf("INSERT INTO tokens"); got != 1 {
		t.Errorf("the statements that wrote a token = %d, want 1", got)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheCounter -v
```

失败：编译不过（`NewCounter`、`WithConnector` 还没有）。

- [ ] **Step 3: 实现**

`internal/store/counting.go`（新）：

```go
package store

import (
	"context"
	"database/sql/driver"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// Counter counts the statements the store sends to SQLite.
//
// 它是量具：这个版本要量的是「一个请求打在库上的次数」。数的是语句，不是库的
// 方法 —— 方法的个数说不了往返了几次。
//
// 它是安全的：好几条连接同时数也不会乱。
type Counter struct {
	mu    sync.Mutex
	stmts []string
}

// NewCounter makes a counter that has counted nothing yet.
func NewCounter() *Counter {
	return &Counter{}
}

// Statements gives the statements that were sent, in the order they were
// sent.
func (c *Counter) Statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.stmts...)
}

// Count gives how many statements were sent.
func (c *Counter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stmts)
}

// CountOf gives how many of the statements mention the given text.
//
// 说的是「提到」不是「就是」：一条语句往往一行一行地写，问「哪几条提到了
// sessions」比问「哪几条正好是这一句」有用。
func (c *Counter) CountOf(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.stmts {
		if strings.Contains(s, text) {
			n++
		}
	}
	return n
}

// Connector gives the connector that counts into this counter.
//
// 把它交给 WithConnector，store 的每一条连接（常用的那条和丢不得的那条）都从
// 它这里来。
func (c *Counter) Connector() Connector {
	return func(dsn string) (driver.Connector, error) {
		base, err := sqlite.NewConnector(dsn)
		if err != nil {
			return nil, err
		}
		return &countingConnector{base: base, counter: c}, nil
	}
}

// add puts one statement into the count.
func (c *Counter) add(query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = append(c.stmts, query)
}

// countingConnector counts the statements of the connections it opens.
type countingConnector struct {
	base    driver.Connector
	counter *Counter
}

// Connect opens a counting connection.
func (c *countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, counter: c.counter}, nil
}

// Driver gives the driver of the connections underneath.
func (c *countingConnector) Driver() driver.Driver {
	return c.base.Driver()
}

// countingConn counts the statements that go through it.
//
// 它把 SQLite 那些「可选的」接口都转发下去：少转一个，database/sql 就退回那
// 条不带 context 的老路子上（等锁的时候就不认 context 了，规格 §5.1 说的那个
// 「锁要等得到」就破了）。
type countingConn struct {
	driver.Conn
	counter *Counter
}

// Unwrap gives the connection underneath.
//
// SQLite 的 Limit 看着这一层：没有它就看不见底下的连接。
func (c *countingConn) Unwrap() driver.Conn {
	return c.Conn
}

// PrepareContext counts the statement and prepares it.
func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.counter.add(query)
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

// ExecContext counts the statement and runs it.
func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		// 底下的连接不会直接跑这一句：让 database/sql 走准备那条路（那边
		// 数过了），免得数两遍。
		return nil, driver.ErrSkip
	}
	c.counter.add(query)
	return e.ExecContext(ctx, query, args)
}

// QueryContext counts the statement and runs it.
func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.counter.add(query)
	return q.QueryContext(ctx, query, args)
}

// BeginTx starts a transaction.
func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

// Ping checks the connection.
func (c *countingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ResetSession puts the connection back the way it was.
func (c *countingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid says whether the connection can be used.
func (c *countingConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

```

`internal/store/store.go` —— 那条缝：

```go
// Connector opens the connections of the database.
//
// 平时是 SQLite 自己的（见 openConn）。量语句条数的时候是套了一层计数的
// （见 counting.go）。
type Connector func(dsn string) (driver.Connector, error)

// WithConnector gives the store the connections to use.
//
// 那是量具用的：套一层计数的连接，就知道一个请求打了几条语句。生产的那个
// 连接不用它 —— 那时候还是 SQLite 自己的。
func WithConnector(c Connector) Option {
	return func(o *options) { o.connector = c }
}

```

`internal/store/store.go` —— `options` 多一个字段：

```go
	// connector opens the connections. Nil means SQLite's own.
	connector Connector
```

`internal/store/store.go` —— 开连接的地方走那一条缝：

```go
// openConn opens the connections of the database.
//
// 没有量具的时候是 SQLite 自己的连接 —— 那条路一点没变。有量具的时候是量具
// 给的那条（见 counting.go）。
func openConn(path string, o options) (*sql.DB, error) {
	if o.connector == nil {
		return sql.Open("sqlite", dsn(path, o))
	}
	conn, err := o.connector(dsn(path, o))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return sql.OpenDB(conn), nil
}

```

`Open` 里那一句 `sql.Open("sqlite", dsn(path, o))` 换成 `openConn(path, o)`；
`openDurable` 里那一句 `sql.Open("sqlite", dsn(path, d))` 换成 `openConn(path, d)`。

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ -run TestTheCounter -v
```

- [ ] **Step 5: 量一下（改之前的数）**

在 `internal/gateway` 的测试里发一个热请求，数它打了几条语句 —— 那个数是
**5**（规格 §2.1）。量具的用法写在 Task 2 里。

---

### Task 2: 在网关那一层量 —— 改之前的账

**Files:**
- Modify: `internal/gateway/gateway_test.go`（`harness` 带上量具）
- Test: `internal/gateway/gateway_test.go`

量具是从 store 那一层数的，网关这一层的数才有意义（「一个请求」）。

**数什么：** 这一版改的是**会话那一行**的读和写。所以要数的是这一行：

- 热请求：会话那一行被读了**几次**、写了**几次**；
- 冷请求（新会话）：同样。

总数（热 5 / 冷 9）记在计划最后那张表里 —— 它**不是**断言的东西：里面还有
「有没有 key」和「总并发」那两条，它们带 TTL，什么时候问看缓存到期没有。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/gateway_test.go` —— `harness` 多一个字段：

```go
	// counter counts the statements the store sends: the measuring tool
	// (see internal/store/counting.go).
	counter *store.Counter
```

`newHarness` 里那一句开库的地方：

```go
	h.counter = store.NewCounter()
	s, err := store.Open(t.TempDir()+"/t.db", store.WithConnector(h.counter.Connector()))
```

（开库之后建号、存令牌那些语句也算进去 —— 所以量之前要先 `Reset`。）

新的测试：

```go
// TestTheSessionRowIsReadOnce says a request reads the session's row once.
//
// 这一版要盯的就是这个数：那一行的两列（钉在哪个号上、上游的 id）是分开读
// 的，所以每个请求读它两次。合成一条之后是一次。
//
// 数的是**这一行**，不是总数：总数里还有「有没有 key」和「总并发」那两条，
// 它们带 TTL，什么时候问看缓存到期没有 —— 拿总数当断言，测的是 TTL。
func TestTheSessionRowIsReadOnce(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	})
	body := `{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`

	// 第一趟：会话还没有 —— 冷的那一趟。
	h.counter.Reset()
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if got := h.counter.CountOf("FROM sessions"); got != 1 {
		t.Errorf("the reads of the session row = %d, want 1: %v", got, h.counter.Statements())
	}
	if got := h.counter.CountOf("INSERT INTO sessions"); got != 1 {
		t.Errorf("the writes of the session row = %d, want 1: %v", got, h.counter.Statements())
	}

	// 第二趟：会话有了，号也钉上了 —— 热的那一趟。
	h.counter.Reset()
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if got := h.counter.CountOf("FROM sessions"); got != 1 {
		t.Errorf("the reads of the session row = %d, want 1: %v", got, h.counter.Statements())
	}
	if got := h.counter.CountOf("INSERT INTO sessions"); got != 0 {
		t.Errorf("the writes of the session row = %d, want none: %v", got, h.counter.Statements())
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run TestTheSessionRowIsReadOnce -v
```

失败：冷的那一趟读了两次会话那一行（`the reads of the session row = 2, want 1`），
热的那一趟也是两次。

- [ ] **Step 3: 实现**

（量具是 Task 1 做好的。这一件只改测试。）

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/gateway/ -run TestTheSessionRowIsReadOnce -v
```

它现在还**不过** —— 这是这一版要修的东西。Task 5 和 Task 6 修它。

- [ ] **Step 5: 把改之前的数字记下来**

```bash
go test ./internal/gateway/ -run TestTheSessionRowIsReadOnce -v 2>&1 | tee /tmp/before.txt
```

会话那一行：热 **读 2 / 写 0**，冷 **读 2 / 写 2**。

---

### Task 3: 后台那一趟的两条读合成一条

**Files:**
- Modify: `internal/store/store.go`（`AccountsWithWindows`）
- Modify: `internal/usage/round.go`（`Targets`）
- Test: `internal/usage/round_test.go`

读额度那一趟（`usage.Round.Targets`）读四张表。`ListAccounts` 和 `ListExhausted`
是其中两条 —— 1000 个号的时候，第一条是 2.8 ms。合成一条，那一趟就少一次往返。

**口径一个字都不动**：两条查询的「最新」判法（时刻优先、id 平局，规格 §3.4）、
排序、过滤，全都在合成的那一条里照抄。

- [ ] **Step 1: 写失败的测试**

`internal/usage/round_test.go` —— 对拍：同一个库，新旧两条路都跑一遍，结果逐
字段比对。故意造几条 `created_at` 完全相同的读数，让新旧结果不同的时候被抓到。

```go
// TestTheTargetsAreTheSameWithTheMergedReads says the merged read gives
// the same targets as the two reads.
//
// 合成一条只是把两次往返变成一次：算出来的东西必须一个字都不差。
func TestTheTargetsAreTheSameWithTheMergedReads(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		makeAccount(t, s, id)
	}
	// 一个号的标记还没到：它该被读一次。
	if err := s.SetExhausted(ctx, "b", "general", now.Add(-time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	// 一个号的标记还在：它不用被读。
	if err := s.SetExhausted(ctx, "c", "general", now.Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}

	accounts, exhausted, err := s.AccountsWithWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 账号跟 ListAccounts 一样：按 created_at 排，一行一个。
	if len(accounts) != 3 {
		t.Fatalf("the accounts = %+v, want three", accounts)
	}
	// 标记跟 ListExhausted 一样：按账号排，一行一个窗口。
	if len(exhausted) != 2 {
		t.Fatalf("the spent windows = %+v, want two", exhausted)
	}
	if exhausted[0].AccountID != "b" || exhausted[1].AccountID != "c" {
		t.Errorf("the spent windows = %+v, want b then c", exhausted)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/usage/ -run TestTheTargetsAreTheSameWithTheMergedReads -v
```

失败：编译不过（`AccountsWithWindows` 还没有）。

- [ ] **Step 3: 实现**

`internal/store/store.go`：

```go
// AccountsWithWindows gives the accounts and the windows that are spent,
// in one read.
//
// 账号和它的耗尽标记本来是两条读：合成一条，那一趟就少一次往返 —— 1000 个
// 号的时候是 2.8 ms（规格 §3.4）。
//
// 一个号有几条标记就出来几行；没有标记的号出来一行，那三条是空的。账号按
// created_at 排（跟 ListAccounts 一样），标记按账号排（跟 ListExhausted 一
// 样）。
func (s *Store) AccountsWithWindows(ctx context.Context) ([]Account, []Exhausted, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_group,
			a.proxy_url, a.last_error, a.created_at, a.updated_at,
			e.window_key, e.until, e.reason
		FROM accounts a
		LEFT JOIN exhausted_windows e ON e.account_id=a.id
		ORDER BY a.created_at, e.window_key`)
	if err != nil {
		return nil, nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var (
		out      []Account
		spent    []Exhausted
		lastID   string
		haveLast bool
	)
	for rows.Next() {
		var (
			windowKey sql.NullString
			until     sql.NullInt64
			reason    sql.NullString
		)
		a, err := scanAccount(rows, &windowKey, &until, &reason)
		if err != nil {
			return nil, nil, fmt.Errorf("list accounts: %w", err)
		}
		// 一个号有几条标记就出来几行：账号只算一次。
		if !haveLast || a.ID != lastID {
			out = append(out, a)
			lastID = a.ID
			haveLast = true
		}
		if windowKey.Valid {
			spent = append(spent, Exhausted{
				AccountID: a.ID,
				WindowKey: windowKey.String,
				Until:     fromMS(until.Int64),
				Reason:    reason.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, spent, nil
}

```

`internal/usage/round.go` —— `Targets` 里那两条读换成一条：

```go
	accounts, exhausted, err := r.Store.AccountsWithWindows(ctx)
	if err != nil {
		return nil, err
	}
```

（`ListAccounts` 和 `ListExhausted` 那两句删掉。）

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ ./internal/usage/ -count=1
```

- [ ] **Step 5: 量一下**

```bash
go test ./internal/store/ -run XXX -bench BenchmarkRoundTargets -benchtime 30x -count=3
```

（基准要另写，见最后一节。）

---

### Task 4: `HasAPIKeys` 的缓存用事件失效

**Files:**
- Modify: `internal/store/store.go`（`Store` 多一个 `apiKeys`；`HasAPIKeys`；`CreateAPIKey` 和 `RevokeAPIKey` 之后把缓存置掉）
- Test: `internal/gateway/gateway_test.go`

**为什么不能只靠 TTL：** 这个缓存管的是「**要不要鉴权**」。TTL 窗口里有人在后台
申请了一把 key，那窗口里不带 key 的请求会被放行。窗口很短，但性质是鉴权。

所以：**谁改了谁就通知**。改的地方在 store 自己的写方法里（`CreateAPIKey` 和
`RevokeAPIKey`）—— 后台页面走的正是那两条路（`internal/webui/keys.go:88` 和
`:109`），所以把失效放在那里，后台页面就不用管这件事了，也不会有人忘。

**唯一的坑：「false」跟「没缓存过」必须分开。** 调查的原型第一版就踩了这个坑：
缓存永远不命中（每次都当没缓存过）。所以缓存的值是三态的。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/gateway_test.go`：

```go
// TestTheKeyMadeInThePanelIsSeenAtOnce says a key made in the panel is the
// answer the next request gets.
//
// 这个缓存管的是「要不要鉴权」：它慢了半秒，就是半秒的窗口里不带 key 的
// 请求被放行。所以它不能只靠 TTL 过期 —— 谁改了谁就通知。
func TestTheKeyMadeInThePanelIsSeenAtOnce(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	})
	body := `{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`

	// 还没有 key：请求过去。
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}

	// 后台申请了一把 key（后台页面走的就是 store 的这一个方法）。
	if err := h.store.CreateAPIKey(context.Background(), store.APIKey{
		ID: "key_01", Name: "k", KeyHash: "hash", Prefix: "mmx",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 立刻再发一个不带 key 的请求：它得进不来。
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != 401 {
		t.Fatalf("code = %d, want 401: a key was made and the request was let in", w.Code)
	}

	// 撤销了：又没有 key 了，请求过去。
	if err := h.store.RevokeAPIKey(context.Background(), "key_01", time.Now()); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200: the key was taken away and the request was refused", w.Code)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run TestTheKeyMadeInThePanelIsSeenAtOnce -v
```

现在它**过**（因为还没有缓存）。改完缓存以后它还得过 —— 这一条是防着缓存把
鉴权放坏的。

- [ ] **Step 3: 实现**

`internal/store/store.go` —— 三态：

```go
// keyState is what the store knows about the keys in the console.
//
// 三态：「没查过」/「一把都没有」/「有」。「一把都没有」跟「没查过」必须分开
// —— 分不开的话缓存永远不命中（每次都当没查过）。
type keyState int32

const (
	// keysUnknown says nobody has asked yet.
	keysUnknown keyState = iota
	// keysNone says there is no key.
	keysNone
	// keysSome says there at least one key.
	keysSome
)

```

`internal/store/store.go` —— `Store` 多一个字段：

```go
	// apiKeys is the answer to "has anybody asked for a key yet".
	//
	// 它是「要不要鉴权」：缓一秒就有一秒的窗口 —— 那一秒里后台申请了一把
	// key，不带 key 的请求被放行。所以它不能只靠 TTL 过期：谁改了谁就通
	// 知（见 forgetAPIKeys）。
	apiKeys atomic.Int32
```

`internal/store/store.go` —— 那个答案：

```go
// HasAPIKeys says whether any key has been made in the console.
//
// 答案记在内存里：这个数每个请求都要问一次，而它变不变只有一种理由 —— 有
// 人申请了或者撤销了 key。谁改了谁就把这个答案扔掉（见 forgetAPIKeys）。
func (s *Store) HasAPIKeys(ctx context.Context) (bool, error) {
	switch keyState(s.apiKeys.Load()) {
	case keysSome:
		return true, nil
	case keysNone:
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n); err != nil {
		return false, fmt.Errorf("count api keys: %w", err)
	}
	state := keysNone
	if n > 0 {
		state = keysSome
	}
	s.apiKeys.Store(int32(state))
	return n > 0, nil
}

// forgetAPIKeys throws the cached answer away.
//
// 谁改了 key 谁就通知：这个答案是「要不要鉴权」，它慢了半秒就是半秒的窗口
// 里不带 key 的请求被放行。
func (s *Store) forgetAPIKeys() {
	s.apiKeys.Store(int32(keysUnknown))
}

```

`internal/store/store.go` —— `CreateAPIKey` 和 `RevokeAPIKey` 写完了各调一次
`forgetAPIKeys()`。

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/gateway/ -run TestTheKeyMadeInThePanelIsSeenAtOnce -v
go test ./internal/store/ ./internal/webui/ -count=1
```

- [ ] **Step 5: 量一下**

「有没有 key」那条从每个请求一次变成**每个进程一次**（直到有人改 key）。

---

### Task 5: 会话那一行读一次就够

**Files:**
- Create: `internal/store/sessions.go`（会话那几个方法从 `store.go` 挪过来 —— 一个字都不改，只是搬家）
- Modify: `internal/store/store.go`（少掉那几个方法）
- Modify: `internal/rotator/rotator.go`（`Pool` 的 `SessionAccount` 换成 `SessionState`；`Pick` 读那一行一次；`Plan` 带上上游的 id）
- Modify: `internal/gateway/gateway.go`（`poolStore` 和 `poolAdapter` 跟着换；请求那一路用 `plan` 里的 id）
- Test: `internal/store/store_test.go`, `internal/rotator/rotator_test.go`, `internal/gateway/gateway_test.go`

`sessions` 那一行的两列（钉在哪个号上、上游的 id）现在是**分开读**的：

- `rotator.UpstreamID`（`internal/rotator/rotator.go:536`）读 `upstream_id`；
- `rotator.Pick`（`internal/rotator/rotator.go:338`）读 `account_id`。

同一个请求里，同一行，读两次。

**新的走法**：`Pick` 读那一行**一次**（两列一起拿），上游的 id 跟着 **Plan** 走回
网关 —— 网关不用再单独问一次。

**为什么 id 跟着 plan 走，而不是另外传一遍：** 那一行每个请求都要问一次，问出来
的东西就该跟着走。再传一遍就是又问一次 —— 等于没改（规格 §3.1 那句「唯一的坑」）。

**为什么让 `Pick` 读，而不是让网关读了再递给 `Pick`：** 读的时刻离用它的那一刻越
近越好。`Pick` 读的时候正要决定「这一段对话钉在哪个号上」—— 那正是要用的时刻。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/gateway_test.go` —— Task 2 那个测试就是这一件的目标（热请求读
那一行一次）。它现在**不过**。

`internal/rotator/rotator_test.go` —— 轮转读那一行的时候，两列要一起拿：

```go
// TestTheSessionRowIsReadInOneGo says the rotator reads the session's row
// in one read.
//
// 那一行的两列是同一行：分开读就是两次往返，而每个请求都要问这一行。
func TestTheSessionRowIsReadInOneGo(t *testing.T) {
	ctx := context.Background()
	p := &fakePool{}
	r := newRotator(p, Options{Strategy: StrategySticky})
	makeAccount(t, p, "acc_01")

	if _, err := r.Pick(ctx, "sess-1", "MiniMax-M3", nil); err != nil {
		t.Fatal(err)
	}
	if got := p.sessionReads; got != 1 {
		t.Errorf("the reads of the session row = %d, want 1", got)
	}
}

```

（`fakePool` 多一个 `sessionReads` 计数器和一个 `SessionState` 方法。）

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/rotator/ -run TestTheSessionRowIsReadInOneGo -v
go test ./internal/gateway/ -run TestTheSessionRowIsReadOnce -v
```

失败：轮转读那一行还是两次（或者编译不过 —— `SessionState` 还没有）。

- [ ] **Step 3: 实现**

**第一步：store 那边。**

`internal/store/sessions.go`（新）—— 把 `BindSession`、`SessionAccount`、
`UpstreamID`、`upstreamIDOf`、`randomID` 从 `store.go` 挪过来（搬家，不改），
再加一个新方法：

```go
// SessionState is what the store knows about a session's row.
//
// 「没读过」跟「读过、那一行是空的」是两件事：分不开的话，轮转会以为每一段
// 对话都没有绑定的号，每一次都重新绑。
type SessionState struct {
	// Known says whether the row was read at all.
	Known bool
	// AccountID is the account the session is bound to. Empty when it is
	// bound to nobody.
	AccountID string
	// UpstreamID is the id the upstream knows this conversation by.
	UpstreamID string
}

// SessionState gives the session's row in one read.
//
// 那一行的两列（钉在哪个号上、上游的 id）本来是分开读的：分开读就是两次往
// 返，而每个请求都要问这一行。
func (s *Store) SessionState(ctx context.Context, sessionKey string) (SessionState, error) {
	var (
		account  sql.NullString
		upstream sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT account_id, upstream_id FROM sessions WHERE session_key=?", sessionKey).
		Scan(&account, &upstream)
	if errors.Is(err, sql.ErrNoRows) {
		// 那一行还没有：读过了，是空的。
		return SessionState{Known: true}, nil
	}
	if err != nil {
		return SessionState{}, fmt.Errorf("the session row: %w", err)
	}
	return SessionState{
		Known:      true,
		AccountID:  account.String,
		UpstreamID: upstream.String,
	}, nil
}

```

**第二步：轮转那边。**

`internal/rotator/rotator.go` —— `Pool` 接口里 `SessionAccount` 换成：

```go
	// SessionState gives the session's row in one read.
	//
	// 那一行的两列（钉在哪个号上、上游的 id）一起拿：分开问就是两次往返。
	SessionState(ctx context.Context, sessionKey string) (store.SessionState, error)
```

`Plan` 多一个字段：

```go
	// UpstreamID is the id the upstream knows this conversation by.
	//
	// 它是跟着那一行一起读出来的：那一行每个请求都要问一次，所以问出来的
	// 东西就跟着走，不用再问一遍。
	UpstreamID string
```

`Pick` 里那处读会话绑定的地方：

```go
	// 这一段对话在库里那一行：钉在哪个号上、上游的 id。
	//
	// 那一行以前是分开读的（两次往返），而每个请求都要问它一次。
	var state store.SessionState
	if sessionKey != "" {
		state, err = r.pool.SessionState(ctx, sessionKey)
		if err != nil {
			// 读不出来不是「没绑定」：当成没绑定的话，库里那个绑定会被覆
			// 盖掉。
			return Plan{}, fmt.Errorf("the session row could not be read: %w", err)
		}
	}
```

（`sticky && sessionKey != ""` 那一段里，`boundID, ok, err := r.pool.SessionAccount(ctx, sessionKey)`
换成 `boundID, ok := state.AccountID, state.AccountID != ""`。）

**上游的 id 跟着 `Plan` 走：**

```go
	// 上游的 id：那一行里的那个。
	//
	// 那一行没有 id（新的对话）：问池子要一个 —— 它会编一个写下去。
	upstreamID := state.UpstreamID
	if upstreamID == "" {
		if ids, ok := r.pool.(UpstreamIDs); ok {
			upstreamID, err = ids.UpstreamID(ctx, sessionKey)
			if err != nil {
				return Plan{}, fmt.Errorf("the upstream id could not be read: %w", err)
			}
		}
	}
```

（`Plan` 的每一处 `return r.plan(...)` 都要带上这个 id —— 最省事的做法是
`r.plan` 多收一个参数。）

**第三步：网关那边。**

`internal/gateway/gateway.go` —— `poolStore` 接口里 `SessionAccount` 换成
`SessionState`（同样的一句），`poolAdapter` 跟着换。

请求那一路：那个单独问上游 id 的地方去掉，id 跟着 `Plan` 走：

```go
	sess := rotator.SessionOf(r.Header, systemOf(body), firstUserOf(body))
	// 发给上游的会话 id 是数据库里存着的那个：不重算，也不把客户端
	// 自己那串字符串发出去。取不到就是取不到，不能拿别的顶上：顶上来的
	// 那个上游没见过，会把这段对话当成新的一段。
	//
	// 它是跟着轮转的 plan 一起回来的：那一行每个请求都要读一次，读出来
	// 的东西就跟着走，不用再问一遍。池子给不出 id 的时候（那种池子没有
	// 上游 id 这个本事），用的是请求自己算出来的那个。
	upstreamID := sess.UpstreamID
```

`for` 循环里 `Pick` 之后：

```go
		if plan.UpstreamID != "" {
			upstreamID = plan.UpstreamID
		}
```

`rotator.UpstreamID`（那个公开方法）网关不再用了 —— id 跟着 plan 走。留着它
（它是 rotator 的公开方法，别的调用方可能用），但网关这一路不再调它。

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ ./internal/rotator/ ./internal/gateway/ -count=1
```

- [ ] **Step 5: 量一下**

```bash
go test ./internal/gateway/ -run TestTheSessionRowIsReadOnce -v
```

热请求：会话那一行**读 1 次**（改之前 2 次），**写 0 次**。

---

## 前后两遍

**同一台机器，同一条命令**（量具是 Task 1 做的那个）。

```bash
go test ./internal/gateway/ -run TestTheSessionRowIsReadOnce -v
```

| | 改之前 | 改之后 |
|---|---|---|
| 热请求的语句数 | 5 | ? |
| 冷请求的语句数 | 9 | ? |
| 热请求：会话那一行读 | 2 | 1 |
| 热请求：会话那一行写 | 0 | 0 |
| 冷请求：会话那一行读 | 3 | ? |
| 冷请求：会话那一行写 | 2 | ? |
| 后台那一趟的读 | 4 | 3 |

改之前的那些数是规格 §2 里量出来的（调查在 `E:\tmp\inv-req\repo` 的副本上做，
`internal/store/counting.go` 给 SQLite 套了一层计数）。

**总数不是断言的东西** —— 里面还有「有没有 key」和「总并发」那两条，它们带
TTL，什么时候问看缓存到期没有。断言的是**会话那一行**的读和写：那才是这一
版动的东西。

## 下一个（不在这一版里）

**冷路径那两次写合成一条**（规格 §3.2）：`UpstreamID` 的 INSERT 和
`BindSession` 的 INSERT 写的是同一行，合成一条 INSERT，写进 `account_id` 和
`upstream_id`，用 `RETURNING upstream_id` 把「别人先写了」的那个 id 拿回来。

**这一版不做，理由有两条：**

1. **规格 §2.3 那个数是在 `synchronous=full` 下量的**（写一笔 874 µs）。上一
   轮已经把 `normal` 做成了默认（写一笔 64 µs），那两次写现在一共 128 µs ——
   合成一条省下的是 64 µs。而要合成它，就得把 `rotator.Pick` 的结构改掉
   （写的那一步要挪到挑完号之后），那是这个项目里最容易改坏的一段逻辑。
2. **这一版的量具刚好能回答「值不值得」**：改完之后冷路径写几遍、每遍多少
   毫秒，量出来再说。够大就做，不够大就记在这里。

写锁排队那笔钱（100 个并发冷请求）也要用这一版的量具重新量一遍 —— 上一轮的
`synchronous=normal` 已经把 fsync 的那部分拿掉了，剩下的才是排队本身。
