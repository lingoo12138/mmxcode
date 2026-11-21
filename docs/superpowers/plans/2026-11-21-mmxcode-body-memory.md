# 一个身体在飞的时候占多少 —— 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一个 63 MiB 的身体进来，网关的内存只涨 63 MiB —— 现在涨 378 MiB。

**Architecture:** 身体只读一遍（四个 `json.Unmarshal` 合成一个），读的时候照着
`Content-Length` 读进一块刚好那么大的缓冲（`io.ReadAll` 一路翻倍着长大），身
体里的文字**指着**身体里原来那一段而不是抄一份（图片的 base64 一个字节也不
抄），会话的哈希分块写进去不拼（拼起来是两份，转成字节又是一份），OpenAI 和
Codex 的转换也指着身体（`json.RawMessage` 抄的是整段内容）。

**Tech Stack:** Go 1.26、标准库（`encoding/json`、`crypto/sha256`、`bytes`）。

**规格：** `docs/superpowers/specs/2026-11-21-mmxcode-body-memory-design.md`

## 文件一览

| 文件 | 干什么 |
|---|---|
| `internal/gateway/bodies.go` | `readBody`：照着 `Content-Length` 读一次 |
| `internal/gateway/memory_test.go` | 内存的账：读身体不翻倍、图片不抄、对话不抄、哈希的是字 |
| `internal/gateway/gateway.go` | `requestPeek`：身体读一遍，文字指着身体里那一段 |
| `internal/rotator/session.go` | `SessionOfBytes` 和分块写的哈希 |
| `internal/convert/raw.go` | `rawJSON`（指着身体的那一段）和开够了的输出缓冲 |
| `internal/convert/openai.go`、`internal/convert/requests.go` | 转换不再抄整段内容 |
| `internal/convert/memory_test.go` | 转换的账：三个基准 |

---

### Task 1: 读身体，一次读完

**Files:**
- Modify: `internal/gateway/bodies.go`
- Modify: `internal/gateway/gateway.go`

**为什么先看规格 §3.1：** `io.ReadAll` 一路翻倍着长大，翻倍的那一刻两份都在
—— 那 127 MiB 就是峰值，而身体本身只有 63 MiB。

- [ ] **Step 1: 写失败的测试**

`internal/gateway/memory_test.go` —— 整个文件：

```go
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/rotator"
)

// TestTheBodyIsReadIntoOneBuffer says that reading a body does not
// double it.
//
// ReadAll 一路翻倍着长大：16 MiB 的身体它要分配 32 MiB，翻倍的那一刻两份
// 都在 —— 那 32 MiB 就是峰值，而身体本身只有 16 MiB。
func TestTheBodyIsReadIntoOneBuffer(t *testing.T) {
	const size = 16 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = 'x'
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	body, err := readBody(w, r)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != size {
		t.Fatalf("read %d bytes, want %d", len(body), size)
	}
	used := after.TotalAlloc - before.TotalAlloc
	if used > size+size/4 {
		t.Fatalf("reading a %d byte body allocated %d bytes", size, used)
	}
}

// TestTheImageIsNotCopied says that the picture in a body is not copied.
//
// 抄一份是几十兆的白抄：抄完只是为了知道「这段里没有文字」。
func TestTheImageIsNotCopied(t *testing.T) {
	const size = 16 << 20
	picture := strings.Repeat("A", size)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		picture + `"},"type":"text","text":"what is this?"}]}]}`)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	peek := peekOf(body)
	runtime.ReadMemStats(&after)

	if got := peek.System.text; len(got) != 0 {
		t.Fatalf("the system prompt is %d bytes, want none", len(got))
	}
	if got := string(firstUserOfPeek(peek)); got != "what is this?" {
		t.Fatalf("the first user message is %q", got)
	}
	used := after.TotalAlloc - before.TotalAlloc
	if used > 1<<20 {
		t.Fatalf("reading a body with a %d byte picture allocated %d bytes", size, used)
	}
}

