# 每个请求少打几次库 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一个聊天请求打在库上的次数从 5 条（热）/ 9 条（冷）降到 3 条 / 5 条。

**Architecture:** 三个改动 —— 量具（数一个请求打了几条语句）、`HasAPIKeys` 的答案缓存在内存里（谁改了谁通知）、会话那一行读一次就够（`UpstreamID` 那条语句也顺带从三条变成一条）。缓存会话行、token、账号行**不做** —— 调查里实测它们会错。后台那一趟的两条读合成一条也**不做** —— 量过了，合成的那一条更慢（见 Task 3）。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`（纯 Go）、`database/sql`。

**规格：** `docs/superpowers/specs/2026-11-19-mmxcode-request-db-round-trips-design.md`

**做的顺序：** 1 → 4 → 5 → 2。Task 2 那个测量的测试要等 Task 5 修好了才
会过（它数的就是 Task 5 改的那个数），所以它放在最后做 —— 中间每一步仓库都
是绿的。

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
| `internal/store/store.go` | `options` 多一个 `connector`（量具从这儿进来）、`HasAPIKeys` 的缓存 |
| `internal/store/sessions.go`（新） | 会话那几个方法（`store.go` 已经 2500 多行了）：`SessionState`、一条语句的 `UpstreamID` |
| `internal/rotator/rotator.go` | `Pool` 接口的 `SessionAccount` 换成 `SessionState`；`Pick` 读那一行一次，上游的 id 跟着 `Plan` 走 |
| `internal/gateway/gateway.go` | `poolAdapter` 跟着换；请求那一路用 `plan` 里的上游 id |
| `internal/webui/keys.go` | 不用改 —— 失效在 store 自己的写方法里（见 Task 4） |
| `internal/usage/round.go` | 不用改 —— 那两条读合成一条更慢（见 Task 3） |

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
	// 要「normal」才有那两条连接：常用的那条不 fsync，丢不得的那条才另开
	// 一条（synchronous=full）。默认的时候 SQLite 自己就是 full，两条是同
	// 一条 —— 那样这条测试就只是在数同一条连接上的语句，没有测到「另一
	// 条」这件事。
	s := openTestOpts(t, WithConnector(counter.Connector()), WithSynchronous("normal"))
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

// Reset throws the count away.
//
// 量一个请求之前要先把准备的开头扔掉：建号、存令牌那些语句不是这个请求
// 打的，它们混在里面量出来的条数就是多的。
func (c *Counter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = nil
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
// 条不带 context 的老路子上（等锁的时候就不认 context 了）。
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
//
// 出错的时候不包一层「open sqlite」：包了的是 Open 和 openDurable，它们是
// 唯一会把这句话说出口的地方 —— 包两遍的话那句话就重复了。
func openConn(path string, o options) (*sql.DB, error) {
	if o.connector == nil {
		return sql.Open("sqlite", dsn(path, o))
	}
	conn, err := o.connector(dsn(path, o))
	if err != nil {
		return nil, err
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
// 它们什么时候问看缓存的状态 —— 拿总数当断言，测的是缓存。
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

它现在还**不过** —— 这是这一版要修的东西。Task 5 修它。

- [ ] **Step 5: 把改之前的数字记下来**

```bash
go test ./internal/gateway/ -run TestTheSessionRowIsReadOnce -v 2>&1 | tee /tmp/before.txt
```

会话那一行：热 **读 2 / 写 0**，冷 **读 2 / 写 2**。

---

### Task 3: 后台那一趟的两条读合成一条 —— **不做**（量过了）

**这一条做过了，也量过了，然后撤回来了。** 下面是量出来的数 —— 留着是给下一个
人看的，免得再想着「合成一条总该快点吧」。

计划的原文是：把 `usage.Round.Targets` 里的 `ListAccounts` 和 `ListExhausted`
合成一条（`accounts` 带上它的耗尽标记），说「1000 个号的时候是 2.8 ms」。

**做出来的两种写法都比原来的两条读慢**（同一个夹具：1000 个号、30 天签到，
每个量三遍）：

| | 一次 |
|---|---|
| 两条读（`ListAccounts` + `ListExhausted`） | **2.6 ms** |
| 合成一条（`UNION ALL`：两条原句各自带着自己的 `ORDER BY`，各在一个子查询里，
外面 `LIMIT -1` 挡住 SQLite 把子查询拍平） | **4.2 ms** |
| 合成一条（`LEFT JOIN`） | **4.2 ms** |

**多出来的那 1.6 ms 是怎么来的：** 合成的那一条里，账号那一行**多带几列**
（`UNION ALL` 补四列空的，`LEFT JOIN` 带上窗口的三列）—— Go 那边每读一行账号
就多扫几个值、多分配几个（`sql.NullString` / `sql.NullInt64` 各一个）。1000 行
就是几千个额外的分配。

**那 2.6 ms 本身是什么：** 不是 SQLite 的排序。只扫 id 的那一条只要 **0.47 ms**
（带上排序 0.76 ms）—— 差的 1.8 ms 是 Go 那边把 1000 行读成 1000 个
`Account` 结构体（八个字符串字段，八千个字符串分配）。给 `accounts(created_at)`
加一条索引再量，一模一样（2.5~2.8 ms）：**排序不是瓶颈，那条索引治不了它。**

**省下来的是什么：** 一次往返（几十微秒）。**换来的是 1.6 ms 的亏。**

**而且这一趟每 30 秒才跑一次**：2.6 ms 是 0.009% 的一个核。

**「口径一个字都不动」也做不到**：合成的那一条只有一种排序 —— 账号按
`created_at` 排的时候，标记就排不成 `account_id` 了（`ListExhausted` 是按
`account_id` 排的）。要一样就得在 Go 里再排一遍：那是把两条读换成一条读加一次
排序，外加上面那 1.6 ms。

→ **这一条不做。** 两条读留着（`internal/usage/round.go` 一个字没改）。

**顺带记下来的一个数**（下一个碰到「读额度这一趟」的人会想知道）：那 2.6 ms
里 1.8 ms 是 Go 那边把行读成结构体的分配。**真的要把这一趟变快，要动的是
「读出来的行怎么进内存」，不是 SQL。**

### Task 4: `HasAPIKeys` 的缓存用事件失效

**Files:**
- Modify: `internal/store/store.go`（`Store` 多一个 `apiKeys`；`HasAPIKeys`；`CreateAPIKey` 和 `RevokeAPIKey` 之后把缓存置掉）
- Test: `internal/gateway/gateway_test.go`
- Test: `internal/store/apikeys_cache_test.go`（新 —— 缓存这一层自己的测试：它数库那一趟，问五次只查一次）

**为什么不能只靠 TTL：** 这个缓存管的是「**要不要鉴权**」。TTL 窗口里有人在后台
申请了一把 key，那窗口里不带 key 的请求会被放行。窗口很短，但性质是鉴权。

所以：**谁改了谁就通知**。改的地方在 store 自己的写方法里（`CreateAPIKey` 和
`RevokeAPIKey`）—— 后台页面走的正是那两条路（`internal/webui/keys.go:88` 和
`:109`），所以把失效放在那里，后台页面就不用管这件事了，也不会有人忘。

**唯一的坑：「false」跟「没缓存过」必须分开。** 调查的原型第一版就踩了这个坑：
缓存永远不命中（每次都当没缓存过）。所以缓存的值是三态的。

**第二个坑（实现的时候发现的，计划里原来没有）：查库和写答案要锁在一起。**
一个正在查库的请求会在别人申请了 key 之后，把「一把都没有」这个旧答案写回
去 —— 那个答案留在缓存里不动了，门就一直开着。所以 `HasAPIKeys` 查库的那一
段和 `forgetAPIKeys` 拿着**同一把锁**。

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
	// keysSome says there is at least one key.
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
	// apiKeysMu guards the moment when the answer is put back.
	//
	// 查库和把答案写回去要锁在一起：不然一个正在查库的请求会在别人申请了
	// key 之后，把「一把都没有」这个旧答案写回去 —— 那个旧答案就留在缓存
	// 里不动了，门就一直开着（见 forgetAPIKeys）。
	apiKeysMu sync.Mutex
```

