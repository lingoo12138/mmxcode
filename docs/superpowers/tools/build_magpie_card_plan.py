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
gwt = read("internal/gateway/gateway_test.go")

out = []
w = out.append

w("""# magpie 的额度卡片 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 网关给出一个 magpie 认得的额度卡片接口，magpie 把它当作 provider 的额度卡片画出来。

**Architecture:** `internal/card` 把网关的读数摆成 magpie 的卡片形状（`plan`、`until`、`balance`、`windows`）；`internal/gateway` 的 `GET /v1/usage` 把它交出去，在聊天那扇门后面（手里有聊天那把 key 的都能问）。

**Tech Stack:** Go 1.26、`net/http`、`encoding/json`

---

## 三条必须守住的不变量

1. **形状是 magpie 的。** 字段名照着参考实现（`packages/minimax/index.mjs` 的 `usageOf`）：`plan`、`until`、`balance`、`windows`。我们自己多知道的东西收在 `mmxcode` 那一个名字下面，不跟 magpie 的字段混在一起。
2. **没读到的不是零。** 读不到的账号不算进 `balance`；读不到的计划不写成 `Free`。卡片上少一个数，比卡片上有一个假数好。
3. **网关是轮转的，卡片是整个网关的。** `balance` 是所有账号剩下的积分加起来；计划取账号里最重的那个；窗口每一个都说清是哪个账号的。

""")

w("""### Task 1: card 包

**Files:**
- Create: `internal/card/card.go`
- Create: `internal/card/card_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/card/card_test.go`：

""")
w(block("go", whole("internal/card/card_test.go")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/card/
```

失败：`undefined: Of`。

- [ ] **Step 3: 实现 card**

创建 `internal/card/card.go`：

""")
w(block("go", whole("internal/card/card.go")))
w("""
- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/card/
```

- [ ] **Step 5: 提交**

```
git add internal/card
git commit -m "feat(card): 网关的额度摆成 magpie 的卡片形状"
```

""")

w("""### Task 2: 网关的卡片接口

**Files:**
- Create: `internal/gateway/usage.go`
- Modify: `internal/gateway/gateway.go`（路由）
- Modify: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/gateway/gateway_test.go` 里加：

""")
w(block("go", decls(gwt,
                    "TestTheUsageCardIsMagpiesShape",
                    "TestTheUsageCardNeedsAKey",
                    "TestAKeyMadeInTheConsoleOpensTheUsageCard")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "UsageCard"
```

失败：`code = 404`。

- [ ] **Step 3: 实现接口**

创建 `internal/gateway/usage.go`：

""")
w(block("go", whole("internal/gateway/usage.go")))
w("""
- [ ] **Step 4: 挂路由**

`internal/gateway/gateway.go` 的 `ServeHTTP`：

""")
w(block("go", decl_of(gw, "ServeHTTP")))
w("""
- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 6: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): GET /v1/usage 给 magpie 的卡片"
```

""")

w("""### Task 3: 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 写清楚这个接口**

README 的「对外接口」里加上 `GET /v1/usage`，再写一节「magpie 的额度卡片」：卡片长什么样、四个字段是什么、网关是多账号的时候每个字段怎么来的、数字是上一次读到的（`refresh=1` 才现读）。

- [ ] **Step 2: 提交**

```
git add README.md
git commit -m "docs(readme): magpie 的额度卡片"
```

""")

path = os.path.join(ROOT, "docs/superpowers/plans/2026-11-15-mmxcode-magpie-card.md")
io.open(path, "w", encoding="utf-8", newline="\n").write("".join(out))
print("written", path)
