# 上游回包的大小：那个数该在哪儿 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 上游回包的大小有一个数说了算（配置里那个键，默认 8 MiB）；聊天的回答
也过那道闸，超了是错，不是半个回答。

**Architecture:** `internal/upstream` 里有那个数（进程范围的一个数，开机从配置
定一次）：所有读上游回包的地方都用它 —— 读额度、模型名单、签到、续签，还有聊天的
回答。网关那三处读（错误页、非流式的回答、流式里的错误）从裸的 `io.ReadAll` 换成
过闸的读；超了是 `*upstream.TooLargeError`，网关当场答 502，不重试（再问同一个
账号也是这么大的回答）。

**Tech Stack:** Go 1.26、标准库（`io`、`sync/atomic`、`net/http`）。

**规格：** `docs/superpowers/specs/2026-11-21-mmxcode-upstream-body-limit-design.md`

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/config/config.go` | 配置里那个键；写错的值报错 |
| `internal/config/config_test.go` | 那个键的默认值和错值 |
| `internal/upstream/client.go` | 那个数（进程范围的一个）；过闸的读 |
| `internal/upstream/errors.go` | 「回包太大了」那个错 |
| `internal/upstream/upstream_test.go` | 闸和那个数的测试 |
| `internal/gateway/gateway.go` | 聊天那三处读过闸；超了答 502 |
| `internal/gateway/gateway_test.go` | 超限的回答被挡住，而且不重试 |
| `cmd/mmxcode/root.go` | 开机的时候定那个数 |
| `README.md`、`config.example.yaml`、`config.docker.yaml` | 那个键的文档 |

---

### Task 1: 配置里加那个键

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**为什么先看规格 §3 的洞三：** 那个数是写死的（`maxBody` 是一个常量）—— 站点
改了它自己的上限，我们改不了，我们要放宽也只能改代码重新编。

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go` —— 挨着 `sqlite` 那几条加：

```go
// TestTheMaxBodyDefaults says the default is 8 MiB.
//
// 站点自己那个输出上限（524288 个 token）折成字节是 2 MB 上下，8 MiB 是它上
// 面的三倍多。
func TestTheMaxBodyDefaults(t *testing.T) {
	c := Default()
	if c.Upstream.MaxBodyMB != 8 {
		t.Errorf("max_body_mb = %d, want 8", c.Upstream.MaxBodyMB)
	}
}

// TestTheMaxBodyRefusesZero says a zero body limit is refused.
//
// 0 会被读成「没有上限」—— 那正是要修的那个洞（聊天的回答从前就完全没有
// 闸），所以宁可在这里报错。
func TestTheMaxBodyRefusesZero(t *testing.T) {
	_, err := loadUpstream(t, "upstream:\n  max_body_mb: 0\n")
	if err == nil {
		t.Fatal("the zero body limit was taken")
	}
	if !strings.Contains(err.Error(), "max_body_mb") {
		t.Fatalf("error %q does not name the key", err)
	}
}

// TestTheMaxBodyRefusesANegative says a negative body limit is refused.
func TestTheMaxBodyRefusesANegative(t *testing.T) {
	_, err := loadUpstream(t, "upstream:\n  max_body_mb: -1\n")
	if err == nil {
		t.Fatal("the negative body limit was taken")
	}
	if !strings.Contains(err.Error(), "max_body_mb") {
		t.Fatalf("error %q does not name the key", err)
	}
}

// TestTheMaxBodyIsTaken says a body limit in MB is taken.
func TestTheMaxBodyIsTaken(t *testing.T) {
	c, err := loadUpstream(t, "upstream:\n  max_body_mb: 16\n")
	if err != nil {
		t.Fatalf("the body limit was refused: %v", err)
	}
	if c.Upstream.MaxBodyMB != 16 {
		t.Fatalf("max_body_mb = %d, want 16", c.Upstream.MaxBodyMB)
	}
}

// loadUpstream reads a configuration that says one thing about the
// upstream.
func loadUpstream(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/config/ -run TestTheMaxBody -v
```

失败：编译不过（`Upstream.MaxBodyMB` 还没有）。

- [ ] **Step 3: 改 `internal/config/config.go`**

`internal/config/config.go` —— `Upstream` 多一个字段：

```go
// Upstream holds the upstream site's settings.
type Upstream struct {
	Timeout Duration `yaml:"timeout" json:"timeout"`
	Site    string   `yaml:"site" json:"site"`
	// MaxBodyMB is the most an upstream response body may be, in MiB.
	//
	// 它是**回包**的大小，不是请求的。回包的大小是上游的事：站点答一个正经
	// 问题的回答最多是它那个输出上限（524288 个 token，约 2 MB），所以 8
	// MiB 有三倍多的余量。
	//
	// 0 和负数不许（那是「没有上限」，正是要修的那个洞）。
	MaxBodyMB int `yaml:"max_body_mb" json:"max_body_mb"`
}

```

