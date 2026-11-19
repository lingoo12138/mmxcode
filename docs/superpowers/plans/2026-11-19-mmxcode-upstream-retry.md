# 上游的重试 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 上游说了什么时候再来，网关就什么时候再来；我们自己的退避带上抖动。

**Architecture:** 网关的重试循环里多一个函数（`waitBeforeRetry`）：它读上游的
`Retry-After`，读得到就用它，读不到就用我们自己的退避；上游说的太远（超过 10
秒）就不等，换一个号。退避本身（`backoff`）加 ±20% 的抖动。

**Tech Stack:** Go 1.26、标准库（`net/http` 的 `ParseTime`、`math/rand`）。

**规格：** `docs/superpowers/specs/2026-11-19-mmxcode-upstream-retry-design.md`

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/gateway/gateway.go` | 读 `Retry-After`、退避加抖动 |
| `internal/gateway/retry_test.go`（新） | 这两件事的测试 |

---

### Task 1: 听上游说话 + 退避加抖动

**Files:**
- Modify: `internal/gateway/gateway.go`
- Create: `internal/gateway/retry_test.go`

**为什么先看规格 §2 的两个洞：** 上游说了什么时候再来，没人听（限流窗口里敲
门）；我们自己的退避每一次都一样（惊群）。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/retry_test.go`（新）：

`internal/gateway/retry_test.go`（新文件，`package gateway`；import：
`net/http`、`net/http/httptest`、`strings`、`testing`、`time`）：

```go
// TestTheRetryAfterIsRead says the upstream's Retry-After is read.
//
// 它有两种写法：一个秒数，一个 HTTP 日期。两个都要认。
func TestTheRetryAfterIsRead(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"120", 120 * time.Second, true},
		{"0", 0, true},
		{"", 0, false},
		{"soon", 0, false},
		{"-5", 0, false},
		{"Wed, 21 Oct 2026 12:00:30 GMT", 30 * time.Second, true},
		// 已经过去的日期：不是没有，是一个零（现在就可以再来）。
		{"Wed, 21 Oct 2026 11:59:00 GMT", 0, true},
	} {
		got, ok := retryAfter(c.header, now)
		if ok != c.ok || got != c.want {
			t.Errorf("the Retry-After %q = %s (%v), want %s (%v)",
				c.header, got, ok, c.want, c.ok)
		}
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run TestTheRetryAfterIsRead -v
```

失败：编译不过（`retryAfter` 还没有）。

- [ ] **Step 3: 实现 `retryAfter`**

`internal/gateway/gateway.go`：

```go
// retryAfter reads the upstream's Retry-After header.
//
// 它说的是「什么时候再来」：两种写法 —— 一个秒数（"120"），或者一个
// HTTP 日期（"Wed, 21 Oct 2015 07:28:00 GMT"）。读不出来就当没有。
//
// 已经过去的日期是「现在就可以再来」：不是没有，是一个零。
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	d := t.Sub(now)
	if d < 0 {
		d = 0
	}
	return d, true
}

```

（`strconv` 和 `strings` 要 import。）

- [ ] **Step 4: 写失败的测试（抖动）**

`internal/gateway/retry_test.go` —— 加两个：

```go
// TestTheBackoffIsNotTheSameEveryTime says two requests that hit the
// same limit do not back off at the same moment.
//
// 一起退避、一起再问，是惊群：每一遍都正好落在同一个限流窗口的边上。
func TestTheBackoffIsNotTheSameEveryTime(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[backoff(0)] = true
	}
	if len(seen) < 2 {
		t.Errorf("the backoffs = %v: they are all the same", seen)
	}
}

// TestTheBackoffGrowsAndStops says the backoff grows and then stops.
//
// 抖动是加在上面的：形状不能变 —— 半秒、一秒、两秒、四秒，封顶四秒。
func TestTheBackoffGrowsAndStops(t *testing.T) {
	if backoff(0) >= backoff(1) || backoff(1) >= backoff(2) {
		t.Errorf("the backoffs = %s, %s, %s: they do not grow",
			backoff(0), backoff(1), backoff(2))
	}
	if got := backoff(10); got > 4*time.Second {
		t.Errorf("the backoff = %s, want at most 4s", got)
	}
}

```

- [ ] **Step 5: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run "TestTheBackoff" -v
```

失败：`TestTheBackoffIsNotTheSameEveryTime` 红（每一次都一样）。

- [ ] **Step 6: 实现抖动**

`internal/gateway/gateway.go` —— `backoff` 改成这样，`jitter` 是新函数：

```go
// backoff is how long to wait before asking again.
//
// The wait grows with the attempt and is short enough that a request
// does not hang for long.
//
// 抖动是加在上面的：形状不能变 —— 半秒、一秒、两秒、四秒，封顶四秒。
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	d = jitter(d)
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

// jitter spreads a wait over a fifth of it, up and down.
//
// 一批请求同时撞上限流的时候，它们会同时退避、同时再问 —— 那是最坏的：
// 每一遍都正好落在同一个限流窗口的边上。加一点随机，它们就散开了。
//
// 上下都是五分之一：最多多等两成，也最多少等两成。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := d / 5
	if span <= 0 {
		return d
	}
	return d - span + time.Duration(rand.Int63n(int64(2*span)+1))
}

