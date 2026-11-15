# 只读热账号 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 网关读额度只碰该碰的账号（签到待补的、标记到期的、从没读过的、还有热的），每一趟有并发有时限有预算；额度用完的账号立刻标记、后台读一次把标记改准；签到给的积分读一次就进卡；每个 mcode 账号同时只跑一个请求。

**Architecture:** `internal/manager` 记着「谁刚被用过」（热账号的计数，内存里的窗口计数），`internal/usage` 新增一个 `Round`：它算出该读哪些账号、按什么顺序读、一趟读几个、并发几个、时限多久、预算多久。`internal/gateway` 记着「谁在飞」（每个账号同时一个请求）和总并发，聊天那条路上多两道门。签到那条路（调度器 + 后台页面）签完读一次，读到的数说话：说有额度就清掉「用完」的标记。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`、`context`、`sync`

设计文档：`docs/superpowers/specs/2026-11-15-mmxcode-magpie-plugin-design.md`（§3、§4、§5、§6、§8、§9）

---

## 三条必须守住的不变量

1. **额度只读该读的账号。** 账号多起来以后，冷账号的额度没有必要读 —— 读了也是旧的。要读的是：签到待补的、标记到期的、从没读过的、还有热的。别的等下一趟。
2. **用完的账号回不来，除非有读数证明它有额度。** 标记的那一刻就把「什么时候回来」定下来：读到的窗口时刻在将来就用它，取不到或者已经在过去就用冷却时间。后台再读一次，把标记改成真的时刻。签到给了积分的那一次读数说它有额度，就清掉标记。
3. **一个账号同时只跑一个请求。** 挑账号时避开在飞的账号；它忙就换号，但不改绑（忙不是它不能答了的理由）。总并发是可用账号的五分之一，小规模至少三个。

## 账号的五个状态

| 状态 | 是什么意思 | 卡上 | 在轮转里吗 |
|---|---|---|---|
| **U 未读** | 从没读过账本 | 没有它 | 在（但挑不到） |
| **H 热** | 最近被用过 | 数是新的 | 在 |
| **C 冷** | 一阵子没被用 | 数是旧的 | 在 |
| **X 用完** | 被记了「额度用完」 | 显示用完 | 不在 |
| **P 待补** | 签到成功，账本还没读到 | 数是旧的 | 在 |

---

### Task 1: 配置

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go` 里加：

```go
// TestTheUsageDefaults says what a round of reading the credits does
// when nothing is said about it.
//
// 读额度有几道控：多久算热、多久扫一遍、并发几个、每个账号的时限、
// 一趟的预算、一趟最多读几个、卡片现读读几个。这些数是配的，配不
// 着的时候也有一个说得过去的数。
func TestTheUsageDefaults(t *testing.T) {
	c := Default()
	if c.Usage.Refresh.HotWindow.Duration() != 5*time.Minute {
		t.Errorf("hot_window = %s, want 5m", c.Usage.Refresh.HotWindow)
	}
	if c.Usage.Refresh.ScanEvery.Duration() != 30*time.Second {
		t.Errorf("scan_every = %s, want 30s", c.Usage.Refresh.ScanEvery)
	}
	if c.Usage.Refresh.Concurrency != 4 {
		t.Errorf("concurrency = %d, want 4", c.Usage.Refresh.Concurrency)
	}
	if c.Usage.Refresh.Timeout.Duration() != 15*time.Second {
		t.Errorf("timeout = %s, want 15s", c.Usage.Refresh.Timeout)
	}
	if c.Usage.Refresh.Budget.Duration() != 30*time.Second {
		t.Errorf("budget = %s, want 30s", c.Usage.Refresh.Budget)
	}
	if c.Usage.Refresh.Batch != 6 {
		t.Errorf("batch = %d, want 6", c.Usage.Refresh.Batch)
	}
	if c.Usage.Refresh.Card != 2 {
		t.Errorf("card = %d, want 2", c.Usage.Refresh.Card)
	}
	if c.Usage.Refresh.CardBudget.Duration() != 5*time.Second {
		t.Errorf("card_budget = %s, want 5s", c.Usage.Refresh.CardBudget)
	}
}

// TestTheConcurrencyDefaults says how many requests may be in the air.
//
// 每个账号同时一个请求；总并发是账号数的五分之一，小规模至少三个。
func TestTheConcurrencyDefaults(t *testing.T) {
	c := Default()
	if c.Concurrency.PerAccount != 1 {
		t.Errorf("per_account = %d, want 1", c.Concurrency.PerAccount)
	}
	if c.Concurrency.TotalRatio != 5 {
		t.Errorf("total_ratio = %d, want 5", c.Concurrency.TotalRatio)
	}
	if c.Concurrency.TotalMin != 3 {
		t.Errorf("total_min = %d, want 3", c.Concurrency.TotalMin)
	}
	if c.Concurrency.Wait.Duration() != 10*time.Second {
		t.Errorf("wait = %s, want 10s", c.Concurrency.Wait)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/config/
```

失败：`c.Usage undefined`。

- [ ] **Step 3: 写配置**

`internal/config/config.go` 里加两个结构和两个字段：

```go
// Usage holds how the credits are read.
type Usage struct {
	// Refresh holds how one round of reading the credits goes.
	Refresh Refresh `yaml:"refresh" json:"refresh"`
}

// Refresh holds the controls of one round of reading the credits.
//
// 一趟读额度有四道控：并发（读得快，不是读得多）、每个账号的时限
// （一个卡住的账号不能拖住整趟）、一趟的预算（预算用完就停）、一趟
// 最多读几个（剩下的下一趟接着读）。
//
// 配 0 的时候按 1 算：0 并发、0 个一批都是一趟也读不了，那不是关掉，
// 是坏了。
type Refresh struct {
	// HotWindow is how long an account counts as hot after it was used.
	HotWindow Duration `yaml:"hot_window" json:"hot_window"`
	// ScanEvery is how often the accounts that should be read are read.
	ScanEvery Duration `yaml:"scan_every" json:"scan_every"`
	// Concurrency is how many accounts one round reads at once.
	Concurrency int `yaml:"concurrency" json:"concurrency"`
	// Timeout is how long one account's reading may take.
	Timeout Duration `yaml:"timeout" json:"timeout"`
	// Budget is how long one round may take.
	Budget Duration `yaml:"budget" json:"budget"`
	// Batch is how many accounts one round reads at most.
	Batch int `yaml:"batch" json:"batch"`
	// Card is how many accounts the card's own refresh reads.
	Card int `yaml:"card" json:"card"`
	// CardBudget is how long the card's own refresh may take.
	CardBudget Duration `yaml:"card_budget" json:"card_budget"`
}

// Concurrency holds how many requests may be in the air at once.
//
// 配 0 的时候按 1 算：0 并发是一个请求也进不来，那不是关掉，是坏了。
type Concurrency struct {
	// PerAccount is how many requests one account takes at once.
	PerAccount int `yaml:"per_account" json:"per_account"`
	// TotalRatio is how many accounts share one place (5 = one in five).
	//
	// 五分之一：100 个账号同时只用 20 个，这 20 个把额度用完了，还有
	// 80 个是满的可以接。一个接一个地烧，而不是所有账号一起烧到没。
	TotalRatio int `yaml:"total_ratio" json:"total_ratio"`
	// TotalMin is the floor for a small number of accounts.
	//
	// 5 个账号只跑 1 个太憋屈：小规模的时候用 min(账号数, 3)。
	TotalMin int `yaml:"total_min" json:"total_min"`
	// Wait is how long a request waits for a free place.
	Wait Duration `yaml:"wait" json:"wait"`
}
```

`Config` 里加：

```go
	// Usage holds how the credits are read.
	Usage Usage `yaml:"usage" json:"usage"`
	// Concurrency holds how many requests may be in the air at once.
	Concurrency Concurrency `yaml:"concurrency" json:"concurrency"`
```

`Default()` 里加：

```go
		Usage: Usage{Refresh: Refresh{
			HotWindow:  Duration(5 * time.Minute),
			ScanEvery:  Duration(30 * time.Second),
			Concurrency: 4,
			Timeout:    Duration(15 * time.Second),
			Budget:     Duration(30 * time.Second),
			Batch:      6,
			Card:       2,
			CardBudget: Duration(5 * time.Second),
		}},
		Concurrency: Concurrency{
			PerAccount: 1,
			TotalRatio: 5,
			TotalMin:   3,
			Wait:       Duration(10 * time.Second),
		},
```

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/config/
```

- [ ] **Step 5: 提交**

```
git add internal/config
git commit -m "feat(config): 读额度和并发的配置"
```

---

### Task 2: 热账号的计数

**Files:**
- Create: `internal/manager/hot.go`
- Create: `internal/manager/hot_test.go`
- Modify: `internal/manager/manager.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/manager/hot_test.go`：

```go
package manager

