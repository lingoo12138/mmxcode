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
mmxcode import --file accounts.txt  # 批量导入账号
mmxcode relogin --all             # 把签出的账号登回去
mmxcode checkin                  # 给所有账号签到
mmxcode usage                    # 积分汇总
mmxcode models                   # 模型列表
```

全局开关，对所有子命令有效：

- `--config <path>`：配置文件路径。默认取环境变量 `MMXCODE_CONFIG`；都不给就用内置默认值。
- `--db <path>`：数据库路径，覆盖配置文件里的 `db`。

`serve` 自己没有别的开关：监听地址、key、轮转参数都在配置文件里。

其余子命令的开关：`login --name`（账号标签）、`accounts proxy --group/--url`（出口组名 / 单独的代理地址）、`checkin --account`（只签这一个账号）、`usage --json`（输出 JSON）、`usage --refresh`（现读一遍账本，不看上次读数）。

### 积分什么时候读

账本是站点那边的数，网关只能去问它要。

- 调度器每 `usage.refresh.scan_every`（默认 30 秒）扫一遍**该读的账号**，一趟最多读 `usage.refresh.batch`（默认 6 个），剩下的下一趟接着读。读到的每一次都存一行；每个账号只留最新的一行，其余的只留一刻钟 —— 不收拾的话一天下来就是几千行。
- 把 `usage.refresh.scan_every` 写成 0，间隔才交回老的 `schedule.usage_refresh`（再没有就 15 分钟）；不写就是默认的 30 秒。两个都写的时候新键赢 —— `schedule.usage_refresh` 只是新键写 0 时的回退。
- 刚登录进来的账号、刚导入进来的账号，马上问一次 —— 不用等那一趟。
- 想现在就知道：`mmxcode usage --refresh`（所有账号读一遍），或者在「用量」那一页上点「刷新」。
- 一次也没读过的账号，在页面上和命令行里都写着「还没读」：它跟「读到了但是读不出来」不是一回事。

### 批量导入

一份清单就能把一堆账号登进来。每行一个账号，写成 `邮箱|密码`，`#` 开头是注释，空行跳过：

```bash
# 清单每行一个账号：邮箱|密码，# 开头是注释
mmxcode import --file accounts.txt

# 每个账号走哪个出口组，每个账号之间隔多久
mmxcode import --file accounts.txt --group clash --interval 5s

# 整趟导入最多跑多久
mmxcode import --file accounts.txt --timeout 30m

# 登录成功就丢密码，不入库
mmxcode import --file accounts.txt --no-store-password
```

导入走的是网页登录那一路（设备码 → 密码登录 → 批准 → 换令牌），所以只需要
账号的邮箱和密码，不需要人点浏览器。

**密码存不存。** 登录成功以后密码会加密存进库里（AES-256-GCM），密钥从环境
变量 `MMXCODE_PASSWORD_KEY` 来（base64 的 32 字节）：

```bash
export MMXCODE_PASSWORD_KEY=$(head -c 32 /dev/urandom | base64)
```

不设这个变量，密码登录一次就丢：账号掉线了就得自己再登一次。设了，
掉线的账号可以自己登回去。

**掉线的账号自己登回去。** 默认是关的 —— 要开就在配置文件里写：

```yaml
batch:
  relogin:
    enabled: true
```

调度器会定期看那些签出（signed_out）而且还存着密码的账号，用存的密码重新
登一遍。也可以手动来一次：

```bash
mmxcode relogin --all              # 所有签出且有密码的账号
mmxcode relogin --account acc_123  # 只登这一个
```

**导入和已有的账号。** 同一个站点账号，库里只有一行：令牌里带的那个用户 id
是它的身份。重跑一次导入是改那一行，不是再插一行；网页上登录一个已经导入
过的账号也一样。掉线的账号被重新导入的时候还是那一行——认它的那个身份在
签出以后还留着。

那一行上的邮箱写的是站点自己认的那个地址（清单上写的那个地址在导入的结果
里还留着）。

**出口组说了算。** 导入带了出口组（命令行 `--group`，或者配置里的
`batch.group`）的时候，账号自己手填的出口地址会被清掉 —— 出口是按「先看
地址、再看组」找的，地址留着就还是从那个地方出去。导入结果里会写明清掉的
是哪个地址。网页上传用的也是配置里的 `batch.group`。

