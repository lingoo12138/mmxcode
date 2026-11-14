# mmxcode

MiniMax Code 国际站的多账号网关。

## 做什么

- **登录**：MiniMax Code 自己的 device flow（PKCE，client `mcode-public`）。不读写 MiniMax Code 自己的 `~/.minimax` 登录信息。
- **多账号**：每个账号一份凭据，各自独立。
- **签到**：每个账号每天签到一次，签到得到的积分进各自的账本。
- **轮转**：会话粘在一个账号上，配额耗尽时自动换号。
- **代理**：按账号、按出口组或全局指定出口，Cloudflare WARP 可作额外出口。

## 快速开始

```bash
# 编译
go build -o mmxcode ./cmd/mmxcode

# 配置文件：照着 config.example.yaml 改一份
cp config.example.yaml config.yaml
# 把 server.listen 改成 "127.0.0.1:8080"（本机用），
# 或者写上 server.api_keys（给别的机器用）

# 登录一个账号：命令会打印一个链接和一个验证码
./mmxcode --config config.yaml --db ./mmxcode.db login

# 看看账号、签到、看积分
./mmxcode --config config.yaml --db ./mmxcode.db accounts list
./mmxcode --config config.yaml --db ./mmxcode.db checkin
./mmxcode --config config.yaml --db ./mmxcode.db usage

# 起网关
./mmxcode --config config.yaml --db ./mmxcode.db serve
```

数据库路径由 `--db` 覆盖（示例配置里的 `db: /data/mmxcode.db` 是容器里的路径，本机跑要换掉）。

起完网关之后，客户端指过来就行：

```bash
curl -s http://127.0.0.1:8080/v1/messages \
  -H "content-type: application/json" \
  -d '{"model":"MiniMax-M3","max_tokens":64,"messages":[{"role":"user","content":"say hi"}]}'
```

配了 `server.api_keys` 的话，请求要带上 key：

```bash
curl -s http://127.0.0.1:8080/v1/messages \
  -H "content-type: application/json" \
  -H "x-api-key: sk-local-xxxx" \
  -d '{"model":"MiniMax-M3","max_tokens":64,"messages":[{"role":"user","content":"say hi"}]}'
```

## 命令

```
mmxcode serve                     # 起 HTTP 服务、WebUI 与定时任务
mmxcode login                    # 登录一个账号
mmxcode accounts list            # 列出账号
mmxcode accounts proxy <id> --group warp
mmxcode checkin                  # 给所有账号签到
mmxcode usage                    # 积分汇总
mmxcode models                   # 模型列表
```

全局开关，对所有子命令有效：

- `--config <path>`：配置文件路径。默认取环境变量 `MMXCODE_CONFIG`；都不给就用内置默认值。
- `--db <path>`：数据库路径，覆盖配置文件里的 `db`。

`serve` 自己没有别的开关：监听地址、key、轮转参数都在配置文件里。

其余子命令的开关：`login --name`（账号标签）、`accounts proxy --group/--url`（出口组名 / 单独的代理地址）、`checkin --account`（只签这一个账号）、`usage --json`（输出 JSON）。

## 对外接口

聊天协议：

| 路由 | 用途 |
|---|---|
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | Codex Responses |
| `GET /v1/models` | 模型列表 |

管理接口，都在 `/api/` 下：

| 路由 | 方法 | 用途 |
|---|---|---|
| `/api/accounts` | GET / POST | 列出账号 / 发起一次登录 |
| `/api/accounts/poll` | POST | 查登录进度 |
| `/api/accounts/{id}` | PATCH / DELETE | 改账号（名字、出口组、代理地址）/ 删除账号 |
| `/api/accounts/{id}/checkin` | POST | 给这个账号签到 |
| `/api/accounts/{id}/usage` | GET | 读这个账号的积分 |
| `/api/usage` | GET | 所有账号的积分汇总 |
| `/api/egress` | GET | 出口组和它们的状态 |
| `/api/egress/test` | POST | 立刻测一个出口组 |
| `/api/sessions` | GET | 会话绑定（目前是空列表） |
| `/api/events` | GET | SSE，每 30 秒一个心跳 |
| `/api/session` | POST / DELETE | 登录换 cookie / 注销 |

WebUI 页面：

| 路由 | 页面 |
|---|---|
| `/` | 账号 |
| `/login` | 登录 |
| `/egress` | 出口 |
| `/usage` | 用量 |
| `/signin` | 交 key 的登录页 |
| `/static/app.js` | 页面上的脚本 |

### 鉴权

