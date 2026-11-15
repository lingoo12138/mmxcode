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

site_models = read("internal/site/models.go")
site_models_test = read("internal/site/models_test.go")
acc = read("internal/account/account.go")
gw = read("internal/gateway/gateway.go")

out = []
w = out.append

w("""# 模型列表听站点的 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `GET /v1/models` 给的是站点自己那份名单 —— MiniMax 把 `MiniMax-M3.1-Flash-Preview` 改叫 `MiniMax-M3.1-Flash` 之后，客户端（magpie 之类）也要能挑到现在这个模型。

**Architecture:** `internal/gateway` 的 `modelsFor` 去站点读一次名单，读到的留着（十分钟），读不到就摆 `internal/site` 的内置名单。`internal/account` 把读 provider config 的那个函数露出来（`ParseModels`），因为站点答的就是那个形状。

**Tech Stack:** Go 1.26、`net/http`、`encoding/json`

---

## 三条必须守住的不变量

1. **名单是站点的。** 内置的那份只是兜底：它抄的时候会旧（MiniMax 改过模型的名字），站点那份不会。
2. **问的时候不带令牌。** 站点没有令牌也答；带令牌去问有可能在路上把令牌续了 —— 一份名单不值得换一把新令牌。
3. **问一次，留着。** 客户端问十回，站点被问一回。名单一年也换不了几回。

""")

w("""### Task 1: 内置名单补上现在那个模型

**Files:**
- Modify: `internal/site/models.go`
- Modify: `internal/site/models_test.go`

- [ ] **Step 1: 写失败的测试**

`internal/site/models_test.go` 的 `TestFallbackModelsLimits` 里加一行：

""")
w(block("go", decl_of(site_models_test, "TestFallbackModelsLimits")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/site/
```

失败：`got 4 models, want 5`。

- [ ] **Step 3: 内置名单加上它**

`internal/site/models.go`：

""")
w(block("go", decl_of(site_models, "fallbackModels")))
w("""
- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/site/
```

- [ ] **Step 5: 提交**

```
git add internal/site
git commit -m "feat(site): 内置名单补上 MiniMax-M3.1-Flash"
```

""")

w("""### Task 2: 把读 provider config 的那个函数露出来

**Files:**
- Modify: `internal/account/account.go`

- [ ] **Step 1: 导出读名单的函数**

站点答的是 provider config 的样子（`{providers:[{providerId,config:{models,model_order}}]}`），不是模型列表的样子。读它的那个函数本来是包内的，网关要用就得露出来：

""")
w(block("go", decl_of(acc, "ParseModels")))
w("""
- [ ] **Step 2: 运行测试确认通过**

```
go test ./internal/account/
```

- [ ] **Step 3: 提交**

```
git add internal/account
git commit -m "feat(account): 读模型名单的函数露出来"
```

""")

w("""### Task 3: 网关的名单听站点的

**Files:**
- Create: `internal/gateway/models.go`
- Create: `internal/gateway/models_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/failures_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/gateway/models_test.go`：

""")
w(block("go", whole("internal/gateway/models_test.go")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "ModelList|Model"
```

失败：名单是内置的那份，站点一次也没被问。

- [ ] **Step 3: 实现**

创建 `internal/gateway/models.go`：

""")
w(block("go", whole("internal/gateway/models.go")))
w("""
`internal/gateway/gateway.go` 的 `Server` 加上留名单的地方，`handleModels` 改成用 `modelsFor`：

""")
w(block("go", decls(gw, "Server", "handleModels")))
w("""
`internal/gateway/failures_test.go` 里那个「模型列表不问上游」的测试要改掉：那个决定翻过来了 —— 现在问的是站点，问一次。断言改成「站点被问了一次」，其余不变。

- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/gateway/
```

- [ ] **Step 5: 提交**

```
git add internal/gateway
git commit -m "feat(gateway): 模型列表听站点的"
```

""")

w("""### Task 4: 文档

**Files:**
- Modify: `README.md`
- Modify: `cmd/mmxcode/root_test.go`

- [ ] **Step 1: 写清楚这个名单是怎么来的**

README 加一节「模型列表」：名单是站点自己那份（十分钟读一次、留着）、问的时候不带令牌、站点答不上来的时候摆内置的那一份（`mmxcode models` 打印的就是它）、站点现在给的是什么。

- [ ] **Step 2: 命令行那个测试跟着改**

内置名单多了一个模型，`cmd/mmxcode/root_test.go` 的 `TestModelsPrintsTheFallbackList` 里的行数从 4 改成 5。

- [ ] **Step 3: 提交**

```
git add README.md cmd/mmxcode/root_test.go
git commit -m "docs(readme): 模型列表听站点的"
```

""")

path = os.path.join(ROOT, "docs/superpowers/plans/2026-11-15-mmxcode-model-list.md")
io.open(path, "w", encoding="utf-8", newline="\n").write("".join(out))
print("written", path)