`internal/config/config.go` —— 默认值：

```go
		Upstream: Upstream{Timeout: Duration(120 * time.Second), Site: "global", MaxBodyMB: 8},
```

`internal/config/config.go` —— 错值的检查，和 `Load` 里那一句：

```go
// checkUpstream refuses the settings that would mean something else.
//
// 回包能有多大：8 MiB 是站点自己那个输出上限的三倍多。0 和负数不许 —— 「没
// 有上限」不是一个选项：那正是要修的那个洞（聊天的回答从前完全没有闸）。
func (c *Config) checkUpstream() error {
	if c.Upstream.MaxBodyMB < 1 {
		return fmt.Errorf("upstream.max_body_mb is %d: it is 1 or more", c.Upstream.MaxBodyMB)
	}
	return nil
}

```

```go
	if err := c.checkUpstream(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
```

- [ ] **Step 4: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/config/ -count=1
```

- [ ] **Step 5: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 2: 上游包里有那个数

**Files:**
- Modify: `internal/upstream/client.go`
- Modify: `internal/upstream/errors.go`
- Modify: `internal/upstream/upstream_test.go`

**为什么先看规格 §4.2：** 回包的大小是**上游**的事，不是某个调用方的事 ——
读额度、模型名单、签到、续签、聊天回答，读的都是同一个上游。挂在客户端上就得
把它一路传下去（`account.Sender` 那个接口、`oauth` 的续签器、`manager.accFor`
另造的那个客户端……），传一遍就多一处能漏的地方。所以它是进程范围的一个数。

- [ ] **Step 1: 写失败的测试**

`internal/upstream/upstream_test.go` —— 加两条：

```go
// TestTheBodyIsRefusedWhenItIsTooBig says a body over the limit is an
// error, not a truncated body.
//
// 截断的回答是半个回答：客户端拿着它当完整的用，比报错糟得多。
func TestTheBodyIsRefusedWhenItIsTooBig(t *testing.T) {
	body := io.NopCloser(strings.NewReader(strings.Repeat("x", 100)))
	_, err := ReadBodyLimit(body, 10)
	if err == nil {
		t.Fatal("the too big body was taken")
	}
	var tooBig *TooLargeError
	if !errors.As(err, &tooBig) {
		t.Fatalf("the error = %v, want the one that says the body is too big", err)
	}
	if tooBig.Limit != 10 {
		t.Errorf("the limit in the error = %d, want 10", tooBig.Limit)
	}
}

// TestTheLimitIsOneForTheWholeProcess says the limit is one number for
// the whole process, and that a zero does not take it away.
//
// 「没有上限」不是一个选项：那个洞正是这么来的。
func TestTheLimitIsOneForTheWholeProcess(t *testing.T) {
	t.Cleanup(func() { SetMaxBody(DefaultMaxBody) })
	SetMaxBody(1234)
	if MaxBody() != 1234 {
		t.Errorf("the limit = %d, want 1234", MaxBody())
	}
	SetMaxBody(0)
	if MaxBody() != 1234 {
		t.Errorf("the limit = %d after a zero, want 1234", MaxBody())
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/upstream/ -run "TestTheBodyIsRefused|TestTheLimitIsOne" -v
```

失败：编译不过（`ReadBodyLimit`、`TooLargeError`、`SetMaxBody` 都还没有）。

- [ ] **Step 3: 改 `internal/upstream/client.go` —— 那个数和过闸的读**

```go
// DefaultMaxBody is the most an upstream response body may be when the
// configuration does not say otherwise.
//
// 8 MiB：站点自己那个输出上限（524288 个 token）折成字节是 2 MB 上下，它
// 的输入上限（1048576 个 token）是 4 MB 上下 —— 8 MiB 是它上面的一个整数。
const DefaultMaxBody int64 = 8 << 20

// maxBody is the most an upstream response body may be.
//
// 它是整个进程一个数：读上游回包的地方读的都是它 —— 读额度、模型名单、签
// 到、续签、聊天的回答。
//
// 开机的时候从配置里定一次（upstream.max_body_mb），之后只读。读的时候用
// atomic：它是开机定、之后读的东西，可定它的那一行和读它的那一行不在同一
// 时候，撞上了就是数据竞争。
var maxBody atomic.Int64

// SetMaxBody sets the most an upstream response body may be.
//
// 定一次就好（开机的时候）。不是正数就还是原来那个 —— 「没有上限」不是一
// 个选项。
func SetMaxBody(n int64) {
	if n > 0 {
		maxBody.Store(n)
	}
}

// MaxBody is the most an upstream response body may be.
func MaxBody() int64 {
	if n := maxBody.Load(); n > 0 {
		return n
	}
	return DefaultMaxBody
}

```

```go
// ReadBody reads a response's body and closes it.
//
// The body is read through a limit: an upstream that streams without
// end must not be able to take all the memory. A body over the limit
// is an error, not a truncated answer.
func ReadBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("read body: no body")
	}
	defer resp.Body.Close()
	return ReadBodyLimit(resp.Body, MaxBody())
}

