"""Assemble the body-memory plan, splicing the shipped Go in verbatim.

The plan's Go blocks have to match the repo byte for byte, so they are
read out of the repo rather than typed here.
"""
import io
import os

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))


def read(path):
    return io.open(os.path.join(REPO, path), encoding="utf-8").read().replace("\r\n", "\n")


def cut(text, start, end, keep_end=True):
    """The text from the line that starts with `start` to the line that starts with `end`."""
    i = text.index(start)
    j = text.index(end, i) + (len(end) if keep_end else 0)
    return text[i:j].rstrip("\n")


gw = read("internal/gateway/gateway.go")
rot = read("internal/rotator/session.go")
openai = read("internal/convert/openai.go")
requests = read("internal/convert/requests.go")

blocks = {
    "BODIES": read("internal/gateway/bodies.go"),
    "MEMORY_TEST": read("internal/gateway/memory_test.go"),
    "READ": cut(
        gw,
        "\t// 请求体有个上限：不然一个客户端发来一大块东西，网关就把它整个",
        "\t\twriteError(w, proto, http.StatusBadRequest, \"the request body could not be read\")\n\t\treturn\n\t}",
    ),
    "PEEK": cut(
        gw,
        "\t// 身体只读一遍：模型、流式、会话这三样都要从身体里读出来，读四遍",
        "\tsess := rotator.SessionOfBytes(r.Header, peek.System.text, firstUserOfPeek(peek))",
    ),
    "PEEKERS": cut(
        gw,
        "// requestPeek is what the gateway reads out of a request body before it",
        "func textOfString(raw []byte) []byte {",
        keep_end=False,
    ),
    "TEXT_OF_STRING": cut(
        gw,
        "func textOfString(raw []byte) []byte {",
        "\treturn []byte(s)\n}",
    ),
    "SESSION": cut(
        rot,
        "func SessionOf(headers map[string][]string, system, firstUser string) Session {",
        "\treturn hex.EncodeToString(sum)[:32]\n}",
    ),
    "CONVERT_RAW": read("internal/convert/raw.go"),
    "CONVERT_MEMORY_TEST": read("internal/convert/memory_test.go"),
    "OPENAI_MESSAGE": cut(
        openai,
        "// chatMessage is one message of an OpenAI request.",
        "\tToolCallID string         `json:\"tool_call_id\"`\n}",
    ),
    "OPENAI_BLOCKS": cut(
        openai,
        "// chatBlocks turns an OpenAI message's content into Anthropic blocks.",
        "\treturn out\n}\n\n// chatTextBlocks",
        keep_end=False,
    ),
    "REQUESTS_ITEMS": cut(
        requests,
        "\tvar items []rawJSON",
        "\t\tout = append(out, anthropicMessage{Role: role, Content: blocks})",
    ),
}

plan = u"""# 一个身体在飞的时候占多少 —— 实现计划

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
{MEMORY_TEST}
```

- [ ] **Step 2: 跑一遍，看它失败**

```bash
go test ./internal/gateway/ -run 'TestTheBodyIsReadIntoOneBuffer' -v
```

期望：分配超过 20 MiB（`io.ReadAll` 要 36 MiB）。

- [ ] **Step 3: 写实现**

`internal/gateway/bodies.go` —— 整个文件：

```go
{BODIES}
```

`internal/gateway/gateway.go` —— `handleChat` 里读身体那一段：

```go
{READ}
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
{PEEK}
```

- [ ] **Step 2: 身体里的东西**

`internal/gateway/gateway.go`：

```go
{PEEKERS}
```

```go
{TEXT_OF_STRING}
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

**为什么先看规格 §3.4：** `system + "\\x00" + firstUser` 拼起来是一份，转成
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
	if s := SessionOfBytes(nil, []byte("  "), []byte("\\n\\t")); s.Key != "" {
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
{SESSION}
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
{CONVERT_MEMORY_TEST}
```

```bash
go test ./internal/convert/ -run XXX -bench . -benchmem
```

期望：转换一个 63 MiB 的图片：OpenAI 那条路分配 72 MiB 上下，Codex 那条路
336 MiB 上下。

- [ ] **Step 2: 指着身体的那一段**

`internal/convert/raw.go` —— 整个文件：

```go
{CONVERT_RAW}
```

- [ ] **Step 3: OpenAI 那边不抄**

`internal/convert/openai.go` —— 消息的内容：

```go
{OPENAI_MESSAGE}
```

`internal/convert/openai.go` —— 块：

```go
{OPENAI_BLOCKS}
```

- [ ] **Step 4: Codex 那边不抄**

`internal/convert/requests.go` —— 那些项：

```go
{REQUESTS_ITEMS}
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
"""

for k, v in blocks.items():
    plan = plan.replace("{" + k + "}", v)

out = os.path.join(REPO, "docs/superpowers/plans/2026-11-21-mmxcode-body-memory.md")
io.open(out, "w", encoding="utf-8", newline="\n").write(plan)
print("written", out, len(plan), "bytes")