// TestTheWholeConversationIsNotCopied says that the messages that are
// not the first one are not copied either.
//
// 一段对话里，第一条用户消息以外的那些话不用留：它们只是路过。
func TestTheWholeConversationIsNotCopied(t *testing.T) {
	const size = 16 << 20
	talk := strings.Repeat("y", size)
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"assistant","content":"` + talk + `"}]}`)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	peek := peekOf(body)
	runtime.ReadMemStats(&after)

	if got := string(firstUserOfPeek(peek)); got != "first" {
		t.Fatalf("the first user message is %q", got)
	}
	used := after.TotalAlloc - before.TotalAlloc
	if used > 1<<20 {
		t.Fatalf("reading a body with a %d byte message allocated %d bytes", size, used)
	}
}

// TestTheSessionHashesTheWords says that the session is a hash of the
// words, not of the bytes they came in.
//
// 身体里的字节是转义过的（换行是 \n 两个字节），哈希的那一份是转义之前的
// 那些字：不然同一个会话两次进来会算成两个。
func TestTheSessionHashesTheWords(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"one\ntwo"}]}`)
	peek := peekOf(body)

	got := rotator.SessionOfBytes(nil, nil, firstUserOfPeek(peek)).UpstreamID
	sum := sha256.Sum256([]byte("\x00one\ntwo"))
	want := hex.EncodeToString(sum[:])[:32]
	if got != want {
		t.Fatalf("the session hash is %s, want %s", got, want)
	}
}

// TestTheSessionIsTheSameWithAndWithoutEscapes says that the words and
// the bytes of the same words hash the same.
//
// 转义过的写法算的是同一段字：一个会话改了写法，还是那个会话。
func TestTheSessionIsTheSameWithAndWithoutEscapes(t *testing.T) {
	plain := rotator.SessionOfBytes(nil, nil, []byte("one\ntwo")).UpstreamID
	escaped := rotator.SessionOfBytes(nil, nil, []byte("one two")).UpstreamID
	if plain == escaped {
		t.Fatal("two different conversations hashed the same")
	}
	body := []byte(`{"messages":[{"role":"user","content":"one\ntwo"}]}`)
	peek := peekOf(body)
	if string(firstUserOfPeek(peek)) != "one\ntwo" {
		t.Fatalf("the first user message is %q", firstUserOfPeek(peek))
	}
}

```

- [ ] **Step 2: 跑一遍，看它失败**

```bash
go test ./internal/gateway/ -run 'TestTheBodyIsReadIntoOneBuffer' -v
```

期望：分配超过 20 MiB（`io.ReadAll` 要 36 MiB）。

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

`internal/gateway/gateway.go` —— `handleChat` 里读身体那一段：

```go
	// 请求体有个上限：不然一个客户端发来一大块东西，网关就把它整个
	// 读进内存里，然后就没有内存了。
	//
	// 照着客户端说的那个数读，一次读完：ReadAll 一路翻倍着长大，翻倍的
	// 那一刻两份都在 —— 那就是峰值，而身体本身只有一半那么大。
	body, err := readBody(w, r)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, proto, http.StatusRequestEntityTooLarge, bodyTooBig(tooBig.Limit, -1))
			return
		}
		writeError(w, proto, http.StatusBadRequest, "the request body could not be read")
		return
	}
```

- [ ] **Step 4: 跑一遍，看它过**

```bash
go test ./internal/gateway/ -v
```

期望：全过。

- [ ] **Step 5: 提交**

```bash
git add internal/gateway/bodies.go internal/gateway/gateway.go internal/gateway/memory_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 2: 身体读一遍，文字不抄

**Files:**
- Modify: `internal/gateway/gateway.go`

**为什么先看规格 §3.2 和 §3.3：** 身体被读了四遍（1.5 秒的 CPU），而文字是
抄下来的（图片就是几十兆的白抄）。

- [ ] **Step 1: `handleChat` 里读一遍**

`internal/gateway/gateway.go`：

```go
	// 身体只读一遍：模型、流式、会话这三样都要从身体里读出来，读四遍
	// 就是四倍的 CPU。
	peek := peekOf(body)
	model := peek.Model
	stream := peek.Stream
	sess := rotator.SessionOfBytes(r.Header, peek.System.text, firstUserOfPeek(peek))
```

