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

---

### Task 1: 配置 —— 池多大、倍数、内存预算

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

池的大小 = max(总并发, 活跃会话数) × 倍数（规格 §6.3）。倍数 2：再留一倍给补号用。内存预算是个兜底 —— 一个账号按 512 字节算，32MB 装得下 6 万多个号。

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
//
// 没写的键走默认值：文件只需要写自己要改的。
func TestThePoolIsReadFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "pool:\n  factor: 3\n  max_size: 7\n"
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
	if c.Pool.SessionWindow.Duration() != time.Hour {
		t.Errorf("session_window = %s, want 1h", c.Pool.SessionWindow)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/config/ -run TestThePoolDefaults
```

Expected: FAIL —— `c.Pool undefined`（还没有这个配置）。

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
//
// 零值是每个键自己的规矩，写在各自的字段下面。
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
- Create: `internal/store/pool.go`
- Create: `internal/store/pool_test.go`

池要装的号从哪儿来：库里。查询给的是**最冷的那些** —— 从没读过的排最前，读过的按最后读到的时刻从早到晚。冷号先进池，被用熟的号留给会话（规格 §6.5）。

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

// TestTheCandidatesAreTheColdestFirst says the pool is filled with the
// accounts that were used least, not the ones that were used most.
//
// 冷号先进池：被用熟的号留给会话（规格 §6.5）。从来没读过的号排在最
// 前 —— 它还没有数，读一次就知道它有没有额度。
func TestTheCandidatesAreTheColdestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "warm")
	makeAccount(t, s, "cold")
	makeAccount(t, s, "never")
	now := time.Now()
	saveReading(t, s, "warm", 10, now.Add(-time.Hour))
	saveReading(t, s, "cold", 400, now.Add(-24*time.Hour))

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cands {
		ids = append(ids, c.Account.ID)
	}
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

// TestACandidateThatWasNeverReadSaysNothingAboutItsCredits says the
// credits of an account that was never read are not zero.
//
// 它是「没读到」，不是「没有」：写成 0 就是撒谎，撒谎的数会被当成真的
// 数去挑号（规格不变量 2）。
func TestACandidateThatWasNeverReadSaysNothingAboutItsCredits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")

	cands, err := s.PoolCandidates(ctx, 10, nil)
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
}

// TestACandidateThatWasReadCarriesItsCredits says a candidate that was
// read carries the credits it was read with.
func TestACandidateThatWasReadCarriesItsCredits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	saveReading(t, s, "a", 123, time.Now())

	cands, err := s.PoolCandidates(ctx, 10, nil)
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
}

// TestTheAccountsInThePoolAreNotCandidates says the accounts that are
// already in the pool are not offered again.
func TestTheAccountsInThePoolAreNotCandidates(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")

	cands, err := s.PoolCandidates(ctx, 10, map[string]bool{"a": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Account.ID != "b" {
		t.Fatalf("candidates = %v, want just b", cands)
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

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Account.ID == "a" {
			t.Fatal("a spent account is not a candidate")
		}
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

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = %v, want none", cands)
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
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/store/ -run "Candidate|Usable|RecentSession"
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
	"time"
)

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
	// Known says the credits have been read.
	//
	// 它是 false 的时候 Remaining 说不了话：这个号从来没被读过。不是 0
	// —— 0 是「读到了，没有了」。
	Known bool
	// LastReadAt is when the credits were read last. It is the zero
	// time when they never were.
	LastReadAt time.Time
}

// PoolCandidates gives the accounts that could be admitted to the pool.
//
// **最冷的在前**：从来没读过的排最前，读过的按最后读到的时刻从早到晚。
// 冷号先进池 —— 被用熟的号留给会话（规格 §6.5）。
//
// 排除掉的：exclude 里有的（已经在池里）、状态不是 active 的、现在被
// 标记用完的（标记的时刻还在将来的才算；过去的标记已经回来了，那个号
// 可以再进池）。
//
// 读多少行：要 limit 个候选，外加 exclude 里那么多行 —— 被排除的那些也
// 占着行号，所以要多取一些才够数。
func (s *Store) PoolCandidates(ctx context.Context, limit int, exclude map[string]bool) ([]Candidate, error) {
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
		ORDER BY (u.created_at IS NULL) DESC, u.created_at ASC, a.created_at ASC
		LIMIT ?`, StatusActive, ms(s.now()), limit+len(exclude))
	if err != nil {
		return nil, fmt.Errorf("pool candidates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var (
			c         Candidate
			userID    sql.NullString
			email     sql.NullString
			pg        sql.NullString
			pu        sql.NullString
			lastErr   sql.NullString
			createdAt int64
			updatedAt int64
			remaining sql.NullFloat64
			readAt    sql.NullInt64
		)
		if err := rows.Scan(&c.Account.ID, &c.Account.Name, &userID, &email, &c.Account.Status,
			&pg, &pu, &lastErr, &createdAt, &updatedAt,
			&remaining, &readAt); err != nil {
			return nil, fmt.Errorf("pool candidates: %w", err)
		}
		c.Account.UserID = userID.String
		c.Account.Email = email.String
		c.Account.ProxyGroup = pg.String
		c.Account.ProxyURL = pu.String
		c.Account.LastError = lastErr.String
		c.Account.CreatedAt = fromMS(createdAt)
		c.Account.UpdatedAt = fromMS(updatedAt)
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
func (s *Store) CountRecentSessions(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE updated_at >= ?", ms(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count recent sessions: %w", err)
	}
	return n, nil
}
```

- [ ] **Step 4: 跑测试，看它过**
```sh
go test ./internal/store/
```

Expected: PASS。

- [ ] **Step 5: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(store): 池的候选账号 —— 最冷的在前，用完的和签出的不算

冷号先进池：被用熟的号留给会话（规格 §6.5）。从来没读过的号排在最
前 —— 它还没有数，读一次就知道它有没有额度。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/store/pool.go internal/store/pool_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 3: internal/pool —— 池

**Files:**
- Create: `internal/pool/pool.go`
- Create: `internal/pool/pool_test.go`

池是内存里的一个集合：账号行、剩余积分、耗尽标记。**它不放计划窗口** —— 选号用不上，放进去每个账号从 400 字节变成 2.2 KB。

- [ ] **Step 1: 写失败的测试**

新建 `internal/pool/pool_test.go`：
```go
package pool

