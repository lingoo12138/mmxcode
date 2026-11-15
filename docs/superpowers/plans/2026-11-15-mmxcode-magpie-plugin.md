# magpie 插件（mmxcode） 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一个 magpie 插件（provider id `mmxcode`）：聊天走我们的网关（Anthropic 协议），余额卡片问网关要（一次请求答完），会话 id 传给网关，插件里没有 MiniMax 的账号和令牌。

**Architecture:** 一个 `packages/mmxcode/` 的包，按 magpie-community/plugins 的规矩写：package.json 指向 `index.mjs`，`index.mjs` 导出一个插件函数，钩子有 `config`（摆 provider 和兜底的名单）、`chat.headers`（把 magpie 的会话 id 交给网关）、`provider.models`（问网关的名单）、`auth`（签入、loader、usage）。网关那一侧的事在 `docs/superpowers/plans/2026-11-15-mmxcode-hot-usage.md` 里。

**Tech Stack:** Node（ESM，无依赖）、`node:test`、magpie 的插件宿主（1.x 的插件形状）

设计文档：`docs/superpowers/specs/2026-11-15-mmxcode-magpie-plugin-design.md`（§2、§10）

参考：`magpie-community/plugins` 的 `packages/minimax`（`index.mjs` 定行为）

---

## 三条必须守住的不变量

1. **插件里没有 MiniMax 的账号、令牌和签名。** 插件只认得两样东西：网关的地址，和一把聊天用的 key。它永远不跟 MiniMax 直接说话。
2. **卡片是网关的账，不是插件的账。** 插件原样转交网关答的那张卡（`plan` / `until` / `balance` / `windows`），不自己算，也不在卡上写读到的时间 —— 多出来的东西塞进去只会让 magpie 认错字段。
3. **查询都留在网关里。** magpie 里的每一个账号 = 我们的整个网关，所以卡片是一个请求，不是一个账号一个请求。magpie 自带的 minimax-code-global 插件是另一个 provider id，两个并存。

## magpie 那边的约定（照着它写，别猜）

这几条是 magpie 插件宿主（`magpie.exe` 里那段 JS）真的这么做的：

| 约定 | 是什么 |
|---|---|
| 入口 | `package.json` 的 `exports["./server"]`（没有就 `main`，再没有就 `index.mjs`）。宿主把那个文件导出的**函数**当插件。 |
| 插件形状 | 一个函数，返回 `{config, "chat.headers", provider:{id, models}, auth:{provider, methods, loader, usage}}`。 |
| `provider.models(provider, {auth})` | 返回模型的表（id → 行）。答不出来就返回 `provider.models`（原来那份）。 |
| `auth.methods` | 一条 `type:"api"` 的方法就是「让用户填一把 key」。magpie 把它问出来的那把 key 存成 `{type:"api", key}`。 |
| `auth.methods[].authorize(inputs)` | 我们要它的时候才写。有它的时候 magpie 调它，把**提示的答案**当 inputs 传进来，答案（inputs）会存进签到的 `metadata`。没有它的时候 magpie 自己问一把 key 就存起来。 |
| `auth.loader(getAuth)` | 返回 `{baseURL, apiKey, headers}`。`baseURL` 是 Anthropic SDK 的底座（SDK 自己接 `/messages`）。 |
| `auth.usage(getAuth)` | 返回卡片：`{plan?, until?, balance?, windows?[]}`。magpie 自己把这些字段归一化，多出来的字段它不看。 |
| `magpie.maxConcurrency` | package.json 里的一个数：**一个 magpie 账号同时几个请求**。我们一个账号 = 整个网关，所以它限的是"这个网关同时几个请求"。 |

---

### Task 1: 包的样子

**Files:**
- Create: `packages/mmxcode/package.json`
- Create: `packages/mmxcode/LICENSE`
- Create: `packages/mmxcode/README.md`

- [ ] **Step 1: package.json**

`packages/mmxcode/package.json`：

