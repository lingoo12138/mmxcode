# SQLite 调优 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 纯 Go 的 SQLite（`modernc.org/sqlite`）留着，动手的是索引、查询和 pragma：签到的历史有界、热查询按「怎么问」建索引、读额度那一趟不再扫全表。

**Architecture:** 四个新索引（迁移的第 7 步）把热查询的临时排序和随机探测去掉；`AccountsWithoutCheckin` 改成 `NOT IN`（一天签到的号一次拿全）；`LatestUsageSnapshots` 改成从 `accounts` 出发；`LatestCheckins` 换成 `CheckinTimes`（只问最近七天的签到 —— 签到的积分七天过期，再往前的签到不影响「待补」那一档）；签到的历史每天删一次（留 30 天）；页缓存和 `synchronous` 从配置里来。`synchronous=normal` 让一笔写快了 14 倍（874µs → 64µs），代价是「提交只是写进 WAL」—— 于是丢不得的那两笔（续签后的 token、被清掉的「用完了」标记）走同一个库的**另一条会 fsync 的连接**；签到的记录一批一批写，一个号一个事务要把签到的那几页重写一遍（一千个号 17 MB 的日志，合成一批是 1 MB）。另外两处是这个版本翻出来的：token 每次比较都砍到毫秒（否则每个请求都重写一遍 token 行，写锁排长队就是 `SQLITE_BUSY`），以及 `sqlite:` 一节的三个键都在启动时查（负的页缓存、少于七天的保留天数以前是悄悄按别的意思办的）。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`（WAL，纯 Go，不用 CGO）、`spf13/cobra`

设计文档：`docs/superpowers/specs/2026-11-19-mmxcode-sqlite-tuning-design.md`

分支：`feature/sqlite-tuning`（从 `feature/usage-refresh` 开）

---

## 这一版不做的

| 不做 | 为什么 |
|---|---|
| 换 CGO 版 SQLite | 用户说了不换。纯 Go 的 `modernc.org/sqlite` 留着 |
| 换 JSON 文件存 | 上一轮的结论：JSON 要自己实现查询引擎，内存反而更高 |
| 写的那条也优化 | 写是「一笔一笔记」。`synchronous=normal` 已经把一笔写从 874µs 降到 64µs，剩下的要靠批量，不在这次的范围里 |
| 删 `sessions` | 会话表是亲和的依据，删了老对话的缓存命中就没了 |
| 动 `usage_snapshots` 的清理规矩 | 一刻钟之外、每号最新的一条留着（规格 §3.4） |

---

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/store/store.go` | 迁移的第 7 步（索引）；`CheckinTimes` 换掉 `LatestCheckins`；`LatestUsageSnapshots` 改成从账号出发；`PruneCheckins`；多一条会 fsync 的 `durable` 连接；`SaveCheckins`；`AtMillisecond`；`Open` 的选项 |
| `internal/store/schema.go` | 新的索引清单 |
| `internal/store/checkins.go` | `AccountsWithoutCheckin` 改成 `NOT IN` |
| `internal/store/checkins_query_test.go` | 索引的名字、问法（查询计划） |
| `internal/store/store_test.go` | `SaveCheckins` 的行为；那条 fsync 的连接 |
| `internal/usage/round.go` | 读额度那一趟问「最近的签到」 |
| `internal/manager/manager.go` | 签到（网络）和写记录（库）分开；比 token 的时候两边都砍到毫秒 |
| `internal/manager/manager_test.go` | token 只写一次；`RecordCheckins` |
| `internal/scheduler/scheduler.go` | 签到那一趟跑完删老的签到；记录一批一批写 |
| `internal/scheduler/scheduler_test.go` | 五十个一批 |
| `internal/config/config.go` | 新的 `sqlite:` 一节；那三个键的检查 |
| `internal/config/config_test.go` | 那些检查的测试 |
| `cmd/mmxcode/root.go` | 把配置交给库 |
| `internal/store/bench_test.go`（新） | 量具：八个基准 |
| `config.example.yaml` / `config.docker.yaml` | `sqlite:` 一节 |

---

### Task 1: 迁移的第 7 步 —— 按「怎么问」建索引

**Files:**
- Modify: `internal/store/store.go`（`schemaVersion`、迁移清单、新的一步）
- Modify: `internal/store/schema.go`（索引清单）
- Modify: `internal/store/checkins_query_test.go`（索引的名字变了）

现在库里的索引是照着「表长什么样」建的，不是照着「怎么问」建的
（`internal/store/schema.go:265-269`）：

```go
// The indexes.
//
// 索引是照着「怎么问」建的，不是照着「表长什么样」建的：每一条都对应
// 一条被量出来的热查询。
//
//   - checkins(day, account_id, created_at, credit)：今天谁签到、给了多
//     少积分（DayCheckins）、今天谁还没签（AccountsWithoutCheckin）。
//     旧的那条 idx_checkins_day 只盖住 day 一列，每一行都要回表。
//   - checkins(account_id, created_at DESC, id DESC)：这个号最近签到的
//     时候（CheckinTimes）。旧的 UNIQUE(account_id, day) 按日子排，不按
//     时刻排。
//   - usage_snapshots(account_id, created_at DESC, id DESC)：每个号最新
//     的一条读数（LatestUsageSnapshots）。旧的 idx_usage_account 少了
//     id，排到最后一条的时候还要再排一次。
//   - sessions(account_id)：这个号绑着哪些对话（PoolCandidates 的排序）。
//
// 别的都试过，没有好处（1000 个号 / 36 万行签到的库上量的，每条量三遍
// 取最快的那一遍）：
//
//   - accounts(status, created_at, id)：填池那条查询 6.15 ms 到 6.79 ms
//     （顺序扫变成了按索引随机取行）。
//   - sessions(account_id, updated_at)：写会话（一批 100 个 key）0.77 ms 到
//     0.82 ms —— 请求的路上一分都不能慢。
//   - sessions(updated_at)：数活跃会话 0.196 ms 到 0.247 ms，而它省下的
//     那点时间在一条每 20 秒才跑一次的路上。
//
// 每一条都要在每次写的时候多维护一份，所以每一条索引都对应一条具体的、
// 被量出来的查询。
//
// 被新索引完全盖住的旧索引在这里删掉：留着就是每次写都多维护一份。
var indexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_checkins_day_account ON checkins(day, account_id, created_at, credit)",
	"CREATE INDEX IF NOT EXISTS idx_checkins_account_created ON checkins(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_usage_account_created ON usage_snapshots(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_account ON sessions(account_id)",
	"DROP INDEX IF EXISTS idx_checkins_day",
	"DROP INDEX IF EXISTS idx_usage_account",
	"DROP INDEX IF EXISTS idx_checkins_account",
}

```

要问的几件事，各自有各的形状：

1. 「今天谁签到、给了多少积分」→ `checkins(day, account_id, created_at, credit)`
   —— 旧的那条 `idx_checkins_day` 只盖住 `day` 一列，每一行都要回表；
2. 「这个号最近签到的时刻」→ `checkins(account_id, created_at DESC, id DESC)`
   —— 旧的那条 `UNIQUE(account_id, day)` 按日子排，不按时刻排；
3. 「每个号最新的一条读数」→ `usage_snapshots(account_id, created_at DESC, id DESC)`
   —— 旧的 `idx_usage_account` 少了 `id`，排到最后一条的时候还要再排一次；
4. 「这个号绑着哪些对话」→ `sessions(account_id)` —— 这条本来就有（填池的
   `EXISTS` 要它），留着，只是把它一起写进这份清单。

被新索引**完全盖住**的旧索引（新的列更多、顺序更全）在这一步删掉：留着
就是每次写都多维护一份。**两条**：`idx_checkins_day` 和 `idx_usage_account`
（`idx_checkins_account` 早就不在了）。

**试过、没有好处、所以没有建的**（在 1000 个号 / 36 万行签到的库上量的，
每条量三遍取最快的那一遍）：

| 没建的 | 为什么 |
|---|---|
| `accounts(status, created_at, id)` | 填池那条查询 6.15 ms → **6.79 ms**（慢了）。顺序扫变成了按索引随机取行 |
| `sessions(account_id, updated_at)` | 写会话（一批 100 个 key）0.77 ms → **0.82 ms**（慢了）—— 请求的路上一分都不能慢 |
| `sessions(updated_at)` | 数活跃会话 0.196 ms → **0.247 ms**（慢了）。而它省下的那点时间在一条每 20 秒才跑一次的路上 |

三条都在噪声边上，而每一条都要在每次写的时候多维护一份。**索引不是越多越好**：
每一条都要在每次写的时候维护，而写的那条路上有请求。
所以每一条索引都对应一条**具体的、被量出来的**查询。

- [ ] **Step 1: 写失败的测试**

改 `internal/store/checkins_query_test.go` 里那个查索引名字的测试
（`TestTheCheckinIndexReachesAnOldFile`）：旧的 `idx_checkins_day` 换成了
`idx_checkins_day_account`，它盖住的日子那一列还在，只是多带了三列。

```go
// TestTheCheckinIndexReachesAnOldFile says a file that is already at the
// old version gets the index on the day.
//
// 老库停在版本 6，走不到 migrateConform 里那几条索引（那一步只把版本 1
// 的库带到版本 2）。索引是新的那一步建的：没有它，「今天谁签到了」就是
// 全表扫 —— 一天一个号一行，1000 个号一年 36 万行（规格 §5.3）。
func TestTheCheckinIndexReachesAnOldFile(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// 把它退回到老版本：索引没有。
	if _, err := s.db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_checkins_day_account"); err != nil {
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
		if name == "idx_checkins_day_account" {
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

同一个文件里的 `TestTheCheckinsHaveAnIndexOnTheDay` 也改：名字换成
`idx_checkins_day_account`，注释里写清楚这条索引盖住哪几列。

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run "TestTheCheckin" -v
```

失败：`the old file did not get the index on the day`（索引还不存在）。

- [ ] **Step 3: 实现**

`internal/store/schema.go` —— 旧的索引清单换成新的：

```go
// The indexes.
//
// 索引是照着「怎么问」建的，不是照着「表长什么样」建的：每一条都对应
// 一条被量出来的热查询。
//
//   - checkins(day, account_id, created_at, credit)：今天谁签到、给了多
//     少积分（DayCheckins）、今天谁还没签（AccountsWithoutCheckin）。
//     旧的那条 idx_checkins_day 只盖住 day 一列，每一行都要回表。
//   - checkins(account_id, created_at DESC, id DESC)：这个号最近签到的
//     时候（CheckinTimes）。旧的 UNIQUE(account_id, day) 按日子排，不按
//     时刻排。
//   - usage_snapshots(account_id, created_at DESC, id DESC)：每个号最新
//     的一条读数（LatestUsageSnapshots）。旧的 idx_usage_account 少了
//     id，排到最后一条的时候还要再排一次。
//   - sessions(account_id)：这个号绑着哪些对话（PoolCandidates 的排序）。
//
// 别的都试过，没有好处（1000 个号 / 36 万行签到的库上量的，每条量三遍
// 取最快的那一遍）：
//
//   - accounts(status, created_at, id)：填池那条查询 6.15 ms 到 6.79 ms
//     （顺序扫变成了按索引随机取行）。
//   - sessions(account_id, updated_at)：写会话（一批 100 个 key）0.77 ms 到
//     0.82 ms —— 请求的路上一分都不能慢。
//   - sessions(updated_at)：数活跃会话 0.196 ms 到 0.247 ms，而它省下的
//     那点时间在一条每 20 秒才跑一次的路上。
//
// 每一条都要在每次写的时候多维护一份，所以每一条索引都对应一条具体的、
// 被量出来的查询。
//
// 被新索引完全盖住的旧索引在这里删掉：留着就是每次写都多维护一份。
var indexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_checkins_day_account ON checkins(day, account_id, created_at, credit)",
	"CREATE INDEX IF NOT EXISTS idx_checkins_account_created ON checkins(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_usage_account_created ON usage_snapshots(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_account ON sessions(account_id)",
	"DROP INDEX IF EXISTS idx_checkins_day",
	"DROP INDEX IF EXISTS idx_usage_account",
	"DROP INDEX IF EXISTS idx_checkins_account",
}

```

`internal/store/store.go` —— 版本往前挪一步，迁移清单里加上这一步：

```go
// schemaVersion is the version a file is at when the steps are done.
const schemaVersion = 7

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
	migrateIndexes,
}

```

`internal/store/store.go` —— 新的一步（在 `migrateCheckinIndex` 后面）：

```go
// migrateIndexes gives the tables the indexes the queries ask for.
//
// 它们是第 7 步建的：老库停在版本 6，走不到 migrateConform 里那几条索引
// （那一步只把版本 1 的库带到版本 2）。
//
// 每建一条索引都要重写一遍表，所以这一步在一个事务里：失败就回滚，老
// 库原样不动。
func migrateIndexes(ctx context.Context, conn *sql.Conn) error {
	return runStep(ctx, conn, 7, func(tx *sql.Tx) error {
		for _, q := range indexStatements {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
		}
		return nil
	})
}

```