**页面上也能导。** WebUI 里有一页「导入」，清单有两种交法：

- **上传一个文件**（批量那一份，8 MB 以内）
- **贴一段清单**（手写那一份，页面上的长输入框，**最多 100 行**）

两种交的是同一种清单：每行 `邮箱|密码`，`#` 开头是注释。旁边还有一页
「重登」，列出签出的账号，一键登回去。

上传这一页**一直开着**，配不配 `server.api_keys` 都一样：能不能传看的是有没有
登录后台（那把 key 就是后台登录密钥）。明文密码走不走网络，是登录的人自己的
选择 —— 不想让它出本机，就在服务器上跑 `mmxcode import --file accounts.txt`，
格式跟上传的一样。

### 批量导入的配置

| 键 | 默认值 | 作用 |
|---|---|---|
| `batch.interval` | `3s` | 批量导入时两个账号之间的间隔 |
| `batch.group` | 空 | 批量导入走哪个出口组。空 = 全局默认出口。配了它，清单里那些账号自己手填的出口地址会被清掉 |
| `batch.relogin.enabled` | `false` | 账号掉线了自动用存着的密码登回去 |
| `batch.relogin.cooldown` | `1h` | 调度器两趟重登之间隔多久 |
| `batch.relogin.max_accounts` | `20` | 一趟重登最多处理多少个账号 |

## 对外接口

聊天协议：

| 路由 | 用途 |
|---|---|
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | Codex Responses |
| `GET /v1/models` | 模型列表（站点自己那份） |
| `GET /v1/usage` | magpie 的额度卡片：这个网关还剩多少积分 |

管理接口，都在 `/api/` 下：

| 路由 | 方法 | 用途 |
|---|---|---|
| `/api/accounts` | GET / POST | 列出账号 / 发起一次登录 |
| `/api/accounts/poll` | POST | 查登录进度 |
| `/api/accounts/{id}` | PATCH / DELETE | 改账号（名字、出口组、代理地址）/ 删除账号 |
| `/api/accounts/{id}/checkin` | POST | 给这个账号签到 |
| `/api/accounts/{id}/usage` | GET | 读这个账号的积分 |
| `/api/usage` | GET | 所有账号的积分汇总 |
| `/api/usage/refresh` | POST | 立刻把所有账号的账本读一遍 |
| `/api/egress` | GET | 出口组和它们的状态 |
| `/api/egress/test` | POST | 立刻测一个出口组 |
| `/api/sessions` | GET | 会话绑定（目前是空列表） |
| `/api/events` | GET | SSE，每 30 秒一个心跳 |
| `/api/session` | POST / DELETE | 登录换 cookie / 注销 |
| `/api/import` | POST | 批量导入（上传文件，或贴一段清单；要先登录后台） |
| `/api/import/{id}` | GET | 某一次批量导入走到哪儿了 |
| `/api/relogin` | POST | 把签出的账号登回去 |
| `/api/keys` | GET / POST | 列出聊天用的 key / 申请一把新的 |
| `/api/keys/{id}` | DELETE | 撤销一把 key |

WebUI 页面：

| 路由 | 页面 |
|---|---|
| `/` | 账号 |
| `/login` | 登录 |
| `/egress` | 出口 |
| `/usage` | 用量 |
| `/import` | 批量导入 |
| `/relogin` | 重登 |
| `/keys` | 密钥：申请聊天用的 api key |
| `/signin` | 交 key 的登录页 |
| `/static/app.js` | 页面上的脚本 |

### 鉴权

有两把 key，分工不一样：

| | 从哪来 | 开哪扇门 |
|---|---|---|
| 后台登录 key | 配置里的 `server.api_keys`，或环境变量 `MMXCODE_API_KEYS` | 管理界面（页面和 `/api/` 下的接口） |
| 聊天 key | 在后台页面 `/keys` 上申请 | 聊天接口（`/v1/` 下的路由） |

- 聊天接口两把都认：配好的那把能开，申请来的那把也能开。管理界面只认配好的那把 —— 申请来的 key 是给别的机器聊天用的，它开不了管理界面。
- 两把都没有的时候，聊天接口不鉴权。所以 `mmxcode serve` 只允许听本机地址：`server.listen` 不是回环地址（`:8080`、`0.0.0.0:8080` 都不算）就直接报错退出：

  ```
  no api key is configured, so the gateway would listen on :8080 for everyone; put api_keys in the config or MMXCODE_API_KEYS, or listen on 127.0.0.1
  ```