```json
{
  "name": "mmxcode-auth",
  "version": "0.1.0",
  "description": "magpie 的 provider 插件：聊天和余额卡片都走自己的 mmxcode 网关",
  "type": "module",
  "main": "./index.mjs",
  "exports": { "./server": "./index.mjs", ".": "./index.mjs" },
  "files": ["index.mjs", "README.md", "LICENSE"],
  "keywords": ["magpie", "magpie-plugin", "mmxcode", "minimax", "anthropic"],
  "license": "MIT",
  "magpie": { "maxConcurrency": 8 }
}
```

`maxConcurrency: 8` 是 magpie 眼里「一个账号同时几个请求」—— 我们一个账号就是整个网关，所以它限的是"这个网关同时几个请求"。真正的「每个 mcode 账号同时一个」在网关里做（网关那一侧的计划里有）。用户可以在 magpie 里改这个数，用户设的赢过插件声明的。

- [ ] **Step 2: LICENSE**

`packages/mmxcode/LICENSE`：

```
MIT License

Copyright (c) 2026 mmxcode

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

- [ ] **Step 3: README**

`packages/mmxcode/README.md`（中英文各一份）：

```markdown
# mmxcode-auth

把 magpie 接到自己的 **mmxcode 网关**上：聊天走网关（轮转、多账号、故障转移都在网关里），余额卡片问网关要（一次请求答完）。

## 它是什么

magpie 的 provider 插件，provider id 是 `mmxcode`。

插件里没有 MiniMax 的账号、令牌和签名 —— 那些全在网关里。插件只认得两样东西：

- **网关的地址**（默认 `http://127.0.0.1:8080`，也可以用环境变量 `MMXCODE_BASE` 指）；
- **一把聊天用的 key**（网关后台 `/keys` 申请的；网关一把 key 都没有的时候可以留空）。

## 怎么装

```sh
magpie plugin add ./packages/mmxcode
```

然后在 magpie 里签入 `MMXCode 网关`：填网关地址和聊天 key。

## 它跟 magpie 自带的 minimax-code-global 差在哪

| | minimax-code-global | mmxcode（这个） |
|---|---|---|
| 账号在哪 | magpie 自己拿着 | 网关里 |
| 余额卡片 | 逐个账号去问 MiniMax（账号一多就卡） | 问网关一次 |
| 签到 | magpie 里有按钮 | 没有（签到是网关自己的活） |
| 工具、搜索 | 有 | 没有 |

两个 provider 并存：`minimax-code-global` 还在，`mmxcode` 是另一个 id。要不要卸掉前者是用户的事。

## English

A magpie provider plugin that points magpie at **your own mmxcode gateway**: chat goes through the gateway (rotation, many accounts and failover live there), and the balance card is one request to the gateway.

The plugin holds no MiniMax account, token or signature — the gateway holds those. It knows two things: the gateway's address (default `http://127.0.0.1:8080`, or the `MMXCODE_BASE` environment variable) and a chat key (one you apply for in the gateway's console; leave it empty when the gateway has no keys at all).

```sh
magpie plugin add ./packages/mmxcode
```