**注意**：`internal/store/schema.go` 里那句「那条按天的索引不在这里：它由
migrateCheckinIndex 建（版本 6 的那一步）」的注释要改掉 —— 现在按天的索引
在 `indexStatements` 里，由版本 7 的那一步建。第 6 步（`migrateCheckinIndex`）
改成建新的那条（`idx_checkins_day_account`），免得新库先建一条旧的再删。

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ -v
```

`TestTheCheckinIndexReachesAnOldFile` 和 `TestTheCheckinsHaveAnIndexOnTheDay`
都过；别的也都过。

- [ ] **Step 5: 量一下**

```bash
go test ./internal/store/ -run XXX -bench . -benchtime 30x
```

把数字记在下面（Task 6 会再跑一遍正式的）。

---

### Task 2: `AccountsWithoutCheckin` —— 一天签到的号一次拿全

**Files:**
- Modify: `internal/store/checkins.go`

现在是「一个号一次问一次签到没有」（1000 个号 = 1000 次对签到表的随机探测，
签到的表越大越贵：一年 36 万行的库上是 4.05 ms）。同一天签到的那些号是可以
一次拿全的。

- [ ] **Step 1: 写失败的测试**

`internal/store/checkins_query_test.go` 里已经有
`TestTheAccountsWithoutACheckinAreTheOnesThatHaveNotSignedInToday` ——
它验的是名单对不对。**名单不变**，只是问法变了，所以这个测试不用改。

要加的是一条「问法」的测试：签到的表很大的时候，问法不能退化成全表扫。
测「问法」的办法是看 SQLite 怎么说（`EXPLAIN QUERY PLAN`）：

```go
// TestTheAccountsWithoutACheckinAskTheDayAtOnce says the day's list is
// asked for in one go.
//
// 一个号一次问一次签到没有，1000 个号就是 1000 次对签到表的随机探测
// （签到的表越大越贵：一年 36 万行的库上是 4.05 ms）。同一天签到的那些号
// 是可以一次拿全的：签到表按日子排，那一天就是连着的一千行。
//
// 断言抓的是**我们自己的**名字（索引名），不是 SQLite 的措辞 ——
// 「LIST SUBQUERY」那些词是 SQLite 的话，升级一下就可能换一句，而它要防
// 的那件事（那一天的签到一次拿全）没变。
func TestTheAccountsWithoutACheckinAskTheDayAtOnce(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "waiting")

	plan := queryPlan(t, s, accountsWithoutCheckinSQL, StatusActive, "2026-11-18")
	if !strings.Contains(plan, "idx_checkins_day_account") {
		t.Fatalf("the plan does not use the index on the day: %s", plan)
	}
}

```

`queryPlan` 这个帮手放在 `internal/store/store_test.go` 里（那个文件已经有
`openTest`、`makeAccount` 这些帮手）：

```go
// queryPlan says what SQLite is going to do with a statement.
//
// 它是量「问法」的工具：同一条查询，问法不同，SQLite 做的事就不同 ——
// 而做的事不同，快慢差着十倍（一年 36 万行的签到表上，「一个号一次问一
// 次签到没有」是 4.05 ms，「一天签到的号一次拿全」是 1.52 ms）。
func queryPlan(t *testing.T, s *Store, q string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		b.WriteString(detail)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheAccountsWithoutACheckinAskTheDayAtOnce -v
```

失败：计划里没有 `LIST SUBQUERY`（现在的写法是 `CORRELATED SCALAR SUBQUERY`）。

- [ ] **Step 3: 实现**

`internal/store/checkins.go`：那句 SQL 拿出来成一个常数（测试要对着它问
SQLite「你打算怎么做」）：

```go
// accountsWithoutCheckinSQL is the query that gives the accounts that
// were not checked in on a day.
//
// 它是签到那一趟的名单：只签今天还没签的号（规格 §5.2）。
//
// 问法是「今天签到过的号有哪些」一次拿全，再去掉它们（NOT IN），不是一
// 个号一次问一次签到没有：后者是 1000 次对签到表的随机探测，签到的表越
// 大越贵（一年 36 万行的库上：4.05 ms 对 1.52 ms）。
//
// NOT IN 遇到 NULL 会一个都不返回。签到的 account_id 是 NOT NULL 的
// （schema.go），迁移的时候老行补的也是空串不是 NULL，所以不会。
const accountsWithoutCheckinSQL = `
		SELECT a.id, a.name, a.user_id, a.email, a.status, a.proxy_group, a.proxy_url,
			a.last_error, a.created_at, a.updated_at
		FROM accounts a
		WHERE a.status = ?
		  AND a.id NOT IN (SELECT c.account_id FROM checkins c WHERE c.day = ?)
		ORDER BY a.created_at, a.id`

// AccountsWithoutCheckin gives the accounts that were not checked in on a
// day.
//
// 只有 active 的号：签不上的号签不了（signed_out 是 token 掉了，要的是
// 重登，不是签到）。
func (s *Store) AccountsWithoutCheckin(ctx context.Context, day string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, accountsWithoutCheckinSQL, StatusActive, day)
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

（那个测试要对着**生产用的那句 SQL** 问计划，所以 SQL 是常数，测试拿着
它用 —— 不然测试验的是它自己写的那一份，生产用的那一份改了也没人管。）

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ -v
```

- [ ] **Step 5: 量一下**

`BenchmarkAccountsWithoutCheckin` 应该从 3.62 ms 掉到 2.73 ms 上下（一年
36 万行的库上是 4.05 ms 掉到 1.52 ms）。

---

### Task 3: 读额度那一趟 —— 只问最近的签到

**Files:**
- Modify: `internal/store/store.go`（`LatestCheckins` → `CheckinTimes`）
- Modify: `internal/usage/round.go`（问最近七天的）
- Modify: `internal/store/store_test.go`（用 `LatestCheckins` 的测试）

现在读额度那一趟每 30 秒问一次「每个号最后一次签到是什么时候」，那条
查询的形状是「扫一遍签到表的全表，每一行再问一次这个号最后一次是哪一行」，
而那个「再一次」没有能用的索引：**一年 36 万行的库上，那条查询要 138 秒**。
读额度那一趟有 30 秒的预算，于是每一趟都超时，一趟也读不完。

而这一档（「签到待补」）其实只关心**最近**的签到：

- `checked.After(read)`：签到比上一次读额度还新 → 要补读；
- `now.Sub(checked) < retryWindow`（10 分钟）：刚签到，读回来却没数 → 再试。

一条**七天以前**的签到，两条都占不上（积分早过期了，10 分钟的窗口也早就
过去了）。而七天是签到积分自己的有效期（规格 §5.3）。所以问「最近七天的
签到」跟问「全部签到」在这一档上是一回事。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go` 里有一条用 `LatestCheckins` 的测试（找
「每个号最新的一次签到」）。把它改成 `CheckinTimes`：

```go
// TestTheCheckinTimesAreTheOnesSinceAMoment says the check-in times
// since a moment are given, one per account.
//
// 两个签到可以是一天里的同一刻：日子分不开它们，时刻分得开。
//
// 它问的是「最近」：签到的积分七天就过期了，再往前的签到既不会让一个号
// 变成「待补」，也不会在十分钟的重试窗口里（规格 §5.3）。
func TestTheCheckinTimesAreTheOnesSinceAMoment(t *testing.T) {
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
	got, err := s.CheckinTimes(context.Background(), now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("the check-in times: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the check-in times = %d, want one per account", len(got))
	}
	at := map[string]time.Time{}
	for _, c := range got {
		at[c.AccountID] = c.At
	}
	// acc_1 的最新一次是后来改的那一行：它的 id 还是当初那个，时刻是最新
	// 的。
	if a := at["acc_1"].UnixMilli(); a != now.Add(time.Minute).UnixMilli() {
		t.Errorf("acc_1's check-in was at %v, want the newest one", a)
	}
	if a := at["acc_2"].UnixMilli(); a != now.UnixMilli() {
		t.Errorf("acc_2's check-in was at %v, want its own", a)
	}
	// 再往前的就不算了：问的是「最近」。
	none, err := s.CheckinTimes(context.Background(), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("the check-in times: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("the check-in times = %+v, want none", none)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheCheckinTimesAreTheOnesSinceAMoment -v
```

失败：编译不过（`CheckinTimes` 还没有）。

- [ ] **Step 3: 实现**

`internal/store/store.go` —— 把 `LatestCheckins` 换成 `CheckinTimes`：

```go
// CheckinTime is when an account was checked in.
type CheckinTime struct {
	// AccountID is the account.
	AccountID string
	// At is when it was checked in.
	At time.Time
}

// CheckinTimes gives, for every account checked in since a moment, the
// time of that check-in.
//
// 它是「签到待补」那一档要的东西：这个号最后一次签到是什么时候（规格
// §5.3）。签到给了积分，卡上还摆着签到之前的旧数 —— 那种号要补读一次
// 额度。
//
// 问的是「最近」，不是「全部」：签到的积分七天就过期了（规格 §5.3），七
// 天以前的签到既不会让一个号变成「待补」，也不会在十分钟的重试窗口里。
// 而问「全部」的那条查询在有历史的库上慢得没法看：一年 36 万行，138 秒
// —— 而这一档每 30 秒问一次。
//
// 每个号一条：它在这段时间里最后一次签到。时刻说了算（MAX 出来的就是
// 最大的时刻），平局的时候是哪一个不影响 —— 这里的时刻就是全部。
func (s *Store) CheckinTimes(ctx context.Context, since time.Time) ([]CheckinTime, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, MAX(created_at) FROM checkins
		WHERE created_at >= ? GROUP BY account_id`, ms(since))
	if err != nil {
		return nil, fmt.Errorf("the check-in times: %w", err)
	}
	defer rows.Close()
	var out []CheckinTime
	for rows.Next() {
		var (
			t  CheckinTime
			at int64
		)
		if err := rows.Scan(&t.AccountID, &at); err != nil {
			return nil, fmt.Errorf("the check-in times: %w", err)
		}
		t.At = fromMS(at)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the check-in times: %w", err)
	}
	return out, nil
}

```

`internal/usage/round.go` —— 常数：

```go
// checkinWindow is how far back a check-in can still matter.
//
// 签到给的积分七天有效（规格 §5.3），而「签到待补」问的是「这个号签到
// 给了积分，可是卡上还摆着旧数」—— 那种情况只有刚签到的号才有：积分过
// 期了，补读它读回来的也是过期数。
//
// 所以问「最近七天的签到」跟问「全部签到」是一回事，而后者在有历史的库
// 上慢得没法看（一年 36 万行：138 秒，而这一档每 30 秒问一次）。
const checkinWindow = 7 * 24 * time.Hour

