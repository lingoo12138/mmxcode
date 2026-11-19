# SQLite 调优 —— 设计文档

> 日期：2026-11-19
> **状态：设计定稿**（用户 2026-11-19：「我们还是sqlite，但是不换CGO版本的，你看看sqlite的性能优化怎么做，查询优化」—— 纯 Go 的 `modernc.org/sqlite` 留着，动手的是索引、查询和 pragma）。
> **还没写代码。** 计划在 `docs/superpowers/plans/2026-11-19-mmxcode-sqlite-tuning.md`。
> 相关：[[2026-11-18-mmxcode-auto-checkin-design]]（签到）、[[2026-11-17-mmxcode-account-pool-staging-design]]（账号池）、[[2026-11-13-mmxcode-gateway-design]]（网关）

## 1. 原话

> 「可以按照你的推荐，我们还是sqlite，但是不换CGO版本的，你看看sqlite的性能优化怎么做，查询优化，这些你自己拉多几个agent讨论下，自己干活吧，我上班了。」

三条约束，贯穿全文：

1. **还是 SQLite**，不换 JSON 文件（上一轮讨论的结论：JSON 要自己实现查询引擎，内存反而更高，多进程一致性也没有）；
2. **不换 CGO 版**，`modernc.org/sqlite`（纯 Go）留着 —— 免 CGO、交叉编译和 `gcc` 依赖；
3. **动的是查询和索引**，不是驱动、不是表结构的大改、不是换数据库。

## 2. 现在的库长什么样

先量了一遍（Windows 11 / i7-7700 3.6GHz / Go 1.26 / `modernc.org/sqlite v1.62.0`），
夹具是「1000 个号、一年的签到、每个号 15 条读数」—— 一年 = 36.5 万行签到，
`checkins` 表本身 18 MB，加上索引 40 MB 出头。

**读一次要多久**（`internal/store/bench_test.go`，同一台机器）：

| 查询 | 谁在问 | 一次 |
|---|---|---|
| `LatestCheckins`（每个号最后一次签到） | **读额度那一趟，每 30 秒一次** | **138 秒** |
| `LatestUsageSnapshots`（每个号最新的一条读数） | 卡片、`/v1/usage`、读额度那一趟 | 22.3 ms |
| `PoolCandidates`（填池的候选） | 网关补池，每 30 秒一次 | 15.0 ms |
| `AccountsWithoutCheckin`（今天谁还没签） | 签到那一趟，一天一次 | 11.3 ms |
| `DayCheckins`（今天签到的积分） | 签到完进池 | 794 µs |
| `CountUsable`（有几个号能答） | 网关算并发 | 365 µs |

`LatestCheckins` 的 138 秒不是笔误 —— 那条查询的形状是
「扫一遍 `checkins` 全表，每一行再问一次『这个号最后一次签到是哪一行』」，
而那个『再一次』没有能用的索引：36.5 万行外层扫描 × 每个号 365 行的排序。
**读额度那一趟每 30 秒问一次这个问题**：跑在 1000 个号的库上，读额度这一趟
永远跑不完（它有 30 秒的预算，于是每一趟都超时，一趟也读不完）。

这不是「有点慢」，这是**规模化之后功能坏掉**。一千个号是设计的目标规模
（规格 §5.1：「1000 个号也就 1000 秒」），到了这个规模，读额度这一趟是坏的。

## 3. 三个病根

### 3.1 签到的历史只涨不落

`checkins` 一行一个号一天，一天一个号。1000 个号一年 36.5 万行、40 MB，
三年就是 120 MB。而库里的签到积分**七天就过期**（规格 §5.3），网页上每个号
只摆最近 5 条（`internal/webui/webui.go:318`）。

**一年以前的签到记录，除了占地方没有别的用处。** 现在没有一处代码删它
（`internal/store/store.go:1788` 的 `SaveCheckin` 只写不删），于是：

- 库越来越大；
- 每一个碰 `checkins` 的查询都跟着变慢（下面两条都是）。

### 3.2 索引是按「表长什么样」建的，不是按「怎么问」建的

现在的索引（`internal/store/schema.go:265-269`）：

