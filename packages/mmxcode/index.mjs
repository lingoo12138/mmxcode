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
// 空着的地址算是没有：提示里写着默认值，答个空就用它。
const baseOf = (a) => {
  const b = a?.base ?? a?.metadata?.base ?? process.env[BASE_ENV] ?? DEFAULT_BASE
  const s = String(b ?? "").trim()
  return (s || DEFAULT_BASE).replace(/\/+$/, "")
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

// variantsOf are a model's reasoning levels, as @ai-sdk/anthropic options.
//
// "none" 是把思考关掉（模型自己说关得掉的时候）；其余的是思考的档位。
function variantsOf(thinking, efforts) {
  const out = {}
  if (thinking === "switchable") out.none = { thinking: { type: "disabled" } }
  if (efforts.length) {
    for (const e of efforts) out[e] = { thinking: { type: "adaptive" }, effort: e }
  } else if (thinking === "switchable") {
    out.high = { thinking: { type: "adaptive" } }
  }
  return out
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
  const variants = variantsOf(thinking, efforts)
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

// configRowOf gives one model as magpie's config wants it.
//
// The config hook's models are a smaller shape than the rows the models
// hook gives: the host reads the names it knows (reasoning, tool_call,
// attachment, modalities, limit, cost, variants) and fills the rest in
// itself. A full row loses them.
//
// 配的是配置形状：宿主照着这些名字读（reasoning、tool_call、
// attachment、modalities、limit、cost、variants），剩下的它自己补。塞
// 一整行进去，这些名字就没人读了 —— 图读不了、思考也没有。
function configRowOf(m) {
  const inputs = Array.isArray(m.inputs) && m.inputs.length ? m.inputs : ["text"]
  const efforts = Array.isArray(m.efforts) ? m.efforts : []
  return {
    name: m.name,
    reasoning: m.thinking !== "off",
    tool_call: true,
    attachment: inputs.length > 1,
    modalities: { input: inputs, output: ["text"] },
    limit: { context: m.context ?? 0, output: m.output ?? 0 },
    cost: { input: 0, output: 0, cache_read: 0, cache_write: 0 },
    variants: variantsOf(m.thinking, efforts),
  }
}

// configRowsOf gives the built-in models as config entries.
function configRowsOf(models) {
  const out = {}
  for (const m of models) out[m.id] = configRowOf(m)
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
// 题：那是说"我要一把 key"，而那把 key 是可以没有的（网关一把 key
// 都没有的时候它自己认）。
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
      p.models = { ...configRowsOf(MODELS), ...(p.models ?? {}) }
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
          // We ask for the address and the key ourselves.
          //
          // magpie 自己不问 key（keyless），我们问：网关的地址和聊天用的
          // 那把 key 都是提示的答案，authorize 拿得到，才谈得上"打一次
          // 网关，说清它在不在、它收不收"。没有提示，authorize 拿到的
          // 永远是空对象，地址只能从环境变量里来。
          type: "api",
          label: "MMXCode 网关",
          keyless: true,
          placeholder: "sk-…（网关一把 key 都没有的时候可以留空）",
          prompts: [
            { type: "text", key: "base", message: "网关地址（默认 http://127.0.0.1:8080）" },
            { type: "password", key: "key", required: false, message: "聊天用的 key（后台 /keys 申请的；网关一把 key 都没有的时候可以留空）" },
            { type: "text", key: "label", required: false, message: "这个名字（magpie 里认这个账号用）" },
          ],
          async authorize(inputs = {}) {
            const base = baseOf(inputs)
            const key = keyOf(inputs)
            const label = typeof inputs?.label === "string" ? inputs.label.trim() : ""
            const res = await authorize(base, key)
            if (res.type !== "success") return res
            // magpie names an account by accountId, else by metadata.email:
            // the label the user gave is the name it will know it by.
            // magpie 给账号起名字用的是 accountId，没有就 metadata.email
            // —— 用户给的那个名字得落在 email 上，账号才认得出来。
            return { ...res, metadata: { ...res.metadata, ...(label ? { email: label } : {}) } }
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