// ReadBodyLimit reads a body through a limit.
//
// 读满上限就报错，不是截断：截断的回答是半个回答，客户端拿着它当完整的用，
// 比报错糟得多。
func ReadBodyLimit(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, &TooLargeError{Limit: limit}
	}
	return b, nil
}

```

（`sync/atomic` 要 import。）

- [ ] **Step 4: 改 `internal/upstream/errors.go` —— 那个错**

```go
// TooLargeError says a body was larger than it may be.
//
// 它是「上游发的不是回答」的一种：回包的大小是上游的事，站点答一个正经问
// 题不会超过它自己那个输出上限（524288 个 token，约 2 MB）。所以超了就是
// 有什么不对，不是「这个答案太长」。
type TooLargeError struct {
	// Limit is how many bytes the body may be.
	Limit int64
}

// Error is the error's text.
func (e *TooLargeError) Error() string {
	return fmt.Sprintf("the upstream's answer is larger than %d bytes", e.Limit)
}

```

- [ ] **Step 5: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/upstream/ -count=1
```

- [ ] **Step 6: 提交**

```bash
git add internal/upstream/client.go internal/upstream/errors.go internal/upstream/upstream_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 3: 聊天那条路读回包要过闸

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`

**为什么先看规格 §3 的洞一：** 那个 32 MiB 的闸只管小回包；**聊天的回答根本没
有闸**（`attempt` 里是裸的 `io.ReadAll`）—— 上游发多少，进程就吃多少。三处都
是：错误页、非流式的回答（平常走的那条）、流式里的错误。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/gateway_test.go` —— 加一条：

```go
// TestAnAnswerThatIsTooBigIsRefused says an answer that is bigger than
// the limit is refused, and the upstream is not asked again.
//
// 超限的回答是「上游发的不是回答」：再问同一个账号也是这么大的回答（回答
// 的大小是模型和请求的事，不是账号的事），所以当场就答，不重试。
func TestAnAnswerThatIsTooBigIsRefused(t *testing.T) {
	upstream.SetMaxBody(1024)
	t.Cleanup(func() { upstream.SetMaxBody(upstream.DefaultMaxBody) })
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 4096)))
	})
	body := `{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "larger than") {
		t.Fatalf("body = %s", w.Body.String())
	}
	h.mu.Lock()
	hits := h.hits
	h.mu.Unlock()
	if hits != 1 {
		t.Errorf("the upstream was asked %d times, want 1", hits)
	}
}

```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run TestAnAnswerThatIsTooBig -v
```

失败：没有闸，那个 4096 字节的回答整个被读进来了（答的不是 502，或者说的话里没有
「larger than」）。

- [ ] **Step 3: 改 `internal/gateway/gateway.go` —— 那三处读**

`internal/gateway/gateway.go` —— `attempt` 里：

```go
	if resp.StatusCode != http.StatusOK {
		raw, _ := upstream.ReadBody(resp)
		return resp, raw, nil
	}
	if !wantStream {
		raw, err := upstream.ReadBody(resp)
		if err != nil {
			return resp, nil, err
		}
		return resp, raw, nil
	}
	br := bufio.NewReader(resp.Body)
	head, _ := peekEvent(br)
	if sseHasError(head) {
		// 200 里装着错误：整条读完，当成失败处理
		rest, _ := upstream.ReadBodyLimit(br, upstream.MaxBody())
		resp.Body.Close()
		return resp, append(head, rest...), nil
	}