```sql
CREATE INDEX idx_usage_account ON usage_snapshots(account_id, created_at DESC);
CREATE INDEX idx_sessions_account ON sessions(account_id);
CREATE INDEX idx_checkins_day   ON checkins(day);
```

三个问题：

1. **`checkins` 上没有 `(account_id, created_at)` 的索引。** 「这个号最后
   一次签到是哪一行」要的是它，而 `UNIQUE(account_id, day)` 这个自动索引
   按**日子**排，不按**时刻**排 —— 于是 SQLite 只能把这个号的 365 行捞出来
   重新排一遍。这就是那 138 秒。
2. **`idx_checkins_day` 只盖住 `day` 一列。** 「今天签到的号和它们的积分」
   （`DayCheckins`）要 `account_id` 和 `credit`，索引盖不住 → 每行都要回表
   一次（1000 次随机读）。
3. **`accounts` 上没有索引。** 「有几个 active 号」「今天谁还没签（按创建
   顺序）」都是全表扫 + 临时排序。

### 3.3 查询的形状是「一行一行地问」

`AccountsWithoutCheckin` 是这样问的：

```sql
... WHERE a.status = 'active'
  AND NOT EXISTS (SELECT 1 FROM checkins c WHERE c.account_id = a.id AND c.day = ?)
```

1000 个号 → 1000 次对 `checkins` 的随机探测。签到的表越大（3.1），探测
越贵（一年 36 万行的库上是 4.05 ms）。而**同一天签到的那些号是可以一次拿全
的** —— 索引 `idx_checkins_day` 上那一天就是连着的一千行。

## 4. 改什么

### 4.1 签到的历史有界（`sqlite.checkin_retention_days`，默认 30 天）

- 库里加一步：**每天签到那一趟跑完，删掉 `day` 早于 30 天前的签到**。
- 30 天是「积分七天过期」和「网页上要看得见历史」之间的一个宽松的折中：
  30 天以前的积分早过期了，网页只摆最近 5 条，剩下的没人看。
- 配 `sqlite.checkin_retention_days: 0` = 永远不删（想留全历史的人自己选）。

**效果**：36.5 万行 → 3 万行；`checkins` 表 40 MB → 3 MB；所有碰它的
查询都快一个数量级；库不再一年涨 40 MB。

**行为变了的地方**：30 天以前的签到记录没有了。这是唯一一处「数据被删」，
单列在 §6。

### 4.2 索引（迁移的第 7 步）

新增：

| 索引 | 给谁用 | 替掉 |
|---|---|---|
| `checkins(day, account_id, created_at, credit)` | 「今天谁签到、给了多少积分」「今天谁没签」 | `idx_checkins_day` |
| `checkins(account_id, created_at DESC, id DESC)` | 「这个号最近签到的时刻」 | `idx_checkins_account` |
| `usage_snapshots(account_id, created_at DESC, id DESC)` | 「每个号最新的一条读数」「清理读数」 | `idx_usage_account` |
| `sessions(account_id)` | 填池那条查询里「这个号有没有绑着对话」 | — |

三条「替掉」的索引都是被新索引**完全盖住**的旧索引（新的列更多、顺序更全），
迁移里把它们删掉：留着就是每次写都多维护一份，白花写的时间和空间。

**试过、没有好处、所以没有建的**（在 1000 个号 / 36 万行签到的库上量的，
每条量三遍取最快的那一遍）：

| 没建的 | 为什么 |
|---|---|
| `accounts(status, created_at, id)` | 填池那条查询 6.15 ms → **6.79 ms**（慢了）。顺序扫变成了按索引随机取行 |
| `sessions(account_id, updated_at)` | 写会话（一批 100 个 key）0.77 ms → **0.82 ms**（慢了）—— 请求的路上一分都不能慢 |
| `sessions(updated_at)` | 数活跃会话 0.196 ms → **0.247 ms**（慢了）。而它省下的那点时间在一条每 20 秒才跑一次的路上 |

三条都在噪声边上，而每一条都要在每次写的时候多维护一份。**索引不是越多越好**：
每一条都要在每次写的时候维护，而写的那条路上有请求。所以每一条索引都对应
一条**具体的、被量出来的**查询。

