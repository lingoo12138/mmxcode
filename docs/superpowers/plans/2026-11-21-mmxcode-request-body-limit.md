# 请求体：闸门在读身体之前，上限 64 MiB —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 排队的时候网关不替客户端占着身体（闸门挪到读请求体之前），请求体的
上限从 32 MiB 变成 64 MiB（用户可能传图片），往上游发的时候身体不再被抄一遍。

**Architecture:** `internal/gateway` 的 `handleChat` 先占并发名额再读身体 —— 闸门是
网关对外面承诺的并发，在这个数里面，请求才占着网关的内存；排队的时候身体还在客
户端那边。`maxBodyBytes` 从 32 MiB 变成 64 MiB。`internal/upstream` 往上游发身体的
时候用 `bytes.NewReader(req.Body)`，不把身体再抄一遍。

**Tech Stack:** Go 1.26、标准库（`bytes`、`io`、`net/http`）。

**规格：** `docs/superpowers/specs/2026-11-21-mmxcode-request-body-limit-design.md`

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/gateway/gateway.go` | 闸门挪到读身体之前；上限 64 MiB；超限的请求连读都不用读；413 把那个数报出来 |
| `internal/gateway/failures_test.go` | 排队的时候身体没被读；上限是 64 MiB；超限被拒；超限的请求一个字节都没读；413 说的是那个数 |
| `internal/upstream/chat.go` | 往上游发的时候不抄身体 |
| `internal/upstream/chat_test.go` | 身体原样到了上游；那个抄没抄的基准 |
| `docs/superpowers/plans/2026-11-13-mmxcode-gateway.md` | 那份老计划里的那个数跟着改 |

---

### Task 1: 排队的时候身体没有被读进来

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/failures_test.go`