import (
	"testing"
	"time"
)

// TestTheHotCountForgetsTheOldUses says a use that is out of the
// window is not a use any more.
//
// 热账号是"刚刚"的账：窗口一过就算数。它不落库，所以窗口一过就忘了。
func TestTheHotCountForgetsTheOldUses(t *testing.T) {
	now := time.Now()
	c := newHotCounter()
	c.mark("acc_1", time.Minute, now.Add(-2*time.Minute))
	if got := c.list(time.Minute, now); len(got) != 0 {
		t.Fatalf("the account is still hot: %v", got)
	}
	c.mark("acc_1", time.Minute, now)
	if got := c.list(time.Minute, now); len(got) != 1 || got[0] != "acc_1" {
		t.Fatalf("the hot accounts = %v, want the one", got)
	}
}

// TestTheHotCountCountsTheUses says the account used most comes first.
//
// 热的里面也要有先后：刚用得多的先读。
func TestTheHotCountCountsTheUses(t *testing.T) {
	now := time.Now()
	c := newHotCounter()
	c.mark("acc_1", time.Minute, now)
	c.mark("acc_2", time.Minute, now)
	c.mark("acc_2", time.Minute, now)
	c.mark("acc_3", time.Minute, now)
	got := c.list(time.Minute, now)
	if len(got) != 3 {
		t.Fatalf("the hot accounts = %v, want three", got)
	}
	if got[0] != "acc_2" {
		t.Errorf("the first hot account = %q, want the one used twice", got[0])
	}
}

