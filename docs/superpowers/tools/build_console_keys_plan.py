import io, os, re, sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".."))

def read(p):
    return io.open(os.path.join(ROOT, p), encoding="utf-8").read()

DECL = re.compile(
    r"^(?:func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)|type\s+([A-Za-z_]\w*)|"
    r"var\s+([A-Za-z_]\w*)|const\s+([A-Za-z_]\w*))", re.M)

def decl_of(text, name):
    """The text of one top-level declaration, with the comment above it."""
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
    # the comment above it
    i = start - 1
    while i >= 0 and lines[i].startswith("//"):
        i -= 1
    start = i + 1
    # the end: braces balance
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

auth = read("internal/gateway/auth.go")
gw = read("internal/gateway/gateway.go")
gwt = read("internal/gateway/gateway_test.go")
store = read("internal/store/store.go")
storet = read("internal/store/store_test.go")
schema = read("internal/store/schema.go")
webui = read("internal/webui/webui.go")
appjs = read("internal/webui/static/app.js")
layout = read("internal/webui/templates/layout.html")

# the api_keys table entry in the schema
m = re.search(r"\t\{\n\t\t// The keys that open the chat API.*?\n\t\},\n", schema, re.S)
schema_entry = m.group(0).rstrip("\n") if m else ""

# the app.js part that belongs to the keys page
js_start = appjs.index("// 密钥：申请一把新的")
js_part = appjs[js_start:].rstrip("\n")

# the layout's nav line
nav_line = '\t<a href="/keys">密钥</a>\n'

out = []
w = out.append

w("""# 后台申请 api key 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在后台页面上申请聊天用的 api key，申请来的那一把能开聊天接口，开不了管理界面。

**Architecture:** `internal/apikey` 做 key 本身（生成、哈希、可以摆出来的那一段）；`internal/store` 存它的哈希（表 `api_keys`），key 本身不入库；`internal/gateway` 分两扇门 —— 聊天那扇（`/v1/`）认两把 key，管理页那扇（`/api/` 和页面）只认配置里那把；`internal/webui` 的 `/keys` 页和 `/api/keys` 管申请和撤销。

**Tech Stack:** Go 1.26、`modernc.org/sqlite`、`net/http`、`html/template`、原生 JS

---

## 三条必须守住的不变量

1. **key 本身不入库。** 库里存的是它的哈希（SHA-256）。数据库被人拿走的时候，里面没有一把能开门的 key。
2. **申请来的 key 开不了管理界面。** 它是给别的机器聊天用的；能开管理页的是配置里那把（后台登录 key）。
3. **key 只给得出一次。** 申请的那一次回答里有它本人，往后页面上只有它的开头（`sk-` 加八个字符）。

""")