### 4.3 查询改写

| 查询 | 现在 | 改成 | 量出来的 |
|---|---|---|---|
| `AccountsWithoutCheckin` | `NOT EXISTS`（1000 次随机探测） | `NOT IN (SELECT account_id FROM checkins WHERE day = ?)`（一次把那一天捞全，SQLite 拿它建布隆过滤器） | 3.62 ms → **2.73 ms**（改写本身：1.43 ms → 0.77 ms） |
| `LatestUsageSnapshots` | 从读数表里扫，靠相关子查询找每个号最新的一条 | 从 `accounts` 出发，连到每个号最新的一条读数（`ORDER BY created_at DESC, id DESC LIMIT 1`） | 21.92 ms → **3.71 ms**（改写本身：23.10 ms → 3.08 ms） |
| `LatestCheckins` | 「每个号最后一次签到」（扫全表） | **删掉这个方法**，改成问「最近 7 天签到过的号」 | 750.62 ms → **4.53 ms**（一年 36 万行的库上：138 秒 → 67.8 ms） |
| `PoolCandidates` | 现在的写法 | 不改 SQL，靠新索引 + 签到历史变短 | 4.67 ms → **4.11 ms**（几乎没变） |

「改写本身」那一栏是**两条 SQL 在同一个库上跑**（索引是同一套，差的只有
写法）：`BenchmarkIsoNoCheckinOld`/`New` 和 `BenchmarkIsoUsageOld`/`New`。
30 天的夹具上量的；一年 36 万行的库上，「今天谁没签」是 4.05 ms → 1.52 ms。

第一条和第三条的写法在 §4.3 展开。

#### 两条查询改写

**「今天谁没签」** —— 从「一个号一次问一次签到没有」改成「今天签到过的号有
哪些，一次拿全，再去掉它们」：

```sql
-- 改之前：1000 次对签到表的随机探测
WHERE NOT EXISTS (SELECT 1 FROM checkins c WHERE c.account_id = a.id AND c.day = ?)

-- 改之后：一次把那一天捞全，SQLite 拿它建布隆过滤器
WHERE a.id NOT IN (SELECT c.account_id FROM checkins c WHERE c.day = ?)
```

`NOT IN` 有个老毛病：子查询里只要有一个 NULL，结果就一个都不返回。签到的
`account_id` 是 `NOT NULL` 的，迁移的时候老行补的也是空串不是 NULL ——
所以不会。测试里直接查这个语句的查询计划（`LIST SUBQUERY`，不是
`CORRELATED SCALAR SUBQUERY`），免得以后有人手一抖改回去。

**「每个号最新的一条读数」** —— 从「从读数表里扫，靠相关子查询找每个号最新
的一条」改成「从 `accounts` 出发，连到每个号最新的一条读数」：

```sql
-- 改之后
SELECT ...
FROM accounts a JOIN usage_snapshots u ON u.id = (
    SELECT s2.id FROM usage_snapshots s2
    WHERE s2.account_id = a.id
    ORDER BY s2.created_at DESC, s2.id DESC
    LIMIT 1)
ORDER BY u.account_id
```

「最新」的判法一个字没动：时刻优先，id 平局（规格 §3.4：一条读数开始的时候
打时刻、结束的时候写下来，两条重叠的读数里旧的那条 id 反而大）。

#### 为什么「最近 7 天」就够了

读额度那一趟要的是「**签到待补**」这一档（`internal/usage/round.go:180-210`）：
签到给了积分，可是卡上还摆着签到之前的旧数 —— 这种号要现读一次额度。

现在这一档是这么判的：

```go
pending = !hasSnap || checked.After(read)     // 签到比上一次读额度还新
if !pending && !snapHas[id] && now.Sub(checked) < retryWindow {
    pending = true                            // 读回来了但没有数，10 分钟内再试
}
```

`checked` 是「这个号最后一次签到的时刻」。**可是它只在两个地方说话**：

1. `checked.After(read)`：签到比上一次读额度还新 → 要补读。
2. `now.Sub(checked) < retryWindow`（10 分钟）：刚签到，读回来却没数 → 再试。

