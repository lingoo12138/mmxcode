# 账号池分阶段加载 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 网关把账号放在内存里的一个池子里：挑号不再每次扫全表，池有上界，额度用完的号出池、补一个冷的进来，会话照旧黏在一个号上。

**Architecture:** 新包 `internal/pool` 装着两样东西：池（内存里的账号、剩余积分、耗尽标记）和补它的人（keeper：启动时用库里的数填满，之后缺了补进去）。网关的 pool 适配器从池里读，不再读整表；池空了就回到从前的样子从库里读 —— 网关不能因为池空着就一个请求都答不了。池的大小 = max(总并发, 活跃会话数) × 倍数，对外的总并发按池夹。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`、`context`、`sync`

设计文档：`docs/superpowers/specs/2026-11-17-mmxcode-account-pool-staging-design.md`

---

## 四条不变量

1. **池只是缓存，不是真相。** 账号、耗尽标记、会话绑定的真相都在库里。池丢了，重启后从库里重建就行；池里的数过期了，读额度补上。**任何时候池都不能是「唯一知道账号存在的地方」。**
2. **没读到的不是零。** 池里一个号的积分没读过，就是「没读到」，不是 0。它在选号时排在有数的号后面。
3. **会话黏性只有「额度用完」能破。** 忙不是理由，请求内换号也不改绑。会话钉的号出池了 → 改绑（历史照带）。
4. **承诺的并发不超过池。** 池装不下就少承诺，不能一边说能接 200 一边只有 40 个号。

## 这一版不做的

| 不做 | 为什么 |
|---|---|
| 会话绑定的内存缓存 | 现在每次请求一次索引查询（`sessions` 表主键），几十微秒；池化之后每请求本来就只剩这一次数库 |
| 卡片那条路拆两段读 | 200–1000 个号时它只占 2MB；上到 10 万号才是生死线（另开一份规格） |
| 池里的号按「一阵子没用」换出 | 换出要重读额度，贵；而且会把会话钉的号换掉，缓存命中率掉。**只按额度用完出池** |
| 池里的计划窗口（`windows_json`） | 选号用不上；放进去内存账从 395B 变 2168B |
| 一个号同时跑多个请求 | `concurrency.per_account` 已经是 1，不动 |
| **读额度的令牌桶** | 现在读额度的开销是 `usage.refresh` 的 4 并发 + 6 个/30s，已经有上界。池子跑起来、看到真实的耗尽形态之前先不动它 —— 参数要拿真的数据来定（另开一份规格） |
| **每个窗口组一个熔断器** | 一个组的额度用完了，那个组该不该停手，现在靠「标记用完 → 出池」就够了（另开一份规格） |
| **失败的号短冷却** | 现在失败的号只是这一趟换下一个（重试上限 3）。失败是「额度没了」还是「网络抖」是两种病，不能一起治（另开一份规格） |
| **把池里读到的余额写回去**（`pool.SetRemaining`） | 定时读额度的那趟在 `internal/scheduler` / `internal/manager` 里，它们手里没有池。接起来要改那两处的结构 —— 跟「池归谁管」一起做（另开一份规格） |

---

### Task 1: 配置 —— 池多大、倍数、内存预算、并发天花板

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

池的大小 = max(总并发, 活跃会话数) × 倍数（规格 §6.3）。倍数 2：再留一倍给补号用。内存预算是个兜底 —— 一个账号按 512 字节算，32MB 装得下 6 万多个号。

外加**并发天花板**（`concurrency.total_max`，规格 §6.9）：池空的时候承诺是从库里数出来的，库里 1000 个号就承诺 200 —— 那一下能从池子撑的数抬到库里的数。天花板把它按住。0 = 不限，跟从前一样。

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go` 末尾加：
```go
// TestThePoolDefaults says what the pool does when nothing is said about
// it.
//
// 池多大 = max(总并发, 活跃会话数) × 倍数。倍数是 2：再留一倍给补号
// （规格 §6.3）。内存预算是兜底 —— 一个账号 512 字节，32MB 装得下 6 万
// 多个，实际用不到。
func TestThePoolDefaults(t *testing.T) {
	c := Default()
	if c.Pool.Factor != 2 {
		t.Errorf("factor = %d, want 2", c.Pool.Factor)
	}
	if c.Pool.MaxSize != 0 {
		t.Errorf("max_size = %d, want 0", c.Pool.MaxSize)
	}
	if c.Pool.MemoryBudgetMB != 32 {
		t.Errorf("memory_budget_mb = %d, want 32", c.Pool.MemoryBudgetMB)
	}
	if c.Pool.SessionWindow.Duration() != time.Hour {
		t.Errorf("session_window = %s, want 1h", c.Pool.SessionWindow)
	}
	if !c.Pool.ReadOnAdmit {
		t.Error("read_on_admit = false, want true")
	}
}

// TestThePoolIsReadFromTheFile says the pool's keys are the ones the
// file uses.
//
// 这些键名是给用户看的。写错了加载器会当错字拒掉（KnownFields），用户
// 只看到一句「unknown key」，不知道自己写错了还是网关认错了。
func TestThePoolIsReadFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "pool:\n  factor: 3\n  max_size: 7\n  memory_budget_mb: 64\n" +
		"  session_window: 30m\n  read_on_admit: false\n"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.Factor != 3 {
		t.Errorf("factor = %d, want 3", c.Pool.Factor)
	}
	if c.Pool.MaxSize != 7 {
		t.Errorf("max_size = %d, want 7", c.Pool.MaxSize)
	}
	if c.Pool.MemoryBudgetMB != 64 {
		t.Errorf("memory_budget_mb = %d, want 64", c.Pool.MemoryBudgetMB)
	}
	if c.Pool.SessionWindow.Duration() != 30*time.Minute {
		t.Errorf("session_window = %s, want 30m", c.Pool.SessionWindow)
	}
	if c.Pool.ReadOnAdmit {
		t.Error("read_on_admit = true, want false")
	}
}

// TestThePoolKeepsTheDefaultsOfTheKeysThatAreNotWritten says a file
// only has to write the keys it changes.
//
// 整段 pool 不写也是默认值：一个什么都不说的配置文件，网关也得能起来。
func TestThePoolKeepsTheDefaultsOfTheKeysThatAreNotWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "pool:\n  factor: 3\n"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.Factor != 3 {
		t.Errorf("factor = %d, want 3", c.Pool.Factor)
	}
	if c.Pool.SessionWindow.Duration() != time.Hour {
		t.Errorf("session_window = %s, want 1h", c.Pool.SessionWindow)
	}
	if c.Pool.MemoryBudgetMB != 32 {
		t.Errorf("memory_budget_mb = %d, want 32", c.Pool.MemoryBudgetMB)
	}
	if !c.Pool.ReadOnAdmit {
		t.Error("read_on_admit = false, want true")
	}
}

// TestThePoolRefusesAZeroSessionWindow says the session window does
// not take a zero.
//
// 没有窗口的「活跃」数不清楚：数出来的数是几，取决于拿什么时候比。别的
// 键写 0 也是错（TestTheOtherDurationsStillRefuseZero）。
func TestThePoolRefusesAZeroSessionWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "pool:\n  session_window: 0\n"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("the zero session window was taken")
	}
}

// TestTheTotalMaxIsReadFromTheFile says the ceiling's key is the one the
// file uses, and that it is off by default.
//
// 天花板是「并发可控」的那根杆：不管池子里有多少个号、库里有多少个号，
// 承诺的并发不超过它。默认 0 = 不限，跟从前一样。
func TestTheTotalMaxIsReadFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "concurrency:\n  total_max: 12\n"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency.TotalMax != 12 {
		t.Errorf("total_max = %d, want 12", c.Concurrency.TotalMax)
	}
	if Default().Concurrency.TotalMax != 0 {
		t.Errorf("the default total_max = %d, want 0", Default().Concurrency.TotalMax)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/config/ -run "TestThePool|TestTheTotalMax"
```

Expected: FAIL —— `c.Pool undefined`（还没有这个配置），`c.Concurrency.TotalMax undefined`（还没有天花板）。

- [ ] **Step 3: 写配置**

`internal/config/config.go`，加一个结构体：
```go
// Pool holds how many accounts the gateway keeps in memory.
//
// 池是缓存，不是真相（规格不变量 1）：账号、耗尽标记、会话绑定的真相都
// 在库里。池存在的理由是选号不用每次扫全表，还有内存有个上界。
//
// 池多大 = max(总并发, 活跃会话数) × 倍数。倍数是 2：总并发是「该跑多
// 少个请求」，活跃会话是「有多少段对话在用」，池要装得下它们（会话钉
// 的号不在池里会被改绑，缓存命中就没了），再留一倍给补号。
type Pool struct {
	// Factor is how many times over the demand the pool holds.
	//
	// 池装几个号 = max(总并发, 活跃会话数) × 这个倍数（2 = 装两倍）。
	//
	// 配 0 按 1 算：0 倍数是「池里一个号都没有」，那不是关掉，是坏了。
	Factor int `yaml:"factor" json:"factor"`
	// MaxSize caps the pool by the number of accounts. 0 means no cap.
	//
	// 配 0 是不限：这时候池的大小由内存预算和库里能用的号数说了算。
	MaxSize int `yaml:"max_size" json:"max_size"`
	// MemoryBudgetMB is how much memory the pool may take.
	//
	// 一个号按 512 字节算。配 0 是不设内存上限。
	MemoryBudgetMB int `yaml:"memory_budget_mb" json:"memory_budget_mb"`
	// SessionWindow is how long a session counts as active.
	//
	// 「活跃」= 这段时间里被碰过的会话，按最后一次碰的时间算。会话表从
	// 来不删，所以数的是最近碰过的，不是一共有的。
	//
	// 配 0 是错的（配置文件会拒），因为一个没有窗口的「活跃」数不清楚。
	SessionWindow Duration `yaml:"session_window" json:"session_window"`
	// ReadOnAdmit says whether a new account's credits are read before
	// it goes into the pool.
	//
	// 读一次是准的（读到的数说它有额度，它才进池），但读一次是 2~11 个
	// 上游请求。冷启动的填池不等这个读：池要立刻能用（规格 §6.3）。
	ReadOnAdmit bool `yaml:"read_on_admit" json:"read_on_admit"`
}
```

`internal/config/config.go`，`Config` 里加一个字段（挨着别的字段放就行）：
```go
	// Pool holds how many accounts the gateway keeps in memory.
	Pool Pool `yaml:"pool" json:"pool"`
```

`internal/config/config.go`，`Default()` 里加：
```go
		Pool: Pool{
			Factor:         2,
			MaxSize:        0,
			MemoryBudgetMB: 32,
			SessionWindow:  Duration(time.Hour),
			ReadOnAdmit:    true,
		},
```

`internal/config/config.go`，`Concurrency` 里加一个字段（挨着 `TotalMin` 放）：
```go
	// TotalMax is the ceiling on the total concurrency.
	//
	// 天花板：不管池子里有多少个号、库里有多少个号，承诺的并发不超过
	// 它。0 = 不限（用可用账号数 ÷ 5 算出来的那个数）。
	//
	// 它是「并发可控」的那根杆：池子的大小是按承诺的并发算的
	// （max(总并发, 活跃会话数) × 倍数），所以天花板定 64，池子就是
	// 128 个号 —— 「n×2」那个说法才有着落。
	//
	// 池空的时候它也在：那时候承诺从库里数，库里有多少号就承诺多少，
	// 那一下能从 20 抬到 80。天花板就是给这一下准备的。
	TotalMax int `yaml:"total_max" json:"total_max"`
```

`internal/config/config.go`，`Default()` 里 `Concurrency` 那一段改成：
```go
		Concurrency: Concurrency{
			PerAccount: 1,
			TotalRatio: 5,
			TotalMin:   3,
			TotalMax:   0,
			Wait:       Duration(10 * time.Second),
		},
```

- [ ] **Step 4: 跑测试，看它过**
```sh
go test ./internal/config/
```

Expected: PASS。

- [ ] **Step 5: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(config): 池的配置 —— 倍数、上限、内存预算

池多大 = max(总并发, 活跃会话数) × 倍数（规格 §6.3）。倍数 2：再留一倍
给补号。内存预算是兜底：一个账号 512 字节，32MB 装得下 6 万多个。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/config/config.go internal/config/config_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 2: store —— 候选账号的查询

**Files:**
- Modify: `internal/store/store.go`
- Create: `internal/store/pool.go`
- Create: `internal/store/pool_test.go`

池要装的号从哪儿来：库里。排队的规矩是三层：**最近活跃的对话绑着的号**（会话钉在号上，钉住的号不在池里就会被改绑，规格不变量 3）→ **从没读过的号，最新的导入在前**（新导入的几乎肯定没被用过）→ **读得最早的号**（最冷的那些）。

这个任务还带上**会话活跃**那件事（规格 §6.8）：`TouchSessions` 把用过的会话写回去，「活跃会话数」才是个真的数。

- [ ] **Step 1: 写失败的测试**

