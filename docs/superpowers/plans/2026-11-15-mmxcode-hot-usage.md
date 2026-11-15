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

该读的是四类账号：签到待补的、标记到期的、从没读过的、热的。其余的（冷的）不读：它的数是旧的，读它只是把旧的数再读一遍。它自己会醒过来 —— 旧的数是高估（积分只会往下走），轮转会挑它，它被用到就变热，热了才进名单。

排在前面的先读：签到待补 → 标记到期 → 从没读过的 → 热。

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
	// 同一天再签一次是改那一行，不是插第二行：id 不变，时刻往前走。
	// 于是"id 最大的那条"不是最新的那条。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: "2026-11-14", Outcome: "claimed",
		Credit: 40, Streak: 1, CreatedAt: now.Add(time.Minute),
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
	byAccount := map[string]store.Checkin{}
	for _, c := range got {
		byAccount[c.AccountID] = c
	}
	if byAccount["acc_1"].Credit != 40 {
		t.Errorf("acc_1's check-in = %v, want the newest one", byAccount["acc_1"])
	}
	if byAccount["acc_2"].Credit != 30 {
		t.Errorf("acc_2's check-in = %v, want its own", byAccount["acc_2"])
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
// 在热的前面：它是一次性的活儿，排在后面就永远排在后面。冷的账号不
// 在名单里：它的数是旧的，读它也是把旧的数再读一遍。
func TestTheAccountsToReadAreTheOnesThatNeedIt(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_cold", now.Add(-time.Minute))
	mk(t, s, "acc_never", time.Time{})
	mk(t, s, "acc_hot", now.Add(-time.Hour))
	mk(t, s, "acc_pending", now.Add(-time.Hour))
	mk(t, s, "acc_due", now.Add(-time.Hour))

	// 签到待补：签到比快照新，卡上还是签到前的旧数。
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
	want := []string{"acc_pending", "acc_due", "acc_never", "acc_hot"}
	if len(got) != len(want) {
		t.Fatalf("the accounts to read = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the accounts to read = %v, want %v", got, want)
		}
	}
}