// TestTheHotCountIsNotBig says the counter does not grow without end.
//
// 账号多了也不会把它撑大：过期的自己清掉，清完还有多就把最久没用的
// 那个丢掉。
func TestTheHotCountIsNotBig(t *testing.T) {
	now := time.Now()
	c := newHotCounter()
	for i := 0; i < maxHotAccounts+50; i++ {
		c.mark(string(rune('a'+i%26))+time.Duration(i).String(), time.Minute, now)
	}
	if got := len(c.list(time.Minute, now)); got > maxHotAccounts {
		t.Fatalf("the counter holds %d accounts, want at most %d", got, maxHotAccounts)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/manager/ -run Hot
```

失败：`undefined: newHotCounter`。

- [ ] **Step 3: 写计数器**

创建 `internal/manager/hot.go`：

```go
package manager

import (
	"sort"
	"sync"
	"time"
)

// maxHotAccounts is how many accounts the counter keeps.
//
// 两百个账号的用量已经够看了：再多就是记着没人看的账。超过的时候，
// 最久没用的那个被丢掉。
const maxHotAccounts = 200

// hotUse is one account's use in the window.
type hotUse struct {
	// count is how many times it was used without a gap of a whole
	// window.
	//
	// 它是"用得多不多"的粗略数：中间隔了一个整窗口就从头数。热账号
	// 名单要的是"刚用过"，先后的粗略先后就够，不必是精确的滑动窗口。
	count int
	// last is when it was used last.
	last time.Time
}

// hotCounter counts the accounts that were used lately.
//
// 它记的是"刚刚"的账，不是账本：窗口一过，用量就算数，所以它不落库。
type hotCounter struct {
	mu sync.Mutex
	// at holds the accounts that were used.
	at map[string]*hotUse
}

// newHotCounter makes a counter.
func newHotCounter() *hotCounter {
	return &hotCounter{at: map[string]*hotUse{}}
}

// mark says an account was used.
func (c *hotCounter) mark(accountID string, window time.Duration, now time.Time) {
	if accountID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.at[accountID]
	if e == nil {
		e = &hotUse{}
		c.at[accountID] = e
	}
	if !e.last.IsZero() && !e.last.Add(window).After(now) {
		// 上一回太久了：这一回从头数。
		e.count = 0
	}
	e.count++
	e.last = now
	c.prune(window, now)
	c.trim()
}

// prune throws away the accounts whose use is out of the window.
//
// 调用的人拿着锁。
func (c *hotCounter) prune(window time.Duration, now time.Time) {
	for id, e := range c.at {
		if !e.last.Add(window).After(now) {
			delete(c.at, id)
		}
	}
}

// trim keeps the counter from growing without end.
//
// 调用的人拿着锁。
func (c *hotCounter) trim() {
	for len(c.at) > maxHotAccounts {
		oldest := ""
		var oldestAt time.Time
		for id, e := range c.at {
			if oldest == "" || e.last.Before(oldestAt) {
				oldest, oldestAt = id, e.last
			}
		}
		if oldest == "" {
			return
		}
		delete(c.at, oldest)
	}
}

// list gives the accounts that were used in the window, the most used
// first.
func (c *hotCounter) list(window time.Duration, now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(window, now)
	out := make([]string, 0, len(c.at))
	for id := range c.at {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := c.at[out[i]], c.at[out[j]]
		if a.count != b.count {
			return a.count > b.count
		}
		if !a.last.Equal(b.last) {
			return a.last.After(b.last)
		}
		// 同样的次数、同样的时候：按 id 分出来，结果才每次一样。
		return out[i] < out[j]
	})
	return out
}
```

- [ ] **Step 4: 管理器记着谁刚被用过**

`internal/manager/manager.go` 里加一个常量（放在 `accountTimeout` 后面）：

```go
// defaultHotWindow is how long an account counts as hot when nobody says.
//
// 五个分钟：刚用过一次的账号，它的额度最可能刚变。
const defaultHotWindow = 5 * time.Minute
```

`internal/manager/manager.go` 的 `Manager` 里加两个字段：

```go
	// hot counts the accounts that were used lately.
	hot *hotCounter
	// HotWindow is how long an account counts as hot after it was used.
	//
	// 它是配置说了算，所以它是公开的：网关起的时候会把它设成配置里的
	// 那个。没有人设它的时候是五分钟。
	//
	// 它是起来的时候写一次、之后只读的东西，所以没有锁：写它要在服务
	// 起来之前。
	HotWindow time.Duration
```

`New` 里加：

```go
		hot:       newHotCounter(),
		HotWindow: defaultHotWindow,
```

再加两个方法：

```go
// MarkUsed says an account was used.
//
// 聊天成功一次就算一次"用过"：热账号是"刚刚"的账，而这一笔就是刚刚。
func (m *Manager) MarkUsed(accountID string) {
	m.hot.mark(accountID, m.hotWindow(), time.Now())
}

// HotAccounts gives the accounts that were used lately, the most used
// first.
//
// 它给的是"刚刚"的那批账号：读额度的时候它们在热的那一组里。
func (m *Manager) HotAccounts() []string {
	return m.hot.list(m.hotWindow(), time.Now())
}

// hotWindow says how long an account counts as hot.
//
// 配 0 的时候按默认算：0 分钟的窗口等于没有热账号，那不是关掉，是坏了。
func (m *Manager) hotWindow() time.Duration {
	if m.HotWindow <= 0 {
		return defaultHotWindow
	}
	return m.HotWindow
}
```

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/manager/
```

- [ ] **Step 6: 提交**

```
git add internal/manager
git commit -m "feat(manager): 记着谁刚被用过"
```

---

### Task 3: 该读哪些账号

**Files:**
- Create: `internal/usage/round.go`
- Create: `internal/usage/round_test.go`
- Create: `internal/store/checkins_test.go`
- Modify: `internal/store/store.go`

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go` 是 `package store`（它用包内的 `s.db`），在这个包里再 import `mmxcode/internal/store` 是循环引用，编译不过。所以这条测试另开一个文件 `internal/store/checkins_test.go`，用 `package store_test`：

```go
package store_test

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// TestTheLatestCheckinsOfEveryAccount says each account's newest
// check-in is the one that is given.
//
// 两个签到可以是一天里的同一刻：日子分不开它们，id 分得开。
func TestTheLatestCheckinsOfEveryAccount(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: "2026-11-14", Outcome: "claimed",
		Credit: 10, Streak: 1, CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: "2026-11-15", Outcome: "claimed",
		Credit: 20, Streak: 2, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_2", Day: "2026-11-15", Outcome: "claimed",
		Credit: 30, Streak: 1, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	got, err := s.LatestCheckins(context.Background())
	if err != nil {
		t.Fatalf("latest checkins: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the check-ins = %d, want one per account", len(got))
	}
	for _, c := range got {
		if c.AccountID == "acc_1" && c.Credit != 20 {
			t.Errorf("acc_1's check-in = %v, want the newest one", c.Credit)
		}
	}
}

// newStore gives a store with the schema in it.
//
// The two accounts the check-in test uses are in it as well: a check-in
// belongs to an account, and the store refuses a check-in whose
// account is not there.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/mmxcode.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, id := range []string{"acc_1", "acc_2"} {
		now := time.Now()
		if err := s.CreateAccount(context.Background(), store.Account{
			ID: id, Name: id, Status: store.StatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	return s
}
```

`internal/usage/round_test.go`（新文件）：

```go
package usage

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// TestTheAccountsToReadAreTheOnesThatNeedIt says the read set is the
// accounts that need a reading, in the order they should be read.
//
// 该读的 = 从没读过的 ∪ 热的 ∪ 签到待补的 ∪ 标记到期的。没读过的排
// 在热的前面：它是一次性的活儿，排在后面就永远排在后面。
func TestTheAccountsToReadAreTheOnesThatNeedIt(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_old", now.Add(-time.Hour))
	mk(t, s, "acc_new", now.Add(-time.Minute))
	mk(t, s, "acc_never", time.Time{})
	mk(t, s, "acc_hot", now.Add(-time.Hour))
	mk(t, s, "acc_pending", now.Add(-time.Hour))
	mk(t, s, "acc_due", now.Add(-time.Hour))

	// 签到待补：签到比快照新，而且那次读没读到账本。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_pending", HasAmounts: false, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_pending", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	// 标记到期：标记的时刻到了，账号回到轮转里，但没人读过它。
	if err := s.SetExhausted(context.Background(), "acc_due", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}

	r := NewRound(s, nil, func() []string { return []string{"acc_hot"} }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	want := []string{"acc_pending", "acc_due", "acc_never", "acc_hot", "acc_old", "acc_new"}
	if len(got) != len(want) {
		t.Fatalf("the accounts to read = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the accounts to read = %v, want %v", got, want)
		}
	}
}

// TestTheAccountsToReadSkipTheSignedOut says a signed-out account is
// not read.
//
// 它读不到：读了也是白读。
func TestTheAccountsToReadSkipTheSignedOut(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_out", time.Time{})
	if err := s.UpdateAccountStatus(context.Background(), "acc_out",
		store.StatusSignedOut, "the site refused the token", now); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// newStore gives a store with the schema in it.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/mmxcode.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

// mk makes an account, with a reading of it or without one.
func mk(t *testing.T, s *store.Store, id string, readAt time.Time) {
	t.Helper()
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if readAt.IsZero() {
		return
	}
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: id, Granted: 100, Consumed: 10, Remaining: 90, HasAmounts: true,
		CreatedAt: readAt,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/store/ ./internal/usage/
```

失败：`s.LatestCheckins undefined`、`undefined: NewRound`。

- [ ] **Step 3: 库里加「每个账号最新的签到」**

`internal/store/store.go` 里加（放在 `ListCheckins` 后面）：

```go
// LatestCheckins gives each account's newest check-in.
//
// The newest is the row with the greatest id: two check-ins of one
// account can share a day, and the day alone would then name two rows.
// The id grows with every row written, so it tells them apart, and
// there is one row per account, always.
func (s *Store) LatestCheckins(ctx context.Context) ([]Checkin, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.account_id, c.day, c.outcome, c.credit, c.streak, c.message, c.created_at
		FROM checkins c
		JOIN (SELECT account_id, MAX(id) AS mid FROM checkins GROUP BY account_id)
			m ON m.account_id = c.account_id AND m.mid = c.id
		ORDER BY c.account_id`)
	if err != nil {
		return nil, fmt.Errorf("latest checkins: %w", err)
	}
	defer rows.Close()
	var out []Checkin
	for rows.Next() {
		var (
			c         Checkin
			credit    sql.NullFloat64
			streak    sql.NullInt64
			message   sql.NullString
			createdAt int64
		)
		if err := rows.Scan(&c.ID, &c.AccountID, &c.Day, &c.Outcome, &credit, &streak, &message, &createdAt); err != nil {
			return nil, fmt.Errorf("latest checkins: %w", err)
		}
		c.Credit = credit.Float64
		c.Streak = int(streak.Int64)
		c.Message = message.String
		c.CreatedAt = fromMS(createdAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest checkins: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 4: 写「该读哪些账号」**

创建 `internal/usage/round.go`：

```go
package usage

import (
	"context"
	"sort"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// Reader reads one account's credits.
//
// 它是一个接口，manager.Manager 正好是这个接口：这一趟读额度的活不
// 需要知道 manager 的别的本事。
type Reader interface {
	// RefreshUsage reads an account's credits and windows and saves them.
	RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error)
}

// Round reads the credits of the accounts that should be read.
//
// 一趟读额度有四道控：并发、每个账号的时限、一趟的预算、一趟最多读几
// 个。卡住的一个账号不能拖住整趟，一趟也不能读上一百个账号。
type Round struct {
	// Store is the database.
	Store *store.Store
	// Reader reads one account.
	Reader Reader
	// Hot gives the accounts that were used lately, the most used first.
	Hot func() []string
	// Cfg is the configuration.
	Cfg config.Config
}

// NewRound makes a round.
func NewRound(s *store.Store, r Reader, hot func() []string, cfg config.Config) *Round {
	return &Round{Store: s, Reader: r, Hot: hot, Cfg: cfg}
}

// The ranks the accounts are read in.
//
// 排在前面的先读：签到待补的卡上现在是错的，到期的马上要回轮转，从
// 没读过的排在热的前面（它是一次性的活儿，排在后面就永远排在后
// 面），热的刚变过，晚一轮没关系，其余的最久没读的先。
const (
	rankPending = iota
	rankDue
	rankNever
	rankHot
	rankCold
)

// target is one account to read, and where it stands in the order.
type target struct {
	// id is the account.
	id string
	// rank is the group it belongs to.
	rank int
	// order is what breaks a tie inside the rank.
	order int
}

// Targets gives the accounts to read now, in the order they should be
// read.
//
// 该读的 = 从没读过的 ∪ 热的 ∪ 签到待补的 ∪ 标记到期的。
func (r *Round) Targets(ctx context.Context) ([]string, error) {
	accounts, err := r.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := r.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	checkins, err := r.Store.LatestCheckins(ctx)
	if err != nil {
		return nil, err
	}
	exhausted, err := r.Store.ListExhausted(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	snapAt := map[string]time.Time{}
	snapHas := map[string]bool{}
	for _, s := range snaps {
		snapAt[s.AccountID] = s.CreatedAt
		snapHas[s.AccountID] = s.HasAmounts
	}
	checkAt := map[string]time.Time{}
	for _, c := range checkins {
		checkAt[c.AccountID] = c.CreatedAt
	}
	// 标记到期：标记的时刻到了，账号回到轮转里，但没有人读过它。
	due := map[string]bool{}
	for _, e := range exhausted {
		if e.Until.After(now) {
			continue
		}
		due[e.AccountID] = true
	}
	hotAt := map[string]int{}
	if r.Hot != nil {
		for i, id := range r.Hot() {
			hotAt[id] = i
		}
	}

	ts := make([]target, 0, len(accounts))
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			// 签出的账号读不到：读了也是白读。
			continue
		}
		read, hasSnap := snapAt[a.ID]
		checked := checkAt[a.ID]
		// 签到待补：签到过了，账本还没读到。
		//
		// 签到是把它放进这个名单的那一件事：签到给了积分，可账本还没
		// 读到（没读到，或者读到的那次失败了），卡上就还是旧的。
		pending := !checked.IsZero() &&
			(!hasSnap || checked.After(read) || !snapHas[a.ID])
		switch {
		case pending:
			ts = append(ts, target{id: a.ID, rank: rankPending})
		case due[a.ID]:
			ts = append(ts, target{id: a.ID, rank: rankDue})
		case !hasSnap:
			// 从没读过的：卡上没有它，而且它永远排在最后就永远读不到。
			ts = append(ts, target{id: a.ID, rank: rankNever})
		default:
			if at, ok := hotAt[a.ID]; ok {
				ts = append(ts, target{id: a.ID, rank: rankHot, order: at})
				continue
			}
			ts = append(ts, target{id: a.ID, rank: rankCold})
		}
	}

	// 其余的最久没读的先：越旧越可能过期。
	cold := []string{}
	for _, t := range ts {
		if t.rank == rankCold {
			cold = append(cold, t.id)
		}
	}
	sort.SliceStable(cold, func(i, j int) bool {
		return snapAt[cold[i]].Before(snapAt[cold[j]])
	})
	coldAt := map[string]int{}
	for i, id := range cold {
		coldAt[id] = i
	}
	for i := range ts {
		if ts[i].rank == rankCold {
			ts[i].order = coldAt[ts[i].id]
		}
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].rank != ts[j].rank {
			return ts[i].rank < ts[j].rank
		}
		return ts[i].order < ts[j].order
	})

	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.id)
	}
	return out, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/store/ ./internal/usage/
```

- [ ] **Step 6: 提交**

```
git add internal/store internal/usage
git commit -m "feat(usage): 该读哪些账号"
```

---

### Task 4: 一趟读额度

**Files:**
- Modify: `internal/usage/round.go`
- Modify: `internal/usage/round_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/usage/round_test.go` 里加：

```go
// fakeReader reads an account's credits without a network.
type fakeReader struct {
	// read are the accounts that were read.
	read []string
	// fail are the accounts whose reading fails.
	fail map[string]bool
	// wait is how long a reading takes.
	wait time.Duration
	// at is when the readings happened.
	at time.Time
	// mu guards read.
	mu sync.Mutex
}

// RefreshUsage reads an account's credits.
func (f *fakeReader) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	if f.wait > 0 {
		select {
		case <-time.After(f.wait):
		case <-ctx.Done():
			return store.UsageSnapshot{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.read = append(f.read, accountID)
	f.mu.Unlock()
	if f.fail[accountID] {
		return store.UsageSnapshot{}, errors.New("the ledger could not be read")
	}
	return store.UsageSnapshot{
		AccountID: accountID, Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: f.at,
	}, nil
}

// TestOneRoundReadsEachAccountOnce says the same account is not read
// twice in one round.
//
// 签到那一趟和热的那一趟可能撞上同一个账号。
func TestOneRoundReadsEachAccountOnce(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_1", "acc_2"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 2 {
		t.Fatalf("the readings = %d, want two", res.Refreshed)
	}
}

// TestOneRoundKeepsToItsBatch says a round reads at most so many.
//
// 剩下的下一趟接着读：不然一个账号多的网关一轮就把上游问死了。
func TestOneRoundKeepsToItsBatch(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2", "acc_3"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: time.Second, Batch: 2})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 2 {
		t.Fatalf("the readings = %d, want two", res.Refreshed)
	}
	if res.Skipped != 1 {
		t.Fatalf("the skipped = %d, want one", res.Skipped)
	}
}

// TestOneRoundKeepsToItsConcurrency says a round reads so many at once.
//
// 并发是读得快，不是读得多：上游不是只给我们用。
func TestOneRoundKeepsToItsConcurrency(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}, wait: 50 * time.Millisecond}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2", "acc_3", "acc_4"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: 5 * time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 4 {
		t.Fatalf("the readings = %d, want four", res.Refreshed)
	}
}