It has no check-in button (checking in is the gateway's job) and no tools. It runs beside magpie's own `minimax-code-global`, under the provider id `mmxcode`.
```

- [ ] **Step 4: 提交**

```
git add packages/mmxcode
git commit -m "feat(plugin): 包的样子"
```

---

### Task 2: 插件本体

**Files:**
- Create: `packages/mmxcode/index.mjs`

- [ ] **Step 1: 写插件**

`packages/mmxcode/index.mjs`：

```js
// mmxcode: magpie 的 provider 插件，聊天和余额卡片都指向自己的网关。
//
// 插件里没有 MiniMax 的账号、令牌和签名 —— 那些都在网关里。插件只认
// 得两样东西：网关的地址，和一把聊天用的 key。
//
// 它跟 magpie 自带的 minimax-code-global 的分别：那个插件 magpie 自
// 己拿着账号的令牌去问 MiniMax，账号一多卡片就卡死；我们把查询都留
// 在网关里，卡片是一个请求。

const PROVIDER = "mmxcode"
const NPM = "@ai-sdk/anthropic"
const DEFAULT_BASE = "http://127.0.0.1:8080"
const BASE_ENV = "MMXCODE_BASE"
const KEY_ENV = "MMXCODE_KEY"

// The models the gateway is known to have, for when it cannot be asked.
//
// 兜底的那一份：网关答得出的时候用它自己的（`GET /v1/models`）。
const MODELS = [
  { id: "MiniMax-M3.1-Flash", name: "M3.1-Flash", context: 1048576, output: 524288, inputs: ["text", "image", "video"], thinking: "forced_on", efforts: ["low", "medium", "high", "xhigh", "max"] },
  { id: "MiniMax-M3", name: "M3", context: 1048576, output: 524288, inputs: ["text", "image", "video"], thinking: "switchable", efforts: [] },
]

// baseOf gives the gateway's address.
//
// 签入的时候问的地址存进了签到的 metadata；没有就问环境变量；再没有
// 就是本机的那个默认地址。末尾的斜杠去掉：底座后面还要接路径。
const baseOf = (a) => {
  const b = a?.base ?? a?.metadata?.base ?? process.env[BASE_ENV] ?? DEFAULT_BASE
  return String(b ?? "").replace(/\/+$/, "")
}

// keyOf gives the chat key.
//
// magpie 自己问用户要那把 key，存成签到的 key；签入的时候给的（提示里
// 的或环境变量里的）也算。
const keyOf = (a) => {
  const k = a?.metadata?.key ?? a?.key ?? process.env[KEY_ENV] ?? ""
  return typeof k === "string" ? k.trim() : ""
}

// headersFor gives the headers the gateway wants.
//
// 网关认 x-api-key，也认 Authorization: Bearer。magpie 的 Anthropic
// SDK 发的就是 x-api-key。
const headersFor = (key) => {
  const h = {}
  if (key) h["x-api-key"] = key
  return h
}

// rowOf gives one model as magpie's provider wants it.
//
// 网关的名单带元数据：context/output、能不能读图、思考档位。magpie 要
// 的就是这些。
function rowOf(m, base) {
  const inputs = Array.isArray(m.modalities?.input) && m.modalities.input.length ? m.modalities.input : ["text"]
  const efforts = Array.isArray(m.efforts) ? m.efforts : []
  const thinking = typeof m.thinking === "string" ? m.thinking : "off"
  const limit = m.limit ?? {}
  const variants = {}
  if (thinking === "switchable") variants.none = { thinking: { type: "disabled" } }
  if (efforts.length) {
    for (const e of efforts) variants[e] = { thinking: { type: "adaptive" }, effort: e }
  } else if (thinking === "switchable") {
    variants.high = { thinking: { type: "adaptive" } }
  }
  return {
    id: m.id,
    providerID: PROVIDER,
    name: m.name ?? m.id,
    api: { id: m.id, url: base + "/v1", npm: NPM },
    status: "active",
    headers: {},
    options: {},
    cost: { input: 0, output: 0, cache: { read: 0, write: 0 } },
    limit: { context: limit.context ?? 0, output: limit.output ?? 0 },
    capabilities: {
      temperature: true,
      reasoning: m.reasoning === true || thinking !== "off",
      attachment: inputs.length > 1,
      toolcall: true,
      input: {
        text: true,
        image: inputs.includes("image"),
        audio: inputs.includes("audio"),
        video: inputs.includes("video"),
        pdf: inputs.includes("pdf"),
      },
      output: { text: true, image: false, audio: false, video: false, pdf: false },
      interleaved: false,
    },
    release_date: "",
    variants,
  }
}

// rowsOf gives the built-in models as rows.
function rowsOf(models, base) {
  const out = {}
  for (const m of models) {
    out[m.id] = rowOf(
      {
        id: m.id,
        name: m.name,
        limit: { context: m.context, output: m.output },
        modalities: { input: m.inputs },
        reasoning: m.thinking !== "off",
        thinking: m.thinking,
        efforts: m.efforts,
      },
      base,
    )
  }
  return out
}

// modelsOf asks the gateway for its model list.
//
// 答不出来的时候是空的：调用方摆兜底的那一份。
async function modelsOf(base, key) {
  try {
    const res = await globalThis.fetch(base + "/v1/models", { headers: headersFor(key) })
    if (!res.ok) return []
    const body = await res.json()
    return Array.isArray(body?.data) ? body.data : []
  } catch {
    return []
  }
}

// authorize tries the gateway once, so that a sign-in that cannot work
// says why.
//
// 有 key 而网关不认，是 key 的问题。没有 key 而网关说 401，不是问
// 题：那是说"我要一把 key"，而那把 key 是 magpie 自己问用户要的那一
// 把。
async function authorize(base, key) {
  try {
    const res = await globalThis.fetch(base + "/v1/models", { headers: headersFor(key) })
    if (res.ok) return { type: "success", key: key || undefined, metadata: { base } }
    if (res.status === 401 || res.status === 403) {
      if (key) {
        return { type: "failed", error: "网关拒绝了这把 key —— 它开的是哪扇门？聊天接口要聊天 key，后台登录 key 也行" }
      }
      return { type: "success", key: undefined, metadata: { base } }
    }
    return { type: "failed", error: `网关答了 HTTP ${res.status}` }
  } catch (e) {
    return { type: "failed", error: `网关连不上：${base}（${e?.message ?? e}）` }
  }
}

// MMXCodePlugin is the plugin.
//
// 它是一个导出的异步函数：magpie 的宿主调它一次，拿到钩子。
export const MMXCodePlugin = async ({ client, magpie } = {}) => {
  return {
    // the provider, its package and its models
    config: async (config) => {
      config.provider ??= {}
      config.provider[PROVIDER] ??= {}
      const p = config.provider[PROVIDER]
      p.name ??= "MMXCode"
      // 网关讲 Anthropic 的 /v1/messages
      p.npm ??= NPM
      p.api ??= DEFAULT_BASE + "/v1"
      p.models = { ...rowsOf(MODELS, DEFAULT_BASE), ...(p.models ?? {}) }
    },

    // the conversation's id, which the gateway binds to an account
    "chat.headers": async (input, output) => {
      const id = input?.model?.providerID ?? input?.provider?.info?.id
      if (id !== PROVIDER || !input?.sessionID) return
      // 网关认的头是 X-Session-Id / X-Conversation-Id / X-Session-Id-Codex
      // / Session-Id，第一个优先。有了它，一段对话黏在一个账号上。
      output.headers["X-Session-Id"] = input.sessionID
    },

    provider: {
      id: PROVIDER,
      // the gateway's own list, when it answers; else the list above
      async models(provider, { auth } = {}) {
        const base = baseOf(auth)
        const list = await modelsOf(base, keyOf(auth))
        if (!list.length) return provider.models
        const rows = {}
        for (const m of list) rows[m.id] = rowOf(m, base)
        return { ...provider.models, ...rows }
      },
    },

    auth: {
      provider: PROVIDER,
      methods: [
        {
          // magpie 自己问那把 key：它是网关后台申请的那一把。
          //
          // 我们也写 authorize：签入的时候要打一次网关，说清它在不在、
          // 它收不收。authorize 拿得到的是提示的答案（地址），拿不到
          // magpie 问出来的那把 key —— 所以 401 在没有 key 可试的时候
          // 不算失败。
          type: "api",
          label: "MMXCode 网关",
          placeholder: "sk-…（网关一把 key 都没有时可以留空）",
          async authorize(inputs = {}) {
            const base = baseOf(inputs)
            const key = keyOf(inputs)
            return await authorize(base, key)
          },
        },
      ],

      // what each request goes out with
      async loader(getAuth) {
        const auth = await getAuth()
        const base = baseOf(auth)
        const key = keyOf(auth)
        return {
          // The Anthropic SDK appends /messages to it.
          baseURL: base + "/v1",
          apiKey: key,
          headers: headersFor(key),
        }
      },

      // the gateway's card: the credits and the plan of every account
      // behind it, in one answer
      async usage(getAuth) {
        const auth = await getAuth()
        const base = baseOf(auth)
        const key = keyOf(auth)
        try {
          // refresh=1 是要现读。网关那一侧只现读最热的几个，有预算，
          // 所以卡片不会等。
          const res = await globalThis.fetch(base + "/v1/usage?refresh=1", { headers: headersFor(key) })
          if (!res.ok) return { error: `网关答了 HTTP ${res.status}` }
          return await res.json()
        } catch (e) {
          return { error: e?.message ?? String(e) }
        }
      },
    },
  }
}

export default MMXCodePlugin
```

- [ ] **Step 2: 提交**

```
git add packages/mmxcode/index.mjs
git commit -m "feat(plugin): 插件本体"
```

---

### Task 3: 钩子的测试

**Files:**
- Create: `packages/mmxcode/index.test.mjs`

- [ ] **Step 1: 写测试**

`packages/mmxcode/index.test.mjs`：

```js
import test from "node:test"
import assert from "node:assert/strict"
import http from "node:http"

import { MMXCodePlugin } from "./index.mjs"

// aGateway starts a fake gateway and gives its address.
async function aGateway(t, handler) {
  const srv = http.createServer(handler)
  await new Promise((r) => srv.listen(0, "127.0.0.1", r))
  t.after(() => new Promise((r) => srv.close(r)))
  return `http://127.0.0.1:${srv.address().port}`
}

// thePlugin gives a loaded plugin.
async function thePlugin() {
  return await MMXCodePlugin({})
}

// TestTheSignInSaysTheGatewayIsNotThere says an unreachable gateway is
// said in words.
//
// 签入不成功要说清楚，不留一个半签入的账号。
test("the sign-in says the gateway is not there", async () => {
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base: "http://127.0.0.1:1" })
  assert.equal(res.type, "failed")
  assert.match(res.error, /网关连不上/)
})

