# 身体分两条道 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 大的请求体一次只让一个进来，小的有一个字节上限 —— 在飞的身体加起来
不超过 144 MiB（一个大的加十九个小的）。

**Architecture:** `internal/gateway` 的 `handleChat` 在读身体之前先过两道门：
**大道**（大身体，一次一个，用现成的 `gate` 限 1）和**小道**（小身体，一个
字节池，占自己大小的字节）。两道的门都排在并发闸门**前面** —— 排在后面的话，
20 个大请求会把 20 个并发位全占住（它们在等大道那个位），小请求连闸门都进不
去。分道看的是 `Content-Length`：客户端没说多大（分块传的）算大的。

**Tech Stack:** Go 1.26、标准库（`context`、`sync`、`time`）、`internal/config`。

**规格：** `docs/superpowers/specs/2026-11-21-mmxcode-body-lanes-design.md`

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/gateway/bodies.go` | 小身体的字节池；两个配置怎么读成字节 |
| `internal/gateway/bodies_test.go` | 两条道的测试：大身体一个一个来、小的占字节、OpenAI 的身体算两份 |
| `internal/gateway/gateway.go` | Server 多两个字段；`handleChat` 在读身体之前过那两道门 |
| `internal/config/config.go` | `concurrency.body_small_mb` 和 `concurrency.body_budget_mb` |
| `internal/config/config_test.go` | 发出去的配置跟默认值一致 |
| `config.example.yaml`、`config.docker.yaml`、`README.md` | 两个新键和它们的说法 |

---

### Task 1: 小身体的字节池

**Files:**
- Create: `internal/gateway/bodies.go`
- Create: `internal/gateway/bodies_test.go`

**为什么先看规格 §5：** 小身体走的是普通并发，但它们的总量得有个上限 —— 那
个上限是运营定的，跟有多少账号、承诺多少并发没关系。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/bodies_test.go` —— 整个文件：