// TestTheColdAccountsAreLeftAlone says a cold account is not read.
//
// 冷账号的数是旧的，读它也是把旧的数再读一遍。它自己会醒过来：旧的
// 数是高估（积分只会往下走），轮转会挑它，它就热了，热了就该读。
func TestTheColdAccountsAreLeftAlone(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_cold", now.Add(-time.Minute))
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheDueAccountIsReadOnceAfterItsMarkExpires says a spent mark
// that has expired is read once, and then the account is left alone.
//
// 标记自然过期 → 到期读一次 → 数变新，账号回轮转（规格 §4.5 的恢复
// 点 A）。一次就好：读过的账号不到期了，不然六个到期的账号就把一趟
// 的批次占满，从来没读过的账号永远读不到。
func TestTheDueAccountIsReadOnceAfterItsMarkExpires(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_due", now.Add(-time.Hour))
	if err := s.SetExhausted(context.Background(), "acc_due", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_due" {
		t.Fatalf("the accounts to read = %v, want the due one", got)
	}
	// 读过了：数变新了，它不到期了。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_due", Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	got, err = r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheAccountThatIsStillSpentIsNotDue says an account that is
// still spent for one group is not read.
//
// 它还有一个组欠着额度：读它只是把用完的那一组再读一遍。标记全过期
// 了它才轮到——没过期的那一个说明它还欠着，回不了轮转。
func TestTheAccountThatIsStillSpentIsNotDue(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_spent", now.Add(-time.Hour))
	mk(t, s, "acc_waiting", now.Add(-time.Hour))
	// 一个过期了，另一个还没有。
	if err := s.SetExhausted(context.Background(), "acc_spent", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	if err := s.SetExhausted(context.Background(), "acc_spent", "other",
		now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	// 标记还没到：它还欠着，不该读。
	if err := s.SetExhausted(context.Background(), "acc_waiting", "general",
		now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
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

// TestTheFailedReadingIsNotRetriedForever says a reading that failed is
// tried again for a while, and then the account is left alone.
//
// 读失败的账号要再读（签到给的积分还在账本里等着），可是不能一直重
// 试：一个永远读不回来的账号就占着待补那一位，一趟的批次全被它拿
// 走，从来没读过的账号再也读不到——而每个账号至少读一次是硬要求。
func TestTheFailedReadingIsNotRetriedForever(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_failed", now.Add(-time.Hour))
	// 那次读失败了：没有数。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_failed", HasAmounts: false, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	// 签到不久之前：还再读一遍。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_failed", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_failed" {
		t.Fatalf("the accounts to read = %v, want the failed one", got)
	}
	// 十一分钟以后：不再重读了，把它放下。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_failed", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-11 * time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	got, err = r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheCheckInThatIsNotOnTheCardIsStillPending says a check-in that
// is newer than the reading is pending, whatever the retry window says.
//
// 签到给的积分卡上还没有，那是要补的：规格 §5.3 的那条规则跟重试的
// 期限是两回事。
func TestTheCheckInThatIsNotOnTheCardIsStillPending(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_checked", now.Add(-time.Hour))
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_checked", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-11 * time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_checked" {
		t.Fatalf("the accounts to read = %v, want the pending one", got)
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
// The newest is the one written last: a day is unique for an account,
// but the same day's row is updated in place — its id stays the one it
// was given and only its time moves. So the id does not say which
// check-in is the latest; the time does. Ties go to the greater id.
func (s *Store) LatestCheckins(ctx context.Context) ([]Checkin, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.account_id, c.day, c.outcome, c.credit, c.streak, c.message, c.created_at
		FROM checkins c
		WHERE c.id = (
			SELECT c2.id FROM checkins c2
			WHERE c2.account_id = c.account_id
			ORDER BY c2.created_at DESC, c2.id DESC
			LIMIT 1
		)
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

名单里的四类各是一档（`rankPending` / `rankDue` / `rankNever` / `rankHot`）。排序是一个全序：先看档，档里按它自己的次序（等得最久的先，热的按用得多的先），最后按 id —— 每次跑出来的名单都是同一个。

三件事在名单里是要算出来的：

- **签到待补**：签到比这个账号最新的快照还新（规格 §5.3）；或者那次读失败了（快照里没有数）—— 后者只重试 `retryWindow`（十分钟）：不限次数地重试，一个永远读不回来的账号就把整趟的批次占住了，从来没读过的账号再也读不到。
- **标记到期**：标记过了时刻，而且这个账号没有还没过时刻的标记（那个组还欠着，回不了轮转），而且标记过期之后没人读过它（最新的快照比标记的时刻还旧）。到期读一次就好：读过了，数变新，账号回轮转（规格 §4.5 的恢复点 A）。
- **从没读过的**：没有快照。**每个账号至少读一次**是硬要求（规格 §3.5），它必须排在热的前面。

冷账号不在名单里。

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
// 面），热的刚变过，晚一轮没关系。
const (
	rankPending = iota
	rankDue
	rankNever
	rankHot
)

// retryWindow is how long a reading that failed is tried again.
//
// 读失败的账号要再读一遍（签到给的积分还在账本里等着），可是不能一
// 直重试：一个永远读不回来的账号（账本的批次没有期限、上游在挂）
// 会一直占着待补那一位，一趟的批次全被它拿走，从来没读过的账号就再
// 也读不到——而每个账号至少读一次是硬要求。
//
// 所以重试是有期限的：失败之后的一段时间里还再读（上游多半是暂时
// 的），过了这段时间就把这个账号放下，让别的账号先读。
//
// 它是一个常数，不是一个配置项：配置的键是规格定死了的。
const retryWindow = 10 * time.Minute

// target is one account to read, and where it stands in the order.
type target struct {
	// id is the account.
	id string
	// rank is the group it belongs to.
	rank int
	// order is what breaks a tie inside the rank.
	order int
	// since is when it began to wait, for the ranks that wait.
	since time.Time
}

// Targets gives the accounts to read now, in the order they should be
// read.
//
// 该读的就是那四类：签到待补的、标记到期的、从没读过的、热的。冷账号
// 不在名单里：它的数是旧的，读它只是把旧的数再读一遍——它被用到的时候
// 自己会醒过来（旧的数是高估，轮转会挑它），那时候它就热了。
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
	// 标记到期：标记过了时刻，账号该回轮转里读一次了。
	//
	// 到期读一次就好：读过了（数变新了）它就不再到期。还有一个标记没
	// 过时刻的，那个组还欠着额度，账号回不了轮转。
	expiredAt := map[string]time.Time{}
	stillSpent := map[string]bool{}
	for _, e := range exhausted {
		if e.Until.After(now) {
			stillSpent[e.AccountID] = true
			continue
		}
		if at, ok := expiredAt[e.AccountID]; !ok || e.Until.After(at) {
			expiredAt[e.AccountID] = e.Until
		}
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
		// 读到，卡上就还是旧的。
		//
		// 读到的那次失败了也要再读，可是只再读一会儿（retryWindow）：
		// 不限次数地重试，一个一直读不回来的账号就把整趟的批次占住
		// 了，从来没读过的账号再也读不到。
		pending := false
		if !checked.IsZero() {
			pending = !hasSnap || checked.After(read)
			if !pending && !snapHas[a.ID] && now.Sub(checked) < retryWindow {
				pending = true
			}
		}
		// 标记到期：标记过了时刻，而且没人读过它。
		//
		// 读过了就不到期了：到期读一次，数变新，账号回轮转。
		due := false
		if until, ok := expiredAt[a.ID]; ok && !stillSpent[a.ID] {
			due = !hasSnap || read.Before(until)
		}
		switch {
		case pending:
			ts = append(ts, target{id: a.ID, rank: rankPending, since: checked})
		case due:
			ts = append(ts, target{id: a.ID, rank: rankDue, since: expiredAt[a.ID]})
		case !hasSnap:
			// 从没读过的：卡上没有它，而且它永远排在最后就永远读不到。
			ts = append(ts, target{id: a.ID, rank: rankNever})
		default:
			if at, ok := hotAt[a.ID]; ok {
				ts = append(ts, target{id: a.ID, rank: rankHot, order: at})
			}
			// 冷的：不读。它的数是旧的，被用到的时候它自己会醒过来。
		}
	}

	// 排在前面的先读。同一组里：等得最久的先（签到待补的按签到的时
	// 刻，标记到期的按到期的时刻），热的按用得多的先，其余按 id 分出
	// 来——结果每次一样。
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].rank != ts[j].rank {
			return ts[i].rank < ts[j].rank
		}
		switch ts[i].rank {
		case rankPending, rankDue:
			if !ts[i].since.Equal(ts[j].since) {
				return ts[i].since.Before(ts[j].since)
			}
		case rankHot:
			if ts[i].order != ts[j].order {
				return ts[i].order < ts[j].order
			}
		}
		return ts[i].id < ts[j].id
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

// TestABatchOfZeroMeansOne says a batch of zero reads one account.
//
// 配 0 的时候按 1 算：0 个一批是一趟也读不了，那不是关掉，是坏了。
func TestABatchOfZeroMeansOne(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2"},
		Options{Concurrency: 1, Timeout: time.Second, Budget: time.Second, Batch: 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 1 {
		t.Fatalf("the readings = %d, want one", res.Refreshed)
	}
	if res.Skipped != 1 {
		t.Fatalf("the skipped = %d, want the one the batch left out", res.Skipped)
	}
}

// TestTheBatchOfZeroIsOneInTheControls says the two rounds' controls
// say one account when the config says zero.
//
// 卡片那一趟也是：card: 0 不该是"全都读"，该是"读一个"。
func TestTheBatchOfZeroIsOneInTheControls(t *testing.T) {
	cfg := config.Default()
	cfg.Usage.Refresh.Batch = 0
	cfg.Usage.Refresh.Card = 0
	r := NewRound(nil, nil, nil, cfg)
	if got := r.Options().Batch; got != 1 {
		t.Errorf("the periodic batch = %d, want one", got)
	}
	if got := r.CardOptions().Batch; got != 1 {
		t.Errorf("the card's batch = %d, want one", got)
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

批次配 0 的时候按 1 算：0 个一批是一趟也读不了，那不是关掉，是坏了（跟 0 并发一个道理）。卡片那一趟也一样：`card: 0` 是「读一个」，不是「全都读」。

读一个账号之前先问一句「有人正在读它吗」：正在读的那一个不读（规格 §3.3 的单账号去重，Task 13 加上）。撞上的那个账号不是读失败，也不是没轮上，所以不算也不记一笔理由。

```go
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
	opts.Batch = batch(opts.Batch)
	if len(ids) > opts.Batch {
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
	// guard 是读者跟别的那一趟之间的那把锁：没有它，就跟从前一样。
	guard, hasGuard := r.Reader.(ReadingGuard)
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
			if hasGuard && !guard.BeginReading(id) {
				// 别的这一趟正在读这个账号：这一趟不读它。读的是同
				// 一次，读两遍是白读（规格 §3.3）。
				//
				// 这不是失败，也不是没轮上：它很正常，所以不算，也不
				// 记一笔理由。
				return
			}
			if hasGuard {
				// 读完了（读失败也是读完了）就把账号还回去。
				defer guard.EndReading(id)
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

标记是「这个账号的这个组用完了，客户端别再撞它」。它得说清楚什么时候回来：说准了，客户端等得到；说不准，一个还欠着额度的账号就被提前放回轮转里。

标记只往后挪：读到的时刻比现在的标记晚，标记就挪到那儿；读不到，或者读到的东西里没有这个组，标记原样留着。一个零写进去，冷却时间（五分钟）就会把标记缩短——那正是提前放行。

**Files:**
- Create: `internal/gateway/spent.go`
- Create: `internal/gateway/spent_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`
- Modify: `cmd/mmxcode/serve.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/gateway/spent_test.go`：

```go
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/store"
)

// fakeReader reads the credits without the network.
//
// 网关读额度只有一个口子：Reader 那个字段。测试把它换成这个，测试就
// 永远不碰 MiniMax 自己。
type fakeReader struct {
	mu sync.Mutex
	// snap is what a reading gives back.
	snap store.UsageSnapshot
	// err is what a reading fails with.
	err error
	// reads counts the readings that were asked for.
	reads int
	// asked are the accounts the readings were asked for.
	asked []string
}

// set gives the reader what it should say.
func (f *fakeReader) set(snap store.UsageSnapshot, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	f.err = err
}

// RefreshUsage reads the credits from the test's own answers.
func (f *fakeReader) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.asked = append(f.asked, accountID)
	if f.err != nil {
		return store.UsageSnapshot{}, f.err
	}
	return f.snap, nil
}

// count says how many readings were asked for.
func (f *fakeReader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// askedFor says which accounts the readings were asked for.
//
// 它记下问的是谁：光数次数说不清是哪些账号被问过。
func (f *fakeReader) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// readingOf makes a reading that says the windows it has.
func readingOf(t *testing.T, ws []account.Window) store.UsageSnapshot {
	t.Helper()
	raw, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}
	return store.UsageSnapshot{WindowsJSON: string(raw)}
}

// markUntil gives the moment the account's group is marked spent until.
//
// 库里那一行说了算：标记现在到什么时候，是写下去的那一份。
func markUntil(t *testing.T, s *store.Store, accountID, groupKey string) time.Time {
	t.Helper()
	rows, err := s.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.AccountID == accountID && e.WindowKey == groupKey {
			return e.Until
		}
	}
	return time.Time{}
}

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

// TestTheGroupComesBackWithItsLatestWindow says a group comes back as a
// whole: its moment is the latest of the windows it still has to come
// back from.
//
// 一个组是好几个窗口（"5 hours"、"7 days"），组是整个回来的：它最后
// 那个窗口回来了，组才回来。已经过去的那个窗口不算：它不能盖住一个
// 还在将来的窗口。
func TestTheGroupComesBackWithItsLatestWindow(t *testing.T) {
	now := time.Now()
	// 一个过去的，一个将来的：将来的是那个时刻。
	ws := []account.Window{
		{ModelName: "general", ResetsAt: now.Add(-time.Hour)},
		{ModelName: "general", ResetsAt: now.Add(time.Hour)},
	}
	if got := resetOf(ws, "general"); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("the group's reset = %s, want %s", got, now.Add(time.Hour))
	}
	// 两个将来的：晚的那个是那个时刻。
	ws = []account.Window{
		{ModelName: "general", ResetsAt: now.Add(time.Hour)},
		{ModelName: "general", ResetsAt: now.Add(3 * time.Hour)},
	}
	if got := resetOf(ws, "general"); !got.Equal(now.Add(3 * time.Hour)) {
		t.Errorf("the group's reset = %s, want %s", got, now.Add(3*time.Hour))
	}
	// 组的键比的时候不看大小写：站上写的是 "General"，这边说的是
	// "general"，说的是同一个组。
	ws = []account.Window{{ModelName: "General", ResetsAt: now.Add(2 * time.Hour)}}
	if got := resetOf(ws, "general"); !got.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("the group's reset = %s, want %s", got, now.Add(2*time.Hour))
	}
}

// TestTheBackgroundReadingDoesNotShortenTheMark says a reading that
// does not mention the group leaves the mark as it is.
//
// 标记是三小时以后：读到的数里没有这个组，那就没有。没有就是没有，写
// 一个零进去，冷却时间（五分钟）就会把标记缩短，一个还欠着额度的账号
// 就提前回到轮转里 —— 提前放行正是这个标记要防的事。
func TestTheBackgroundReadingDoesNotShortenTheMark(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	until := time.Now().Add(3 * time.Hour)
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	// 这一次读到的东西里没有这个组。
	h.reader.set(readingOf(t, []account.Window{{ModelName: "video", ResetsAt: time.Now().Add(time.Hour)}}), nil)
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != until.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, until)
	}
}

// TestTheBackgroundReadingDoesNotShortenTheMarkWithAnEarlierMoment says
// a reading that says an earlier moment leaves the mark as it is.
//
// 标记是三小时以后，读到的时刻是一小时以后：标记不动。只往后挪。
func TestTheBackgroundReadingDoesNotShortenTheMarkWithAnEarlierMoment(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	until := time.Now().Add(3 * time.Hour)
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	h.reader.set(readingOf(t, []account.Window{{ModelName: "general", ResetsAt: time.Now().Add(time.Hour)}}), nil)
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != until.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, until)
	}
}

// TestTheBackgroundReadingMovesTheMarkLater says a reading that says a
// later moment moves the mark there.
//
// 标记是三小时以后，读到的时刻是五小时以后：标记挪到五小时以后。一个
// 滚动的窗口它的时刻本来就是越来越晚的，所以平常的时候这就是"照着真
// 的时刻改"。
func TestTheBackgroundReadingMovesTheMarkLater(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", time.Now().Add(3*time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(5 * time.Hour)
	h.reader.set(readingOf(t, []account.Window{{ModelName: "general", ResetsAt: later}}), nil)
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != later.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, later)
	}
}

// TestTheBackgroundReadingThatFailsLeavesTheMarkAlone says a reading
// that fails leaves the mark as it is.
//
// 读不到就不知道什么时候回来：不知道就不动。动一下就有可能提前放行。
func TestTheBackgroundReadingThatFailsLeavesTheMarkAlone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	until := time.Now().Add(3 * time.Hour)
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	h.reader.set(store.UsageSnapshot{}, errors.New("the site is not there"))
	h.gw.correctMark("acc_01", "general")
	got := markUntil(t, h.store, "acc_01", "general")
	if got.UnixMilli() != until.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, until)
	}
}

