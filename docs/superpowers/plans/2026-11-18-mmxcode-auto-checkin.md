# 自动签到 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 账号每天按**大陆时间 00:00** 自动签到一趟；服务器没开着的那一天，起进程时补签；签到给的积分在一趟补号之内进到池里。

**Architecture:** 库是唯一的真相：`checkins` 表记着今天谁签到、给了多少积分。网关每一趟补号问库一次，把签到的积分换进池里那份数；补号的候选排序里今天签过的排前面，它们自己会进池。调度器按站点时区（大陆时间）算下一趟签到的时刻，只签今天还没签的号，启动时补一趟。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`（WAL）、`spf13/cobra`

设计文档：`docs/superpowers/specs/2026-11-18-mmxcode-auto-checkin-design.md`

分支：`feature/auto-checkin`（从 `feature/usage-refresh` 开）

---

## 这一版不做的

| 不做 | 为什么 |
|---|---|
| 签到失败的重试 | 失败就是失败：token 掉了要的是重登（`batch.relogin` 那一趟管它），不是重签 |
| 并发签到 | 用户说了串行慢慢签（1000 个号 1000 秒） |
| 签到完立刻给每个号读额度 | 每个号 2~11 个上游请求，1000 个号是 2000~11000 个请求。卡上的余额晚一点准，是上游的账（规格 §5.6） |
| 池里的数一律从库里刷新 | 那样签到的积分要等「签到待补」那一档读完才进池（1000 个号约 83 分钟）—— 用户抱怨的正是这个（规格 §4 做法 C） |
| 池满的时候把签到的号换进去 | 池里绑着对话的号不能赶（规格不变量 3） |

---

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/store/checkins.go`（新） | 签到这一块的事：站点时区、今天签到给了积分的号、今天没签到的号 |
| `internal/store/schema.go` | `checkins(day)` 上的索引 |
| `internal/store/pool.go` | 候选排序：今天签过的排前面 |
| `internal/gateway/pool.go` | 把签到的积分换进池里那份数 |
| `internal/config/config.go` | `schedule.checkin_at` 默认 `"00:00"`；新的 `schedule.checkin_jitter` |
| `internal/scheduler/scheduler.go` | 触发时刻按大陆时间；只签今天没签的号；启动时补签 |
| `cmd/mmxcode/serve.go` | 校验的错话 |
| `README.md` | 文档 |

---

### Task 1: 库 —— 今天签到给了积分的号、今天没签到的号

**Files:**
- Create: `internal/store/checkins.go`
- Create: `internal/store/checkins_query_test.go`
- Modify: `internal/store/store.go`（把时区和 `DayKey` 挪到 `checkins.go`）
- Modify: `internal/store/schema.go`（`checkins(day)` 上的索引）

签到这一块的事散在 `store.go` 里（2400 行），挪到一个自己的文件里。挪的是时区和 `DayKey` —— 「今天是哪一天」是签到的事。

- [ ] **Step 1: 写失败的测试**

新建 `internal/store/checkins_query_test.go`（包 `store`，跟 `pool_test.go` 一个包，用得上 `openTest` 和 `makeAccount`）：

```go
package store

import (
	"context"
	"testing"
	"time"
)

// TestTheCheckinsOfTheDayAreTheOnesThatGotCredits says the day's
// check-ins are given, with the credits they gave.
//
// 它是签到的积分进池的那条路（规格 §5.3）：只给给了积分的 —— 没有积分
// 的签到没有东西要告诉池子。
func TestTheCheckinsOfTheDayAreTheOnesThatGotCredits(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "paid")
	makeAccount(t, s, "free")
	day := "2026-11-18"
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "paid", Day: day, Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 没有积分的签到：没有东西要告诉池子。
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "free", Day: day, Outcome: "ineligible",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 别的日子签的，不算今天的。
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "free", Day: "2026-11-17", Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.DayCheckins(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AccountID != "paid" || got[0].Credit != 25 {
		t.Fatalf("the check-ins of the day = %+v, want just the paid one", got)
	}
}

// TestTheAccountsWithoutACheckinAreTheOnesThatHaveNotSignedInToday
// says the day's list is the accounts that were not checked in.
//
// 只签今天还没签的号（规格 §5.2）：签过的再签一次，站点只会答 done，白
// 花一次调用 —— 而今天没签到的那部分，正是服务器 00:00 没开着时漏掉的
// 那部分。
func TestTheAccountsWithoutACheckinAreTheOnesThatHaveNotSignedInToday(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "signed")
	makeAccount(t, s, "waiting")
	day := "2026-11-18"
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "signed", Day: day, Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 签不上的号不在名单里：要的是重登，不是签到。
	if err := s.CreateAccount(ctx, Account{
		ID: "gone", Name: "gone", Status: StatusSignedOut,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.AccountsWithoutCheckin(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "waiting" {
		t.Fatalf("the accounts = %+v, want just the waiting one", got)
	}
}

// TestTheCheckinZoneIsTheSitesZone says the check-in days are counted in
// the site's zone.
//
// 站点按大陆时间算一天：签到的时刻和「今天是哪一天」都得按那个时区算
// （规格 §5.1）。
func TestTheCheckinZoneIsTheSitesZone(t *testing.T) {
	// UTC 的 11-18 23:30 是大陆时间的 11-19 07:30。
	at := time.Date(2026, 11, 18, 23, 30, 0, 0, time.UTC)
	if got := at.In(CheckinZone()).Hour(); got != 7 {
		t.Errorf("the hour in the site's zone = %d, want 7", got)
	}
	if got := DayKey(at); got != "2026-11-19" {
		t.Errorf("the day = %s, want 2026-11-19", got)
	}
}

// TestTheCheckinsHaveAnIndexOnTheDay says the day's check-ins can be
// found without reading the whole table.
//
// 签到一天一行，一个号一天一行：1000 个号一年就是 36 万行。查「今天谁
// 签到」要的是索引，不是全表扫（规格 §5.3）。
func TestTheCheckinsHaveAnIndexOnTheDay(t *testing.T) {
	s := openTest(t)
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='checkins'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == "idx_checkins_day" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the check-ins have no index on the day")
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/store/ -run 'Checkin|CheckinsOfTheDay|AccountsWithoutACheckin|CheckinZone' -v`