- 配了 `server.api_keys` 时，每个请求都要带 `x-api-key` 或 `Authorization: Bearer <key>`；两个头都没有、或者 key 不对，一律 401。
- 鉴权对所有路径生效。带 key 的请求（`x-api-key` 头或 `Authorization: Bearer`）一律放行；不带 key 的请求分两种：`/api/` 下的接口回 401，浏览器要开的页面回登录页 `/signin`（200），登录页上填 key 换来的 cookie 之后就能进管理界面。
- cookie 的值是拿 key 算出来的 HMAC，服务器自己不留东西；`HttpOnly`、`SameSite=Strict`，走 TLS 时才加 `Secure`。

### 模型列表

`GET /v1/models` 给的是**站点自己那份**名单：网关去 MiniMax 读一次，读到的留着（十分钟），客户端问十回，站点被问一回。

- 问的时候不带账号的令牌：站点没有令牌也答，而带令牌去问有可能在路上把令牌续了 —— 一份模型名单不值得换一把新令牌。
- 站点答不上来（断网、站点改形状）的时候，摆的是**内置的那一份**（`mmxcode models` 打印的就是它）。内置的那一份也会旧：MiniMax 把 `MiniMax-M3.1-Flash-Preview` 改叫 `MiniMax-M3.1-Flash` 之后，内置的那份差点就挑不到现在这个模型。所以它只是兜底，不是主路。
- 站点现在给的名单是 `MiniMax-M3.1-Flash` 和 `MiniMax-M3`。

### 申请聊天用的 key

聊天用的 key 在后台页面 `/keys` 上申请（也可以走 `POST /api/keys`）：

```
curl -X POST http://127.0.0.1:8080/api/keys \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: <后台登录 key>' \
  -d '{"name":"我的笔记本"}'
```

回答里有那把 key 本身：`{"id":"key_...","name":"我的笔记本","key":"sk-...","prefix":"sk-XXXXXXXX…"}`。**它只出现这一次** —— 库里存的是它的哈希（SHA-256），不是它，所以往后任何地方都拿不回这把 key。抄走收好。

- 客户端用它：`x-api-key: sk-...` 或者 `Authorization: Bearer sk-...`。
- 撤销：`DELETE /api/keys/{id}`。撤销之后它就不开门了，但那一行还在列表里 —— 看得到它曾经有过、什么时候没的。删掉的话，人会以为这把 key 从来没有存在过。
- 撤销过的 key 不再算「申请过」：一把都没有的时候聊天接口还是不鉴权。

### magpie 的额度卡片

`GET /v1/usage` 给的是 magpie 的**卡片形状**：magpie 的 provider 插件有一个 `usage()` 钩子，卡片就是从那里来的（参考实现 `packages/minimax/index.mjs` 的 `usageOf`）。我们不是 magpie 的插件，是 magpie 里的一个端点，所以那个钩子由网关自己顶上来。

卡片长这样：

```json
{
  "plan": "Token Plan M3",
  "until": "2026-12-01T00:00:00Z",
  "balance": "437.5 credits",
  "windows": [
    {"name": "Credits", "used": 12.5, "display": "62.5 / 500", "amount": 62.5, "limit": 500, "unit": "credits", "runsOutAt": "2026-12-14T16:00:00Z", "runsOut": 437.5},
    {"name": "7 days", "used": 24, "resetsAt": "2026-11-18T00:00:00Z"}
  ],
  "mmxcode": {"read_at": "2026-11-15T01:50:23Z", "accounts": 2, "readings": 2, "unread": 0, "unreadable": 0}
}
```

上面四个字段（`plan`、`until`、`balance`、`windows`）是 magpie 的，magpie 照着这几个名字读。`mmxcode` 那块是我们自己的（读数是什么时候的、几个账号在后面），magpie 不认识它。

网关是轮转的，所以卡片说的是**整个网关**：

| 字段 | 是什么 |
|---|---|
| `balance` | 所有账号剩下的积分加起来（`"437.5 credits"`，跟参考实现一样的写法） |
| `plan` | 账号里最重的那个计划（有名字的 > `M Plan` > `Free`）。计划读不到的时候这个字段没有 —— 读不到不是一个「没有计划」 |
| `until` | 有计划的那些账号里最早到期的一个 |
| `windows` | 每个账号的积分是一个窗口（`Credits`），它的计划的窗口也在里面。账号不止一个的时候，窗口名字前面带账号名（`主号 · Credits`），不然分不出是谁的 |