// TestTheBackgroundReadingGoesThroughTheReader says the background
// reading goes through the injected reader and not to the network.
//
// 网关读额度只有一个口子：Reader 那个字段。这个账号没有令牌，所以管
// 理器那条路读不到它 —— 读额度就只可能走那个口子。
func TestTheBackgroundReadingGoesThroughTheReader(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	now := time.Now()
	h.store.CreateAccount(ctx, store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, UserID: "0987654321",
		CreatedAt: now, UpdatedAt: now,
	})
	if err := h.gw.Rot.MarkWindowExhausted(ctx, "acc_02", "general", time.Now().Add(3*time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(5 * time.Hour)
	h.reader.set(readingOf(t, []account.Window{{ModelName: "general", ResetsAt: later}}), nil)
	h.gw.correctMark("acc_02", "general")
	if n := h.reader.count(); n != 1 {
		t.Errorf("the reader was asked %d times, want 1", n)
	}
	got := markUntil(t, h.store, "acc_02", "general")
	if got.UnixMilli() != later.UnixMilli() {
		t.Errorf("the mark = %s, want %s", got, later)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run 'TestTheGroupComesBackWithItsLatestWindow|TestTheBackgroundReading'
```

失败：`undefined: resetOf`、`undefined: correctMark`。

- [ ] **Step 3: 写「什么时候回来」**

创建 `internal/gateway/spent.go`：

```go
package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/store"
)

// The timeouts of the background work.
//
// 读和写各有一份：读的那一份用完了，不该把写也一起拖掉。
const (
	// spentReadTimeout is how long the background reading may take.
	spentReadTimeout = 30 * time.Second
	// spentWriteTimeout is how long the background writing may take.
	spentWriteTimeout = 5 * time.Second
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
//
// 一个组是好几个窗口（"5 hours"、"7 days"），组是整个回来的：它最后
// 那个窗口回来了，组才回来。所以组的时刻取它那些窗口里最晚的一个，
// 而且只算将来的 —— 已经过去的那个不算：它不能盖住一个还在将来的窗
// 口。
//
// 组的键比的时候不看大小写：站上写的是 "General"，这边说的是
// "general"，说的是同一个组。
func resetOf(ws []account.Window, groupKey string) time.Time {
	var latest time.Time
	for _, w := range ws {
		if !strings.EqualFold(w.ModelName, groupKey) {
			continue
		}
		at := resetAfter(w.ResetsAt)
		if at.After(latest) {
			latest = at
		}
	}
	return latest
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
// 它，取不到就用冷却时间。后台那一次读是来改标记的，而且只往后改：
// 读到的时刻比现在的标记晚，标记就挪到那儿；读不到，或者读到的东西
// 里没有这个组，标记就原样留着。
//
// 提前放一个用完的账号回轮转，正是这个标记要防的事。而一个滚动的窗
// 口，它的时刻本来也只是越来越晚：所以"只往后改"平常的时候就是"照
// 着真的时刻改"。
func (s *Server) markSpent(ctx context.Context, accountID, groupKey string) {
	until := s.windowReset(ctx, accountID, groupKey)
	_ = s.Rot.MarkWindowExhausted(ctx, accountID, groupKey, until, "quota")
	// 后台的活儿有上限：到顶了这一次就不读，标记照着上面写的那份留
	// 着（冷却时间），下一次再说。
	s.startBackground(func() { s.correctMark(accountID, groupKey) })
}

// correctMark reads the account and puts the mark's time right.
//
// 它不挡客户端：后台的活。
//
// 读到的时刻比现在的标记晚，标记就挪到那儿；不然标记原样留着。读到的
// 东西里没有这个组（或者读失败了），那就没有时刻：没有时刻就不动。
//
// 提前放一个用完的账号回轮转，正是这个标记要防的事。
func (s *Server) correctMark(accountID, groupKey string) {
	ctx, cancel := context.WithTimeout(s.lifeCtx, spentReadTimeout)
	defer cancel()
	snap, err := s.Reader.RefreshUsage(ctx, accountID)
	if err != nil {
		// 读不到：标记原样留着。
		return
	}
	until := resetOfSnapshot(snap, groupKey)
	if until.IsZero() {
		// 这一次读到的东西里没有这个组：没有就是没有。写一个零进去，
		// 冷却时间就会把标记缩短。
		return
	}
	// 写的时限另起一份：读的那一份用完了，写也该写得下。
	wctx, wcancel := context.WithTimeout(s.lifeCtx, spentWriteTimeout)
	defer wcancel()
	cur, ok := s.markedUntil(wctx, accountID, groupKey)
	if !ok {
		// 不知道现在的标记到什么时候：不知道就不动。
		return
	}
	if cur.After(until) {
		// 现在的标记比读到的时刻还晚：标记不动。标记只往后挪。
		return
	}
	_ = s.Rot.MarkWindowExhausted(wctx, accountID, groupKey, until, "quota")
}

// markedUntil gives the moment the account's group is marked spent
// until, and whether that mark could be read.
//
// 库里那一行说了算：标记现在到什么时候，是写下去的那一份。读不到就
// 说一声：不知道就不动。
func (s *Server) markedUntil(ctx context.Context, accountID, groupKey string) (time.Time, bool) {
	rows, err := s.Store.ListExhausted(ctx)
	if err != nil {
		return time.Time{}, false
	}
	var until time.Time
	for _, e := range rows {
		if e.AccountID != accountID || e.WindowKey != groupKey {
			continue
		}
		if e.Until.After(until) {
			until = e.Until
		}
	}
	return until, true
}
```

- [ ] **Step 4: 网关把读额度放在一个字段上，后台的活儿挂在网关的命上**

`internal/gateway/gateway.go` 的 import 补上：

```go
	"mmxcode/internal/usage"
```

`Server` 里加读额度的那个字段（网关读额度只有这一个口子，测试把它换成假的，测试就永远不碰 MiniMax 自己）：

```go
	// Reader reads the credits of an account. It is a field so that a
	// test can stand in for the network: the gateway's only way to the
	// network for the credits goes through it.
	Reader usage.Reader
```

`Server` 里加后台那点活儿的账：

```go
	// lifeCtx is the life of the gateway: the background work hangs on
	// it, so it stops when the gateway stops.
	lifeCtx context.Context
	// lifeCancel ends the life of the gateway.
	lifeCancel context.CancelFunc
	// bgMu guards the background work's bookkeeping.
	bgMu sync.Mutex
	// bgClosed says the gateway is closing: no new background work.
	bgClosed bool
	// bgSem bounds how many background readings run at once.
	bgSem chan struct{}
	// bgWG counts the background readings that are in the air.
	bgWG sync.WaitGroup
```

`New` 里加：

```go
		Reader:       mgr,
```

```go
	g.lifeCtx, g.lifeCancel = context.WithCancel(context.Background())
	// 后台的读有上限：到顶了就跳过那一次，不排队。
	limit := cfg.Usage.Refresh.Concurrency
	if limit <= 0 {
		limit = 1
	}
	g.bgSem = make(chan struct{}, limit)
```

网关还得能关掉后台那点活儿（关的时候要等在飞的那些停下来，不然它会往一个正在关着的库上写）：

```go
// Close stops the background work and waits for it.
//
// 后台的活儿挂在网关的命上：网关关了它也停，而且要等在飞的那些停下
// 来 —— 不然它会往一个正在关着的库上写。
func (s *Server) Close() {
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgMu.Unlock()
	s.lifeCancel()
	s.bgWG.Wait()
}

// startBackground runs fn in the background, if there is room for it.
//
// 同时在跑的读有上限：到顶了就跳过这一次。标记就照着写下去的那份留
// 着（冷却时间），下一趟再说。
//
// 它说活儿有没有开始。
func (s *Server) startBackground(fn func()) bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return false
	}
	select {
	case s.bgSem <- struct{}{}:
	default:
		return false
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() { <-s.bgSem }()
		fn()
	}()
	return true
}
```

- [ ] **Step 5: 测试里换上假读者**

`internal/gateway/gateway_test.go` 里，`harness` 加一个字段：

```go
	// reader is the credits reader the gateway uses: a fake one, so a
	// test never goes to the network to read the credits.
	reader *fakeReader
```

`newHarness` 里：

```go
	// 读额度走假读者：网关读额度只有一个口子，把它换成假的，测试就
	// 永远不碰 MiniMax 自己。
	h.reader = &fakeReader{}
	h.gw.Reader = h.reader
	// 网关关了以后，后台的活儿也得停：它在往库里写。
	t.Cleanup(func() { h.gw.Close() })
```

- [ ] **Step 6: 网关关了以后后台的活儿也得停**

`cmd/mmxcode/serve.go` 里：

```go
			cancel()
			// 网关关了以后，后台的读额度也得停，而且要等它停下来：库
			// 是接着关的，它不该往一个正在关着的库上写。
			gw.Close()
```

- [ ] **Step 7: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 8: 提交**

```
git add internal/gateway cmd/mmxcode/serve.go
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
	round := s.round()
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

一个账号同时只跑一个请求。挑账号的时候要避开在飞的账号：不然第二个请求又落它身上，用的是同一份额度，额度掉得快一倍（规格 §6.1）。

**忙不是它不能答了的理由。** 绑定的那个账号在飞的时候，另一个账号答这一次的请求，绑定不动 —— 绑定是「这个会话的家」，不能因为一次忙就搬走（规格 §6.1）。

**能答的账号都忙的时候，等，不报错**（规格 §6.4）：等前面的请求结束，等的时候不占一次尝试。等不到（客户端自己超时了）就 429 带 `Retry-After`（规格 §6.2）—— 账号只是忙，不是坏了，客户端不该拿到一个 502。

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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
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

// TestTheBusyAccountIsNotTaken says an account with a request in the
// air on it is not taken.
//
// 在飞的账号再接一个请求，额度掉得就快一倍：它们用的是同一份额度。
func TestTheBusyAccountIsNotTaken(t *testing.T) {
	b := newBusyAccounts(1)
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request")
	}
	if !b.isBusy("acc_1") {
		t.Fatal("the account does not say it is busy")
	}
	if b.isBusy("acc_2") {
		t.Fatal("the account that is not in the air says it is busy")
	}
	b.give("acc_1")
	if b.isBusy("acc_1") {
		t.Fatal("the account still says it is busy")
	}
}

// TestTheRequestIsAnsweredByTheAccountThatIsNotBusy says a request goes
// to an account that has nothing in the air on it.
//
// 三个账号上都有请求在飞，第四个空着：请求要落到第四个身上，不是 502。
// 忙不是它不能答了的理由（规格 §6.1、§6.4）。
func TestTheRequestIsAnsweredByTheAccountThatIsNotBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_02", "acc_03", "acc_04"} {
		if err := h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok" + id, Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 前三个账号上都有一个请求在飞。
	for _, id := range []string{"acc_01", "acc_02", "acc_03"} {
		if !h.gw.busy.take(id) {
			t.Fatalf("the account %s did not take the request", id)
		}
		defer h.gw.busy.give(id)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	// 答的是那个空着的账号。
	h.mu.Lock()
	tokens := append([]string(nil), h.tokens...)
	h.mu.Unlock()
	if len(tokens) != 1 {
		t.Fatalf("the upstream was asked %d times, want once", len(tokens))
	}
	if !strings.Contains(tokens[0], "tokacc_04") {
		t.Fatalf("the upstream was asked with %q, want the account that is not busy", tokens[0])
	}
}

// TestTheSecondRequestWaitsForTheBusyAccount says the second request
// waits for the busy account instead of being given up on.
//
// 只有一个账号的时候它忙，第二个请求要等它做完，而不是报错（规格
// §6.4）。等到了就答上，等不到才是 429。
func TestTheSecondRequestWaitsForTheBusyAccount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 唯一的账号上有一个请求在飞：它一会儿就做完。
	if !h.gw.busy.take("acc_01") {
		t.Fatal("the account did not take the request")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		h.gw.busy.give("acc_01")
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestTheRequestIsToldToWaitWhenEveryAccountIsBusy says a request that
// cannot get a free account is told to come back.
//
// 等不到就是等不到：客户端自己超时了，网关不该把它吊着（规格 §6.2）。
// 那个答案是 429 带 Retry-After，不是 502：账号只是忙，不是坏了。
func TestTheRequestIsToldToWaitWhenEveryAccountIsBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 等不了多久：这个测试等的是那个「等不到」。
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	// 唯一的账号上有一个请求在飞，它一直不来。
	if !h.gw.busy.take("acc_01") {
		t.Fatal("the account did not take the request")
	}
	defer h.gw.busy.give("acc_01")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After was given")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "Busy|Concurrency|Gate|NotBusy|SecondRequest|Wait"
```

失败：`undefined: newBusyAccounts`、`undefined: isBusy`。

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

// isBusy says whether a request is in the air on an account.
//
// 它是 rotator 问的那一句：这个账号现在忙不忙（规格 §6.3）。
func (b *busyAccounts) isBusy(accountID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.at[accountID] >= b.per
}

// anyFree says whether some account is free right now.
//
// 一个账号都没在飞的时候，当然有空的：账本上是空的。
func (b *busyAccounts) anyFree() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.at) == 0 {
		return true
	}
	for _, n := range b.at {
		if n < b.per {
			return true
		}
	}
	return false
}

// waitForFree waits until an account is free, or the deadline is over.
//
// 等一个账号空出来：忙不是它不能答了的理由，答案是等它做完（规格
// §6.4）。等不到就是等不到：客户端自己超时了，网关不该把它吊着。
func (b *busyAccounts) waitForFree(ctx context.Context, deadline time.Time) bool {
	for {
		// 每一次都先睡一下再看：刚空出来的那个账号可能又被别的请求
		// 抢走了，一醒就轮着看会转不停。
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
		if b.anyFree() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
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

- [ ] **Step 4: 池子多一个查询：这个账号现在忙不忙**

规格 §6.3：挑账号要避开在飞的账号 —— rotator 要能问一句「这个账号现在忙不忙」—— 跟现有的 `RemainingFor` 一个路子：**池接口加一个查询**，网关把在飞的计数报给它。

`internal/rotator/rotator.go` 里加：

```go
// BusyFor is an optional part of a Pool: it says whether an account
// is busy right now — a request is in the air on it.
//
// The accounts that are busy are not picked: a second request on one of
// them spends the same credits twice as fast. A request that finds
// every account that could answer busy gets ErrAllBusy, and waits for
// one of them to come free.
//
// A pool without it is asked nothing: no account is ever busy as far as
// the rotator knows, and everything works as it did before.
type BusyFor interface {
	Busy(ctx context.Context, accountID string) bool
}

// ErrAllBusy says every account that could answer is busy right now.
//
// 它跟「都花完了」是两件事：花完的账号等不来（在飞的会空出来）。网关
// 认得它，就知道该等，而不是报错。
var ErrAllBusy = errors.New("every account that could answer is busy right now")
```

「都忙」也有它自己的一句话，别的路认得出它：

`internal/rotator/rotator.go` 里，`Pick` 挑账号的时候先问一句谁在飞：

```go
	// The accounts that are not busy: a request is in the air on the
	// other ones, and they are not picked.
	free, err := r.freeAccounts(ctx, usable)
	if err != nil {
		return Plan{}, err
	}
```

在飞的账号不挑，绑定的那个账号在飞的时候是另一回事（规格 §6.1：忙不是它不能答了的理由）：

```go
			case r.isBusy(ctx, boundID):
				// The bound account is busy: another account answers
				// this request, and the conversation stays where it
				// is. A busy account is not one that cannot answer.
				a, err := r.bestAccount(ctx, free)
				if err != nil {
					return Plan{}, err
				}
				r.markUsed(a.ID)
				return r.plan(a, group, false, true), nil
```

`internal/rotator/rotator.go` 里加这两个：

```go
// freeAccounts gives the accounts that are not busy right now.
//
// 在飞的账号不挑：第二个请求落在它身上，额度掉得就快一倍。
//
// 它们都忙的时候是 ErrAllBusy：那是「等一等」的事，不是「坏了」的事。
func (r *Rotator) freeAccounts(ctx context.Context, usable []store.Account) ([]store.Account, error) {
	busy, ok := r.pool.(BusyFor)
	if !ok {
		// 这个池子说不了谁在飞：跟从前一样，一个都不算忙。
		return usable, nil
	}
	free := make([]store.Account, 0, len(usable))
	for _, a := range usable {
		if busy.Busy(ctx, a.ID) {
			continue
		}
		free = append(free, a)
	}
	if len(free) == 0 {
		return nil, ErrAllBusy
	}
	return free, nil
}

// isBusy says whether a request is in the air on an account.
//
// 池子说不了谁在飞的时候，谁也不忙。
func (r *Rotator) isBusy(ctx context.Context, accountID string) bool {
	busy, ok := r.pool.(BusyFor)
	if !ok {
		return false
	}
	return busy.Busy(ctx, accountID)
}
```

- [ ] **Step 5: 网关拿上这两样**

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

网关是那个池子：它把在飞的计数报给 rotator。

`internal/gateway/gateway.go` 里，`poolAdapter` 加一个方法：

```go
// Busy says whether a request is in the air on an account.
//
// 网关把在飞的计数报给 rotator，rotator 挑账号的时候避开在飞的那些
// （规格 §6.3）。这个账是网关自己的：它不用问数据库。
func (p poolAdapter) Busy(ctx context.Context, accountID string) bool {
	if p.busy == nil {
		return false
	}
	return p.busy.isBusy(accountID)
}
```

- [ ] **Step 6: 聊天那条路多两道门**

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

账号那一圈：挑到账号之后先抢，抢不到就等 —— 等的时候不占一次尝试，因为忙不是一次失败。等不到就 429 带 `Retry-After`（规格 §6.2、§6.4）：

```go
	// busyDeadline is when this request stops waiting for a free account.
	// 它是整个请求的一份：等一个账号空出来，等不到就 429（规格 §6.2）。
	var busyDeadline time.Time
	// waitForFree waits for an account to come free.
	waitForFree := func() bool {
		if busyDeadline.IsZero() {
			busyDeadline = time.Now().Add(s.Cfg.Concurrency.Wait.Duration())
		}
		return s.busy.waitForFree(ctx, busyDeadline)
	}
	for {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if errors.Is(err, rotator.ErrAllBusy) {
				// 能答的账号都在飞：等前面的请求结束，而不是报错
				// （规格 §6.4）。等不到就是等不到：客户端自己超时了。
				if !waitForFree() {
					w.Header().Set("Retry-After", "5")
					writeError(w, proto, http.StatusTooManyRequests,
						"every account is busy right now: too many requests are in the air")
					return
				}
				continue
			}
			if lastErr != nil {
				s.writeFailure(w, proto, quota, quotaMsg, lastStatus, lastMsg, lastErr)
				return
			}
			writeError(w, proto, http.StatusServiceUnavailable, err.Error())
			return
		}
		if !s.busy.take(plan.AccountID) {
			// 刚挑中的那个账号在挑的时候还是闲的，现在被人抢了：等它
			// 空出来。这一次不算一次失败：它没有坏，它只是忙。
			if !waitForFree() {
				w.Header().Set("Retry-After", "5")
				writeError(w, proto, http.StatusTooManyRequests,
					"every account is busy right now: too many requests are in the air")
				return
			}
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
	// busyDeadline is when this request stops waiting for a free account.
	// 它是整个请求的一份：等一个账号空出来，等不到就 429（规格 §6.2）。
	var busyDeadline time.Time
	// waitForFree waits for an account to come free.
	waitForFree := func() bool {
		if busyDeadline.IsZero() {
			busyDeadline = time.Now().Add(s.Cfg.Concurrency.Wait.Duration())
		}
		return s.busy.waitForFree(ctx, busyDeadline)
	}
	for {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if errors.Is(err, rotator.ErrAllBusy) {
				// 能答的账号都在飞：等前面的请求结束，而不是报错
				// （规格 §6.4）。等不到就是等不到：客户端自己超时了。
				if !waitForFree() {
					w.Header().Set("Retry-After", "5")
					writeError(w, proto, http.StatusTooManyRequests,
						"every account is busy right now: too many requests are in the air")
					return
				}
				continue
			}
			if lastErr != nil {
				s.writeFailure(w, proto, quota, quotaMsg, lastStatus, lastMsg, lastErr)
				return
			}
			writeError(w, proto, http.StatusServiceUnavailable, err.Error())
			return
		}
		if !s.busy.take(plan.AccountID) {
			// 刚挑中的那个账号在挑的时候还是闲的，现在被人抢了：等它
			// 空出来。这一次不算一次失败：它没有坏，它只是忙。
			if !waitForFree() {
				w.Header().Set("Retry-After", "5")
				writeError(w, proto, http.StatusTooManyRequests,
					"every account is busy right now: too many requests are in the air")
				return
			}
			continue
		}
		// 抢到了：这个账号归这个请求了。这一次算一次尝试：它真的被问
		// 过上游了。
		used[plan.AccountID] = true
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
}
```

改这一段的时候有五处要一起改，漏了哪一个都编译不过或者行为变了：

| 原来 | 改成 | 为什么 |
|---|---|---|
| `accountLoop:` 这个标号 | 删掉 | 闭包里没有标号可跳；留着 Go 会说「这个标号没有用」。 |
| `continue accountLoop` | `return false` | 换一个账号再来：外层那一圈会再挑一个。 |
| `break accountLoop` | `return true` | 客户端走了：请求到此为止，不再往一个已经走了的客户端写东西。 |
| 写完答案之后的 `return` | `return true` | 客户端拿到东西了，请求到此为止。 |
| `lastErr = err` 之后的 `continue` | `return false` | 这个账号这一轮没答上，换一个。 |
| `for attempt := 0; attempt < attempts; attempt++` | `for {` | 忙的那一圈不算一次尝试：次数由 rotator 那份账算，它算的是真的问过上游的次数。 |
| `used[plan.AccountID] = true` 放在抢之前 | 放在抢之后 | 抢不到的那个账号没有坏，它只是忙：它不算这一次被试过。 |
| 循环后面那句 `the request could not be served` | 删掉 | 循环里每一条路都自己回答客户端了，那句永远轮不到；留着 Go 会说「这段代码到不了」。 |

- [ ] **Step 7: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 8: 提交**

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
			ID         string                        `json:"id"`
			Limit      struct{ Context, Output int } `json:"limit"`
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
- Modify: `internal/gateway/spent_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/gateway/gateway_test.go` 里加（`internal/gateway/spent_test.go` 的那个假读者要记下问的是谁：多一个 `asked` 字段和一个 `askedFor()`）：

```go
// TestTheCardRefreshReadsOnlyTheHotAccounts says the card's refresh
// reads the hot accounts and no others.
//
// 卡片是 magpie 画的：它等不了。顺序读所有账号就是卡死的来源 —— 一
// 个账号要问三四个请求，N 个账号就是 N 倍。
func TestTheCardRefreshReadsOnlyTheHotAccounts(t *testing.T) {
	var mu sync.Mutex
	var askedAbout []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 站点被问起了谁：账号的 user_id 就在地址上。
		mu.Lock()
		askedAbout = append(askedAbout, r.URL.Query().Get("user_id"))
		mu.Unlock()
		w.Write([]byte(`{}`))
	})
	// 管理器读额度也指到假服务器：这一条路上同样不碰 MiniMax 自己。
	h.gw.Mgr.Acc.SetBase(h.srv.URL, h.srv.URL)
	ctx := context.Background()
	now := time.Now()
	// 两个热账号：它们得在库里，读到的数才有地方可写（外键指着账号）。
	for _, id := range []string{"acc_hot1", "acc_hot2"} {
		h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "0987654321",
			CreatedAt: now, UpdatedAt: now,
		})
	}
	// 冷账号上一次读到的数：卡片要摆着它。
	if err := h.store.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 500, Consumed: 62.5, Remaining: 437.5,
		Plan: "Token Plan M3", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 这两个刚被用过：它们是最热的。
	h.gw.Mgr.MarkUsed("acc_hot1")
	h.gw.Mgr.MarkUsed("acc_hot2")
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?refresh=1", nil)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 读的是那两个热的。
	asked := map[string]bool{}
	for _, id := range h.reader.askedFor() {
		asked[id] = true
	}
	for _, id := range []string{"acc_hot1", "acc_hot2"} {
		if !asked[id] {
			t.Errorf("the reader was not asked for %s, it was asked for %v", id, h.reader.askedFor())
		}
	}
	// 冷的那个没有读：它上一次读到的数摆着就够。
	if asked["acc_01"] {
		t.Errorf("the reader was asked for the cold acc_01, it was asked for %v", h.reader.askedFor())
	}
	// 站点也没有被问起冷账号：那一个一个读就是卡片卡死的地方。
	mu.Lock()
	about := append([]string(nil), askedAbout...)
	mu.Unlock()
	for _, uid := range about {
		if uid == "1234567890" {
			t.Errorf("the site was asked about the cold acc_01: %v", about)
			break
		}
	}
	// 卡片还是卡片的形状。
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
	return usage.NewRound(s.Store, s.Reader, s.Mgr.HotAccounts, s.Cfg)
}
```

`New` 里加（把热窗口告诉管理器）：

```go
	// 多久算热是配置说了算：管理器记着谁刚被用过，这个窗口是它的。
	mgr.HotWindow = cfg.Usage.Refresh.HotWindow.Duration()