```

`internal/usage/round.go` —— `Targets` 里改用它：

```go
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
	now := time.Now()
	checkins, err := r.Store.CheckinTimes(ctx, now.Add(-checkinWindow))
	if err != nil {
		return nil, err
	}
	exhausted, err := r.Store.ListExhausted(ctx)
	if err != nil {
		return nil, err
	}

	snapAt := map[string]time.Time{}
	snapHas := map[string]bool{}
	for _, s := range snaps {
		snapAt[s.AccountID] = s.CreatedAt
		snapHas[s.AccountID] = s.HasAmounts
	}
	checkAt := map[string]time.Time{}
	for _, c := range checkins {
		checkAt[c.AccountID] = c.At
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

（原来那行 `now := time.Now()` 在 `exhausted` 后面，挪到 `CheckinTimes` 前面
—— 问「最近七天」要用的是问之前那一刻。）

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ ./internal/usage/ -v
```

- [ ] **Step 5: 量一下**

`BenchmarkCheckinTimes` 应该在一毫秒上下。

---

### Task 4: `LatestUsageSnapshots` —— 从账号那边走

**Files:**
- Modify: `internal/store/store.go`（`LatestUsageSnapshots`）

这条查询在卡片那条路上：用户点一次卡片就问一次「每个号最新的一条读数是
多少」。

现在的写法是**从读数表里扫**，每读一行问一次「这个号最新的是哪一行」。
那条「再一次」在旧的索引（只有 `account_id`）上要把这个号的读数排一遍 ——
一个号一刻钟一条的话，一天就是几千行。

所以：读数表比账号多得多，从读数那边走就是「每读一行问一次」；从**账号**
那边走是一千次查找 —— 账号有几个就是几次。

「最新」的判法一个字不动：时刻优先，id 平局（规格 §3.4：一条读数开始的
时候打时刻、结束的时候写下来，两条重叠的读数里旧的那条 id 反而大）。没有
读过的号不在里面（是 `JOIN`，不是 `LEFT JOIN`：规格 §3.4 说卡上摆的是每个
号最新的一条读数，没有读过的号没有那条）。

- [ ] **Step 1: 写失败的测试**

这条查询的行为**一个字没变**（判「最新」的规矩、没读过的号不在里面，都
没动），所以 `internal/store/store_test.go` 里那几条用
`LatestUsageSnapshots` 的测试就是它的测试 —— 不用新写。

要新写的是**问法**的测试：这条查询不能退化成扫读数表。看 SQLite 怎么说
（`EXPLAIN QUERY PLAN`）：

```go
// TestTheLatestUsageSnapshotsAreAskedPerAccount says the newest reading
// of every account is asked for account by account.
//
// 这条查询在卡片那条路上（用户点一次卡片问一次「每个号最新的一条读数是多
// 少」）。从读数表里扫、每读一行问一次「这个号最新的是哪一行」，在只有
// account_id 的索引上要把这个号的读数排一遍 —— 而读数表比账号多得多（一个
// 号一刻钟一条的话，一天就是几千行）。从账号那边走是一千次查找。
//
// 断言抓的是**我们自己的**名字（索引名、表的别名），不是 SQLite 的措辞
// ——「SCAN」「SEARCH」「LIST SUBQUERY」那些词是 SQLite 的话，升级一下
// 就可能换一句，而它要防的那件事（读数表不被扫）没变。
func TestTheLatestUsageSnapshotsAreAskedPerAccount(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "one")

	plan := queryPlan(t, s, latestUsageSnapshotsSQL)
	// 读数表只被查，不被扫：那个别名是 u（和子查询里的 s2）。
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "SCAN u") || strings.Contains(line, "SCAN s2") {
			t.Fatalf("the plan scans the readings: %s", plan)
		}
	}
	// 找最新那一条走的是那条索引（account_id, created_at）。
	if !strings.Contains(plan, "idx_usage_account_created") {
		t.Fatalf("the plan does not use the index on the readings: %s", plan)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheLatestUsageSnapshotsAreAskedPerAccount -v
```

失败：编译不过（`latestUsageSnapshotsSQL` 还没有）。

- [ ] **Step 3: 实现**

`internal/store/store.go` —— 那句 SQL 拿出来成一个变量（测试要对着它问
SQLite「你打算怎么做」），查询改成从账号出发：

```go
// latestUsageSnapshotsSQL is the query that gives every account's newest
// usage reading.
//
// 它是从账号那边走的，不是从读数那边走的：读数表比账号多得多（一个号一
// 刻钟一条的话，一天就是几千行），从读数那边走就是「每读一行问一次这个
// 号最新的是哪一行」—— 110 万行的时候是 1.5 秒，而这条路在卡片的路上
// （用户点一次卡片就等 1.5 秒）。从账号那边走是一千次查找：12 毫秒。
//
// 没有读过的号不在里面：它是 JOIN，不是 LEFT JOIN（规格 §3.4：卡上摆的
// 是每个号最新的一条读数，没有读过的号没有那条）。
//
// 排的是 a.id 而不是 u.account_id：这两个是同一个号（u 就是 a 那个号的
// 最新一条），而账号的主键索引本来就是按 id 排的 —— 排 u.account_id 要让
// SQLite 临时排一遍（USE TEMP B-TREE FOR ORDER BY）。
var latestUsageSnapshotsSQL = `
	SELECT ` + usageSnapshotColumns("u") + `
	FROM accounts a JOIN usage_snapshots u ON u.id = (
		SELECT s2.id FROM usage_snapshots s2
		WHERE s2.account_id = a.id
		ORDER BY s2.created_at DESC, s2.id DESC
		LIMIT 1)
	ORDER BY a.id`

```

（`usageSnapshotColumns` 是函数，所以是 `var` 不是 `const` —— 包里的变量
按依赖的顺序初始化，`usageSnapshotColumns` 先算出来。）

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ ./internal/usage/ -v
```

- [ ] **Step 5: 量一下**

`BenchmarkLatestUsageSnapshots` 应该从 116.9 ms 掉到 5 ms 上下。

---

### Task 5: 签到的历史有界

**Files:**
- Modify: `internal/store/store.go`（`PruneCheckins`）
- Modify: `internal/scheduler/scheduler.go`（签到那一趟跑完删一次）
- Modify: `internal/config/config.go`（`sqlite.checkin_retention_days`）

签到的历史是唯一会长个没完的东西：一天一个号一行，1000 个号一年 36 万
行、40 MB。而库里的签到积分**七天就过期**（规格 §5.3），网页上每个号
只摆最近 5 条。现在没有一处代码删它。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go`：

```go
// TestTheOldCheckinsAreThrownAway says the check-ins of the days before a
// day are thrown away.
//
// 签到的历史是唯一会长个没完的东西：一天一个号一行，1000 个号一年 36 万
// 行、40 MB。签到的积分七天就过期了（规格 §5.3），网页上每个号只摆最近
// 5 条 —— 再往前的记录除了占地方没有别的用处。
func TestTheOldCheckinsAreThrownAway(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	old := now.AddDate(0, 0, -40)
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: store.DayKey(old), Outcome: "claimed",
		Credit: 10, CreatedAt: old,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_2", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 20, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}

	if err := s.PruneCheckins(context.Background(), now.AddDate(0, 0, -30)); err != nil {
		t.Fatalf("prune checkins: %v", err)
	}

	gone, err := s.ListCheckins(context.Background(), "acc_1", 0)
	if err != nil {
		t.Fatalf("list checkins: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("the old check-in is still there: %+v", gone)
	}
	kept, err := s.ListCheckins(context.Background(), "acc_2", 0)
	if err != nil {
		t.Fatalf("list checkins: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("the new check-in is gone: %+v", kept)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheOldCheckinsAreThrownAway -v
```

失败：编译不过（`PruneCheckins` 还没有）。

- [ ] **Step 3: 实现**

`internal/store/store.go`（放在 `SaveCheckin` 后面）：

```go
// PruneCheckins throws away the check-ins of the days before a day.
//
// 签到的历史是唯一会长个没完的东西：一天一个号一行，1000 个号一年 36 万
// 行、40 MB。签到的积分七天就过期了（规格 §5.3），网页上每个号只摆最近
// 5 条 —— 再往前的记录除了占地方没有别的用处。
//
// 留多少天是外面说了算（sqlite.checkin_retention_days）。
func (s *Store) PruneCheckins(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM checkins WHERE day < ?", DayKey(olderThan))
	if err != nil {
		return fmt.Errorf("prune the check-ins: %w", err)
	}
	return nil
}

```

`internal/config/config.go` —— 新的 `sqlite:` 一节（跟 Task 5 一起做也行，
这一节先给任务三的一半）：

```go
// SQLite is the database's own tuning.
//
// 它是「库怎么干活」的几件事：页缓存多大、写盘要不要每次都 fsync、签到
// 的历史留多少天。
type SQLite struct {
	// CacheMB is how many MB of pages one connection may keep.
	//
	// 页缓存是「读过的页先留在手边」：工作集装得下的时候，同一批查询不
	// 用一遍一遍地去磁盘要页（1000 个号、一年的签到：填池那条查询从
	// 11.7 ms 到 5.9 ms）。0 是 SQLite 自己的默认（2 MB）。
	//
	// 负数是不许的：它会悄悄按 0 办（store 的 dsn 只在 cacheMB > 0 的时
	// 候才加那条 pragma），写的人以为自己说了什么，其实什么也没说。
	CacheMB int `yaml:"cache_mb" json:"cache_mb"`
	// Synchronous is how hard a write hits the disk: "normal" or "full".
	//
	// "normal"：写下去就交给操作系统，断电的时候最近这几笔可能丢（库不
	// 会坏）。"full"：每一笔都 fsync（同一笔写：normal 64µs 对 full
	// 874µs —— full 那一笔要等盘）。空是 SQLite 自己的默认（"full"）。
	Synchronous string `yaml:"synchronous" json:"synchronous"`
	// CheckinRetentionDays is how many days of check-ins are kept.
	//
	// 签到的积分七天就过期了，网页上只摆最近 5 条 —— 再往前的记录除了
	// 占地方没有别的用处（1000 个号一年 36 万行、40 MB）。0 是永远留。
	//
	// 少于 7 天是不许的（负数也不行）：「待补」那一档只看最近七天的签
	// 到，而清理是按站点的那个日子删的，站点时区跟 UTC 差着几个钟头。
	// 留 6 天的话，被删掉的那一段里可能有七天窗口还要用的行 —— 那些号
	// 就永远等不到「签到待补」那一读，卡上摆的数就一直旧着。7 天是刚够
	// 的。
	CheckinRetentionDays int `yaml:"checkin_retention_days" json:"checkin_retention_days"`
}

```

`internal/config/config.go` —— `Config` 里的那一行：

```go
	SQLite SQLite `yaml:"sqlite" json:"sqlite"`
```

`internal/config/config.go` —— 默认值（`Default()` 里）：

```go
		SQLite: SQLite{
			CheckinRetentionDays: 30,
		},
```

`internal/scheduler/scheduler.go` —— 签到那一趟跑完删一次：

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
	s.pruneCheckins(ctx)
}

// pruneCheckins throws away the check-in history that is not needed any
// more.
//
// 签到的历史是唯一会长个没完的东西：一天一个号一行，1000 个号一年 36 万
// 行、40 MB。签到的积分七天就过期了（规格 §5.3）—— 再往前的记录除了占
// 地方没有别的用处。
//
// 签到那一趟跑完删一次：那是每天唯一一趟往签到表里写的事。
func (s *Scheduler) pruneCheckins(ctx context.Context) {
	days := s.Cfg.SQLite.CheckinRetentionDays
	if days <= 0 {
		return
	}
	cut := time.Now().AddDate(0, 0, -days)
	if err := s.Mgr.Store.PruneCheckins(ctx, cut); err != nil {
		fmt.Printf("checkin: the old check-ins could not be thrown away: %v\n", err)
	}
}

```

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ ./internal/scheduler/ ./internal/config/ -v
```

- [ ] **Step 5: 量一下**

一年的签到（36 万行）删到 30 天（3 万行）以后，`BenchmarkDayCheckins` 和
`BenchmarkPoolCandidates` 都该快一截。

---

### Task 6: pragma —— 页缓存、`synchronous`

**Files:**
- Modify: `internal/store/store.go`（`Open` 的选项、DSN）
- Modify: `internal/config/config.go`（`sqlite:` 一节的另外两个键）
- Modify: `cmd/mmxcode/root.go`（把配置交给库）

量出来的（1000 个号、一年的签到 —— 36 万行签到的库）：

| | 读（填池那条查询） | 写（一笔记） |
|---|---|---|
| SQLite 的默认（页缓存 2MB、`synchronous=full`） | 11.7 ms | 874 µs |
| 页缓存 8MB | **5.9 ms** | 874 µs |
| `synchronous=normal` | 5.9 ms | **64 µs** |

写那一栏的数字是这台机器上的磁盘吃下来的那一笔：`full` 要 fsync，快的时候
盘把这一笔收了，慢的时候十几毫秒（跑与跑之间差十几倍）。`normal` 不 fsync，
所以它稳。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go`：

```go
// TestTheStoreIsOpenedWithItsTuning says the store is opened with the
// pragmas it was asked for.
//
// pragma 是每条连接自己的（DSN 里给），所以它们得在打开的时候给。
func TestTheStoreIsOpenedWithItsTuning(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir()+"/t.db", WithCacheMB(8), WithSynchronous("normal"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var cache, sync string
	if err := s.db.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cache); err != nil {
		t.Fatal(err)
	}
	// The size is in KB and SQLite rounds it to a power of two: 8 MB is
	// 8192 KB.
	kb, err := strconv.Atoi(cache)
	if err != nil {
		t.Fatalf("the cache size = %q, want a number of KB", cache)
	}
	if kb > -8000 {
		t.Errorf("the cache size = %d KB, want at least 8000 KB", kb)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != "1" {
		t.Errorf("synchronous = %s, want 1 (normal)", sync)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run TestTheStoreIsOpenedWithItsTuning -v
```

失败：编译不过（`WithCacheMB` 还没有）。

- [ ] **Step 3: 实现**

`internal/store/store.go` —— 选项：

```go
// Option is one thing about how the database is opened.
type Option func(*options)

// options is what the options add up to.
type options struct {
	// cacheMB is the page cache of one connection, in MB. Zero leaves it
	// to SQLite.
	cacheMB int
	// synchronous is how hard a write hits the disk. Empty leaves it to
	// SQLite.
	synchronous string
}

// WithCacheMB says how many MB of pages one connection may keep.
//
// 页缓存是「读过的页先留在手边」：工作集装得下的时候，同一批查询不用一
// 遍一遍地去磁盘要页。1000 个号、一年的签到：填池那条查询从 11.7 ms 到
// 5.9 ms。
//
// 0 是 SQLite 自己的默认（2 MB）。缓存是用多少占多少：库小的时候它占不
// 到那么多。
func WithCacheMB(n int) Option {
	return func(o *options) { o.cacheMB = n }
}

// WithSynchronous says how hard a write hits the disk.
//
// "normal"：写下去就交给操作系统，断电的时候最近这几笔可能丢（库不会
// 坏 —— SQLite 对 WAL 的推荐就是这个）。"full"：每一笔都 fsync，慢一点
// （同一笔写：normal 64µs 对 full 874µs —— full 那一笔要等盘）。
//
// 它管的是**常用的那些写**。丢不起的那两笔（续签后的 token、清掉的「用完
// 了」标记）走另一条连接，那条永远是 full：断了电它们不会丢。配了 "normal"
// 也是那样 —— full 不是全局的，只有那两笔等得起盘。
//
// 空是 SQLite 自己的默认（"full"）。
func WithSynchronous(s string) Option {
	return func(o *options) { o.synchronous = s }
}

```

`internal/store/store.go` —— `Open` 收下这些选项：

```go
// Open opens the database at path, creating it and its directory if
// they are not there.
//
// The directory and the file are made for their owner only: the file
// holds the refresh tokens of every account in clear. Windows has no
// such a mode, and there the file is left as Windows made it.
//
// A directory that is already there is left as it is: the store does
// not tighten the permissions of a directory the user picked. The file
// itself is always tightened, whoever made it.
//
// The file is opened twice when the everyday writes do not fsync: once
// for them, and once with synchronous=full for the writes that must
// not be lost. See the durable field.
func Open(path string, opts ...Option) (*Store, error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path, o))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	// The database is opened lazily, so a path that cannot be used
	// would only show up at the first query. Open it now: the caller
	// wants to know now.
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := protectFile(path); err != nil {
		db.Close()
		return nil, err
	}
	durable, err := openDurable(path, o, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, durable: durable, Clock: time.Now}, nil
}

```

`internal/store/store.go` —— DSN 带上 pragma：

```go
// dsn gives the data source name of a database file.
//
// The path is made a file: URI, so that a path with a question mark
// in it is the path and not the end of the file name. The pragmas
// come after the question mark; a pragma is per connection, so every
// connection of the pool gets them.
//
// The write lock is taken when a transaction starts and not in the
// middle of it. A transaction that finds the lock taken fails at once
// instead of failing at the end, after it has done its work.
func dsn(path string, o options) string {
	p := filepath.ToSlash(path)
	prefix := "file:"
	if strings.HasPrefix(p, "//") {
		// A path that begins with two slashes is a network path, and
		// the two slashes after "file:" are the empty authority that
		// says so.
		prefix = "file://"
	}
	q := prefix + escapePath(p) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
	if o.cacheMB > 0 {
		// The page cache is in KB, and a negative number means "that many
		// KB" (a positive one would mean "that many pages").
		q += fmt.Sprintf("&_pragma=cache_size(%d)", -o.cacheMB*1024)
	}
	if o.synchronous != "" {
		q += "&_pragma=synchronous(" + o.synchronous + ")"
	}
	return q
}

```

`internal/config/config.go` —— `SQLite` 这一节补上两个键：

```go
// SQLite is the database's own tuning.
//
// 它是「库怎么干活」的几件事：页缓存多大、写盘要不要每次都 fsync、签到
// 的历史留多少天。
type SQLite struct {
	// CacheMB is how many MB of pages one connection may keep.
	//
	// 页缓存是「读过的页先留在手边」：工作集装得下的时候，同一批查询不
	// 用一遍一遍地去磁盘要页（1000 个号、一年的签到：填池那条查询从
	// 11.7 ms 到 5.9 ms）。0 是 SQLite 自己的默认（2 MB）。
	//
	// 负数是不许的：它会悄悄按 0 办（store 的 dsn 只在 cacheMB > 0 的时
	// 候才加那条 pragma），写的人以为自己说了什么，其实什么也没说。
	CacheMB int `yaml:"cache_mb" json:"cache_mb"`
	// Synchronous is how hard a write hits the disk: "normal" or "full".
	//
	// "normal"：写下去就交给操作系统，断电的时候最近这几笔可能丢（库不
	// 会坏）。"full"：每一笔都 fsync（同一笔写：normal 64µs 对 full
	// 874µs —— full 那一笔要等盘）。空是 SQLite 自己的默认（"full"）。
	Synchronous string `yaml:"synchronous" json:"synchronous"`
	// CheckinRetentionDays is how many days of check-ins are kept.
	//
	// 签到的积分七天就过期了，网页上只摆最近 5 条 —— 再往前的记录除了
	// 占地方没有别的用处（1000 个号一年 36 万行、40 MB）。0 是永远留。
	//
	// 少于 7 天是不许的（负数也不行）：「待补」那一档只看最近七天的签
	// 到，而清理是按站点的那个日子删的，站点时区跟 UTC 差着几个钟头。
	// 留 6 天的话，被删掉的那一段里可能有七天窗口还要用的行 —— 那些号
	// 就永远等不到「签到待补」那一读，卡上摆的数就一直旧着。7 天是刚够
	// 的。
	CheckinRetentionDays int `yaml:"checkin_retention_days" json:"checkin_retention_days"`
}

```

默认值：

```go
		SQLite: SQLite{
			CacheMB:             8,
			Synchronous:         "normal",
			CheckinRetentionDays: 30,
		},
```

`cmd/mmxcode/root.go` —— 把配置交给库：

```go
		s, err := store.Open(cfg.DB,
			store.WithCacheMB(cfg.SQLite.CacheMB),
			store.WithSynchronous(cfg.SQLite.Synchronous))
```

`internal/config/config.go` —— 写错的 `synchronous` 在启动时就被拒：

```go
	if err := c.checkSQLite(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
```

```go
// checkSQLite refuses the tuning the database does not know, and the
// numbers that would quietly mean something else.
//
// 写盘的狠劲只有两档：normal 和 full。别的词 SQLite 不认，库打开的时候
// 才说，那时已经晚了一步 —— 宁可在这里报错。
//
// 另外两个键写错的时候没有人会报错：它们会悄悄按别的意思来。负的页缓存
// 按 0 办（store 的 dsn 只在 cacheMB > 0 的时候才加那条 pragma），负的
// 保留天数按"永远留"办（pruneCheckins 见到 days <= 0 就不删）。写 -1
// 的人以为自己说了什么，其实什么也没说 —— 宁可在这里报错。
//
// 保留天数还有一条更细的：1 到 6 天不许。签到的积分七天就过期，所以
// 「待补」那一档只看最近七天的签到（usage/round.go 的 checkinWindow =
// 7 * 24 * time.Hour，它问 Store.CheckinTimes(ctx, now.Add(-checkinWindow))）。
// 而清理是按**站点的那个日子**删的（Store.PruneCheckins 删的是
// day < DayKey(cut)，day 是站点时区的那一天），站点时区跟 UTC 差着几个
// 钟头。留 6 天的话，被删掉的那一段里可能有七天窗口还要用的行 —— 那些
// 号就永远等不到「签到待补」那一读，卡上摆的数就一直旧着。留 7 天是刚
// 够的：DayKey 对时刻是单调的，一条 created_at >= now-7天 的行，它的
// 站点那天一定 >= DayKey(now-7天)，所以删 day < DayKey(now-7天) 删不到
// 它。
func (c *Config) checkSQLite() error {
	switch strings.ToLower(c.SQLite.Synchronous) {
	case "", "normal", "full":
	default:
		return fmt.Errorf("sqlite.synchronous is %q: it is \"normal\" or \"full\"",
			c.SQLite.Synchronous)
	}
	if c.SQLite.CacheMB < 0 {
		return fmt.Errorf("sqlite.cache_mb is %d: it is 0 (SQLite's own default, 2 MB) or more",
			c.SQLite.CacheMB)
	}
	if c.SQLite.CheckinRetentionDays < 0 {
		return fmt.Errorf("sqlite.checkin_retention_days is %d: it is 0 (keep every check-in) or 7 or more",
			c.SQLite.CheckinRetentionDays)
	}
	if c.SQLite.CheckinRetentionDays > 0 && c.SQLite.CheckinRetentionDays < 7 {
		return fmt.Errorf("sqlite.checkin_retention_days is %d: it is 0 (keep every check-in) or 7 or more, because the check-in window is seven days",
			c.SQLite.CheckinRetentionDays)
	}
	return nil
}

```

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./... -v
```

- [ ] **Step 5: 配置的两个例子**

`config.example.yaml` 和 `config.docker.yaml` 末尾加上：

```yaml
# 库自己怎么干活。
sqlite:
  # 一条连接留多少 MB 的页在工作集里（0 = SQLite 自己的默认，2 MB）
  cache_mb: 8
  # 写盘：normal = 交给操作系统（断电可能丢最近几笔），full = 每一笔都
  # fsync（慢一点）
  synchronous: normal
  # 签到的历史留多少天（0 = 永远留）
  checkin_retention_days: 30
```

---

### Task 7: 量具 —— 基准与前后两遍的数字

**Files:**
- Create: `internal/store/bench_test.go`
- Modify: `internal/store/bench_test.go`（加 `BenchmarkCheckinTimes`）

基准是这套改动的验收工具：改了索引、改了写法，`go test -bench` 一跑就知道。

- [ ] **Step 1: 写基准**

`internal/store/bench_test.go`（新文件）：夹具是 1000 个号、**30 天**的签到
（一天一个号一行，3 万行）、每个号 15 条读数（1.5 万行），所有基准共用。

夹具是**稳态**的样子：签到的历史留 30 天（`sqlite.checkin_retention_days`
就是这件事的开关），每个号留最新的一条读数，外加最近一刻钟里读的那些
（清理那一趟留的）。一年 36 万行签到的那种库不是稳态 —— 那是没删干净的库，
拿它量出来的数字是「没删干净」的数字，不是这套改动要修的东西。

```go
package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The benchmarks measure the queries that run on their own, over a
// database with the size the gateway is meant to carry: a thousand
// accounts and a month of check-ins.
//
// 测的是**查询本身**，不是整个服务：网关每 30 秒补一次池、调度器每 30 秒
// 跑一趟读额度、每个客户端请求问一次号剩多少 —— 这些 SQL 就是它们的
// 全部开销。改了索引或者改了写法，`go test -bench` 一跑就知道。
//
// 夹具的样子是**稳态**：签到的历史留 30 天（sqlite.checkin_retention_days），
// 每个号留最新的一条读数，外加最近一刻钟里读的那些（清理那一趟留的）。
// 一年 36 万行签到的那种库不是稳态 —— 那是没删干净的库。

// fixture is the database the benchmarks run against.
type fixture struct {
	store *Store
	dir   string
}

var (
	sharedFixture *fixture
	fixtureErr    error
)

// theFixture builds the database the benchmarks share. It is built once.
func theFixture(tb testing.TB) *fixture {
	tb.Helper()
	if sharedFixture == nil {
		sharedFixture, fixtureErr = buildFixture()
	}
	if fixtureErr != nil {
		tb.Fatal(fixtureErr)
	}
	return sharedFixture
}

// The shape of the fixture.
const (
	// fixtureAccounts is how many accounts the gateway is meant to carry.
	fixtureAccounts = 1000
	// fixtureDays is how many days of check-ins are kept.
	fixtureDays = 30
	// fixtureRounds is how many rounds of readings are still in the table:
	// one per account plus the ones of the last quarter of an hour.
	fixtureRounds = 15
)

// buildFixture makes a database with a thousand accounts and a month of
// check-ins.
func buildFixture() (*fixture, error) {
	dir, err := os.MkdirTemp("", "mmxbench")
	if err != nil {
		return nil, err
	}
	s, err := Open(filepath.Join(dir, "bench.db"),
		WithCacheMB(8), WithSynchronous("normal"))
	if err != nil {
		return nil, err
	}
	if err := s.Migrate(context.Background()); err != nil {
		return nil, err
	}
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < fixtureAccounts; i++ {
		id := fmt.Sprintf("acc_%04d", i)
		if err := s.CreateAccount(ctx, Account{
			ID: id, Name: "name-" + id, UserID: fmt.Sprintf("user-%d", i),
			Email: id + "@example.com", Status: StatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return nil, err
		}
		if err := s.SaveToken(ctx, Token{
			AccountID: id, Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		}); err != nil {
			return nil, err
		}
	}
	// 一个月的签到：一天一个号一行。这是库里唯一会长大的东西。
	//
	// 一批一批地插（一条语句插一万行）：夹具是给基准用的，不要把时间
	// 花在造数据上。
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO checkins (account_id,day,outcome,credit,streak,message,created_at)
		 VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	for d := 0; d < fixtureDays; d++ {
		key := day.AddDate(0, 0, d).Format("2006-01-02")
		for i := 0; i < fixtureAccounts; i++ {
			if _, err := stmt.ExecContext(ctx,
				fmt.Sprintf("acc_%04d", i), key, "claimed", 25.0, 7, "", now.UnixMilli()); err != nil {
				return nil, err
			}
		}
	}
	if err := stmt.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// 读数：每个号最新的一条，外加最近一刻钟里那一轮轮的。
	//
	// 清理那一趟（PruneUsageSnapshots）留的就是这个形状：每个号一条最新
	// 的，加上 15 分钟之内的那几条。
	for r := 0; r < fixtureRounds; r++ {
		at := now.Add(-time.Duration(r) * 30 * time.Second)
		for i := 0; i < fixtureAccounts; i++ {
			if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
				AccountID: fmt.Sprintf("acc_%04d", i),
				Granted:   100, Consumed: 1, Remaining: 99,
				CreatedAt: at,
			}); err != nil {
				return nil, err
			}
		}
	}
	return &fixture{store: s, dir: dir}, nil
}

// BenchmarkPoolCandidates measures the query that fills the pool.
func BenchmarkPoolCandidates(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cands, err := f.store.PoolCandidates(ctx, 400, map[string]bool{}, since)
		if err != nil {
			b.Fatal(err)
		}
		if len(cands) == 0 {
			b.Fatal("no candidates")
		}
	}
}

// BenchmarkLatestUsageSnapshots measures the query the card and the
// fallback ask for.
func BenchmarkLatestUsageSnapshots(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snaps, err := f.store.LatestUsageSnapshots(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if len(snaps) != fixtureAccounts {
			b.Fatalf("snaps = %d, want %d", len(snaps), fixtureAccounts)
		}
	}
}

// BenchmarkAccountsWithoutCheckin measures the list the check-in signs.
//
// 用夹具里没有签到记录的那一天：签到那一趟问的就是「今天谁还没签」，
// 答是「一千个」。
func BenchmarkAccountsWithoutCheckin(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	day := "2027-01-01"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		accs, err := f.store.AccountsWithoutCheckin(ctx, day)
		if err != nil {
			b.Fatal(err)
		}
		if len(accs) != fixtureAccounts {
			b.Fatalf("accounts = %d, want %d", len(accs), fixtureAccounts)
		}
	}
}

// BenchmarkCheckinTimes measures the check-in times the credits round
// asks for.
//
// 读额度那一趟每 30 秒问一次「最近签到的号是什么时候」。
func BenchmarkCheckinTimes(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	since := time.Now().AddDate(0, 0, -7)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		times, err := f.store.CheckinTimes(ctx, since)
		if err != nil {
			b.Fatal(err)
		}
		if len(times) == 0 {
			b.Fatal("no check-in times")
		}
	}
}

// BenchmarkDayCheckins measures the credits the check-ins gave.
func BenchmarkDayCheckins(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	day := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		credits, err := f.store.DayCheckins(ctx, day)
		if err != nil {
			b.Fatal(err)
		}
		if len(credits) != fixtureAccounts {
			b.Fatalf("credits = %d, want %d", len(credits), fixtureAccounts)
		}
	}
}