一条**七天以前**的签到，两条都占不上：

- 第 1 条：那条签到的积分早过期了，补读它读回来的也是过期数（卡上摆的还是旧数，可那旧数跟一个过期积分无关）；
- 第 2 条：10 分钟的窗口早就过去了。

而七天是签到积分自己的有效期（规格 §5.3：「签到刚给了积分（七天有效）」）。
**所以「最近七天的签到」跟「全部签到」在这条判断上是一回事。**

新方法签名：

```go
// CheckinTime is when an account was checked in.
type CheckinTime struct {
	AccountID string
	At        time.Time
}

// CheckinTimes gives, for every account checked in since a moment, the
// time of that check-in.
func (s *Store) CheckinTimes(ctx context.Context, since time.Time) ([]CheckinTime, error)
```

问的是「每个号在这段时间里**最后一次**签到」：一条 `GROUP BY account_id`
配 `MAX(created_at)`，走新索引 `checkins(account_id, created_at DESC, id DESC)`
的覆盖前缀。时刻说了算（`MAX` 出来的就是最大的时刻），平局的时候是哪一个
不影响 —— 这里的时刻就是全部。

读额度那一趟问的是「最近 7 天」（`internal/usage` 里的常数 `checkinWindow`，
注释里写清楚它为什么是七天）。

**行为变了的地方**：`LatestCheckins` 这个方法没有了（库里只有读额度那一趟
用它）。见 §6。

### 4.4 pragma（`sqlite:` 一节）

| 键 | 默认 | 是干什么的 |
|---|---|---|
| `sqlite.cache_mb` | 8 | 一条连接的页缓存（MB）。SQLite 自己默认 2 MB。 |
| `sqlite.synchronous` | `normal` | 写盘要不要每次都 `fsync`。`full` 是 SQLite 的默认。 |
| `sqlite.checkin_retention_days` | 30 | 签到历史留多少天。0 = 永远留。 |

**`synchronous=normal` 换了什么**：WAL 模式下 `normal` 跟 `full` 的差别是
「断电的时候，最近这几笔写可能丢」。SQLite 官方对 WAL 的推荐就是 `normal`
（不会损坏库，最多丢最后几笔）。

丢得起的写有很多：额度快照旧一点（下一趟补上）、「用完了」的标记没写上
（撞一次空，换号重试，标记就写准了）、会话行没写上（会话重新绑一个号）。

**丢不起的只有两笔**：

1. **续签后的 token**。refresh token 在站点那边是一次性的：库里那笔丢了，
   下一次续签拿的就是一个**已经被站点用过的** refresh token，站点会拒，
   账号就被记成签入失效 —— 要重新签入（密码在库里，能自动做，但要多花
   一次站点调用，而且账号在那之前是死的）。
2. **「额度用完了」被清掉的那一笔**。签到给的积分能救活一个用完的号，
   标记清掉了它才回到轮转里。断了电丢了这一笔，它就还被关在外面 ——
   最多关到签到的积分过期（七天）。

**这两笔走另一条连接**：同一个文件的第二条连接，用 `synchronous=full` 开的
（连接池上限 1 —— 这两笔很少写，写锁也只有一把）。提交的时候它把 WAL
fsync 掉，写完了就真的在盘上。常用的那条连接还是 `normal`（写一笔 64µs 对
full 的 874µs —— full 那一笔要等盘）。

**为什么不是 `wal_checkpoint(FULL)`**（这一版推翻的做法）：checkpoint 要把
WAL 里的页**抄回主库**，抄的时候**拿着 WAL 的写锁**。别的写要排队，排队
排满了（`busy_timeout` 5 秒）就是 `SQLITE_BUSY` —— 线上已经报出来了：

```
save token: database is locked (5) (SQLITE_BUSY)
```

更糟的是它被放在了**每个请求**上。库里存的时间是毫秒（`expires_at` 是
`UnixMilli`），而续签那一刻拿到的是毫秒以下的精度；不砍到毫秒去比，库里
那笔永远比手里那笔「旧」那么一点 —— 于是每个请求都以为要重写，于是每个
请求都写一次、checkpoint 一次。fsync 只等盘，不抄页，也不挡别人写。