```

`internal/gateway/gateway.go` 的 import 已经有 `"mmxcode/internal/usage"` 了（Task 5 加的）。

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

`internal/scheduler/scheduler_test.go` 里加（照着这个文件里 `TestUsageRefresh` 的那套环境写：起一个替身的站点服务器，让账本那个请求答一个数出来）：

```go
// TestTheUsageRoundReadsTheAccountsThatNeedIt says the round reads the
// accounts that need a reading first, and stops at the batch.
//
// 账号多起来以后，冷账号的额度没有必要读：读了也是旧的。批次满了就
// 停，剩下的下一趟接着读——排在前面的是该读的。
func TestTheUsageRoundReadsTheAccountsThatNeedIt(t *testing.T) {
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
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	// acc_never 一次也没读过：它得读一次，不然卡上没有它。
	// acc_cold 很久以前读过：它排在后面，这一趟轮不到它。
	for _, id := range []string{"acc_never", "acc_cold"} {
		if err := s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "1",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_cold", Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	// The account API is pointed at the test server. Without this the
	// test asks MiniMax for the numbers and waits for an answer that
	// never comes.
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	cfg := config.Default()
	// 一趟只读一个：读谁由排在前面的说了算。
	cfg.Usage.Refresh.Batch = 1
	sc := New(mgr, cfg)
	sc.RefreshUsage(ctx)

	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 只有那个从没读过的被读了。
	fresh := []string{}
	for _, snap := range snaps {
		if snap.CreatedAt.After(now) {
			fresh = append(fresh, snap.AccountID)
		}
	}
	if len(fresh) != 1 || fresh[0] != "acc_never" {
		t.Fatalf("the accounts read = %v, want only acc_never: %+v", fresh, snaps)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/scheduler/
```

失败：现在的 `RefreshUsage` 把每个账号都读了一遍，这一趟读了 2 个而不是 1 个。

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
//
// 它是网关和调度器共用的那一套：该读哪些账号、一趟读几个、并发几个。
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

- 额度只读该读的账号：签到待补的、标记到期的、从没读过的、还有热的。冷账号不读（它的数是旧的，它被用到的时候自己会醒过来）；标记到期的是读一次就好，读过了就不再到期；读失败的只重试十分钟。
- 一趟读的控：并发、每个账号的时限、一趟的预算、一趟最多读几个（配在 `usage.refresh` 下面）。
- 多久算热（`usage.refresh.hot_window`）。
- 额度用完的账号：立刻标记，后台读一次把标记改准；签到给了积分就读一次，读到的数说有额度就清掉标记。
- 并发：每个账号同时一个请求（`concurrency.per_account`），总并发是可用账号的五分之一（`concurrency.total_ratio` / `concurrency.total_min`）。

README 里「积分什么时候读」那一节要改：它写的还是「每 `schedule.usage_refresh` 问一遍**所有账号**」，现在不是了 —— 调度器每 `usage.refresh.scan_every` 扫一遍，只读该读的账号，一趟最多读 `usage.refresh.batch` 个。

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

### Task 12: 该读的名单收窄

这一节记的是复查改掉的东西：名单里多出来的那些账号，以及多出来的那几件事。

改之前，名单是「所有活着的账号」，冷账号排在最后。一趟最多读几个（批次）把它们截掉，于是每 30 秒就有六个冷账号被读一遍——一年下来比原来「每 15 分钟读一遍所有账号」多二十倍的流量，而这一趟读到的数还是旧的（冷账号的数只会更旧，不会更新）。规格 §3.2 写的该读的是四类：签到待补的、标记到期的、从没读过的、热的。

另外三件事也是同一根病根 —— 一个账号可以永远占着名单的第一位：

- 过期的「用完」标记没有人收，账号永远「到期」，六个这样的账号就把一整趟的批次占满，从来没读过的账号再也读不到（规格 §3.5 的「每个账号至少读一次」是硬要求）。
- 读失败的账号永远「签到待补」（规格 §5.3），一个一直读不回来的账号就把批次占住。
- 批次配 0 是「全都读」而不是「读一个」：配置里写着「配 0 的时候按 1 算」（`internal/config/config.go`），代码只照做了并发那一半。

还有一件是读出来的东西不对：`LatestUsageSnapshots` 按 id 排（最后写进去的那一行），而读额度的时候时刻是在读的开头盖的、插入是在读完之后 —— 两次读撞在一起的时候，先读到的那一次反而是后写进去的。卡上于是摆着旧的数。

**Files:**
- Modify: `internal/usage/round.go`
- Modify: `internal/usage/round_test.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`
- Modify: `README.md`
- Modify: `docs/superpowers/plans/2026-11-15-mmxcode-hot-usage.md`

- [ ] **Step 1: 写失败的测试**

`internal/usage/round_test.go` 里加：

```go
// TestTheColdAccountsAreLeftAlone says a cold account is not read.
//
// 冷账号的数是旧的，读它也是把旧的数再读一遍。它自己会醒过来：旧的
// 数是高估（积分只会往下走），轮转会挑它，它就热了，热了就该读。
func TestTheColdAccountsAreLeftAlone(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_cold", now.Add(-time.Minute))
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheDueAccountIsReadOnceAfterItsMarkExpires says a spent mark
// that has expired is read once, and then the account is left alone.
//
// 标记自然过期 → 到期读一次 → 数变新，账号回轮转（规格 §4.5 的恢复
// 点 A）。一次就好：读过的账号不到期了，不然六个到期的账号就把一趟
// 的批次占满，从来没读过的账号永远读不到。
func TestTheDueAccountIsReadOnceAfterItsMarkExpires(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_due", now.Add(-time.Hour))
	if err := s.SetExhausted(context.Background(), "acc_due", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_due" {
		t.Fatalf("the accounts to read = %v, want the due one", got)
	}
	// 读过了：数变新了，它不到期了。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_due", Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	got, err = r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheAccountThatIsStillSpentIsNotDue says an account that is
// still spent for one group is not read.
//
// 它还有一个组欠着额度：读它只是把用完的那一组再读一遍。标记全过期
// 了它才轮到——没过期的那一个说明它还欠着，回不了轮转。
func TestTheAccountThatIsStillSpentIsNotDue(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_spent", now.Add(-time.Hour))
	mk(t, s, "acc_waiting", now.Add(-time.Hour))
	// 一个过期了，另一个还没有。
	if err := s.SetExhausted(context.Background(), "acc_spent", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	if err := s.SetExhausted(context.Background(), "acc_spent", "other",
		now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}
	// 标记还没到：它还欠着，不该读。
	if err := s.SetExhausted(context.Background(), "acc_waiting", "general",
		now.Add(time.Hour), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
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

// TestTheFailedReadingIsNotRetriedForever says a reading that failed is
// tried again for a while, and then the account is left alone.
//
// 读失败的账号要再读（签到给的积分还在账本里等着），可是不能一直重
// 试：一个永远读不回来的账号就占着待补那一位，一趟的批次全被它拿
// 走，从来没读过的账号再也读不到——而每个账号至少读一次是硬要求。
func TestTheFailedReadingIsNotRetriedForever(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_failed", now.Add(-time.Hour))
	// 那次读失败了：没有数。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_failed", HasAmounts: false, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	// 签到不久之前：还再读一遍。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_failed", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_failed" {
		t.Fatalf("the accounts to read = %v, want the failed one", got)
	}
	// 十一分钟以后：不再重读了，把它放下。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_failed", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-11 * time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	got, err = r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// TestTheCheckInThatIsNotOnTheCardIsStillPending says a check-in that
// is newer than the reading is pending, whatever the retry window says.
//
// 签到给的积分卡上还没有，那是要补的：规格 §5.3 的那条规则跟重试的
// 期限是两回事。
func TestTheCheckInThatIsNotOnTheCardIsStillPending(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_checked", now.Add(-time.Hour))
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_checked", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now.Add(-11 * time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 || got[0] != "acc_checked" {
		t.Fatalf("the accounts to read = %v, want the pending one", got)
	}
}

// TestABatchOfZeroMeansOne says a batch of zero reads one account.
//
// 配 0 的时候按 1 算：0 个一批是一趟也读不了，那不是关掉，是坏了。
func TestABatchOfZeroMeansOne(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1", "acc_2"},
		Options{Concurrency: 1, Timeout: time.Second, Budget: time.Second, Batch: 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 1 {
		t.Fatalf("the readings = %d, want one", res.Refreshed)
	}
	if res.Skipped != 1 {
		t.Fatalf("the skipped = %d, want the one the batch left out", res.Skipped)
	}
}

// TestTheBatchOfZeroIsOneInTheControls says the two rounds' controls
// say one account when the config says zero.
//
// 卡片那一趟也是：card: 0 不该是"全都读"，该是"读一个"。
func TestTheBatchOfZeroIsOneInTheControls(t *testing.T) {
	cfg := config.Default()
	cfg.Usage.Refresh.Batch = 0
	cfg.Usage.Refresh.Card = 0
	r := NewRound(nil, nil, nil, cfg)
	if got := r.Options().Batch; got != 1 {
		t.Errorf("the periodic batch = %d, want one", got)
	}
	if got := r.CardOptions().Batch; got != 1 {
		t.Errorf("the card's batch = %d, want one", got)
	}
}
```

`internal/store/store_test.go` 里加：

```go
// TestTheNewestUsageSnapshotIsTheOneReadLast says the newest reading
// of an account is the one that was read last, not the one that was
// written last.
//
// 读额度的时候时刻是在读的开头盖的，插入是在读完之后：两次读撞在
// 一起的时候，先读到的那一次反而是后写进去的（id 更大）。只按 id
// 排，卡上就摆着旧的数。
//
// 时刻说哪一次读是最新的，id 只在时刻一样时分出先后——跟签到那边
// 一个样。
func TestTheNewestUsageSnapshotIsTheOneReadLast(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	readFirst := time.UnixMilli(1763000000000)
	readLater := readFirst.Add(time.Minute)
	// 先写进去的那一次读，是更晚读到的那一次。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Consumed: 0, Remaining: 100,
		ExpiresAt: readLater.Add(time.Hour), CreatedAt: readLater,
	}); err != nil {
		t.Fatal(err)
	}
	// 后写进去的那一次读，是更早读到的那一次：id 更大，读到的更早。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 300, Consumed: 0, Remaining: 300,
		ExpiresAt: readFirst.Add(time.Hour), CreatedAt: readFirst,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want one per account", len(got))
	}
	if got[0].Remaining != 100 {
		t.Fatalf("got %+v, want the reading that was made last", got[0])
	}
	one, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if one.Remaining != 100 {
		t.Fatalf("got %+v, want the reading that was made last", one)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/store/ ./internal/usage/
```

失败：冷账号在名单里；过期的标记让账号永远到期；读失败的账号永远待补；批次配 0 读的是全部；库里最新的一次读是最后写进去的那一次。

- [ ] **Step 3: 名单里只留该读的那些**

`internal/usage/round.go` 里改：冷账号不进名单（它被用到的时候自己会醒过来：旧的数是高估，轮转会挑它）；过期的标记只读一次（要没有还没过时刻的标记，而且标记过期之后没人读过它）；读失败的只重试 `retryWindow`；批次配 0 按 1 算。

```go
// The ranks the accounts are read in.
//
// 排在前面的先读：签到待补的卡上现在是错的，到期的马上要回轮转，从
// 没读过的排在热的前面（它是一次性的活儿，排在后面就永远排在后
// 面），热的刚变过，晚一轮没关系。
const (
	rankPending = iota
	rankDue
	rankNever
	rankHot
)

// retryWindow is how long a reading that failed is tried again.
//
// 读失败的账号要再读一遍（签到给的积分还在账本里等着），可是不能一
// 直重试：一个永远读不回来的账号（账本的批次没有期限、上游在挂）
// 会一直占着待补那一位，一趟的批次全被它拿走，从来没读过的账号就再
// 也读不到——而每个账号至少读一次是硬要求。
//
// 所以重试是有期限的：失败之后的一段时间里还再读（上游多半是暂时
// 的），过了这段时间就把这个账号放下，让别的账号先读。
//
// 它是一个常数，不是一个配置项：配置的键是规格定死了的。
const retryWindow = 10 * time.Minute

// target is one account to read, and where it stands in the order.
type target struct {
	// id is the account.
	id string
	// rank is the group it belongs to.
	rank int
	// order is what breaks a tie inside the rank.
	order int
	// since is when it began to wait, for the ranks that wait.
	since time.Time
}

// Targets gives the accounts to read now, in the order they should be
// read.
//
// 该读的就是那四类：签到待补的、标记到期的、从没读过的、热的。冷账号
// 不在名单里：它的数是旧的，读它只是把旧的数再读一遍——它被用到的时候
// 自己会醒过来（旧的数是高估，轮转会挑它），那时候它就热了。
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
	// 标记到期：标记过了时刻，账号该回轮转里读一次了。
	//
	// 到期读一次就好：读过了（数变新了）它就不再到期。还有一个标记没
	// 过时刻的，那个组还欠着额度，账号回不了轮转。
	expiredAt := map[string]time.Time{}
	stillSpent := map[string]bool{}
	for _, e := range exhausted {
		if e.Until.After(now) {
			stillSpent[e.AccountID] = true
			continue
		}
		if at, ok := expiredAt[e.AccountID]; !ok || e.Until.After(at) {
			expiredAt[e.AccountID] = e.Until
		}
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
		// 读到，卡上就还是旧的。
		//
		// 读到的那次失败了也要再读，可是只再读一会儿（retryWindow）：
		// 不限次数地重试，一个一直读不回来的账号就把整趟的批次占住
		// 了，从来没读过的账号再也读不到。
		pending := false
		if !checked.IsZero() {
			pending = !hasSnap || checked.After(read)
			if !pending && !snapHas[a.ID] && now.Sub(checked) < retryWindow {
				pending = true
			}
		}
		// 标记到期：标记过了时刻，而且没人读过它。
		//
		// 读过了就不到期了：到期读一次，数变新，账号回轮转。
		due := false
		if until, ok := expiredAt[a.ID]; ok && !stillSpent[a.ID] {
			due = !hasSnap || read.Before(until)
		}
		switch {
		case pending:
			ts = append(ts, target{id: a.ID, rank: rankPending, since: checked})
		case due:
			ts = append(ts, target{id: a.ID, rank: rankDue, since: expiredAt[a.ID]})
		case !hasSnap:
			// 从没读过的：卡上没有它，而且它永远排在最后就永远读不到。
			ts = append(ts, target{id: a.ID, rank: rankNever})
		default:
			if at, ok := hotAt[a.ID]; ok {
				ts = append(ts, target{id: a.ID, rank: rankHot, order: at})
			}
			// 冷的：不读。它的数是旧的，被用到的时候它自己会醒过来。
		}
	}

	// 排在前面的先读。同一组里：等得最久的先（签到待补的按签到的时
	// 刻，标记到期的按到期的时刻），热的按用得多的先，其余按 id 分出
	// 来——结果每次一样。
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].rank != ts[j].rank {
			return ts[i].rank < ts[j].rank
		}
		switch ts[i].rank {
		case rankPending, rankDue:
			if !ts[i].since.Equal(ts[j].since) {
				return ts[i].since.Before(ts[j].since)
			}
		case rankHot:
			if ts[i].order != ts[j].order {
				return ts[i].order < ts[j].order
			}
		}
		return ts[i].id < ts[j].id
	})

	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.id)
	}
	return out, nil
}
```

`internal/usage/round.go` 里加：

```go
// batch gives how many accounts one round reads at most.
//
// 配 0 的时候按 1 算：0 个一批是一趟也读不了，那不是关掉，是坏了。
// 卡片那一趟也一样：card: 0 该是"读一个"，不是"全都读"。
func batch(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}
```

- [ ] **Step 4: 库里「最新的一次读」按时刻排**

`internal/store/store.go` 里改：时刻说哪一次读是最新的，id 只在时刻一样时分出先后 —— 跟签到那边一个样（签到那一行是就地改的，id 不动，只有时刻往前走）。

```go
// LatestUsageSnapshots gives each account's newest usage reading.
//
// The newest is the one that was read last: the time says when the
// reading was made, and the id only breaks a tie between two readings
// of the same millisecond.
//
// The id alone is not enough. A reading stamps its time when it starts
// and writes it down when it ends, so two readings of one account that
// overlap leave the older reading with the greater id -- and the card
// would then show the older numbers. The check-ins have the same shape
// and the same rule.
func (s *Store) LatestUsageSnapshots(ctx context.Context) ([]UsageSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+usageSnapshotColumns("s")+`
		FROM usage_snapshots s
		WHERE s.id = (
			SELECT s2.id FROM usage_snapshots s2
			WHERE s2.account_id = s.account_id
			ORDER BY s2.created_at DESC, s2.id DESC
			LIMIT 1
		)
		ORDER BY s.account_id`)
	if err != nil {
		return nil, fmt.Errorf("latest usage snapshots: %w", err)
	}
	defer rows.Close()
	var out []UsageSnapshot
	for rows.Next() {
		u, err := scanUsageSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("latest usage snapshots: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest usage snapshots: %w", err)
	}
	return out, nil
}