// BenchmarkCountUsable measures the count the concurrency promise is
// computed from.
func BenchmarkCountUsable(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := f.store.CountUsable(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if n != fixtureAccounts {
			b.Fatalf("usable = %d, want %d", n, fixtureAccounts)
		}
	}
}

// BenchmarkPruneUsageSnapshots measures the cleanup that runs every round.
func BenchmarkPruneUsageSnapshots(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := f.store.PruneUsageSnapshots(ctx, time.Now().Add(-15*time.Minute), 1); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSaveUsageSnapshot measures one write: what the credits round
// does, six times every thirty seconds.
func BenchmarkSaveUsageSnapshot(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := f.store.SaveUsageSnapshot(ctx, UsageSnapshot{
			AccountID: "acc_0000", Granted: 100, Consumed: 1, Remaining: 99,
			CreatedAt: now,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// The isolation benchmarks: the old SQL and the new SQL, on the same
// database.
//
// 前面的基准量的是「改之前」和「改之后」两个整体。这几个量的是**改写本身**
// 值多少：两条 SQL 在同一个库上跑，索引是同一套，差的只有写法。
//
// 改写的那两条（`NOT IN` 和从 accounts 出发）在两边都留着：将来谁把写法
// 改回去，基准会立刻说出来。

// isoCount runs a query and counts the rows it gives.
func isoCount(ctx context.Context, s *Store, q string, args ...any) (int, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// isoNoCheckinOldSQL is how the list of the accounts to check in used to
// be asked for.
const isoNoCheckinOldSQL = `
	SELECT a.id FROM accounts a
	WHERE a.status = ?
	  AND NOT EXISTS (SELECT 1 FROM checkins c WHERE c.account_id = a.id AND c.day = ?)
	ORDER BY a.created_at, a.id`

// isoNoCheckinNewSQL is how it is asked for now.
const isoNoCheckinNewSQL = `
	SELECT a.id FROM accounts a
	WHERE a.status = ?
	  AND a.id NOT IN (SELECT c.account_id FROM checkins c WHERE c.day = ?)
	ORDER BY a.created_at, a.id`

// isoUsageOldSQL is how the latest reading of every account used to be
// asked for.
var isoUsageOldSQL = `
	SELECT ` + usageSnapshotColumns("s") + `
	FROM usage_snapshots s
	WHERE s.id = (SELECT s2.id FROM usage_snapshots s2
		WHERE s2.account_id = s.account_id
		ORDER BY s2.created_at DESC, s2.id DESC LIMIT 1)
	ORDER BY s.account_id`

// isoUsageNewSQL is how it is asked for now.
var isoUsageNewSQL = `
	SELECT ` + usageSnapshotColumns("u") + `
	FROM accounts a JOIN usage_snapshots u ON u.id = (
		SELECT s2.id FROM usage_snapshots s2
		WHERE s2.account_id = a.id
		ORDER BY s2.created_at DESC, s2.id DESC LIMIT 1)
	ORDER BY a.id`

// BenchmarkIsoNoCheckinOld measures the old way of asking the list.
func BenchmarkIsoNoCheckinOld(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoNoCheckinOldSQL, StatusActive, "2027-01-01"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIsoNoCheckinNew measures the new way of asking the list.
func BenchmarkIsoNoCheckinNew(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoNoCheckinNewSQL, StatusActive, "2027-01-01"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIsoUsageOld measures the old way of asking the latest reading.
func BenchmarkIsoUsageOld(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoUsageOldSQL); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIsoUsageNew measures the new way of asking the latest reading.
func BenchmarkIsoUsageNew(b *testing.B) {
	f := theFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := isoCount(ctx, f.store, isoUsageNewSQL); err != nil {
			b.Fatal(err)
		}
	}
}

// TestMain cleans up the fixture the benchmarks share.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedFixture != nil {
		os.RemoveAll(sharedFixture.dir)
	}
	os.Exit(code)
}
```

- [ ] **Step 2: 跑基准**

```bash
go test ./internal/store/ -run XXX -bench . -benchtime 30x
```

数字写进这个计划文件的末尾（下面那张表）。

- [ ] **Step 3: 前后两遍**

改之前的那一遍是**把老代码原样拷出来**跑的 —— 不 `git stash`（那份代码里
还没有 `Open` 的那两个选项，基准文件在它上面跑不起来）：

```bash
rm -rf E:/tmp/before-repo && mkdir -p E:/tmp/before-repo
git archive feature/usage-refresh | tar -x -C E:/tmp/before-repo
# 把基准文件拷过去，改两处：Open 不带那两个新选项，
# BenchmarkCheckinTimes 换成老的那条 LatestCheckins。
cd E:/tmp/before-repo && go test ./internal/store/ -run XXX -bench . -benchtime 30x
```

- [ ] **Step 4: 把数字填进这个计划**

见下面「## 前后两遍」。

---

### Task 8: 丢不得的那两笔走一条会 fsync 的连接

**Files:**
- Modify: `internal/store/store.go`（`Store` 多一条 `durable` 连接、`SaveToken`、`ClearExhausted`、拿掉 `Flush`、`AtMillisecond`）
- Modify: `internal/store/store_test.go`
- Modify: `internal/manager/manager.go`（`saveToken` 不再 flush、`tokenNotDurableReason` 拿掉）

`sqlite.synchronous: normal` 换来的那一大截（写一笔 874µs → 64µs；`full` 那一
笔要 fsync，跑与跑之间差十几倍）是**有**代价的：**提交只是把内容写进 WAL 交给
操作系统，断电可能把最后这几笔丢掉**（库不会坏）。

丢得起的写有很多：额度快照旧一点（下一趟补上）、「用完了」的标记没写上
（撞一次空，换号重试，标记就写准了）、会话行没写上（会话重新绑一个号）。

**丢不起的只有两笔**：

1. **续签后的 token**。refresh token 在站点那边是一次性的：库里那笔丢
   了，下一次续签拿的就是一个**已经被站点用过的** refresh token，站点会
   拒，账号就被记成签入失效 —— 要重新签入（密码在库里，能自动做，但要
   多花一次站点调用，而且账号在那之前是死的）。
2. **「额度用完了」被清掉的那一笔**。签到给的积分能救活一个用完的号，
   标记清掉了它才回到轮转里。断了电丢了这一笔，它就还被关在外面 —— 最多
   关到签到的积分过期（七天）。

**这两笔走同一个库的另一条连接**，那条连接用 `synchronous=full` 开着：提
交的时候它把 WAL fsync 掉，写完了就真的在盘上。常用的那条连接还是
`normal`（写一笔 64µs）。

**为什么不是 `wal_checkpoint(FULL)`**（这一版推翻的做法）：checkpoint 要把
WAL 里的页**抄回主库**，抄的时候**拿着 WAL 的写锁**。别的写要排队，排队
排满了（`busy_timeout` 5 秒）就是 `SQLITE_BUSY`。而它本来是被放在**每个
请求**上的（Task 10 那个比较的毛病让每个请求都要写一次 token）—— 线上已
经报出来了：

```
save token: database is locked (5) (SQLITE_BUSY)
```

fsync 只等盘，不抄页，也不挡别人写。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go`（`package store`，看得见未导出的字段）：

```go
// TestTheDurableConnectionSyncsAndTheEverydayOneDoesNot says the writes
// that must not be lost go on a connection that fsyncs, and the everyday
// writes go on one that does not.
//
// 「丢不起」的两笔（续签后的 token、清掉的「用完了」标记）走同一个文件的另
// 一条连接，那条是 full 的：提交的时候它把 WAL fsync 掉，写完了的东西就
// 真的在盘上。常用的那条连接还是 normal —— 别的写丢了都能自己好起来。
//
// 配了 full 的时候不必再开第二条：主连接自己就在 fsync。
func TestTheDurableConnectionSyncsAndTheEverydayOneDoesNot(t *testing.T) {
	ctx := context.Background()

	s := openTestOpts(t, WithSynchronous("normal"))
	var sync int
	if err := s.durable.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 2 {
		t.Errorf("the durable connection's synchronous = %d, want 2 (full)", sync)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 1 {
		t.Errorf("the everyday connection's synchronous = %d, want 1 (normal)", sync)
	}

	// 配了 full 就没有第二条连接了：主连接自己就在 fsync。
	full := openTestOpts(t, WithSynchronous("full"))
	if full.durable != full.db {
		t.Error("there is a second connection although the everyday one fsyncs")
	}
}

// TestTheTokenIsWrittenOnTheDurableConnection says the token is written
// on the connection that fsyncs.
//
// 把那条连接关掉，token 就写不进去了 —— 而别的写照常写得进去。这一条守的
// 是「走哪条连接」：token 走错了连接，断电就能把它带走。
func TestTheTokenIsWrittenOnTheDurableConnection(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")

	if err := s.durable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, Token{
		AccountID: "one", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
	}); err == nil {
		t.Fatal("the token was written without the connection that fsyncs")
	}

	// 别的写还在那条连接上：它们照常写得进去。
	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("the everyday write failed too: %v", err)
	}
}

// TestTheClearedSpentMarkIsWrittenOnTheDurableConnection says the "used
// up" mark is taken off on the connection that fsyncs.
//
// 把那条连接关掉，标记就清不掉 —— 而别的写照常写得进去。
func TestTheClearedSpentMarkIsWrittenOnTheDurableConnection(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")
	if err := s.SetExhausted(ctx, "one", "general", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}

	if err := s.durable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearExhausted(ctx, "one", "general"); err == nil {
		t.Fatal("the spent mark was cleared without the connection that fsyncs")
	}
}

// TestTheClearedSpentMarkIsReallyGone says clearing the "used up" mark
// really clears it, and says nothing is wrong.
//
// 删掉了就是删掉了：不报假错。以前那个 flush 失败是「删掉了但没落盘」这种
// 半个状态 —— 现在写不进去就是没删掉，写进去了就是在盘上的。
func TestTheClearedSpentMarkIsReallyGone(t *testing.T) {
	ctx := context.Background()
	s := openTestOpts(t, WithSynchronous("normal"))
	makeAccount(t, s, "one")
	if err := s.SetExhausted(ctx, "one", "general", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearExhausted(ctx, "one", "general"); err != nil {
		t.Fatalf("clear exhausted: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM exhausted_windows WHERE account_id=?", "one").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d spent marks are left", n)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ -run "Fsynced|Fsyncs" -v
```

失败：`s.durable undefined`（还没有这条连接）。

- [ ] **Step 3: 实现**

`internal/store/store.go` —— `Store` 多一条连接：

```go
// Store is the database.
type Store struct {
	db *sql.DB

	// durable is the connection the writes that must not be lost go on.
	//
	// It is the same file as db, opened with synchronous=full: the
	// commit fsyncs the write-ahead log, so the write is on the disk
	// when the commit is done. It is db itself when db already fsyncs
	// every commit, and then there is no second connection.
	//
	// 丢不得的那两笔（续签后的 token、清掉的「用完了」标记）走它。常用的
	// 那些写还是走 db，按配的速度写（normal）—— 那些写丢了都能自己好起来
	// （额度快照旧一点、标记没写上撞一次空换号重试）。
	durable *sql.DB

	// Clock is the store's clock.
	//
	// It is there so that a test can say what time it is. A store
	// with no clock uses the wall clock.
	Clock func() time.Time
}

```

`internal/store/store.go` —— 开那条连接：

```go
// Open opens the database at path, creating it and its directory if
// they are not there.
//
// The directory and the file are made for their owner only: the file
// holds the refresh tokens of every account in clear. Windows has no
// such a mode, and there the file is left as Windows made it.
//
// A directory that is already there is left as it is: the store does
// not tighten the permissions of a directory the user picked. The file
// itself is always tightened, whoever made it.
//
// The file is opened twice when the everyday writes do not fsync: once
// for them, and once with synchronous=full for the writes that must
// not be lost. See the durable field.
func Open(path string, opts ...Option) (*Store, error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path, o))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	// The database is opened lazily, so a path that cannot be used
	// would only show up at the first query. Open it now: the caller
	// wants to know now.
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := protectFile(path); err != nil {
		db.Close()
		return nil, err
	}
	durable, err := openDurable(path, o, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, durable: durable, Clock: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.durable != nil && s.durable != s.db {
		if e := s.durable.Close(); err == nil {
			err = e
		}
	}
	return err
}

```

`internal/store/store.go` —— 那条连接的选项：

```go
// openDurable opens the connection the writes that must not be lost go
// on.
//
// It is the same file, opened with synchronous=full: the commit fsyncs
// the write-ahead log, so the write is on the disk when the commit is
// done. The everyday connection keeps the speed it was given (normal):
// most writes can be lost and recover by themselves.
//
// If the everyday connection fsyncs every write already, it is the
// durable one and there is no second connection.
//
// 丢不得的那两笔（续签后的 token、清掉的「用完了」标记）走这条连接。它们的
// 提交是一次 fsync：几毫秒。它们很少发生 —— 一次续签，和一次签到救活了
// 一个用完的号 —— 所以等得起。
func openDurable(path string, o options, db *sql.DB) (*sql.DB, error) {
	fsyncs, err := fsyncsEveryWrite(db)
	if err != nil {
		return nil, err
	}
	if fsyncs {
		return db, nil
	}
	d := o
	d.synchronous = "full"
	durable, err := sql.Open("sqlite", dsn(path, d))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	durable.SetMaxOpenConns(durableConns)
	durable.SetMaxIdleConns(durableConns)
	if err := durable.PingContext(context.Background()); err != nil {
		durable.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return durable, nil
}

// fsyncsEveryWrite says whether the connection fsyncs every commit.
//
// The pragma is asked of the database, not of the options: SQLite's own
// default (no synchronous in the data source name) is full, and a build
// that says otherwise is the build's business, not ours.
func fsyncsEveryWrite(db *sql.DB) (bool, error) {
	var sync int
	if err := db.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&sync); err != nil {
		return false, fmt.Errorf("read the synchronous setting: %w", err)
	}
	// 2 is full, 3 is extra: both of them fsync every commit.
	return sync >= 2, nil
}

```

`internal/store/store.go` —— 那两笔走那条连接：

```go
// SaveToken writes an account's token.
//
// 这笔丢不得：库里那笔 token 丢了，下一次续签拿的就是一个已经被站点用过
// 一次的 refresh token，站点会拒，账号就被记成签入失效。所以它走那条
// fsync 的连接：写完了就在盘上。
func (s *Store) SaveToken(ctx context.Context, t Token) error {
	return s.saveToken(ctx, s.durable, t)
}

// ClearExhausted clears an account's window.
//
// 清除的那笔要落盘：这个标记是「这个号的额度用完了」，而签到给的积分能
// 救活它 —— 标记没了它才回到轮转里。断了电把这一笔丢了，它就还被关在
// 外面，最多关到签到的积分过期（七天）。
//
// 它走那条 fsync 的连接：写不进去就是没删掉，写进去了就是在盘上的 ——
// 没有「删掉了但没落盘」这种半个状态，所以只有 DELETE 本身失败才报错。
func (s *Store) ClearExhausted(ctx context.Context, accountID, windowKey string) error {
	_, err := s.durable.ExecContext(ctx,
		"DELETE FROM exhausted_windows WHERE account_id=? AND window_key=?", accountID, windowKey)
	if err != nil {
		return fmt.Errorf("clear exhausted: %w", err)
	}
	return nil
}

```

`internal/store/store.go` —— 拿掉 `Flush`（那个方法整个删掉）。

`internal/manager/manager.go` —— 写完就在盘上了，没有第二步：

```go
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
//
// 库里存的是毫秒，而续签那一刻拿到的是毫秒以下的精度。所以比的时候**两边
// 都要砍到毫秒**：不砍的话，库里那个 = fromMS(ms(手里那个))，永远比手里
// 那个「旧」那么一点，于是这个守卫永远是「库里那个旧，写吧」—— 每个请求
// 都重写一遍 token 行，每一遍都是一个写事务加一次 fsync，写锁排长队，排满
// busy_timeout 就是 SQLITE_BUSY。
func (m *Manager) saveToken(ctx context.Context, accountID string, tok *oauth.Token) error {
	stored, err := m.Store.GetToken(ctx, accountID)
	if err == nil && !stored.ExpiresAt.Before(store.AtMillisecond(tok.Expires)) {
		// The store already holds a token that is at least as new.
		return nil
	}
	// A token that could not be read is written over: the store is
	// the only place the token lives, and a renewal that is not
	// written down is a renewal that is lost.
	if err := m.Store.SaveToken(ctx, store.Token{
		AccountID: accountID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: time.Now(),
	}); err != nil {
		return err
	}
	// 写下去了，库里那份就追上了手里这份 —— 那条「写不下去」的记号不再
	// 成立，把它擦掉。
	m.clearTokenNote(ctx, accountID)
	return nil
}

```

`internal/manager/manager.go` —— `tokenNotDurableReason` 那个常量整个删掉。

`internal/manager/manager_test.go` —— 那两笔在盘上，写的时候不挡着别人：

```go
// TestTheRenewedTokenIsOnTheDisk says the token of a renewal is on the
// disk when the renewal is over.
//
// 库里写下的东西在 WAL 模式下是「写进 WAL 交给操作系统」：断电可能把最
// 后这几笔丢掉。而 token 丢了就是下一次续签拿一个已经被站点用过的 refresh
// token（站点会拒，账号就被记成签入失效）。所以 token 走的那条连接每次
// 提交都 fsync。
//
// 「真的在盘上」没法在测试里造一次断电来验，但**盘上有什么**是能验的：把
// 库文件和它旁边的 WAL 一起拷一份（那两个文件加起来就是盘上的东西），那个
// token 得在里面。
//
// WAL 也要跟着走 —— 那一笔是写进 WAL 的，页还没有抄回主库，所以主库里还是
// 续签之前的样子。两个文件一起拷才是盘上的全部。
func TestTheRenewedTokenIsOnTheDisk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"new","refresh_token":"newref","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m, path := newTestManagerWithPath(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "old", Refresh: "ref",
		// 两分钟后过期，落在续期窗口内
		ExpiresAt: now.Add(2 * time.Minute), UpdatedAt: now,
	})
	if _, err := m.Token(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}

	db := rawDB(t, copyOfTheDatabase(t, path))
	var access, refresh string
	if err := db.QueryRowContext(ctx,
		"SELECT access, refresh FROM tokens WHERE account_id=?", "acc_01").
		Scan(&access, &refresh); err != nil {
		t.Fatal(err)
	}
	if access != "new" || refresh != "newref" {
		t.Fatalf("the token on the disk = %q / %q", access, refresh)
	}
}

// TestTheTokenIsWrittenWhileSomethingElseIsReading says writing the token
// does not wait for a reader.
//
// WAL 的 checkpoint（把 WAL 里的页抄回主库）要拿着写锁，抄不完就一直等
// —— 等到 busy_timeout 就是 SQLITE_BUSY。而从前那个 checkpoint 就放在写
// token 的路上。fsync 只等盘，不抄页，也不挡别人写。
func TestTheTokenIsWrittenWhileSomethingElseIsReading(t *testing.T) {
	ctx := context.Background()
	m, path := newTestManagerWithPath(t, nil)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})

	// 一个读者开着读事务不放。
	reader := rawDB(t, path)
	tx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// 读一次，读者才拿住快照。
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts").Scan(&n); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := m.saveToken(ctx, "acc_01", &oauth.Token{
		Access: "a", Refresh: "r", Expires: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("writing the token took %v: it waited for the reader", d)
	}
}

// copyOfTheDatabase copies the database file and the log beside it.
//
// WAL 也要跟着走：那一笔是写进 WAL 的，页还没有抄回主库 —— 主库里是写它
// 之前的样子。两个文件加起来才是盘上的全部。
func copyOfTheDatabase(t *testing.T, path string) string {
	t.Helper()
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return copyPath
}

```


- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/store/ ./internal/manager/ -count=1
```

- [ ] **Step 5: 量一下**

那一笔 fsync 只在续签（一个号一个钟头一次）和签到救活了一个用完的号的时候
发生 —— 一天几十次，一次不到一毫秒。而它换来的是「断电丢 token」这件事不会
发生。

---

### Task 9: 签到的记录一批一批写

**Files:**
- Modify: `internal/store/store.go`（`SaveCheckins`）
- Modify: `internal/manager/manager.go`（`CheckinRecord`、`RecordCheckins`）
- Modify: `internal/scheduler/scheduler.go`（`checkinAll` 那一趟攒着写）
- Modify: `internal/store/store_test.go`（日志涨了多少）
- Modify: `internal/scheduler/scheduler_test.go`（记录都写下去了）

签到那一趟是唯一一趟往签到表里写的事，而它是**一个号一个事务**：每个号都
要把签到表的那一页和它的几个索引页重写一遍。量出来的（1000 个号）：

| | 每个号 | 一趟 |
|---|---|---|
| 一个号一个事务 | 18004 B | **17.2 MB** |
| 五十个一事务 | 1133 B | **1.1 MB** |

一天一趟的事，17 MB 的日志换 1 MB 是值得的。而且新加的两个索引让每一行
要写的页多了一个（表页 + 三个索引页），不合成一批写，这一条就把新索引
省下来的还回去了。

**丢得起**：一批丢了的记录是「签到成了、可是记录没了」，最多五十个号今天
的积分进不了池（规格 §5.3）。签到本身在站点那边已经成了。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go`：「一个事务」是看得见的 —— 日志涨了多少。

```go
// TestTheCheckinsInABatchAreWrittenInOneGo says a batch of check-ins is
// written in one transaction.
//
// 一趟签到有几百个号：一个号一个事务的话，每个号都要把签到表的那一页和
// 它的几个索引页重写一遍 —— 一千个号是 17 MB 的日志。合成一个事务写是
// 1 MB（量出来的：每个号 1133 B，而不是 18004 B）。
//
// 「一个事务」是看得见的：日志涨了多少。
func TestTheCheckinsInABatchAreWrittenInOneGo(t *testing.T) {
	const rows = 100
	oneByOne := logGrowth(t, rows, false)
	inABatch := logGrowth(t, rows, true)
	if inABatch >= oneByOne/2 {
		t.Fatalf("the batch wrote %d bytes, the single writes %d", inABatch, oneByOne)
	}
}

```

（`logGrowth` 和 `logSize` 两个帮手也在那个文件里：一个开一个库，把一百个
签到写下去，报日志长了多少。）

`internal/scheduler/scheduler_test.go`：记录都写下去了（攒起来写的那一趟，
最后不满一批的也要写）。

```go
// TestTheCheckinRoundRecordsEveryCheckin says every check-in of a round
// is written down.
//
// 记录是签到给的积分进池的那条路（规格 §5.3）：签到了可是记录没了，那个
// 号今天的积分就进不了池。
//
// 一趟签到把记录攒起来一批一批写下去（一个号一个事务要把签到的那几页重
// 写一遍），所以这一条守的是「攒起来的那些也都被写下去了」。
func TestTheCheckinRoundRecordsEveryCheckin(t *testing.T) {
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
	// 三个号：签到那一趟的记录是攒起来写的，攒不满一批也要写下去。
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("acc_%02d", i)
		s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, UserID: "1",
			CreatedAt: now, UpdatedAt: now,
		})
		s.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok", Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())

	sc := New(mgr, config.Default())
	sc.CheckinAll(ctx)

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("acc_%02d", i)
		rows, err := s.ListCheckins(ctx, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("the check-ins of %s = %+v, want one", id, rows)
		}
		if rows[0].Credit != 25 {
			t.Fatalf("the credit of %s = %v, want 25", id, rows[0].Credit)
		}
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ ./internal/scheduler/ -run "InABatch|RecordsEveryCheckin" -v
```

失败：编译不过（`SaveCheckins` 还没有）。

- [ ] **Step 3: 实现**

`internal/store/store.go` —— 一批一个事务：

```go
// SaveCheckins writes several check-ins down in one transaction.
//
// 一趟签到有几百个号：一个号一个事务的话，每个号都要把签到表的那一页和
// 它的几个索引页重写一遍 —— 一千个号是 17 MB 的日志，而合成一个事务写
// 是 1 MB（量出来的：每个号 1133 B，而不是 18004 B）。
//
// 一整批是一个事务：一个写不下去，一整批都回去。
//
// 全批一起回是划得来的：签到的记录是彼此独立的，可是它们写的是同一张表的
// 同一批页 —— 半批写下去比全批写下去更贵，而丢掉的最多是这一批里那些号
// 今天的积分进不了池（规格 §5.3）。丢得起。
func (s *Store) SaveCheckins(ctx context.Context, cs []Checkin) error {
	if len(cs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save checkins: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO checkins (account_id, day, outcome, credit, streak, message, created_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(account_id, day) DO UPDATE SET
			outcome=excluded.outcome, credit=excluded.credit, streak=excluded.streak,
			message=excluded.message, created_at=excluded.created_at`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save checkins: %w", err)
	}
	for _, c := range cs {
		if _, err := stmt.ExecContext(ctx,
			c.AccountID, c.Day, c.Outcome, c.Credit, c.Streak, c.Message, ms(s.stamp(c.CreatedAt))); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("save checkin: %w", err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save checkins: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save checkins: %w", err)
	}
	return nil
}

```

`internal/manager/manager.go` —— 签到（网络）和写记录（库）分成两件事：

```go
// CheckinRecord checks an account in and gives the record of it.
//
// 记录不在这里写：写是另一件事。一趟签到有几百个号，一个号一个事务的话，
// 每个号都要把签到表的那一页和它的几个索引页重写一遍 —— 一千个号是 17 MB
// 的日志，而合成一批写是 1 MB（量出来的：每个号 1133 B，而不是 18004 B）。
//
// 那一趟用 `RecordCheckins` 把记录一批一批写下去。
func (m *Manager) CheckinRecord(ctx context.Context, accountID string) (checkin.Outcome, store.Checkin, error) {
	a, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return checkin.Outcome{}, store.Checkin{}, err
	}
	tok, err := m.Token(ctx, accountID)
	if err != nil {
		return checkin.Outcome{}, store.Checkin{}, err
	}
	acc, err := m.accFor(a)
	if err != nil {
		return checkin.Outcome{}, store.Checkin{}, err
	}
	out, err := checkin.New(acc).Run(ctx, tok.Access, a.UserID)
	if err != nil {
		return checkin.Outcome{}, store.Checkin{}, err
	}
	return out, store.Checkin{
		AccountID: accountID,
		Day:       m.Store.Today(),
		Outcome:   out.Outcome,
		Credit:    out.Credit,
		Streak:    out.Streak,
		Message:   out.Message,
		CreatedAt: time.Now(),
	}, nil
}

// RecordCheckins writes the records of a batch of check-ins down.
//
// 记不住的那些号要记在账号上：签到成了，记录没了。而记在库里的那笔正是
// 签到给的积分进池的那条路（规格 §5.3）。
func (m *Manager) RecordCheckins(ctx context.Context, records []store.Checkin) {
	if len(records) == 0 {
		return
	}
	if err := m.Store.SaveCheckins(ctx, records); err != nil {
		for _, r := range records {
			m.note(ctx, r.AccountID, checkinNotWrittenReason+": "+err.Error())
		}
	}
}

```

`Manager.Checkin`（后台页面点的那一次签到）还是签完就写 —— 一次一个号，
写下去就完了。

`internal/scheduler/scheduler.go` —— 那一趟攒着写：

```go
// checkinBatch is how many check-in records are written at once.
//
// 一趟签到有几百个号。一个号一个事务的话，每个号都要把签到表的那一页和
// 它的几个索引页重写一遍 —— 一千个号是 17 MB 的日志，而五十个一批是
// 1 MB（量出来的：每个号 1133 B，而不是 18004 B）。
//
// 五十个一批也是丢得起的：一批丢了的记录是「签到成了、可是记录没了」，
// 最多五十个号今天的积分进不了池（规格 §5.3）。
const checkinBatch = 50

```

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./... -v
```

- [ ] **Step 5: 量一下**

签到那一趟的日志：一千个号从 17.2 MB 到 1.1 MB（每 50 个一批）。

---

### Task 10: token 只写一次 —— 比的时候两边都砍到毫秒

**Files:**
- Modify: `internal/store/store.go`（`AtMillisecond`）
- Modify: `internal/manager/manager.go`（`newerThanStored`、`saveToken`）
- Test: `internal/manager/manager_test.go`

**这一件是线上那条错的根因。**

库里 `tokens.expires_at` 存的是毫秒（`ms(t)` = `t.UnixMilli()`），而续签那一
刻拿到的 `oauth.Token.Expires` 是毫秒以下的精度。于是：

- 库里那个 = `fromMS(ms(手里那个))` = 手里那个砍到毫秒；
- `stored.ExpiresAt.Before(tok.Expires)` → 砍过的那一个**永远**比手里那个
  「旧」那么一点（除非它正好落在毫秒的边界上）；
- 于是 `newerThanStored` **永远为真** → `Manager.Token()`（每个被代理的请求
  都调它，`internal/gateway/gateway.go:644`）**每个请求都重写一遍 token 行**；
- 每个请求一次写事务（老代码 `synchronous` 没配 = SQLite 的默认 `full`，一
  笔要 fsync）→ 写锁排长队 → 排满 `busy_timeout(5000)` → `SQLITE_BUSY`：

```
save token: database is locked (5) (SQLITE_BUSY)
```

线上看到的正是这个（Task 8 那个 `Flush` 又在它上面加了一脚：每个请求还要
做一次 checkpoint）。

**改法**：比的时候两边都砍到毫秒。`internal/store` 加一个导出函数 ——
「库里存的是毫秒」这件事只有那个包里知道：

```go
// AtMillisecond gives the time as the store writes it down.
//
// 库里存的时间是毫秒 —— 比毫秒细的那部分存下去就没有了。所以拿一个时刻跟
// 库里的比，要先把它也砍到毫秒：不砍的话，库里那个（毫秒）永远比手里那个
// （毫秒以下）旧那么一点，于是每一次比较都说「要重写」。
func AtMillisecond(t time.Time) time.Time {
	return fromMS(ms(t))
}

```

`internal/manager/manager.go` 两处都用它：

```go
// newerThanStored says the token is newer than the one the store has.
//
// The expiry says which one is newer, the same way saveToken says it:
// the site hands out access tokens that live an hour, so the token of
// the later renewal ends later. Two tokens that end at the same time
// are the same step of the chain, and the one in the store is kept.
//
// 库里存的是毫秒（tokens.expires_at 是 UnixMilli），而续签那一刻拿到的是
// 毫秒以下的精度。所以比的时候**两边都要砍到毫秒**：不砍的话，库里那个 =
// fromMS(ms(手里那个))，永远比手里那个「旧」那么一点（除非它正好落在毫秒
// 的边界上），于是 newerThanStored 永远为真 —— 每个被代理的请求都以为要
// 重写一遍 token 行，每一遍都是一个写事务加一次 fsync，写锁排长队，排满
// busy_timeout 就是 SQLITE_BUSY。
func newerThanStored(tok *oauth.Token, stored store.Token) bool {
	if tok == nil || tok.Expires.IsZero() {
		return false
	}
	return stored.ExpiresAt.Before(store.AtMillisecond(tok.Expires))
}

```

`saveToken` 里的那个守卫（库里那笔至少跟手里这个一样新，就不写）也一样：

```go
	if err == nil && !stored.ExpiresAt.Before(store.AtMillisecond(tok.Expires)) {
		// The store already holds a token that is at least as new.
		return nil
	}
```

两处的注释都要补上「库里存的是毫秒，不砍的话每一次都比成要重写」这句话。

- [ ] **Step 1: 写失败的测试**

`internal/manager/manager_test.go`：

```go
// TestARenewedAccountIsNotWrittenOnEveryRequest says the token of a
// renewed account is written once.
//
// 库里存的时间是毫秒，续签那一刻拿到的是毫秒以下的精度。不把两边砍到同
// 一个精度，库里那个就永远比手里那个「旧」那么一点 —— 于是每个被代理的
// 请求都重写一遍 token 行：写锁排长队，排满了（busy_timeout 5 秒）就是
// SQLITE_BUSY。
//
// 怎么知道写没写：另一个连接看着 `PRAGMA data_version` —— 别的连接提交了
// 它就变。
func TestARenewedAccountIsNotWrittenOnEveryRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"new","refresh_token":"newref","expires_in":3600}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	m, path := newTestManagerWithPath(t, srv)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.Store.SaveToken(ctx, store.Token{
		AccountID: "acc_01", Access: "old", Refresh: "ref",
		ExpiresAt: now.Add(2 * time.Minute), UpdatedAt: now,
	})
	// 续签一次，写下去一次。
	if _, err := m.Token(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}

	watch := rawDB(t, path)
	before := dataVersion(t, watch)
	// 再来一个请求：不该再写了。
	if _, err := m.Token(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	if after := dataVersion(t, watch); after != before {
		t.Error("the token was written again")
	}
}

// dataVersion says what the database file is at, seen from another
// connection.
//
// 它变的是「别的连接提交了没有」：一个连接自己提交不改自己的数。
func dataVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

```

`internal/manager/manager_test.go` —— 库追上了，那条记号就不成立了：

```go
// TestTheTokenNoteGoesAwayWhenTheStoreCatchesUp says the note about a
// token that could not be written down goes away when the store catches
// up.
//
// 那条记号说的是「库里那份比手里那份旧」。token 写成功了，库里那份就追上
// 了 —— 记号不成立了，可它还挂着：last_error 只有在改状态的时候才会被
// 清掉，所以一次暂时的忙会在一个好好的账号上留一条「这个号有问题」。
func TestTheTokenNoteGoesAwayWhenTheStoreCatchesUp(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManagerWithPath(t, nil)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	// 写不下去的那一次留下的记号。
	m.note(ctx, "acc_01", tokenNotWrittenReason+": the disk is full")
	// 写进去了，库里那份追上了手里那份。
	if err := m.saveToken(ctx, "acc_01", &oauth.Token{
		Access: "a", Refresh: "r", Expires: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.LastError != "" {
		t.Errorf("the note is still on the account: %q", a.LastError)
	}
}

// TestTheNoteOfAnotherReasonStays says a note that is not about the token
// is left alone.
//
// 记号上写的是别的理由（续签失败但手里那份还好），那件事还是真的 —— 擦掉
// 它就是把真话也藏起来。
func TestTheNoteOfAnotherReasonStays(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManagerWithPath(t, nil)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.note(ctx, "acc_01", renewalFailedReason+": the site is having a bad moment")
	if err := m.saveToken(ctx, "acc_01", &oauth.Token{
		Access: "a", Refresh: "r", Expires: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	a, err := m.Store.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.LastError, renewalFailedReason) {
		t.Errorf("the note on the account = %q, want the one that was there", a.LastError)
	}
}

```


- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/manager/ -run TestARenewedAccountIsNotWrittenOnEveryRequest -v
```

失败：`the token was written again`（每一个请求都写）。

- [ ] **Step 3: 实现**

（上面那两处比较。）

写成功了就把那条记号擦掉 —— 只擦这一种理由的：

```go
// clearTokenNote takes away the note about a token that could not be
// written down.
//
// 那条记号说的是「库里那份比手里那份旧」。token 写成功了，库里那份就追上
// 了手里那份 —— 记号不成立了，可它还挂着：last_error 只有在改状态的时候
// 才会被清掉，所以一次暂时的忙会在一个好好的账号上留一条「这个号有问题」，
// 一直留到下一次改状态为止。
//
// 所以 token 写成功之后要把它擦掉。只擦这一条：别的理由（续签失败但手里
// 那份还好之类）说的是别的事，擦了就是把真话也藏起来。
//
// 它是尽力而为的：擦不掉就算了，下一次写成功还会再擦一次。
func (m *Manager) clearTokenNote(ctx context.Context, accountID string) {
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
	if !strings.HasPrefix(a.LastError, tokenNotWrittenReason) {
		// Some other reason is on the account, and it is still true.
		return
	}
	_ = m.Store.SetAccountLastError(ctx, accountID, "", time.Now())
}

```


- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/manager/ -count=1
```


---

### Task 11: `sqlite:` 一节的三个键都在启动时查

**Files:**
- Modify: `internal/config/config.go`（`checkSQLite`）
- Test: `internal/config/config_test.go`

`sqlite:` 一节有三个键。`synchronous` 已经在查了（写下的值 SQLite 不认，
库打开的时候才说 —— 那已经晚了一步）。另外两个键写错了**没有人会报错**，
它们会悄悄按别的意思来：

- `cache_mb` 是负数：store 的 `dsn` 只在 `cacheMB > 0` 的时候才加那条
  pragma，所以负数是**按 0 办**的（SQLite 自己的默认，2 MB）。写 -1 的人以
  为自己说了什么，其实什么也没说。
- `checkin_retention_days` 是负数：调度器的 `pruneCheckins` 见到
  `days <= 0` 就不删，所以负数是**按「永远留」办**的。

还有一条更细的：`checkin_retention_days` 是 1 到 6 天的时候**不许**。签到
的积分七天就过期，所以「待补」那一档只看最近七天的签到（`usage/round.go`
的 `checkinWindow = 7 * 24 * time.Hour`，它问
`Store.CheckinTimes(ctx, now.Add(-checkinWindow))`）。而清理是按**站点的
那个日子**删的（`Store.PruneCheckins` 删的是 `day < DayKey(cut)`，`day` 是
站点时区的那一天），站点时区跟 UTC 差着几个钟头。留 6 天的话，被删掉的那
一段里可能有七天窗口还要用的行 —— 那些号就永远等不到「签到待补」那一读，
卡上摆的数就一直旧着。

留 7 天是刚够的：`DayKey` 对时刻是单调的，一条 `created_at >= now-7天` 的
行，它的站点那天一定 `>= DayKey(now-7天)`，所以删 `day < DayKey(now-7天)`
删不到它。

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go` —— 这个包的测试是写一个临时的 yaml 文件走
`Load`（照着现有的测试走）：

```go
// TestTheCacheMBRefusesANegative says a negative page cache is an error.
//
// 负的页缓存是"悄悄按 0 办"：store 的 dsn 只在 cacheMB > 0 的时候才加那
// 条 pragma，所以 -1 跟 0 是一回事。写 -1 的人以为自己说了什么，其实
// 什么也没说 —— 宁可在这里报错。
func TestTheCacheMBRefusesANegative(t *testing.T) {
	_, err := loadSQLite(t, "sqlite:\n  cache_mb: -1\n")
	if err == nil {
		t.Fatal("the negative page cache was taken")
	}
	if !strings.Contains(err.Error(), "cache_mb") {
		t.Fatalf("error %q does not name the key", err)
	}
}

```

（其余：`cache_mb` 的 0 和正数放过；`checkin_retention_days` 的负数、1 到 6
天拒掉，0 和 7 天以上放过；`synchronous` 的老规矩别改坏。）

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/config/ -count=1
```

- [ ] **Step 3: 实现**

`internal/config/config.go`：

```go
// checkSQLite refuses the tuning the database does not know, and the
// numbers that would quietly mean something else.
//
// 写盘的狠劲只有两档：normal 和 full。别的词 SQLite 不认，库打开的时候
// 才说，那时已经晚了一步 —— 宁可在这里报错。
//
// 另外两个键写错的时候没有人会报错：它们会悄悄按别的意思来。负的页缓存
// 按 0 办（store 的 dsn 只在 cacheMB > 0 的时候才加那条 pragma），负的
// 保留天数按"永远留"办（pruneCheckins 见到 days <= 0 就不删）。写 -1
// 的人以为自己说了什么，其实什么也没说 —— 宁可在这里报错。
//
// 保留天数还有一条更细的：1 到 6 天不许。签到的积分七天就过期，所以
// 「待补」那一档只看最近七天的签到（usage/round.go 的 checkinWindow =
// 7 * 24 * time.Hour，它问 Store.CheckinTimes(ctx, now.Add(-checkinWindow))）。
// 而清理是按**站点的那个日子**删的（Store.PruneCheckins 删的是
// day < DayKey(cut)，day 是站点时区的那一天），站点时区跟 UTC 差着几个
// 钟头。留 6 天的话，被删掉的那一段里可能有七天窗口还要用的行 —— 那些
// 号就永远等不到「签到待补」那一读，卡上摆的数就一直旧着。留 7 天是刚
// 够的：DayKey 对时刻是单调的，一条 created_at >= now-7天 的行，它的
// 站点那天一定 >= DayKey(now-7天)，所以删 day < DayKey(now-7天) 删不到
// 它。
func (c *Config) checkSQLite() error {
	switch strings.ToLower(c.SQLite.Synchronous) {
	case "", "normal", "full":
	default:
		return fmt.Errorf("sqlite.synchronous is %q: it is \"normal\" or \"full\"",
			c.SQLite.Synchronous)
	}
	if c.SQLite.CacheMB < 0 {
		return fmt.Errorf("sqlite.cache_mb is %d: it is 0 (SQLite's own default, 2 MB) or more",
			c.SQLite.CacheMB)
	}
	if c.SQLite.CheckinRetentionDays < 0 {
		return fmt.Errorf("sqlite.checkin_retention_days is %d: it is 0 (keep every check-in) or 7 or more",
			c.SQLite.CheckinRetentionDays)
	}
	if c.SQLite.CheckinRetentionDays > 0 && c.SQLite.CheckinRetentionDays < 7 {
		return fmt.Errorf("sqlite.checkin_retention_days is %d: it is 0 (keep every check-in) or 7 or more, because the check-in window is seven days",
			c.SQLite.CheckinRetentionDays)
	}
	return nil
}

```

函数的注释和 `SQLite` 那两个字段的注释都要跟上（负数为什么不许、少于七天为
什么不许）。

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./internal/config/ -count=1
```

---

### Task 12: 评审点出来的测试缺口

**Files:**
- Test: `internal/store/store_test.go`（`SaveCheckins`）
- Test: `internal/manager/manager_test.go`（`RecordCheckins`）
- Test: `internal/scheduler/scheduler_test.go`（五十个一批）
- Test: `internal/store/checkins_query_test.go`（查询计划别钉住 SQLite 的措辞）

这一版的代码评审点了三处没有测试的地方：

1. **`Store.SaveCheckins` 一个测试都没有**。它是「签到的记录一批一批写」那
   一件的核心：一个事务，一批全部落或者全部不落。
2. **`Manager.RecordCheckins` 和五十个一批也没有测试**。签到那一趟攒够五十
   个写一次 —— 攒够五十、写出去、最后不满一批的也写出去，这三件事都要在。
3. **查询计划那两条断言钉住了 SQLite 的措辞**：`LIST SUBQUERY` 和
   `SCAN a ` 都是 SQLite 自己的话，升级一下 SQLite 就可能换一句，测试就
   红了 —— 而它要防的那件事（读数表不被扫）没变。改抓**我们自己的**名字：
   索引的名字（`idx_checkins_day_account`）和表的名字。

- [ ] **Step 1: 写失败的测试**

`internal/store/store_test.go`：

```go
// TestTheCheckinsInABatchAreWrittenDown says a batch of check-ins is
// written down and can be read back.
//
// 一批签到写进去能读出来 —— 这一条是「写进去了」的意思。
func TestTheCheckinsInABatchAreWrittenDown(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "one")
	makeAccount(t, s, "two")

	if err := s.SaveCheckins(ctx, []Checkin{
		{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25, Streak: 3},
		{AccountID: "two", Day: "2026-11-19", Outcome: "claimed", Credit: 10, Streak: 1},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListCheckins(ctx, "one", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d check-ins for the account, want one", len(got))
	}
	if got[0].Day != "2026-11-19" || got[0].Credit != 25 {
		t.Fatalf("got %+v, want the check-in that was written", got[0])
	}
}

// TestTheSameBatchOfCheckinsWrittenTwiceIsAnOverwrite says writing the
// same batch again replaces the rows instead of making more of them.
//
// 签到那一趟会重跑：一个号一天只有一行（account_id + day 上有唯一约束），
// 写第二次是覆盖，不是多出一行来。
func TestTheSameBatchOfCheckinsWrittenTwiceIsAnOverwrite(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "one")

	first := []Checkin{{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25}}
	if err := s.SaveCheckins(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := []Checkin{{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 40}}
	if err := s.SaveCheckins(ctx, second); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM checkins WHERE account_id=?", "one").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d rows for the account and the day, want one", n)
	}
	var credit float64
	if err := s.db.QueryRowContext(ctx,
		"SELECT credit FROM checkins WHERE account_id=?", "one").Scan(&credit); err != nil {
		t.Fatal(err)
	}
	if credit != 40 {
		t.Fatalf("the credit = %v, want the one that was written last", credit)
	}
}

// TestAnEmptyBatchOfCheckinsWritesNothing says an empty batch is not an
// error, and writes nothing.
//
// 没有要签到的号：那是「今天不用签」，不是「签到坏了」。
func TestAnEmptyBatchOfCheckinsWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.SaveCheckins(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkins").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d rows, want none", n)
	}
}