// TestOneRoundStopsWhenTheBudgetIsOut says the budget stops the round.
//
// 预算用完就停，剩下的下一趟接着读。
func TestOneRoundStopsWhenTheBudgetIsOut(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}, wait: time.Second}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2"},
		Options{Concurrency: 1, Timeout: time.Second, Budget: 100 * time.Millisecond, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 0 {
		t.Fatalf("the readings = %d, want none", res.Refreshed)
	}
	if res.Skipped != 1 {
		t.Fatalf("the skipped = %d, want the one the budget left out", res.Skipped)
	}
}

// TestAFailedReadingDoesNotStopTheOthers says one account that cannot
// be read does not stop the rest.
//
// 一个账号读不到不影响别的——但要说出来是哪一个，人才知道去看哪一行。
func TestAFailedReadingDoesNotStopTheOthers(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{"acc_1": true}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2"},
		Options{Concurrency: 2, Timeout: time.Second, Budget: time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 1 {
		t.Fatalf("the readings = %d, want one", res.Refreshed)
	}
	if res.Failed != 1 {
		t.Fatalf("the failed = %d, want one", res.Failed)
	}
	if len(res.Reasons) != 1 {
		t.Fatalf("the reasons = %v, want one", res.Reasons)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/usage/
```

失败：`undefined: Options`、`undefined: Run`。

- [ ] **Step 3: 写那一趟**

`internal/usage/round.go` 里加：

```go
// Options holds the controls of one round of readings.
type Options struct {
	// Concurrency is how many accounts are read at once.
	Concurrency int
	// Timeout is how long one account's reading may take.
	Timeout time.Duration
	// Budget is how long the whole round may take.
	Budget time.Duration
	// Batch is how many accounts the round reads at most.
	Batch int
}

// Result says what one round of readings came to.
type Result struct {
	// Read are the readings that came back.
	Read []store.UsageSnapshot
	// Refreshed counts the accounts whose credits were read.
	Refreshed int
	// Failed counts the accounts whose credits could not be read.
	Failed int
	// Skipped counts the accounts the round did not get to.
	Skipped int
	// Reasons says why, one line per account.
	Reasons []string
}

// Run reads the given accounts' credits, within a budget.
//
// 同一个账号同一时刻只读一次：一个账号在名单里出现两遍，读它一遍就
// 够了。读失败不挡别的账号。
func (r *Round) Run(ctx context.Context, ids []string, opts Options) (Result, error) {
	var out Result
	ids = dedupe(ids)
	if len(ids) == 0 {
		return out, nil
	}
	if opts.Batch > 0 && len(ids) > opts.Batch {
		out.Skipped = len(ids) - opts.Batch
		ids = ids[:opts.Batch]
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Budget)
		defer cancel()
	}
	sem := make(chan struct{}, opts.Concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			skipped := func() {
				mu.Lock()
				out.Skipped++
				out.Reasons = append(out.Reasons, id+": the budget is used up")
				mu.Unlock()
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				skipped()
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				// 抢到了位置，可是预算刚好在这时候用完了：这个账号没读
				// 成，算没轮上，不算读失败。
				//
				// 不查这一下就说不准：select 的两个都成了的时候，Go 随
				// 便挑一个，于是同一个账号有时算跳过，有时算失败。
				skipped()
				return
			}
			one := ctx
			if opts.Timeout > 0 {
				var cancel context.CancelFunc
				one, cancel = context.WithTimeout(ctx, opts.Timeout)
				defer cancel()
			}
			snap, err := r.Reader.RefreshUsage(one, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.Failed++
				out.Reasons = append(out.Reasons, id+": "+err.Error())
				return
			}
			out.Refreshed++
			out.Read = append(out.Read, snap)
		}(id)
	}
	wg.Wait()
	return out, nil
}

// RefreshDue reads the credits of the accounts that should be read.
func (r *Round) RefreshDue(ctx context.Context) (Result, error) {
	ids, err := r.Targets(ctx)
	if err != nil {
		return Result{}, err
	}
	return r.Run(ctx, ids, r.Options())
}

// RefreshHot reads the credits of the accounts that were used lately.
//
// 卡片那条路：只现读最热的几个，其余用上一次读到的数。卡片因此永远
// 几秒内回来。
func (r *Round) RefreshHot(ctx context.Context) (Result, error) {
	var ids []string
	if r.Hot != nil {
		ids = r.Hot()
	}
	return r.Run(ctx, ids, r.CardOptions())
}

// Options gives the controls of the periodic round.
func (r *Round) Options() Options {
	f := r.Cfg.Usage.Refresh
	return Options{
		Concurrency: f.Concurrency,
		Timeout:     f.Timeout.Duration(),
		Budget:      f.Budget.Duration(),
		Batch:       f.Batch,
	}
}

// CardOptions gives the controls of the card's own round.
//
// 卡片有自己的一份预算：它不能等。它读的那些账号的时限也是这份预算
// 说了算，不然一个卡住的账号会把卡片拖到超时。
func (r *Round) CardOptions() Options {
	f := r.Cfg.Usage.Refresh
	timeout := f.Timeout.Duration()
	if b := f.CardBudget.Duration(); b > 0 && (timeout <= 0 || b < timeout) {
		timeout = b
	}
	return Options{
		Concurrency: f.Concurrency,
		Timeout:     timeout,
		Budget:      f.CardBudget.Duration(),
		Batch:       f.Card,
	}
}

// dedupe takes the repeated ids out of a list, keeping the order.
func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
```

`internal/usage/round.go` 的 import 补上 `"sync"`。

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/usage/
```

- [ ] **Step 5: 提交**

```
git add internal/usage
git commit -m "feat(usage): 一趟读额度有并发有时限有预算"
```

---

### Task 5: 额度用完的标记

**Files:**
- Create: `internal/gateway/spent.go`
- Create: `internal/gateway/spent_test.go`
- Modify: `internal/gateway/gateway.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/gateway/spent_test.go`：

```go
package gateway

import (
	"testing"
	"time"
)

// TestAResetInThePastIsNoReset says a window that came back already is
// not a time to come back to.
//
// 过去的时刻照写不误有个坏处：isExhausted 看到过去的时刻就当"没耗
// 尽"，于是这个账号立刻又回到轮转里，客户端下一次还是撞上它。
func TestAResetInThePastIsNoReset(t *testing.T) {
	now := time.Now()
	if got := resetAfter(time.Now().Add(-time.Hour)); !got.IsZero() {
		t.Errorf("the reset = %s, want none", got)
	}
	if got := resetAfter(now.Add(time.Hour)); !got.After(now) {
		t.Errorf("the reset = %s, want a time in the future", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run Reset
```

失败：`undefined: resetAfter`。

- [ ] **Step 3: 写「什么时候回来」**

创建 `internal/gateway/spent.go`：

```go
package gateway

import (
	"context"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/store"
)

// resetAfter gives the moment a window comes back, when it is in the
// future.
//
// 过去的时刻当没有：拿一个已经过去的时间去说"它三点回来"，说的是一
// 个已经过去的时刻，而 isExhausted 看到过去的时刻就当没耗尽 —— 于是
// 这个账号立刻又回到轮转里，客户端下一次还是撞上它。没有就是没有，
// 让它走冷却时间。
func resetAfter(at time.Time) time.Time {
	if at.IsZero() || !at.After(time.Now()) {
		return time.Time{}
	}
	return at
}

// windowReset gives the moment a spent window comes back.
//
// The key is the plan group, not one window: the windows "5 hours" and
// "7 days" belong to a group, and the group comes back as a whole.
//
// A time that has passed is no time: the caller falls back to the
// cooldown.
func (s *Server) windowReset(ctx context.Context, accountID, groupKey string) time.Time {
	ws, err := s.Mgr.LatestWindows(ctx, accountID)
	if err != nil {
		return time.Time{}
	}
	return resetOf(ws, groupKey)
}

// resetOf gives the group's reset out of a list of windows.
func resetOf(ws []account.Window, groupKey string) time.Time {
	for _, w := range ws {
		if w.ModelName == groupKey && !w.ResetsAt.IsZero() {
			return resetAfter(w.ResetsAt)
		}
	}
	return time.Time{}
}

// resetOfSnapshot gives the group's reset out of a reading.
//
// 它是后台那一次读用的：读到的数才是真的，标记要照着它改。
func resetOfSnapshot(snap store.UsageSnapshot, groupKey string) time.Time {
	if snap.WindowsJSON == "" {
		return time.Time{}
	}
	var ws []account.Window
	if err := json.Unmarshal([]byte(snap.WindowsJSON), &ws); err != nil {
		return time.Time{}
	}
	return resetOf(ws, groupKey)
}

// markSpent marks an account's group as spent, and reads it once more
// in the background.
//
// 标记的那一刻就把"什么时候回来"定下来：读到的窗口时刻在将来就用
// 它，取不到就用冷却时间。后台那一次读是来改标记的：读到了，标记就
// 照着真的时刻改；读不到，标记留着（冷却）。
func (s *Server) markSpent(ctx context.Context, accountID, groupKey string) {
	until := s.windowReset(ctx, accountID, groupKey)
	_ = s.Rot.MarkWindowExhausted(ctx, accountID, groupKey, until, "quota")
	go s.refixSpent(accountID, groupKey)
}

// refixSpent reads the account and puts the mark's time right.
//
// 它不挡客户端：后台的活。
func (s *Server) refixSpent(accountID, groupKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snap, err := s.Mgr.RefreshUsage(ctx, accountID)
	if err != nil {
		// 读不到：标记留着（冷却）。
		return
	}
	until := resetOfSnapshot(snap, groupKey)
	_ = s.Rot.MarkWindowExhausted(ctx, accountID, groupKey, until, "quota")
}
```

`internal/gateway/spent.go` 的 import 补上 `"encoding/json"`。

- [ ] **Step 4: 聊天那条路用它**

`internal/gateway/gateway.go` 里：

把 `windowReset` 整个换掉（它搬到了 `spent.go`）：

```go
```

（删掉 `internal/gateway/gateway.go` 里原来的 `windowReset` 那个函数。）

`handleChat` 里 `rotator.ActionFailover` 那一段改成：

```go
			case rotator.ActionFailover:
				// 额度用完了：这个账号的窗口记成用完，换一个账号。
				s.markSpent(ctx, plan.AccountID, plan.GroupKey)
				lastErr = fmt.Errorf("the account's credits or plan are used up")
				quota = true
				quotaMsg = failure.Message
				continue accountLoop
```

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 6: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): 用完的标记照着读到的数改"
```

---

### Task 6: 签到之后读一次

**Files:**
- Modify: `internal/usage/round.go`
- Modify: `internal/usage/round_test.go`
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/webui/webui.go`

