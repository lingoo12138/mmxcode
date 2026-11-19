# 每个请求少打几次库 —— 设计

日期：2026-11-19
状态：已定。上一条分支（`feature/sqlite-tuning`）合了之后落进 `docs/superpowers/specs/`。
计划：`docs/superpowers/plans/2026-11-19-mmxcode-request-db-round-trips.md`。

## 1. 原话

用户：「你看看 sqlite 的性能优化怎么做，查询优化。」

上一轮做的是**库那一侧**（索引、查询形状、pragma、写的事务）。这一轮做的是
**请求那一侧**：一个客户端请求打了几次库，哪些能省。

## 2. 现在的账（实测）

调查在副本 `E:\tmp\inv-req\repo` 里做，`internal/store/counting.go` 给 SQLite
套了一层计数，把每条语句和它的耗时都记下来。完整的报告在
`E:\tmp\inv-req\report.md`（383 行）。

机器：Windows 11 / i7-7700 3.6GHz。**时钟粒度约 500 µs**，所以「每次调用多少
微秒」都是 200~500 次的平均。

### 2.1 热路径（池里有号、会话已绑定、令牌没过期）

一次 `/v1/chat/completions` 打到 **5 条语句**，平均 **254 µs** 的库时间：

| 顺序 | 方法 | 目的 | 一次 |
|---|---|---|---|
| 1 | `Store.HasAPIKeys` | 「后台有没有申请过 key」——没有配置 key 且没申请过 key 时不鉴权 | 63 µs |
| 2 | `Store.UpstreamID` → `upstreamIDOf` | 取这轮对话发给上游的固定 id | 56 µs |
| 3 | `Store.SessionAccount` | 轮转要知道这轮对话钉在哪个号上 | 30 µs |
| 4 | `Store.GetToken` | 取 access token 去问上游 | 51 µs |
| 5 | `Store.GetAccount` | **只为了解析代理地址** | 54 µs |

**这 5 条里没有一条是「必须打在请求上」的。** 全都是「为了拿到一个数」。

### 2.2 冷路径（新会话第一趟）

**9 条语句**，平均 **20.3 ms**：

| 顺序 | 方法 | 一次 |
|---|---|---|
| 1 | `Store.HasAPIKeys` | 44 µs |
| 2 | `Store.CountUsable` | 49 µs（1000 个号时 402 µs） |
| 3 | `upstreamIDOf`（读，未命中） | 0.03 ms |
| 4 | `Store.UpstreamID` 的 INSERT（建会话行，顺便写下上游 id） | **10.1 ms** |
| 5 | `upstreamIDOf`（读回来） | 0.19 ms |
| 6 | `Store.SessionAccount` | 0.03 ms |
| 7 | `Store.BindSession` 的 INSERT（把会话钉到号上） | **10.0 ms** |
| 8 | `Store.GetToken` | 65 µs |
| 9 | `Store.GetAccount` | 58 µs |

**冷路径的 20 ms 里 20 ms 全是两次写**，读只占 0.22 ms。（这 20 ms 是在
`synchronous=full` 下量的；上一轮已经把 `normal` 做成默认，所以现在是
1.28 ms —— 不过那两次写还是两次。）

### 2.3 100 个并发冷请求

**874 条语句（8.74 条/请求），库时间 9.37 s** —— 其中写占 9.30 s。

**最大的那笔钱不在往返上，在写锁排队上**：100 个并发各写一次 `sessions`，在
写锁上排成队。缓存一条也省不掉这个。

### 2.4 后台那一趟

`usage.Round.Targets` 读四张表（`ListAccounts`、`LatestUsageSnapshots`、
`CheckinTimes`、`ListExhausted`）。1000 个号时 `ListAccounts` 是 2.8 ms。

## 3. 改什么

### 3.1 会话那一行读一次就够（省 1 条 / 26.8 µs，零语义风险）

`sessions` 那一行的 `account_id` 和 `upstream_id` 现在分两次读：

- `rotator.UpstreamID`（`internal/rotator/rotator.go:536`）读 `upstream_id`
- `rotator.Pick`（`internal/rotator/rotator.go:338`）读 `account_id`

合成一条 `SELECT account_id, upstream_id FROM sessions WHERE session_key=?`。

**唯一的坑**：两个调用方各自去调那个新方法，还是 2 条往返，等于没改。所以新
方法必须让调用方一次拿到两列。

改法：`rotator.Pool` 接口上加一个 `SessionState(ctx, key) (SessionState, error)`，
`Rotator.UpstreamID` 读一次，把结果带着往下传；`Pick` 用带着的那份，不再读。

### 3.2 冷路径的两次写合成一条（省 1 条写）

`UpstreamID` 的 INSERT 和 `BindSession` 的 INSERT 写的是**同一行**（同一个
`session_key`）。合成一条 INSERT，写进 `account_id` 和 `upstream_id`。

**坑一：「写完不读回来」。** 现在的 `UpstreamID` 写完要读一遍，是为了处理
「别人先写了，我的那笔被 `COALESCE(sessions.upstream_id, excluded.upstream_id)`
挡掉了」—— 那种情况下**该用别人写的那个 id**。合成一条之后这个判断必须保留。