数字是**上一次读到的**：调度器每 `usage.refresh.scan_every`（默认 30 秒）读一遍该读的账号，后台页面上也有一个「刷新」按钮。要现读一遍就在地址上加 `refresh=1`（`GET /v1/usage?refresh=1`）—— 那一遍只现读最热的几个账号，卡片等不了所有账号的账。

**用完的账号写着用完。** 被记了「额度用完」的账号，它的窗口上写着「用完」，它的积分也**不算**在 `balance` 里 —— 网关不会再把它给客户端，卡上不能还许着积分。标记是按「账号 × plan 组」记的：别的组没有用完，那些窗口照旧摆着数。标记过了时刻（那个组已经回来）就不算用完了。

这个接口在聊天那扇门后面：手里有聊天那把 key（或者后台登录那把）才能问。

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
| `upstream.max_body_mb` | `8` | 上游回包最多多大（MiB）。站点自己那个回答上限折成字节是 2 MB 上下，8 MiB 是三倍多。超了就是上游发的不是回答，客户端拿到 502 |
| `rotation.max_attempts` | `3` | 一个请求最多换几个账号 |
| `rotation.strategy` | `"sticky"` | `sticky`（会话粘账号）或 `round-robin`（每次都转）。写错的值当 `sticky` 处理 |
| `rotation.exhausted_cooldown` | `5m` | 配额耗尽后暂时不用这个账号的时间。读不到窗口重置时间时按它算 |
| `proxy.default` | 空 | 全局默认出口 |
| `proxy.groups` | 空 | 出口组：组名 → 地址。账号可以指向其中一组，一组共用一个出口 |
| `schedule.checkin_at` | `"00:00"` | 每天签到的时间，**大陆时间**，`HH:MM`。站点按大陆时间算一天，签到要落在那一天里 |
| `schedule.checkin_jitter` | `"0"` | 签到最多往后推多久。几个实例一起跑的时候抖一下就散开了 |
| `schedule.usage_refresh` | `15m` | 积分刷新间隔。只在 `usage.refresh.scan_every` 写成 0 时才用它；两个都写的时候新键赢 |
| `schedule.token_refresh` | `5m` | token 续期检查间隔 |
| `schedule.egress_health` | `2m` | 出口健康检查间隔 |
| `concurrency.total_max` | `0` | 总并发的天花板。0 = 不限 |
| `pool.factor` | `2` | 池装几个号 = max(总并发, 活跃会话数) × 它 |
| `pool.max_size` | `0` | 池里最多几个号。0 = 不限 |
| `pool.memory_budget_mb` | `32` | 池的内存预算（兜底）。一个号按 512 字节算 |
| `pool.session_window` | `1h` | 算「活跃会话数」时看多久以内的会话 |
| `pool.read_on_admit` | `true` | 补号时先读一次额度再进池。读到用完了就不进池 |

环境变量：`MMXCODE_CONFIG` 是 `--config` 的默认值；`MMXCODE_PROXY_DEFAULT` 覆盖 `proxy.default`（不设就不覆盖）；`MMXCODE_API_KEYS` 给出网关的 key 列表（逗号隔开，不设就不覆盖配置里的）。

密钥放哪里都行：配置文件里的 `server.api_keys`，或者环境变量 `MMXCODE_API_KEYS`。容器里推荐后者 —— 密钥是申请来的，写进镜像就等于把它摊在别人眼前。两者都没有的时候，网关只肯听本机地址（`server.listen` 不是回环地址就报错退出）。后台登录用的就是这把 key：它是管理界面和网关共用的那一把。

两个值写错了 `mmxcode serve` 会拒绝启动，而不是悄悄按别的来：`schedule.checkin_at` 不是 `HH:MM` 会报 `schedule.checkin_at = "..."; want HH:MM`，`upstream.site` 不是 `global` 会报 `upstream.site = "..."; only "global" is supported`。

### 数据库

库是 SQLite，用的是纯 Go 的那一份（`modernc.org/sqlite`）—— 不用 cgo，交叉编译出来的东西哪儿都能跑。路径是 `db`。