- [ ] **Step 1: 写失败的测试**

`internal/usage/round_test.go` 里加：

```go
// TestTheReadingSaysWhetherTheAccountHasRoom says a reading says
// whether the account has something to spend.
//
// 账本还有余额，或者计划窗口还有余量，都算还有。
func TestTheReadingSaysWhetherTheAccountHasRoom(t *testing.T) {
	if HasRoom(store.UsageSnapshot{HasAmounts: true, Remaining: 10}) != true {
		t.Error("an account with credits left has no room")
	}
	if HasRoom(store.UsageSnapshot{HasAmounts: true, Remaining: 0}) {
		t.Error("an account without credits has room")
	}
	if HasRoom(store.UsageSnapshot{HasAmounts: false}) {
		t.Error("an account that could not be read has room")
	}
	ws := `[{"name":"5 hours","modelName":"general","amount":10,"limit":100}]`
	if HasRoom(store.UsageSnapshot{WindowsJSON: ws}) != true {
		t.Error("a window with room left has no room")
	}
	full := `[{"name":"5 hours","modelName":"general","amount":100,"limit":100}]`
	if HasRoom(store.UsageSnapshot{WindowsJSON: full}) {
		t.Error("a window that is used up has room")
	}
}

// TestTheReadingClearsTheSpentMark says a reading that says there is
// something to spend takes the "used up" mark off.
//
// 签到给的积分能救活一个用完的账号。不处理的话它会一直待在外面的
// 轮转之外，直到窗口回来（最长七天）—— 白扔一个账号。
func TestTheReadingClearsTheSpentMark(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_1", now)
	if err := s.SetExhausted(context.Background(), "acc_1", "general", now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, &fakeReader{fail: map[string]bool{}}, nil, config.Default())
	if _, err := r.Settle(context.Background(), "acc_1"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	left, err := s.ListExhausted(context.Background())
	if err != nil {
		t.Fatalf("list exhausted: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("the spent marks = %v, want none", left)
	}
}

// TestAFailedReadingKeepsTheSpentMark says a reading that failed does
// not take the "used up" mark off.
//
// 宁可少用一个账号，也不要把一个用完的账号放回去让客户端撞它。
func TestAFailedReadingKeepsTheSpentMark(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_1", now)
	if err := s.SetExhausted(context.Background(), "acc_1", "general", now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, &fakeReader{fail: map[string]bool{"acc_1": true}}, nil, config.Default())
	if _, err := r.Settle(context.Background(), "acc_1"); err == nil {
		t.Fatal("settle said nothing about the failed reading")
	}
	left, err := s.ListExhausted(context.Background())
	if err != nil {
		t.Fatalf("list exhausted: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("the spent marks = %v, want the one", left)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/usage/
```

失败：`undefined: HasRoom`、`undefined: Settle`。

- [ ] **Step 3: 写「读到的数说话」**

`internal/usage/round.go` 里加：

```go
// HasRoom says whether a reading says the account has something to spend.
//
// 账本还有余额，或者计划窗口还有余量，都算还有。读不到（没有账本，
// 窗口也没有余量）就是没有：宁可少用一个账号，也不要把一个用完的
// 账号放回去让客户端撞它。
func HasRoom(snap store.UsageSnapshot) bool {
	if snap.HasAmounts && snap.Remaining > 0 {
		return true
	}
	for _, w := range windowsOf(snap) {
		if w.Unlimited {
			return true
		}
		if w.Limit > 0 && w.Amount < w.Limit {
			return true
		}
	}
	return false
}

// windowsOf reads the plan's windows out of a reading.
func windowsOf(snap store.UsageSnapshot) []account.Window {
	if snap.WindowsJSON == "" {
		return nil
	}
	var ws []account.Window
	if err := json.Unmarshal([]byte(snap.WindowsJSON), &ws); err != nil {
		return nil
	}
	return ws
}

// ClearSpent takes the "used up" marks off an account.
//
// 它是给签到用的：签到给的积分能救活一个用完的账号，而清不清由读到
// 的数决定。
func (r *Round) ClearSpent(ctx context.Context, accountID string) error {
	rows, err := r.Store.ListExhausted(ctx)
	if err != nil {
		return err
	}
	for _, e := range rows {
		if e.AccountID != accountID {
			continue
		}
		if err := r.Store.ClearExhausted(ctx, e.AccountID, e.WindowKey); err != nil {
			return err
		}
	}
	return nil
}

// Settle reads an account after a check-in and says whether it is spent.
//
// 它是签到那条路的一整套：读完，读到的数说有额度就清掉"用完"的标
// 记，读不到就留着（账号进"待补"名单，下一趟再读）。
func (r *Round) Settle(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	snap, err := r.Reader.RefreshUsage(ctx, accountID)
	if err != nil {
		return snap, err
	}
	if !HasRoom(snap) {
		return snap, nil
	}
	if err := r.ClearSpent(ctx, accountID); err != nil {
		return snap, err
	}
	return snap, nil
}
```