**顺带改掉的那个比较**（§4.6）：库里存的是毫秒，比的时候两边都要砍到毫秒。
砍完之后，一个续签过的账号**只写一次**。

**要更稳就配 `sqlite.synchronous: full`**：写还是每次 `fsync`，慢一点
（量出来的差别在 §7）。配了 `full` 那两笔就不用另开连接 —— 主连接自己就
在 fsync。

**`cache_mb=8` 换了什么**：一条连接最多缓存 8 MB 的页。库的工作集
（1000 个号 + 30 天签到 + 读数）大约 5 MB —— 8 MB 的缓存装得下工作集，
于是同一批查询不再一遍一遍地去磁盘要页。一年 36 万行签到的库上，填池那条
查询从 11.7 ms 到 5.9 ms。四条连接最多 32 MB，**但缓存是用多少占多少**：
库小的时候它占不到那么多。

**写错的 `sqlite:` 一节在启动时就被拒**：

- `synchronous` 只有 `normal` 和 `full` 认（空是 SQLite 自己的默认）。
- `cache_mb` 不能是负数。0 是 SQLite 自己的默认（2 MB）；负数以前是**悄悄
  按 0 办的**（那个 pragma 只在正数时才加）—— 写 -1 的人以为自己在说什
  么，实际上什么也没说。
- `checkin_retention_days` 要是 0（永远留）或者 **7 天以上**。签到的积分
  七天就过期，「待补」那一档只看最近七天的签到；而清理是按**站点的那个
  日子**删的（`day` 那一列是站点时区的那一天），站点时区跟 UTC 差着几个
  小时。留 6 天的话，被删掉的那一段里可能有七天窗口还要用的行 —— 那些号
  就永远等不到「签到待补」那一读，卡上摆的数就一直旧着。

别的词 SQLite 不认，而库打开的时候才说 —— 那已经晚了一步。跟
`server.api_keys`、`schedule.checkin_at` 一个规矩：宁可启动时报错，也不悄
悄按别的来。

内存这件事上一轮量过：新起的服务工作集 29.8 MB、提交量 48.6 MB；
1000 个号 36.2 MB / 55.7 MB。这次的改动往上涨的只有页缓存（有上限），
换来的是 §7 里那些毫秒。

### 4.5 签到的记录一批一批写

签到那一趟是唯一一趟往签到表里写的事，而它原来是**一个号一个事务**：每个
号都要把签到表的那一页和它的几个索引页重写一遍。量出来的（1000 个号）：

| | 每个号 | 一趟 |
|---|---|---|
| 一个号一个事务 | 18004 B | **17.2 MB** |
| 五十个一事务 | 1133 B | **1.1 MB** |

一天一趟的事，17 MB 的日志换 1 MB 是值得的。而且 §4.2 新加的两个索引让每
一行要写的页多了一个（表页 + 三个索引页），不合成一批写，这一条就把新索
引省下来的还回去了。

**丢得起**：一批丢了的记录是「签到成了、可是记录没了」，最多五十个号今天
的积分进不了池（规格 §5.3）。签到本身在站点那边已经成了。

### 4.6 token 写几遍 —— 比的时候两边都砍到毫秒

**这一件是线上那条错的根因，而且它在改这一版之前就在。**

库里 `tokens.expires_at` 存的是**毫秒**（`ms(t)` = `t.UnixMilli()`），而续签
那一刻拿到的 `oauth.Token.Expires` 是**毫秒以下**的精度。于是：

- 库里那笔 = `fromMS(ms(手里那笔))` = 手里那笔砍到毫秒；
- `stored.ExpiresAt.Before(tok.Expires)` → 砍过的那一个**永远**比手里那个
  「旧」那么一点（除非它正好落在毫秒的边界上）；
- 于是 `newerThanStored` **永远为真** → `Manager.Token()`（每个被代理的请求
  都调它）**每个请求都重写一遍 token 行**；
- 每个请求一次写事务。老代码没有 `sqlite:` 那一节，`synchronous` 没配 =
  SQLite 的默认 `full` = **每一笔都 fsync**（874µs）→ 写锁排长队 → 排满
  `busy_timeout(5000)` → `SQLITE_BUSY`：