| 键 | 默认值 | 作用 |
|---|---|---|
| `sqlite.cache_mb` | `8` | 一个连接留多少 MB 的页缓存。读过的页留在手边，工作集装得下就不用一遍遍去磁盘要页 |
| `sqlite.synchronous` | `"normal"` | 写下去多硬。`normal`：交给操作系统，断电的时候最近这几笔可能丢（**库不会坏**）。`full`：每一笔都 fsync，慢一个数量级 |
| `sqlite.checkin_retention_days` | `30` | 签到记录留几天。签到的积分七天就过期了，再往前的记录除了占地方没有别的用处。0 = 永远留。**少于 7 天会被拒** |

**丢不得的那两笔走一条会 fsync 的连接。** 续好的 token 和清掉的「用完」标记走同一个库的另一条连接，那条连接每次提交都 fsync —— 断电不会丢这两笔。别的写入不 fsync：那一趟的账下一趟再对就是。

> 早先这里是把写日志刷进库文件（`wal_checkpoint`）。那个做法会把整个日志抄回库文件，抄的时候占着写锁，别人要写就排队 —— 排队排满了就是 `database is locked`。现在改成 fsync：只等盘，不抄页，也不挡别人。

**写错的 `sqlite:` 一节会让网关拒绝启动。** `synchronous` 只认 `normal` 和 `full`；`cache_mb` 不能是负数；`checkin_retention_days` 要么 0（永远留）要么 7 天以上（少于 7 天的话，「签到待补」那一档要用的那几天的记录会被清掉）。

**签到的记录一批一批写。** 一趟签到几百个号，五十个一批写一个事务 —— 一个号一个事务要把签到表的那几页重写一遍。丢得起：最多五十个号今天的积分进不了池，下一趟补上。

### 读额度

额度不是所有账号一起读的 —— 冷账号的数是旧的，读它也是把旧的数再读一遍。调度器每 `usage.refresh.scan_every` 扫一遍，只读**该读的账号**：

- **签到待补的**：签到过了，账本还没读到（没读到，或者读到的那次失败了 —— 失败的只重试十分钟，过了就把它放下，不让它占着批次）。
- **标记到期的**：额度用完被标记的账号，冷却时间到了，回到轮转里 —— 到期读一次就好，读过了就不再到期。
- **从没读过的**：卡上没有它，一次也没读过。
- **热的**：刚被用过的账号。

排在前面的先读：签到待补 → 标记到期 → 从没读过 → 热。

**冷账号不在名单里。** 它的数是旧的（积分只会往下走，旧的数是高估），读它也是白读。它自己会醒过来：轮转会挑它，被用到就变热，热了才进名单。签到是另一个入口 —— 签到完它就进「签到待补」，下一趟就读到它，签到给的积分立刻上卡。

一趟有四道控，都在 `usage.refresh` 下面：

| 键 | 默认值 | 作用 |
|---|---|---|
| `usage.refresh.concurrency` | `4` | 一趟同时读几个账号的额度 |
| `usage.refresh.timeout` | `15s` | 一个账号的额度最多读多久。卡住的账号不拖住整趟 |
| `usage.refresh.budget` | `30s` | 一趟读额度最多花多久。用完就停，剩下的下一趟接着读 |
| `usage.refresh.batch` | `6` | 一趟最多读几个账号 |
| `usage.refresh.hot_window` | `5m` | 一个账号用过之后多久还算热。热账号的额度才值得现读 |
| `usage.refresh.scan_every` | `30s` | 多久扫一遍该读额度的账号。写成 0 就交回 `schedule.usage_refresh` |

magpie 的额度卡片另有一份：它只现读最热的几个（`usage.refresh.card`，默认 2 个；`usage.refresh.card_budget`，默认 5s），所以卡片不会卡。

**额度用完的账号。** 上游说配额耗尽，网关立刻把这个账号的这个 plan 组标记成用完 —— 它暂时不进轮转。后台再读一次，把标记的时刻改准：读到几点的窗口回来，就到几点（标记只往后挪）；读不到就留着冷却时间（`rotation.exhausted_cooldown`，默认 5 分钟）。签到给了积分就读一次，读到的数说有额度就清掉标记。