最干净的做法是 `RETURNING upstream_id`（SQLite 3.35+；`modernc.org/sqlite
v1.62.0` 带的 SQLite 是 3.54.1，够）：一条语句，既写又拿回别人可能已经写好
的值。

**坑二：并发。** 两个 goroutine 同时对同一个 `session_key` 调 `UpstreamID`，
必须拿到**同一个** id。测试要按这个写：两个 goroutine 各拿一个 id，断言它们
相等，而且等于库里那一行的。

### 3.3 `HasAPIKeys` 的缓存用事件失效（省 1 条 / 44 µs）

这个是「**要不要鉴权**」，不能只用 TTL：TTL 窗口里有人在后台申请了一把 key，
那 1 秒内不带 key 的请求会被放行。窗口很短，但性质是鉴权。

WebUI 创建/撤销 key 的时候（**同一个进程**）主动把这个缓存置掉。

**坑：「false」跟「没缓存过」必须分开。** 调查的原型第一版就踩了这个坑：
缓存永远不命中（每次都当没缓存过）。所以缓存的值是一个三态的：
「没查过」/「没有 key」/「有 key」。

### 3.4 后台那一趟的两条读合成一条（省 1 条 / 2.8 ms）

`usage.Round.Targets` 里 `ListAccounts` 和 `ListExhausted` 还是两条。合成一条
（`accounts` 带上它的耗尽标记），1000 个号时每趟省 2.8 ms。

**坑：「最新」的判法一个字都不能动**（时刻优先、id 平局 —— 规格 §3.4）。

## 4. 不做什么

| 不做 | 为什么 |
|---|---|
| 缓存会话行（`sessionKey → (account_id, upstream_id)`） | **实测会错**：库里改绑了，缓存还拿着旧的号，请求被送到旧账号上（`TestSemanticsRebind` 挂在原型上）。改绑有三条路，其中一条是别的进程改的库 —— TTL 也兜不住 |
| 缓存 token | **实测会错**：读额度那一趟会续签并把新 token 写进库，聊天那条路同时在读同一个号 —— 缓存拿着旧 token 去问上游，上游 401，**而 MiniMax 的 refresh token 只能用一次** |
| 缓存账号行（只为代理地址） | 改代理以后旧代理还在用 |
| `CountUsable` 换成数池里的号 | 池空的时候它返回 0 → 总并发算成 0 → **所有请求 429** |
| 把新会话的写合进 `TouchSessions` 那一批 | 省 15.3 s / 100 并发冷请求，可是绑定最多晚 20 秒 —— 同一会话的两个并发请求可能落到不同的号上。只在 round_robin 策略下安全 |

那三个缓存的收益（95 µs / 65 µs / 58 µs）都不够大，而它们各自要一个「谁改
了就通知谁」的失效通道 —— 那个通道现在不够宽（跨进程的改绑、导入时的写
token）。**先做零风险的那几条。**

## 5. 行为变了的地方

1. **`rotator.Pool` 多了一个方法**（`SessionState`），`internal/gateway` 的
   `poolAdapter` 跟着加一个。
2. **冷路径的会话行是一条 INSERT**：新会话的第一次请求少一条写。
3. **`HasAPIKeys` 的答案缓存在内存里**：后台申请/撤销 key 之后立刻生效
   （不是等 TTL）。
4. **`usage.Round.Targets` 少读一条**。

## 6. 怎么量

- **语句条数**：给 store 套一层计数（调查用的 `internal/store/counting.go`），
  发一个热请求，断言 `SELECT ... FROM sessions` 恰好 1 次；冷请求断言整趟
  的语句数从 8 降到 5。
- **结果一致**：同一个库，新旧两条路都跑一遍，结果逐字段比对。故意造几条
  `created_at` 完全相同的读数，让新旧结果不同的时候被抓到。
- **并发**：两个 goroutine 同时 `UpstreamID` 同一个 key，断言拿到同一个 id。
- **鉴权**：没有 key → 请求通过 → 后台创建一把 key → 立刻再发一个不带 key
  的请求 → 断言 401；撤销之后同样断言 401。
- **时间**：热请求的库时间 254 µs → 目标 190 µs；冷请求 9 条 → 5 条。

## 7. 风险

| 风险 | 怎么办 |
|---|---|
| `RETURNING` 在 `ON CONFLICT` 上拿到的不是自己写的那一行 | 那是对的：别人先写的 id 就是这个会话的 id（现在的代码也是这个意思，只是多读了一次） |
| 合成一条 INSERT 之后 `account_id` 和 `upstream_id` 互相覆盖 | 两条语句写的是同一行，同一个 `session_key`；`upstream_id` 用 `COALESCE`（先到先得），`account_id` 用 `excluded`（后写的赢） |
| `HasAPIKeys` 的缓存让一把刚申请的 key 有一小段时间不认 | **用事件失效，不用 TTL**：WebUI 创建/撤销 key 的时候主动置掉。同一个进程，没有跨进程的窗口 |
| 合成 `ListAccounts` 和 `ListExhausted` 之后「耗尽的号」的口径变了 | 两条查询的口径一个字都不动，只是合在一条语句里；测试逐字段比对 |