```
save token: database is locked (5) (SQLITE_BUSY)
```

**改法**：比的时候两边都砍到毫秒。`internal/store` 加一个导出函数 ——
「库里存的是毫秒」这件事只有那个包里知道：

```go
// AtMillisecond gives the time as the store writes it down.
func AtMillisecond(t time.Time) time.Time {
	return fromMS(ms(t))
}
```

`newerThanStored` 和 `saveToken` 里的那个守卫都用它。砍完之后，一个续签过
的账号**一次续签写一次**，不是每个请求写一次。

**写下去之后，那条「写不下去」的记号要擦掉。** 写 token 失败的时候账号上会
留一条记号（`last_error`）：「库里那份比手里那份旧」。库追上来了，那条记号
就不成立了 —— 而 `last_error` 只有在改账号状态的时候才会被清掉，所以一次暂
时的忙会在一个好好的账号上留一条「这个号有问题」。写成功之后擦掉它 —— 只擦
这一种理由的记号：别的理由（续签失败但手里那份还好）说的还是真的，擦了就是
把真话藏起来。

这一条跟 §4.4 是两件事，但它们一起造成了同一条错：这一条让每个请求都要写，
§4.4 那个（被推翻的）flush 又让每个请求都要 checkpoint —— 而 checkpoint
拿着写锁。

## 5. 不改什么

- **不换驱动**：`modernc.org/sqlite`（纯 Go）不动，不引入 CGO。
- **不换存储**：不换 JSON，不换别的数据库。
- **不改语义**：排序的规矩、池的规矩、签到的规矩，一条都不动。
  `PoolCandidates` 那六条排序规则、`AccountsWithoutCheckin` 的名单、
  `LatestUsageSnapshots`「最新」的判法（时刻优先、id 平局）都原样。
- **不动 `usage_snapshots` 的清理规矩**：一刻钟之外、每号最新的一条留着
  （规格 §3.4）。
- **不删 `sessions`**：会话表是亲和的依据，删了老对话的缓存命中就没了。

## 6. 行为变了的地方（只有八处）

1. **30 天以前的签到记录会被删掉**（§4.1）。配 0 可以不删。
2. **`LatestCheckins` 这个方法没了**，改成 `CheckinTimes(ctx, since)`
   （§4.3）。用它的只有读额度那一趟。
3. **读额度那一趟问的是「最近 7 天的签到」**，不是「全部签到」（§4.3）。
   判「签到待补」的结果一样（理由在 §4.3）。
4. **续签后的 token 和被清掉的「用完了」标记，走一条会 fsync 的连接**
   （§4.4）。断了电，这两笔不会丢。
5. **续签过的账号，token 只写一次**，不是每个请求写一次（§4.6）。从前那个
   「每个请求写一次」就是线上 `SQLITE_BUSY` 的来由。
6. **签到的记录一批一批写**（§4.5）：一趟签到攒够五十个写一次。进程在
   一趟签到中间没了的话，最多五十个号的记录没了（签到在站点那边已经成了，
   丢的是记录，也就是今天的积分进不了池）。
7. **写错的 `sqlite:` 一节让网关拒绝启动**（§4.4）。跟 `server.api_keys`、
   `schedule.checkin_at` 一个规矩。
8. **token 写成功之后，账号上那条「token 写不下去」的记号被擦掉**（§4.6）。
   只擦这一种理由的记号 —— 别的理由说的还是真的。

## 7. 怎么量

`internal/store/bench_test.go`（新增）就是这个量具：1000 个号、30 天的签到
（3 万行）、每个号 15 条读数（1.5 万行），同一个夹具跑八个基准，外加四个
「改写本身」的基准（两条 SQL 在同一个库上跑，索引是同一套）。

夹具是**稳态**的样子：签到的历史留 30 天（`sqlite.checkin_retention_days`），
每个号留最新的一条读数，外加最近一刻钟里读的那些（清理那一趟留的）。一年
36 万行签到的那种库不是稳态 —— 那是没删干净的库。