Expected: 编译失败 —— `s.DayCheckins`、`s.AccountsWithoutCheckin`、`CheckinZone` 还没有。

- [ ] **Step 3: 写实现**

新建 `internal/store/checkins.go`：

```go
package store

import (
	"context"
	"fmt"
	"time"
)

// checkinZone is the timezone a check-in day is counted in.
//
// The site counts its days in China, so a day here is that zone's day.
// The zone is loaded by name, so the name is never ambiguous: "CST" is
// China Standard Time here and US Central somewhere else. A machine
// without the timezone database gets the same offset under a name that
// says what it is.
var checkinZone = loadCheckinZone()

// loadCheckinZone gives the timezone a check-in day is counted in.
func loadCheckinZone() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("UTC+8", 8*3600)
}

// CheckinZone gives the timezone the check-in days are counted in.
//
// 它导出出来，是为了让调度器算「下一趟签到是几点」的时候跟库里算「今
// 天是哪一天」用的是同一个时区：站点按大陆时间算一天，签到就得落在那
// 一天里（规格 §5.1）。
func CheckinZone() *time.Location {
	return checkinZone
}

// DayKey is the day a check-in is counted for.
//
// The day is the site's day, so that the day rolls over at the same
// hour for every account, whatever the machine's own timezone is.
func DayKey(t time.Time) string {
	return t.In(checkinZone).Format("2006-01-02")
}

// CheckinCredit is what one account's check-in gave.
type CheckinCredit struct {
	// AccountID is the account.
	AccountID string
	// Credit is how many credits the check-in gave.
	Credit float64
}

// DayCheckins gives the accounts that were checked in on a day, with the
// credits the check-in gave.
//
// 它是签到的积分进池的那条路：签到给了积分，池里那一份是进池那一刻的，
// 不知道这件事（规格 §5.3）。
//
// 只给给了积分的：没有积分的签到没有东西要告诉池子。
func (s *Store) DayCheckins(ctx context.Context, day string) ([]CheckinCredit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, credit FROM checkins
		WHERE day = ? AND credit > 0
		ORDER BY account_id`, day)
	if err != nil {
		return nil, fmt.Errorf("the check-ins of the day: %w", err)
	}
	defer rows.Close()
	var out []CheckinCredit
	for rows.Next() {
		var c CheckinCredit
		if err := rows.Scan(&c.AccountID, &c.Credit); err != nil {
			return nil, fmt.Errorf("the check-ins of the day: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the check-ins of the day: %w", err)
	}
	return out, nil
}