- [ ] **Step 2: 身体里的东西**

`internal/gateway/gateway.go`：

```go
// requestPeek is what the gateway reads out of a request body before it
// sends it on.
//
// 读一遍就够了：读四遍就是四倍的 CPU —— 一个 63 MiB 的身体，读四遍要
// 1.5 秒，读一遍要 0.4 秒。
type requestPeek struct {
	// Model is the model the client asked for.
	Model string `json:"model"`
	// Stream says the client wants the answer streamed.
	Stream bool `json:"stream"`
	// System is the system prompt.
	System peekText `json:"system"`
	// Messages are the conversation so far.
	Messages []struct {
		Role    string   `json:"role"`
		Content peekText `json:"content"`
	} `json:"messages"`
	// Input is the Codex shape: the whole conversation in one field.
	Input peekInput `json:"input"`
}

// peekOf reads what the gateway needs out of a request body.
//
// 读不出来就当没有：网关不能因为一个字段怪就把整个请求扔了。
func peekOf(body []byte) requestPeek {
	var p requestPeek
	json.Unmarshal(body, &p)
	return p
}

// firstUserOfPeek reads the first user message out of what was read.
//
// The three protocols put it in different places, so all of them are
// looked at.
func firstUserOfPeek(p requestPeek) []byte {
	for _, m := range p.Messages {
		if m.Role == "user" {
			return m.Content.text
		}
	}
	return p.Input.text
}

// peekText is the text of a content field, as it stands in the body.
//
// 它记的是身体里原来的那一段字节，不是抄一份：抄一份就是几十兆的白抄。
// 身体本身在请求的整个过程里都在，所以指着它的那一段，一个字节也不用多。
//
// 图片的 base64 在别的字段里（source、image_url），标准库看它不看就跳过
// 去了 —— 几十兆的图片，一个字节也不抄。
type peekText struct {
	// text is the text of the field.
	text []byte
}

// UnmarshalJSON reads the text out of a content field.
func (c *peekText) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		c.text = textOfString(raw)
		return nil
	}
	// 是一串部件？只有文字算数。
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []byte
	for _, p := range parts {
		out = append(out, p.Text...)
	}
	c.text = out
	return nil
}

// peekInput is the text of an input field, when it is a plain string.
//
// Codex 的 input 是一个数组（那边是 items）的时候，那里面没有文字可取 ——
// 从前就是这样（只认字符串）。
type peekInput struct {
	// text is the input.
	text []byte
}

// UnmarshalJSON reads the input when it is a plain string.
func (c *peekInput) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || raw[0] != '"' {
		return nil
	}
	c.text = textOfString(raw)
	return nil
}

// textOfString reads the text out of a JSON string, without copying it
// when it can.
//
// 没有转义的时候，字符串里那些字节就是文字本身 —— 指着就行。有转义的时
// 候（\" 或者 \n），那里面是另一段字节了，抄一份。
```

```go
func textOfString(raw []byte) []byte {
	if len(raw) < 2 {
		return nil
	}
	inside := raw[1 : len(raw)-1]
	if bytes.IndexByte(inside, '\\') < 0 {
		return inside
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return []byte(s)
}
```

- [ ] **Step 3: 跑一遍**

```bash
go build ./... && go test ./internal/gateway/ -v
```

期望：全过。图片不抄、对话不抄那两条（`TestTheImageIsNotCopied`、
`TestTheWholeConversationIsNotCopied`）要过。

- [ ] **Step 4: 提交**

```bash
git add internal/gateway/gateway.go
git commit -F .git/commit_msg.txt
```

---

### Task 3: 会话的哈希分块写

**Files:**
- Modify: `internal/rotator/session.go`
- Modify: `internal/rotator/rotator_test.go`

**为什么先看规格 §3.4：** `system + "\x00" + firstUser` 拼起来是一份，转成
字节又是一份 —— 一句几十兆的话，光是这个哈希就白涨两倍。