// TestTheSignInSaysTheKeyWasRefused says a refused key is said in
// words.
//
// 网关开的是哪扇门：聊天接口要聊天 key，后台登录 key 也行。
test("the sign-in says the key was refused", async () => {
  const base = await aGateway(t, (w, r) => {
    w.writeHead(401)
    w.end("{}")
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base, key: "sk-wrong" })
  assert.equal(res.type, "failed")
  assert.match(res.error, /网关拒绝了这把 key/)
})

// TestTheSignInSaysTheOtherAnswers says an unexpected answer is said
// as it is.
test("the sign-in says the other answers", async () => {
  const base = await aGateway(t, (w, r) => {
    w.writeHead(503)
    w.end("{}")
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "failed")
  assert.match(res.error, /HTTP 503/)
})

// TestTheSignInKeepsTheAddress says a sign-in that worked keeps the
// address.
//
// 地址是签到的一部分：没有它，插件不知道往哪儿发请求。
test("the sign-in keeps the address", async () => {
  const base = await aGateway(t, (w, r) => {
    w.writeHead(200, { "content-type": "application/json" })
    w.end(JSON.stringify({ object: "list", data: [] }))
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "success")
  assert.equal(res.metadata.base, base)
})

// TestTheSignInWithoutAKeyIsFine says a gateway that wants a key is
// not a failed sign-in.
//
// 那把 key 是 magpie 自己问用户要的：它还没有，所以 401 不是失败。
test("the sign-in without a key is fine", async () => {
  const base = await aGateway(t, (w, r) => {
    w.writeHead(401)
    w.end("{}")
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base })
  assert.equal(res.type, "success")
})

// TestTheCardIsHandedOverAsItIs says the card comes from the gateway
// as it is.
//
// 插件不自己算，也不在卡上写读到的时间：多出来的东西塞进去只会让
// magpie 认错字段。
test("the card is handed over as it is", async () => {
  const card = {
    plan: "Token Plan M3",
    until: "2026-12-01T00:00:00Z",
    balance: "437.5 credits",
    windows: [{ name: "主号 · Credits", used: 12.5, display: "62.5 / 500", amount: 62.5, limit: 500, unit: "credits" }],
  }
  const base = await aGateway(t, (w, r) => {
    w.writeHead(200, { "content-type": "application/json" })
    w.end(JSON.stringify(card))
  })
  const plugin = await thePlugin()
  const got = await plugin.auth.usage(async () => ({ type: "api", key: "sk-x", metadata: { base } }))
  assert.equal(got.plan, card.plan)
  assert.equal(got.balance, card.balance)
  assert.equal(got.windows.length, 1)
  assert.equal(got.windows[0].name, card.windows[0].name)
})

// TestTheModelsAreTheGateways says the models are the gateway's own,
// with what magpie needs to know about them.
test("the models are the gateway's", async () => {
  const base = await aGateway(t, (w, r) => {
    w.writeHead(200, { "content-type": "application/json" })
    w.end(
      JSON.stringify({
        object: "list",
        data: [
          {
            id: "MiniMax-M3.1-Flash",
            limit: { context: 1048576, output: 524288 },
            modalities: { input: ["text", "image"], output: ["text"] },
            reasoning: true,
            thinking: "forced_on",
            efforts: ["low", "high"],
          },
        ],
      }),
    )
  })
  const plugin = await thePlugin()
  const provider = { id: "mmxcode", models: {} }
  const got = await plugin.provider.models(provider, { auth: { type: "api", key: "sk-x", metadata: { base } } })
  const m = got["MiniMax-M3.1-Flash"]
  assert.ok(m, "the model is not in the list")
  assert.equal(m.limit.context, 1048576)
  assert.equal(m.limit.output, 524288)
  assert.equal(m.capabilities.reasoning, true)
  assert.equal(m.capabilities.input.image, true)
  assert.equal(m.api.npm, "@ai-sdk/anthropic")
  assert.equal(m.api.url, base + "/v1")
  assert.ok(m.variants.low, "the reasoning levels are not said")
})

// TestTheModelsFallBackWhenTheGatewayIsSilent says the built-in list
// is used when the gateway cannot be asked.
//
// 跟参考插件一个做法：magpie 手里那份名单留着。
test("the models fall back when the gateway is silent", async () => {
  const base = await aGateway(t, (w, r) => {
    w.writeHead(500)
    w.end("{}")
  })
  const plugin = await thePlugin()
  const provider = { id: "mmxcode", models: { "MiniMax-M3.1-Flash": { id: "MiniMax-M3.1-Flash" } } }
  const got = await plugin.provider.models(provider, { auth: { type: "api", key: "sk-x", metadata: { base } } })
  assert.deepEqual(Object.keys(got), ["MiniMax-M3.1-Flash"])
})

// TestTheConfigPutsTheProvider says the config hook puts the provider
// there, with the built-in list.
test("the config puts the provider there", async () => {
  const plugin = await thePlugin()
  const config = {}
  await plugin.config(config)
  const p = config.provider.mmxcode
  assert.ok(p, "the provider is not there")
  assert.equal(p.npm, "@ai-sdk/anthropic")
  assert.equal(p.api, "http://127.0.0.1:8080/v1")
  assert.ok(p.models["MiniMax-M3.1-Flash"], "the built-in list is not there")
})

// TestTheSessionIdIsGivenToTheGateway says the conversation's id goes
// to the gateway.
//
// 有了它，一段对话黏在一个账号上；没有它，两个用户的第一句话一样就
// 会撞成一个会话。
test("the session id is given to the gateway", async () => {
  const plugin = await thePlugin()
  const out = { headers: {} }
  await plugin["chat.headers"]({ model: { providerID: "mmxcode" }, sessionID: "sess-1" }, out)
  assert.equal(out.headers["X-Session-Id"], "sess-1")
})

// TestTheSessionIdIsOnlyForOurProvider says another provider's request
// is left alone.
test("the session id is only for our provider", async () => {
  const plugin = await thePlugin()
  const out = { headers: {} }
  await plugin["chat.headers"]({ model: { providerID: "somebody-else" }, sessionID: "sess-1" }, out)
  assert.equal(out.headers["X-Session-Id"], undefined)
})

// TestTheLoaderGivesTheGateway says the loader points at the gateway
// with the key.
test("the loader gives the gateway", async () => {
  const plugin = await thePlugin()
  const got = await plugin.auth.loader(async () => ({
    type: "api",
    key: "sk-x",
    metadata: { base: "http://127.0.0.1:9999" },
  }))
  assert.equal(got.baseURL, "http://127.0.0.1:9999/v1")
  assert.equal(got.apiKey, "sk-x")
  assert.equal(got.headers["x-api-key"], "sk-x")
})
```

- [ ] **Step 2: 运行测试确认失败**

```
cd packages/mmxcode && node --test index.test.mjs
```

失败：`Cannot find module './index.mjs'`（Task 2 之前跑）。

- [ ] **Step 3: 运行测试确认通过**

```
cd packages/mmxcode && node --test index.test.mjs
```

全过。

- [ ] **Step 4: 提交**

```
git add packages/mmxcode/index.test.mjs
git commit -m "test(plugin): 钩子的测试"
```

---

### Task 4: 装到 magpie 上看

**Files:**
- Modify: `README.md`（仓库根目录那份）

- [ ] **Step 1: 本机装一下**

网关先起在本机：

```
MMXCODE_API_KEYS=sk-admin ./mmxcode.exe --config config.yaml --db ./mmxcode.db serve
```

然后：

```
magpie plugin add ./packages/mmxcode
```

在 magpie 里签入 `MMXCode 网关`：地址填 `http://127.0.0.1:8080`，key 填 `sk-admin`（或者网关一把 key 都没有的时候留空）。

- [ ] **Step 2: 看三样东西**

1. **模型**：magpie 的模型列表里有 `mmxcode` 的模型，context 和 output 跟网关 `/v1/models` 给的一样。
2. **聊天**：选一个 `mmxcode` 的模型聊两句。网关的日志里看得到请求带上了 `X-Session-Id`（网关把它黏在一个账号上）。
3. **卡片**：magpie 的 Providers 页里 `mmxcode` 的卡片几秒内出来（网关的 `/v1/usage?refresh=1` 只现读最热的几个，有预算）。

- [ ] **Step 3: 写清楚怎么装**

仓库根目录的 `README.md` 里加一节「magpie 插件」，写：

- 插件在 `packages/mmxcode/`，`magpie plugin add ./packages/mmxcode`；
- 签入要两样东西：网关地址和聊天 key；
- 它跟 magpie 自带的 `minimax-code-global` 的分别（查询留在网关里，不会卡）；
- 网关那一侧要配的东西（`usage.refresh.*` 和 `concurrency.*`）。

- [ ] **Step 4: 提交**

```
git add README.md
git commit -m "docs(readme): magpie 插件"
```

---

## 验证

```
cd packages/mmxcode && node --test index.test.mjs
node --check packages/mmxcode/index.mjs
```

本机（不起 docker）：网关起在本机，`magpie plugin add ./packages/mmxcode`，签入，magpie 里选 `mmxcode` 的模型聊几句，看 Providers 页的卡片。

**卡死这件事要看得见**：多账号时对比 global 插件和我们的插件在卡片上的反应 —— global 是逐个账号去问 MiniMax 的，账号一多就卡；我们是问网关一次。