// TestABatchOfCheckinsWithABadRowIsNotWrittenAtAll says one bad row
// stops the whole batch.
//
// 整批是一个事务：一个写不下去，一整批都回去 —— 半批写下去比全批写下去
// 更贵，而丢掉的最多是这一批里那些号今天的积分进不了池。
func TestABatchOfCheckinsWithABadRowIsNotWrittenAtAll(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "one")

	err := s.SaveCheckins(ctx, []Checkin{
		{AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25},
		// 这个号不在库里：外键不答应，这一批就都回去。
		{AccountID: "nope", Day: "2026-11-19", Outcome: "claimed", Credit: 25},
	})
	if err == nil {
		t.Fatal("the batch with the bad row was written down")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkins").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d rows, want none: the batch is one transaction", n)
	}
}

```

`internal/manager/manager_test.go` —— 记录写下去了，写不进去的记在每个号上：

```go
// TestTheCheckinRecordsAreWrittenDown says the records of a check-in are
// written down.
//
// 记录是签到给的积分进池的那条路（规格 §5.3）：签到了可是记录没了，那个
// 号今天的积分就进不了池。
func TestTheCheckinRecordsAreWrittenDown(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManagerWithPath(t, nil)
	now := time.Now()
	m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	m.RecordCheckins(ctx, []store.Checkin{{
		AccountID: "acc_01", Day: "2026-11-19", Outcome: "claimed", Credit: 25,
		Streak: 3, CreatedAt: now,
	}})
	rows, err := m.Store.ListCheckins(ctx, "acc_01", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the check-ins = %+v, want one", rows)
	}
}