# ---------------------------------------------------------------- Task 1
w("""### Task 1: apikey 包

**Files:**
- Create: `internal/apikey/apikey.go`
- Create: `internal/apikey/apikey_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/apikey/apikey_test.go`：

""")
w(block("go", whole("internal/apikey/apikey_test.go")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/apikey/
```

失败：`undefined: New`。

- [ ] **Step 3: 实现 apikey**

创建 `internal/apikey/apikey.go`：

""")
w(block("go", whole("internal/apikey/apikey.go")))
w("""
- [ ] **Step 4: 运行测试确认通过**

```
go test ./internal/apikey/
```

- [ ] **Step 5: 提交**

```
git add internal/apikey
git commit -m "feat(apikey): key 自己会生成，也会算哈希"
```

""")

# ---------------------------------------------------------------- Task 2
w("""### Task 2: 库里的 api_keys 表

**Files:**
- Modify: `internal/store/schema.go`（加 `api_keys` 表）
- Modify: `internal/store/store.go`（`APIKey` 和它的五个方法）
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/store/store_test.go` 里加：

""")
w(block("go", decls(storet,
                    "TestAKeyIsWrittenAndFound",
                    "TestTheKeyItselfIsNotInTheDatabase",
                    "TestARevokedKeyIsNotFound",
                    "TestTheKeysAreListedNewestFirst",
                    "TestAKeyThatIsNotThereIsNotFound")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/store/ -run "Key"
```

失败：`unknown: APIKey`。

- [ ] **Step 3: 加表**

`internal/store/schema.go` 的 `tables` 里加这一条（这张表不属于任何一个账号，所以它不在 `childTables` 里）：

""")
w(block("go", schema_entry))
w("""
- [ ] **Step 4: 实现库的方法**

`internal/store/store.go` 里加：

""")
w(block("go", decls(store,
                    "APIKey",
                    "Revoked",
                    "CreateAPIKey",
                    "ListAPIKeys",
                    "RevokeAPIKey",
                    "APIKeyByHash",
                    "HasAPIKeys")))
w("""
- [ ] **Step 5: 运行测试确认通过**

```
go test ./internal/store/
```

- [ ] **Step 6: 提交**

```
git add internal/store
git commit -m "feat(store): 存 key 的哈希，不存 key"
```

""")

# ---------------------------------------------------------------- Task 3
w("""### Task 3: 网关的两扇门

**Files:**
- Modify: `internal/gateway/auth.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/gateway_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/gateway/gateway_test.go` 里加：

""")
w(block("go", decls(gwt,
                    "TestAKeyMadeInTheConsoleOpensTheChatAPI",
                    "TestARevokedKeyDoesNotOpenTheChatAPI",
                    "TestAKeyMadeInTheConsoleDoesNotOpenTheConsole",
                    "makeAKey")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/gateway/ -run "TestAKeyMadeInTheConsole|TestARevokedKeyDoesNotOpen"
```

失败：`the key made in the console was refused`。

- [ ] **Step 3: 实现鉴权**

`internal/gateway/auth.go` 里改 `auth`，把它拆成两半：认 key 的那半（`authWith`）和聊天那扇门（`authChat`）。

""")
w(block("go", decls(auth,
                    "auth",
                    "authWith",
                    "givenKey",
                    "authChat",
                    "hasMadeKeys",
                    "isChatPath")))
w("""
- [ ] **Step 4: 实现路由**

`internal/gateway/gateway.go` 的 `ServeHTTP`：路径是聊天接口的走 `authChat`，别的走 `auth`。

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
git commit -m "feat(gateway): 聊天那扇门认两把 key"
```

""")

# ---------------------------------------------------------------- Task 4
w("""### Task 4: 后台的密钥页

**Files:**
- Create: `internal/webui/keys.go`
- Create: `internal/webui/keys_test.go`
- Create: `internal/webui/templates/keys.html`
- Modify: `internal/webui/webui.go`（路由）
- Modify: `internal/webui/templates/layout.html`（导航里加一页）
- Modify: `internal/webui/static/app.js`

- [ ] **Step 1: 写失败的测试**

创建 `internal/webui/keys_test.go`：

""")
w(block("go", whole("internal/webui/keys_test.go")))
w("""
- [ ] **Step 2: 运行测试确认失败**

```
go test ./internal/webui/ -run "Key"
```

失败：404。

- [ ] **Step 3: 实现密钥的接口**

创建 `internal/webui/keys.go`：

""")
w(block("go", whole("internal/webui/keys.go")))
w("""
- [ ] **Step 4: 写页面**

`internal/webui/templates/keys.html`：

""")
w(block("html", whole("internal/webui/templates/keys.html")))
w("""
`internal/webui/templates/layout.html` 的导航里加一行：

""")
w(block("html", nav_line.rstrip("\n")))
w("""
`internal/webui/static/app.js` 里加：

""")
w(block("js", js_part))
w("""
- [ ] **Step 5: 挂路由**

`internal/webui/webui.go` 的 `ServeHTTP`：

""")
w(block("go", decl_of(webui, "ServeHTTP")))
w("""
- [ ] **Step 6: 运行测试确认通过**

```
go test ./internal/webui/
```

- [ ] **Step 7: 提交**

```
git add internal/webui
git commit -m "feat(webui): 后台能申请和撤销聊天用的 key"
```

""")

# ---------------------------------------------------------------- Task 5
w("""### Task 5: 文档

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 写清楚两把 key 的分工**

README 的「鉴权」一节改成两把 key 的表：后台登录 key（配置里的）开管理界面，聊天 key（后台申请的）开聊天接口。聊天接口两把都认，管理界面只认第一把。

- [ ] **Step 2: 写清楚 key 只给得出一次**

README 写明：库里存的是哈希，申请的那一次回答里有 key 本身，往后拿不回来；撤销是撤销不是删除，那一行还在列表里。

- [ ] **Step 3: 提交**

```
git add README.md
git commit -m "docs(readme): 两把 key 的分工和申请的那一次"
```

""")

path = os.path.join(ROOT, "docs/superpowers/plans/2026-11-15-mmxcode-console-keys.md")
io.open(path, "w", encoding="utf-8", newline="\n").write("".join(out))
print("written", path)