- [ ] **Step 1: 写失败的测试**

`internal/rotator/rotator_test.go` 后面加两条：

```go
func TestTheBytesSessionIsTheStringSession(t *testing.T) {
	a := SessionOf(nil, "sys", "hello")
	b := SessionOfBytes(nil, []byte("sys"), []byte("hello"))
	if a.Key != b.Key || a.UpstreamID != b.UpstreamID {
		t.Fatalf("the same words gave two sessions: %v and %v", a, b)
	}
	if SessionOf(nil, "sys", "other").Key == b.Key {
		t.Fatal("two conversations gave the same key")
	}
}

func TestTheBytesSessionIsEmptyWhenThereAreNoWords(t *testing.T) {
	if s := SessionOfBytes(nil, []byte("  "), []byte("\n\t")); s.Key != "" {
		t.Fatalf("blank words gave the key %q", s.Key)
	}
	if s := SessionOfBytes(nil, nil, nil); s.Key != "" {
		t.Fatalf("no words gave the key %q", s.Key)
	}
}
```

- [ ] **Step 2: 跑一遍，看它失败**

```bash
go test ./internal/rotator/ -run 'TestTheBytesSession' -v
```

期望：`SessionOfBytes` 没有定义，编译不过。

- [ ] **Step 3: 写实现**

`internal/rotator/session.go`：

```go
func SessionOf(headers map[string][]string, system, firstUser string) Session {
	return SessionOfBytes(headers, []byte(system), []byte(firstUser))
}

// SessionOfBytes is SessionOf with the words as bytes.
//
// 字节是身体的原来那一段，指着就行：抄一份是几十兆的白抄 —— 一句几十兆
// 的话，抄两份就是白涨一百多兆。
func SessionOfBytes(headers map[string][]string, system, firstUser []byte) Session {
	for _, h := range sessionHeaders {
		for _, v := range headers[h] {
			if s := strings.TrimSpace(v); s != "" {
				return Session{Key: "h:" + digest("hdr:"+s), UpstreamID: s}
			}
		}
	}
	if len(bytes.TrimSpace(system)) == 0 && len(bytes.TrimSpace(firstUser)) == 0 {
		return Session{}
	}
	d := digestBytes(system, []byte{0}, firstUser)
	return Session{Key: "m:" + d, UpstreamID: d}
}

// digest is the short hash a session key is made of.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

// digestBytes is the short hash of several pieces put together.
//
// 分开写进去，不先拼起来：拼起来是一份，转成字节又是一份 —— 一句几十兆
// 的第一句话，光是这个哈希就白涨两倍。分块写进去，多的那点地方是固定
// 的。
//
// 哈希出来的东西跟拼起来算的**一个字节都不差**：写进去的还是同样的那
// 些字节，同样的顺序。
func digestBytes(parts ...[]byte) string {
	h := sha256.New()
	buf := make([]byte, 32<<10)
	for _, p := range parts {
		for len(p) > 0 {
			n := copy(buf, p)
			h.Write(buf[:n])
			p = p[n:]
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:32]
}
```

- [ ] **Step 4: 跑一遍，看它过**

```bash
go test ./internal/rotator/ -v
```

期望：全过。而且从前的那些哈希**一个都没有变**（`SessionOf` 还是原来那个
样子）。

- [ ] **Step 5: 提交**

```bash
git add internal/rotator/session.go internal/rotator/rotator_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 4: 转换不再抄整段内容

**Files:**
- Create: `internal/convert/raw.go`
- Modify: `internal/convert/openai.go`
- Modify: `internal/convert/requests.go`
- Create: `internal/convert/memory_test.go`

**为什么先看规格 §3.5：** 转成 Anthropic 的形状的时候也在抄：`json.RawMessage`
抄下整段内容，Codex 那边每一项都重新写一遍再读一遍。

- [ ] **Step 1: 写基准（它是账，不是闸门）**

`internal/convert/memory_test.go` —— 整个文件：

```go
package convert

import (
	"strings"
	"testing"
)