`internal/store/store.go` —— 那个答案：

```go
// HasAPIKeys says whether any key has been made in the console.
//
// 一把都没有的时候网关不鉴权：那时候它只该听在本机。申请过就不一样
// 了：那是有人打算从别的机器用它。
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
	// 没查过：去查库。查库和写答案锁在一起，不然一个正在查库的请求会在
	// 别人申请了 key 之后，把「一把都没有」这个旧答案写回去 —— 那个答案
	// 留在缓存里不动了，门就一直开着。
	s.apiKeysMu.Lock()
	defer s.apiKeysMu.Unlock()
	// 等锁的这会儿，答案可能已经有人填上了。
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
//
// 它拿着那把锁：不然一个正在查库的请求会在这次通知之后把旧答案写回去。
func (s *Store) forgetAPIKeys() {
	s.apiKeysMu.Lock()
	defer s.apiKeysMu.Unlock()
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
// TestTheSessionRowIsReadInOneGo says the rotator reads the session's
// row in one read.
//
// 那一行的两列是同一行：分开读就是两次往返，而每个请求都要问这一行。
func TestTheSessionRowIsReadInOneGo(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := newRotator(p, Options{Strategy: StrategySticky})

	if _, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil); err != nil {
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
`UpstreamID`、`randomID` 从 `store.go` 挪过来（搬家，`UpstreamID` 除外：它变成
一条语句了，见下），再加一个新方法：

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

**`UpstreamID` 变成一条语句**（它现在是三条：读、写、再读一遍）：

```go
// UpstreamID gives the session's upstream id, making one if the
// session has none.
//
// 一条语句：写完把这一行最后的上游 id 拿回来（RETURNING）。从前是「读一
// 遍、写一遍、再读一遍」—— 那最后一遍是为了知道别人有没有先写一个 id。
// RETURNING 是同一件事，而且省掉两趟。
//
// 那一行已经有了 id 的时候：写的还是那一行，id 不换（先到先得）。
//
// The id is random: it is not derived from the session key, so the
// upstream never sees a hash of the words in the request. It is also
// kept: the same session has the same upstream id every time it is
// asked for.
//
// The key is what decides which id a conversation gets, and two
// conversations that arrive under the same key are one conversation as
// far as this store can tell. Telling them apart is the caller's job:
// it has to give them different keys.
//
// The session does not have to be bound to an account yet: a
// conversation has an upstream id before it has an account.
func (s *Store) UpstreamID(ctx context.Context, key string) (string, error) {
	newID, err := randomID()
	if err != nil {
		return "", err
	}
	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO sessions (session_key, account_id, upstream_id, updated_at)
		VALUES (?, NULL, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET
			upstream_id=CASE WHEN sessions.upstream_id IS NULL OR sessions.upstream_id=''
			                  THEN excluded.upstream_id ELSE sessions.upstream_id END
		RETURNING upstream_id`, key, newID, ms(s.now())).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("make the session %s: %w", key, err)
	}
	if id == "" {
		return "", fmt.Errorf("the session %s has no upstream id", key)
	}
	return id, nil
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
	//
	// 只有「那一行还没有 id」的时候才问：问一次就是一次写，而热路径上那一
	// 行已经有 id 了（那一行每个请求都要读一次，所以问出来的东西就跟着
	// 走，不用再问一遍）。
	upstreamID := state.UpstreamID
	if upstreamID == "" && sessionKey != "" {
		if ids, ok := r.pool.(UpstreamIDs); ok {
			upstreamID, err = ids.UpstreamID(ctx, sessionKey)
			if err != nil {
				return Plan{}, fmt.Errorf("the upstream id could not be read: %w", err)
			}
		}
	}
```

**`sessionKey != ""` 那个条件不能省：** 没有会话就没有对话要命名 —— 空的会话行
不该有（`Rotator.UpstreamID` 里就是这么写的，注释也是那么说的）。

**`upstreamID == ""` 那个条件更不能省：** 库里那个 `UpstreamID` 现在是**一条写**
（`INSERT ... ON CONFLICT ... RETURNING`）：那一行已经有 id 的时候它照样写一遍
（实测：这种 upsert ≈ 918 µs，一条普通的读 ≈ 32 µs）。热路径上那一行**已经有
id 了**，所以不能每趟都去问池子要 id。

（`Plan` 的每一处 `return r.plan(...)` 都要带上这个 id —— 最省事的做法是
`r.plan` 多收一个参数。`Pick` 里有**五处**。）

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
go test ./internal/gateway/ -run "TestTheSessionRowIsReadOnce|TestTheStatementsOfARequest" -v
```

| | 改之前 | 改之后（量出来的） |
|---|---|---|
| 热请求的语句数 | 5 | **3** |
| 冷请求的语句数 | 9 | **5** |
| 热请求：会话那一行读 | 2 | **1** |
| 热请求：会话那一行写 | 0 | **0** |
| 冷请求：会话那一行读 | 3 | **1** |
| 冷请求：会话那一行写 | 2 | **2** |
| 后台那一趟的读 | 4 | **4**（合成更慢，不做 —— 见 Task 3） |

**改之后的数是量出来的**（`TestTheStatementsOfARequest`，在仓库里留着）：

- 热请求那 3 条：读会话那一行 + `FROM tokens` + `FROM accounts WHERE id=?`；
- 冷请求那 5 条：上面那 3 条，加 `INSERT INTO sessions … RETURNING upstream_id`
  （造上游 id）和 `INSERT INTO sessions (session_key, account_id, updated_at)`
  （钉号）—— 会话那一行**读 1 次、写 2 次**；
- 还有两个参考数：**第一趟**（什么都还没缓存）是 **10** 条（多出「有没有 key」
  1 条、「总并发」1 条、fallback 那 3 条）；**后台那一趟**（读额度）是 **4** 条。

**量具怎么数：** 它挂在驱动这一层，数的是 `PrepareContext` / `ExecContext` /
`QueryContext` 三个口子。一条**只准备一次、跑很多遍**的语句只算 **1** 条
（数在准备那一下）—— 像 `SaveCheckins` 那种批量写，N 行也只算 1 条，想看行数
的话这个量具给不了。这次量到的 3 / 5 / 10 都是「一条语句跑一次」的一次性查
询，走的是直接执行那条路，所以条数就是真的往返次数。

（热请求的 3 和冷请求的 5 都是**缓存热了**的数：「有没有 key」那条问过一次就
不问了（它是事件失效的，谁改了 key 谁就通知），「总并发」那条带 TTL，缓存过期
的时候才问。）

改之前的那些数是规格 §2 里量出来的（调查在 `E:\tmp\inv-req\repo` 的副本上做，
`internal/store/counting.go` 给 SQLite 套了一层计数）。

**总数不是断言的东西** —— 里面还有「有没有 key」和「总并发」那两条，它们什么
时候问看缓存的状态。断言的是**会话那一行**的读和写：那才是这一版动的东西。

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