改前改后各跑一遍，同一台机器、同一条命令：

```bash
go test ./internal/store/ -run XXX -bench . -benchtime 30x -count=3
```

**改之前**跑的是 `feature/usage-refresh` 那份代码（`git archive` 出来的一份
拷贝），夹具和命令一模一样，只有两处要改：`Open` 不带那两个新选项（老代码
没有），`BenchmarkCheckinTimes` 换成老的那条 `LatestCheckins`。

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

| 改写本身 | 旧写法 | 新写法 |
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

- **token 写几遍**（§4.6）：另一个连接看着 `PRAGMA data_version` —— 别的连接
  提交了它就变。改之前每个被代理的请求都提交一次（所以每个请求它都变），
  改之后一个续签只变一次。这一条不是快不快的事：是写锁被占了多久的事。
- **库的大小**：一年 40 MB → 30 天 3 MB。

## 8. 风险

| 风险 | 怎么办 |
|---|---|
| 迁移第 7 步在老库上建索引要时间 | 1000 个号的库上建这四个索引，估几十秒；迁移是一个事务，失败就回滚，老库原样不动 |
| `NOT IN` 遇到 NULL 会一个都不返回 | `checkins.account_id` 是 `NOT NULL`（`schema.go:162`），迁移时老行也补的是 `''` 不是 NULL（`schema.go:170`）—— 不会 |
| 删了签到历史，网页上看不到老的 | 30 天，网页只摆最近 5 条；配 0 可以不删 |
| `synchronous=normal` 断电丢最后几笔 | 丢得起的那些（读数、标记没写上、会话行）都能自己好起来；丢不起的两笔（续签后的 token、被清掉的标记）走那条会 fsync 的连接（§4.4）。想更稳就配 `full` |
| 多一条连接就多一个句柄 | 同一个文件的第二个句柄，页缓存跟主连接一样 —— 库小的时候它占不到那么多 |
| 那一笔 fsync 要等盘 | 一笔写 874µs 对 64µs。而它只在续签（一个号一个钟头一次）和签到救活一个号的时候发生 —— 一天几十次，一次不到一毫秒 |
| `checkin_retention_days` 少于 7 天会毁掉「待补」那一档 | 启动时就拒（§4.4） |
| 第二条连接跟主连接抢写锁 | 写锁本来就是一把（SQLite 就一把）。这条连接只有一条连接，所以它自己不会跟自己抢 |
| 一趟签到中间进程没了 | 最多五十个号的签到记录没了（站点那边已经签成了，丢的是记录 —— 今天的积分进不了池）；签到下一趟不会重来（站点说 done） |
| 页缓存占内存 | 每条连接最多 `cache_mb`，用多少占多少；四条连接封顶 32 MB |

## 9. 计划

`docs/superpowers/plans/2026-11-19-mmxcode-sqlite-tuning.md`，十二件事：

1. 迁移第 7 步：四个索引（被盖住的三条旧的删掉：`idx_checkins_day`、
   `idx_checkins_account`、`idx_usage_account`）；
2. `AccountsWithoutCheckin` 改写成 `NOT IN`；
3. `LatestUsageSnapshots` 改成从 `accounts` 出发；
4. `LatestCheckins` → `CheckinTimes`，读额度那一趟改问最近 7 天；
5. 签到历史有界：库里加一步删老的，签到那一趟跑完删一次；
6. pragma：配置里的 `sqlite:` 一节（页缓存、`synchronous`、留多少天）；
7. 基准与测试：量具留着，前后两遍的数字写进计划里；
8. 丢不得的那两笔走一条会 fsync 的连接（`Store` 多一条 `durable` 连接，
   `Flush` 拿掉）；
9. 签到的记录一批一批写；
10. 拿库里存的那个毫秒去比的时候，两边都砍到毫秒（token 只写一次）；写成功
    之后把账号上那条「token 写不下去」的记号擦掉；
11. `sqlite:` 一节的三个键都在启动时查（`cache_mb` 不能是负数、
    `checkin_retention_days` 要 0 或者 7 天以上）；
12. 补上评审点出来的测试缺口（`SaveCheckins`、`RecordCheckins` 的分批）。