- 配了 `server.api_keys` 时，每个请求都要带 `x-api-key` 或 `Authorization: Bearer <key>`；两个头都没有、或者 key 不对，一律 401。
- 一个 key 都不配时，鉴权整体跳过。所以 `mmxcode serve` 只允许听本机地址：`server.listen` 不是回环地址（`:8080`、`0.0.0.0:8080` 都不算）就直接报错退出：

  ```
  no api key is configured, so the gateway would listen on :8080 for everyone; put api_keys in the config or listen on 127.0.0.1
  ```

- 鉴权对所有路径生效。带 key 的请求（`x-api-key` 头或 `Authorization: Bearer`）一律放行；不带 key 的请求分两种：`/api/` 下的接口回 401，浏览器要开的页面回登录页 `/signin`（200），登录页上填 key 换来的 cookie 之后就能进管理界面。
- cookie 的值是拿 key 算出来的 HMAC，服务器自己不留东西；`HttpOnly`、`SameSite=Strict`，走 TLS 时才加 `Secure`。

### 轮转

**会话怎么定。** 请求的会话优先看客户端给的头：`X-Session-Id`（也认 `X-Conversation-Id`、`X-Session-Id-Codex`、`Session-Id`）。有就用它的哈希当账号绑定键。客户端没带头，就用 system prompt 加第一条 user 消息的哈希当绑定键。两样都没有就没有会话，按轮转挑账号。

**发给上游的会话 id。** 上游拿到的是数据库里存着的一个随机 id：一个会话一个，存下来不变，重启之后还是它，换号也不变。客户端自己那串字符串不外发，请求里的话的哈希也不外发。

这个改不了有一种情况：两段 system prompt 和第一条 user 消息都一样的对话，客户端又不带会话 id 的时候，按上面的规则它们本来就是同一个会话，于是共用一个上游 id、一个上游会话。服务器分不出来 —— 请求里没有能分开它们的东西；要分开只能让客户端带上 `X-Session-Id`。

**换号。** 账号的配额用完时，上游的拒绝被判定成配额耗尽，网关把这个账号的这个 plan 组标记成耗尽，换一个账号把请求重放一遍。重放只发生在还没往客户端写出任何字节的时候：流式响应会先读第一个 SSE 事件，判断它是错误还是回答；一旦写出去就不再换号，流到一半断了只给客户端补一个 error 事件收尾。

- 耗尽的账号在 `rotation.exhausted_cooldown`（默认 5m）之内不再被挑中；能从上游读到窗口重置时间就按那个时间算，读不到才按 cooldown 算。
- 这个请求里已经试过并失败的账号会被跳过：另一个账号来答这一次，但会话绑定留在原处 —— 一次 5xx 不会把整个会话搬到别的账号上去。

## 配置

见 `config.example.yaml`（容器里见 `config.docker.yaml`）。文件只需要写自己要改的键，剩下的走默认值；认不出的键直接报错。

| 键 | 默认值 | 作用 |
|---|---|---|
| `server.listen` | `":8080"` | 监听地址 |
| `server.api_keys` | 空 | 网关接受的 key 列表。空 = 不鉴权，只允许听本机 |
| `db` | `"/data/mmxcode.db"` | SQLite 数据库路径 |
| `upstream.timeout` | `120s` | 上游请求时限。流式对话也吃这份预算，想聊更久就调大 |
| `upstream.site` | `"global"` | 上游站点，只支持 `global` |
| `rotation.max_attempts` | `3` | 一个请求最多换几个账号 |
| `rotation.strategy` | `"sticky"` | `sticky`（会话粘账号）或 `round-robin`（每次都转）。写错的值当 `sticky` 处理 |
| `rotation.exhausted_cooldown` | `5m` | 配额耗尽后暂时不用这个账号的时间。读不到窗口重置时间时按它算 |
| `proxy.default` | 空 | 全局默认出口 |
| `proxy.groups` | 空 | 出口组：组名 → 地址。账号可以指向其中一组，一组共用一个出口 |
| `schedule.checkin_at` | `"09:00"` | 每天签到的时间，本地时区，`HH:MM` |
| `schedule.usage_refresh` | `15m` | 积分刷新间隔 |
| `schedule.token_refresh` | `5m` | token 续期检查间隔 |
| `schedule.egress_health` | `2m` | 出口健康检查间隔 |

环境变量：`MMXCODE_CONFIG` 是 `--config` 的默认值；`MMXCODE_PROXY_DEFAULT` 覆盖 `proxy.default`（不设就不覆盖）。

两个值写错了 `mmxcode serve` 会拒绝启动，而不是悄悄按别的来：`schedule.checkin_at` 不是 `HH:MM` 会报 `schedule.checkin_at = "..."; want HH:MM`，`upstream.site` 不是 `global` 会报 `upstream.site = "..."; only "global" is supported`。

### 代理模型

出口按这个顺序挑：