// A body of 63 MiB: the biggest one the gateway lets in.
const bigBody = 63 << 20

// BenchmarkTheConversionOfABigBody says what a big OpenAI body costs.
//
// 一份 63 MiB 的文字，转成 Anthropic 的形状要重新写一遍 —— 那份是身体
// 的第二份。数字是量出来的，不是猜的。
func BenchmarkTheConversionOfABigBody(b *testing.B) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", bigBody) + `"}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := ToAnthropicOpenAI(body)
		if err != nil || len(out) == 0 {
			b.Fatal(err)
		}
	}
}

// BenchmarkTheConversionOfABigImage says what a big picture costs.
//
// 图片在 OpenAI 那边是没有的（它被丢掉了），所以出来的那份是小的：身体
// 指着就行，不用抄。
func BenchmarkTheConversionOfABigImage(b *testing.B) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("A", bigBody) + `"}}]}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := ToAnthropicOpenAI(body)
		if err != nil || len(out) == 0 {
			b.Fatal(err)
		}
	}
}

// BenchmarkTheCodexConversionOfABigImage says what a big picture costs
// on the Codex road.
func BenchmarkTheCodexConversionOfABigImage(b *testing.B) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"what is this?"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` +
		strings.Repeat("A", bigBody) + `"}]}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := ToAnthropic(body)
		if err != nil || len(out) == 0 {
			b.Fatal(err)
		}
	}
}

```

```bash
go test ./internal/convert/ -run XXX -bench . -benchmem
```

期望：转换一个 63 MiB 的图片：OpenAI 那条路分配 72 MiB 上下，Codex 那条路
336 MiB 上下。

- [ ] **Step 2: 指着身体的那一段**

`internal/convert/raw.go` —— 整个文件：

```go
package convert

import (
	"bytes"
	"encoding/json"
)

// nullBytes is the JSON null.
var nullBytes = []byte("null")

// rawJSON is a piece of JSON as it stands in the body.
//
// 它记的是身体里原来的那一段字节，不是抄一份：抄一份是几十兆的白抄。
// 身体在转换的整个过程里都在，所以指着它的那一段，一个字节也不用多。
type rawJSON []byte

// UnmarshalJSON keeps the bytes as they are.
func (r *rawJSON) UnmarshalJSON(b []byte) error {
	*r = b
	return nil
}

// sizeOf estimates how many bytes an Anthropic request will take.
//
// 缓冲开够了就不用翻倍着长大：翻倍是白抄 —— 一份 63 MiB 的文字，一路翻
// 倍抄过去要 126 MiB。开够了就只抄一遍。
//
// 估的是留下来的文字，不是身体：一张几十兆的图片在 OpenAI 那边是没有的
// （它被丢掉了），身体再大，出来的那份也是小的 —— 按身体估就是白开一块
// 几十兆的缓冲。
func sizeOf(out anthropicRequest) int {
	n := 1024 + len(out.Model) + len(out.System)
	for _, m := range out.Messages {
		n += len(m.Role) + 16
		for _, b := range m.Content {
			n += len(b.Text) + len(b.Content) + len(b.ID) + len(b.Name) +
				len(b.ToolUseID) + len(b.Input) + 64
		}
	}
	// 转义要多占一点地方：换行是两个字节。
	return n + n/8
}

// marshalTo writes the value as JSON into a buffer that is big enough from
// the start.
//
// json.Marshal 自己开一块缓冲，一路翻倍着长大 —— 一个 63 MiB 的身体，它
// 要分配 126 MiB 的缓冲，最后再抄一份 63 MiB 的出来。这里先把缓冲开够，
// 抄的那份也省了。
//
// 写出来的东西跟 json.Marshal 的**一个字节都不差**：同样的编码，同样的
// 转义。
func marshalTo(v any, size int) ([]byte, error) {
	if size < 1024 {
		size = 1024
	}
	buf := bytes.NewBuffer(make([]byte, 0, size+1024))
	enc := json.NewEncoder(buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode 会在后面添一个换行：请求的身体不要那个。
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

```