新建 `internal/store/pool_test.go`：
```go
package store

import (
	"context"
	"testing"
	"time"
)

// saveReading puts a reading of an account's credits in the store.
//
// 读到的时刻是可以说了算的：那个时刻就是「这个号有多冷」的凭据。
func saveReading(t *testing.T, s *Store, accountID string, remaining float64, at time.Time) {
	t.Helper()
	err := s.SaveUsageSnapshot(context.Background(), UsageSnapshot{
		AccountID:  accountID,
		Granted:    400,
		Consumed:   400 - remaining,
		Remaining:  remaining,
		HasAmounts: true,
		CreatedAt:  at,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// makeAccountAt puts an account in the store with a said creation time.
//
// 导入的时刻是「这个号有多新」的凭据。
func makeAccountAt(t *testing.T, s *Store, id string, createdAt time.Time) {
	t.Helper()
	err := s.CreateAccount(context.Background(), Account{
		ID: id, Name: id, Status: StatusActive, CreatedAt: createdAt, UpdatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// candidateIDs gives the ids of the candidates, in order.
func candidateIDs(t *testing.T, s *Store, limit int, exclude map[string]bool, since time.Time) []string {
	t.Helper()
	cands, err := s.PoolCandidates(context.Background(), limit, exclude, since)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Account.ID)
	}
	return ids
}

// TestTheCandidatesAreTheColdestFirst says the pool is filled with the
// accounts that were used least, not the ones that were used most.
//
// 冷号先进池：被用熟的号留给会话（规格 §6.5）。从来没读过的号排在最
// 前 —— 它还没有数，读一次就知道它有没有额度。
func TestTheCandidatesAreTheColdestFirst(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "warm")
	makeAccount(t, s, "cold")
	makeAccount(t, s, "never")
	now := time.Now()
	saveReading(t, s, "warm", 10, now.Add(-time.Hour))
	saveReading(t, s, "cold", 400, now.Add(-24*time.Hour))

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
	want := []string{"never", "cold", "warm"}
	if len(ids) != len(want) {
		t.Fatalf("candidates = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("candidates = %v, want %v", ids, want)
		}
	}
}

// TestANewlyImportedAccountComesBeforeAnOldOne says the accounts that
// were never read are taken newest first.
//
// 新导入的号几乎肯定没被用过：用过就会被读，读过就有数。导入很久还没
// 数的号多半是被用过的，它的额度早就不知道剩多少了（规格 §6.5）。
func TestANewlyImportedAccountComesBeforeAnOldOne(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	makeAccountAt(t, s, "old", now.Add(-30*24*time.Hour))
	makeAccountAt(t, s, "new", now)

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
	if len(ids) != 2 || ids[0] != "new" || ids[1] != "old" {
		t.Fatalf("candidates = %v, want [new old]", ids)
	}
}

// TestTheAccountsThatConversationsAreBoundToComeFirst says the account
// a live conversation is bound to is the first one into the pool.
//
// 会话钉在号上，钉住的号不在池里就会被改绑，缓存命中就没了（规格不变
// 量 3）。所以池子先装它们。
func TestTheAccountsThatConversationsAreBoundToComeFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "busy")
	makeAccount(t, s, "quiet")
	// 绑着的那号是被用过的：它有读数，是热的。
	saveReading(t, s, "busy", 400, now.Add(-time.Minute))
	if err := s.BindSession(ctx, "a-conversation", "busy"); err != nil {
		t.Fatal(err)
	}

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
	if len(ids) != 2 || ids[0] != "busy" {
		t.Fatalf("candidates = %v, want busy first", ids)
	}
}

// TestAConversationThatWasNotUsedLatelyDoesNotComeFirst says a
// conversation that was not used lately does not hold a place in the
// pool.
//
// 「用过」是网关写进去的（TouchSessions）。写不进去的对话不算活跃。
func TestAConversationThatWasNotUsedLatelyDoesNotComeFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "busy")
	makeAccount(t, s, "quiet")
	saveReading(t, s, "busy", 400, now.Add(-time.Minute))
	if err := s.BindSession(ctx, "a-conversation", "busy"); err != nil {
		t.Fatal(err)
	}
	// 把时钟拨回去：这个对话两小时前用过，之后没人碰它。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	if err := s.TouchSessions(ctx, []string{"a-conversation"}); err != nil {
		t.Fatal(err)
	}
	s.Clock = time.Now

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
	if len(ids) != 2 || ids[0] != "quiet" {
		t.Fatalf("candidates = %v, want quiet first", ids)
	}
}

// TestTheLimitIsALimit says the caller gets at most what it asked for.
//
// 多取的那些行是为了把被排除的补上，但补齐之后要截断 —— 调用方说要多
// 少，就是多少。
func TestTheLimitIsALimit(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")
	makeAccount(t, s, "c")

	ids := candidateIDs(t, s, 1, nil, now.Add(-time.Hour))
	if len(ids) != 1 {
		t.Fatalf("candidates = %v, want 1", ids)
	}
	// 被排除的那个正好排在前面：多取的行补上了它，截断之后还是 limit 个。
	ids = candidateIDs(t, s, 1, map[string]bool{"a": true}, now.Add(-time.Hour))
	if len(ids) != 1 {
		t.Fatalf("candidates = %v, want 1", ids)
	}
}

// TestACandidateThatWasNeverReadSaysNothingAboutItsCredits says the
// credits of an account that was never read are not zero.
//
// 它是「没读到」，不是「没有」：写成 0 就是撒谎，撒谎的数会被当成真的
// 数去挑号（规格不变量 2）。
func TestACandidateThatWasNeverReadSaysNothingAboutItsCredits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")

	cands, err := s.PoolCandidates(ctx, 10, nil, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if cands[0].Known {
		t.Fatal("the credits of an account that was never read cannot be known")
	}
	if cands[0].Remaining != 0 {
		t.Fatalf("remaining = %v, want 0", cands[0].Remaining)
	}
	if !cands[0].LastReadAt.IsZero() {
		t.Fatalf("last read at = %v, want the zero time", cands[0].LastReadAt)
	}
}

// TestACandidateThatWasReadCarriesItsCredits says a candidate that was
// read carries the credits it was read with.
func TestACandidateThatWasReadCarriesItsCredits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	at := time.Now().Add(-time.Minute)
	saveReading(t, s, "a", 123, at)

	cands, err := s.PoolCandidates(ctx, 10, nil, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if !cands[0].Known {
		t.Fatal("the credits of an account that was read are known")
	}
	if cands[0].Remaining != 123 {
		t.Fatalf("remaining = %v, want 123", cands[0].Remaining)
	}
	if cands[0].LastReadAt.UnixMilli() != at.UnixMilli() {
		t.Fatalf("last read at = %v, want %v", cands[0].LastReadAt, at)
	}
}

// TestTheAccountsInThePoolAreNotCandidates says the accounts that are
// already in the pool are not offered again.
func TestTheAccountsInThePoolAreNotCandidates(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")

	ids := candidateIDs(t, s, 10, map[string]bool{"a": true}, time.Now().Add(-time.Hour))
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("candidates = %v, want just b", ids)
	}
}

// TestAnAccountThatIsSpentIsNotACandidate says an account whose window
// is spent is not offered to the pool.
//
// 它的窗口什么时候回来，标记说了算：标记还在将来，它就接不了活。
func TestAnAccountThatIsSpentIsNotACandidate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")
	if err := s.SetExhausted(ctx, "a", "", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}

	ids := candidateIDs(t, s, 10, nil, time.Now().Add(-time.Hour))
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("candidates = %v, want just b", ids)
	}
}

// TestAnAccountWhoseMarkHasExpiredIsACandidate says an account whose
// window is back is offered again.
//
// 标记的时刻过去了，窗口就回来了，那个号又能接活了（规格 §4.5）。
func TestAnAccountWhoseMarkHasExpiredIsACandidate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	if err := s.SetExhausted(ctx, "a", "", time.Now().Add(-time.Minute), "quota"); err != nil {
		t.Fatal(err)
	}

	ids := candidateIDs(t, s, 10, nil, time.Now().Add(-time.Hour))
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("candidates = %v, want just a", ids)
	}
	n, err := s.CountUsable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("usable = %d, want 1", n)
	}
}

// TestAnAccountThatIsSignedOutIsNotACandidate says an account that is
// not signed in is not offered to the pool.
func TestAnAccountThatIsSignedOutIsNotACandidate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	if err := s.UpdateAccountStatus(ctx, "a", StatusSignedOut, "", time.Now()); err != nil {
		t.Fatal(err)
	}

	ids := candidateIDs(t, s, 10, nil, time.Now().Add(-time.Hour))
	if len(ids) != 0 {
		t.Fatalf("candidates = %v, want none", ids)
	}
}

// TestTheUsableAccountsAreTheOnesThatCanAnswer says the accounts that
// are signed in and not spent are the ones that can answer.
func TestTheUsableAccountsAreTheOnesThatCanAnswer(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")
	makeAccount(t, s, "c")
	if err := s.SetExhausted(ctx, "b", "", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAccountStatus(ctx, "c", StatusSignedOut, "", time.Now()); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountUsable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("usable = %d, want 1", n)
	}
}

// TestTheRecentSessionsAreCounted says the sessions that were touched
// lately are counted, and the old ones are not.
//
// 池要装得下活跃的会话：会话钉在号上，钉住的号不在池里会被改绑，缓存
// 命中就没了（规格 §6.3）。
func TestTheRecentSessionsAreCounted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	if err := s.BindSession(ctx, "recent", "a"); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountRecentSessions(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recent sessions = %d, want 1", n)
	}
	n, err = s.CountRecentSessions(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recent sessions = %d, want 0", n)
	}
}

// TestTouchingASessionMakesItActive says a session that was used is
// counted as active.
//
// 「用过」只有网关知道：它把用过的会话写回去（规格 §6.3）。写回去的
// 那一批，是一批一次写的。
func TestTouchingASessionMakesItActive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "a")
	if err := s.BindSession(ctx, "old", "a"); err != nil {
		t.Fatal(err)
	}
	// 这个会话两小时前绑的，之后没人碰它。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	if err := s.TouchSessions(ctx, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	s.Clock = time.Now

	n, err := s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recent sessions = %d, want 0", n)
	}
	if err := s.TouchSessions(ctx, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	n, err = s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recent sessions = %d, want 1", n)
	}
}

// TestTouchingNoSessionIsNothing says touching nothing does not touch
// the database.
//
// 空的批次不该发一条语句出去。
func TestTouchingNoSessionIsNothing(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.TouchSessions(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSessions(ctx, []string{""}); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/store/
```

Expected: FAIL —— `s.PoolCandidates undefined`。

- [ ] **Step 3: 写实现**