**并发。** 每个账号同时一个请求（`concurrency.per_account`，默认 1）；总并发是可用账号的五分之一（`concurrency.total_ratio`，默认 5），账号少的时候按 `concurrency.total_min`（默认 3）兜底。等一个空位最多等 `concurrency.wait`（默认 10s），等不到就 429。

### 签到

每个账号每天签一次到，签到给的积分进各自的账本。

- **什么时候签**：每天一趟，**大陆时间** `schedule.checkin_at`（默认
  `00:00`）。站点按大陆时间算一天，签到要落在那一天里 —— 差 8 个钟头
  就签到前一天的积分，那一份拿不到。
- **起进程时补签**：服务器 00:00 没开着的时候，那一天的签到就没人做了。
  起进程的时候补一趟，签的是**今天还没签**的号。
- **只签今天没签的号**：签过的再签一次，站点只会答 done，白花一次调用。
  签到是串行的，一个号一个号地签 —— 1000 个号 1000 秒。
- **签到的积分进池**：签到给了积分，池里那一份是进池那一刻的数。网关
  每一趟补号（`usage.refresh.scan_every`）问一次库，把签到的积分换进
  池里那份数；不在池里的签到过的号排在补号候选的最前面，它们自己会进
  池。池里那份数只是「有没有额度」的提示，确切是多少由读额度那一趟慢
  慢补上。

### 账号池

网关把账号放在内存里的一个池子里，挑号的时候从池里挑，不扫全表。

- **池多大** = max(总并发, 活跃会话数) × `pool.factor`。默认 2 倍：再留
  一倍给补号用。会话数进公式是因为会话钉在号上，钉住的号不在池里会被
  改绑，缓存命中就没了。
- **池空着也能干活**：池是缓存不是真相 —— 账号、耗尽标记、会话绑定的
  真相都在库里。池空了或者填池失败，网关回到从前的样子从库里读。
- **额度用完的号出池**：标记写进库，那个号让位给一个冷的，进池之前读
  一次额度。标记写不写得进去，那个号都出池 —— 池是缓存，留着一个用完的
  号，它每一次被挑中都是一次白花的请求。
- **签不上的号也出池**：库里说它 signed_out 的时候它就出池，然后当场补
  一个 —— 池短着的时候，承诺的并发就少一个。
- **补号读额度有账**：`usage.refresh.batch` 是一趟读几个（写 0 按 1 算），
  `usage.refresh.budget` 是一趟的时限。池子短着的时候上游的读请求是双份
  的（读额度那一趟 + 补号那一趟）—— 那是要付的价钱。
- **承诺的并发按池夹**：池里只有几个号就只承诺几个 —— 承诺了接不住就是
  一片 429。
- **总并发还有个天花板**：`concurrency.total_max`。0 = 不限；定一个数，
  承诺的并发就永远不超过它 —— 池空的那一下（承诺从库里数）最容易失控，
  天花板是给它的。
- **签到的号排在候选的最前面**：签到给了积分（七天有效），它一定还有额
  度；而从没读过的号有没有额度是不知道的 —— 填池那一趟要读它，读到 0
  就白读一次。
- **会话照旧黏在一个号上**：只有额度用完才改绑。

冷启动填池不等读额度：读一次是 2~11 个上游请求，填满 400 个号要半个
多小时，而网关要立刻能用。库里最后读到的数只是高估，撞空时换号重试会
把耗尽标记写准，系统自己会好。

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
- 环境变量：`MMXCODE_CONFIG=/app/config.yaml`、`TZ=Asia/Shanghai`、`MMXCODE_PROXY_DEFAULT`（默认 `http://host.docker.internal:7890`；容器里访问宿主机的代理要写 `host.docker.internal`，compose 里已经配好）、`MMXCODE_PASSWORD_KEY`（批量导入的密码加密密钥；不设就不存密码）、`MMXCODE_API_KEYS`（网关的 key，也就是后台登录那把；镜像里不带密钥，在 `.env` 里写 `MMXCODE_API_KEYS=sk-xxx`，多把用逗号隔开）。

  不设 `MMXCODE_API_KEYS` 的话，容器里的网关只肯听本机：它监听的是 `:8080`（所有网卡），没有 key 就等于把它白送出去，所以它会报错退出。

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

## magpie 插件

仓库里带一个 magpie 的 provider 插件，在 `packages/mmxcode/`：把 magpie 指到自己的网关上来。

```bash
magpie plugin add ./packages/mmxcode
```

