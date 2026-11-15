import test from "node:test"
import assert from "node:assert/strict"
import http from "node:http"

import { MMXCodePlugin } from "./index.mjs"

// aGateway starts a fake gateway and gives its address.
//
// 关的时候要把还开着的连接掐掉：fetch 的连接是留着的，不掐的话
// close() 等着它们，这个测试就挂着不走了。
async function aGateway(t, handler) {
  const srv = http.createServer(handler)
  await new Promise((r) => srv.listen(0, "127.0.0.1", r))
  t.after(() => new Promise((r) => {
    srv.closeAllConnections()
    srv.close(r)
  }))
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
test("the sign-in says the key was refused", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(401)
    res.end("{}")
  })
  const plugin = await thePlugin()
  const res = await plugin.auth.methods[0].authorize({ base, key: "sk-wrong" })
  assert.equal(res.type, "failed")
  assert.match(res.error, /网关拒绝了这把 key/)
})

// TestTheSignInSaysTheOtherAnswers says an unexpected answer is said
// as it is.
test("the sign-in says the other answers", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(503)
    res.end("{}")
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
test("the sign-in keeps the address", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify({ object: "list", data: [] }))
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
test("the sign-in without a key is fine", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(401)
    res.end("{}")
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
test("the card is handed over as it is", async (t) => {
  const card = {
    plan: "Token Plan M3",
    until: "2026-12-01T00:00:00Z",
    balance: "437.5 credits",
    windows: [{ name: "主号 · Credits", used: 12.5, display: "62.5 / 500", amount: 62.5, limit: 500, unit: "credits" }],
  }
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify(card))
  })
  const plugin = await thePlugin()
  const got = await plugin.auth.usage(async () => ({ type: "api", key: "sk-x", metadata: { base } }))
  assert.equal(got.plan, card.plan)
  assert.equal(got.balance, card.balance)
  assert.equal(got.windows.length, 1)
  assert.equal(got.windows[0].name, card.windows[0].name)
  // 卡片是网关那张：插件一个字也不加。
  assert.deepEqual(got, card)
})

// TestTheModelsAreTheGateways says the models are the gateway's own,
// with what magpie needs to know about them.
test("the models are the gateway's", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(
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
test("the models fall back when the gateway is silent", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(500)
    res.end("{}")
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

// TestTheConfigModelsAreConfigShaped says the config hook's models are
// the shape the host reads, not the whole provider row.
//
// 宿主照着配置形状里的名字读：reasoning、tool_call、attachment、
// modalities、limit、cost、variants。塞一整行进去，这些名字就没人读
// 了 —— 图读不了、思考也没有。参考插件的 configModels() 也是这样
// （minimax/index.mjs:203-220），它那 whole row 是 modelOf()
// （:222-246），走的是另一个钩子。
test("the config's models are in the shape the host reads", async () => {
  const plugin = await thePlugin()
  const config = {}
  await plugin.config(config)
  const m = config.provider.mmxcode.models["MiniMax-M3.1-Flash"]
  assert.ok(m, "the model is not in the config")
  assert.equal(m.name, "M3.1-Flash")
  assert.equal(m.reasoning, true)
  assert.equal(m.tool_call, true)
  assert.equal(m.attachment, true)
  assert.deepEqual(m.modalities, { input: ["text", "image", "video"], output: ["text"] })
  assert.deepEqual(m.limit, { context: 1048576, output: 524288 })
  assert.deepEqual(m.cost, { input: 0, output: 0, cache_read: 0, cache_write: 0 })
  assert.ok(m.variants.low, "the reasoning levels are not said")
  assert.deepEqual(m.variants.low, { thinking: { type: "adaptive" }, effort: "low" })
  // 不是 provider 的整行：整行里那些名字宿主不读。
  assert.equal(m.id, undefined)
  assert.equal(m.api, undefined)
  assert.equal(m.capabilities, undefined)
})

// TestTheConfigModelsSayTheThinkingThatCanBeTurnedOff says a model
// whose thinking can be turned off has that said.
//
// 思考能关的模型有 none 这一档，能关才关得掉。
test("the config's models say the thinking that can be turned off", async () => {
  const plugin = await thePlugin()
  const config = {}
  await plugin.config(config)
  const m = config.provider.mmxcode.models["MiniMax-M3"]
  assert.ok(m, "the model is not in the config")
  assert.deepEqual(m.variants.none, { thinking: { type: "disabled" } })
  assert.ok(m.variants.high, "the reasoning levels are not said")
  // 强制开着的模型没有 none：那档它做不到。
  const flash = config.provider.mmxcode.models["MiniMax-M3.1-Flash"]
  assert.equal(flash.variants.none, undefined)
})

// TestTheSignInAsksForTheAddressAndTheKey says the sign-in method asks
// for the gateway's address and the chat key itself.
//
// magpie 调 authorize(inputs) 时给的是提示的答案：没有提示，地址就
// 只能从环境变量里来，用户连网关在哪儿都答不了。所以这方法是
// keyless 的，提示里必须有地址。
test("the sign-in asks for the address and the key", async () => {
  const plugin = await thePlugin()
  const method = plugin.auth.methods[0]
  assert.equal(method.keyless, true)
  assert.ok(Array.isArray(method.prompts), "the sign-in asks for nothing")
  const base = method.prompts.find((p) => p.key === "base")
  assert.ok(base, "the gateway's address is not asked for")
  assert.match(base.message, /网关地址/)
  const key = method.prompts.find((p) => p.key === "key")
  assert.ok(key, "the chat key is not asked for")
  // 网关一把 key 都没有的时候它自己认：这把 key 可以不填。
  assert.equal(key.required, false)
})

// TestTheSignInTakesTheAnswers says the sign-in takes the answers to
// its prompts.
//
// 提示的答案就是 authorize 的入参：地址在 base 上，key 在 key 上。
// 少了这一条，签到就不知道该往哪儿打。
test("the sign-in takes the answers to its prompts", async (t) => {
  const base = await aGateway(t, (req, res) => {
    res.writeHead(200, { "content-type": "application/json" })
    res.end(JSON.stringify({ object: "list", data: [] }))
  })
  const plugin = await thePlugin()
  const method = plugin.auth.methods[0]
  const answers = {}
  for (const p of method.prompts) answers[p.key] = p.key === "base" ? base : ""
  answers.key = "sk-x"
  const res = await method.authorize(answers)
  assert.equal(res.type, "success")
  assert.equal(res.metadata.base, base)
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