// TestAFailedCheckinIsNotedOnEveryAccount says a batch that could not be
// written is noted on every account of the batch.
//
// 写不进去的记录，网页上要看得见是哪个号 —— 不然签到了可是积分没进池，
// 而没有人知道是哪个号。
func TestAFailedCheckinIsNotedOnEveryAccount(t *testing.T) {
	ctx := context.Background()
	m, path := newTestManagerWithPath(t, nil)
	now := time.Now()
	for _, id := range []string{"acc_01", "acc_02"} {
		m.Store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		})
	}
	refuseWrites(t, path, "checkins", "INSERT")
	m.RecordCheckins(ctx, []store.Checkin{
		{AccountID: "acc_01", Day: "2026-11-19", Outcome: "claimed", Credit: 25, CreatedAt: now},
		{AccountID: "acc_02", Day: "2026-11-19", Outcome: "claimed", Credit: 25, CreatedAt: now},
	})
	for _, id := range []string{"acc_01", "acc_02"} {
		a, err := m.Store.GetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(a.LastError, checkinNotWrittenReason) {
			t.Errorf("the note on %s = %q", id, a.LastError)
		}
	}
}

```


（还要：空批次不报错也不写东西；一条坏的那批整批都不落。）

`internal/scheduler/scheduler_test.go` —— 五十个一批：

```go
// TestTheCheckinRecordsAreWrittenInBatches says the check-in records are
// written fifty at a time.
//
// 一趟签到几百个号。一个号一个事务要把签到的几页重写一遍（1000 个号是
// 17 MB 的日志，五十个一批是 1 MB）。而且一批中间要是有一条坏了，那一批
// 整批都不落 —— 所以「攒够五十写一次」这件事要验。
//
// 怎么验「攒够五十」：让最后一个号的那条记录写不进去（一个触发器拒绝它）。
// 全是一批（51 个一个事务）的话，那一批整个不落 —— 一个记录都没有；攒成
// 五十加一的话，前五十个落了，最后那个没落。
func TestTheCheckinRecordsAreWrittenInBatches(t *testing.T) {
	ctx := context.Background()
	s := checkinRound(t, 51, "acc_50")

	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("acc_%02d", i)
		rows, err := s.ListCheckins(ctx, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("the check-ins of %s = %+v, want one", id, rows)
		}
	}
	rows, err := s.ListCheckins(ctx, "acc_50", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the check-ins of acc_50 = %+v, want none", rows)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/store/ ./internal/manager/ ./internal/scheduler/ -count=1
```

- [ ] **Step 3: 实现**

（测试本身。这一件没有生产代码要改 —— 除非测试揭出真的有毛病。）

- [ ] **Step 4: 跑测试，看它过了**

```bash
go test ./... -count=1
```
## 前后两遍

（跑完 Task 7 填进来。同一台机器，同一条命令：）

```
go test ./internal/store/ -run XXX -bench . -benchtime 30x -count=3
```

**改之前**的那一遍是把 `feature/usage-refresh` 那份代码原样拷出来跑的
（`git archive feature/usage-refresh | tar -x -C E:/tmp/before-repo`），
夹具和命令一模一样，只有两处要改：`Open` 不带那两个新选项（老代码没有），
`BenchmarkCheckinTimes` 换成老的那条 `LatestCheckins`。

| 基准 | 改之前 | 改之后 | 快了 |
|---|---|---|---|
| `BenchmarkPoolCandidates` | 4.67 ms | 4.11 ms | 没变 |
| `BenchmarkLatestUsageSnapshots` | 21.92 ms | **3.71 ms** | 5.9 倍 |
| `BenchmarkAccountsWithoutCheckin` | 3.62 ms | **2.73 ms** | 1.3 倍 |
| `BenchmarkLatestCheckins` → `BenchmarkCheckinTimes` | 750.62 ms | **4.53 ms** | 166 倍 |
| `BenchmarkDayCheckins` | 0.83 ms | **0.57 ms** | 1.5 倍 |
| `BenchmarkCountUsable` | 0.39 ms | 0.37 ms | 没变 |
| `BenchmarkPruneUsageSnapshots` | 26.63 ms | **18.29 ms** | 1.5 倍 |
| `BenchmarkSaveUsageSnapshot` | 874 µs | **64 µs** | 14 倍 |

还有四个「改写本身」的基准：两条 SQL 在同一个库上跑，索引是同一套，差的只
有写法。

| 基准 | 旧写法 | 新写法 |
|---|---|---|
| `BenchmarkIsoNoCheckinOld` → `BenchmarkIsoNoCheckinNew` | 1.43 ms（`NOT EXISTS`） | **0.77 ms**（`NOT IN`） |
| `BenchmarkIsoUsageOld` → `BenchmarkIsoUsageNew` | 23.10 ms（扫读数表） | **3.08 ms**（从 `accounts` 出发） |

四件事要说明白：

- **数字是三遍里最快的那一遍**（`-count=3`）。这台机器上还跑着别的东西，
  数字会飘：`BenchmarkSaveUsageSnapshot` 那一栏（写一笔要 fsync）在跑与跑
  之间差过十几倍（0.87 ms 到 11 ms），所以那一栏只能当量级看。
- **`BenchmarkCheckinTimes` 那一行的前后不是同一件事**：改之前是「每个号
  最后一次签到」（扫全表），改之后是「最近 7 天签到过的号」。138 秒那个
  数字是一年 36 万行的库上的；3 万行的库上就是 751 ms —— 一样不能用
  （这一档每 30 秒问一次）。
- **`BenchmarkSaveUsageSnapshot` 快 14 倍**是 `synchronous=normal` 的功劳
  （不是每笔都 `fsync`），不是 SQL 的功劳。SQL 一个字没动。
- **`BenchmarkPoolCandidates` 和 `BenchmarkCountUsable` 没有变**：新加的索引
  一条都没盖住它们（走的是 `accounts` 的 `status` 和那几条排序规矩），而
  它们在一条一分钟才跑几次的路上。

还有两条不是基准的：

- **token 写几遍**（Task 10）：另一个连接看着 `PRAGMA data_version` —— 别的
  连接提交了它就变。改之前每个被代理的请求都提交一次（所以每个请求它都
  变），改之后一个续签只变一次。这一条不是快不快的事：是写锁被占了多久
  的事。
- **库的大小**：一年 40 MB → 30 天 3 MB。