装完在 magpie 里签入 `MMXCode 网关`，要答两样东西：

| 提示 | 是什么 |
|---|---|
| 网关地址 | 默认 `http://127.0.0.1:8080`。网关在别的机器上就写那个地址 |
| 聊天用的 key | 后台 `/keys` 申请的那把。网关一把 key 都没有的时候可以留空（那就不鉴权） |

插件做的事：

- **聊天**走网关的 `/v1/messages`（Anthropic 那条路）。一段对话的会话 id 顺着 `X-Session-Id` 交给网关 —— 网关把它黏在一个账号上，同一段对话不会换号。
- **模型名单**问网关要（`GET /v1/models`），所以 context、能不能读图、思考档位都是网关那边真的。
- **额度卡片**问网关要一次（`GET /v1/usage?refresh=1`），答的是整个网关的样子：所有账号剩下的积分加起来、最重的那个计划、每个账号的窗口。

跟 magpie 自带的 `minimax-code-global` 的分别：那个插件自己拿着 MiniMax 的账号，卡片是逐个账号去问 MiniMax 的 —— 账号一多就卡；这个插件一个账号都不拿，查询全留在网关里，网关答一次就完了。两个 provider 可以并存（id 是 `mmxcode` 和 `minimax-code-global`），用不用得卸掉前者是用户的事。

网关那一侧要配的两处：

- `usage.refresh.*`：多久读一次额度（`scan_every`）、哪些账号算热（`hot_window`）、一趟读几个（`batch`）、现读的预算（`budget`）、一次读超时多久（`timeout`）。卡片上的数是**上一次读到的**，只有该读的账号会被读（热的、没读过的、签到待补的、标记到期的），冷账号等它被用到再说。
- `concurrency.*`：一个账号同时接几个请求（`per_account`，默认 1）、整个网关同时接几个（`total_ratio` / `total_min`）、等得到多久（`wait`）。等不到的时候网关回 429 和 `Retry-After`，不假装忙。

## 协议对齐

上游行为对齐社区插件 [`@magpie-community/opencode-minimax-auth`](https://github.com/magpie-community/plugins/tree/main/packages/minimax) 的 `packages/minimax/index.mjs`：端点、请求签名、刷新语义、签到语义、配额拒绝的判定，都以它为准。

### 上游只认 Anthropic 那条路

参考插件只会打 Anthropic 的 `/messages`（它接的是 `@ai-sdk/anthropic`），它从来没走过 OpenAI 的 `/chat/completions`，也没走过 Codex 的 `/responses`。所以这两种客户端协议都备着本地转换：

- **OpenAI**：先按原样发给上游的 `/chat/completions`；上游回 404（就是没有这个接口）时，改成本地转成 Anthropic 的 `/messages` 再发一次，回来的答案转回 OpenAI 的形状。这个判断只做一次，之后都走本地转换。
- **Codex**：同样的办法，先试 `/responses`，404 就转成 Anthropic 再发。
- **还有一种「没有这条路」**：MiniMax 的开放平台是给 API key 用的，MiniMax Code 的账号在那条路上没有配好的路由，站点答的是 **503** 加一句 `direct_route_not_configured`（`errorCode` 50115）。这不是站点坏了，是那条路对这个账号不通。网关认得这个回答，跟 404 一样处理：关掉那条路，改走 Anthropic。

  认不出来的话，客户端拿到的就是 503 —— magpie 之类用 OpenAI 协议的客户端，一次对话都开不了头。

转换出来的形状是客户端真的能用的：Codex 的非流式回答里，工具调用是 `function_call` 条目（`call_id`、`name`、`arguments` 都在），不是一段空文本。

## 计划

两个计划都已实现：

- 计划 A：核心库与 CLI（`docs/superpowers/plans/2026-11-13-mmxcode-core.md`）
- 计划 B：HTTP 网关（Anthropic / OpenAI / Codex 三协议）、WebUI、Docker 部署（`docs/superpowers/plans/2026-11-13-mmxcode-gateway.md`）

### 不做的

图片生成、视频生成、语音、web search 这些**不做**。参考插件通过 MiniMax 的 Matrix 连接器（`/minimax-cloud/api/v1/connectors/*`）用得上它们，但那些是另一个量级的工作量，要做的时候单开一个计划 —— 设计文档 §1.5 记着调研到的东西。