新建 `internal/store/pool.go`：
```go
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// touchBatch is how many sessions one statement touches.
//
// SQLite has a limit on how many placeholders a statement may have (999
// is the old default). A batch bigger than that would not run at all.
const touchBatch = 500

// Candidate is an account that could be admitted to the pool.
//
// 它是「还没被挑进池」的那一类账号：签入中、现在没有被标记用完，而且
// 带着它最后读到的那份积分。
type Candidate struct {
	// Account is the account itself.
	Account Account
	// Remaining is how many credits are left. It says something only
	// when Known is true.
	Remaining float64
	// Known says the credits are known.
	//
	// 它是 false 的时候 Remaining 说不了话：这个号的额度不知道是多少。
	// 不是 0 —— 0 是「读到了，没有了」。
	//
	// 它和 LastReadAt 是两回事：读过一次但那次没有数（HasAmounts 是
	// false）的号，是「读过了，数额不知道」：Known 是 false，LastReadAt
	// 不是零。
	Known bool
	// LastReadAt is when the credits were read last. It is the zero
	// time when they never were.
	LastReadAt time.Time
}

// PoolCandidates gives the accounts that could be admitted to the pool.
//
// 谁先进池（排队的规矩）：
//
//  1. **最近活跃的对话绑着的号** —— 它们不能丢：会话钉在号上，钉住的号
//     不在池里就会被改绑，缓存命中就没了（规格不变量 3）。
//  2. **从没读过的号，最新的导入排前面** —— 新导入的号几乎肯定没被用过
//     （用过就会被读，读过就有数）。导入很久还没数的号多半是被用过的，
//     它的额度早就不知道剩多少了。
//  3. **读过、但读得最早的号** —— 最冷的那些：它们闲着，闲着的号额度
//     还没被消耗光。
//
// 排除掉的：exclude 里有的（已经在池里）、状态不是 active 的、现在被
// 标记用完的（标记的时刻还在将来的才算；过去的标记已经回来了，那个号
// 可以再进池）。
//
// since 是「多久以前算活跃」：只有这段时间里被碰过的会话，它绑着的号
// 才排在最前。
//
// 读多少行：要 limit 个候选，外加 exclude 里那么多行 —— 被排除的那些也
// 占着行号，所以要多取一些才够数。回来的是**至多** limit 个。
func (s *Store) PoolCandidates(ctx context.Context, limit int, exclude map[string]bool, since time.Time) ([]Candidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_group, a.proxy_url,
			a.last_error, a.created_at, a.updated_at,
			u.remaining, u.created_at
		FROM accounts a
		LEFT JOIN usage_snapshots u ON u.id = (
			SELECT u2.id FROM usage_snapshots u2 WHERE u2.account_id = a.id
			ORDER BY u2.created_at DESC, u2.id DESC LIMIT 1)
		WHERE a.status = ?
		  AND NOT EXISTS (
			SELECT 1 FROM exhausted_windows e
			WHERE e.account_id = a.id AND e.until > ?)
		ORDER BY
			(EXISTS (SELECT 1 FROM sessions s
				WHERE s.account_id = a.id AND s.updated_at >= ?)) DESC,
			(u.created_at IS NULL) DESC,
			u.created_at ASC,
			a.created_at DESC,
			a.id
		LIMIT ?`, StatusActive, ms(s.now()), ms(since), limit+len(exclude))
	if err != nil {
		return nil, fmt.Errorf("pool candidates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var (
			c         Candidate
			remaining sql.NullFloat64
			readAt    sql.NullInt64
		)
		a, err := scanAccount(rows, &remaining, &readAt)
		if err != nil {
			return nil, fmt.Errorf("pool candidates: %w", err)
		}
		c.Account = a
		c.Remaining = remaining.Float64
		c.Known = remaining.Valid
		c.LastReadAt = fromMS(readAt.Int64)
		if exclude[c.Account.ID] {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pool candidates: %w", err)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CountUsable says how many accounts could answer right now.
//
// 能答 = 签入中（status 是 active）而且现在没有被标记用完。标记的时刻
// 已经过去的那个号算能答 —— 它的窗口回来了（规格 §4.5）。
func (s *Store) CountUsable(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM accounts a
		WHERE a.status = ?
		  AND NOT EXISTS (
			SELECT 1 FROM exhausted_windows e
			WHERE e.account_id = a.id AND e.until > ?)`,
		StatusActive, ms(s.now())).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count usable accounts: %w", err)
	}
	return n, nil
}

// CountRecentSessions says how many sessions were touched since a moment.
//
// 「活跃会话」是这么数的：一段时间里被碰过的会话。绑在它们身上的号是
// 缓存命中的地方 —— 池要装得下它们（规格 §6.3）。
//
// 会话表从来不删（规格 §8），所以数的是「最近碰过的」，不是「一共有的」。
//
// 「碰过」是网关写进去的（TouchSessions）：每次请求真的用了这个会话，
// 网关就把它的 updated_at 刷新一下。刷新是攒着写的 —— 一批一次，不是
// 每个请求写一行。
func (s *Store) CountRecentSessions(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE updated_at >= ?", ms(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count recent sessions: %w", err)
	}
	return n, nil
}

// TouchSessions marks sessions as used now.
//
// 「用过」是活跃会话唯一的凭据。updated_at 只在改绑的时候刷新：一段一直
// 用同一个号的对话从来不改绑，它的 updated_at 就停在当初绑定的那一刻，
// 一小时以后看起来就不活跃了 —— 而它明明一直在用。
//
// 一次改一批：每个请求写一行是浪费，锁也白抢。网关那边攒一批再一次写
// 掉，这里也按批写 —— 一条语句的占位符个数有上限。
func (s *Store) TouchSessions(ctx context.Context, sessionKeys []string) error {
	if len(sessionKeys) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(sessionKeys))
	keys := make([]string, 0, len(sessionKeys))
	for _, k := range sessionKeys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	for start := 0; start < len(keys); start += touchBatch {
		end := start + touchBatch
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		args = append(args, ms(s.now()))
		for _, k := range chunk {
			args = append(args, k)
		}
		q := "UPDATE sessions SET updated_at = ? WHERE session_key IN (" + marks + ")"
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("touch the sessions: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 4: `scanAccount` 收下多的那几列**

候选账号的查询要多读两列（额度和读到的时刻），但账号自己的十列不用再抄一遍 —— `scanAccount` 收下多的目的地：

`internal/store/store.go`，`scanAccount` 改成：
```go
// scanAccount reads one account row.
//
// The extra destinations are read after the account's own columns, so a
// caller that wants more of the row (the credits of a reading, say) does
// not have to spell the account's ten columns out again.
func scanAccount(sc scanner, extra ...any) (Account, error) {
	var (
		a                     Account
		userID, email, pg, pu sql.NullString
		lastErr               sql.NullString
		createdAt, updatedAt  int64
	)
	dest := []any{&a.ID, &a.Name, &userID, &email, &a.Status, &pg, &pu,
		&lastErr, &createdAt, &updatedAt}
	dest = append(dest, extra...)
	if err := sc.Scan(dest...); err != nil {
		return Account{}, err
	}
	a.UserID = userID.String
	a.Email = email.String
	a.ProxyGroup = pg.String
	a.ProxyURL = pu.String
	a.LastError = lastErr.String
	a.CreatedAt = fromMS(createdAt)
	a.UpdatedAt = fromMS(updatedAt)
	return a, nil
}
```

- [ ] **Step 5: 跑测试，看它过**
```sh
go test ./internal/store/
```

Expected: PASS。

- [ ] **Step 6: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(store): 池的候选账号 —— 对话绑着的号在前，冷号随后

排队的规矩：最近活跃的对话绑着的号（会话钉在号上，钉住的号不在池里就
会被改绑，规格不变量 3）→ 从没读过的号（新导入的在前）→ 读得最早的号。
用完的和签出的不算；limit 就是 limit，多取的行补齐被排除的之后截断。

加上 TouchSessions：把用过的会话写回去，「活跃会话数」才是个真的数
（规格 §6.8）。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/store/store.go internal/store/pool.go internal/store/pool_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 3: internal/pool —— 池

**Files:**
- Create: `internal/pool/pool.go`
- Create: `internal/pool/pool_test.go`

池是内存里的一个集合：账号行、剩余积分、耗尽标记。**它不放计划窗口** —— 选号用不上，放进去每个账号从 400 字节变成 2.2 KB。

**号按进池的顺序排**（规格 §6.9）：轮转从队首走到队尾，冷号先被挑；一个号出池再进池，它排到队尾。map 的顺序是随机的 —— 随机的顺序让轮转不是轮转。

- [ ] **Step 1: 写失败的测试**

新建 `internal/pool/pool_test.go`：
```go
package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// anAccount is an account that is signed in.
func anAccount(id string) store.Account {
	return store.Account{ID: id, Name: id, Status: store.StatusActive}
}

// TestThePoolTakesAccountsUntilItIsFull says the pool has an upper
// bound.
//
// 池的上界就是它存在的理由之一：库里的号再多，池里也只有那么多。
func TestThePoolTakesAccountsUntilItIsFull(t *testing.T) {
	p := New(2)
	if !p.Admit(Entry{Account: anAccount("a")}) {
		t.Fatal("the first account did not go in")
	}
	if !p.Admit(Entry{Account: anAccount("b")}) {
		t.Fatal("the second account did not go in")
	}
	if p.Admit(Entry{Account: anAccount("c")}) {
		t.Fatal("a full pool took another account")
	}
	if p.Len() != 2 {
		t.Fatalf("len = %d, want 2", p.Len())
	}
	if !p.Full() {
		t.Fatal("the pool is full")
	}
}

// TestAPoolWithoutACapTakesEverything says a capacity of zero means no
// limit.
func TestAPoolWithoutACapTakesEverything(t *testing.T) {
	p := New(0)
	for i := 0; i < 100; i++ {
		if !p.Admit(Entry{Account: anAccount(string(rune('a'+i%26)) + string(rune('0'+i/26)))}) {
			t.Fatalf("account %d did not go in", i)
		}
	}
	if p.Full() {
		t.Fatal("a pool without a cap is full")
	}
}

// TestAnAccountThatIsInThePoolIsNotTakenTwice says an account that is
// already in the pool keeps its place.
//
// 池里的号带着它读到的积分：换掉它就是把刚读到的数扔了。
func TestAnAccountThatIsInThePoolIsNotTakenTwice(t *testing.T) {
	p := New(4)
	if !p.Admit(Entry{Account: anAccount("a"), Remaining: 400, Known: true}) {
		t.Fatal("the account did not go in")
	}
	if p.Admit(Entry{Account: anAccount("a"), Remaining: 1, Known: true}) {
		t.Fatal("the account went in twice")
	}
	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account is not in the pool")
	}
	if e.Remaining != 400 {
		t.Fatalf("remaining = %v, want 400", e.Remaining)
	}
}

// TestASpentAccountIsNotUsable says an account whose window is spent
// cannot answer.
//
// 它的窗口什么时候回来，标记说了算：还在将来，它就接不了活（规格
// §4.2）。
func TestASpentAccountIsNotUsable(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.MarkSpent("a", "general", time.Now().Add(time.Hour), "quota")

	if p.Usable(time.Now()) != 0 {
		t.Fatalf("usable = %d, want 0", p.Usable(time.Now()))
	}
	spent := p.Spent()
	if len(spent) != 1 || spent[0].AccountID != "a" {
		t.Fatalf("spent = %v, want just a", spent)
	}
}

// TestASpentAccountWhoseTimeHasComeIsUsableAgain says a spent account
// comes back when its window comes back.
func TestASpentAccountWhoseTimeHasComeIsUsableAgain(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.MarkSpent("a", "general", time.Now().Add(-time.Minute), "quota")

	if p.Usable(time.Now()) != 1 {
		t.Fatalf("usable = %d, want 1", p.Usable(time.Now()))
	}
}

// TestAnAccountThatIsSignedOutIsNotUsable says an account that is not
// signed in cannot answer.
func TestAnAccountThatIsSignedOutIsNotUsable(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: store.Account{ID: "a", Status: store.StatusSignedOut}})

	if p.Usable(time.Now()) != 0 {
		t.Fatalf("usable = %d, want 0", p.Usable(time.Now()))
	}
}

// TestTheCreditsOfAnAccountThatWasNeverReadSayNothing says the credits
// that were never read say nothing.
//
// 不是 0 —— 0 是「读到了，没有了」（规格不变量 2）。
func TestTheCreditsOfAnAccountThatWasNeverReadSayNothing(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})

	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account is not in the pool")
	}
	if e.Known {
		t.Fatal("the credits of an account that was never read cannot be known")
	}
	if e.Remaining != 0 {
		t.Fatalf("remaining = %v, want 0", e.Remaining)
	}
}

// TestThePoolGivesItsAccounts says the pool gives the accounts it holds,
// and taking one out takes it away from the list.
func TestThePoolGivesItsAccounts(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})

	if len(p.Accounts()) != 2 {
		t.Fatalf("accounts = %d, want 2", len(p.Accounts()))
	}
	if !p.Remove("a") {
		t.Fatal("the account was not in the pool")
	}
	if p.Remove("a") {
		t.Fatal("the account was removed twice")
	}
	accounts := p.Accounts()
	if len(accounts) != 1 || accounts[0].ID != "b" {
		t.Fatalf("accounts = %v, want just b", accounts)
	}
	if _, ok := p.Get("a"); ok {
		t.Fatal("the removed account is still in the pool")
	}
}

// TestTheCreditsOfAnAccountAreWritten says the credits that were read
// later are written into the pool.
func TestTheCreditsOfAnAccountAreWritten(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.SetRemaining("a", 123, true)

	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account is not in the pool")
	}
	if !e.Known || e.Remaining != 123 {
		t.Fatalf("entry = %+v, want 123 known", e)
	}
}

// TestThePoolGivesItsAccountsInTheOrderTheyWentIn says the pool hands
// out its accounts in the order they went in.
//
// 顺序是给「轮转」用的：map 的顺序是随机的，随机的顺序让轮转不是轮转 ——
// 冷号（先进来的）该先被挑。
func TestThePoolGivesItsAccountsInTheOrderTheyWentIn(t *testing.T) {
	p := New(4)
	for _, id := range []string{"a", "b", "c"} {
		p.Admit(Entry{Account: anAccount(id)})
	}

	accounts := p.Accounts()
	if len(accounts) != 3 {
		t.Fatalf("accounts = %d, want 3", len(accounts))
	}
	for i, want := range []string{"a", "b", "c"} {
		if accounts[i].ID != want {
			t.Fatalf("accounts = %v, want a, b, c", accounts)
		}
	}
}

// TestAnAccountThatComesBackGoesToTheEnd says an account that left the
// pool and came back goes to the end of the order.
//
// 它刚用过：排到队尾，下一轮轮转才轮到它。
func TestAnAccountThatComesBackGoesToTheEnd(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})
	p.Remove("a")
	p.Admit(Entry{Account: anAccount("a")})

	accounts := p.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	if accounts[0].ID != "b" || accounts[1].ID != "a" {
		t.Fatalf("accounts = %v, want b then a", accounts)
	}
}

// TestThePoolIsSafeWhileItIsUsed says the pool can be read and written at
// the same time.
//
// 聊天那条路读它，后台补号那条路写它 —— 两边同时。这个包里说的「并发
// 安全」得有一道测试钉住，不然那只是注释里的一句话。
//
// 就算没有 -race（这台机器上没有 C 编译器），Go 自己会喊：并发读写同一
// 张 map 是 fatal error，跑一次就够。
func TestThePoolIsSafeWhileItIsUsed(t *testing.T) {
	p := New(64)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 一边写：进池、写余额、标记用完、出池。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := fmt.Sprintf("acc_%d", i%32)
			p.Admit(Entry{Account: anAccount(id)})
			p.SetRemaining(id, float64(i), true)
			p.MarkSpent(id, "general", time.Now().Add(time.Hour), "quota")
			p.Remove(id)
		}
	}()

	// 一边读：拿账号、看余额、数能答的。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.Get("acc_1")
			p.Entries()
			p.IDs()
			p.Accounts()
			p.Spent()
			p.Usable(time.Now())
			p.Len()
			p.Full()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestTheCapacityOfZeroMeansNoLimit says a capacity of zero is no limit,
// whatever is in the pool.
//
// 0 = 不限，是池空的时候也是池里有号的时候。同一个 0 在两种状态下是两种
// 意思的话，池会被永久冻住。
func TestTheCapacityOfZeroMeansNoLimit(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})

	p.SetCapacity(0)

	if p.Capacity() != 0 {
		t.Fatalf("capacity = %d, want 0", p.Capacity())
	}
	if p.Full() {
		t.Fatal("the pool is full, want not full")
	}
	if !p.Admit(Entry{Account: anAccount("c")}) {
		t.Fatal("the account did not go into the pool")
	}
}

// TestTheCapacityDoesNotTakeAccountsOut says a smaller capacity does not
// throw accounts away.
//
// 缩容请不走任何号：请出去的理由只有一个 —— 额度用完了（规格 §4.4）。池里
// 超了就是超了，等号一个个地被用完。
func TestTheCapacityDoesNotTakeAccountsOut(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.Admit(Entry{Account: anAccount("b")})

	p.SetCapacity(1)

	if p.Len() != 2 {
		t.Fatalf("pool = %d, want 2", p.Len())
	}
	if p.Capacity() != 1 {
		t.Fatalf("capacity = %d, want 1", p.Capacity())
	}
}

// TestTheSpentListDropsTheExpiredOnes says a spent window that has come
// back is not in the spent list.
//
// 窗口回来了，那个号就回来了（规格 §4.5）：名单里还留着它，挑号就会跳过
// 一个能用的号。
func TestTheSpentListDropsTheExpiredOnes(t *testing.T) {
	p := New(4)
	p.Admit(Entry{Account: anAccount("a")})
	p.MarkSpent("a", "general", time.Now().Add(-time.Minute), "quota")

	if spent := p.Spent(); len(spent) != 0 {
		t.Fatalf("spent = %v, want none", spent)
	}
}