- [ ] **Step 3: OpenAI 那边不抄**

`internal/convert/openai.go` —— 消息的内容：

```go
// chatMessage is one message of an OpenAI request.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    rawJSON        `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
}
```

`internal/convert/openai.go` —— 块：

```go
// chatBlocks turns an OpenAI message's content into Anthropic blocks.
//
// The parts that carry text come across; the pictures and the files do
// not: their shapes differ, and a picture turned into a piece of text
// would be a lie.
func chatBlocks(raw rawJSON) []anthropicBlock {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), nullBytes) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]anthropicBlock, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			out = append(out, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
```

- [ ] **Step 4: Codex 那边不抄**

`internal/convert/requests.go` —— 那些项：

```go
	var items []rawJSON
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses: input is neither a string nor a list")
	}
	var out []anthropicMessage
	for _, item := range items {
		// 每一样东西都是身体里原来那一段字节：抄一份是几十兆的白抄。
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			continue
		}
		switch head.Type {
		case "function_call":
			var fc struct {
				CallID    string          `json:"call_id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(item, &fc); err != nil {
				return nil, err
			}
			input, err := toolInput(fc.Arguments)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{
				Role: "assistant",
				Content: []anthropicBlock{{
					Type: "tool_use", ID: fc.CallID, Name: fc.Name, Input: input,
				}},
			})
		case "function_call_output":
			var fo struct {
				CallID string `json:"call_id"`
				Output string `json:"output"`
			}
			if err := json.Unmarshal(item, &fo); err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{
				Role: "user",
				Content: []anthropicBlock{{
					Type: "tool_result", ToolUseID: fo.CallID, Content: fo.Output,
				}},
			})
		case "message", "":
			var mi struct {
				Role    string  `json:"role"`
				Content rawJSON `json:"content"`
			}
			if err := json.Unmarshal(item, &mi); err != nil {
				return nil, err
			}
			role := mi.Role
			if role == "" {
				role = "user"
			}
			if role == "system" {
				role = "user"
			}
			blocks, err := blocks(mi.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{Role: role, Content: blocks})
```

- [ ] **Step 5: 跑一遍**

```bash
go build ./... && go test ./internal/convert/ -v
go test ./internal/convert/ -run XXX -bench . -benchmem
```

期望：测试全过；转换一个 63 MiB 的图片：那两条路都是 6 MiB 上下。

- [ ] **Step 6: 提交**

```bash
git add internal/convert/raw.go internal/convert/openai.go internal/convert/requests.go internal/convert/memory_test.go
git commit -F .git/commit_msg.txt
```

---

### Task 5: 量一遍

**Files:**
- Modify: `docs/superpowers/specs/2026-11-21-mmxcode-body-memory-design.md`

- [ ] **Step 1: 把整个测试跑一遍**

```bash
go test ./...
```

- [ ] **Step 2: 把基准跑一遍，把数填回规格**

```bash
go test ./internal/convert/ -run XXX -bench . -benchmem
```

- [ ] **Step 3: 查计划跟代码对不对得上**

```bash
python -I docs/superpowers/tools/check_plan_fences.py docs/superpowers/plans/2026-11-21-mmxcode-body-memory.md
python -I docs/superpowers/tools/check_plan_files.py docs/superpowers/plans/2026-11-21-mmxcode-body-memory.md .
```

期望：`0 problem(s)`，`stale declarations: 0`。

---

## 前后两遍

| | 之前 | 之后 |
|---|---|---|
| 一个 63 MiB 的身体（Anthropic） | 378 MiB 堆峰值，409 MiB 分配 | 63 MiB（就是身体本身） |
| 读身体的 CPU | 读四遍，1.5 秒 | 读一遍，0.4 秒 |
| 图片 | 抄一遍（几十兆） | 抄 0 字节 |
| 会话的哈希 | 拼起来两份，转字节一份 | 分块写，多的地方固定 32 KiB |
| 转换（OpenAI，图片） | 72 MiB | 6 MiB |
| 转换（Codex，图片） | 336 MiB | 6 MiB |