**为什么先看规格 §3 的洞一：** 闸门是网关对外面承诺的并发。请求体最大 64 MiB ——
排在一百个请求后面的每一个都已经把身体读进内存了的话，那是 6.4 GB，跟承诺的并
发一点关系都没有。量过的：100 个 4 MB 的请求，闸门 20，先读后排队是 886 MB 的峰
值常驻，先排队后读是 267 MB。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/failures_test.go` —— 加一条（`io` 和 `config` 要 import）：

```go
// TestABodyIsNotReadWhileTheGatewayIsBusy says a request that is
// waiting for a free slot has not read its body yet.
//
// 闸门是网关对外面承诺的并发：在这个数里面，请求才占着网关的内存。请求
// 体最大 64 MiB —— 排在一百个请求后面的每一个都已经把身体读进内存了的
// 话，那是 6.4 GB，跟承诺的并发一点关系都没有。排队的时候身体还在客
// 户端那边，网关没有替它占内存。
func TestABodyIsNotReadWhileTheGatewayIsBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 等不了多久：这道测试要的是「身体没被读」，不是「等到了」。
	h.gw.Cfg.Concurrency.Wait = config.Duration(200 * time.Millisecond)
	// 把闸门占满：承诺的那个名额没有了。
	limit := h.gw.concurrencyLimit(context.Background())
	for i := 0; i < limit; i++ {
		if !h.gw.gate.take(limit) {
			t.Fatalf("the gate gave out only %d of %d slots", i, limit)
		}
	}
	body := &countedBody{Reader: strings.NewReader(`{"model":"m"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429, body = %s", w.Code, w.Body.String())
	}
	if body.read != 0 {
		t.Fatalf("the body was read %d bytes while the gateway was busy, want 0", body.read)
	}
}

// countedBody counts how many bytes were read out of it.
//
// 它是那道测试的眼睛：身体被读了多少，看得见。
type countedBody struct {
	*strings.Reader
	read int
}

func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.read += n
	return n, err
}

func (c *countedBody) Close() error { return nil }
```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run TestABodyIsNotReadWhileTheGatewayIsBusy -v
```

失败：身体被读过了（`the body was read 16 bytes while the gateway was busy, want 0`）
—— 现在是先把身体读完，再去排队。

- [ ] **Step 3: 改 `internal/gateway/gateway.go` —— 闸门挪到读身体之前**

`internal/gateway/gateway.go` —— `handleChat` 开头：

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
	// 总并发：可用账号的五分之一，小规模至少三个。超了的请求等，等
	// 不到就 429 带 Retry-After —— 客户端自己超时了，网关不该把它吊
	// 着。
	//
	// 闸门在读请求体**之前**：闸门是网关对外面承诺的并发，在这个数里
	// 面，请求才占着网关的内存。请求体最大 64 MiB —— 排在一百个请求
	// 后面的那每一个都已经把身体读进内存了的话，那是 6.4 GB，跟承诺的
	// 并发一点关系都没有。排队的时候身体还在客户端那边，网关没有替它
	// 占内存。
	if !s.gate.waitFor(ctx, s.concurrencyLimit(ctx), s.Cfg.Concurrency.Wait.Duration()) {
		w.Header().Set("Retry-After", "5")
		writeError(w, proto, http.StatusTooManyRequests, "the gateway is busy: too many requests are in the air")
		return
	}
	defer s.gate.give()
	// 请求体有个上限：不然一个客户端发来一大块东西，网关就把它整个
	// 读进内存里，然后就没有内存了。
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, proto, http.StatusRequestEntityTooLarge, bodyTooBig(tooBig.Limit, -1))
			return
		}
		writeError(w, proto, http.StatusBadRequest, "the request body could not be read")
		return
	}
	// 上游请求的时限就是配的那一个。流式对话也吃这份预算：想要更长的
	// 就把 upstream.timeout 调大。
	if d := s.Cfg.Upstream.Timeout.Duration(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
```

（`bodyTooBig` 是 Task 3 里那个。）

（`ctx := r.Context()` 从原来「上游请求的时限」那段挪到最前面 —— 它现在先是闸门的
上下文，再是上游的。）

- [ ] **Step 4: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/gateway/ -count=1
```

- [ ] **Step 5: 提交**

```bash
git add internal/gateway/gateway.go internal/gateway/failures_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 2: 上限 64 MiB

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/failures_test.go`

**为什么先看规格 §3 的洞二：** 图片是 base64 写在身体里的（胀三分之一），一张图
几 MB 是常态，用户传几张就上三十兆。32 MiB 那道闸会把正经的请求挡掉。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/failures_test.go` —— 加一条：

```go
// TestTheRequestBodyLimitIs64MiB says how large a request body may be.
//
// 64 MiB：用户是可能传图片的 —— 站点的模型收 text、image、video（参考
// 插件的模型名单就是这么写的），而图片是 base64 写在身体里的，一张图几
// MB 是常态。而 1M token 的对话是 4 MB 上下 —— 64 MiB 是它的十几倍。
func TestTheRequestBodyLimitIs64MiB(t *testing.T) {
	if maxBodyBytes != 64<<20 {
		t.Fatalf("maxBodyBytes = %d, want %d (64 MiB)", maxBodyBytes, 64<<20)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run TestTheRequestBodyLimitIs64MiB -v
```

失败：`maxBodyBytes = 33554432, want 67108864 (64 MiB)`。

- [ ] **Step 3: 改 `internal/gateway/gateway.go` —— 那个数**

`internal/gateway/gateway.go`：

```go
// maxBodyBytes is how large a request body may be.
//
// 64 MiB：用户是可能传图片的 —— 站点的模型收 text、image、video（参考插件
// 的模型名单就是这么写的），而图片是 base64 写在身体里的，一张图几 MB 是常
// 态，用户传几张就上三十兆。
//
// 1M token 的对话是 4 MB 上下（带上 JSON 外壳 4.5–5 MB），64 MiB 是它的十几
// 倍，够宽裕。
//
// 再大的请求不是对话，是要把它这个网关的内存吃光的东西：没有上限的时候，一
// 个请求就能让网关没有内存可用。
const maxBodyBytes = 64 << 20
```

- [ ] **Step 4: 改 `internal/gateway/failures_test.go` —— 超限那条测试别自己拿着 64 MiB**

从前那条测试用 `strings.Repeat` 造 32 MiB；现在是 64 MiB，那一条测试自己就拿着
64 MiB，再加读身体时的那一份 —— 测试自己比它测的东西还胖。换个不存身体的：

```go
// TestABodyOverTheLimitIsRefused says a body that is too big is turned
// away instead of being read into memory.
//
// 没有上限的时候，一个请求就能把它这个网关的内存吃光：这里读的是整
// 个身子，读进来就一直拿着。
func TestABodyOverTheLimitIsRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	body := &manyBytes{left: maxBodyBytes + 1}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413, body = %s", w.Code, w.Body.String())
	}
	if h.hits != 0 {
		t.Fatalf("the upstream was asked %d times, want 0", h.hits)
	}
}

// manyBytes is a body of n bytes that does not hold them: it makes them
// as they are read.
//
// 64 MiB 的身体用 strings.Repeat 造的话，那条测试自己就拿着 64 MiB，再加
// 读身体时的那一份 —— 测试自己比它测的东西还胖。这个不占那份内存。
type manyBytes struct {
	left int
}

func (m *manyBytes) Read(p []byte) (int, error) {
	if m.left <= 0 {
		return 0, io.EOF
	}
	if len(p) > m.left {
		p = p[:m.left]
	}
	for i := range p {
		p[i] = 'x'
	}
	m.left -= len(p)
	return len(p), nil
}

func (m *manyBytes) Close() error { return nil }
```

- [ ] **Step 5: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/gateway/ -count=1
```

- [ ] **Step 6: 提交**

```bash
git add internal/gateway/gateway.go internal/gateway/failures_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 3: 超限的请求连读都不用读，413 说的是那个数

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/failures_test.go`

**为什么先看规格 §3 的洞二和 §6 的第 2 条：** 客户端在请求头里说了身体有多大
（`Content-Length`）。知道了就不用读：读进来是 64 MiB 的内存，而它马上就是一个
413。分块传的客户端没说 —— 那种还是读，`MaxBytesReader` 照旧挡着。

**413 得说那个数**：传图片的人被挡下来的时候，得知道那个上限是多少、自己超了多
少，才知道该少传一张还是该改配置。一句「太大」是什么也没说。回包那边那道闸早
就这么说了（「the upstream's answer is larger than %d bytes」）。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/failures_test.go` —— 加两条（`fmt` 要 import）：

```go
// TestABodyOverTheLimitIsRefusedWithoutBeingRead says a body that
// declares itself too big is turned away without being read.
//
// 客户端说了身体有多大（Content-Length）：知道了就不用读 —— 读进来是 64
// MiB 的内存，而它马上就是一个 413。
func TestABodyOverTheLimitIsRefusedWithoutBeingRead(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	body := &countedBody{Reader: strings.NewReader(`{"model":"m"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.ContentLength = maxBodyBytes + 1
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413, body = %s", w.Code, w.Body.String())
	}
	if body.read != 0 {
		t.Fatalf("the body was read %d bytes, want 0", body.read)
	}
	if h.hits != 0 {
		t.Fatalf("the upstream was asked %d times, want 0", h.hits)
	}
}

// TestTheTooBigMessageSaysTheLimit says the client is told how big the
// limit is, and how big it sent.
//
// 传图片的人被挡下来的时候，得知道那个上限是多少、自己超了多少，才知道
// 该少传一张还是该改配置。一句「太大」是什么也没说。
func TestTheTooBigMessageSaysTheLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	body := &countedBody{Reader: strings.NewReader(`{"model":"m"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.ContentLength = maxBodyBytes + 1
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413, body = %s", w.Code, w.Body.String())
	}
	msg := w.Body.String()
	if !strings.Contains(msg, fmt.Sprintf("%d", maxBodyBytes)) {
		t.Errorf("the message does not say the limit (%d): %s", maxBodyBytes, msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("%d", maxBodyBytes+1)) {
		t.Errorf("the message does not say how big the body was: %s", msg)
	}
}
```

- [ ] **Step 2: 跑测试，看它失败**

```bash
go test ./internal/gateway/ -run "TestABodyOverTheLimitIsRefusedWithoutBeingRead|TestTheTooBigMessageSaysTheLimit" -v
```

失败：答的是 200 —— 没有那道早退，身体读进去了（那 13 个字节是假身体的大小），
上游被问了一回。

- [ ] **Step 3: 改 `internal/gateway/gateway.go` —— 那个消息**

```go
// bodyTooBig says the request body is larger than the gateway takes.
//
// 数要报出来：传图片的人被挡下来的时候，得知道那个上限是多少、自己超了
// 多少，才知道该少传一张还是该改配置。got 是负数的时候是不知道有多大
// （客户端没说是多大的）。
func bodyTooBig(limit, got int64) string {
	if got >= 0 {
		return fmt.Sprintf("the request body is %d bytes; the gateway takes at most %d bytes (%d MiB)",
			got, limit, limit>>20)
	}
	return fmt.Sprintf("the request body is larger than the %d bytes (%d MiB) the gateway takes",
		limit, limit>>20)
}
```

`handleChat` 里那两处 `writeError` 换成它：

```go
	if r.ContentLength > maxBodyBytes {
		writeError(w, proto, http.StatusRequestEntityTooLarge, bodyTooBig(maxBodyBytes, r.ContentLength))
		return
	}
```

```go
			writeError(w, proto, http.StatusRequestEntityTooLarge, bodyTooBig(tooBig.Limit, -1))
```

- [ ] **Step 4: 跑测试，看它过了**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./internal/gateway/ -count=1
```

- [ ] **Step 5: 提交**

```bash
git add internal/gateway/gateway.go internal/gateway/failures_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 4: 往上游发的时候不抄那一遍

**Files:**
- Modify: `internal/upstream/chat.go`
- Modify: `internal/upstream/chat_test.go`

**为什么先看规格 §3 的洞三：** `strings.NewReader(string(req.Body))` 把整个身体再
抄一份。64 MiB 的身体在飞的时候，内存里躺着两份。量过的（100 个 4 MB 的请求）：
一个 4 MB 的身体走一遍 `Chat`：每次的分配从 4,202,038 B 变成 4,794 B —— 少
的正好是一份身体。

- [ ] **Step 1: 写测试**

这一步改的是**内存**，不是行为 —— 没有能红的测试。所以配两条：一条守住「身体原
样到了上游」（防的是改坏了），一条量「抄没抄」。

`internal/upstream/chat_test.go` —— 加两条（`bytes` 要 import）：

```go
// TestChatSendsTheBodyAsItIs says the body arrives at the upstream byte
// for byte, whatever is in it.
//
// 往上游发的时候身体是不该被抄一遍的（64 MiB 的身体抄一遍就是多 64 MiB
// 的内存）。抄的那份跟原来那份在行为上是一模一样的 —— 所以这里守的不是
// 「不抄」，守的是「抄了也别把身体改坏」。
func TestChatSendsTheBodyAsItIs(t *testing.T) {
	want := []byte{0x00, 0xff, 0xfe, '"', '\\', 0x00, 'a'}
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Chat(context.Background(), ChatRequest{
		BaseURL: srv.URL, Path: "/p", Access: "a", Body: want,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("the body = %v, want %v", got, want)
	}
}

// discardTransport answers every request without touching the network.
//
// 它是那个基准的地面：身体走的是它，不是真的网 —— 基准量的是网关自己
// 那一份，不是网络快慢。
type discardTransport struct{}

func (discardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	io.Copy(io.Discard, r.Body)
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

// BenchmarkChatSendsABigBody says what a big body costs on the way to
// the upstream.
//
// 身体是 4 MB（1M token 的对话）：往上游发的时候身体不该被抄一遍 ——
// 抄一遍就是多一份 4 MB 的分配，也是多 4 MB 的内存（在飞的时候）。
func BenchmarkChatSendsABigBody(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 4<<20)
	c := New(Options{Timeout: 30 * time.Second})
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, err := c.Chat(context.Background(), ChatRequest{
			BaseURL: "http://upstream.invalid", Path: "/p", Access: "a", Body: body,
		}, discardTransport{})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}
```

- [ ] **Step 2: 跑基准，先记下改之前的数**

```bash
go test ./internal/upstream/ -run XXX -bench BenchmarkChatSendsABigBody -benchmem -count=1
```

记下 `B/op` 和 `allocs/op`。**改之前**身体是被抄一遍的：

```
BenchmarkChatSendsABigBody-8   1236   811363 ns/op   4202471 B/op   45 allocs/op
```

`B/op` 里有一份 4 MB —— 那就是身体本身的大小。

- [ ] **Step 3: 改 `internal/upstream/chat.go`**

`internal/upstream/chat.go` —— import 加上 `bytes`（放在 `context` 前面），然后：

```go
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(req.BaseURL, "/")+req.Path, bytes.NewReader(req.Body))
```

（`bytes.NewReader` 读的是原来那块 `[]byte`，不抄。`req.Body` 在 `Chat` 里面不会被
改，所以没有别名上的问题。）

- [ ] **Step 4: 跑基准，再记下改之后的数**

```bash
go test ./internal/upstream/ -run XXX -bench BenchmarkChatSendsABigBody -benchmem -count=1
```

`B/op` 里那份 4 MB 没有了：

```
BenchmarkChatSendsABigBody-8  228183     4703 ns/op      4794 B/op   44 allocs/op
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
git add internal/upstream/chat.go internal/upstream/chat_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 5: 那份老计划里那个数跟着改

**Files:**
- Modify: `docs/superpowers/plans/2026-11-13-mmxcode-gateway.md`

**为什么：** 那份计划里写着 `const maxBodyBytes = 32 << 20` —— 那正是这次改掉的数
（规格 §3 的洞二）。计划里的 Go 块是要跟 shipped 的代码对得上的。

**只改这一段。** 那份计划里的 `handleChat` 早就不跟 shipped 的代码一样了（那是别
的改动留下的：会话那一行合成一次读、`upstreamID` 跟着那一行走），那份计划整体已经
有 68 处陈旧 —— 那些是别的改动的账，不在这次的范围里。这次改的只是那个数。

- [ ] **Step 1: 把那个数改过来**

`docs/superpowers/plans/2026-11-13-mmxcode-gateway.md` —— `maxBodyBytes` 那一段改成
Task 2 Step 3 里那一段（逐字一样）。

- [ ] **Step 2: 提交**

```bash
git add docs/superpowers/plans/2026-11-13-mmxcode-gateway.md
git commit -F .git/commit_msg.txt
```

---

### Task 6: 全查一遍

- [ ] **Step 1: 全部的测试**

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
```

- [ ] **Step 2: 计划的检查**

```bash
python -I docs/superpowers/tools/check_plan_fences.py docs/superpowers/plans/2026-11-21-mmxcode-request-body-limit.md
python -I docs/superpowers/tools/check_plan_files.py docs/superpowers/plans/2026-11-21-mmxcode-request-body-limit.md .
```

第一个要打印 `0 problem(s)`，第二个要打印 `stale declarations: 0`。

- [ ] **Step 3: 自查有没有牙**

- 把 `handleChat` 里的闸门挪回读身体之后 → 「排队的时候身体没被读」那条测试要
  **红**；改回来。
- 把 `maxBodyBytes` 改回 `32 << 20` → 「上限是 64 MiB」那条测试要**红**；改回来。
- 把 `bytes.NewReader(req.Body)` 换回 `strings.NewReader(string(req.Body))` → 基准
  里那份 4 MB 要**回来**；换回来。

- [ ] **Step 4: 提交**

```bash
git add -A
git commit -F .git/commit_msg.txt
```

## 前后两遍

| | 改之前 | 改之后 |
|---|---|---|
| 排队的时候 | 身体已经被读进内存了 | 身体还在客户端那边，网关没有替它占内存 |
| 排队时网关占的内存 | 排队数 × 64 MiB（没有上限） | 0 —— 排着队的请求一个字节都没读 |
| 100 个 4 MB 的请求，闸门 20 | 峰值常驻 886 MB、峰值堆 821 MB | 峰值常驻 267 MB、峰值堆 199 MB |
| 请求体的上限 | 32 MiB | 64 MiB（用户可能传图片） |
| 往上游发身体的时候 | 抄一遍（多一份 64 MiB） | 不抄（`bytes.NewReader`） |
| 超限的身体 | 413（读完才知道超了） | 413（读的时候就知道） |
| 429 | 身体已经读过了 | 身体没读；连接多半被关掉 |