// TestTheNamesAreCopies says the caller cannot break the pool by changing
// the list it was given.
//
// 顺序是轮转靠的东西：rotator 拿着名单从队首走到队尾，冷号先被挑。谁改
// 了顺序，轮转就不是轮转了。
func TestTheNamesAreCopies(t *testing.T) {
	p := New(4)
	for _, id := range []string{"a", "b", "c"} {
		p.Admit(Entry{Account: anAccount(id)})
	}

	accounts := p.Accounts()
	accounts[0], accounts[2] = accounts[2], accounts[0]
	accounts[1].Status = store.StatusSignedOut

	again := p.Accounts()
	if again[0].ID != "a" || again[1].ID != "b" || again[2].ID != "c" {
		t.Fatalf("accounts = %v, want a, b, c", again)
	}
	if again[1].Status != store.StatusActive {
		t.Fatalf("status = %q, want %q", again[1].Status, store.StatusActive)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/pool/
```

Expected: FAIL —— `package pool` 里没有 `Pool`（这个包还不存在）。

- [ ] **Step 3: 写实现**

新建 `internal/pool/pool.go`：
```go
// Package pool holds the accounts the gateway keeps in memory.
//
// 池是缓存，不是真相：账号、耗尽标记、会话绑定的真相都在库里。池丢了，
// 重启后从库里重建就行（规格不变量 1）。
//
// 池存在的理由有两个：
//
//   - 选号不该每次都把整张账号表读一遍。1000 个号就是 1000 行 × 每个
//     请求，池把它变成一次内存里的查表。
//   - 池有上界。库里的号再多，池里也只有「并发 + 会话」那么多个。
//
// 池里放的是选号要用的三样：账号行、剩余积分、耗尽标记。计划窗口
// （windows_json）不放 —— 选号用不上它，放进去每个账号从 400 字节变成
// 2.2 KB，内存账翻五倍。
package pool

import (
	"sync"
	"time"

	"mmxcode/internal/store"
)

// BytesPerAccount is what one account in the pool takes, in bytes.
//
// 它是量出来的：账号行（id/name/user_id/email/status 五个字符串）加上
// 剩余积分、耗尽标记和 map 的索引，一行 395 字节。这里取 512 —— 宁可高
// 一点，内存预算算得保守些。
//
// 池里不留第二份账号行：名单是每次现给的那一份（见 Accounts）。
const BytesPerAccount = 512

// Entry is one account in the pool.
type Entry struct {
	// Account is the account itself.
	Account store.Account
	// Remaining is how many credits are left. It says something only
	// when Known is true.
	Remaining float64
	// Known says the credits have been read.
	//
	// 它是 false 的时候 Remaining 说不了话：这个号还没被读过。不是 0
	// —— 0 是「读到了，没有了」。
	Known bool
	// Spent says the account's window is spent.
	Spent bool
	// SpentUntil is when the window comes back.
	SpentUntil time.Time
	// SpentGroup is the window group that is spent.
	SpentGroup string
	// SpentReason is why the window is spent.
	SpentReason string
}

// Pool is the set of accounts the gateway keeps in memory.
//
// 它是并发安全的：聊天那条路读它，后台补号那条路写它。
//
// 池要用 New 造：零值的池没有那张表，往里放东西会 panic。
//
// 号按进池的顺序排：轮转从队首走到队尾，冷号（先进来的）先被挑。
type Pool struct {
	mu   sync.RWMutex
	byID map[string]*Entry
	cap  int
	// order is the accounts in the order they went in.
	//
	// 顺序就是进池的顺序：轮转的时候从队首走到队尾，冷号（先进来的）
	// 先被挑。一个号出池再进池，它排到队尾 —— 它刚用过。
	//
	// 它是给「轮转」用的：map 的顺序是随机的，随机的顺序让轮转不是轮转。
	order []string
}

// New makes a pool that may hold capacity accounts.
//
// A capacity of zero means no limit.
func New(capacity int) *Pool {
	if capacity < 0 {
		capacity = 0
	}
	return &Pool{byID: map[string]*Entry{}, cap: capacity}
}

// Capacity is how many accounts the pool may hold.
//
// 0 means no limit.
func (p *Pool) Capacity() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cap
}

// SetCapacity sets how many accounts the pool may hold.
//
// 0 means no limit. It never takes accounts out: the pool shrinks by
// accounts being spent, not by being told to.
//
// 所以缩容不会请走任何号：池里已经有 40 个的时候调 SetCapacity(10)，池里
// 还是 40 个 —— 但也进不去新的了（满了），要等号一个个地被用完。请出去
// 的理由只有一个：额度用完了（规格 §4.4）。
//
// 0 = 不限，不管池里现在有几个。
func (p *Pool) SetCapacity(n int) {
	if n < 0 {
		n = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cap = n
}

// Len is how many accounts the pool holds.
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.byID)
}

// Full says the pool cannot take one more account.
func (p *Pool) Full() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cap > 0 && len(p.byID) >= p.cap
}

// Admit puts an account into the pool.
//
// It says whether the account went in. It does not go in when the pool
// is full, or when it is already in.
//
// 一个已经在池里的号不会被换掉：池里的号带着它读到的积分，换掉它就是把
// 刚读到的数扔了。
func (p *Pool) Admit(e Entry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cap > 0 && len(p.byID) >= p.cap {
		return false
	}
	if _, ok := p.byID[e.Account.ID]; ok {
		return false
	}
	entry := e
	p.byID[e.Account.ID] = &entry
	p.order = append(p.order, e.Account.ID)
	return true
}

// Remove takes an account out of the pool.
//
// It says whether the account was in the pool.
//
// 一个号出池只有一个理由：额度用完了（规格 §4.4）。别的理由都不出池 ——
// 「一阵子没用」不是理由：会话钉在号上，把它换出去，缓存命中就没了。
func (p *Pool) Remove(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byID[accountID]; !ok {
		return false
	}
	delete(p.byID, accountID)
	p.dropFromOrder(accountID)
	return true
}

// dropFromOrder takes an account out of the order.
//
// 顺序是进池的顺序：出池的号从顺序里去掉，它再进来的时候排到队尾。
func (p *Pool) dropFromOrder(accountID string) {
	for i, id := range p.order {
		if id != accountID {
			continue
		}
		p.order = append(p.order[:i], p.order[i+1:]...)
		return
	}
}

// Get gives one entry of the pool.
func (p *Pool) Get(accountID string) (Entry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byID[accountID]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Entries gives every entry of the pool, in the order they went in.
//
// 给的是一份新的：拿着它的人可以随便改 —— 池的那一份不会被改坏。
func (p *Pool) Entries() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Entry, 0, len(p.order))
	for _, id := range p.order {
		e, ok := p.byID[id]
		if !ok {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// IDs gives the ids of the accounts in the pool.
func (p *Pool) IDs() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]bool, len(p.byID))
	for id := range p.byID {
		out[id] = true
	}
	return out
}

// MarkSpent marks an account's window spent.
//
// 标记的是「这个窗口用完了，什么时候回来」：池里的号照旧留着，只是它
// 不再能被挑到 —— 挑号的那一套会看 Spent 和 SpentUntil。
//
// reason 是为什么用完的（现在是 "quota"）：它跟着名单一起给出去，别的地方
// 看得见为什么这个号被搁下了。
func (p *Pool) MarkSpent(accountID, windowKey string, until time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byID[accountID]
	if !ok {
		return
	}
	e.Spent = true
	e.SpentGroup = windowKey
	e.SpentUntil = until
	e.SpentReason = reason
}

// SetRemaining writes the credits of an account that is in the pool.
//
// 一个不在池里的号没有地方可写：它不是池的成员，它的数跟挑号无关。
func (p *Pool) SetRemaining(accountID string, left float64, known bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byID[accountID]
	if !ok {
		return
	}
	e.Remaining = left
	e.Known = known
}

// Usable is how many accounts in the pool can answer right now.
//
// 能答 = 签入中（status 是 active）而且它的窗口没有用完。窗口的时刻已
// 经过去的标记不算用完 —— 那个号回来了（规格 §4.5）。
func (p *Pool) Usable(now time.Time) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.byID {
		if e.Account.Status != store.StatusActive {
			continue
		}
		if e.Spent && e.SpentUntil.After(now) {
			continue
		}
		n++
	}
	return n
}

// Accounts gives the accounts in the pool, in the order they went in.
//
// 给的是一份新的：拿着它的人可以随便改 —— 池的那一份不会被改坏。
//
// **顺序是轮转靠的东西**：rotator 拿着这份名单从队首走到队尾，冷号先被
// 挑。谁改了顺序，轮转就不是轮转了 —— 所以池给出去的每一份都是它自己的
// 副本，改了也不影响池。
func (p *Pool) Accounts() []store.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]store.Account, 0, len(p.order))
	for _, id := range p.order {
		e, ok := p.byID[id]
		if !ok {
			continue
		}
		out = append(out, e.Account)
	}
	return out
}

// Spent gives the accounts in the pool whose window is spent.
//
// 过期的不算：窗口回来了，那个号就回来了（规格 §4.5）。
//
// 给的是一份新的：拿着它的人可以随便改。
func (p *Pool) Spent() []store.Exhausted {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make([]store.Exhausted, 0, len(p.byID))
	for _, id := range p.order {
		e, ok := p.byID[id]
		if !ok || !e.Spent {
			continue
		}
		if !e.SpentUntil.After(now) {
			continue
		}
		out = append(out, store.Exhausted{
			AccountID: e.Account.ID,
			WindowKey: e.SpentGroup,
			Until:     e.SpentUntil,
			Reason:    e.SpentReason,
		})
	}
	return out
}
```

- [ ] **Step 4: 跑测试，看它过**
```sh
go test ./internal/pool/
```

Expected: PASS。

- [ ] **Step 5: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(pool): 内存里的账号池 —— 有上界，只放选号要用的三样

账号行、剩余积分、耗尽标记。计划窗口不放：选号用不上它，放进去内存账
从 395 字节变 2.2 KB（规格 §5.5）。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/pool/pool.go internal/pool/pool_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 4: internal/pool —— keeper（填池、补号）

**Files:**
- Create: `internal/pool/keeper.go`
- Create: `internal/pool/keeper_test.go`

两个动作，代价差得很远：

- **Seed**（启动时填池）：**不问站点**，用库里最后读到的数顶上。读额度是 2~11 个上游请求，填满 400 个号要半个多小时，而网关要立刻能用（规格 §6.3）。旧数只是高估 —— 撞空时换号重试会把标记写准，自愈。
- **Replenish**（缺了补上）：补进来的号**读一次额度**。读到的数说它有额度，它才进池；读到「用完了」，它不进池，而且被标记掉。

- [ ] **Step 1: 写失败的测试**

新建 `internal/pool/keeper_test.go`：
```go
package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// fakeSource is a set of candidates.
type fakeSource struct {
	mu sync.Mutex
	// all are the candidates it knows.
	all []store.Candidate
	// asked counts the questions it was asked.
	asked int
	// err is what it answers with, when it is set.
	err error
}

func (f *fakeSource) Candidates(_ context.Context, limit int, exclude map[string]bool) ([]store.Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]store.Candidate, 0, limit)
	for _, c := range f.all {
		if exclude[c.Account.ID] {
			continue
		}
		out = append(out, c)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// fakeReader reads credits and counts what it was asked.
type fakeReader struct {
	mu sync.Mutex
	// left is how many credits each account has.
	left map[string]float64
	// err is what it answers with, when it is set.
	err error
	// asked counts the accounts it was asked about.
	asked int
}

func (f *fakeReader) RefreshUsage(_ context.Context, accountID string) (store.UsageSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.err != nil {
		return store.UsageSnapshot{}, f.err
	}
	left, ok := f.left[accountID]
	if !ok {
		return store.UsageSnapshot{AccountID: accountID}, nil
	}
	return store.UsageSnapshot{
		AccountID:  accountID,
		Granted:    400,
		Remaining:  left,
		HasAmounts: true,
	}, nil
}

// fakeMarker records the marks that were written.
type fakeMarker struct {
	mu sync.Mutex
	// marks are the marks that were written.
	marks []store.Exhausted
}

func (f *fakeMarker) SetExhausted(_ context.Context, accountID, windowKey string, until time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, store.Exhausted{
		AccountID: accountID, WindowKey: windowKey, Until: until, Reason: reason,
	})
	return nil
}

// TestTheSeedFillsThePoolWithoutAskingTheSite says the pool is filled
// at startup without asking the site for anything.
//
// 读额度是 2~11 个上游请求，填满 400 个号要半个多小时，而网关要立刻能
// 用（规格 §6.3）。旧数只是高估 —— 撞空时换号重试会把标记写准，自愈。
func TestTheSeedFillsThePoolWithoutAskingTheSite(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{
		{Account: anAccount("a"), Remaining: 400, Known: true},
		{Account: anAccount("b"), Remaining: 400, Known: true},
	}}
	reader := &fakeReader{left: map[string]float64{"a": 400, "b": 400}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

	n, err := k.Seed(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("seeded = %d, want 2", n)
	}
	if p.Len() != 2 {
		t.Fatalf("pool = %d, want 2", p.Len())
	}
	if reader.asked != 0 {
		t.Fatalf("the site was asked %d times, want 0", reader.asked)
	}
}

// TestTheReplenishReadsTheCreditsOfANewAccount says a new account's
// credits are read before it goes into the pool.
//
// 读到的数说它有额度，它才进池：读不出来的不进池（它没有数，挑号也挑不
// 到它）。
func TestTheReplenishReadsTheCreditsOfANewAccount(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{left: map[string]float64{"a": 250}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replenished = %d, want 1", n)
	}
	if reader.asked != 1 {
		t.Fatalf("the site was asked %d times, want 1", reader.asked)
	}
	e, ok := p.Get("a")
	if !ok {
		t.Fatal("the account did not go into the pool")
	}
	if !e.Known || e.Remaining != 250 {
		t.Fatalf("entry = %+v, want 250 known", e)
	}
}