```go
package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/config"
	"github.com/lingoo12138/mmxcode/internal/store"
)

// bigBody is a request body that is larger than the small lane takes.
//
// 它是合法的 JSON：网关要读它（模型名、会话），读不出来就是另一种错。
func bigBody(n int) string {
	return `{"model":"m","pad":"` + strings.Repeat("x", n) + `"}`
}

// addAccounts puts more signed-in accounts in the store.
//
// 网关承诺的并发是从「能用的账号有几个」算出来的：一个号就一个并发。
func addAccounts(t *testing.T, h *harness, ids ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	for _, id := range ids {
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
}

// TestTwoBigBodiesDoNotRunAtTheSameTime says a big request body waits for
// the one before it.
//
// 大身体一次只让一个进来：两个一起来那是一百多兆（规格 §5）。
func TestTwoBigBodiesDoNotRunAtTheSameTime(t *testing.T) {
	inTheAir := make(chan struct{}, 2)
	release := make(chan struct{})
	// 放的活儿一定得放：测试要是死在半路上，那个还堵在上游的请求也得
	// 走 —— 不然关假服务器的时候会等它等到天荒地老。
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		inTheAir <- struct{}{}
		<-release
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 三个号才有三个并发位：不然第二个大请求是被并发闸门挡住的，测的
	// 就不是大道了。
	addAccounts(t, h, "acc_02", "acc_03")
	h.gw.concTTL = 0
	big := bigBody(5 << 20)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(big))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-inTheAir:
	case <-time.After(10 * time.Second):
		t.Fatal("the first big request never reached the upstream")
	}

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(big))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-inTheAir:
		t.Fatal("the second big request reached the upstream while the first was in the air")
	case <-time.After(300 * time.Millisecond):
	}

	// 第一个走完了，第二个就该进去了。
	open()
	select {
	case <-inTheAir:
	case <-time.After(10 * time.Second):
		t.Fatal("the second big request never reached the upstream")
	}
}

// TestASmallBodyIsNotHeldBackByABigOne says a small request goes while a
// big one is in the air.
//
// 小的走自己的道：它不占大道的位，也不被大道的那个位挡着（规格 §4）。
func TestASmallBodyIsNotHeldBackByABigOne(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 大的那个在上游那里待着，小的直接答。
		if r.ContentLength > 4<<20 {
			<-release
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 三个号才有两个并发位。
	addAccounts(t, h, "acc_02", "acc_03")
	h.gw.concTTL = 0

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bigBody(5<<20)))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	defer close(release)
	// 那个大请求到上游了才算数。
	deadline := time.Now().Add(10 * time.Second)
	for h.hits == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	hits := h.hits
	h.mu.Unlock()
	if hits == 0 {
		t.Fatal("the big request never reached the upstream")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestABigBodyWaitingForItsLaneDoesNotHoldAConcurrencyPlace says a request
// that waits for the big lane does not take a place of the concurrency
// gate.
//
// 道的门在并发闸门之前：不然 20 个大请求会把 20 个位全占住（它们在等大道
// 那个位），小请求连闸门都进不去 —— 那 19 个小请求就没了（规格 §4）。
func TestABigBodyWaitingForItsLaneDoesNotHoldAConcurrencyPlace(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 大的那个在上游那里待着（道的门要是没挡住它，它就到这儿来了）。
		if r.ContentLength > 4<<20 {
			<-release
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(500 * time.Millisecond)
	defer close(release)
	// 大道的那个位被人占着。
	if !h.gw.bigGate.take(1) {
		t.Fatal("the big lane did not give out its place")
	}
	defer h.gw.bigGate.give()

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bigBody(5<<20)))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(100 * time.Millisecond)

	// 小请求照常走：并发那一个位还空着。
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestABigBodyIsRefusedWhenTheBigLaneIsBusy says a big body that cannot
// get its place gets a 429 that says what is busy.
func TestABigBodyIsRefusedWhenTheBigLaneIsBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	if !h.gw.bigGate.take(1) {
		t.Fatal("the big lane did not give out its place")
	}
	defer h.gw.bigGate.give()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bigBody(5<<20)))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "another big request body is in the air") {
		t.Fatalf("body = %s, want it to say the big lane is busy", w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("the 429 has no Retry-After")
	}
}

// TestABodyWithoutAContentLengthGoesToTheBigLane says a chunked request
// counts as a big one.
//
// 客户端没说多大，那就当它大：宁可慢一点，不能让内存被撑破（规格 §3）。
func TestABodyWithoutAContentLengthGoesToTheBigLane(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	if !h.gw.bigGate.take(1) {
		t.Fatal("the big lane did not give out its place")
	}
	defer h.gw.bigGate.give()

	body := io.NopCloser(strings.NewReader(`{"model":"m"}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	if req.ContentLength >= 0 {
		t.Fatalf("the request says it is %d bytes, want it to say nothing", req.ContentLength)
	}
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "another big request body is in the air") {
		t.Fatalf("body = %s, want it to say the big lane is busy", w.Body.String())
	}
}