import (
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
	p.MarkSpent("a", "general", time.Now().Add(time.Hour))

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
	p.MarkSpent("a", "general", time.Now().Add(-time.Minute))

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
// 剩余积分、耗尽标记和 map 的索引，一行 395 字节。这里取 512 —— 宁可
// 高一点，内存预算算得保守些。
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
}

// Pool is the set of accounts the gateway keeps in memory.
//
// 它是并发安全的：聊天那条路读它，后台补号那条路写它。
//
// 账号名单和用完的名单各有一份现成的快照：挑号每个请求都要问一次，问
// 的时候不该现凑一份。快照是整份换掉的，不是就地改的 —— 拿着它的人看
// 到的一直是完整的一份。
type Pool struct {
	mu   sync.RWMutex
	byID map[string]*Entry
	cap  int
	// accounts is the snapshot of the accounts in the pool.
	accounts []store.Account
	// spent is the snapshot of the accounts whose window is spent.
	spent []store.Exhausted
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
	p.rebuild()
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
	p.rebuild()
	return true
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

// Entries gives every entry of the pool.
func (p *Pool) Entries() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Entry, 0, len(p.byID))
	for _, e := range p.byID {
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
func (p *Pool) MarkSpent(accountID, windowKey string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byID[accountID]
	if !ok {
		return
	}
	e.Spent = true
	e.SpentGroup = windowKey
	e.SpentUntil = until
	p.rebuild()
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

// Accounts gives the accounts in the pool.
//
// 给的是现成的那一份：挑号每个请求都要问一次，问的时候不该现凑一份。
// 那一份是整份换掉的，不是就地改的 —— 拿着它的人看到的一直是完整的一
// 份。
func (p *Pool) Accounts() []store.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accounts
}

// Spent gives the accounts in the pool whose window is spent.
func (p *Pool) Spent() []store.Exhausted {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.spent
}

// rebuild makes the snapshots again.
//
// 它在写锁底下跑：换的是整份，不是就地改。
func (p *Pool) rebuild() {
	accounts := make([]store.Account, 0, len(p.byID))
	spent := make([]store.Exhausted, 0, len(p.byID))
	for _, e := range p.byID {
		accounts = append(accounts, e.Account)
		if !e.Spent {
			continue
		}
		spent = append(spent, store.Exhausted{
			AccountID: e.Account.ID,
			WindowKey: e.SpentGroup,
			Until:     e.SpentUntil,
			Reason:    "quota",
		})
	}
	p.accounts = accounts
	p.spent = spent
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
go test ./internal/pool/ -run Keeper
```

Expected: FAIL —— `NewKeeper undefined`。

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

网关的 rotator 现在靠一个 `poolAdapter` 从「库」拿账号、耗尽标记和余额。这一步把适配器的来源换成池：**池里有就读池，池是空的还是从库里读** —— 网关不能因为池空着就一个请求都答不了（规格不变量 1）。

- [ ] **Step 1: 写失败的测试**

新建 `internal/gateway/pool_test.go`：
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

// TestTheConcurrencyIsNotMoreThanThePool says the gateway does not
// promise more requests than the pool can carry.
//
// 承诺了接不住就是一片 429（规格不变量 4）。
func TestTheConcurrencyIsNotMoreThanThePool(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}

	// 库里有很多能用的号，池里只有一个。
	for i := 0; i < 100; i++ {
		if err := s.CreateAccount(ctx, store.Account{
			ID:     "acc_" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Status: store.StatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	g.Pool.Admit(pool.Entry{Account: store.Account{ID: "acc_a0", Status: store.StatusActive}})

	if got := g.totalConcurrency(ctx); got != 1 {
		t.Fatalf("concurrency = %d, want 1", got)
	}
}

// TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty says a
// gateway whose pool is empty still promises what it used to.
func TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	cfg := config.Default()
	g := &Server{Store: s, Cfg: cfg, Pool: pool.New(10)}
	for i := 0; i < 10; i++ {
		if err := s.CreateAccount(ctx, store.Account{
			ID: "acc_" + string(rune('a'+i)), Status: store.StatusActive,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got := g.totalConcurrency(ctx); got != 3 {
		t.Fatalf("concurrency = %d, want 3", got)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/gateway/ -run "Pool|WholeTable"
```

Expected: FAIL —— `poolAdapter` 没有 `pool` 字段，`Server` 没有 `Pool` 字段。

- [ ] **Step 3: 网关里挂上池**

`internal/gateway/gateway.go`，import 里加：
```go
	"mmxcode/internal/pool"
```

`internal/gateway/gateway.go`，`Server` 里加两个字段：
```go
	// Pool is the accounts the gateway keeps in memory.
	//
	// 池是缓存，不是真相（规格不变量 1）：账号、耗尽标记、会话绑定的真
	// 相都在库里。池存在的理由是选号不用每次扫全表，还有内存有个上界。
	Pool *pool.Pool
	// Keeper fills the pool: at startup with what the database knows,
	// and later with the accounts that are needed.
	Keeper *pool.Keeper
```

`internal/gateway/gateway.go`，`New()` 里（`g.Rot = rotator.New(...)` 之前）加：
```go
	// 池：选号从它读，不再每次扫全表（规格 §6.2）。
	g.Pool = pool.New(0)
	g.Keeper = pool.NewKeeper(g.Pool, poolSource{s}, mgr, s,
		cfg.Pool.ReadOnAdmit, cfg.Rotation.ExhaustedCooldown.Duration())
```

`internal/gateway/gateway.go`，`rotator.New(...)` 那行改成：
```go
	g.Rot = rotator.New(poolAdapter{s: s, busy: g.busy, pool: g.Pool}, rotator.Options{
```

`internal/gateway/gateway.go`，`New()` 末尾（`return g` 之前）加：
```go
	// 池要一直是满的：每 scan_every 一趟看看少了几个，补上。
	go g.poolLoop()
```

`internal/gateway/gateway.go`，`poolAdapter` 加一个字段：
```go
	// pool is the accounts kept in memory.
	//
	// 它是空的（还没填上，或者填池失败）的时候，适配器回到从前的样子，
	// 从库里拿：网关不能因为池空着就一个请求都答不了。
	pool *pool.Pool
```

`internal/gateway/gateway.go`，`poolAdapter` 的 `Accounts` 改成：
```go
func (p poolAdapter) Accounts(ctx context.Context) ([]store.Account, error) {
	if p.pool == nil || p.pool.Len() == 0 {
		return p.s.ListAccounts(ctx)
	}
	return p.pool.Accounts(), nil
}
```

`internal/gateway/gateway.go`，`poolAdapter` 的 `Exhausted` 改成：
```go
func (p poolAdapter) Exhausted(ctx context.Context) ([]store.Exhausted, error) {
	if p.pool == nil || p.pool.Len() == 0 {
		return p.s.ListExhausted(ctx)
	}
	return p.pool.Spent(), nil
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
		p.pool.MarkSpent(accountID, windowKey, until)
		p.pool.Remove(accountID)
	}
	return nil
}
```

`internal/gateway/gateway.go`，`poolAdapter` 的 `RemainingAll` 开头（`out := map[string]rotator.Remaining{}` 之后、`wanted := ...` 之前）插入：
```go
	if p.pool != nil {
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
```

- [ ] **Step 4: 写池的驱动 —— `internal/gateway/pool.go`**

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
type poolSource struct{ s *store.Store }

// Candidates gives the accounts that are not in the pool, coldest first.
func (p poolSource) Candidates(ctx context.Context, limit int, exclude map[string]bool) ([]store.Candidate, error) {
	return p.s.PoolCandidates(ctx, limit, exclude)
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

- [ ] **Step 5: 跑测试，看它过**
```sh
go test ./internal/gateway/
```

Expected: PASS。

- [ ] **Step 6: 提交**
```sh
cat > .git/commit_msg.txt <<'EOF'
feat(gateway): 选号走池 —— 不再每个请求扫一遍全表

适配器的来源换成池：池里有就读池，池是空的还是从库里读 —— 网关不能因
为池空着就一个请求都答不了（规格不变量 1）。标记写下去的时候那个号
出池，让位给一个冷的。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/gateway/gateway.go internal/gateway/pool.go internal/gateway/pool_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 6: 承诺的并发按池夹 + 耗尽了出池补号

**Files:**
- Modify: `internal/gateway/busy.go`
- Modify: `internal/gateway/spent.go`
- Modify: `cmd/mmxcode/serve.go`

**总并发**：`totalFor(usable)` 是从「库里有多少能用的号」算出来的。现在池里有几个号才算几个 —— 池里没有那么多号，就只能承诺那么多，承诺了接不住就是一片 429（规格不变量 4）。池是空的（还没填上，或者填池失败）的时候从库里数：网关得能干活。

**耗尽了出池**：额度用完的号写完标记就出池，补一个冷的进来。补号是后台的活，不挡客户端。

- [ ] **Step 1: 写失败的测试**

测试在 Task 5 的 `internal/gateway/pool_test.go` 里已经有了：

- `TestTheConcurrencyIsNotMoreThanThePool` —— 库里有 100 个号，池里只有 1 个，承诺的并发是 1。
- `TestTheConcurrencyFallsBackToTheDatabaseWhenThePoolIsEmpty` —— 池是空的，承诺的并发跟从前一样。
- `TestASpentAccountLeavesThePool` —— 标记写下去，那个号出池。

- [ ] **Step 2: 跑测试，看它失败**
```sh
go test ./internal/gateway/ -run "Concurrency|Spent"
```

Expected: FAIL —— `totalConcurrency` 还是从库里数，`MarkExhausted` 还没让号出池。

- [ ] **Step 3: 总并发按池夹**

`internal/gateway/busy.go`，`totalConcurrency` 改成：
```go
// totalConcurrency says how many requests may be in the air at once.
//
// 承诺的并发 = min(期望并发, 池里能用的号数)：
//
//   - 期望并发 = 可用账号数 ÷ 5（小规模至少 3），跟从前一样；
//   - 池里能用的号数 = 池里签入中、没有被标记用完的号。
//
// 池里没有那么多号，就只能承诺那么多 —— 承诺了接不住就是一片 429（规格
// 不变量 4）。
//
// 池是空的（还没填上，或者填池失败）的时候从库里数：网关得能干活。
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
	if inPool == 0 {
		// 池是空的：还没填上，或者填池失败。跟从前一样，从库里数。
		return want
	}
	if inPool < want {
		return inPool
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

标记写下去的时候那个号出池，让位给一个冷的。补号在后台跑，不挡客户
端。

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
git add internal/gateway/busy.go internal/gateway/spent.go cmd/mmxcode/serve.go
git commit -F .git/commit_msg.txt
```

---

### Task 7: 文档 —— 配置怎么写

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

- [ ] **Step 2: `README.md` 的配置表加五行**

`README.md` 的 `## 配置` 那一节的表格末尾加：
```markdown
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

计划里的每一段 Go 都跑过（在一个干净的副本里）：`go build ./...`、`go vet ./...`、`gofmt -l` 全干净，`go test ./...` 每个包都过，新增 26 个测试。

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

