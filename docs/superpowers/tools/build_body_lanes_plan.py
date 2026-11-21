"""Assemble the body-lanes plan, splicing the shipped Go in verbatim.

The plan's Go blocks have to match the repo byte for byte, so they are
read out of the repo rather than typed here.
"""
import io
import os
import re

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))


def read(path):
    return io.open(os.path.join(REPO, path), encoding="utf-8").read().replace("\r\n", "\n")


def cut(text, start, end, keep_end=True):
    """The text from the line that starts with `start` to the line that starts with `end`."""
    i = text.index(start)
    j = text.index(end, i) + (len(end) if keep_end else 0)
    return text[i:j].rstrip("\n")


cfg = read("internal/config/config.go")
cfg_test = read("internal/config/config_test.go")
gw = read("internal/gateway/gateway.go")
readme = read("README.md")

blocks = {
    "BODIES": read("internal/gateway/bodies.go"),
    "BODIES_TEST": read("internal/gateway/bodies_test.go"),
    "CONC_STRUCT": cut(
        cfg,
        "// Concurrency holds how many requests may be in the air at once.",
        "\tBodyBudgetMB int `yaml:\"body_budget_mb\" json:\"body_budget_mb\"`\n}",
    ),
    "DEFAULT": cut(cfg, "func Default() Config {", "\n}\n"),
    "CFG_TEST": cut(
        cfg_test,
        "// TestTheShippedConfigsHaveTheBodyLimits says",
        "\t\t\t\tname, c.Concurrency.BodyBudgetMB, Default().Concurrency.BodyBudgetMB)\n\t\t}\n\t}\n}",
    ),
    "FIELDS": cut(
        gw,
        "// Server is the gateway.",
        "\tfallback *dbFallback\n}",
    ),
    "NEW": cut(
        gw,
        "func New(mgr *manager.Manager, cfg config.Config, s *store.Store, webui http.Handler) *Server {",
        "\treturn g\n}",
    ),
    "LANES": cut(
        gw,
        "func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, proto protocol) {",
        "\t\tdefer s.bodies.give(n)\n\t}",
    ),
    "README_LANES": cut(
        readme,
        "**身体分两条道。**",
        "大道的门排在并发闸门**前面**",
    ),
    "README_NGINX": cut(
        readme,
        "### 前面挡着 nginx 的话",
        "- `proxy_read_timeout` / `proxy_send_timeout`：流式的对话要传很久，默认 60s 不够。",
    ),
}

plan = u"""# 身体分两条道 —— 实现计划

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
{BODIES_TEST}
```

- [ ] **Step 2: 跑一遍，看它失败**

```bash
go test ./internal/gateway/ -run 'TestThePool|TestTheBodyLimits' -v
```

期望：`bodyPool` 没有定义，编译不过。

- [ ] **Step 3: 写实现**

`internal/gateway/bodies.go` —— 整个文件：

```go
{BODIES}
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
{CFG_TEST}
```

- [ ] **Step 2: 跑一遍，看它失败**

```bash
go test ./internal/config/ -run 'TestTheBodyLimits' -v
```

期望：`body_small_mb` 没有这个字段，编译不过。

- [ ] **Step 3: 写实现**

`internal/config/config.go` —— `Concurrency` 后面加两个键：

```go
{CONC_STRUCT}
```

`Default()` 里的并发那一段：

```go
{DEFAULT}
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
{FIELDS}
```

`New()` 里把它们建出来：

```go
{NEW}
```

- [ ] **Step 2: `handleChat` 在读身体之前过那两道门**

`internal/gateway/gateway.go`：

```go
{LANES}
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
{README_LANES}
```

- [ ] **Step 3: README 写上 nginx 挡在前面的时候那个上限**

`README.md` 的部署那一段后面：

```markdown
{README_NGINX}
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
"""

for k, v in blocks.items():
    plan = plan.replace("{" + k + "}", v)

out = os.path.join(REPO, "docs/superpowers/plans/2026-11-21-mmxcode-body-lanes.md")
io.open(out, "w", encoding="utf-8", newline="\n").write(plan)
print("written", out, len(plan), "bytes")