// TestAnAccountThatReadsAsSpentIsMarkedAndLeftOut says an account that
// reads as spent does not go into the pool, and is marked.
//
// 它留在库里占着一个位置：下次补号不会再挑它（规格 §6.5）。
func TestAnAccountThatReadsAsSpentIsMarkedAndLeftOut(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{left: map[string]float64{"a": 0}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
	if len(marker.marks) != 1 || marker.marks[0].AccountID != "a" {
		t.Fatalf("marks = %v, want just a", marker.marks)
	}
}

// TestAnAccountThatCannotBeReadDoesNotGoIntoThePool says an account
// whose credits cannot be read does not go into the pool.
//
// 不知道有没有额度，就别占着池里的位置：它的数说不了话，挑号也挑不到它。
func TestAnAccountThatCannotBeReadDoesNotGoIntoThePool(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{err: errors.New("the site is not answering")}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
	if len(marker.marks) != 0 {
		t.Fatalf("marks = %v, want none", marker.marks)
	}
}

// TestTheReplenishDoesNotRunTwiceAtOnce says two replenishments do not
// run at the same time.
//
// 一次只跑一趟：池里的号不会因为跑两趟就多出来，而上游的请求会翻倍。
func TestTheReplenishDoesNotRunTwiceAtOnce(t *testing.T) {
	p := New(10)
	src := &fakeSource{all: []store.Candidate{{Account: anAccount("a")}}}
	reader := &fakeReader{left: map[string]float64{"a": 400}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

	// The first one holds the keeper busy while the second one comes.
	k.mu.Lock()
	k.running = true
	k.mu.Unlock()
	n, err := k.Replenish(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if reader.asked != 0 {
		t.Fatalf("the site was asked %d times, want 0", reader.asked)
	}
}

// TestTheReplenishStopsWhenThereIsNothingToTake says a pool that cannot
// be filled is not filled with rubbish.
func TestTheReplenishStopsWhenThereIsNothingToTake(t *testing.T) {
	p := New(10)
	src := &fakeSource{}
	reader := &fakeReader{left: map[string]float64{}}
	marker := &fakeMarker{}
	k := NewKeeper(p, src, reader, marker, true, time.Minute)

	n, err := k.Replenish(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("replenished = %d, want 0", n)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/pool/
```

Expected: FAIL —— `undefined: NewKeeper`。

（整包跑，别用 `-run` 筛名字：keeper 的六个测试名里一个都没有 "Keeper"，
筛了就一个都不跑，红的绿的都看不见。）

- [ ] **Step 3: 写实现**

新建 `internal/pool/keeper.go`：
```go
package pool

import (
	"context"
	"sync"
	"time"

	"mmxcode/internal/store"
)

// Source gives the accounts that could be admitted to the pool.
type Source interface {
	// Candidates gives the accounts that are not in the pool, coldest
	// first. The exclude map holds the ids that are in it already; a
	// source that does not need it may ignore it.
	Candidates(ctx context.Context, limit int, exclude map[string]bool) ([]store.Candidate, error)
}

// Reader reads an account's credits.
type Reader interface {
	// RefreshUsage reads an account's credits and saves them.
	RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error)
}

// Marker marks an account's window spent.
type Marker interface {
	// SetExhausted marks an account's window spent. The order is the
	// account, the window key, the time it comes back and the reason.
	SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
}

// Keeper keeps the pool full.
//
// 两个动作：
//
//   - Seed：启动时填池。**不问站点** —— 用库里最后读到的数顶上，等不
//     起（读额度是 2~11 个上游请求，填满 400 个号要半个多小时）。
//   - Replenish：池里少了号，补进来。补进来的号**读一次额度**：读不
//     到就别进池，读到「用完了」就把它标记掉，换下一个。
type Keeper struct {
	pool   *Pool
	source Source
	reader Reader
	marker Marker
	// read says whether a newly admitted account's credits are read
	// before it goes in.
	read bool
	// cooldown is how long an account that reads as spent is given up
	// on when the reading says no moment.
	cooldown time.Duration
	// mu guards running.
	mu sync.Mutex
	// running says a replenishment is in the air.
	//
	// 一次只跑一趟：补号是上游的活，跑两趟就是两倍的请求，而池里的
	// 号不会因为跑两趟就多出来。
	running bool
}

// NewKeeper makes a keeper.
func NewKeeper(p *Pool, source Source, reader Reader, marker Marker, read bool, cooldown time.Duration) *Keeper {
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	return &Keeper{
		pool:     p,
		source:   source,
		reader:   reader,
		marker:   marker,
		read:     read,
		cooldown: cooldown,
	}
}

// Pool gives the pool the keeper fills.
func (k *Keeper) Pool() *Pool { return k.pool }

// Seed fills the pool with what the database knows, without asking the
// site for anything.
//
// It says how many accounts went in.
//
// 冷启动用这个：池要立刻能用，而读额度等不起（一次读是 2~11 个上游请
// 求）。旧数只是高估 —— 撞空时换号重试会把标记写准，自愈。
func (k *Keeper) Seed(ctx context.Context, want int) (int, error) {
	if want <= 0 {
		return 0, nil
	}
	candidates, err := k.source.Candidates(ctx, want, k.pool.IDs())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range candidates {
		if k.pool.Len() >= want {
			break
		}
		if !k.pool.Admit(Entry{
			Account:   c.Account,
			Remaining: c.Remaining,
			Known:     c.Known,
		}) {
			continue
		}
		n++
	}
	return n, nil
}

// Replenish puts accounts into the pool until it holds want of them.
//
// It says how many accounts went in.
//
// 补进来的号读一次额度：读到的数说有额度，它进池；读到「用完了」，它
// 不进池，而且被标记掉（它留在库里占着一个位置，下次补号不会再挑它）。
//
// 一次只跑一趟：已经在跑的那一趟没有跑完，这一次就不跑 —— 池里的号不
// 会因为跑两趟就多出来，而上游的请求会翻倍。
func (k *Keeper) Replenish(ctx context.Context, want int) (int, error) {
	if want <= 0 {
		return 0, nil
	}
	k.mu.Lock()
	if k.running {
		k.mu.Unlock()
		return 0, nil
	}
	k.running = true
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		k.running = false
		k.mu.Unlock()
	}()

	added := 0
	var lastErr error
	// 一趟最多试这么多候选：候选都读不出东西来的时候，别把整张表试
	// 完 —— 试到没有新的候选为止。
	for attempts := 0; attempts < want*3+3; attempts++ {
		if k.pool.Len() >= want {
			break
		}
		candidates, err := k.source.Candidates(ctx, want+len(k.pool.IDs()), k.pool.IDs())
		if err != nil {
			lastErr = err
			break
		}
		if len(candidates) == 0 {
			break
		}
		progressed := false
		for _, c := range candidates {
			if k.pool.Len() >= want {
				break
			}
			if k.pool.IDs()[c.Account.ID] {
				continue
			}
			if !k.read {
				if k.pool.Admit(Entry{
					Account:   c.Account,
					Remaining: c.Remaining,
					Known:     c.Known,
				}) {
					added++
					progressed = true
				}
				continue
			}
			snap, err := k.reader.RefreshUsage(ctx, c.Account.ID)
			if err != nil {
				// 读不到：不知道有没有额度，就别进池（进池了挑号也挑不
				// 到它 —— 没读到数的号排在最后）。这一趟就当它不合格。
				continue
			}
			if snap.HasAmounts && snap.Remaining <= 0 {
				// 读到了：它没有额度。标记掉，别让它占着位置。
				k.markSpent(ctx, c.Account.ID, snap)
				continue
			}
			if k.pool.Admit(Entry{
				Account:   c.Account,
				Remaining: snap.Remaining,
				Known:     snap.HasAmounts,
			}) {
				added++
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return added, lastErr
}

// markSpent writes the mark of an account that reads as spent.
//
// 窗口键是空的：那是账号级的标记，对所有窗口组都算 —— 读到的积分是零，
// 说的不是「这个模型组用完了」，是「这个号没有额度了」。
//
// 什么时候回来：读到的时间在将来就用它，取不到就用冷却时间。
func (k *Keeper) markSpent(ctx context.Context, accountID string, snap store.UsageSnapshot) {
	until := time.Time{}
	if snap.ExpiresAt.After(time.Now()) {
		until = snap.ExpiresAt
	}
	if until.IsZero() {
		until = time.Now().Add(k.cooldown)
	}
	if k.marker == nil {
		return
	}
	_ = k.marker.SetExhausted(ctx, accountID, "", until, "quota")
}
```

- [ ] **Step 4: 跑测试，看它过**
```sh
go test ./internal/pool/
```

Expected: PASS。

- [ ] **Step 5: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(pool): keeper —— 启动填池不等读，缺号了补进去并读一次额度

Seed 不问站点：读额度是 2~11 个上游请求，填满 400 个号要半个多小时，
而网关要立刻能用。Replenish 补号时读一次：读到有额度才进池，读到用完
了就不进池而且标记掉。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/pool/keeper.go internal/pool/keeper_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 5: 网关走池 —— 适配器从池里读

**Files:**
- Modify: `internal/gateway/gateway.go`
- Create: `internal/gateway/pool.go`
- Create: `internal/gateway/pool_test.go`
- Create: `internal/gateway/touch.go`
- Create: `internal/gateway/touch_test.go`
- Create: `internal/gateway/fallback.go`
- Create: `internal/gateway/fallback_test.go`

网关的 rotator 现在靠一个 `poolAdapter` 从「库」拿账号、耗尽标记和余额。这一步把适配器的来源换成池：**池里有就读池，池是空的还是从库里读** —— 网关不能因为池空着就一个请求都答不了（规格不变量 1）。

**池空的时候从库里读，读出来的结果缓存一秒**（规格 §6.9）：账号、耗尽标记、余额这三样都是全表读 —— 1000 个号就是 2.6MB，一个请求一次。几十个并发就是一波几百 MB。缓存一秒，那一波只读一次。

这一任务还带上**会话活跃**那件事（规格 §6.8）：网关把用过的会话写回库，「活跃会话数」才是个真的数；填池时最近活跃的对话绑着的号排在最前，池子才留得住它们。

- [ ] **Step 1: 写失败的测试**

新建 `internal/gateway/pool_test.go`：
```go
package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// openGatewayStore opens a store for a test.
func openGatewayStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/pool.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// listingStore counts the times the whole table was read.
//
// 它是包在真库外面的：里面的方法都是真的，只是多了两个计数器。
type listingStore struct {
	*store.Store
	// listed is how many times the whole table was read.
	listed int
}

func (c *listingStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	c.listed++
	return c.Store.ListAccounts(ctx)
}

func (c *listingStore) ListExhausted(ctx context.Context) ([]store.Exhausted, error) {
	c.listed++
	return c.Store.ListExhausted(ctx)
}

// TestThePickerReadsThePoolAndNotTheWholeTable says picking an account
// reads the pool, not the whole table.
//
// 现在每个聊天请求都把账号表和耗尽标记表读一遍（规格 §6.2）：1000 个
// 号就是 1000 行 × 每个请求。池把它变成一次内存里的查表。
func TestThePickerReadsThePoolAndNotTheWholeTable(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	p := pool.New(10)
	p.Admit(pool.Entry{
		Account:   store.Account{ID: "acc_01", Status: store.StatusActive},
		Remaining: 400,
		Known:     true,
	})
	cs := &listingStore{Store: s}
	ad := poolAdapter{s: cs, pool: p}

	accounts, err := ad.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acc_01" {
		t.Fatalf("accounts = %v, want just acc_01", accounts)
	}
	if _, err := ad.Exhausted(ctx); err != nil {
		t.Fatal(err)
	}
	if cs.listed != 0 {
		t.Fatalf("the whole table was read %d times, want 0", cs.listed)
	}
}

// TestAnEmptyPoolFallsBackToTheDatabase says a gateway whose pool is
// empty still works.
//
// 池是空的（还没填上，或者填池失败）的时候，网关不能一个请求都答不了。
func TestAnEmptyPoolFallsBackToTheDatabase(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	cs := &listingStore{Store: s}
	ad := poolAdapter{s: cs, pool: pool.New(10)}

	accounts, err := ad.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) == 0 {
		t.Fatal("the database was not read")
	}
	if cs.listed == 0 {
		t.Fatal("the database was not read")
	}
}

// TestASpentAccountLeavesThePool says an account whose window is spent
// goes out of the pool.
//
// 它接不了活了，让位给一个冷的（规格 §4.4）。标记照旧写进库里。
func TestASpentAccountLeavesThePool(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	ad := poolAdapter{s: s, pool: p}

	until := time.Now().Add(time.Hour)
	if err := ad.MarkExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 0 {
		t.Fatalf("pool = %d, want 0", p.Len())
	}
	marks, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 1 || marks[0].AccountID != "acc_01" {
		t.Fatalf("marks = %v, want just acc_01", marks)
	}
}
```

「承诺的并发按池夹」那两个测试不在这里 —— 它们测的是 Task 6 改的
`totalConcurrency`，写在这里的话这一步就绿不了了。它们在 Task 6 的
`internal/gateway/concurrency_test.go` 里。

新建 `internal/gateway/fallback_test.go`：
```go
package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// readCountingStore counts the reads of the database.
//
// 它数的是三样：账号、耗尽标记、余额快照 —— 池空的时候适配器问的就是这
// 三样，每一样都是全表读。
//
// 它数的是三样：账号、耗尽标记、余额快照 —— 池空的时候适配器问的就是这
// 三样，每一样都是全表读。
type readCountingStore struct {
	*store.Store
	accounts  int
	exhausted int
	snapshots int
}

func (c *readCountingStore) ListAccounts(ctx context.Context) ([]store.Account, error) {
	c.accounts++
	return c.Store.ListAccounts(ctx)
}

func (c *readCountingStore) ListExhausted(ctx context.Context) ([]store.Exhausted, error) {
	c.exhausted++
	return c.Store.ListExhausted(ctx)
}

func (c *readCountingStore) LatestUsageSnapshots(ctx context.Context) ([]store.UsageSnapshot, error) {
	c.snapshots++
	return c.Store.LatestUsageSnapshots(ctx)
}

// oneAccount makes a store with one signed-in account.
func oneAccount(t *testing.T) (*store.Store, *readCountingStore) {
	t.Helper()
	s := openGatewayStore(t)
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return s, &readCountingStore{Store: s}
}

// TestTheEmptyPoolReadsTheDatabaseOnce says the database is read once
// for a batch of questions while the pool is empty.
//
// 池空的时候适配器回到从前的样子，从库里拿账号、标记和余额。那三样都是
// 全表读：一个请求一次就是 2.6MB 的垃圾，几十个并发就是一波几百 MB。
// 缓存一秒，那一波只读一次。
func TestTheEmptyPoolReadsTheDatabaseOnce(t *testing.T) {
	_, cs := oneAccount(t)
	ctx := context.Background()
	ad := poolAdapter{s: cs, fallback: newDBFallback(cs, time.Minute)}

	for i := 0; i < 3; i++ {
		if _, err := ad.Accounts(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := ad.Exhausted(ctx); err != nil {
			t.Fatal(err)
		}
		ad.RemainingAll(ctx, []string{"acc_01"})
	}

	if cs.accounts != 1 {
		t.Fatalf("the accounts were read %d times, want 1", cs.accounts)
	}
	if cs.exhausted != 1 {
		t.Fatalf("the exhausted marks were read %d times, want 1", cs.exhausted)
	}
	if cs.snapshots != 1 {
		t.Fatalf("the snapshots were read %d times, want 1", cs.snapshots)
	}
}

// TestTheEmptyPoolReadsTheDatabaseAgainAfterTheWindow says the cached
// answer does not last forever.
//
// 缓存久了就 stale：刚被标记成「用完」的号还看得出能用。窗口一小，它
// 就重新读一次。
func TestTheEmptyPoolReadsTheDatabaseAgainAfterTheWindow(t *testing.T) {
	_, cs := oneAccount(t)
	ctx := context.Background()
	ad := poolAdapter{s: cs, fallback: newDBFallback(cs, time.Nanosecond)}

	if _, err := ad.Accounts(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := ad.Accounts(ctx); err != nil {
		t.Fatal(err)
	}

	if cs.accounts != 2 {
		t.Fatalf("the accounts were read %d times, want 2", cs.accounts)
	}
}

// TestTheFilledPoolDoesNotReadTheDatabase says the pool answers and the
// database is left alone.
//
// 池里有号的时候，那三样都从池里拿 —— 这就是池存在的理由。
func TestTheFilledPoolDoesNotReadTheDatabase(t *testing.T) {
	_, cs := oneAccount(t)
	ctx := context.Background()
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	ad := poolAdapter{s: cs, pool: p, fallback: newDBFallback(cs, time.Minute)}

	accounts, err := ad.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acc_01" {
		t.Fatalf("accounts = %v, want just acc_01", accounts)
	}
	if _, err := ad.Exhausted(ctx); err != nil {
		t.Fatal(err)
	}
	if cs.accounts != 0 || cs.exhausted != 0 || cs.snapshots != 0 {
		t.Fatalf("the database was read: %d accounts, %d marks, %d snapshots",
			cs.accounts, cs.exhausted, cs.snapshots)
	}
}
```

新建 `internal/gateway/touch_test.go`：
```go
package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// TestTheUsedSessionsAreWritten says a session that was used is written
// back into the database.
//
// 「用过」是活跃会话唯一的凭据：网关不问上游，它只知道这段对话被用过
// （规格 §6.3）。不写回去，池子就不会留着它绑着的号。
func TestTheUsedSessionsAreWritten(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "a-conversation", "acc_01"); err != nil {
		t.Fatal(err)
	}
	g := &Server{Store: s, Cfg: config.Default(), touches: newSessionTouches()}

	// 这个会话两小时前用过，之后没人碰它。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	g.touches.add("a-conversation")
	g.flushTouches(ctx)
	s.Clock = time.Now

	n, err := s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recent sessions = %d, want 0", n)
	}

	// 刚用过：它是活跃的。
	g.touches.add("a-conversation")
	g.flushTouches(ctx)
	n, err = s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recent sessions = %d, want 1", n)
	}
}

// TestTheUsedSessionsAreWrittenOnlyOnce says a session that was used
// twice is written once.
//
// 写的是「用过没有」，不是「用了几次」。
func TestTheUsedSessionsAreWrittenOnlyOnce(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "a-conversation", "acc_01"); err != nil {
		t.Fatal(err)
	}
	g := &Server{Store: s, Cfg: config.Default(), touches: newSessionTouches()}

	g.touches.add("a-conversation")
	g.touches.add("a-conversation")
	keys := g.touches.take()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want one", keys)
	}
	// 拿了就是归网关了：下一批不该再有它。
	if again := g.touches.take(); len(again) != 0 {
		t.Fatalf("keys = %v, want none", again)
	}
}