// AccountsWithoutCheckin gives the accounts that were not checked in on a
// day.
//
// 它是签到那一趟的名单：只签今天还没签的号（规格 §5.2）。
//
// 只有 active 的号：签不上的号签不了（signed_out 是 token 掉了，要的是
// 重登，不是签到）。
func (s *Store) AccountsWithoutCheckin(ctx context.Context, day string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_group, a.proxy_url,
			a.last_error, a.created_at, a.updated_at
		FROM accounts a
		WHERE a.status = ?
		  AND NOT EXISTS (
			SELECT 1 FROM checkins c WHERE c.account_id = a.id AND c.day = ?)
		ORDER BY a.created_at, a.id`, StatusActive, day)
	if err != nil {
		return nil, fmt.Errorf("the accounts without a check-in: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("the accounts without a check-in: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the accounts without a check-in: %w", err)
	}
	return out, nil
}
```

把 `internal/store/store.go` 里下面这一段**删掉**（挪到 `checkins.go` 了）：

```go
// checkinZone is the timezone a check-in day is counted in.
//
// The site counts its days in China, so a day here is that zone's day.
// The zone is loaded by name, so the name is never ambiguous: "CST" is
// China Standard Time here and US Central somewhere else. A machine
// without the timezone database gets the same offset under a name that
// says what it is.
var checkinZone = loadCheckinZone()

// loadCheckinZone gives the timezone a check-in day is counted in.
func loadCheckinZone() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("UTC+8", 8*3600)
}

// DayKey is the day a check-in is counted for.
//
// The day is the site's day, so that the day rolls over at the same
// hour for every account, whatever the machine's own timezone is.
func DayKey(t time.Time) string {
	return t.In(checkinZone).Format("2006-01-02")
}
```

`internal/store/schema.go` 的 `indexStatements` —— 那条按天的索引**不在**
这里（它有自己的一步，见下面）：

```go
// The indexes.
//
// 签到的行要按日子找：「今天谁签到了」是签到那一趟的名单，也是积分进
// 池的那条路（规格 §5.3）。一天一个号一行，1000 个号一年就是 36 万行
// —— 没有索引就是全表扫。
//
// 那条按天的索引不在这里：它由 migrateCheckinIndex 建（版本 6 的那一步），
// 因为老库停在版本 5，走不到这里 —— 这里只把版本 1 的库带到版本 2。
var indexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_account ON sessions(account_id)",
	"DROP INDEX IF EXISTS idx_checkins_account",
}
```

索引要有自己的一步：`indexStatements` 只在 migrateConform 那一步里跑，而那
一步只把版本 1 的库带到版本 2 —— 停在版本 5 的老库走不到它。老库没有那个
索引，「今天谁签到了」就变成全表扫（一年 36 万行），而那是补号那一趟
（每 15 分钟一趟）都在问的。

`internal/store/store.go`：版本往上抬一步，加一步建索引的：

```go
// schemaVersion is the version a file is at when the steps are done.
const schemaVersion = 6

// migrations are the steps, in order.
//
// The step at index n brings a file at version n to version n+1.
var migrations = []func(ctx context.Context, conn *sql.Conn) error{
	migrateMake,
	migrateConform,
	migratePlanError,
	migrateBatchImport,
	migrateAPIKeys,
	migrateCheckinIndex,
}
```

```go
// migrateCheckinIndex gives the check-ins an index on the day.
//
// 「今天谁签到了」是签到那一趟的名单，也是签到的积分进池的那条路（规格
// §5.3）。一天一个号一行，1000 个号一年就是 36 万行 —— 没有索引就是全表
// 扫，而那一趟是补号那一趟（每 15 分钟一趟）都在问的。
//
// 这一步是单独的：索引那几条在 migrateConform 里，而老库停在版本 5 ——
// 走不到那一步（那一步只把版本 1 的库带到版本 2）。
func migrateCheckinIndex(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 6, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_checkins_day ON checkins(day)"); err != nil {
			return fmt.Errorf("migrate: the index on the check-in day: %w", err)
		}
		return nil
	})
}
```

「今天是哪一天」也得只有一个钟：签到那一趟的名单（今天谁还没签）、签到
的积分进池（今天谁签了）、候选的排序（今天谁签过）—— 它们得说的是同一
个日子。库里带着自己的钟（`Store.Clock`），测试拿它把日子钉住。

`internal/store/checkins.go` 加一个：

```go
// Today is the site's day, by the store's clock.
//
// 签到这一件事只有一个"今天"：签到那一趟的名单（今天谁还没签）、签到的
// 积分进池（今天谁签了）、候选的排序（今天谁签过）—— 它们得说的是同
// 一个日子。库里带着自己的钟（Store.Clock），测试拿它把日子钉住。
func (s *Store) Today() string {
	return DayKey(s.now())
}
```

这条也要有测试（老库要拿到索引）—— 加在 `internal/store/checkins_query_test.go`：

```go
// TestTheCheckinIndexReachesAnOldFile says a file that is already at the
// old version gets the index on the day.
//
// 老库停在版本 5，走不到 migrateConform 里那几条索引（那一步只把版本 1
// 的库带到版本 2）。索引是新的那一步建的：没有它，「今天谁签到了」就是
// 全表扫 —— 一天一个号一行，1000 个号一年 36 万行（规格 §5.3）。
func TestTheCheckinIndexReachesAnOldFile(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// 把它退回到老版本：索引没有。
	if _, err := s.db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_checkins_day"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion-1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='checkins'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == "idx_checkins_day" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the old file did not get the index on the day")
	}
}
```

（那个文件的 import 要多一个 `fmt`。）

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/store/ -v -run 'Checkin'`

Expected: PASS。

- [ ] **Step 5: 跑整个存储层的测试**

Run: `go test ./internal/store/`

Expected: PASS（挪走的 `DayKey` 还在库里，用它的地方不用改）。

- [ ] **Step 6: 提交**

```bash
git add internal/store/checkins.go internal/store/checkins_query_test.go internal/store/store.go internal/store/schema.go
git commit -F .git/commit_msg.txt
```

提交信息：

```
feat(store): 库里查得到今天签到给了多少积分、今天还有谁没签

签到的积分要进池（规格 §5.3），池问库要的就是这两样：今天谁签到了、给
了多少钱；今天还有谁没签到（签到那一趟的名单）。

签到这一块的事从 store.go 挪到 checkins.go：时区、DayKey 和这两个查询
是同一件事。
```

---

### Task 2: 网关 —— 把签到的积分换进池里那份数

**Files:**
- Modify: `internal/gateway/pool.go`
- Modify: `internal/gateway/pool_test.go`

池里那一份数是**进池那一刻**的。签到给了积分，池不知道，那个号就排在有数的号后面，签到给的七天积分白瞎（规格 §5.3）。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/pool_test.go` 末尾加：

```go
// TestTheCheckinCreditsGoIntoThePool says the credits a check-in gave
// reach the pool.
//
// 池里那一份是进池那一刻的：签到给了积分，池不知道，那个号就排在有数
// 的号后面，签到给的七天积分白瞎（规格 §5.3）。
func TestTheCheckinCreditsGoIntoThePool(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckin(ctx, store.Checkin{
		AccountID: "acc_01", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 25, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	g := &Server{Store: s, Cfg: config.Default(), Pool: p}

	g.refreshCheckinCredits(ctx)

	e, ok := p.Get("acc_01")
	if !ok {
		t.Fatal("the account left the pool")
	}
	if !e.Known || e.Remaining != 25 {
		t.Fatalf("the pool's number = %v (known %v), want 25 and known", e.Remaining, e.Known)
	}
}

// TestThePoolDoesNotSayMoreThanItKnows says a check-in does not make the
// pool's number bigger than it really is.
//
// 池里那一份可能是签到**之后**读到的（读额度那一趟会把签到的积分读进
// 去）。那时候再加上签到的积分，就是说它有更多的额度（规格 §5.3）。
func TestThePoolDoesNotSayMoreThanItKnows(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckin(ctx, store.Checkin{
		AccountID: "acc_01", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 25, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{
		Account:   store.Account{ID: "acc_01", Status: store.StatusActive},
		Remaining: 100, Known: true,
	})
	g := &Server{Store: s, Cfg: config.Default(), Pool: p}

	g.refreshCheckinCredits(ctx)

	e, _ := p.Get("acc_01")
	if e.Remaining != 100 {
		t.Fatalf("the pool's number = %v, want the 100 it already had", e.Remaining)
	}
}

// TestAnAccountThatWasNeverReadIsKnownAfterItsCheckIn says a check-in
// makes an account that was never read a known one.
//
// 没读到的不算零，它排在有数的号后面（规格不变量 2）。签到给了积分，它
// 就确实有额度了 —— 别让它排在最后（规格 §5.3）。
func TestAnAccountThatWasNeverReadIsKnownAfterItsCheckIn(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckin(ctx, store.Checkin{
		AccountID: "acc_01", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 25, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	g := &Server{Store: s, Cfg: config.Default(), Pool: p}

	g.refreshCheckinCredits(ctx)

	e, _ := p.Get("acc_01")
	if !e.Known || e.Remaining != 25 {
		t.Fatalf("the pool's number = %v (known %v), want 25 and known", e.Remaining, e.Known)
	}
}

// TestTheTopUpPutsTheCheckinCreditsIn says the pass that fills the pool
// is the pass that puts the check-in's credits in.
//
// 每一趟补号（30 秒一趟）都跑的那一趟，就是换积分的那一趟：接线漏了，
// 签到的积分就永远进不了池（规格 §5.3）。
func TestTheTopUpPutsTheCheckinCreditsIn(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckin(ctx, store.Checkin{
		AccountID: "acc_01", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 25, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New(10)
	p.Admit(pool.Entry{Account: store.Account{ID: "acc_01", Status: store.StatusActive}})
	cfg := config.Default()
	// 补号的时候不读额度：读额度是问上游的，这个测试不碰上游。
	cfg.Pool.ReadOnAdmit = false
	lifeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keeper := pool.NewKeeper(p, poolSource{s: s, window: cfg.Pool.SessionWindow.Duration()},
		&fakeReader{}, s, pool.KeeperOptions{
			Read: false, Cooldown: time.Minute, ReadBudget: 1,
			ReadTimeout: time.Second, PassTimeout: 5 * time.Second,
		})
	g := &Server{
		Store: s, Cfg: cfg, Pool: p, Keeper: keeper,
		lifeCtx: lifeCtx, lifeCancel: cancel,
		bgSem: make(chan struct{}, 1),
	}

	g.TopUpPool(ctx)

	e, _ := p.Get("acc_01")
	if !e.Known || e.Remaining != 25 {
		t.Fatalf("the pool's number = %v (known %v), want 25 and known", e.Remaining, e.Known)
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/gateway/ -run 'CheckinCredits|PoolDoesNotSayMore|NeverReadIsKnown' -v`

Expected: 编译失败 —— `g.refreshCheckinCredits` 还没有。

- [ ] **Step 3: 写实现**

`internal/gateway/pool.go` 末尾加：

```go
// refreshCheckinCredits puts the credits the check-ins gave into the pool.
//
// 签到给了积分，池里那一份是进池那一刻的：那个号排在有数的号后面，签到
// 给的七天积分就白瞎了（规格 §5.3）。
//
// 新数是「池里那一份」和「签到给的」里大的那个，不是两份加起来：池里那
// 一份可能是签到之后读到的（读额度那一趟会把签到的积分读进去），再加
// 一遍就是说它有更多的额度。
//
// 不在池里的号不在这里管：补号的候选排序里今天签过的排前面，它自己会
// 进来（规格 §5.3）。
func (s *Server) refreshCheckinCredits(ctx context.Context) {
	if s.Pool == nil || s.Store == nil {
		return
	}
	credits, err := s.Store.DayCheckins(ctx, s.Store.Today())
	if err != nil {
		log.Printf("the check-in credits could not be read: %v", err)
		return
	}
	for _, c := range credits {
		e, ok := s.Pool.Get(c.AccountID)
		if !ok {
			continue
		}
		if e.Known && e.Remaining >= c.Credit {
			continue
		}
		s.Pool.SetRemaining(c.AccountID, c.Credit, true)
	}
}
```

`TopUpPool` 里，补号之前先换一次：

```go
func (s *Server) TopUpPool(ctx context.Context) {
	want := s.poolTarget(ctx)
	s.Pool.SetCapacity(want)
	// 签到的积分先换进池里：池里那一份是进池那一刻的（规格 §5.3）。
	s.refreshCheckinCredits(ctx)
	n, err := s.Keeper.Replenish(ctx, want)
	if err != nil {
		log.Printf("the account pool could not be filled: %v", err)
		return
	}
	if n > 0 {
		log.Printf("%d accounts went into the pool (%d of %d)", n, s.Pool.Len(), want)
	}
}
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/gateway/ -v -run 'CheckinCredits|PoolDoesNotSayMore|NeverReadIsKnown'`

Expected: PASS。

- [ ] **Step 5: 跑网关这一层的测试**

Run: `go test ./internal/gateway/`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/gateway/pool.go internal/gateway/pool_test.go
git commit -F .git/commit_msg.txt
```

提交信息：

```
feat(gateway): 签到的积分换进池里那份数

池里那一份是进池那一刻的：签到给了积分，池不知道，那个号就排在有数的
号后面，签到给的七天积分白瞎（规格 §5.3）。

新数取「池里那一份」和「签到给的」里大的那个，不相加：池里那一份可能
是签到之后读到的，再加一遍就是说它有更多的额度。
```

---

### Task 3: 库 —— 候选排序里今天签过的排前面

**Files:**
- Modify: `internal/store/pool.go`
- Modify: `internal/store/pool_test.go`

池外的签到过的号要自己进得来：候选排序里今天签过的排在「从没读过的号」前面（规格 §5.3）。

- [ ] **Step 1: 写失败的测试**

`internal/store/pool_test.go` 末尾加：

```go
// TestTheAccountsThatCheckedInComeFirst says the accounts that were
// checked in today are the first ones into the pool.
//
// 签到过的号一定刚拿到积分（七天有效），而从没读过的号有没有额度是**不
// 知道的** —— 填池那一趟要读它，读到 0 就白读一次（2~11 个上游请求，
// 规格 §5.3）。
func TestTheAccountsThatCheckedInComeFirst(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	s.Clock = func() time.Time { return at }
	makeAccount(t, s, "checked")
	makeAccount(t, s, "never")
	// 签过的那个号是有数的（它被读过），另一个从来没读过。
	saveReading(t, s, "checked", 10, at.Add(-24*time.Hour))
	if err := s.SaveCheckin(context.Background(), Checkin{
		AccountID: "checked", Day: DayKey(at), Outcome: "claimed",
		Credit: 25, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}

	ids := candidateIDs(t, s, 10, nil, at.Add(-time.Hour))
	want := []string{"checked", "never"}
	if len(ids) != len(want) {
		t.Fatalf("candidates = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("candidates = %v, want %v", ids, want)
		}
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/store/ -run 'TestTheAccountsThatCheckedInComeFirst' -v`

Expected: FAIL —— 候选是 `[never checked]`（从没读过的排在前面）。

- [ ] **Step 3: 写实现**

`internal/store/pool.go` 的 `PoolCandidates`，SQL 和参数改成：

```go
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
			(EXISTS (SELECT 1 FROM checkins c
				WHERE c.account_id = a.id AND c.day = ?)) DESC,
			(u.created_at IS NULL) DESC,
			u.created_at ASC,
			a.created_at DESC,
			a.id
		LIMIT ?`, StatusActive, ms(s.now()), ms(since), s.Today(), limit+len(exclude))
```

`PoolCandidates` 的注释里补上这一条（在「2. 从没读过的号」前面）：

```
//  2. **今天签到过的号** —— 签到给了积分（七天有效），它一定还有额度；
//     而从没读过的号有没有额度是不知道的，读它可能白读一次。
//  3. **从没读过的号，最新的导入排前面** —— 新导入的号几乎肯定没被用过
//     （用过就会被读，读过就有数）。导入很久还没数的号多半是被用过的，
//     它的额度早就不知道剩多少了。
//  4. **读过、但读得最早的号** —— 最冷的那些：它们闲着，闲着的号额度
//     还没被消耗光。
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/store/ -v -run 'Candidates|Candidate'`

Expected: PASS。

- [ ] **Step 5: 跑整个存储层的测试**

Run: `go test ./internal/store/`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/store/pool.go internal/store/pool_test.go
git commit -F .git/commit_msg.txt
```

提交信息：

```
feat(store): 候选里今天签到过的号排前面

签到过的号一定刚拿到积分（七天有效），而从没读过的号有没有额度是不知道
的 —— 填池那一趟要读它，读到 0 就白读一次（规格 §5.3）。
```

---

### Task 4: 配置 —— 签到几点、抖不抖

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go` 末尾加：

```go
// TestTheCheckinDefaults says when the check-in runs by default.
//
// 按大陆时间 00:00（规格 §5.1）：站点按大陆时间算一天，签到要落在那
// 一天的头里，签到给的积分才能用满七天。抖动是 0：一天一个实例，号又
// 是一个一个签的，抖动只是把开始往后推。
func TestTheCheckinDefaults(t *testing.T) {
	c := Default()
	if c.Schedule.CheckinAt != "00:00" {
		t.Errorf("checkin_at = %q, want 00:00", c.Schedule.CheckinAt)
	}
	if c.Schedule.CheckinJitter.Duration() != 0 {
		t.Errorf("checkin_jitter = %s, want 0", c.Schedule.CheckinJitter)
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/config/ -run 'TestTheCheckinDefaults' -v`

Expected: 编译失败 —— `Schedule.CheckinJitter` 还没有。

- [ ] **Step 3: 写实现**

`internal/config/config.go` 的 `Schedule` 改成：

```go
// Schedule holds the timers.
type Schedule struct {
	// CheckinAt is when the daily check-in runs, HH:MM.
	//
	// 它是**大陆时间**：站点按大陆时间算一天，签到要落在站点的那一天
	// 里（规格 §5.1）。
	CheckinAt string `yaml:"checkin_at" json:"checkin_at"`
	// CheckinJitter is how long the daily check-in may be held back.
	//
	// 它的零是有意思的：不抖。几个实例一起跑的时候，抖一下就散开了
	// （规格 §5.1）。
	CheckinJitter OptionalDuration `yaml:"checkin_jitter" json:"checkin_jitter"`
	UsageRefresh  Duration         `yaml:"usage_refresh" json:"usage_refresh"`
	TokenRefresh  Duration         `yaml:"token_refresh" json:"token_refresh"`
	EgressHealth  Duration         `yaml:"egress_health" json:"egress_health"`
}
```

`Default()` 里的 `Schedule` 改成：

```go
		Schedule: Schedule{
			CheckinAt:     "00:00",
			CheckinJitter: OptionalDuration(0),
			UsageRefresh:  Duration(15 * time.Minute),
			TokenRefresh:  Duration(5 * time.Minute),
			EgressHealth:  Duration(2 * time.Minute),
		},
```

`internal/config/config_test.go` 里 `TestLoadDefaults` 的那一条也要跟着改（它说的是老的默认值）：

```go
	if c.Schedule.CheckinAt != "00:00" {
		t.Fatalf("checkin_at = %q, want 00:00", c.Schedule.CheckinAt)
	}
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/config/`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -F .git/commit_msg.txt
```

提交信息：

```
feat(config): 签到默认按大陆时间 00:00，抖动是个键

签到的时刻按站点时区算（大陆时间，规格 §5.1），默认 00:00。抖动从写死
的 0~30 分钟变成一个键，默认 0：不抖。
```

---

### Task 5: 调度器 —— 下一趟签到是大陆时间的下一个 00:00

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/scheduler/scheduler_test.go` 里，把 `TestNextCheckin` 换成下面这两个：

```go
// TestNextCheckin says the daily check-in runs at the site's time.
//
// 站点按大陆时间算一天，签到要落在那一天里（规格 §5.1）。
func TestNextCheckin(t *testing.T) {
	at, err := time.Parse("15:04", "09:00")
	if err != nil {
		t.Fatal(err)
	}
	// 大陆时间 2026-11-13 08:00（= UTC 2026-11-13 00:00）：今天 09:00
	// 还没到，是今天。
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	got := NextCheckin(now, at, 0)
	want := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	// 大陆时间 10:00（= UTC 02:00）：今天 09:00 过了，是明天。
	now = time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	got = NextCheckin(now, at, 0)
	want = time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

// TestTheNextCheckinDoesNotDependOnTheMachinesZone says the daily
// check-in is at the site's midnight, whatever the machine's timezone is.
//
// 一台 UTC 的机器上「00:00」是大陆时间 08:00 —— 差 8 个钟头；一台东京
// 的机器上「00:00」是大陆时间前一天 23:00，签到落在站点的**前一天**，
// 那天的积分拿不到（规格 §5.1）。
func TestTheNextCheckinDoesNotDependOnTheMachinesZone(t *testing.T) {
	at, err := time.Parse("15:04", "00:00")
	if err != nil {
		t.Fatal(err)
	}
	// UTC 的 2026-11-18 20:00 是大陆时间的 11-19 04:00：下一个大陆时
	// 间的 00:00 是 11-20 的，也就是 UTC 的 11-19 16:00。
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	got := NextCheckin(now, at, 0)
	want := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/scheduler/ -run 'NextCheckin' -v`

Expected: FAIL —— 现在按本机时区算，`TestTheNextCheckinDoesNotDependOnTheMachinesZone` 拿到的是 `2026-11-18 20:00 UTC`（本机的下一个 00:00），不是 `2026-11-19 16:00 UTC`。

- [ ] **Step 3: 写实现**

`internal/scheduler/scheduler.go` 的 `NextCheckin` 改成：

```go
// NextCheckin gives the moment the daily check-in runs.
//
// The time is a wall-clock time in the site's zone: the site counts its
// days in China, so a check-in has to land on the site's day（规格 §5.1）。
// The machine's own timezone is not the site's, and using it puts the
// check-in on the wrong day — or eight hours late.
//
// When the moment has passed, it is tomorrow's. The jitter is added on
// top: a few instances together spread themselves out that way.
func NextCheckin(now time.Time, at time.Time, jitter time.Duration) time.Time {
	loc := store.CheckinZone()
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), at.Hour(), at.Minute(), 0, 0, loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	// 抖动不许把签到推到站点的第二天去：「23:50」抖 30 分钟就是 00:20 ——
	// 那是另一天的积分，那一份拿不到（规格 §5.1）。跨了就不抖。
	d := CheckinDelay(0, jitter)
	if store.DayKey(next.Add(d)) != store.DayKey(next) {
		d = 0
	}
	return next.Add(d)
}
```

抖动这件事要有测试（它是一个键，不是一个写死的数）—— 加在
`internal/scheduler/scheduler_test.go`：

```go
// TestTheNextCheckinIsJittered says the daily check-in may be held back.
//
// 抖动是配置里的一个键（schedule.checkin_jitter）：几个实例一起跑的时候
// 抖一下就散开了。零是它的默认值（不抖），写死 30 分钟是另一回事 ——
// 那是 §5.1 里点名要去掉的。
func TestTheNextCheckinIsJittered(t *testing.T) {
	at, err := time.Parse("15:04", "00:00")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	// 不抖的时候是那一刻。
	want := time.Date(2026, 11, 19, 16, 0, 0, 0, time.UTC)
	seen := map[time.Time]bool{}
	for i := 0; i < 40; i++ {
		got := NextCheckin(now, at, 30*time.Minute)
		if got.Before(want) || !got.Before(want.Add(30*time.Minute)) {
			t.Fatalf("next = %v, want between %v and %v", got, want, want.Add(30*time.Minute))
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("the check-in time does not jitter")
	}
}

// TestTheJitterDoesNotCrossTheSitesMidnight says the jitter cannot push
// the check-in into the next day.
//
// 「23:50」抖 30 分钟就是 00:20 —— 那是站点第二天的积分，那一份拿不到，
// 而且站点只给一天一次（规格 §5.1）。跨了就不抖。
func TestTheJitterDoesNotCrossTheSitesMidnight(t *testing.T) {
	at, err := time.Parse("15:04", "23:50")
	if err != nil {
		t.Fatal(err)
	}
	// UTC 的 11-18 20:00 是大陆时间的 11-19 04:00：今天 23:50 还没到。
	now := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		got := NextCheckin(now, at, 30*time.Minute)
		if day := store.DayKey(got); day != "2026-11-19" {
			t.Fatalf("the check-in landed on %s, want 2026-11-19", day)
		}
	}
}
```

`Run` 里那一行改成（抖不抖由配置说了算）：

```go
		next := NextCheckin(time.Now(), at, s.Cfg.Schedule.CheckinJitter.Duration())
```

`Run` 里解析 `checkin_at` 失败时的兜底也改（09:00 → 00:00）：

```go
	at, err := time.Parse("15:04", s.Cfg.Schedule.CheckinAt)
	if err != nil {
		at = time.Date(0, 1, 1, 0, 0, 0, 0, time.Local)
	}
```

`internal/scheduler/scheduler.go` 的 import 里加上 `"mmxcode/internal/store"`。

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/scheduler/ -v -run 'NextCheckin'`

Expected: PASS。

- [ ] **Step 5: 跑调度器这一层的测试**

Run: `go test ./internal/scheduler/`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit -F .git/commit_msg.txt
```

提交信息：

```
feat(scheduler): 下一趟签到是大陆时间的下一个 00:00

站点按大陆时间算一天，签到要落在那一天里。现在按本机时区算：一台 UTC
的机器上「00:00」是大陆时间 08:00，一台东京的机器上「00:00」是大陆时间
前一天 23:00 —— 那天的积分拿不到（规格 §5.1）。

抖动从写死的 0~30 分钟变成 schedule.checkin_jitter。
```

---

### Task 6: 调度器 —— 只签今天没签到的号，启动时补签

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/scheduler/scheduler_test.go` 末尾加：

```go
// TestTheCheckinSkipsTheAccountsThatSignedInToday says the accounts that
// were checked in today are not checked in again.
//
// 签过的再签一次，站点只会答 done，白花一次调用 —— 而今天没签到的那部
// 分，正是服务器 00:00 没开着时漏掉的那部分（规格 §5.2）。
func TestTheCheckinSkipsTheAccountsThatSignedInToday(t *testing.T) {
	var claims atomic.Int64
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
			claims.Add(1)
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
	for _, id := range []string{"acc_done", "acc_waiting"} {
		s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "1",
			CreatedAt: now, UpdatedAt: now,
		})
		s.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	// 这个号今天签过了。
	s.SaveCheckin(ctx, store.Checkin{
		AccountID: "acc_done", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 25, CreatedAt: now,
	})

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	results := sc.CheckinAll(ctx)
	if len(results) != 1 || results[0].AccountID != "acc_waiting" {
		t.Fatalf("the results = %+v, want just the waiting account", results)
	}
	if n := claims.Load(); n != 1 {
		t.Fatalf("the claims = %d, want 1", n)
	}
}

// TestTheCheckinIsMadeUpOnStartup says the accounts that have not signed
// in today are checked in when the process starts.
//
// 服务器 00:00 没开着的时候，那一天的签到就没人做了 —— 起进程的时候补
// 一趟，签的是今天还没签的号（规格 §5.2）。
func TestTheCheckinIsMadeUpOnStartup(t *testing.T) {
	var claims atomic.Int64
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
			claims.Add(1)
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
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sc.Run(runCtx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if claims.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the check-in was not made up on startup")
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/scheduler/ -run 'TheCheckinSkips|TheCheckinIsMadeUpOnStartup' -v`

Expected: FAIL —— `TestTheCheckinSkipsTheAccountsThatSignedInToday` 拿到两个结果（今天签过的那个也去签了）；`TestTheCheckinIsMadeUpOnStartup` 一直等不到（没有补签）。

- [ ] **Step 3: 写实现**

`internal/scheduler/scheduler.go` 的 `CheckinAll` 改成：

```go
// CheckinAll claims every account's daily credits.
//
// 只签今天还没签的号（规格 §5.2）：签过的再签一次，站点只会答 done，白
// 花一次调用 —— 而今天没签到的那部分，正是服务器 00:00 没开着时漏掉
// 的那部分。
//
// 「今天」是站点的那一天（大陆时间），跟管理器写进去的那一行是同一个
// 日子。
func (s *Scheduler) CheckinAll(ctx context.Context) []CheckinResult {
	accounts, err := s.Mgr.Store.AccountsWithoutCheckin(ctx, s.Mgr.Store.Today())
	if err != nil {
		fmt.Printf("checkin: the accounts to check in could not be read: %v\n", err)
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
```

`Run` 里，每天那一趟之前，加上启动时的补签；两处都用同一个 `checkinAll`。先把 `checkinAll` 这个函数加在 `// run runs a piece of work every so often, on the scheduler's account.` 上面（中间空一行）：

```go
// checkinAll runs the check-in and says what happened.
//
// 一趟签到一趟签到：两趟不叠着跑。起进程的补签和 00:00 那趟会碰上
// （23:59 起进程，补签还没跑完就到 00:00）—— 叠着跑就是同一个号被站点
// 拒一次（白花一次调用，规格 §5.2）。后到的那趟等前一趟跑完，名单是
// 现读的，前一趟签过的号不在里面。
func (s *Scheduler) checkinAll(ctx context.Context) {
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	for _, r := range s.CheckinAll(ctx) {
		if r.Error != "" {
			fmt.Printf("checkin %s: %s\n", r.AccountID, r.Error)
			continue
		}
		fmt.Printf("checkin %s: %s\n", r.AccountID, r.Outcome)
	}
}
```

`Scheduler` 上多一把锁（结构体里 `wg` 的下面）：

```go
	// checkinMu keeps two check-in passes from running at once.
	//
	// 起进程的补签和 00:00 那趟会碰上：23:59 起进程，补签还没跑完就
	// 到 00:00。叠着跑就是同一个号被站点拒一次 —— 白花一次调用（规格
	// §5.2）。
	checkinMu sync.Mutex
```

这件事也要有测试 —— 加在 `internal/scheduler/scheduler_test.go`：

```go
// TestTheCheckinPassesDoNotOverlap says two check-in passes do not run at
// the same time.
//
// 起进程的补签和 00:00 那趟会碰上：23:59 起进程，补签还没跑完就到 00:00。
// 叠着跑就是同一个号被站点拒一次 —— 白花一次调用（规格 §5.2）。
func TestTheCheckinPassesDoNotOverlap(t *testing.T) {
	var claims atomic.Int64
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
			claims.Add(1)
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
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	// 前一趟还在跑（这里就是拿着锁的那一趟）。
	sc.checkinMu.Lock()
	started := make(chan struct{})
	go func() {
		close(started)
		sc.checkinAll(ctx)
	}()
	<-started
	time.Sleep(200 * time.Millisecond)
	if n := claims.Load(); n != 0 {
		t.Fatalf("the claims = %d while another pass was running, want 0", n)
	}
	sc.checkinMu.Unlock()

	// 前一趟完了，后一趟才跑。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if claims.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the pass did not run after the other one was done")
}
```

（那个文件的 import 里多一个 `"sync/atomic"`；测签到次数的计数器都用
`atomic.Int64` —— 处理请求的是另一个线程。）

`Run` 里，`for {` 那个循环之前：

```go
	// 启动时补签：服务器 00:00 没开着的时候，那一天的签到就没人做了
	// （规格 §5.2）。起进程的时候补一趟：签的是今天还没签的号。
	//
	// 它是后台的活：网关不等它（一个号一个号地签，1000 个号要 1000 秒）。
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.checkinAll(ctx)
	}()
```

`Run` 的循环里那一趟换成：

```go
		case <-timer.C:
			s.checkinAll(ctx)
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/scheduler/ -v -run 'Checkin'`

Expected: PASS。

- [ ] **Step 5: 跑整个调度器的测试**

Run: `go test ./internal/scheduler/`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit -F .git/commit_msg.txt
```

提交信息：

```
feat(scheduler): 只签今天没签的号，起进程时补签一趟

签过的再签一次，站点只会答 done，白花一次调用 —— 而今天没签到的那部分
正是服务器 00:00 没开着时漏掉的那部分（规格 §5.2）。

签到是后台的活：网关不等它（1000 个号要 1000 秒）。
```

---

### Task 7: 文档 —— README 和那句校验的错话

**Files:**
- Modify: `cmd/mmxcode/serve.go`
- Modify: `README.md`
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`

- [ ] **Step 1: 改 `cmd/mmxcode/serve.go` 的那句错话**

```go
			// 签到时间写错的话，调度器会默默按 00:00 跑。用户以为自己
			// 配了别的点，就永远等不到那个点的签到。起不来比悄悄跑错好。
			if _, err := time.Parse("15:04", e.cfg.Schedule.CheckinAt); err != nil {
				return fmt.Errorf("schedule.checkin_at = %q; want HH:MM", e.cfg.Schedule.CheckinAt)
			}
```

- [ ] **Step 2: 改 README 的配置表**

`README.md` 的配置表里，`schedule.checkin_at` 那一行改成：

```
| `schedule.checkin_at` | `"00:00"` | 每天签到的时间，**大陆时间**，`HH:MM`。站点按大陆时间算一天，签到要落在那一天里 |
| `schedule.checkin_jitter` | `"0"` | 签到最多往后推多久。几个实例一起跑的时候抖一下就散开了 |
```

发出去的两份配置也要改：它们写着 `09:00`，而那个 `09:00` 现在说的是
**大陆时间**的 09:00 —— 照着配置跑的人，签到比说好的晚九个钟头。

`config.example.yaml` 和 `config.docker.yaml` 的 `schedule` 那一段改成：

```yaml
schedule:
  # 每天签到的时间，大陆时间（站点按大陆时间算一天）
  checkin_at: "00:00"
  # 签到最多往后推多久（几个实例一起跑的时候抖一下就散开了）
  checkin_jitter: 0s
  usage_refresh: 15m
  token_refresh: 5m
  egress_health: 2m
```

这两份配置也要有测试（它们跟默认值说的一样）—— 加在
`internal/config/config_test.go`：

```go
// TestTheShippedConfigsCheckInAtTheSitesMidnight says the configs that
// ship with the program agree with the default.
//
// 发出去的配置写着 09:00，README 写着 00:00：照着配置文件跑的人，签到
// 就落在 09:00 —— 比说好的晚九个钟头（规格 §5.1）。而且 09:00 现在是
// **大陆时间**的 09:00：老配置里的那个 09:00 说的是本机时区。
func TestTheShippedConfigsCheckInAtTheSitesMidnight(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "config.docker.yaml"} {
		c, err := Load(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Schedule.CheckinAt != Default().Schedule.CheckinAt {
			t.Errorf("%s: checkin_at = %q, want %q",
				name, c.Schedule.CheckinAt, Default().Schedule.CheckinAt)
		}
		if c.Schedule.CheckinJitter.Duration() != Default().Schedule.CheckinJitter.Duration() {
			t.Errorf("%s: checkin_jitter = %s, want %s",
				name, c.Schedule.CheckinJitter, Default().Schedule.CheckinJitter)
		}
	}
}
```

- [ ] **Step 3: README 加一节「### 签到」**

在 `### 账号池` 之前加（这一节和 `### 账号池` 之间空一行）：

```markdown
### 签到

每个账号每天签一次到，签到给的积分进各自的账本。

- **什么时候签**：每天一趟，**大陆时间** `schedule.checkin_at`（默认
  `00:00`）。站点按大陆时间算一天，签到要落在那一天里 —— 差 8 个钟头
  就签到前一天的积分，那一份拿不到。
- **起进程时补签**：服务器 00:00 没开着的时候，那一天的签到就没人做了。
  起进程的时候补一趟，签的是**今天还没签**的号。
- **只签今天没签的号**：签过的再签一次，站点只会答 done，白花一次调用。
  签到是串行的，一个号一个号地签 —— 1000 个号 1000 秒。
- **签到的积分进池**：签到给了积分，池里那一份是进池那一刻的数。网关
  每一趟补号（`usage.refresh.scan_every`）问一次库，把签到的积分换进
  池里那份数；不在池里的签到过的号排在补号候选的最前面，它们自己会进
  池。池里那份数只是「有没有额度」的提示，确切是多少由读额度那一趟慢
  慢补上。

```

- [ ] **Step 4: README 的「### 账号池」补一条**

那节的最后加一条（跟别的条目之间空一行）：

```markdown
- **签到的号排在候选的最前面**：签到给了积分（七天有效），它一定还有额
  度；而从没读过的号有没有额度是不知道的 —— 填池那一趟要读它，读到 0
  就白读一次。

```

- [ ] **Step 5: 跑一遍全部测试**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: 全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add cmd/mmxcode/serve.go README.md
git commit -F .git/commit_msg.txt
```

提交信息：

```
docs: 签到的时间、时区和补签写进 README

签到按大陆时间 00:00（站点按大陆时间算一天），起进程时补签今天漏掉
的号，签到的积分进池（规格 §5.1、§5.2、§5.3）。
```

---

## 评审后的修补

两轮评审（规格对不对、代码好不好）在实现之后跑过。它们找到的东西都补进
上面的任务里了 —— 计划跟代码是逐字对着的（`check_plan_files.py` 报
stale declarations: 0）。

| 找到的 | 在哪儿 |
|---|---|
| 发出去的两份配置还写着 `09:00` —— 那个 `09:00` 现在是大陆时间的 09:00 | Task 7 |
| 索引那条只跑在 migrateConform 里，老库（版本 5）走不到：一年 36 万行的全表扫 | Task 1（版本 6 的那一步） |
| 「今天」有三个钟（库的钟和墙上时间各算各的） | Task 1（`Store.Today`）、Task 2、Task 3、Task 6 |
| 启动时的补签和 00:00 那趟会叠着跑：同一个号被站点拒一次 | Task 6（`checkinMu`） |
| 抖动可能把签到推到站点的第二天（`23:50` 抖 30 分钟） | Task 5 |
| 抖动这个键没有测试钉着 | Task 5 |
| `NextCheckin`、`CheckinAll` 的注释重了、跟代码对不上 | Task 5、Task 6 |
| `OptionalDuration` 的注释说「只有一个键的零是合法的」 | Task 4 |
| `PoolCandidates` 的注释里没有「今天签过的排前面」这条 | Task 3 |
| 测签到次数的计数器是普通变量（处理请求的是另一个线程） | Task 6 |

两件没改的，说明理由：

- **签不上的号不在签到名单里**（`AccountsWithoutCheckin` 只给 active 的
  号）：签不上的号要的是重登，不是签到。它签回来的那天是 active 了，下一
  趟自然就在名单里。
- **名单读不出来的时候那一天就过去了**：`CheckinAll` 读到名单失败会打印
  出来（`checkin: the accounts to check in could not be read: …`），日志里
  看得见；库是本地的 SQLite，一个偶发的错就是极小概率的事，为它加一遍重
  试不如让它在日志里说话。

---

## 收尾

- [ ] **Step 1: 全套测试**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: 全部 PASS。

- [ ] **Step 2: 本机跑一趟**

```bash
go run ./cmd/mmxcode serve --config config.example.yaml
```

看两件事：日志里有没有 `checkin <号>: claimed`，以及页面上那些号的余额在
签到之后有没有变（变之前是「签到待补」）。

- [ ] **Step 3: 计划跟代码对一遍**

把计划里的 Go 块抽出来，编译一遍（`docs/superpowers/tools/` 里的脚本），
确认计划跟代码逐字一致。

- [ ] **Step 4: 交出去**

按 `superpowers:finishing-a-development-branch` 走：合回 `feature/usage-refresh`。
