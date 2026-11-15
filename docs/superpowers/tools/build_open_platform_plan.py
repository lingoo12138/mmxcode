import io, os, re, sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".."))

def read(p):
    return io.open(os.path.join(ROOT, p), encoding="utf-8").read()

DECL = re.compile(
    r"^(?:func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)|type\s+([A-Za-z_]\w*)|"
    r"var\s+([A-Za-z_]\w*)|const\s+([A-Za-z_]\w*))", re.M)

def decl_of(text, name):
    lines = text.split("\n")
    start = None
    for i, line in enumerate(lines):
        m = DECL.match(line)
        if not m:
            continue
        got = None
        for g in m.groups():
            if g:
                got = g
        if got != name:
            continue
        start = i
        break
    if start is None:
        raise SystemExit("not found: " + name)
    i = start - 1
    while i >= 0 and lines[i].startswith("//"):
        i -= 1
    start = i + 1
    depth = 0
    seen = False
    end = start
    for j in range(start, len(lines)):
        depth += lines[j].count("{") - lines[j].count("}")
        if "{" in lines[j]:
            seen = True
        if seen and depth <= 0:
            end = j
            break
    return "\n".join(lines[start:end + 1])

def decls(text, *names):
    return "\n\n".join(decl_of(text, n) for n in names)

def whole(p):
    return read(p).rstrip("\n")

def block(lang, body):
    return "```" + lang + "\n" + body + "\n```\n"

gw = read("internal/gateway/gateway.go")
gwo = read("internal/gateway/openai_test.go")

out = []
w = out.append

w("""# 开放平台说「没有这条路」也要换条路 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** magpie 用 OpenAI 那套协议问网关时不再拿到 503：开放平台答的 `direct_route_not_configured` 要被当成「这条路不通」而不是「站点坏了」。

**Architecture:** `internal/gateway` 里那扇门后头有一条换路的逻辑：上游答 404 时，网关改成 Anthropic 那条路（本地转格式）用同一个账号再问一次。开放平台对 MiniMax Code 的账号答的是 503 加一句 `direct_route_not_configured` —— 说的是同一件事，只是换了Status码和一句人话。把它认出来，走同一条换路的逻辑。

**Tech Stack:** Go 1.26、`net/http`、`encoding/json`

---

## 三条必须守住的不变量

1. **503 不等于站点坏了。** 开放平台对 MiniMax Code 的账号在 `/chat/completions`、`/responses` 上答的是 `503 {"errorCode":50115,"errorReason":"direct_route_not_configured"}`：不是这条路对这个账号不通。那就得换路，不是换号。
2. **换的是路，不是号。** 换路之后还是同一个账号、同一把令牌：不消耗 refresh token，不在账号之间乱跳。
3. **认得出，也认不错。** 只认那一句明确的话（`errorCode 50115` 或 `direct_route_not_configured`）。别的 503 还是 503 —— 站点真的坏了的时候要照实说。

""")

w("""### Task 1: 认得出「没有这条路」

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/openai_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/gateway/openai_test.go` 里加：

""")
w(block("go", decls(gwo,
                    "TestTheOpenAIRequestFallsBackWhenTheOpenPlatformIsNotConfigured",
                    "TestTheCodexRequestFallsBackWhenTheOpenPlatformIsNotConfigured")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "NotConfigured"
```

失败：网关把上游的 503 原样给了客户端（`code = 503`）。

- [ ] **Step 3: 认出那一句**

`internal/gateway/gateway.go` 加一个认得那一句的函数：

""")
w(block("go", decl_of(gw, "openPlatformNotConfigured")))
w("""
- [ ] **Step 4: 在换路的地方用上它**

`internal/gateway/gateway.go` 里换路的那一段：

""")
w(block("go", """// 上游没有这条路：改用本地转换，同一个账号再来一次。
//
// 开放平台对 MiniMax Code 的账号答的也是「没有这条路」：503
// 加一句 direct_route_not_configured。那不是站点坏了，是那条
// 路对这个账号不通 —— 该走 Anthropic 那条。
if resp.StatusCode == http.StatusNotFound || openPlatformNotConfigured(raw) {"""))
w("""
- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 6: 提交**

```
git add internal/gateway
git commit -m "fix(gateway): 开放平台说没有这条路的时候换条路"
```

""")

w("""### Task 2: 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 写清楚这一种 503**

README 里加一条：还有一种「没有这条路」—— 开放平台对 MiniMax Code 的账号答 `503 direct_route_not_configured`，说的是那条路对这个账号不通，网关会自己换到 Anthropic 那条路再问一次。

- [ ] **Step 2: 提交**

```
git add README.md
git commit -m "docs(readme): 还有一种没有这条路"
```

""")

path = os.path.join(ROOT, "docs/superpowers/plans/2026-11-15-mmxcode-open-platform-fallback.md")
io.open(path, "w", encoding="utf-8", newline="\n").write("".join(out))
print("written", path)