`internal/usage/round.go` 的 import 补上 `"encoding/json"` 和 `"mmxcode/internal/account"`。

- [ ] **Step 4: 调度器签完读一次**

`internal/scheduler/scheduler.go` 的 `CheckinAll` 改成：

```go
func (s *Scheduler) CheckinAll(ctx context.Context) []CheckinResult {
	accounts, err := s.Mgr.Accounts(ctx)
	if err != nil {
		return nil
	}
	out := make([]CheckinResult, 0, len(accounts))
	checked := []string{}
	for _, a := range accounts {
		r := CheckinResult{AccountID: a.ID}
		res, err := s.Mgr.Checkin(ctx, a.ID)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Outcome = res.Outcome
			r.Credit = res.Credit
			r.Streak = res.Streak
			checked = append(checked, a.ID)
		}
		out = append(out, r)
	}
	// 签到给的积分是账本池里的：不读额度，卡上的余额还是签到前的旧
	// 数。签到的账号凑成一批，一次有预算地读；读到的数说有额度就清
	// 掉"用完"的标记。
	s.readCheckedIn(ctx, checked)
	return out
}

// readCheckedIn reads the credits of the accounts that were checked in.
//
// 一趟读完不完都没关系：没读到的那些在"签到待补"名单里，下一趟接着
// 读（待补排在最前面，所以几分钟内就清完了）。
func (s *Scheduler) readCheckedIn(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	round := usage.NewRound(s.Mgr.Store, s.Mgr, s.Mgr.HotAccounts, s.Cfg)
	res, err := round.Run(ctx, ids, round.Options())
	if err != nil {
		fmt.Printf("checkin: the credits could not be read: %v\n", err)
		return
	}
	for _, reason := range res.Reasons {
		fmt.Printf("checkin: %s\n", reason)
	}
	// 读到的数说话：说有额度就清掉"用完"的标记。
	for _, snap := range res.Read {
		if !usage.HasRoom(snap) {
			continue
		}
		if err := round.ClearSpent(ctx, snap.AccountID); err != nil {
			fmt.Printf("checkin %s: the spent mark could not be taken off: %v\n", snap.AccountID, err)
		}
	}
}
```

`internal/scheduler/scheduler.go` 的 import 补上 `"mmxcode/internal/usage"`。

- [ ] **Step 5: 后台页面上那个签到按钮读完再答**

`internal/webui/webui.go` 的 `apiCheckin` 改成：

```go
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
```

`internal/webui/webui.go` 的 import 补上 `"mmxcode/internal/usage"`。

- [ ] **Step 6: 运行测试确认通过**

```
go test ./internal/usage/ ./internal/scheduler/ ./internal/webui/
```

- [ ] **Step 7: 提交**

```
git add internal/usage internal/scheduler internal/webui
git commit -m "feat(usage): 签到之后读一次，读到的数说话"
```

---

### Task 7: 每个账号同时一个请求

**Files:**
- Create: `internal/gateway/busy.go`
- Create: `internal/gateway/busy_test.go`
- Modify: `internal/gateway/gateway.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/gateway/busy_test.go`：

```go
package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/config"
)

// TestAnAccountTakesOneRequestAtATime says an account takes one
// request at a time.
//
// 两个请求落在一个账号上，额度掉得就快一倍：它们用的是同一份额度。
func TestAnAccountTakesOneRequestAtATime(t *testing.T) {
	b := newBusyAccounts(1)
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request")
	}
	if b.take("acc_1") {
		t.Fatal("the account took a second request")
	}
	b.give("acc_1")
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request again")
	}
}

// TestTheTotalConcurrencySaysHowMany says the total concurrency for a
// number of accounts.
//
// 总并发 = max(floor(可用账号数 / 5), min(可用账号数, 3))
func TestTheTotalConcurrencySaysHowMany(t *testing.T) {
	cfg := config.Default().Concurrency
	for _, c := range []struct{ usable, want int }{
		{1, 1}, {3, 3}, {5, 3}, {12, 3}, {100, 20},
	} {
		if got := totalFor(c.usable, cfg); got != c.want {
			t.Errorf("the total concurrency for %d accounts = %d, want %d",
				c.usable, got, c.want)
		}
	}
}

// TestTheGateLetsTheRequestsIn says the gate holds the requests that
// fit.
//
// 超了的请求排队等：等不到（客户端自己超时了）就 429 带 Retry-After。
func TestTheGateLetsTheRequestsIn(t *testing.T) {
	g := &gate{}
	if !g.take(2) {
		t.Fatal("the first request did not get in")
	}
	if !g.take(2) {
		t.Fatal("the second request did not get in")
	}
	if g.take(2) {
		t.Fatal("a third request got in under a limit of two")
	}
	g.give()
	if !g.take(2) {
		t.Fatal("the request did not get in after one was done")
	}
}

// TestTheGateWaitsForAPlace says a request waits for a free place.
//
// 只有 1 个账号的时候它忙，第二个请求要等它做完，而不是报错。
func TestTheGateWaitsForAPlace(t *testing.T) {
	g := &gate{}
	if !g.take(1) {
		t.Fatal("the first request did not get in")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		g.give()
	}()
	if !g.waitFor(context.Background(), 1, time.Second) {
		t.Fatal("the request did not wait for a place")
	}
	g.give()
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "Busy|Concurrency|Gate"
```

失败：`undefined: newBusyAccounts`。

- [ ] **Step 3: 写「谁在飞」**

创建 `internal/gateway/busy.go`：

```go
package gateway

import (
	"context"
	"sync"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// busyAccounts counts the requests in the air per account.
//
// 它是内存里的账：网关自己知道有几个请求在飞，不用问数据库。
type busyAccounts struct {
	mu sync.Mutex
	// per is how many requests one account takes at once.
	per int
	// at counts the requests in the air per account.
	at map[string]int
}

// newBusyAccounts makes the counter.
func newBusyAccounts(per int) *busyAccounts {
	if per <= 0 {
		per = 1
	}
	return &busyAccounts{per: per, at: map[string]int{}}
}

// take says a request takes an account, and whether it could.
//
// 抢不到就是抢不到：这个账号正在飞，第二个请求要等它做完，或者换一个
// 账号。
func (b *busyAccounts) take(accountID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at[accountID] >= b.per {
		return false
	}
	b.at[accountID]++
	return true
}

// give says a request is done with an account.
func (b *busyAccounts) give(accountID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at[accountID] <= 1 {
		delete(b.at, accountID)
		return
	}
	b.at[accountID]--
}

// gate counts the requests in the air and lets the ones that do not
// fit wait.
type gate struct {
	mu sync.Mutex
	// inFlight is how many requests are in the air.
	inFlight int
}

// take lets a request in when there is room under limit.
func (g *gate) take(limit int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit < 1 {
		limit = 1
	}
	if g.inFlight >= limit {
		return false
	}
	g.inFlight++
	return true
}

// give says a request is done.
func (g *gate) give() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight > 0 {
		g.inFlight--
	}
}

// waitFor waits until there is room under limit, or the wait is over.
//
// 等不到就是等不到：客户端自己超时了，网关不该把它吊着。
func (g *gate) waitFor(ctx context.Context, limit int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if g.take(limit) {
			return true
		}
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// totalFor gives the total concurrency for a number of usable accounts.
//
// 总并发 = max(floor(可用账号数 / 5), min(可用账号数, 3))
//
// 五分之一在用，四分之五留着：100 个账号同时只用 20 个，这 20 个把额
// 度用完了，还有 80 个是满的可以接。小规模的时候至少 min(账号数, 3)
// 个：5 个账号只跑 1 个太憋屈。
func totalFor(usable int, cfg config.Concurrency) int {
	ratio := cfg.TotalRatio
	if ratio < 1 {
		ratio = 1
	}
	floor := cfg.TotalMin
	if floor < 1 {
		floor = 1
	}
	if usable < floor {
		floor = usable
	}
	total := usable / ratio
	if total < floor {
		total = floor
	}
	if total < 1 {
		total = 1
	}
	return total
}

// totalConcurrency says how many requests may be in the air at once.
//
// 可用账号数 = 状态是 active（签入中）且没有被标记用完的账号数。账号
// 用完了不算：它现在也接不了活。
func (s *Server) totalConcurrency(ctx context.Context) int {
	if s.Store == nil {
		return 1
	}
	accounts, err := s.Store.ListAccounts(ctx)
	if err != nil {
		return 1
	}
	exhausted, err := s.Store.ListExhausted(ctx)
	if err != nil {
		return 1
	}
	now := time.Now()
	usable := 0
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			continue
		}
		if spentNow(exhausted, a.ID, now) {
			continue
		}
		usable++
	}
	return totalFor(usable, s.Cfg.Concurrency)
}

// spentNow says whether an account is marked as spent right now.
func spentNow(exhausted []store.Exhausted, accountID string, now time.Time) bool {
	for _, e := range exhausted {
		if e.AccountID != accountID {
			continue
		}
		if e.Until.After(now) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 网关拿上这两样**

`internal/gateway/gateway.go` 的 `Server` 里加：

```go
	// busy counts the requests in the air per account.
	busy *busyAccounts
	// gate counts the requests in the air in the whole gateway.
	gate *gate