// LatestUsageSnapshot gives one account's newest usage reading.
//
// It is the reading that was made last, for the same reason as in
// LatestUsageSnapshots: the time says which reading is the newest and
// the id only breaks a tie.
//
// An account that was never read has no reading and no error.
func (s *Store) LatestUsageSnapshot(ctx context.Context, accountID string) (UsageSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+usageSnapshotColumns("")+`
		FROM usage_snapshots WHERE account_id=?
		ORDER BY created_at DESC, id DESC LIMIT 1`, accountID)
	u, err := scanUsageSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UsageSnapshot{}, nil
	}
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("latest usage snapshot %s: %w", accountID, err)
	}
	return u, nil
}
```

- [ ] **Step 5: README 改掉那两句**

「读额度」那一节里「排在前面的先读：签到待补 → 标记到期 → 从没读过 → 热 → 冷」和冷账号排在最后的那一句都错了：冷账号不在名单里。

- [ ] **Step 6: 提交**

```
git add internal/usage internal/store README.md docs/superpowers/plans
git commit -m "fix(usage): 该读的是该读的那些账号"
```

---

### Task 13: 忙的账号等一等

这一节记的是复查改掉的两件事：一件是忙的账号，一件是同一个账号被两趟同时读。

**一、忙的账号等一等。**

轮转挑的是余额最多的那个 —— 正是几个请求都在飞的时候最常被挑到的那个。它在飞，而网关自己那份「谁在飞」的账是在挑完之后才说话的：挑中的那个账号抢不到，这一圈就白费了。三次尝试用完了，客户端拿到的是 502「这个请求没法服务」，而账号不过是忙，不是坏了。

规格里那三句话就是改法：

- §6.3：挑账号要避开在飞的账号 —— rotator 要能问一句「这个账号现在忙不忙」—— 跟现有的 `RemainingFor` 一个路子：**池接口加一个查询**，网关把在飞的计数报给它。
- §6.4：可用账号都忙（比如只有 1 个账号）→ **等前面的请求结束，而不是报错**。
- §6.1：满了怎么办：**换号，但不改绑**。绑定是「这个会话的家」，不能因为一次忙就搬走。

于是有三处：

- **池接口多一个可选的查询**（`BusyFor`，Task 7 的 Step 4）：有它的池子被问一句，没它的池子跟从前一样 —— 一个账号都不算忙。
- **rotator 避开在飞的账号**，而且说得出「它们都忙」：`ErrAllBusy`。那是跟「都花完了」分开的一句话：花完的账号等不来，在飞的会空出来。绑定的账号在飞的时候，另一个账号答这一次的请求，绑定不动。
- **网关等，不报错**：等一个账号空出来，等的时候不占一次尝试（忙不是一次失败）。等不到（`concurrency.wait`，默认十秒）就 429 带 `Retry-After` —— 客户端自己超时了，网关不该把它吊着。

**二、一个账号同一时刻只读一次。**

这个进程里有三趟读额度：签到之后那一趟、周期那一趟、卡片现读那一趟。`Round.Run` 只在**一趟之内**去重（同一个账号在名单里出现两遍，读它一遍就够了），两趟撞上的时候两边都去读，读的是同一次。

规格 §3.3 说：「**单账号去重**：同一个账号同一时刻只读一次（签到那一趟和热的那一趟可能撞上）」。守这一条的是管理器：这个进程里三趟读额度共用一个 `*manager.Manager`，所以那把锁放在它身上。

`Round.Run` 读一个账号之前先问一句「有人正在读它吗」：正在读就不读。那不是失败，也不是「预算用完了没轮上」，所以不算，也不记一笔理由 —— 它很正常。

**Files:**
- Modify: `internal/rotator/rotator.go`
- Modify: `internal/rotator/rotator_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/busy.go`
- Modify: `internal/gateway/busy_test.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/manager_test.go`
- Modify: `internal/usage/round.go`
- Modify: `internal/usage/round_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/rotator/rotator_test.go` 里加：

```go
// TestTheBusyAccountIsNotPicked says an account with a request in the
// air on it is not picked.
//
// 在飞的账号再接一个请求，额度掉得就快一倍：它们用的是同一份额度。
// 挑账号的时候要避开在飞的那些（规格 §6.3）。
func TestTheBusyAccountIsNotPicked(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01"), acc("acc_02")),
		busy:     map[string]bool{"acc_01": true},
	}
	r := New(p, Options{MaxAttempts: 3})

	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s, want the one that is not busy", got.AccountID)
	}
}