// TestTheAccountAConversationIsBoundToComesFirst says the account a live
// conversation is bound to is the first one the pool takes.
//
// 会话钉在号上，钉住的号不在池里就会被改绑，缓存命中就没了（规格不变
// 量 3）。
func TestTheAccountAConversationIsBoundToComesFirst(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_busy", "acc_quiet"} {
		if err := s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 绑着的那号是被用过的：它有读数，是热的。
	if err := s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_busy", Granted: 400, Remaining: 400, HasAmounts: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "a-conversation", "acc_busy"); err != nil {
		t.Fatal(err)
	}

	src := poolSource{s: s, window: time.Hour}
	cands, err := src.Candidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].Account.ID != "acc_busy" {
		t.Fatalf("candidates = %v, want acc_busy first", cands)
	}
}

// TestWithoutAWindowNoConversationHoldsAPlace says a gateway with no
// session window counts no conversation as active.
//
// 窗口是「多久算活跃」的那把尺子：没有它就没有「活跃」，一个号也不因
// 为对话而排在前面。
func TestWithoutAWindowNoConversationHoldsAPlace(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_busy", "acc_quiet"} {
		if err := s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_busy", Granted: 400, Remaining: 400, HasAmounts: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 这个会话两小时前绑的：没有窗口的时候它不算活跃。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	if err := s.BindSession(ctx, "a-conversation", "acc_busy"); err != nil {
		t.Fatal(err)
	}
	s.Clock = time.Now

	src := poolSource{s: s}
	cands, err := src.Candidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].Account.ID != "acc_quiet" {
		t.Fatalf("candidates = %v, want acc_quiet first", cands)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/gateway/ -run "Pool|WholeTable|Used|Conversation"
```

Expected: FAIL —— `poolAdapter` 没有 `pool` 字段，`Server` 没有 `Pool` 字段，`sessionTouches` 不存在，`newDBFallback` 不存在。

（过滤器里有 `Pool`：`TestTheEmptyPoolReadsTheDatabaseOnce`、`TestTheFilledPoolDoesNotReadTheDatabase` 这几个都带 Pool，别改窄了 —— 筛掉了就一个都不跑，红的绿的都看不见。）

- [ ] **Step 3: 网关里挂上池**

`internal/gateway/gateway.go`，import 里加：
```go
	"mmxcode/internal/pool"
```

`internal/gateway/gateway.go`，`Server` 里加三个字段：
```go
	// Pool is the accounts the gateway keeps in memory.
	//
	// 池是缓存，不是真相（规格不变量 1）：账号、耗尽标记、会话绑定的真
	// 相都在库里。池存在的理由是选号不用每次扫全表，还有内存有个上界。
	Pool *pool.Pool
	// Keeper fills the pool: at startup with what the database knows,
	// and later with the accounts that are needed.
	Keeper *pool.Keeper
	// touches are the sessions that were used and not written yet.
	//
	// 它们是「活跃会话」唯一的凭据（规格 §6.3）：会话用过没有，只有网
	// 关知道。攒着写回去，写一次算一批。
	touches *sessionTouches
	// fallback is the database's answer, cached, for the time the pool
	// is empty.
	//
	// 池空的时候要从库里拿账号、标记和余额 —— 那三样都是全表读（规格
	// §6.9）。缓存一秒，让那一波并发只读一次。
	fallback *dbFallback
```

`internal/gateway/gateway.go`，`New()` 里（`g.Rot = rotator.New(...)` 之前）加：
```go
	// 池：选号从它读，不再每次扫全表（规格 §6.2）。
	g.Pool = pool.New(0)
	g.touches = newSessionTouches()
	// 池空的时候的那份兜底：库里的答案留一秒（规格 §6.9）。
	g.fallback = newDBFallback(s, concurrencyCacheTTL)
	g.Keeper = pool.NewKeeper(g.Pool, poolSource{s: s, window: cfg.Pool.SessionWindow.Duration()}, mgr, s,
		cfg.Pool.ReadOnAdmit, cfg.Rotation.ExhaustedCooldown.Duration())
```

`internal/gateway/gateway.go`，`rotator.New(...)` 那行改成：
```go
	g.Rot = rotator.New(poolAdapter{s: s, busy: g.busy, pool: g.Pool, fallback: g.fallback}, rotator.Options{
```

`internal/gateway/gateway.go`，`New()` 末尾（`return g` 之前）加：
```go
	// 池要一直是满的：每 scan_every 一趟看看少了几个，补上。
	go g.poolLoop()
	// 用过的会话要写回去：一批一次，它们是「活跃会话数」的凭据（规格
	// §6.3）。它在后台组里：关网关时要等它把最后一批写完，不然它会往
	// 一个正在关着的库上写。
	g.bgWG.Add(1)
	go func() {
		defer g.bgWG.Done()
		g.touchLoop()
	}()
```

`internal/gateway/gateway.go`，`poolAdapter` 加一个字段：
```go
	// pool is the accounts kept in memory.
	//
	// 它是空的（还没填上，或者填池失败）的时候，适配器回到从前的样子，
	// 从库里拿：网关不能因为池空着就一个请求都答不了。
	pool *pool.Pool
	// fallback is the database's answer, cached, for the time the pool
	// is empty.
	//
	// 池空的时候那三样都是全表读（规格 §6.9）：缓存一秒，让那一波并发只
	// 读一次。
	fallback *dbFallback
```

`internal/gateway/gateway.go`，`poolAdapter` 加一个问法（挨着 `Accounts` 放）：
```go
// fromPool says whether the pool has something to give.
//
// 池空着的时候（还没填上，或者填池失败）适配器回到从前的样子，从库里
// 拿：网关不能因为池空着就一个请求都答不了（规格不变量 1）。
func (p poolAdapter) fromPool() bool {
	return p.pool != nil && p.pool.Len() > 0
}
```

`internal/gateway/gateway.go`，`poolAdapter` 的 `Accounts` 改成：
```go
func (p poolAdapter) Accounts(ctx context.Context) ([]store.Account, error) {
	if p.fromPool() {
		return p.pool.Accounts(), nil
	}
	if p.fallback != nil {
		return p.fallback.accounts(ctx)
	}
	return p.s.ListAccounts(ctx)
}
```

`internal/gateway/gateway.go`，`poolAdapter` 的 `Exhausted` 改成：
```go
func (p poolAdapter) Exhausted(ctx context.Context) ([]store.Exhausted, error) {
	if p.fromPool() {
		return p.pool.Spent(), nil
	}
	if p.fallback != nil {
		return p.fallback.exhausted(ctx)
	}
	return p.s.ListExhausted(ctx)
}
```

`internal/gateway/gateway.go`，`poolAdapter` 的 `MarkExhausted` 改成：
```go
func (p poolAdapter) MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	if err := p.s.SetExhausted(ctx, accountID, windowKey, until, reason); err != nil {
		return err
	}
	if p.pool != nil {
		// 标记写下去了，这个号就出池：它接不了活了，让位给一个冷的
		//（规格 §4.4）。补号是后台的活。
		p.pool.MarkSpent(accountID, windowKey, until, reason)
		p.pool.Remove(accountID)
	}
	return nil
}
```

`internal/gateway/gateway.go`，`poolAdapter` 的 `RemainingAll` 开头（`out := map[string]rotator.Remaining{}` 之后、`wanted := ...` 之前）插入：
```go
	if p.fromPool() {
		for id := range wanted {
			e, ok := p.pool.Get(id)
			if !ok || !e.Known {
				// 没读到余额的账号不算有余额：留空，让 rotator 把它排在
				// 后面。
				continue
			}
			out[id] = rotator.Remaining{Left: e.Remaining, Known: true}
		}
		return out
	}
	if p.fallback != nil {
		return p.fallback.remaining(ctx, accountIDs)
	}
```

（`fromPool()` 那道门是必要的：池空的时候 `p.pool != nil` 也成立，从空的池里读出来的余额是空的 —— 余额只当门槛，门槛没了，挑号就瞎挑。）

- [ ] **Step 4: 池空的时候，库里的答案留一秒 —— `internal/gateway/fallback.go`**

新建 `internal/gateway/fallback.go`：
```go
package gateway

import (
	"context"
	"sync"
	"time"

	"mmxcode/internal/rotator"
	"mmxcode/internal/store"
)

// dbFallback is the database's answer for the rotator, taken while
// the pool is empty.
//
// 池空的时候适配器回到从前的样子，从库里拿账号、耗尽标记和余额。那三样
// 都是全表读：1000 个号的账号表 + 标记表 + 全部最新快照（连 windows_json
// 一起），一个请求一次就是 2.6MB 的垃圾，几十个并发就是一波几百 MB。
//
// 所以这里留一份缓存，一秒一份。这一阵子的答案是一个样的 ——
// concurrencyLimit 用的就是同一个道理。缓存让那一波并发只读一次。
//
// 它会过期一秒。这一秒里刚被标记成「用完」的号还看得出能用：请求会挑
// 中它，撞一次空，换号重试。撞一次空的代价是一个请求；不缓存的代价是
// 每一波并发都把整张表读一遍。前者便宜得多。
type dbFallback struct {
	mu   sync.Mutex
	ttl  time.Duration
	s    poolStore
	at   time.Time
	snap *dbAnswer
}

// dbAnswer is one answer from the database.
type dbAnswer struct {
	accounts  []store.Account
	exhausted []store.Exhausted
	remaining map[string]rotator.Remaining
}

// newDBFallback makes the cache.
func newDBFallback(s poolStore, ttl time.Duration) *dbFallback {
	if ttl <= 0 {
		ttl = time.Second
	}
	return &dbFallback{s: s, ttl: ttl}
}

// get gives the snapshot, taking a new one when the old is stale.
//
// 它一次只让一个取：等的时候拿着锁（活儿很便宜，几毫秒）。
func (f *dbFallback) get(ctx context.Context) (*dbAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snap != nil && time.Since(f.at) < f.ttl {
		return f.snap, nil
	}
	accounts, err := f.s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	exhausted, err := f.s.ListExhausted(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := f.s.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	remaining := make(map[string]rotator.Remaining, len(snaps))
	for _, s := range snaps {
		if !s.HasAmounts {
			// 没读到余额的账号不算有余额：留空，让 rotator 把它排在后面
			//（规格不变量 2）。
			continue
		}
		remaining[s.AccountID] = rotator.Remaining{Left: s.Remaining, Known: true}
	}
	f.snap = &dbAnswer{accounts: accounts, exhausted: exhausted, remaining: remaining}
	f.at = time.Now()
	return f.snap, nil
}

// accounts gives the accounts of the snapshot.
func (f *dbFallback) accounts(ctx context.Context) ([]store.Account, error) {
	snap, err := f.get(ctx)
	if err != nil {
		return nil, err
	}
	return snap.accounts, nil
}

// exhausted gives the exhaustion marks of the snapshot.
func (f *dbFallback) exhausted(ctx context.Context) ([]store.Exhausted, error) {
	snap, err := f.get(ctx)
	if err != nil {
		return nil, err
	}
	return snap.exhausted, nil
}

// remaining gives the credits of the snapshot, for the accounts asked for.
//
// 留空的是「没读到余额」的账号：rotator 把它们排在有数的号后面（规格
// 不变量 2）。
func (f *dbFallback) remaining(ctx context.Context, ids []string) map[string]rotator.Remaining {
	snap, err := f.get(ctx)
	if err != nil {
		return map[string]rotator.Remaining{}
	}
	out := make(map[string]rotator.Remaining, len(ids))
	for _, id := range ids {
		if rem, ok := snap.remaining[id]; ok {
			out[id] = rem
		}
	}
	return out
}
```

- [ ] **Step 6: 请求真的用了会话，就记下来**

`internal/gateway/gateway.go`，挑到账号之后（`used[plan.AccountID] = true` 之后）加：
```go
		// 这段对话用过：记下来，攒一批写回库（规格 §6.3）。它的
		// updated_at 是「活跃会话数」的凭据 —— 池要靠它把这段对话绑
		// 着的号留在池里。
		s.touches.add(sess.Key)
```

- [ ] **Step 7: 写池的驱动 —— `internal/gateway/pool.go`**

新建 `internal/gateway/pool.go`：
```go
package gateway

import (
	"context"
	"log"
	"time"

	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// poolSource gives the pool the accounts it could take.
type poolSource struct {
	s *store.Store
	// window is how long a session counts as active.
	//
	// 它是「这段对话还在用吗」的那把尺子：这段时间里被碰过的对话，它绑
	// 着的号要留在池里（规格不变量 3）。
	window time.Duration
}

// Candidates gives the accounts that are not in the pool, in the order
// they should go in.
func (p poolSource) Candidates(ctx context.Context, limit int, exclude map[string]bool) ([]store.Candidate, error) {
	// 没有窗口就是没有「活跃」：没有对话算活跃，一个号也不因为对话而
	// 排在前面（规格 §6.3 的那一项直接不算）。
	since := time.Now()
	if p.window > 0 {
		since = time.Now().Add(-p.window)
	}
	return p.s.PoolCandidates(ctx, limit, exclude, since)
}

// poolTarget says how many accounts the pool should hold.
//
// 池多大 = max(总并发, 活跃会话数) × 倍数（规格 §6.3）：
//
//   - 总并发是「该跑多少个请求」= 可用账号数 ÷ 5（小规模至少 3）；
//   - 活跃会话数是「有多少段对话在用」—— 会话钉在号上，钉住的号不在池
//     里会被改绑，缓存命中就没了，所以池要装得下它们；
//   - 倍数是 2：再留一倍给补号（补号要读一次额度，那段时间位置空着）。
//
// 上限有三个：配的 max_size、内存预算算出来的数、还有库里能用的号数。
func (s *Server) poolTarget(ctx context.Context) int {
	factor := s.Cfg.Pool.Factor
	if factor < 1 {
		factor = 1
	}
	usable, err := s.Store.CountUsable(ctx)
	if err != nil {
		return 0
	}
	demand := totalFor(usable, s.Cfg.Concurrency)
	if w := s.Cfg.Pool.SessionWindow.Duration(); w > 0 {
		n, err := s.Store.CountRecentSessions(ctx, time.Now().Add(-w))
		if err == nil && n > demand {
			demand = n
		}
	}
	want := demand * factor
	if s.Cfg.Pool.MaxSize > 0 && want > s.Cfg.Pool.MaxSize {
		want = s.Cfg.Pool.MaxSize
	}
	if mb := s.Cfg.Pool.MemoryBudgetMB; mb > 0 {
		byMemory := mb * 1024 * 1024 / pool.BytesPerAccount
		if want > byMemory {
			want = byMemory
		}
	}
	// 池装不下比库里能用的号更多的东西。
	if want > usable {
		want = usable
	}
	return want
}

// SeedPool fills the pool with what the database knows.
//
// 它**不问站点**：读额度是 2~11 个上游请求，填满 400 个号要半个多小时，
// 而网关要立刻能用（规格 §6.3）。旧数只是高估 —— 撞空的时候换号重试会
// 把标记写准，自愈。
func (s *Server) SeedPool(ctx context.Context) {
	want := s.poolTarget(ctx)
	s.Pool.SetCapacity(want)
	n, err := s.Keeper.Seed(ctx, want)
	if err != nil {
		log.Printf("the account pool could not be filled: %v", err)
		return
	}
	log.Printf("the account pool has %d of %d accounts", n, want)
}

// TopUpPool puts accounts into the pool until it holds what it should.
//
// 它不挡客户端：后台的活。
//
// 补进来的号读一次额度（read_on_admit）：读到的数说它有额度，它才进池；
// 读到「用完了」，它不进池，而且被标记掉。
func (s *Server) TopUpPool(ctx context.Context) {
	want := s.poolTarget(ctx)
	s.Pool.SetCapacity(want)
	n, err := s.Keeper.Replenish(ctx, want)
	if err != nil {
		log.Printf("the account pool could not be filled: %v", err)
		return
	}
	if n > 0 {
		log.Printf("%d accounts went into the pool (%d of %d)", n, s.Pool.Len(), want)
	}
}

// poolLoop keeps the pool full.
//
// 每 scan_every 一趟：池里少了号就补上。补号是上游的活，所以它在后台，
// 而且一次只跑一趟（规格 §6.4）。
func (s *Server) poolLoop() {
	every := s.Cfg.Usage.Refresh.ScanEvery.Duration()
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.lifeCtx.Done():
			return
		case <-t.C:
			s.startBackground(func() { s.TopUpPool(s.lifeCtx) })
		}
	}
}
```

- [ ] **Step 8: 写会话活跃的账 —— `internal/gateway/touch.go`**

新建 `internal/gateway/touch.go`：
```go
package gateway

import (
	"context"
	"log"
	"sync"
	"time"
)

// touchEvery is how often the used sessions are written into the database.
//
// 攒着写，不是每个请求写一行：一行 SQLite 写相对上游那一下请求不算什
// 么，但它会跟读额度抢写锁，而读额度是要紧的活（规格 §6.4）。
//
// 20 秒够细了：「活跃」的窗口是 1 小时，20 秒的差别看不出来。
const touchEvery = 20 * time.Second

// touchTimeout is how long the write of the used sessions may take.
//
// 写不进去不是要命的事：这一批的 updated_at 旧一点，下一批会再写一次。
// 但它不能拖着请求不放 —— 等一个空位比等一次写更值得等。
const touchTimeout = 5 * time.Second

// sessionTouches holds the sessions that were used, waiting to be written.
//
// 它是内存里的账：会话用过没有，只有网关知道（规格 §6.3）。它记不住什
// 么 —— 进程一停就没了 —— 但那没关系：重启之后头几趟请求会把它写回去。
type sessionTouches struct {
	mu sync.Mutex
	// keys are the sessions that were used and not written yet.
	keys map[string]bool
}

// newSessionTouches makes an empty ledger.
func newSessionTouches() *sessionTouches {
	return &sessionTouches{keys: map[string]bool{}}
}

// add remembers that a session was used.
func (t *sessionTouches) add(sessionKey string) {
	if sessionKey == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keys[sessionKey] = true
}

// take gives the sessions that were used and forgets them.
//
// 拿了就是归网关了：写不进去也得放下 —— 留着只会让下一批更大，而下一批
// 会再写一次同样的键。
func (t *sessionTouches) take() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.keys))
	for k := range t.keys {
		out = append(out, k)
	}
	t.keys = map[string]bool{}
	return out
}