// TestTheSmallBodiesAreRefusedWhenThePoolIsFull says a small body that
// does not fit in the pool gets a 429 that says what is busy.
func TestTheSmallBodiesAreRefusedWhenThePoolIsFull(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	h.gw.Cfg.Concurrency.BodyBudgetMB = 1
	// 池子满了。
	if !h.gw.bodies.take(1<<20, 1<<20) {
		t.Fatal("the pool did not take the bytes")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too many request bodies are in the air") {
		t.Fatalf("body = %s, want it to say the bodies are in the air", w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("the 429 has no Retry-After")
	}
}

// TestTheOpenAIBodyCountsTwice says an OpenAI body takes twice the bytes.
//
// OpenAI 和 Codex 的身体要转成 Anthropic 的形状，那是第二份 —— 转换的那一
// 刻两份都在（规格 §6）。
func TestTheOpenAIBodyCountsTwice(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"pad":"` +
		strings.Repeat("x", 300*1024) + `"}`
	if got := heldBytes(t, "/v1/chat/completions", body); got != int64(len(body))*2 {
		t.Fatalf("the pool holds %d bytes, want %d", got, int64(len(body))*2)
	}
}

// TestTheAnthropicBodyCountsOnce says an Anthropic body takes its own
// size: it is sent on as it is, there is no second copy.
func TestTheAnthropicBodyCountsOnce(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"pad":"` +
		strings.Repeat("x", 300*1024) + `"}`
	if got := heldBytes(t, "/v1/messages", body); got != int64(len(body)) {
		t.Fatalf("the pool holds %d bytes, want %d", got, int64(len(body)))
	}
}

// heldBytes says how many bytes the pool holds while a request of that
// body is in the air.
func heldBytes(t *testing.T, path, body string) int64 {
	t.Helper()
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	go func() {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	// 等它到了上游：那时候它的身体还占着池子。
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		hits := h.hits
		h.mu.Unlock()
		if hits > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request never reached the upstream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.gw.bodies.mu.Lock()
	defer h.gw.bodies.mu.Unlock()
	return h.gw.bodies.used
}

// TestThePoolLetsOneBodyInWhenItIsEmpty says a body that is bigger than
// the whole pool still comes in when the pool is empty.
//
// 配置写小了的时候，那个身体也得能进来 —— 不然它永远等不到，池子空着它
// 也进不来（规格 §5）。
func TestThePoolLetsOneBodyInWhenItIsEmpty(t *testing.T) {
	p := &bodyPool{}
	if !p.take(4<<20, 1<<20) {
		t.Fatal("the pool did not let the body in")
	}
	if p.used != 4<<20 {
		t.Fatalf("used = %d, want %d", p.used, 4<<20)
	}
	// 池子不空了，第二个就进不来了。
	if p.take(1, 1<<20) {
		t.Fatal("the pool let a second body in")
	}
	p.give(4 << 20)
	if p.used != 0 {
		t.Fatalf("used = %d, want 0", p.used)
	}
	if !p.take(1, 1<<20) {
		t.Fatal("the pool did not let the body in after the first one left")
	}
}

// TestThePoolWithNoLimit says a limit of 0 is no limit.
func TestThePoolWithNoLimit(t *testing.T) {
	p := &bodyPool{}
	for i := 0; i < 100; i++ {
		if !p.take(1<<20, 0) {
			t.Fatalf("the pool refused the body number %d", i)
		}
	}
}

// TestTheBodyLimitsSay how big a body may be and how many bytes may be in
// the air at once.
func TestTheBodyLimits(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	if got := h.gw.bodySmallBytes(); got != 4<<20 {
		t.Fatalf("the small limit = %d, want %d (4 MiB)", got, 4<<20)
	}
	if got := h.gw.bodyBudgetBytes(); got != 80<<20 {
		t.Fatalf("the budget = %d, want %d (80 MiB)", got, 80<<20)
	}
	// 配 0 的两样：一个按 1 算，一个是不限。
	h.gw.Cfg.Concurrency.BodySmallMB = 0
	h.gw.Cfg.Concurrency.BodyBudgetMB = 0
	if got := h.gw.bodySmallBytes(); got != 1<<20 {
		t.Fatalf("the small limit = %d, want %d (1 MiB)", got, 1<<20)
	}
	if got := h.gw.bodyBudgetBytes(); got != 0 {
		t.Fatalf("the budget = %d, want 0 (no limit)", got)
	}
}

// TestTheBodyLimitsAreConfigured says the two limits have their defaults.
func TestTheBodyLimitsAreConfigured(t *testing.T) {
	c := config.Default()
	if c.Concurrency.BodySmallMB != 4 {
		t.Errorf("body_small_mb = %d, want 4", c.Concurrency.BodySmallMB)
	}
	if c.Concurrency.BodyBudgetMB != 80 {
		t.Errorf("body_budget_mb = %d, want 80", c.Concurrency.BodyBudgetMB)
	}
}

// TestAnEmptyBodyTakesNoRoom says a body of no bytes does not wait for
// room in the pool.
//
// 零字节的身体不占内存 —— 池子满了也放它进来。
func TestAnEmptyBodyTakesNoRoom(t *testing.T) {
	p := &bodyPool{}
	if !p.take(1<<20, 1<<20) {
		t.Fatal("the pool did not take the bytes")
	}
	if !p.take(0, 1<<20) {
		t.Fatal("the pool did not let an empty body in")
	}
}

```

- [ ] **Step 2: 跑一遍，看它失败**

```bash
go test ./internal/gateway/ -run 'TestThePool|TestTheBodyLimits' -v
```

期望：`bodyPool` 没有定义，编译不过。

- [ ] **Step 3: 写实现**

`internal/gateway/bodies.go` —— 整个文件：

```go
package gateway

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// bodyPool counts the bytes of the small request bodies in the air.
//
// 小身体走的是普通并发，但它们的总量有个上限：那个上限是运营定的，跟有多
// 少账号、承诺多少并发没关系（规格 §5）。
//
// 上限是 0 的时候是不限：那时候只剩并发闸门管着（跟 total_max 一个规
// 矩）。
type bodyPool struct {
	mu sync.Mutex
	// used is how many bytes are in the air.
	used int64
}

// take reserves n bytes when there is room under limit.
//
// 空着的时候总是放一个进来：哪怕这个身体比整个池子还大（配置写小了），
// 它也得能进来 —— 不然它永远等不到，池子空着它也进不来。
func (p *bodyPool) take(n, limit int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 零字节的身体不占地方：池子满了也放它进来。
	if n == 0 {
		return true
	}
	if limit > 0 && p.used > 0 && p.used+n > limit {
		return false
	}
	p.used += n
	return true
}

// give gives n bytes back.
func (p *bodyPool) give(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.used -= n
	if p.used < 0 {
		p.used = 0
	}
}

// waitFor reserves n bytes, waiting until there is room or the wait is over.
//
// 等不到就是等不到：客户端自己超时了，网关不该把它吊着。
func (p *bodyPool) waitFor(ctx context.Context, n, limit int64, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if p.take(n, limit) {
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

// bodySmallBytes is how large a request body may be and still count as
// small.
//
// 小的走普通并发，大的（几十兆 —— 图片就是那么大的）一次只让一个进来。
//
// 配 0 的时候按 1 算：那样每个请求都是大身体，网关就成了一条道。
func (s *Server) bodySmallBytes() int64 {
	mb := int64(s.Cfg.Concurrency.BodySmallMB)
	if mb < 1 {
		mb = 1
	}
	return mb << 20
}

// bodyBudgetBytes is how many bytes of small request bodies may be in the
// air at once.
//
// 0 = 不限（跟 total_max 一个规矩）：那时候只剩并发闸门管着。
func (s *Server) bodyBudgetBytes() int64 {
	mb := int64(s.Cfg.Concurrency.BodyBudgetMB)
	if mb < 1 {
		return 0
	}
	return mb << 20
}

// readBody reads the request body.
//
// 客户端说了多大（Content-Length）就照着那个数读，一次读完：ReadAll 是
// 一路翻倍着长大的，16 MiB 的身体它要分配 32 MiB，翻倍的那一刻两份都在
// —— 那 32 MiB 就是峰值，而身体本身只有 16 MiB。
//
// 不知道多大（分块传的）还是用 MaxBytesReader 挡着：那是唯一一种能超过
// Content-Length 的。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.ContentLength < 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		return io.ReadAll(r.Body)
	}
	// 客户端说了多大就是多大：服务器照着那个数读，多出来的没有。
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

```

- [ ] **Step 4: 跑一遍，看它过**

```bash
go test ./internal/gateway/ -v
```

期望：全过。

- [ ] **Step 5: 提交**

```bash
git add internal/gateway/bodies.go internal/gateway/bodies_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 2: 配置的两个键

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**为什么先看规格 §8：** 两个数是运营定的 —— 多大的身体算大的，小身体同时在飞
多少字节。

- [ ] **Step 1: 写失败的测试**

`internal/config/config_test.go` —— 加一条：

```go
// TestTheShippedConfigsHaveTheBodyLimits says the configs that ship with
// the program agree with the default on the two body limits.
//
// 发出去的配置跟说好的不一样，照着配置文件跑的人拿到的就不是那个配置。
func TestTheShippedConfigsHaveTheBodyLimits(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "config.docker.yaml"} {
		c, err := Load(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Concurrency.BodySmallMB != Default().Concurrency.BodySmallMB {
			t.Errorf("%s: body_small_mb = %d, want %d",
				name, c.Concurrency.BodySmallMB, Default().Concurrency.BodySmallMB)
		}
		if c.Concurrency.BodyBudgetMB != Default().Concurrency.BodyBudgetMB {
			t.Errorf("%s: body_budget_mb = %d, want %d",
				name, c.Concurrency.BodyBudgetMB, Default().Concurrency.BodyBudgetMB)
		}
	}
}
```

- [ ] **Step 2: 跑一遍，看它失败**

```bash
go test ./internal/config/ -run 'TestTheBodyLimits' -v
```

期望：`body_small_mb` 没有这个字段，编译不过。

- [ ] **Step 3: 写实现**

`internal/config/config.go` —— `Concurrency` 后面加两个键：

```go
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
	// Wait is how long a request waits for a free place.
	Wait Duration `yaml:"wait" json:"wait"`
	// BodySmallMB is how large a request body may be and still count as
	// small.
	//
	// 小的走普通并发，大的（几十兆 —— 图片就是那么大的）一次只让一个进
	// 来。两个大的一起来那是一百多兆，三个就更多了。
	//
	// 配 0 的时候按 1 算：那样每个请求都是大身体，网关就成了一条道。
	BodySmallMB int `yaml:"body_small_mb" json:"body_small_mb"`
	// BodyBudgetMB is how many bytes of small request bodies may be in
	// the air at once.
	//
	// 它是内存的天花板，跟有多少账号、承诺多少并发没关系：80 MiB 装得
	// 下 19 个 4 MiB 的小请求。
	//
	// 0 = 不限（跟 total_max 一个规矩）：那时候只剩并发闸门管着。
	BodyBudgetMB int `yaml:"body_budget_mb" json:"body_budget_mb"`
}
```

`Default()` 里的并发那一段：

```go
func Default() Config {
	return Config{
		Server:   Server{Listen: ":8080"},
		Proxy:    Proxy{Default: "", Groups: map[string]string{}},
		Upstream: Upstream{Timeout: Duration(120 * time.Second), Site: "global", MaxBodyMB: 8},
		Rotation: Rotation{Strategy: "sticky", MaxAttempts: 3, ExhaustedCooldown: Duration(5 * time.Minute)},
		Schedule: Schedule{
			CheckinAt:     "00:00",
			CheckinJitter: OptionalDuration(0),
			UsageRefresh:  Duration(15 * time.Minute),
			TokenRefresh:  Duration(5 * time.Minute),
			EgressHealth:  Duration(2 * time.Minute),
		},
		Batch: Batch{
			Interval: Duration(3 * time.Second),
			Relogin: Relogin{
				Cooldown:    Duration(time.Hour),
				MaxAccounts: 20,
			},
		},
		Usage: Usage{Refresh: Refresh{
			HotWindow:   Duration(5 * time.Minute),
			ScanEvery:   OptionalDuration(30 * time.Second),
			Concurrency: 4,
			Timeout:     Duration(15 * time.Second),
			Budget:      Duration(30 * time.Second),
			Batch:       6,
			Card:        2,
			CardBudget:  Duration(5 * time.Second),
		}},
		Concurrency: Concurrency{
			PerAccount:   1,
			TotalRatio:   5,
			TotalMin:     3,
			TotalMax:     0,
			Wait:         Duration(10 * time.Second),
			BodySmallMB:  4,
			BodyBudgetMB: 80,
		},
		Pool: Pool{
			Factor:         2,
			MaxSize:        0,
			MemoryBudgetMB: 32,
			SessionWindow:  Duration(time.Hour),
			ReadOnAdmit:    true,
		},
		SQLite: SQLite{
			CacheMB:              8,
			Synchronous:          "normal",
			CheckinRetentionDays: 30,
		},
		DB: "/data/mmxcode.db",
	}
}
```

- [ ] **Step 4: 跑一遍，看它过**

```bash
go test ./internal/config/ -v
```

期望：全过。

- [ ] **Step 5: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 3: 两道门

**Files:**
- Modify: `internal/gateway/gateway.go`

**为什么先看规格 §4：** 道的门排在并发闸门前面 —— 不然 20 个大请求会把 20
个并发位全占住，小请求连闸门都进不去。

- [ ] **Step 1: Server 多两个字段**

`internal/gateway/gateway.go`：

```go
// Server is the gateway.
type Server struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store
	// Rot picks the account.
	Rot *rotator.Rotator
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
	// Reader reads the credits of an account. It is a field so that a
	// test can stand in for the network: the gateway's only way to the
	// network for the credits goes through it.
	Reader usage.Reader
	// UpstreamBase is the host the chats go to. It is a field so a
	// test can point it at a stand-in.
	UpstreamBase string
	// NativeResponses says the upstream speaks the Responses protocol
	// itself. It is tried and turned off when the upstream says no.
	//
	// 每个请求都读它，而第一个 404 会把它写掉：一个字段被一个请求写、
	// 被别的请求读，就是数据竞争。所以它是 atomic 的。
	NativeResponses atomic.Bool
	// NativeChatCompletions says the upstream speaks the OpenAI chat
	// completions protocol itself. It is tried and turned off when the
	// upstream says no.
	//
	// 参考实现只走 Anthropic 的 /messages，这条路是照着同目录下的
	// /messages 推的：站点也许有，也许没有。它和 NativeResponses 是
	// 同一根杆，同一个理由才用 atomic。
	NativeChatCompletions atomic.Bool
	// WebUI serves the management pages, when there is one.
	WebUI http.Handler
	// modelsMu guards the model list that was read.
	modelsMu sync.Mutex
	// models is the model list, the site's own or the built-in one.
	models []site.Model
	// modelsAt is when it was read.
	modelsAt time.Time
	// modelsLive says the list is the site's own, and not the built-in
	// one that is served when the site cannot be reached.
	modelsLive bool

	// busy counts the requests in the air per account.
	busy *busyAccounts
	// gate counts the requests in the air in the whole gateway.
	gate *gate
	// bigGate lets one big request body at a time through.
	//
	// 大身体是几十兆的内存。两个一起来，那是一百多兆；三个就更多了。
	// 所以它们排着队来，一个一个的（规格 §5）。
	bigGate *gate
	// bodies counts the bytes of the small request bodies in the air.
	bodies *bodyPool
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

	// concMu guards the cached total concurrency.
	concMu sync.Mutex
	// concLimit is the total concurrency that was read last.
	concLimit int
	// concAt is when it was read.
	concAt time.Time
	// concTTL is how long the cached value is kept. A test sets it to
	// zero to turn the cache off.
	concTTL time.Duration
	// fallback is the database's answer, cached, for the time the pool
	// is empty.
	//
	// 池空的时候要从库里拿账号、标记和余额 —— 那三样都是全表读（规格
	// §6.9）。缓存一秒，让那一波并发只读一次。
	fallback *dbFallback
}
```

`New()` 里把它们建出来：

```go
func New(mgr *manager.Manager, cfg config.Config, s *store.Store, webui http.Handler) *Server {
	// 多久算热是配置说了算：管理器记着谁刚被用过，这个窗口是它的。
	mgr.HotWindow = cfg.Usage.Refresh.HotWindow.Duration()
	g := &Server{
		Mgr:          mgr,
		Cfg:          cfg,
		Store:        s,
		Reader:       mgr,
		UpstreamBase: site.LLMHost,
		WebUI:        webui,
	}
	// 原生路径先开着：上游说它没有 /responses 的时候再关。
	g.NativeResponses.Store(true)
	g.NativeChatCompletions.Store(true)
	g.busy = newBusyAccounts(cfg.Concurrency.PerAccount)
	g.gate = &gate{}
	// 两条道：大的一个一个来，小的有一个字节池（规格 §5）。
	g.bigGate = &gate{}
	g.bodies = &bodyPool{}
	// 池空的时候的那份兜底：库里的答案留一秒（规格 §6.9）。
	g.fallback = newDBFallback(s, concurrencyCacheTTL)
	g.lifeCtx, g.lifeCancel = context.WithCancel(context.Background())
	// 后台的读有上限：到顶了就跳过那一次，不排队。
	limit := cfg.Usage.Refresh.Concurrency
	if limit <= 0 {
		limit = 1
	}
	g.bgSem = make(chan struct{}, limit)
	g.concTTL = concurrencyCacheTTL
	// 池：选号从它读，不再每次扫全表（规格 §6.2）。
	g.Pool = pool.New(0)
	g.touches = newSessionTouches()
	g.Keeper = pool.NewKeeper(g.Pool, poolSource{s: s, window: cfg.Pool.SessionWindow.Duration()}, mgr, s,
		keeperOptions(cfg))
	g.Rot = rotator.New(poolAdapter{s: s, busy: g.busy, pool: g.Pool, fallback: g.fallback}, rotator.Options{
		MaxAttempts: cfg.Rotation.MaxAttempts,
		Cooldown:    cfg.Rotation.ExhaustedCooldown.Duration(),
		WindowGroup: manager.WindowGroup,
		Strategy:    cfg.Rotation.Strategy,
	})
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
	return g
}
```

- [ ] **Step 2: `handleChat` 在读身体之前过那两道门**

`internal/gateway/gateway.go`：

```go
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, proto protocol) {
	// 客户端说了身体有多大（Content-Length）的话，超限的那个请求连读都
	// 不用读就拒了：读进来是 64 MiB 的内存，而它马上就是一个 413。
	//
	// 客户端没说的（分块传的）就不知道有多大 —— 那就还是读，MaxBytesReader
	// 照旧挡着。
	if r.ContentLength > maxBodyBytes {
		writeError(w, proto, http.StatusRequestEntityTooLarge, bodyTooBig(maxBodyBytes, r.ContentLength))
		return
	}
	ctx := r.Context()
	// 大身体和小身体走两条道（规格 §4）。
	//
	// 大的（几十兆 —— 图片就是那么大的）一次只让一个进来：两个一起来就
	// 是一百多兆。小的是普通并发，但它们的总量有个上限：那个上限是运营
	// 定的，跟有多少账号、承诺多少并发没关系。
	//
	// 分道看的是 Content-Length：客户端说了多大就是多大。分块传的（不
	// 知道多大）按大的算 —— 宁可慢一点，不能让内存被撑破。
	//
	// 道的门在并发闸门**之前**：并发那 20 个位是共享的，排在它后面的
	// 话，20 个大请求会把 20 个位全占住（它们在等大道那个位），小请求
	// 连闸门都进不去 —— 那 19 个小请求就没了。
	if r.ContentLength < 0 || r.ContentLength > s.bodySmallBytes() {
		if !s.bigGate.waitFor(ctx, 1, s.Cfg.Concurrency.Wait.Duration()) {
			w.Header().Set("Retry-After", "5")
			writeError(w, proto, http.StatusTooManyRequests,
				"the gateway is busy: another big request body is in the air")
			return
		}
		defer s.bigGate.give()
	} else {
		// 小身体占的字节：OpenAI 和 Codex 的身体要转成 Anthropic 的形状，
		// 那是第二份 —— 转换的那一刻两份都在。
		n := r.ContentLength
		if proto != protocolAnthropic {
			n *= 2
		}
		if !s.bodies.waitFor(ctx, n, s.bodyBudgetBytes(), s.Cfg.Concurrency.Wait.Duration()) {
			w.Header().Set("Retry-After", "5")
			writeError(w, proto, http.StatusTooManyRequests,
				"the gateway is busy: too many request bodies are in the air")
			return
		}
		defer s.bodies.give(n)
	}
```

- [ ] **Step 3: 跑一遍**

```bash
go build ./... && go test ./internal/gateway/ -v
```

期望：全过。

- [ ] **Step 4: 提交**

```bash
git add internal/gateway/gateway.go
git commit -F .git/commit_msg.txt
```

---

### Task 4: 文档

**Files:**
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`
- Modify: `README.md`

- [ ] **Step 1: 两个配置文件加上那两个键**

`config.example.yaml` 和 `config.docker.yaml` 的 `concurrency:` 那一段后面：

```yaml
  # 多大的请求体算大的：超过它一次只让一个进来（图片就那么大的）
  body_small_mb: 4
  # 小请求体同时在飞的字节上限（80 MiB ≈ 19 × 4 MiB）。0 = 不限
  body_budget_mb: 80
```

- [ ] **Step 2: README 写上那两条道**

`README.md` 的并发那一段后面：

```markdown
**身体分两条道。** 请求体是网关内存里最大的一块，而且是客户端说了算的：

- **大道**：身体超过 `concurrency.body_small_mb`（默认 4 MiB）算大的 —— 一次只让**一个**进来。图片就是那么大的，两个一起来那是一百多兆。分块传的（客户端没说多大）也算大的。
- **小道**：小的走普通并发，但它们的**总量**有个上限：`concurrency.body_budget_mb`（默认 80 MiB，约 19 × 4 MiB）。0 = 不限（那时候只剩并发闸门管着）。

加起来 144 MiB 上下 —— 一个大的加十九个小的。OpenAI 和 Codex 的身体要转成 Anthropic 的形状，那是第二份拷贝，那一路是 208 MiB。

大道的门排在并发闸门**前面**
```

- [ ] **Step 3: README 写上 nginx 挡在前面的时候那个上限**

`README.md` 的部署那一段后面：

```markdown
### 前面挡着 nginx 的话

网关自己收请求体，收 64 MiB（用户传图片就那么大的）。**nginx 挡在前面的话，它自己也有个上限**：默认 `client_max_body_size` 是 **1m**，一个 4 MB 的对话（1M token 的上下文就 4 MB 上下）会被 nginx 自己挡下来，回一个 413 —— 那个 413 不是网关回的。

```nginx
server {
    client_max_body_size 64m;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering on;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

- `client_max_body_size 64m`：跟网关自己的上限对齐。nginx 收整个身体，收齐了才转给网关 —— 所以请求体在 nginx 那里也占一份内存（`proxy_request_buffering on` 是默认的，别关：关了的话网关那边会变成流式的慢上传）。
- `proxy_read_timeout` / `proxy_send_timeout`：流式的对话要传很久，默认 60s 不够。
```

- [ ] **Step 4: 跑一遍**

```bash
go test ./... && python -I docs/superpowers/tools/check_plan_fences.py docs/superpowers/plans/2026-11-21-mmxcode-body-lanes.md
```

期望：测试全过，`0 problem(s)`。

---

## 前后两遍

| | 之前 | 之后 |
|---|---|---|
| 大的请求体 | 20 个可以一起来 —— 20 × 64 MiB | 一个一个来（大道），≤ 64 MiB |
| 小的请求体 | 20 个 × 64 MiB（上限是 64 MiB，没有更细的账） | 80 MiB 的字节池（`concurrency.body_budget_mb`） |
| 在飞的身体 | 最多 20 × 64 MiB = 1.28 GB | 144 MiB（OpenAI/Codex 那条路 208 MiB） |
| 大身体和小身体 | 不分 | 分道：大的排队，小的不受拖累 |
| 拿不到位置的时候 | 429（只有一种说法） | 429，三种说法：大身体、小身体、并发数 |