// TestEveryAccountBusyIsSaidSo says the rotator says the accounts are
// busy, and not that they are spent.
//
// 都忙跟都花完是两件事：忙的等一等就空出来了，花完的等不来。网关要
// 认得这个差别，才知道该等，而不是报错（规格 §6.4）。
func TestEveryAccountBusyIsSaidSo(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01")),
		busy:     map[string]bool{"acc_01": true},
	}
	r := New(p, Options{MaxAttempts: 3})

	_, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if !errors.Is(err, ErrAllBusy) {
		t.Fatalf("the error = %v, want the all-busy one", err)
	}
}

// TestTheBindingStaysWhenTheBoundAccountIsBusy says a busy bound
// account does not move the binding.
//
// 绑定是"这个会话的家"：忙不是它不能答了的理由（规格 §6.1）。忙的时候
// 换一个账号答这一次，绑定还在原来那个账号上。
func TestTheBindingStaysWhenTheBoundAccountIsBusy(t *testing.T) {
	p := &busyPool{
		fakePool: newFakePool(acc("acc_01"), acc("acc_02")),
		busy:     map[string]bool{"acc_01": true},
	}
	p.sessions["sess-1"] = "acc_01"
	r := New(p, Options{MaxAttempts: 3})

	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("picked %s, want the one that is not busy", got.AccountID)
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

// TestAPoolThatCannotSayIsNotAsked says a pool that cannot say which
// accounts are busy is not asked.
//
// 没有那个说法的池子：跟从前一样，一个账号都不算忙（规格 §6.3 的那
// 个查询是可选的）。
func TestAPoolThatCannotSayIsNotAsked(t *testing.T) {
	p := newFakePool(acc("acc_01"))
	r := New(p, Options{MaxAttempts: 3})
	got, err := r.Pick(context.Background(), "sess-1", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_01" {
		t.Fatalf("picked %s", got.AccountID)
	}
}
```

`internal/manager/manager_test.go` 里加：

```go
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
```

`internal/usage/round_test.go` 里加：

```go
// heldReader reads the credits and can be held in the air.
//
// 它能把一次读吊在半空：另一趟就是在这时候撞上来的。它拿的是管理器
// 的那把锁——这一节看的就是管理器跟那一趟的配合。
type heldReader struct {
	// mgr keeps the rounds from reading the same account at once.
	mgr *manager.Manager
	// started says which account a reading is about.
	started chan string
	// release lets the held reading finish.
	release chan struct{}
	// mu guards read.
	mu sync.Mutex
	// read are the accounts that were read.
	read []string
}

// RefreshUsage reads the credits of an account.
func (h *heldReader) RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	h.mu.Lock()
	h.read = append(h.read, accountID)
	h.mu.Unlock()
	if h.started != nil {
		h.started <- accountID
	}
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return store.UsageSnapshot{}, ctx.Err()
		}
	}
	return store.UsageSnapshot{
		AccountID: accountID, Granted: 100, Consumed: 10, Remaining: 90,
		HasAmounts: true, CreatedAt: time.Now(),
	}, nil
}