// flushTouches writes the sessions that were used into the database.
//
// 写不进去就记一笔：写不进去的会话看起来不活跃，池子就少留一个号 ——
// 说出来才知道少了什么。
func (s *Server) flushTouches(ctx context.Context) {
	keys := s.touches.take()
	if len(keys) == 0 {
		return
	}
	if err := s.Store.TouchSessions(ctx, keys); err != nil {
		log.Printf("the %d used sessions could not be written: %v", len(keys), err)
	}
}

// touchLoop writes the used sessions into the database now and then.
//
// 每 touchEvery 一趟：把这一阵子用过的会话写回去。它们的 updated_at 是
// 「活跃会话数」唯一的凭据（规格 §6.3）—— 不写回去，一段一直在用的对话
// 看起来就像不活跃，池子就不会留着它绑着的号，缓存命中也就没了。
func (s *Server) touchLoop() {
	t := time.NewTicker(touchEvery)
	defer t.Stop()
	for {
		select {
		case <-s.lifeCtx.Done():
			// 走之前把攒着的写回去：最后这一批不该丢。
			ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
			s.flushTouches(ctx)
			cancel()
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(s.lifeCtx, touchTimeout)
			s.flushTouches(ctx)
			cancel()
		}
	}
}
```

- [ ] **Step 9: 跑测试，看它过**
```sh
go test ./internal/gateway/
```

Expected: PASS。

- [ ] **Step 10: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(gateway): 选号走池，用过的会话写回库

适配器的来源换成池：池里有就读池，池是空的还是从库里读 —— 网关不能因
为池空着就一个请求都答不了（规格不变量 1）。标记写下去的时候那个号
出池，让位给一个冷的。

池空的时候从库里读的那三样都是全表读，一个请求一次就是 2.6MB。缓存一
秒，让那一波并发只读一次（规格 §6.9）。

用过的会话攒着写回库（规格 §6.8）：sessions.updated_at 原来只在改绑时
刷新，一直在用的对话看起来不活跃 —— 池子公式里「活跃会话数」那一项
永远是 0，而且重启后被绑定的号不在池里，每段对话丢一次上下文缓存。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/gateway/gateway.go internal/gateway/pool.go internal/gateway/pool_test.go internal/gateway/touch.go internal/gateway/touch_test.go internal/gateway/fallback.go internal/gateway/fallback_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 6: 承诺的并发按池夹 + 耗尽了出池补号

**Files:**
- Create: `internal/gateway/concurrency_test.go`
- Modify: `internal/gateway/busy.go`
- Modify: `internal/gateway/spent.go`
- Modify: `cmd/mmxcode/serve.go`

**总并发**：`totalFor(usable)` 是从「库里有多少能用的号」算出来的。现在池里有几个号才算几个 —— 池里没有那么多号，就只能承诺那么多，承诺了接不住就是一片 429（规格不变量 4）。池是空的（还没填上，或者填池失败）的时候从库里数：网关得能干活。

再往上还有一道**天花板**（`concurrency.total_max`，规格 §6.9）：池空的时候承诺是从库里数出来的，库里 1000 个号就承诺 200 —— 那一下能从池子撑的数抬到库里的数。天花板把它按住。0 = 不限，跟从前一样。

**耗尽了出池**：额度用完的号写完标记就出池，补一个冷的进来。补号是后台的活，不挡客户端。

- [ ] **Step 1: 写失败的测试**

新建 `internal/gateway/concurrency_test.go`：

（`openGatewayStore` 是 Task 5 在 `internal/gateway/pool_test.go` 里写的，同一个包，直接用 —— 别再抄一份。）

```go
package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// manyAccounts makes a store with n signed-in accounts.
//
// 号的名字是数出来的：acc_a0、acc_b0 …… acc_a1 —— 头一个永远是 acc_a0，
// 测试好往池里放它。
func manyAccounts(t *testing.T, n int) *store.Store {
	t.Helper()
	s := openGatewayStore(t)
	now := time.Now()
	for i := 0; i < n; i++ {
		if err := s.CreateAccount(context.Background(), store.Account{
			ID:     "acc_" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// TestTheConcurrencyIsNotMoreThanThePool says the gateway does not
// promise more requests than the pool can carry.
//
// 承诺了接不住就是一片 429（规格不变量 4）。
func TestTheConcurrencyIsNotMoreThanThePool(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	// 库里有很多能用的号，池里只有一个。
	g.Pool.Admit(pool.Entry{Account: store.Account{ID: "acc_a0", Status: store.StatusActive}})

	if got := g.totalConcurrency(ctx); got != 1 {
		t.Fatalf("concurrency = %d, want 1", got)
	}
}

// TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty says a
// gateway whose pool is empty still promises what it used to.
func TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty(t *testing.T) {
	s := manyAccounts(t, 10)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	if got := g.totalConcurrency(ctx); got != 3 {
		t.Fatalf("concurrency = %d, want 3", got)
	}
}

// TestTheConcurrencyDoesNotGoOverTheCeiling says the promised
// concurrency never goes over the ceiling.
//
// 天花板是「并发可控」的那根杆。池空的时候承诺从库里数：库里 100 个号就
// 承诺 20 个 —— 那一下能从池子撑的数抬到库里的数。天花板把它按在运营定
// 的那个数上。
func TestTheConcurrencyDoesNotGoOverTheCeiling(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Concurrency.TotalMax = 5
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	if got := g.totalConcurrency(ctx); got != 5 {
		t.Fatalf("concurrency = %d, want 5", got)
	}
}

// TestTheCeilingDoesNotRaiseThePromise says the ceiling only lowers it.
//
// 池里只有两个号，承诺五个是接不住的：承诺了接不住就是一片 429（规格
// 不变量 4）。
func TestTheCeilingDoesNotRaiseThePromise(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Concurrency.TotalMax = 5
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}
	g.Pool.Admit(pool.Entry{Account: store.Account{ID: "acc_a0", Status: store.StatusActive}})
	g.Pool.Admit(pool.Entry{Account: store.Account{ID: "acc_b0", Status: store.StatusActive}})

	if got := g.totalConcurrency(ctx); got != 2 {
		t.Fatalf("concurrency = %d, want 2", got)
	}
}

// TestWithoutACeilingThePromiseIsTheOldOne says nothing changes when no
// ceiling is set.
//
// 0 = 不限：跟从前一样，池空的时候从库里数。
func TestWithoutACeilingThePromiseIsTheOldOne(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	if got := g.totalConcurrency(ctx); got != 20 {
		t.Fatalf("concurrency = %d, want 20", got)
	}
}

// TestTheCeilingMakesThePoolSmaller says the pool is sized after the
// promise, and the promise is capped.
//
// 池多大 = max(总并发, 活跃会话数) × 倍数（规格 §6.3）。总并发被天花板
// 按住了，池就跟着小 —— 「n×2」那个说法才有着落（规格 §6.9）。
func TestTheCeilingMakesThePoolSmaller(t *testing.T) {
	s := manyAccounts(t, 100)
	ctx := context.Background()
	cfg := config.Default()
	cfg.Concurrency.TotalMax = 5
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(0)}

	// 天花板 5，倍数 2 → 池装 10 个号；没有天花板的时候是 20 × 2 = 40。
	if got := g.poolTarget(ctx); got != 10 {
		t.Fatalf("pool target = %d, want 10", got)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/gateway/ -run "Concurrency|Ceiling|Spent"
```

Expected: FAIL —— `TestTheConcurrencyIsNotMoreThanThePool` 挂：库里 100 个号，
`totalConcurrency` 还是从库里数（100 ÷ 5 = 20），池里只有一个号就该只承诺 1。
`TestTheConcurrencyDoesNotGoOverTheCeiling` 也挂：天花板定 5，承诺的还是 20。

（`TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty` 和 Task 5 的
`TestASpentAccountLeavesThePool` 这时候是过的 —— 它们是回归的护栏，不是这一
步要做的事。）

（过滤器里有 `Ceiling`：`TestTheCeilingDoesNotRaiseThePromise` 和
`TestWithoutACeilingThePromiseIsTheOldOne` 的名字里没有 Concurrency，筛窄了
它们就一个都不跑。）

- [ ] **Step 3: 总并发按池夹，天花板进 `totalFor`**

`internal/gateway/busy.go`，`totalFor` 末尾（`if total < 1 { total = 1 }` 之后、`return total` 之前）插入：
```go
	// 天花板：运营定的那根杆（规格 §6.9）。它在这里，不在
	// totalConcurrency 里 —— 池的大小是按承诺的并发算的
	// （max(总并发, 活跃会话数) × 倍数），天花板压承诺的时候，池也跟着小。
	if top := cfg.TotalMax; top > 0 && total > top {
		total = top
	}
```

**为什么放在 `totalFor` 里，不放在 `totalConcurrency` 里**：池的大小是按
承诺的并发算的（规格 §6.3）。天花板放在 `totalConcurrency` 里的话，承诺被
按住了，池却还按没被按住的那个数去装 —— 「n×2」那个说法就落空了。放在
`totalFor` 里，两边看的都是同一个数。

`internal/gateway/busy.go`，`totalConcurrency` 改成：
```go
// totalConcurrency says how many requests may be in the air at once.
//
// 承诺的并发 = min(期望并发, 池里能用的号数)：
//
//   - 期望并发 = 可用账号数 ÷ 5（小规模至少 3），按天花板截下来，跟从前一样；
//   - 池里能用的号数 = 池里签入中、没有被标记用完的号。
//
// 池里没有那么多号，就只能承诺那么多 —— 承诺了接不住就是一片 429（规格
// 不变量 4）。
//
// 池是空的（还没填上，或者填池失败）的时候从库里数：网关得能干活。但
// 那一下能从 20 抬到 80（库里 400 个号 ÷ 5），所以天花板正好是给这一
// 下准备的：它把承诺按在运营定的那个数上。
func (s *Server) totalConcurrency(ctx context.Context) int {
	if s.Store == nil {
		return 1
	}
	usable, err := s.Store.CountUsable(ctx)
	if err != nil {
		return 1
	}
	want := totalFor(usable, s.Cfg.Concurrency)
	inPool := s.Pool.Usable(time.Now())
	switch {
	case inPool == 0:
		// 池是空的：还没填上，或者填池失败。跟从前一样，从库里数。
	case inPool < want:
		// 池里的号不够：只能承诺这么多。
		want = inPool
	}
	return want
}
```

顺手删掉两样现在没人用的东西：

- `internal/gateway/busy.go` 里的 `spentNow` 函数 —— 耗尽判断搬进池了，它没有调用者了。
- `internal/gateway/busy.go` 的 import 里的 `"mmxcode/internal/store"` —— 删掉 `spentNow` 之后它就没有用处了。

- [ ] **Step 4: 耗尽了出池，后台补号**

`internal/gateway/spent.go`，`markSpent` 里（`s.startBackground(func() { s.correctMark(accountID, groupKey) })` 之后）加：
```go
	// 池里少了一个号：补一个进去。补号是后台的活，不挡客户端（规格
	// §4.4）。
	s.startBackground(func() { s.TopUpPool(s.lifeCtx) })
```

- [ ] **Step 5: 启动时填池**

`cmd/mmxcode/serve.go`，`gw := gateway.New(...)` 之后加：
```go
			// 池要立刻能用：启动的时候用库里最后读到的数填上，不问站点
			//（规格 §6.3）。
			gw.SeedPool(ctx)
```

- [ ] **Step 6: 跑测试，看它过**
```sh
go test ./internal/gateway/ ./cmd/mmxcode/
```

Expected: PASS。

- [ ] **Step 7: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(gateway): 承诺的并发按池夹，额度用完了出池补号

池里没有那么多号，就只能承诺那么多 —— 承诺了接不住就是一片 429（规格
不变量 4）。池是空的还是从库里数：网关得能干活。

再往上有天花板（concurrency.total_max）：池空的那一下承诺从库里数，能
从 20 抬到 200。天花板是运营的那根杆（规格 §6.9）。

标记写下去的时候那个号出池，让位给一个冷的。补号在后台跑，不挡客户
端。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/gateway/busy.go internal/gateway/spent.go internal/gateway/concurrency_test.go cmd/mmxcode/serve.go
git commit -F .git/commit_msg.txt
```

---

### Task 7: 选号的顺序 —— 最久没用过的排前面

**Files:**
- Modify: `internal/rotator/rotator.go`
- Modify: `internal/rotator/rotator_test.go`

**为什么改**：从前挑号的第一条是「余额多的排前面」。那不是「先烧哪个」的问题 —— 那会让一个号被别的号挤在后面一直闲着，到头来被一口气烧光（规格 §6.5、§6.9）。这一版把余额降成「门槛」：读到 0 的号排在最后面；剩下的按「最久没用过的」排，用量摊开。余额只用来判门槛和打平手。

**会话黏性不受影响**：绑着的号能答就是它答（`Pick` 里那条 `default`），`better()` 只在「没有绑定」或者「绑着的号答不了」的时候才被问到。

- [ ] **Step 1: 写失败的测试**

`internal/rotator/rotator_test.go` 末尾加：

```go
// TestTheLeastUsedAccountComesFirst says the accounts are used evenly.
//
// 挑号的第一条是「最久没用过的排前面」，不是「余额最多的排前面」：一个号
// 被别的号挤在后面一直闲着，到头来被一口气烧光（规格 §6.5）。
func TestTheLeastUsedAccountComesFirst(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.remaining = map[string]float64{"acc_01": 400, "acc_02": 100}
	r := New(p, Options{MaxAttempts: 3})

	// 第一个号两个都没用过过，按余额，余额多的先。
	plan, err := r.Pick(context.Background(), "talk-1", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_01" {
		t.Fatalf("account = %s, want acc_01", plan.AccountID)
	}

	// 第二个号：acc_01 刚用过，acc_02 没用过。用最久没用的那个。
	plan, err = r.Pick(context.Background(), "talk-2", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_02" {
		t.Fatalf("account = %s, want acc_02", plan.AccountID)
	}
}

// TestAnAccountThatIsEmptyComesLast says an account that was read and
// found empty is not picked while another account has credits.
//
// 读到 0 就是知道它空了：挑它就是撞一次空，撞空要一个上游请求，还占着
// 并发。
func TestAnAccountThatIsEmptyComesLast(t *testing.T) {
	p := newFakePool(acc("acc_01"), acc("acc_02"))
	p.remaining = map[string]float64{"acc_01": 0, "acc_02": 400}
	r := New(p, Options{MaxAttempts: 3})

	// 先用有额度的那个。
	if _, err := r.Pick(context.Background(), "talk-1", "chat", nil); err != nil {
		t.Fatal(err)
	}
	// acc_01 从没用过（它最久没用过），但它读到的是 0：它接不了活了。
	plan, err := r.Pick(context.Background(), "talk-2", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_02" {
		t.Fatalf("account = %s, want acc_02", plan.AccountID)
	}
}

// TestTheSameAccountsAlwaysGiveTheSameOne says the choice is stable.
//
// 一样的情况挑出同一个号来：随机的挑法让同一个号被反复烧，缓存命中也
// 说不准。
func TestTheSameAccountsAlwaysGiveTheSameOne(t *testing.T) {
	p := newFakePool(acc("acc_02"), acc("acc_01"))
	p.remaining = map[string]float64{"acc_01": 100, "acc_02": 100}
	r := New(p, Options{MaxAttempts: 3})

	plan, err := r.Pick(context.Background(), "talk-1", "chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountID != "acc_01" {
		t.Fatalf("account = %s, want acc_01", plan.AccountID)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/rotator/ -run "LeastUsed|EmptyComesLast|SameAccounts"
```

Expected: FAIL —— `TestTheLeastUsedAccountComesFirst` 挂：第二个还是挑了
`acc_01`（它的余额多），没有换成最久没用过的 `acc_02`。
`TestTheSameAccountsAlwaysGiveTheSameOne` 也挂：挑的是 `acc_02`（它排在
前面），不是按 ID 排出来的 `acc_01`。

- [ ] **Step 3: 改挑选的规矩**

`internal/rotator/rotator.go`，`better()` 改成：
```go
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
```

- [ ] **Step 4: 跑测试，看它过**
```sh
go test ./internal/rotator/
```

Expected: PASS。

- [ ] **Step 5: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(rotator): 挑号改成最久没用过的优先，余额只当门槛

从前第一条是「余额多的排前面」：那会让一个号被别的号挤在后面一直闲着，
到头来被一口气烧光（规格 §6.5）。这一版把余额降成门槛 —— 读到 0 的排
最后面，剩下的按最久没用过的排，用量摊开。

再加两条兜底：一样的时候按 ID 排（同样的情况挑出同一个号来），读到 0
的号排在有数的号后面（挑它就是撞一次空）。

会话黏性不受影响：绑着的号能答就是它答。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/rotator/rotator.go internal/rotator/rotator_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 8: 文档 —— 配置怎么写

**Files:**
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`
- Modify: `README.md`

- [ ] **Step 1: `config.example.yaml` 和 `config.docker.yaml` 加 `pool:` 段**

两个文件都把每一个段都列了出来（`config.docker.yaml` 尤其如此 —— 容器里没有示例文件，段名写错就是「unknown key」），所以两个都要加。加在 `concurrency:` 段后面、`db:` 前面：
```yaml
# 账号池：网关把账号放在内存里的一个池子里，挑号不再每次扫全表。
#
# 池多大 = max(总并发, 活跃会话数) × factor。
#   - factor：倍数。2 = 再留一倍给补号用。
#   - max_size：池里最多几个号。0 = 不限（由别的两个上限说了算）。
#   - memory_budget_mb：池的内存预算（兜底）。一个号按 512 字节算，
#     32MB 装得下 6 万多个号 —— 200–1000 个号的时候这个上限碰不到。
#   - session_window：算「活跃会话数」时看多久以内的会话。1 小时。
#   - read_on_admit：补号时先读一次额度再进池（true = 读不到额度
#     的号不进池）。
pool:
  factor: 2
  max_size: 0
  memory_budget_mb: 32
  session_window: 1h
  read_on_admit: true
```

- [ ] **Step 1b: `config.example.yaml` 和 `config.docker.yaml` 的 `concurrency:` 段加 `total_max`**

两个文件的 `concurrency:` 段都一样，加在 `total_min` 后面、`wait` 前面：
```yaml
  # 总并发的天花板：不管池子里有多少个号、库里有多少个号，承诺的并发都
  # 不超过它。0 = 不限（跟从前一样）。
  #
  # 池空的时候承诺是从库里数出来的 —— 库里 1000 个号就承诺 200，那一下能
  # 从池子撑的数抬到库里的数。天花板是给这一下准备的。池子的大小是按承
  # 诺的并发算的（max(总并发, 活跃会话数) × 倍数），所以天花板定 64，池
  # 子就是 128 个号。
  total_max: 0
```

- [ ] **Step 2: `README.md` 的配置表加六行**

`README.md` 的 `## 配置` 那一节的表格末尾加：
```markdown
| `concurrency.total_max` | `0` | 总并发的天花板。0 = 不限 |
| `pool.factor` | `2` | 池装几个号 = max(总并发, 活跃会话数) × 它 |
| `pool.max_size` | `0` | 池里最多几个号。0 = 不限 |
| `pool.memory_budget_mb` | `32` | 池的内存预算（兜底）。一个号按 512 字节算 |
| `pool.session_window` | `1h` | 算「活跃会话数」时看多久以内的会话 |
| `pool.read_on_admit` | `true` | 补号时先读一次额度再进池。读到用完了就不进池 |
```

- [ ] **Step 3: `README.md` 写一节「账号池」**

加在 `### 代理模型` 前面（`## 配置` 这一节里面，`### 读额度` 的末尾）：
```markdown
### 账号池

网关把账号放在内存里的一个池子里，挑号的时候从池里挑，不扫全表。

- **池多大** = max(总并发, 活跃会话数) × `pool.factor`。默认 2 倍：再留
  一倍给补号用。会话数进公式是因为会话钉在号上，钉住的号不在池里会被
  改绑，缓存命中就没了。
- **池空着也能干活**：池是缓存不是真相 —— 账号、耗尽标记、会话绑定的
  真相都在库里。池空了或者填池失败，网关回到从前的样子从库里读。
- **额度用完的号出池**：标记写进库，那个号让位给一个冷的，进池之前读
  一次额度。
- **承诺的并发按池夹**：池里只有几个号就只承诺几个 —— 承诺了接不住就是
  一片 429。
- **总并发还有个天花板**：`concurrency.total_max`。0 = 不限；定一个数，
  承诺的并发就永远不超过它 —— 池空的那一下（承诺从库里数）最容易失控，
  天花板是给它的。
- **会话照旧黏在一个号上**：只有额度用完才改绑。

冷启动填池不等读额度：读一次是 2~11 个上游请求，填满 400 个号要半个
多小时，而网关要立刻能用。库里最后读到的数只是高估，撞空时换号重试会
把耗尽标记写准，系统自己会好。
```

- [ ] **Step 4: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
docs: 账号池怎么配 —— 倍数、内存预算、会话窗口

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add config.example.yaml config.docker.yaml README.md
git commit -F .git/commit_msg.txt
```

---

## 怎么验

计划里的每一段 Go 都跑过（在一个干净的副本里）：`go build ./...`、`go vet ./...`、`gofmt -l` 全干净，`go test ./...` 每个包都过，新增 56 个测试。

做完之后按这个顺序验：
```sh
go build ./...
go vet ./...
gofmt -l .
go test ./...
```

Expected: 全部干净；`gofmt -l .` 一个字都不吐。

本机跑一遍看看池有没有真的在用：
```sh
go build -o mmxcode.exe ./cmd/mmxcode
MMXCODE_API_KEYS=sk-admin ./mmxcode.exe --config config.yaml --db ./mmxcode.db serve
```

开着网关，日志里应该有：
```
the account pool has 20 of 40 accounts
```

（`20` 是填进去的数，`40` 是池该装多少 —— 由库里能用的号数、活跃会话数和 `pool.factor` 算出来。）

再过 30 秒（`usage.refresh.scan_every`），池少了号的时候日志里会有：
```
2 accounts went into the pool (22 of 40)
```

补号会读一次额度（`pool.read_on_admit`），所以补进去之前它读的是冷的号 —— 补号是上游的活，不挡客户端。

看池的状态：
```sh
sqlite3 mmxcode.db "select count(*) from accounts where status='active'"
```

池的上界是 `max(总并发, 活跃会话数) × 2`，所以池里的号数应该远小于库里能用的号数（除非库里本来就只有那么几个）。

## 不在这一次里做的

- **卡片那条路拆两段读**（`LatestUsageSnapshots` 全量读 `windows_json`）。200–1000 个号时它只占 2MB；10 万个号才是生死线（207MB/请求）。另开一份规格。
- **会话绑定的内存缓存**。现在每请求一次主键索引查询，几十微秒，池化之后每请求本来也只剩这一次数库。
- **读额度的令牌桶、每个窗口组的熔断器、失败的号短冷却、池里余额的写回**。这一版先把并发按住（天花板 + 池夹），把选号的顺序摆正（最久没用过的优先）；池子跑起来、看到真的耗尽形态之后再定这些的参数 —— 现在定就是猜。

