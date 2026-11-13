# mmxcode-gateway 设计文档

日期：2026-11-13
状态：待评审

## 1. 目标与范围

### 1.1 一句话

用 Go 实现一个**不依赖 magpie** 的 MiniMax Code (Global) 多账号网关：登录、多账号管理、每日签到、账号轮转，对外提供 Anthropic / OpenAI / Codex 三种协议，并带 Web 管理界面和 Docker 部署。

上游行为对齐社区插件 [`@magpie-community/opencode-minimax-auth`](https://github.com/magpie-community/plugins/blob/main/packages/minimax/README.md)（下称"参考插件"）的协议实现。

### 1.2 范围内

- MiniMax Code 国际站（`minimax.io`）的账号登录（OAuth 2 device flow + PKCE）
- 多账号存储、管理、状态跟踪
- 每日签到（定时自动 + 手动触发）
- 账号轮转：粘性会话绑定 + 配额耗尽故障转移
- HTTP 网关：`/v1/messages`（Anthropic）、`/v1/chat/completions`（OpenAI）、`/v1/responses`（Codex）
- 用量聚合接口 `GET /api/usage`
- 代理出口：账号级 / 出口组 / 全局三级（对齐 magpie 的代理模型），支持 Cloudflare WARP 作为额外出口（默认关闭，Docker 配置开启）
- Web 管理界面（服务端渲染）
- CLI + Docker 部署

### 1.3 范围外（明确不做）

- 中国站（`minimax.cn` / `minimaxi.com`）
- Matrix 工具：图片/视频/语音生成、`web_search`、图像理解等
- magpie 插件（不写 JS 适配层，不接入 magpie 的 Usage 卡片）
- 图形验证码处理（上游若返回验证码状态，原样透传为 `captcha`）
- 账号签出（删除本地记录即可，不吊销 MiniMax 侧 token，与参考插件一致）
- 真实账号的自动化集成测试（refresh token 一次性，跑两次会废掉账号）

### 1.4 与参考插件的对应关系

参考插件是 JS 实现的 OpenCode/magpie provider 插件。本项目用 Go 网关替代它，承担全部协议、凭据与轮转逻辑；magpie 侧不接入。

协议细节（端点、签名、请求头、错误语义）以参考插件的 `packages/minimax/index.mjs` 为准，本文档逐条记录。

## 2. 站点与端点

只支持国际站（Global）。参考插件的 `SITES["minimax-code-global"]`：

| 用途 | Host |
|---|---|
| OAuth 登录 | `account.minimax.io` |
| 聊天 + 模型列表 | `agent.minimax.io` |
| 账户 API + 签到 + 积分账本 | `agent.minimax.io` |
| 套餐控制台 | `platform.minimax.io` |

端点：

| 功能 | 路径 | 方法 |
|---|---|---|
| 申请设备码 | `{account}/oauth2/device/code` | POST form |
| 换取 token（设备码 / 刷新） | `{account}/oauth2/token` | POST form |
| 聊天（Anthropic） | `{llm}/mavis/api/v1/llm/v1/messages` | POST |
| 聊天（OpenAI） | `{llm}/mavis/api/v1/llm/v1/chat/completions` | POST |
| 聊天（Codex Responses） | `{llm}/mavis/api/v1/llm/v1/responses` | POST |
| 模型列表 | `{llm}/mavis/api/v1/models?region=en&buildEnv=prod` | GET |
| 账户身份 | `{agent}/v1/api/user/info` | GET |
| 工作区信息 | `{agent}/matrix/api/v1/user/get_user_extra_info` | POST |
| 会员信息 | `{agent}/matrix/api/v1/commerce/get_membership_info` | POST |
| 积分账本 | `{agent}/minimax-cloud/api/v1/credit/details` | GET |
| 签到面板 | `{agent}/minimax-cloud/api/v1/signin/status` | GET |
| 签到领取 | `{agent}/minimax-cloud/api/v1/signin/claim` | POST |
| 套餐窗口（首选） | `{platform}/backend/account/token_plan/remains_percent` | GET |
| 套餐窗口（回退） | `{platform}/v1/api/openplatform/coding_plan/remains` | GET |
| 套餐名称 | `{platform}/v1/api/openplatform/charge/combo/cycle_audio_resource_package?...` | GET |

**国际站注意事项**：参考插件实测国际站对 `get_user_extra_info` 与 `get_membership_info` 两个会员接口都返回 404（#666），因此套餐信息在国际站属于"未知"，不是"Free"。`/api/usage` 与 WebUI 必须能表达"套餐未知"这一状态。

## 3. 架构

### 3.1 模块划分

```
mmxcode-api/
├── cmd/mmxcode/main.go         单一入口，cobra 子命令
├── internal/
│   ├── site/       站点常量、端点表、内置模型表（纯常量，无 IO）
│   ├── sign/       MiniMax 请求签名：yy / x-signature（纯函数）
│   ├── oauth/      device flow 登录、token 解析、刷新与单飞保护
│   ├── account/    账户 API 客户端：identity / membership / 积分账本 / 套餐窗口
│   ├── checkin/    签到：读面板 → 判定 → claim → 结果归一
│   ├── store/      SQLite 存储层
│   ├── egress/     出口解析与 HTTP client 缓存（账号级代理、组、健康检查）
│   ├── rotator/    粘性会话 + 配额故障转移（只做决策，不发 HTTP）
│   ├── gateway/    三协议 HTTP 路由、上游请求改写、重放
│   ├── upstream/   上游 HTTP 客户端：重定向策略、超时、业务错误码检查
│   ├── webui/      服务端渲染页面 + SSE
│   └── scheduler/  定时任务（自动签到、token 续期、用量刷新、出口健康检查）
├── web/            templates + static
├── Dockerfile / docker-compose.yml / config.example.yaml
└── docs/superpowers/specs/
```

**依赖方向**：`gateway → rotator → oauth/account → site/sign → store`。`store` 不依赖上层；`rotator` 只决定"用哪个账号、是否允许重放"，重放由 `gateway` 执行，因此轮转逻辑可以脱离网络测试。

### 3.2 边界约定

- `site`：只有常量和纯函数。换站、改端点、调模型表只动这里。
- `sign`：纯函数（path/query/body/timestamp → headers），无 IO，可对照参考插件的 JS 实现造固定向量单测。
- `rotator`：接口为 `Acquire(sessionKey, model) → (account, permit)`，`permit` 表示是否允许重放。不认识 HTTP。
- `gateway`：唯一的重放执行者，也是唯一给上游加认证头的层。

## 4. 登录（OAuth device flow）

### 4.1 流程

1. 生成 32 字节随机 `verifier`（base64url），`challenge = base64url(sha256(verifier))`
2. POST `{account}/oauth2/device/code`，form：`client_id=mcode-public`、`scope=agent.default`、`audience=agent-backend`、`code_challenge=<challenge>`、`code_challenge_method=S256`
3. 响应取 `user_code`、`device_code`、`verification_uri` / `verification_url`、`verification_uri_complete`、`expires_in` / `expired_in`、`interval`
4. 向用户展示 user_code 与链接，轮询 `{account}/oauth2/token`，form：`grant_type=urn:ietf:params:oauth:grant-type:device_code`、`device_code`（或 `user_code`）、`client_id`、`code_verifier`

**MiniMax 变体**：响应里可能没有 `device_code`，而是用 `user_code` 轮询、且 `expired_in` 可能是绝对毫秒时间戳。两 种形式都要支持，由 `expired_in` 是否存在且 `device_code` 是否缺失来判断。

**轮询语义**：
- `authorization_pending` / `status=pending` → 继续等
- `slow_down` / `status=slow_down` → 间隔 +5s
- `denied` / `access_denied` → `access_denied`
- `expired` / `expired_token` → `expired_token`
- 成功 → 解析 token

**重试**：仅传输层错误和 5xx/408/429 重试，最多 3 次，指数退避（1s → 2s → 4s，上限 30s）。`invalid_grant` / `access_denied` / `expired_token` / `invalid_client` / `unauthorized_client` 不重试。刷新 token 的请求**永不重试**（可能已经花掉）。

### 4.2 token 解析

响应体取 `access_token`、`expires_in`、`refresh_token`。access token 是 JWT，从中解出 `sub`（MiniMax user id）和 `account_id` 作为账号身份线索。

### 4.3 token 刷新

**核心约束：refresh token 一次性。** 第二次使用返回 `invalid_grant` + HTTP 400，账号被判登出。

因此：

- **单飞**：每个账号一把锁，同一时刻只允许一个刷新在跑，后来者等待并复用结果。
- **刷新链**：刷新结果按"旧 refresh token → 新 token"缓存，沿链可以取到最新的一代，而不是只记最后一次。
- **落盘优先**：拿到新 token 后先写 SQLite，再发布到内存。写盘失败时**不能**退回旧 token（旧 token 已作废）；正确做法是保留新 token 在内存继续服务，后台按退避重试落盘，并把该账号标为"待持久化"在 WebUI 告警。（参考插件只存在内存里，进程一挂该账号就要重登；我们做得更严。）
- **登出判定**：仅 `invalid_grant` + HTTP 400 视为真登出，标记 `signed_out`。
  - 但若 DB 中当前的 refresh token 与本次使用的不是同一个，说明已被别处（CLI 又登录了一次 / 另一个进程）先行消费，**不算登出**，改用 DB 中的新 token 继续。
  - 5xx / 超时 / 网络错误**不算登出**，token 未过期则继续使用。
- **提前续期**：调度任务每 5 分钟扫描，距过期 10 分钟以上即刷新，保证不会在请求中途过期；请求侧另有一道 2 分钟的检查。

## 5. 签名

参考插件的 `signed()`：

- 查询串固定字段：`device_platform=mcode`、`biz_id=3`、`app_id=3001`、`version_code=22201`、`unix=<毫秒>`、`timezone_offset=<秒>`、`sys_language=en`、`lang=en`、`device_id=0`、`os_name=<platform>`、`browser_name=mcode`、`user_id=<realUserID 或 0>`、`client=mcode`
- 待签名串 `at = path + "?" + query`
- `yy`：
  - GET（无 body）：`md5(urlEncode(at) + "_" + "{}" + md5(unix) + "ooui")`
  - POST：`md5(urlEncode(at) + "_" + body + md5(unix) + "ooui")`
- `x-timestamp` = `unix / 1000`
- `x-signature`：
  - GET：`md5(timestamp + "I*7Cf%WZ#S&%1RlZJ&C2")`
  - POST：`md5(timestamp + "I*7Cf%WZ#S&%1RlZJ&C2" + body)`
- 请求头另有：`Accept: application/json`、`Content-Type: application/json`、`User-Agent: MiniMaxCode`、`Authorization: Bearer <access>`

签到与积分账本使用 `device_platform=web`、`is_desktop=1`、`desktop_version=0.6.12`（覆盖默认的 `device_platform=mcode`），其余签名逻辑不变。

**幂等性**：签名含时间戳与 unix，重放同一请求会产生不同签名，因此所有带签名的请求不可重试到不同的账号——只有聊天请求（无签名）允许换号重放。

## 6. 聊天请求

### 6.1 上游地址

- Anthropic：`{llm}/mavis/api/v1/llm/v1/messages`
- OpenAI：`{llm}/mavis/api/v1/llm/v1/chat/completions`
- Codex：`{llm}/mavis/api/v1/llm/v1/responses`

### 6.2 上游请求头

```
Authorization: Bearer <access_token>
x-api-key: sk-xxx                       # MiniMax Code 给 SDK 的占位 key
User-Agent: MiniMaxAgent
X-Mavis-Agent-Id: main
X-Mavis-Timezone-Offset: <秒>
X-Mavis-Session-Id: <会话 id>
anthropic-version: 2023-06-01          # 仅 Anthropic 协议，客户端没带时补
accept-encoding: identity              # 必须
```

`accept-encoding: identity` 必须显式设置：MiniMax 会把被要求压缩的响应压成单个 gzip member 并在结束时 flush，导致流式响应的所有事件一次性到达、首个 token 等到最后一个。Go 的 `http.Transport` 默认自动加 `Accept-Encoding: gzip`，必须覆盖。回给客户端的响应不携带 `content-encoding`。

### 6.3 协议转换

| 客户端 | 上游 | 转换 |
|---|---|---|
| Anthropic | `/messages` | 原生直通，body 不改 |
| OpenAI | `/chat/completions` | 原生直通，body 不改 |
| Codex | `/responses` | 先试上游原生 `/responses`；上游返回 404 / 不支持时，**本地**转成 Anthropic `/messages`，再把上游 SSE 转回 Responses 事件 |

Codex 回退转换需要产出 `response.output_text.delta`、`response.function_call_arguments.delta`、`response.reasoning_summary_text.delta`、`response.completed` 等事件。

**待实测**：上游是否原生支持 `/responses`。实现时先探原生，探不到再启用回退路径；两条路径的输出行为必须一致。

### 6.4 错误语义

1. **业务错误码**：HTTP 200 不代表成功。检查 body 的 `statusInfo.code`（`1000048` = 登录被拒）与 `base_resp.status_code`，非 0 视为失败。
2. **配额拒绝改写**：状态码 402 / 403 / 429 且 body 命中配额正则时，改写为 `429` + `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit reached: <原消息>"}}`，让客户端知道是配额问题、可以换号重试。其他状态码原样透传。
   配额正则：`insufficient|balance|credit|quota|exhaust|limit|余额|积分|额度|不足|用完|上限`（忽略大小写）
3. **401**：先刷新 token，同账号重试一次；若刷新判定登出则换号。
4. **跨站重定向**：见 8.3，返回 502 + 代理提示，**不当作登录被拒，不消耗 refresh token**。

## 7. 轮转

### 7.1 会话亲和

一个会话键绑定一个账号，稳态下不换。

**会话键来源（优先级）**：
1. 客户端显式请求头 `X-Session-Id` / `X-Conversation-Id`
2. 客户端自带的其他会话标识
3. `hash(system + 首条 user 消息)` —— 同一会话里首条消息不变，能稳定命中；哈希只在本地计算，不外发
4. 都没有 → 退化为轮询（每个请求取下一个账号）

**注意区分两个概念**：

| 概念 | 作用 | 归属 |
|---|---|---|
| `X-Mavis-Session-Id` | 发给 MiniMax 上游的会话标识，影响上游侧上下文与缓存 | 客户端带就用客户端的，否则我们生成固定值；**换号时保持不变**，它标识"这段对话"而非"用哪个账号" |
| 账号亲和键 | 决定本请求使用我们哪个账号 | 由会话键哈希得到 |

### 7.2 选号

会话已有绑定且该账号可用 → 用它。否则从候选池选：跳过已耗尽的账号与窗口，优先剩余额度多的，其次最久未使用的。

### 7.3 故障转移判定

| 上游结果 | 动作 |
|---|---|
| 401 | 刷新 token → 同账号重试一次；刷新判定登出 → 换号 |
| 402 / 403 / 429 且 body 命中配额正则 | 标记耗尽 → 换号重放 |
| 429 但 body 非配额问题 | 限流 → 同账号退避重试（1-2 次） |
| 5xx / 网络错误 | 同账号退避重试 |
| 跨站重定向 | 502 + 代理提示，**不轮转、不消耗 refresh token** |

### 7.4 耗尽粒度：账号 × 窗口

套餐窗口按模型组划分（M3 聊天属 `general`，视频属另一组）。一个账号的 5 小时窗口打满，不影响同账号其他组继续使用。

耗尽标记为二维：`账号 × 窗口键`。窗口键取自套餐窗口的 `model_name`（缺省 `general`）。请求的模型 → 窗口组的映射来自最近一次套餐窗口快照；映射不可得时，退化为账号级耗尽，并按 `exhausted_cooldown`（默认 5 分钟）冷却后自动恢复。

积分余额为 0、或无法归因到具体窗口时，整体标记账号耗尽。

### 7.5 恢复

- 窗口耗尽：按该窗口的 `resetsAt` 自动解锁
- 积分耗尽：靠下一轮账本快照判断余额是否回补
- 轮转器选号时跳过耗尽中的项，恢复是自动的，无需人工干预

### 7.6 重放边界

**只有在上游响应头尚未写给客户端之前，才允许换号重放。**

SSE 一旦开始吐字节就不能再切换账号——客户端已收到部分内容。此时上游中途断流，只能发出一个错误事件收尾，不重放。

配额类拒绝发生在流开始之前（上游先返回 429 JSON），因此这条边界成立。

重放次数上限 `max_attempts`（默认 3），且不超过账号总数。

### 7.7 签名的副作用

带签名的账户 API 请求（账本、签到、会员）**不能换号重放**——签名含时间戳，换账号重放会带着旧签名。只有聊天请求允许重放。

## 8. 上游客户端

### 8.1 超时分级

| 场景 | 超时 |
|---|---|
| OAuth 登录 | 30s |
| 账户 API | 15s |
| 模型列表 | 10s |
| 套餐 console 端点 | 5s（回退端点 10s） |

### 8.2 代理模型

对齐 magpie 的代理模型（magpie 文档原文："Every fetch a plugin makes for a provider or an account goes through the proxy the user set for that provider or account. Otherwise it goes through magpie's own proxy, unless the setting is `direct`. Loopback addresses never go through a proxy."）。

magpie 的三层是"账号级 → provider 级 → 全局"。我们只有一个 provider，但用户需要"一组账号统一走一个代理"，因此把 provider 级那一层称为**出口组**。

**解析顺序**（第一个命中即用）：

1. `accounts.proxy_url` —— 账号级单独指定的地址
2. `accounts.proxy_group` → 查配置里的 `proxy.groups.<name>`
3. `proxy.default` —— 全局默认
4. 都没有 → 直连

`direct` 是特殊值，表示不走代理（与 magpie 一致）。

**回环地址永不走代理**（保留 magpie 的这条规则，防止将来接入本地 mock 时被代理）。

**出口组**在配置里定义，是具名的、可在 WebUI 上选择的：

```yaml
proxy:
  default: "http://127.0.0.1:7890"
  groups:
    clash:  "http://127.0.0.1:7890"
    warp:   "socks5h://warp:4000"
    direct: "direct"
```

### 8.3 代理协议与实现

| scheme | 实现 |
|---|---|
| `http://` | `http.Transport.Proxy` |
| `https://` | `http.Transport.Proxy`（CONNECT） |
| `socks5://` | `golang.org/x/net/proxy` 的 SOCKS5 dialer，**域名透传给代理** |
| `socks5h://` | 同上 |

**Go 内置 SOCKS5 的坑**：`http.Transport` 内置的 socks5 会在**本地**解析 DNS。国内 DNS 解析 `agent.minimax.io` 可能被污染，会连到错误地址。必须用 `x/net/proxy` 自己 dial，域名直接透传，由代理侧解析。

因此我们的 `socks5://` 与 `socks5h://` 行为一致，都是远端 DNS。文档与 WebUI 上均注明这一点。

**HTTP client 必须按账号缓存**：代理是账号级属性，而 `Transport.Proxy` 是 Transport 级属性，一个 Transport 只能一个代理。按 `(账号, 出口 URL)` 缓存 `*http.Client`；账号换组或出口配置变更时，失效对应的缓存条目。

### 8.4 Cloudflare WARP

WARP 作为**额外的出口**，默认关闭，通过 Docker 配置开启。

**接入方式：SOCKS5 出口**（不是全局透明代理）。这样一部分账号走 WARP、一部分走 Clash，互不影响。

**开启方式**：compose profile，改 `.env` 一处即可。

```yaml
# .env
COMPOSE_PROFILES=warp        # 删掉这行 = 不启动 WARP
```

```yaml
# docker-compose.yml
  warp:
    image: cloudflare/warp:latest
    profiles: ["warp"]
    restart: unless-stopped
    cap_add: ["NET_ADMIN"]
    devices: ["/dev/net/tun:/dev/net/tun"]
    sysctls:
      net.ipv4.conf.all.src_valid_mark: "1"
    volumes:
      - warp-state:/var/lib/cloudflare-warp
    command:
      - /bin/sh
      - -c
      - |
        warp-cli --accept-tos registration new || true
        warp-cli mode proxy
        warp-cli proxy port 4000
        warp-cli proxy bind 0.0.0.0
        warp-cli connect
        tail -f /dev/null
    expose: ["4000"]
```

- **地址约定而非配置**：`socks5h://warp:4000` 固定，`proxy.groups.warp` 预置指向它。账号选了 `warp` 组就走 WARP；WARP 没启动时该出口不可用，由健康检查标出。
- **状态卷**持久化 WARP 注册信息，避免每次 `docker compose up` 都重新注册。
- **启动有延迟**：WARP 起来要几秒，第一次请求可能失败。健康检查负责把这件事暴露出来。
- **WARP 的固有限制**：WARP 用 Cloudflare 边缘 IP 回源，部分机房段会被 WARP 直接拒绝（连接断开）。这在配置层绕不开，只能靠健康检查暴露。

**全局透明代理是另一种模式**（`network_mode: "service:warp"`，所有出站流量透明走 WARP），实现更简单，但与"按账号选代理"互斥。本项目采用 SOCKS5 出口模式，不实现透明模式。

### 8.5 出口健康检查

后台任务定期探测各出口是否可用（走该出口请求一个轻量地址），结果写进 `egress_health` 表，WebUI 把不可用出口标红。

用途：WARP 未启动、WARP 被拒绝、代理地址写错——这些在账号真正用到该出口之前都是不可见的。

### 8.6 重定向策略

- **同 host**：跟随，最多 5 跳。307/308 保留 method 与 body 重发；301/302/303 的非 GET/HEAD 请求转为 GET 且丢弃 body。
- **跨 host**：抛 `OtherSite` 错误，对外返回 `502` + 明确提示（需要境外网络的代理），**不当作登录被拒、不消耗 refresh token、不切换账号**。

这是参考插件踩过的坑：两个站的 token 不通用，跟着跳会被误判成"登录被拒"，白白消耗一次性的 refresh token。

### 8.7 业务错误码

见 6.4 第 1 条。

## 9. 签到

### 9.1 流程

1. 读取 `realUserID`：从 token 的 `sub` 取；取不到则调 `/v1/api/user/info` 查
2. GET `{agent}/minimax-cloud/api/v1/signin/status`，查询串带 `device_platform=web`、`is_desktop=1`、`desktop_version=0.6.12`
3. 解析面板：`data.days` 恰好 7 项，`day_no` 为 1..7
4. 判定：
   - 今天的 `status == 3` → `done`（今天已签），返回本次积分与连续天数
   - 今天的 `status == 2` → POST `/minimax-cloud/api/v1/signin/claim`，body `{}`；`claim_result == 1` → `claimed`，`== 2` → `done`
   - 今天的 `status == 4` → `ineligible`
   - 今天的 `status == 1` → `inactive`（今日未开始）
   - 找不到今天 → `inactive`
5. **401 重试**：签到被 401 → 刷新 token → 重新读一次 `realUserID` → 再问一次（只重试一次）

### 9.2 连续天数

从今天往前数连续 `status == 3` 的天数；今天刚领取时为前一天的连续数 +1。

### 9.3 触发方式

- **定时**：调度任务每天在配置时间（默认北京时间 09:00）执行，每个账号加 0-30 分钟随机抖动，避免同时打上游
- **手动**：WebUI 按钮、CLI `mmxcode checkin`
- **幂等**：当天已签到时返回 `done`，不重复领取

## 10. 用量与积分

### 10.1 数据来源

- **积分账本**：GET `{agent}/minimax-cloud/api/v1/credit/details`，查询串 `page_number`（从 1）、`page_size=20`、`include_expired=true`；最多翻 10 页。响应含 `details` 与 `total_count`。每批含 `credit_type`（1 购买 / 2 签到）、`granted_amount`、`consumed_amount`、`remaining_amount`（十进制字符串，可能带千分位逗号；负数按 0 处理）、`granted_at_ms`、`expire_at_ms`。批次超过 `expire_at_ms` 即过期。

**金额口径**（与参考插件 `creditsWindow` 一致）：
- 只统计 `credit_type` 为 1 或 2 的批次
- 批次金额先去掉千分位逗号再解析；不符合 `^\d+(\.\d+)?$` 的视为无法解析，该批次直接跳过（不能用部分数据算出错误的比例）
- `granted` = `granted_amount`；`remaining` = `min(remaining_amount, granted)`（`remaining_amount` 为负按 0）
- `consumed` = `granted - remaining`（不直接用 `consumed_amount`，以免与 `remaining` 口径不一致）
- **套餐窗口**：`{platform}/backend/account/token_plan/remains_percent`（带 `X-Group-Id`），失败回退 `{platform}/v1/api/openplatform/coding_plan/remains`
- **会员信息**：`get_user_extra_info` + `get_membership_info`（国际站预期 404 → 套餐未知）
- **套餐名称**：`cycle_audio_resource_package` 的 `current_subscribe.current_subscribe_title`

### 10.2 刷新策略

- 调度任务默认每 15 分钟刷新每个账号的账本与套餐窗口，写入 `usage_snapshots`
- 被上游按配额拒绝时立即更新对应窗口的耗尽状态与重置时间，不等下一个轮询周期

### 10.3 `GET /api/usage`

网关内所有账号的积分汇总，口径与 MiniMax 账本一致：

```json
{
  "total_granted": 1200,
  "total_consumed": 340.5,
  "total_remaining": 859.5,
  "expires_at": "2026-12-01T16:00:00Z",
  "updated_at": "2026-11-13T22:10:00Z",
  "accounts": [
    { "id": "acc_01", "email": "a@example.com",
      "granted": 600, "consumed": 120.5, "remaining": 479.5,
      "expires_at": "2026-12-01T16:00:00Z" }
  ]
}
```

- `total_*` 只累加未过期的批次（`expire_at_ms > now`）
- `expires_at` 取所有计入批次中最早的 `expire_at_ms`
- `updated_at` 是最近一次刷新时间；快照过期不影响接口返回
- **账本不可用时**（见 19.2）：该账号的 `granted/consumed/remaining` 置为 `null`，不计入 `total_*`；汇总里带一个 `accounts_unavailable` 计数说明有多少账号没数据。宁可少报，不要用套餐窗口的百分比反推积分绝对值——那是两个量纲
- 单账号明细在 `GET /api/accounts` 里一并给出（WebUI 用），`/api/usage` 不额外触发上游请求。

## 11. 模型

### 11.1 列表

GET `{llm}/mavis/api/v1/models?region=en&buildEnv=prod`：带 token 优先；401/403 时不带 token 再试一次；仍失败则用内置列表兜底。

### 11.2 内置列表与真实上限

参考插件实测：上游模型配置对 M3 系列宣称的 512K/128K 是保守声明，端点实际接受 1,048,576 输入 token 与 524,288 输出 token。用实测值覆盖宣传值，否则 agent 会提前约一半压缩历史。

| 模型 | Context | Output | 输入模态 | 思考 |
|---|---|---|---|---|
| MiniMax-M3.1-Flash-Preview | 1048576 | 524288 | text/image/video | forced_on，档位 low/medium/high/xhigh/max |
| MiniMax-M3 | 1048576 | 524288 | text/image/video | switchable：none / high |
| MiniMax-M2.7 | 200000 | 128000 | text | 总是思考 |
| MiniMax-M2.7-highspeed | 200000 | 128000 | text | 总是思考 |

思考变体映射到 Anthropic 的 `thinking: {type:"disabled"}`（none）与 `thinking: {type:"adaptive"}` + `output_config.effort`（各档位）。

## 12. 存储

SQLite（`modernc.org/sqlite`，纯 Go，无 cgo）。文件路径由 `db:` 配置，默认 `/data/mmxcode.db`。

### 12.1 表

```sql
CREATE TABLE accounts (
  id            TEXT PRIMARY KEY,        -- acc_01
  name          TEXT NOT NULL,           -- 用户可改的标签
  user_id       TEXT,                    -- MiniMax realUserID
  email         TEXT,
  status        TEXT NOT NULL,           -- active | signed_out | error
  proxy_group   TEXT,                    -- 出口组名，对应 proxy.groups.<name>
  proxy_url     TEXT,                    -- 账号级代理地址，优先于出口组
  pending_persist INTEGER NOT NULL DEFAULT 0,
  exhausted_until TEXT,                  -- 账号级耗尽的恢复时刻
  last_error    TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE tokens (
  account_id    TEXT PRIMARY KEY REFERENCES accounts(id),
  access        TEXT NOT NULL,
  refresh       TEXT NOT NULL,
  expires_at    INTEGER NOT NULL,        -- 毫秒
  subject       TEXT,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE exhausted_windows (
  account_id    TEXT NOT NULL,
  window_key    TEXT NOT NULL,           -- 模型组，如 general / video
  until         INTEGER NOT NULL,        -- 恢复时刻（毫秒）
  reason        TEXT,
  PRIMARY KEY (account_id, window_key)
);

CREATE TABLE sessions (
  session_key   TEXT PRIMARY KEY,
  account_id    TEXT NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE checkins (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id    TEXT NOT NULL,
  day           TEXT NOT NULL,           -- YYYY-MM-DD（北京时间）
  outcome       TEXT NOT NULL,           -- claimed|done|ineligible|inactive|failed
  credit        REAL,
  streak        INTEGER,
  message       TEXT,
  created_at    INTEGER NOT NULL,
  UNIQUE (account_id, day)
);

CREATE TABLE usage_snapshots (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id    TEXT NOT NULL,
  granted       REAL, consumed REAL, remaining REAL,
  expires_at    INTEGER,
  plan          TEXT,
  plan_until    INTEGER,
  windows_json  TEXT,                    -- 套餐窗口快照
  balance       TEXT,
  created_at    INTEGER NOT NULL
);

CREATE TABLE settings (
  key           TEXT PRIMARY KEY,
  value         TEXT NOT NULL
);

CREATE TABLE egress_health (
  url           TEXT PRIMARY KEY,        -- 出口 URL；"direct" 表示直连
  ok            INTEGER NOT NULL,
  error         TEXT,
  checked_at    INTEGER NOT NULL
);
```

### 12.2 并发

SQLite 开启 WAL，busy_timeout 5s。写操作集中在一个 goroutine 的串行队列里，读操作并发。会话亲和表 `sessions` 与耗尽表 `exhausted_windows` 是热路径，写入频率低。

## 13. HTTP 接口

### 13.1 对外（代理）

| 路由 | 用途 |
|---|---|
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | Codex Responses |
| `GET /v1/models` | 可用模型列表 |

鉴权：配置了 `api_keys` 时，请求必须带 `x-api-key` 或 `Authorization: Bearer <key>`；未配置时不鉴权，此时服务只监听 `127.0.0.1`。

### 13.2 管理

| 路由 | 用途 |
|---|---|
| `GET /api/accounts` | 账号列表（含单账号积分、套餐、签到状态） |
| `POST /api/accounts` | 新增账号（触发 device flow，返回 user_code 与链接） |
| `POST /api/accounts/poll` | 轮询设备登录结果 |
| `DELETE /api/accounts/{id}` | 删除账号 |
| `GET /api/accounts/{id}/usage` | 单账号额度（magpie 卡片形状） |
| `POST /api/accounts/{id}/checkin` | 触发单账号签到，返回 `{outcome, credit?, streak?, message?}` |
| `GET /api/usage` | 全网关积分汇总（见 10.3） |
| `GET /api/egress` | 出口组列表与健康状态 |
| `POST /api/egress/test` | 立即测试某个出口 |
| `PATCH /api/accounts/{id}` | 改账号的出口组 / 代理地址 / 标签 |
| `GET /api/sessions` | 当前会话绑定 |
| `GET /api/events` | SSE 状态推送（签到结果、额度刷新、账号状态变化） |

管理接口的鉴权与对外接口共用 `api_keys`；未配置时同样只监听 `127.0.0.1`。

### 13.3 WebUI

服务端渲染（Go `html/template`）+ 少量原生 JS + SSE 推送。页面：

- **账号总览**：账号列表、状态、套餐、积分、窗口用量、签到连续天数、今日签到状态；每行一个「立即签到」按钮；每行可选**出口组**（下拉，列出 `proxy.groups` 的名字 + `direct`），也可为单账号单独填代理地址
- **出口**：所有出口组与全局默认的健康状态，不可用的标红；一键测试
- **用量汇总**：总积分 / 总消耗 / 总剩余 / 最近过期时间
- **登录**：显示 user_code 与链接，JS 轮询结果
- **日志**：最近的错误与轮转记录

## 14. 配置

```yaml
server:
  listen: ":8080"
  api_keys: ["sk-local-xxxx"]        # 空 = 不鉴权，此时只听 127.0.0.1
proxy:
  default: "http://127.0.0.1:7890"   # 全局默认；"direct" = 不走代理
  groups:                            # 出口组，可在 WebUI 上按账号选择
    clash:  "http://127.0.0.1:7890"
    warp:   "socks5h://warp:4000"    # WARP 默认关闭，见 8.4
    direct: "direct"
upstream:
  timeout: 120s                      # 聊天请求超时
  site: global
rotation:
  strategy: sticky                   # sticky | round-robin
  max_attempts: 3
  exhausted_cooldown: 5m             # 无法归因到窗口时的兜底冷却
schedule:
  checkin_at: "09:00"                # 北京时间，每账号加 0-30min 随机抖动
  usage_refresh: 15m
  token_refresh: 5m
  egress_health: 2m                  # 出口健康检查间隔
db: "/data/mmxcode.db"
```

配置里的出口组只提供**默认名单**；账号在 WebUI 上选择哪个组存在数据库里（`accounts.proxy_group` / `accounts.proxy_url`）。配置改了出口组的地址，已选中该组的账号自动跟随，不需要逐个改账号。

## 15. CLI

```
mmxcode serve                    # 起 HTTP 服务 + WebUI + 定时任务
mmxcode login                    # device flow，打印验证码和链接，轮询到成功
mmxcode accounts                 # 列出账号
mmxcode accounts remove <id>     # 删除账号
mmxcode checkin [--account id]   # 手动签到
mmxcode usage                    # 打印 /api/usage 汇总
mmxcode models                   # 上游模型列表
```

## 16. 部署

### 16.1 Dockerfile

多阶段构建：builder 用 `golang:1.22` 编译静态二进制，runtime 用 `alpine` 或 `distroless`。最终镜像只含二进制与模板，约 20MB。

### 16.2 docker-compose.yml

```yaml
services:
  mmxcode:
    build: .
    image: mmxcode-gateway:latest
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./data:/data
      - ./config.yaml:/app/config.yaml:ro
    environment:
      MMXCODE_CONFIG: /app/config.yaml
      # 容器内访问宿主机的代理需要 host.docker.internal
      MMXCODE_PROXY_DEFAULT: "http://host.docker.internal:7890"

  warp:                      # 默认不启动，见 8.4
    image: cloudflare/warp:latest
    profiles: ["warp"]
    restart: unless-stopped
    cap_add: ["NET_ADMIN"]
    devices: ["/dev/net/tun:/dev/net/tun"]
    sysctls:
      net.ipv4.conf.all.src_valid_mark: "1"
    volumes:
      - warp-state:/var/lib/cloudflare-warp
    command:
      - /bin/sh
      - -c
      - |
        warp-cli --accept-tos registration new || true
        warp-cli mode proxy
        warp-cli proxy port 4000
        warp-cli proxy bind 0.0.0.0
        warp-cli connect
        tail -f /dev/null
    expose: ["4000"]

volumes:
  warp-state:
```

**开启 WARP**：在 `.env` 里写 `COMPOSE_PROFILES=warp`，然后 `docker compose up -d`。删掉这行即关闭。

- 端口默认只绑 `127.0.0.1`，避免把管理界面暴露到公网
- 容器内访问宿主机的 7890 代理需要 `host.docker.internal`（Linux 上需 `extra_hosts: ["host.docker.internal:host-gateway"]`）
- `MMXCODE_PROXY_DEFAULT` 环境变量覆盖配置里的 `proxy.default`，方便同一镜像在不同部署里切换
- WARP 的 SOCKS5 端口只在 compose 网络内 `expose`，不发布到宿主机

### 16.3 备份

SQLite 单文件，备份即复制 `.db` 文件（WAL 模式下需一并处理 `-wal` / `-shm`，或用 `VACUUM INTO` 导出）。

## 17. 测试

| 层 | 覆盖 | 方式 |
|---|---|---|
| `sign` | `yy` / `x-signature` | 对照参考插件 JS 实现造固定向量 |
| `oauth` | device flow 解析（含 MiniMax 的 user_code 变体）、刷新链、invalid_grant 判定 | httptest 假服务器 |
| `account` | 业务错误码检查、账本分页与金额解析 | httptest 假服务器 + 表驱动 |
| `checkin` | 7 天面板各种状态组合、连续天数 | 表驱动单测 |
| `rotator` | 选号、耗尽判定、401→刷新重试、429→换号、会话亲和与改绑 | 假账号池 + 假上游，纯逻辑不碰网络 |
| `egress` | 出口解析顺序（账号级 → 组 → 全局 → 直连）、`direct`、回环不走代理、http/socks5 拨号选择 | 表驱动单测 + httptest 假代理 |
| `store` | 迁移、并发写 | 临时 SQLite |
| `gateway` | 三协议路由、头部改写、错误改写、重放边界 | httptest 假上游 |
| 协议转换 | Responses→Anthropic→Responses SSE | 固定 SSE 样本回放 |

不写真实 MiniMax 账号的自动化集成测试（refresh token 一次性，测试跑两次就废账号）。集成验证手工执行一次 `login` + `checkin` + `usage`。

WARP 的集成验证也是手工的：启动 `warp` profile，在 WebUI 上把一个账号指到 `warp` 组，看出口健康检查是否变绿。

## 18. 实现顺序

1. `site` + `sign`（纯常量与纯函数，先做并单测）
2. `store`（schema 与迁移）
3. `oauth`（登录 + 刷新 + 单飞）
4. `account` + `checkin`（账户 API 与签到）
5. `egress`（出口解析 + HTTP client 缓存 + 健康检查）
6. `rotator`（选号与故障转移）
7. `proxy` + `gateway`（三协议路由与重放）
8. `webui` + `scheduler`
9. CLI 收口、Docker 部署（含 WARP profile）

## 19. 待实测项

1. 上游 `/mavis/api/v1/llm/v1/responses` 是否原生支持 —— 决定 Codex 路径是直通还是走本地转换。**回退已定义**：探到 404 / 不支持就启用本地转换，两条路径输出行为一致
2. 国际站的 `/minimax-cloud/api/v1/credit/details` 是否可用 —— 参考插件只记录了会员接口 404。**回退已定义**：账本不可用时该账号积分置 `null`，`/api/usage` 的 `total_*` 只累加有数据的账号，并带 `accounts_unavailable` 计数
3. `token_plan/remains_percent` 在国际站是否返回数据 —— 决定耗尽标记能否精确到窗口组。**回退已定义**：拿不到窗口映射时退化为账号级耗尽 + `exhausted_cooldown` 冷却
4. WARP 出口在当前网络下能否连通 `agent.minimax.io` —— WARP 的免费线路有区域限制，且部分机房段被 WARP 直接拒绝。**验证方式**：手工启动 `warp` profile，在 WebUI 的出口页看健康检查结果；不可用时该组就不选它
5. 上游 `/mavis/api/v1/llm/v1/chat/completions` 是否原生支持 —— 参考实现只用 Anthropic 的 `/messages`（它接的是 `@ai-sdk/anthropic`），从没走过 OpenAI 的路径；这条路是按同目录下 `/messages` 的样子推的，加上用户说 mcode 原生支持 OpenAI 协议。**回退已定义**：探到 404 / 不支持就把 OpenAI 的请求本地转成 Anthropic 的 `/messages`，再把上游的回答转回 OpenAI 的形状。和 Codex 那条回退共用同一套转换骨架，只是进出形状不同

## 20. 已与参考实现核对过的部分

参考实现：magpie-community/plugins 的 `packages/minimax/index.mjs`
（`main` 分支，2026-11-14 取）。下面这些是照着它逐条核对过的，不是猜的：

| 项目 | 参考实现里的值 | 我们的位置 |
|---|---|---|
| 国际站的四个 host | `account/llm/agent` 都是 `https://agent.minimax.io`（OAuth 是 `https://account.minimax.io`），`platform` 是 `https://platform.minimax.io` | `site.AccountHost` / `LLMHost` / `AgentHost` / `PlatformHost` |
| 站点 region / lang | `en` / `en` | `site.Region` / `site.Lang` |
| 签名密钥 | `I*7Cf%WZ#S&%1RlZJ&C2` | `sign.Secret` |
| OAuth 参数 | client `mcode-public`、scope `agent.default`、audience `agent-backend`、device grant | `site.ClientID` / `Scope` / `Audience` / `DeviceGrant` |
| 聊天的头 | `Authorization`、`x-api-key: sk-xxx`、`User-Agent: MiniMaxAgent`、`X-Mavis-Agent-Id: main`、`X-Mavis-Timezone-Offset`、`X-Mavis-Session-Id`、`anthropic-version: 2023-06-01`、`accept-encoding: identity` | `site.PlaceholderKey` / `UserAgent` / `AgentID`，其余在 §6.2 |
| 聊天 base URL | `<llm>/mavis/api/v1/llm/v1` | `site.MessagesPath` 等 |
| 模型列表 | `<llm>/mavis/api/v1/models?region=<region>&buildEnv=prod` | `site.ModelsPath`（查询串由调用方拼） |
| 签到的查询参数 | `device_platform=web&is_desktop=1&desktop_version=0.6.12` | 签到调用 |
| 积分账本的查询参数 | 签到的三个 + `page_number`、`page_size=20`、`include_expired=true` | 账本调用 |
| 签到面板的状态码 | `1` 未开始、`2` 可领、`3` 已领、`4` 不可领；`claim_result` `1` 领到、`2` 已是今天 | §9 |
| 登录轮询的等待 | `authorization_pending` 和 `slow_down` 都是**继续等**，不是错误；`slow_down` 多等 5 秒 | §9 |
| 账号 API 的端点 | `/v1/api/user/info`、`/matrix/api/v1/user/get_user_extra_info`、`/matrix/api/v1/commerce/get_membership_info`、`/minimax-cloud/api/v1/credit/details`、`/minimax-cloud/api/v1/signin/status`、`/minimax-cloud/api/v1/signin/claim` | `site` 里对应的常量 |
| 套餐窗口的端点 | `/backend/account/token_plan/remains_percent`、`/v1/api/openplatform/coding_plan/remains`、`/v1/api/openplatform/charge/combo/cycle_audio_resource_package` | `site.PlanRemainsPath` 等 |

签名的算法核对得最细，因为它错一点点就是全部请求被拒。
`internal/sign/testdata/regen-vectors.mjs` 里抄了参考实现的 `signed()` 原文，
跑一下就能重新算出六个金标准向量：

```bash
node internal/sign/testdata/regen-vectors.mjs
```

两个容易踩的 JavaScript 的坑，参考实现就是那样，我们的实现照抄：

1. **查询参数的顺序是签名的一部分。** 参考实现用对象展开建参数，
   第一个出现的键决定位置、最后一个出现的键决定值。Go 的
   `url.Values.Encode()` 会排序，签名就变了。
2. **编码是 JavaScript 的编码，不是 Go 的。** `URLSearchParams` 保留
   `*-._`、空格编成 `+`、`~` 编成 `%7E`；`encodeURIComponent` 保留
   `-_.!~*'()`、空格编成 `%20`。Go 的 `url.QueryEscape` 两边都不是。