// BeginReading keeps two rounds from reading the same account at once.
func (h *heldReader) BeginReading(accountID string) bool {
	return h.mgr.BeginReading(accountID)
}

// EndReading says the reading of an account is done.
func (h *heldReader) EndReading(accountID string) {
	h.mgr.EndReading(accountID)
}

// count says how many readings were asked for.
func (h *heldReader) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.read)
}

// TestTwoRoundsDoNotReadTheSameAccountAtTheSameTime says two rounds
// over the same accounts read one of them once between them.
//
// 签到那一趟和热的那一趟可能撞上同一个账号：同一个账号同一时刻只读
// 一次（规格 §3.3）。撞上的那个账号不是读失败，也不是没轮上：另一趟
// 正在读它。
func TestTwoRoundsDoNotReadTheSameAccountAtTheSameTime(t *testing.T) {
	mgr := manager.New(nil, nil, nil, nil, nil)
	reader := &heldReader{
		mgr:     mgr,
		started: make(chan string, 4),
		release: make(chan struct{}),
	}
	r := &Round{Reader: reader}
	opts := Options{Concurrency: 1, Timeout: 5 * time.Second, Budget: 10 * time.Second, Batch: 6}

	first := make(chan Result, 1)
	go func() {
		res, err := r.Run(context.Background(), []string{"acc_1"}, opts)
		if err != nil {
			t.Errorf("run: %v", err)
		}
		first <- res
	}()
	// 第一趟的那一次读已经在半空了。
	if got := <-reader.started; got != "acc_1" {
		t.Fatalf("the reading is about %q, want the one account", got)
	}

	// 第二趟撞上来了：这个账号正在读，它不读。
	res, err := r.Run(context.Background(), []string{"acc_1"}, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 0 || res.Failed != 0 || res.Skipped != 0 {
		t.Fatalf("the second round = %+v, want nothing read, failed or skipped", res)
	}
	if len(res.Reasons) != 0 {
		t.Fatalf("the second round said %v, want nothing: it is normal", res.Reasons)
	}

	close(reader.release)
	<-first
	if n := reader.count(); n != 1 {
		t.Fatalf("the account was read %d times, want once", n)
	}
}

// TestAReaderWithoutTheGuardIsUnaffected says a reader that cannot keep
// the rounds apart is read as it was.
//
// 没有那把锁的读者：每一趟都读它名单上的账号，规则没有变。
func TestAReaderWithoutTheGuardIsUnaffected(t *testing.T) {
	r := &Round{Reader: &fakeReader{fail: map[string]bool{}}}
	res, err := r.Run(context.Background(), []string{"acc_1"},
		Options{Concurrency: 1, Timeout: time.Second, Budget: time.Second, Batch: 6})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Refreshed != 1 {
		t.Fatalf("the readings = %d, want one", res.Refreshed)
	}
}
```

`internal/gateway/busy_test.go` 里加的那几条（Task 7 的 Step 1 那一整个文件）说的是同一件事：忙的账号等一等，等不到就 429，不是 502。

- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/rotator/ ./internal/gateway/ ./internal/manager/ ./internal/usage/
```

失败：`undefined: ErrAllBusy`、`undefined: isBusy`、`undefined: BeginReading`；网关那条是 502「the request could not be served」和 503「no account can answer」。

- [ ] **Step 3: 池子多一个查询**

Task 7 的 Step 4 已经写了：`BusyFor` 是池接口多出来的那个可选的部分，`ErrAllBusy` 是「它们都忙」那一句，`freeAccounts` 是挑之前的那一问。网关是那个池子，它把在飞的计数报给 rotator：

```go
// isBusy says whether a request is in the air on an account.
//
// 它是 rotator 问的那一句：这个账号现在忙不忙（规格 §6.3）。
func (b *busyAccounts) isBusy(accountID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.at[accountID] >= b.per
}

// anyFree says whether some account is free right now.
//
// 一个账号都没在飞的时候，当然有空的：账本上是空的。
func (b *busyAccounts) anyFree() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.at) == 0 {
		return true
	}
	for _, n := range b.at {
		if n < b.per {
			return true
		}
	}
	return false
}

// waitForFree waits until an account is free, or the deadline is over.
//
// 等一个账号空出来：忙不是它不能答了的理由，答案是等它做完（规格
// §6.4）。等不到就是等不到：客户端自己超时了，网关不该把它吊着。
func (b *busyAccounts) waitForFree(ctx context.Context, deadline time.Time) bool {
	for {
		// 每一次都先睡一下再看：刚空出来的那个账号可能又被别的请求
		// 抢走了，一醒就轮着看会转不停。
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
		if b.anyFree() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}
```

`internal/gateway/busy.go` 里加的是那把账上的两个读法：现在忙不忙（rotator 问的），有没有空出来的（网关等的）。

- [ ] **Step 4: 网关等一等**

Task 7 的 Step 6 那一段就是：挑到账号之后先抢，抢不到就等；rotator 说「它们都忙」的时候也是等。等的时候不占一次尝试，等不到就 429 带 `Retry-After`。

- [ ] **Step 5: 一个账号同一时刻只读一次**

`internal/manager/manager.go` 的 `Manager` 里加那份账：

```go
	// readMu guards the accounts that are being read.
	readMu sync.Mutex
	// reading holds the accounts that are being read right now.
	//
	// 它是这一份进程里的账：签到那一趟、周期那一趟和卡片那一趟共用
	// 一个管理器，所以这把锁是三趟共用的（规格 §3.3）。
	reading map[string]bool
```

`internal/manager/manager.go` 里加这两个方法：

```go
// BeginReading says a round is about to read an account, and whether it
// may.
//
// 同一个账号同一时刻只读一次（规格 §3.3）：签到那一趟和热的那一趟可
// 能撞上，两边都去读，读的是同一次。已经在读的那个账号，这一趟就不
// 读了——另一趟正在读它。
//
// It says whether the account was claimed: false means another round is
// reading it right now.
func (m *Manager) BeginReading(accountID string) bool {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	if m.reading == nil {
		m.reading = map[string]bool{}
	}
	if m.reading[accountID] {
		return false
	}
	m.reading[accountID] = true
	return true
}

// EndReading says the reading of an account is done.
//
// 读失败了也要还回来：不然那个账号再也读不到了。
func (m *Manager) EndReading(accountID string) {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	delete(m.reading, accountID)
}
```

`internal/usage/round.go` 里，`Reader` 旁边多一个可选的部分：

```go
// ReadingGuard is an optional part of a Reader: it keeps two rounds
// from reading the same account at the same time.
//
// 同一个账号同一时刻只读一次（规格 §3.3）：签到那一趟和热的那一趟可能
// 撞上，两边都去读，读的是同一次。
//
// A reader that has it is asked before every reading; a reader that has
// not is asked nothing, and every reading is read as it was before.
type ReadingGuard interface {
	// BeginReading says a round is about to read an account, and
	// whether it may. It says false when another round is reading that
	// account right now.
	BeginReading(accountID string) bool
	// EndReading says the reading of an account is done.
	EndReading(accountID string)
}
```

`internal/usage/round.go` 的 `Run` 里，读之前先问一句：

```go
			if hasGuard && !guard.BeginReading(id) {
				// 别的这一趟正在读这个账号：这一趟不读它。读的是同
				// 一次，读两遍是白读（规格 §3.3）。
				//
				// 这不是失败，也不是没轮上：它很正常，所以不算，也不
				// 记一笔理由。
				return
			}
			if hasGuard {
				// 读完了（读失败也是读完了）就把账号还回去。
				defer guard.EndReading(id)
			}
```

- [ ] **Step 6: 运行测试确认通过**

```
go test ./...
```

- [ ] **Step 7: 提交**

```
git add internal/rotator internal/gateway internal/manager internal/usage
git commit -m "fix(gateway): 忙的账号等一等，不算一次失败"
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