```

`New` 里加：

```go
	g.busy = newBusyAccounts(cfg.Concurrency.PerAccount)
	g.gate = &gate{}
```

- [ ] **Step 5: 聊天那条路多两道门**

`internal/gateway/gateway.go` 的 `handleChat` 里：

`ctx := r.Context()` 之后加：

```go
	// 总并发：可用账号的五分之一，小规模至少三个。超了的请求等，等
	// 不到就 429 带 Retry-After —— 客户端自己超时了，网关不该把它吊
	// 着。
	if !s.gate.waitFor(ctx, s.totalConcurrency(ctx), s.Cfg.Concurrency.Wait.Duration()) {
		w.Header().Set("Retry-After", "5")
		writeError(w, proto, http.StatusTooManyRequests, "the gateway is busy: too many requests are in the air")
		return
	}
	defer s.gate.give()
```

账号那一圈里，`used[plan.AccountID] = true` 之后加：

```go
		if !s.busy.take(plan.AccountID) {
			// 这个账号正忙着：另一个请求在飞。换一个账号来答这一
			// 次，绑定不动 —— 忙不是它不能答了的理由。
			continue
		}
```

然后这一圈剩下的活儿要在一个闭包里做，闭包结束时把账号还回去。闭包返回 `true` 的意思是「请求到此为止，客户端已经拿到东西了」，返回 `false` 是「换一个账号再来」。

`internal/gateway/gateway.go` 的 `handleChat` 里，从 `used := map[string]bool{}` 到函数末尾，全部换成：

```go
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
		if !s.busy.take(plan.AccountID) {
			// 这个账号正忙着：另一个请求在飞。换一个账号来答这一
			// 次，绑定不动 —— 忙不是它不能答了的理由。
			continue
		}
		// 这个账号的活干完就把账号还回去：它在飞的时候不能接第二个
		// 请求。闭包是为了那个"还回去"一定发生 —— 无论这一圈是怎么
		// 结束的。
		answered := func() bool {
			defer s.busy.give(plan.AccountID)

			tok, err := s.Mgr.Token(ctx, plan.AccountID)
			if err != nil {
				lastErr = err
				return false
			}
			client, _, err := s.Mgr.Client(ctx, plan.AccountID)
			if err != nil {
				lastErr = err
				return false
			}

			upBody, upPath, convertBack, err := s.upstreamShape(body, proto)
			if err != nil {
				writeError(w, proto, http.StatusBadRequest, err.Error())
				return true
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
						return true
					}
					// 网络没通：还是这个账号，等一等再来。上游没有给出
					// 状态：这是网关自己的问题。
					lastErr = err
					lastStatus = 0
					lastMsg = ""
					if !sleepCtx(ctx, backoff(try)) {
						return true
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
								return true
							}
							lastErr = err
							lastStatus = 0
							lastMsg = ""
							if !sleepCtx(ctx, backoff(try)) {
								return true
							}
							continue
						}
					}
				}

				// 200 且是流：直接转出去
				if resp.StatusCode == http.StatusOK && raw == nil {
					// 答上了：这个账号刚刚被用过，它变热了。
					s.Mgr.MarkUsed(plan.AccountID)
					s.writeStream(w, resp, proto, convertBack, model)
					return true
				}

				// 200 但身体里有业务错误
				if resp.StatusCode == http.StatusOK {
					if err := upstream.CheckEnvelope("chat", raw); err == nil {
						// 答上了：这个账号刚刚被用过，它变热了。
						s.Mgr.MarkUsed(plan.AccountID)
						s.writeJSON(w, resp, raw, proto, convertBack, model)
						return true
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
					s.markSpent(ctx, plan.AccountID, plan.GroupKey)
					lastErr = fmt.Errorf("the account's credits or plan are used up")
					quota = true
					quotaMsg = failure.Message
					return false
				case rotator.ActionRenew:
					if renewed {
						// 续过一次还是被拒：这个账号先放着，换一个。
						lastErr = fmt.Errorf("the sign-in was refused (HTTP %d)", resp.StatusCode)
						quota = false
						quotaMsg = ""
						return false
					}
					newTok, rerr := s.Mgr.ForceRenew(ctx, plan.AccountID)
					if rerr != nil {
						// 跨站：不换号、不消耗 refresh token
						var oe *upstream.OtherSiteError
						// 续签自己的错要看 rerr：err 是上一次尝试的，
						// 那时候它是空的，看它永远看不出来。
						if errors.As(rerr, &oe) {
							writeError(w, proto, http.StatusBadGateway, oe.Error())
							return true
						}
						lastErr = rerr
						quota = false
						quotaMsg = ""
						return false
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
						return true
					}
					continue
				default:
					// 给不出别的说法：上游的状态原样给客户端。
					s.writeFailure(w, proto, false, "", resp.StatusCode, lastMsg, lastErr)
					return true
				}
			}
			return false
		}()
		if answered {
			return
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("the request could not be served")
	}
	s.writeFailure(w, proto, quota, quotaMsg, lastStatus, lastMsg, lastErr)
```

改这一段的时候有五处要一起改，漏了哪一个都编译不过或者行为变了：

| 原来 | 改成 | 为什么 |
|---|---|---|
| `accountLoop:` 这个标号 | 删掉 | 闭包里没有标号可跳；留着 Go 会说「这个标号没有用」。 |
| `continue accountLoop` | `return false` | 换一个账号再来：外层那一圈会再挑一个。 |
| `break accountLoop` | `return true` | 客户端走了：请求到此为止，不再往一个已经走了的客户端写东西。 |
| 写完答案之后的 `return` | `return true` | 客户端拿到东西了，请求到此为止。 |
| `lastErr = err` 之后的 `continue` | `return false` | 这个账号这一轮没答上，换一个。 |

- [ ] **Step 6: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 7: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): 一个账号同时一个请求"
```

---

### Task 8: 模型名单补上元数据

**Files:**
- Modify: `internal/gateway/models.go`
- Modify: `internal/gateway/models_test.go`
- Modify: `internal/gateway/gateway.go`

- [ ] **Step 1: 写失败的测试**

`internal/gateway/models_test.go` 里加：

```go
// TestTheModelListCarriesWhatMagpieNeeds says the list says what the
// models are like, and not only their names.
//
// magpie 要 context/output、能不能读图、思考档位，所以这些字段都
// 给上。别的客户端（OpenAI 形状的那些）会忽略多出来的字段。
func TestTheModelListCarriesWhatMagpieNeeds(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"whatever"}]}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	var body struct {
		Data []struct {
			ID        string `json:"id"`
			Limit     struct{ Context, Output int } `json:"limit"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Reasoning bool     `json:"reasoning"`
			Thinking  string   `json:"thinking"`
			Efforts   []string `json:"efforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if len(body.Data) == 0 {
		t.Fatalf("the list is empty: %s", w.Body.String())
	}
	m := body.Data[0]
	if m.Limit.Context == 0 || m.Limit.Output == 0 {
		t.Errorf("the limits = %+v, want them said", m.Limit)
	}
	if len(m.Modalities.Input) == 0 {
		t.Errorf("the input modalities = %v, want them said", m.Modalities.Input)
	}
	if !m.Reasoning {
		t.Errorf("the model does not reason: %+v", m)
	}
	if m.Thinking == "" {
		t.Errorf("the thinking is not said: %+v", m)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run Magpie
```

失败：名单里没有那些字段。

- [ ] **Step 3: 写那一行**

`internal/gateway/models.go` 里加：