1. 账号自己的代理地址：`mmxcode accounts proxy <id> --url <地址>`；
2. 账号指向的出口组：`mmxcode accounts proxy <id> --group <组名>`，取组里的地址；
3. 全局 `proxy.default`。

组的地址写成空字符串时不算出口：它会落到全局默认，而不是变成直连。全局默认也是空（或明确写 `direct`）时才是直连。

- 支持的 scheme：`http`、`https`、`socks5`、`socks5h`。没有 scheme 的地址会报错，不会默默直连。
- `socks5://` 和 `socks5h://` 都把域名交给代理去解析（远端 DNS），跟 curl 的 `socks5://`（本地解析）不一样。

## 部署

```bash
cp .env.example .env
docker compose up -d
```

compose 做的事：

- 用仓库里的 Dockerfile 构建，镜像名 `mmxcode-gateway:latest`。
- 端口只绑本机：`127.0.0.1:8080:8080`，管理界面不暴露到公网。
- 挂配置文件：默认挂仓库里的 `config.docker.yaml` 到 `/app/config.yaml`（只读）。要用自己的副本就设 `MMXCODE_CONFIG_FILE=./config.yaml`。
- 挂数据目录：默认是 Docker 命名卷 `mmxcode-data`，容器里是 `/data`，数据库落在 `/data/mmxcode.db`。
- 容器以非 root 用户 `mmxcode`（uid 10001）运行，`/data` 归它所有。
- 环境变量：`MMXCODE_CONFIG=/app/config.yaml`、`TZ=Asia/Shanghai`、`MMXCODE_PROXY_DEFAULT`（默认 `http://host.docker.internal:7890`；容器里访问宿主机的代理要写 `host.docker.internal`，compose 里已经配好）。

数据目录：

- 默认 `MMXCODE_DATA_DIR=mmxcode-data`，是 Docker 命名卷。Docker 按镜像里 `/data` 的属主（uid 10001）创建它，容器里的用户直接能读写。
- 想把数据库留在宿主机目录上就写 `MMXCODE_DATA_DIR=./data`，但那个目录必须对 uid 10001 可写，否则容器建不了数据库文件、启动即失败：

  ```bash
  mkdir -p data && chown 10001:10001 data
  ```

### Cloudflare WARP

WARP 是额外的出口选项，跟按账号单独指定的代理并列。

- 默认是关的：`.env.example` 里 `COMPOSE_PROFILES=warp` 那行是注释掉的，所以普通的 `docker compose up` 不会起 WARP。
- 打开：把 `.env` 里那行的注释去掉。宿主机的 `/dev/net/tun` 要可用，容器需要 `NET_ADMIN` 权限（compose 里已经写好）。
- 起了以后，出口组 `warp` 指向 `socks5h://warp:4000`，账号用 `mmxcode accounts proxy <id> --group warp` 就走 WARP。

## 协议对齐

上游行为对齐社区插件 [`@magpie-community/opencode-minimax-auth`](https://github.com/magpie-community/plugins/tree/main/packages/minimax) 的 `packages/minimax/index.mjs`：端点、请求签名、刷新语义、签到语义、配额拒绝的判定，都以它为准。

### 上游只认 Anthropic 那条路

参考插件只会打 Anthropic 的 `/messages`（它接的是 `@ai-sdk/anthropic`），它从来没走过 OpenAI 的 `/chat/completions`，也没走过 Codex 的 `/responses`。所以这两种客户端协议都备着本地转换：

- **OpenAI**：先按原样发给上游的 `/chat/completions`；上游回 404（就是没有这个接口）时，改成本地转成 Anthropic 的 `/messages` 再发一次，回来的答案转回 OpenAI 的形状。这个判断只做一次，之后都走本地转换。
- **Codex**：同样的办法，先试 `/responses`，404 就转成 Anthropic 再发。

转换出来的形状是客户端真的能用的：Codex 的非流式回答里，工具调用是 `function_call` 条目（`call_id`、`name`、`arguments` 都在），不是一段空文本。

## 计划

两个计划都已实现：

- 计划 A：核心库与 CLI（`docs/superpowers/plans/2026-11-13-mmxcode-core.md`）
- 计划 B：HTTP 网关（Anthropic / OpenAI / Codex 三协议）、WebUI、Docker 部署（`docs/superpowers/plans/2026-11-13-mmxcode-gateway.md`）

### 不做的

图片生成、视频生成、语音、web search 这些**不做**。参考插件通过 MiniMax 的 Matrix 连接器（`/minimax-cloud/api/v1/connectors/*`）用得上它们，但那些是另一个量级的工作量，要做的时候单开一个计划 —— 设计文档 §1.5 记着调研到的东西。