```

- [ ] **Step 4: 改 `internal/gateway/gateway.go` —— 超限当场答 502**

`internal/gateway/gateway.go` —— 两处 `attempt` 之后的错（第一处和「上游没有这
条路」之后的那一处）：

```go
				resp, raw, err := s.attempt(ctx, chatReq, client.Transport, stream)
				if err != nil {
					var oe *upstream.OtherSiteError
					if errors.As(err, &oe) {
						// 跨站：不换号、不消耗 refresh token
						writeError(w, proto, http.StatusBadGateway, oe.Error())
						return true
					}
					var tooBig *upstream.TooLargeError
					if errors.As(err, &tooBig) {
						// 上游发的不是回答：再问同一个账号也是这么大的回答
						// （回答的大小是模型和请求的事，不是账号的事）。当场
						// 答 502，不重试。
						writeError(w, proto, http.StatusBadGateway, err.Error())
						return true
					}
```

（第二处照同样加。）

- [ ] **Step 5: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/gateway/ -count=1
```

- [ ] **Step 6: 提交**

```bash
git add internal/gateway/gateway.go internal/gateway/gateway_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 4: 开机的时候定那个数，文档跟上

**Files:**
- Modify: `cmd/mmxcode/root.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`
- Modify: `README.md`

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go` —— 挨着 `sqlite` 那条「发出去的配置」加一条：

```go
// TestTheShippedConfigsHaveTheUpstreamDefaults says the configs that
// ship with the program agree with the default.
//
// 发出去的配置跟说好的不一样，照着配置文件跑的人拿到的就不是那个配置。
func TestTheShippedConfigsHaveTheUpstreamDefaults(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "config.docker.yaml"} {
		c, err := Load(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Upstream != Default().Upstream {
			t.Errorf("%s: upstream = %+v, want %+v", name, c.Upstream, Default().Upstream)
		}
	}
}

```

- [ ] **Step 2: 跑测试，看它过了**

```bash
go test ./internal/config/ -run TestTheShippedConfigsHaveTheUpstream -v
```

**过了 —— 这条是「守着」的，不是「驱动的」**：配置文件里没有那个键的时候，读出
来的就是默认值 8，所以两个配置跟默认值本来就一样。那条测试守的是「发出去的配置
跟默认值说的一样」这件事（有人改了默认值却没改发出去的配置，它会红）。

那个键写进配置文件是给人看的（下面 Step 4）。

- [ ] **Step 3: 改 `cmd/mmxcode/root.go` —— 开机定一次**

`cmd/mmxcode/root.go` —— 读完配置之后：

```go
		e.cfg = cfg
		// 回包能有多大：整个进程一个数（读额度、模型名单、签到、续签、聊
		// 天的回答读的都是它）。开机定一次。
		upstream.SetMaxBody(int64(cfg.Upstream.MaxBodyMB) << 20)
```

- [ ] **Step 4: 两个发出去的配置加上那个键**

`config.example.yaml`：

```yaml
upstream:
  timeout: 120s
  site: global
  # 上游回包最多多大（MiB）。站点自己那个回答上限（524288 个 token）折成
  # 字节是 2 MB 上下，8 MiB 是它上面的三倍多。超了就是上游发的不是回答。
  max_body_mb: 8
```

`config.docker.yaml`：

```yaml
upstream:
  timeout: 120s
  site: global
  # 上游回包最多多大（MiB）。站点自己那个回答上限（524288 个 token）折成
  # 字节是 2 MB 上下，8 MiB 是它上面的三倍多。超了就是上游发的不是回答。
  max_body_mb: 8
```

- [ ] **Step 5: README 那一行**

`README.md` —— 配置那张表里 `upstream.site` 后面加一行：

```markdown
| `upstream.max_body_mb` | `8` | 上游回包最多多大（MiB）。站点自己那个回答上限折成字节是 2 MB 上下，8 MiB 是三倍多。超了就是上游发的不是回答，客户端拿到 502 |
```

- [ ] **Step 6: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
```

- [ ] **Step 7: 提交**

```bash
git add cmd/mmxcode/root.go internal/config/config_test.go config.example.yaml config.docker.yaml README.md
git commit -F .git/commit_msg.txt
```

---

### Task 5: 全查一遍

- [ ] **Step 1: 全部的测试**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
```

- [ ] **Step 2: 自查有没有牙**

- 把 `attempt` 里那三处读改回裸的 `io.ReadAll` → 「超限的回答被挡住」那条测试
  要**红**；改回来。
- 把 `SetMaxBody` 里「不是正数就还是原来那个」去掉（`SetMaxBody(0)` 也存）→
  「那个数是整个进程一个」那条测试要**红**；改回来。
- 把 `checkUpstream` 去掉 → 「0 被拒」那条测试要**红**；改回来。

- [ ] **Step 3: 提交**

```bash
git add -A
git commit -F .git/commit_msg.txt
```

## 前后两遍

| | 改之前 | 改之后 |
|---|---|---|
| 聊天的回答 | **没有闸**（`io.ReadAll`，上游发多少吃多少） | 过闸：超了是错（502），不是半个回答 |
| 小回包的闸 | 32 MiB（写死的） | 那个数（默认 8 MiB） |
| 那个数 | 常量，改代码才能改 | 配置里一个键（`upstream.max_body_mb`），写 0 拒绝启动 |
| 超限以后 | —— | 当场 502，不重试（再问一次也是这么大的回答） |
| 流式的回答 | 逐块转，内存有界 | 一样（流没有「总长」） |