```go
// modelEntry gives one model as the API tells it.
//
// magpie 还要 context/output、能不能读图、思考档位，所以这些字段都
// 给上：别的客户端（OpenAI 形状的那些）会忽略多出来的字段。
func modelEntry(m site.Model) map[string]any {
	inputs := m.Inputs
	if len(inputs) == 0 {
		inputs = []string{"text"}
	}
	efforts := m.Efforts
	if efforts == nil {
		efforts = []string{}
	}
	return map[string]any{
		"id": m.ID, "object": "model", "owned_by": "minimax",
		"limit": map[string]any{
			"context": m.Context,
			"output":  m.Output,
		},
		"modalities": map[string]any{
			"input":  inputs,
			"output": []string{"text"},
		},
		"reasoning": m.Thinking != site.ThinkingOff,
		"thinking":  string(m.Thinking),
		"efforts":   efforts,
	}
}
```

`internal/gateway/gateway.go` 的 `handleModels` 改成：

```go
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, m := range s.modelsFor(r.Context()) {
		data = append(data, modelEntry(m))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}
```

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 5: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): 模型名单带上元数据"
```

---

### Task 9: 卡片只现读最热的

**Files:**
- Modify: `internal/gateway/usage.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/gateway/gateway_test.go` 里加：

```go
// TestTheCardRefreshReadsOnlyTheHotAccounts says the card's refresh
// reads the hot accounts and no others.
//
// 卡片是 magpie 画的：它等不了。顺序读所有账号就是卡死的来源 —— 一
// 个账号要问三四个请求，N 个账号就是 N 倍。
func TestTheCardRefreshReadsOnlyTheHotAccounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 两个热账号。
	h.gw.Mgr.MarkUsed("acc_hot1")
	h.gw.Mgr.MarkUsed("acc_hot2")
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?refresh=1", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run Card
```

（这一条现在的样子是顺序读所有账号；改完之后 `RefreshAllUsage` 不再被卡片调用。）

- [ ] **Step 3: 卡片走热账号**

`internal/gateway/usage.go` 的 `usageCard` 改成：

```go
func (s *Server) usageCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.URL.Query().Get("refresh") == "1" && s.Mgr != nil {
		// 卡片那条路只现读最热的几个：卡片永远几秒内回来。其余的账
		// 用上一次读到的数。
		res, err := s.round().RefreshHot(ctx)
		if err != nil {
			// 读不到不是没有卡片：上一次读到的数照样摆出来，只是它是
			// 上一次的。理由记在日志里。
			log.Printf("usage card: the refresh failed: %v", err)
		} else {
			for _, reason := range res.Reasons {
				log.Printf("usage card: %s", reason)
			}
		}
	}
	snaps, err := s.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	accounts, err := s.Store.ListAccounts(ctx)
	if err != nil {
		writeError(w, protocolOf(r.URL.Path), http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(card.Of(snaps, accounts))
}
```

- [ ] **Step 4: 网关拿上一趟读额度的**

`internal/gateway/gateway.go` 里加：

```go
// round makes the reader of the credits.
//
// 它是网关和调度器共用的那一套：该读哪些账号、一趟读几个、并发几个。
func (s *Server) round() *usage.Round {
	return usage.NewRound(s.Store, s.Mgr, s.Mgr.HotAccounts, s.Cfg)
}
```

`New` 里加（把热窗口告诉管理器）：

```go
	// 多久算热是配置说了算：管理器记着谁刚被用过，这个窗口是它的。
	mgr.HotWindow = cfg.Usage.Refresh.HotWindow.Duration()
```

`internal/gateway/gateway.go` 的 import 补上 `"mmxcode/internal/usage"`。

- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 6: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): 卡片只现读最热的账号"
```

---

### Task 10: 调度器读该读的

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/scheduler/scheduler_test.go` 里加：

```go
// TestTheUsageRoundReadsTheAccountsThatNeedIt says the round reads the
// accounts that need a reading, and not every account.
//
// 账号多起来以后，冷账号的额度没有必要读：读了也是旧的。
func TestTheUsageRoundReadsTheAccountsThatNeedIt(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	for _, id := range []string{"acc_1", "acc_2"} {
		if err := s.CreateAccount(context.Background(), store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	// acc_1 刚被用过：它是热的。
	// acc_2 一次也没用过：它得读一次，不然卡上没有它。
	sc := &Scheduler{Mgr: ..., Cfg: config.Default()}
	...
}
```

（这一条要照着 `internal/scheduler/scheduler_test.go` 里现有的那套测试环境写：那里已经有 `newStore` 和一个替身的 manager。）

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/scheduler/
```

- [ ] **Step 3: 调度器读该读的**

`internal/scheduler/scheduler.go` 的 `RefreshUsage` 改成：

```go
// RefreshUsage reads the credits of the accounts that should be read.
//
// 它不再是"把所有账号读一遍"：账号多起来以后，冷账号的额度没有必要
// 读 —— 读了也是旧的。要读的是那些该读的：签到待补的、标记到期的、
// 从没读过的、还有热的。
func (s *Scheduler) RefreshUsage(ctx context.Context) {
	res, err := s.round().RefreshDue(ctx)
	if err != nil {
		fmt.Printf("usage: the round failed: %v\n", err)
		return
	}
	for _, reason := range res.Reasons {
		fmt.Printf("usage: %s\n", reason)
	}
}
```

再加：

```go
// round makes the reader of the credits.
func (s *Scheduler) round() *usage.Round {
	return usage.NewRound(s.Mgr.Store, s.Mgr, s.Mgr.HotAccounts, s.Cfg)
}
```

`Run` 里那一趟的钟改成新的那个：

```go
	usageEvery := s.Cfg.Usage.Refresh.ScanEvery.Duration()
	if usageEvery <= 0 {
		usageEvery = s.Cfg.Schedule.UsageRefresh.Duration()
	}
	if usageEvery <= 0 {
		usageEvery = 15 * time.Minute
	}
```

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/scheduler/
```

- [ ] **Step 5: 提交**

```
git add internal/scheduler
git commit -m "feat(scheduler): 一趟读额度读的是该读的账号"
```

---

### Task 11: 文档

**Files:**
- Modify: `README.md`
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`

- [ ] **Step 1: README 写清楚这几件事**

README 里加一节「读额度」：

- 额度只读该读的账号：签到待补的、标记到期的、从没读过的、还有热的。
- 一趟读的控：并发、每个账号的时限、一趟的预算、一趟最多读几个（配在 `usage.refresh` 下面）。
- 多久算热（`usage.refresh.hot_window`）。
- 额度用完的账号：立刻标记，后台读一次把标记改准；签到给了积分就读一次，读到的数说有额度就清掉标记。
- 并发：每个账号同时一个请求（`concurrency.per_account`），总并发是可用账号的五分之一（`concurrency.total_ratio` / `concurrency.total_min`）。

- [ ] **Step 2: 两份配置样例补上这两节**

`config.example.yaml` 和 `config.docker.yaml` 现在把每一节都写着（`server`、`proxy`、`upstream`、`rotation`、`schedule`、`batch`），新加的两节也要写上，不然看样例的人不知道有这些旋钮：

```yaml
usage:
  refresh:
    # 一个账号用过之后多久还算热（热账号的额度才值得现读）
    hot_window: 5m
    # 多久扫一遍该读额度的账号
    scan_every: 30s
    # 一趟同时读几个账号的额度
    concurrency: 4
    # 一个账号的额度最多读多久（卡住的账号不拖住整趟）
    timeout: 15s
    # 一趟读额度最多花多久（用完就停，剩下的下一趟接着读）
    budget: 30s
    # 一趟最多读几个账号
    batch: 6
    # 卡片现读读几个账号（卡片等不了）
    card: 2
    # 卡片现读最多花多久
    card_budget: 5s

concurrency:
  # 每个账号同时几个请求
  per_account: 1
  # 总并发是可用账号数的几分之一（5 = 五分之一）
  total_ratio: 5
  # 账号少的时候总并发至少几个
  total_min: 3
  # 请求等一个空位最多等多久（等不到就 429）
  wait: 10s
```

两份文件里都加在 `batch:` 那节后面、`db:` 那行前面。

- [ ] **Step 3: 提交**

```
git add README.md config.example.yaml config.docker.yaml
git commit -m "docs(readme): 读额度只碰该碰的账号"
```

---

## 验证

```
gofmt -l .
go build ./...
go vet ./...
go test ./...
python -I docs/superpowers/tools/check_plan_files.py docs/superpowers/plans/2026-11-15-mmxcode-hot-usage.md .
python -I docs/superpowers/tools/check_plan_fences.py docs/superpowers/plans/2026-11-15-mmxcode-hot-usage.md
```

本机（不起 docker）：

```
MMXCODE_API_KEYS=sk-admin ./mmxcode.exe --config config.yaml --db ./mmxcode.db serve
```

- `GET /v1/models` 带上了 limit / modalities / reasoning / thinking / efforts。
- `GET /v1/usage?refresh=1` 几秒内回来（不卡）。
- 多账号时，聊天两个并发请求落在不同的账号上（一个账号在飞的时候另一个换号）。