```

（`math/rand` 要 import —— Go 1.20 之后全局的 rand 自己会播种。）

- [ ] **Step 7: 跑测试，看它过了**

```bash
go test ./internal/gateway/ -run "TestTheBackoff|TestTheRetryAfter" -v
```

- [ ] **Step 8: 写失败的测试（上游说多久就等多久）**

`internal/gateway/retry_test.go` —— 假的上游第一次答 429 + `Retry-After: 1`，
第二次答 200。断言：客户端拿到 200、**用的时间 ≥ 1 秒**（从前是 0.5 秒）、上
游被问了两次。

- [ ] **Step 9: 写失败的测试（上游让等太久的时候换号）**

假的上游**每次**都答 429 + `Retry-After: 3600`。断言：这个请求**很快就结束
了**（3 秒以内 —— 绝对不能等一小时）、上游**只被问了一次**、客户端拿到 429。

**这个请求自己带一个 10 秒的时限**（`context.WithTimeout`）：网关要是真的去
等那一小时，这个时限会让它停下，测试红 —— 而不是把整个测试挂死。

**假的上游答 429 的时候 body 是空的。** 别在 body 里写「limit」「quota」这类
词 —— 那些词会被认成「额度用完」（那是换号的那条路，不是限流的那条路）。

**那个读 `Retry-After` 的测试里，「现在」是 2026-10-21 12:00 UTC**（不是写
计划那天的日期）：那两个日期的行（12:00:30 和 11:59:00）是同一天的两个时
刻 —— 一个在将来 30 秒，一个已经过去了。拿别的日子当「现在」，「将来 30 秒」
那一行就变成了「已经过去」。

- [ ] **Step 10: 实现 `waitBeforeRetry`**

`internal/gateway/gateway.go`：

```go
// maxWaitForTheSameAccount is how long the gateway waits for an account
// the upstream has just refused.
//
// 上游说「120 秒后再来」的时候，等是等不到的：客户端等不起，那个号也
// 不该被占着一分钟。超过这个数就不等了 —— 换一个号更快。
const maxWaitForTheSameAccount = 10 * time.Second

// waitBeforeRetry says how long to wait before asking the same account
// again.
//
// 上游说了什么时候再来，它说的算：限流窗口里再去敲门就是白烧上游的次数。
// 没有说的话，用自己的退避。
//
// 上游说的太远的时候：不等，换号（第二个返回值是 false）。
func waitBeforeRetry(resp *http.Response, attempt int) (time.Duration, bool) {
	wait := backoff(attempt)
	if resp == nil {
		return wait, true
	}
	d, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now())
	if !ok {
		return wait, true
	}
	if d > maxWaitForTheSameAccount {
		return 0, false
	}
	return d, true
}

```

- [ ] **Step 11: 用它**

`internal/gateway/gateway.go` —— `case rotator.ActionRetrySame:` 那一段：

```go
				case rotator.ActionRetrySame:
					// 限流、5xx：这个账号等一等，再问它一次。
					lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
					quota = false
					quotaMsg = ""
					wait, waitOK := waitBeforeRetry(resp, try)
					if !waitOK {
						// 上游让等太久：等是等不到的，换一个号。
						// 那个窗口不记成用完 —— 限流不是额度的事。
						lastErr = fmt.Errorf("HTTP %d: the upstream asked to wait longer than %s",
							resp.StatusCode, maxWaitForTheSameAccount)
						return false
					}
					if !sleepCtx(ctx, wait) {
						return true
					}
					continue
```

**网络错那两处不用改**：那一档没有上游的回答（没有响应），所以用的是我们
自己的退避 —— 它现在自带抖动。

- [ ] **Step 12: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
```

- [ ] **Step 13: 自查有没有牙**

- 把 `waitBeforeRetry` 里「上游说的太远就不等」那一句去掉（改成无条件用上游的
  时间）→ 那个「等太久」测试要**红**（请求会等一小时，测试超时失败）；改回来。
- 把 `jitter` 改成原样返回（`return d`）→ `TestTheBackoffIsNotTheSameEveryTime`
  要**红**；改回来。
- 把 `retryAfter` 里的 HTTP 日期那一支去掉（只认秒数）→
  `TestTheRetryAfterIsRead` 要**红**；改回来。

- [ ] **Step 14: 提交**

```bash
git add internal/gateway/gateway.go internal/gateway/retry_test.go
git commit -F .git/commit_msg.txt
```

## 前后两遍

| | 改之前 | 改之后（量出来的） |
|---|---|---|
| 上游给了 `Retry-After: 1` | 0.5 秒就再问 | **1 秒**（上游说的那个数） |
| 上游给了 `Retry-After: 3600` | 0.5 秒就再问，问三次（3.27 秒） | **换号**：0.03 秒，上游只被问一次 |
| 我们自己的退避 | 500ms / 1s / 2s / 4s，每次一样 | 同样上下两成，每次不一样 |
| 上游没给 `Retry-After` | 500ms / 1s / 2s / 4s | 同样上下两成（**唯一一个变了的地方**） |

（改之前的两个数是那两条测试红的时候**量出来的**：上游说了 1 秒，网关 0.42
秒就再问；上游说了 3600 秒，网关问了三次、用了 3.27 秒。）
