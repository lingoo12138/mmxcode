# mmxcode 批量导入设计文档

日期：2026-11-14
状态：待评审

## 1. 目标与范围

### 1.1 一句话

给 mmxcode 加一条**批量入号**的路：读一份 `邮箱|密码` 的清单，自动走完「网页登录 → 设备授权 → mcode 凭据」这条链，把账号灌进库里；密码加密入库后，账号掉线可以自动重登。

### 1.2 范围内

- MiniMax 网页版的**邮箱 + 密码**登录（`loginType: "20"`）
- 用登录得到的会话（`_sid`）去批准 mcode 的**设备授权码**，换出 CLI 的 access/refresh token
- 批量导入：文件或 WebUI 上传，逐个账号跑完上面这条链
- 密码**加密入库**（AES-256-GCM），密钥来自环境变量
- **自动重登**：账号被判定签出后，由调度器自动重新入号；重不回来的账号留在签出状态，等人处理
- CLI：`mmxcode import`、`mmxcode relogin`
- WebUI：批量导入页 + 重登页

### 1.3 范围外（明确不做）

- 邮箱验证码登录（`loginType: "21"`）——发码要过 reCAPTCHA Enterprise，无头环境容易被挑战挡掉
- Google（`11`）/ GitHub（`8`）/ 微信（`5`）/ 子账号（`9`）登录
- 中国站（`minimax.cn` / `minimaxi.com`）的账号——三套账号体系互不相通
- 埋点与风控信标（`POST /QbJUiD/…`、meerkat-reporter）——纯 HTTP 复现不需要
- 图形验证码处理
- 改密码、找回密码、绑定手机——只在错误信息里告诉用户该做什么，不代劳

### 1.4 这条链和参考插件的关系

参考插件（`@magpie-community/opencode-minimax-auth`）只做**设备授权**，登录那一步是人在浏览器里点完成的。本设计补上的是**登录那一步**，两段拼起来还是同一个 OAuth 设备流：

```
网页登录（密码）                    设备授权（CLI 的那半）
POST /oauth2/login  ──_sid cookie──▶ GET  /oauth2/device/authorize
                                     POST /oauth2/device/authorize
                                     POST /oauth2/token ──▶ mcode 凭据
```

设备授权那三步的端点、参数、错误语义，以参考插件的 `packages/minimax/index.mjs` 为准；登录那几步以 MiniMax 网页版前端的实际行为为准（下面 §2 逐条列了抓包与实测的证据）。

## 2. 链路

### 2.1 全部步骤

每个账号跑这七步，成功后库里就多一个能用的账号：

| 步 | 请求 | 拿到什么 |
|---|---|---|
| 1 | `POST /oauth2/device/code`（`mcode-public` + PKCE S256） | `device_code`、`user_code`、`verification_uri`、`expires_in`、`interval` |
| 2 | `POST /v1/api/user/device/register` | `device_id`（`data.deviceIDStr`） |
| 3 | `POST /oauth2/login`（`authToken: null`） | 探测：这个号能不能走密码 |
| 4 | `POST /oauth2/login`（`authToken: RSA(密码)`） | `_sid` cookie |
| 5 | `GET /oauth2/device/authorize?user_code=…&app_id=3001`（带 `_sid`） | 设备信息、默认 `group_id` |
| 6 | `POST /oauth2/device/authorize`（带 `_sid`） | 批准成功 |
| 7 | `POST /oauth2/token`（device grant + `code_verifier`） | access / refresh token |

第 7 步的轮询逻辑（`authorization_pending` 继续等、`slow_down` 退避、`invalid_grant` 不重试）与参考插件一致，**直接复用 `internal/oauth`**，不另写一份。

### 2.2 端点与实测

**2026-11-14 实测**（直连与 `http://127.0.0.1:7890` 代理各试一次，两条都通）：

```
POST https://account.minimax.io/oauth2/device/code
  body: client_id=mcode-public&scope=agent.default&audience=agent-backend
        &code_challenge=…&code_challenge_method=S256
-> {"device_code":"YVBt7uL-…","expires_in":300,"interval":3,
    "user_code":"HN32-TWAH",
    "verification_uri":"https://account.minimax.io/oauth-authorize",
    "verification_uri_complete":"https://account.minimax.io/oauth-authorize?user_code=HN32-TWAH"}
```

- `interval: 3` 是**秒**——普通那套 device flow 的间隔按秒读（`byUser` 变体才是毫秒）
- `verification_uri` 是 `https://account.minimax.io/oauth-authorize`，`client_id` 是 `mcode-public`

```
GET https://account.minimax.io/oauth2/device/authorize?user_code=HN32-TWAH&app_id=3001
-> 401 {"error":"login_required",
        "error_description":"account session is missing or expired",
        "login_url":"/unified-login?login_redirect=%2Foauth-authorize%3Fclient_id%3Dmcode-public%26user_code%3DHN32-TWAH"}
```

- 未登录时 401 + `login_required`，`login_url` 指回 mcode 的授权页——**这正是「网页会话可以批准设备码」的入口**
- `login_url` 里的 `login_redirect` 是 `/oauth-authorize?client_id=mcode-public&user_code=…`，登录时要把它原样带回去

### 2.3 网页登录

```
POST https://account.minimax.io/oauth2/login?<网页那套公共 query>
Content-Type: application/json
body: {"loginType":"20","email":"…","authToken":null,"deviceID":"…",
       "login_redirect":"/oauth-authorize?client_id=mcode-public&user_code=…"}
-> 200 {"data":null,"statusInfo":{"code":0,…}}
```

这一步是**探测**：问后端「这个号走哪条路」。

```
POST https://account.minimax.io/oauth2/login?<网页那套公共 query>
body: {"loginType":"20","email":"…","authToken":"<base64(RSA(密码))>",
       "countryCode":"+86","deviceID":"…",
       "login_redirect":"/oauth-authorize?client_id=mcode-public&user_code=…"}
-> 200 {"code":0,"data":{"avatar":"…","is_register":0,"login_redirect":"…"}}
   Set-Cookie: _sid=…; Max-Age=3456000; Path=/; HttpOnly; Secure; SameSite=Lax
```

- `authToken` = `base64(RSA-1024 / PKCS#1 v1.5(密码))`，公钥硬编码在前端（`js/204` 模块 7160）：

  ```
  -----BEGIN PUBLIC KEY-----
  MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDF5ndG2/UB4L5tbvQaNLHSoBTW
  DKbrNBuOmUIP23eCmC2ELMx3kppEikxTp5cV8NxUZl6ii+KLwKugioAXApzypHXb
  gXbq13kTKA7OCA1xtAoMdH9cltjBiFAUJlgmVjr0MuJCknhVAjWLjCVRHege+Atl
  gkUBUeGa9O+cWcPEwQIDAQAB
  -----END PUBLIC KEY-----
  ```

  128 字节密文 → base64 172 字符。PKCS#1 v1.5 的填充是随机的，所以同一密码每次的密文都不同，不能拿密文当密码哈希用。
- `countryCode` 网页按区域给（抓包里是 `+86`），照抄抓包的值
- **密码登录本身不过验证码**。reCAPTCHA Enterprise 只在发邮箱/短信验证码那一步（`POST /v1/api/user/login/sms/send`）才要。
- 公共 query 必须和网页那份一字不差（§3.2），因为签名是把它整个算进去的。
- 每个账号用**自己的 cookie jar**：`_sid` 是会话，`_abck` / `bm_sz` 是 CDN 的风控 cookie，都不与别的账号混用。

### 2.4 批准设备码

```
GET https://account.minimax.io/oauth2/device/authorize?user_code=…&app_id=3001
Cookie: _sid=…
-> 200 {…设备信息…, groups:[{id:…, is_default:true}, …]}
```

`group_id` 取 `is_default` 的那一组；一个都没有就报错（前端也是这么处理的：「A default group is required to approve authorization.」）。

```
POST https://account.minimax.io/oauth2/device/authorize
Cookie: _sid=…
Content-Type: application/json
body: {"user_code":"…","decision":"approve","group_id":"…"}
```

- 这两个请求**不签名**（网页前端用的是原生 `fetch`，没走 axios 拦截器），也不带公共 query
- `decision` 只有 `"approve"` / `"deny"` 两种

### 2.5 登录方式的枚举（`js/809` 模块 375）

| 值 | 方式 | 这一版 |
|---|---|---|
| `""` | 手机号 + 短信码 | 不做 |
| `"1"` / `"2"` | 运营商一键登录 | 不做 |
| `"3"` | 匿名 | 不做 |
| `"5"` | 微信 | 不做 |
| `"8"` | GitHub | 不做 |
| `"9"` | 子账号（首登强制改密） | 不做，但**认它的错误码** |
| `"11"` | Firebase（Google） | 不做 |
| `"20"` | **邮箱 + 密码** | **做** |
| `"21"` | 邮箱验证码 | 不做 |

## 3. 签名与 query

### 3.1 算法

网页和 CLI 是同一套签名，同一把盐：

```
yy          = md5( encodeURIComponent(path?query) + "_" + bodyJSON + md5(unix_ms) + "ooui" )
x-timestamp = floor(unix_ms / 1000)
x-signature = md5( str(x-timestamp) + "I*7Cf%WZ#S&%1RlZJ&C2" + bodyJSON )
```

GET 时 `bodyJSON` 是字面量 `"{}"`。我们 `internal/sign` 里的算法与此**逐字符一致**（同一个盐、同一个 `_` 分隔、同一个 `ooui`），已用抓包向量复核过。

### 3.2 网页那套公共 query（键序也是签名的一部分）

网页的公共 query 是一整块，**排在前面**；调用方自己那份 query **追加在后面**。
真抓包里那条设备注册的请求就是这么排的：

```
POST /v1/api/user/device/register?device_platform=web&biz_id=3&...&uuid=<uuid4>&uuid=<uuid4>
                                                            ↑公共块里的  ↑调用方自己的
```

（`uuid` 出现两次：公共块里有一份，调用方自己那份又追加了一份。）

```
device_platform=web
biz_id=3
app_id=3001
version_code=22201
unix=<毫秒>
timezone_offset=<秒>                          ← -60 × getTimezoneOffset()，东八区是 28800
sys_language=en
lang=en
uuid=<uuid4>                                  ← 长期设备标识
device_id=<设备注册返回的号>
os_name=Windows
browser_name=chrome
device_memory=16
cpu_core_num=16
browser_language=en-US
browser_platform=Win32
user_id=<realUserID>                          ← 登录前是 0
op_ticket=<空>                                ← 网页在登录前发的是字面量 "undefined"
screen_width=1280
screen_height=800
client=web
```

设备注册（公共块在前，自己那份 uuid 在后）：

```
POST https://account.minimax.io/v1/api/user/device/register?<公共 query>&uuid=<uuid4>
body: {"uuid":"<uuid4>"}
-> 200 {"data":{"deviceIDStr":"574786429807779842"}}
```

`device_id` 建议做（它参与后续请求的签名参数）；埋点和风控信标可以不做。

### 3.3 我们要怎么改 `internal/sign`

现在 `internal/sign.Sign` 把 mcode 那套默认值写死了（`device_platform=mcode`、`browser_name=mcode`、`client=mcode`，调用方的 query 插在中间）。网页这套不一样：

- 调用方的 query 排在公共块的**后面**（mcode 是在中间）
- `device_platform` / `browser_name` / `client` 的值不同
- 多出 `uuid` / `device_id` / `screen_width` / `screen_height` / `device_memory` / `cpu_core_num` / `browser_language` / `browser_platform` / `op_ticket`

改法：`sign.Options` 加一个 `Flavor` 字段（`sign.FlavorMCode` / `sign.FlavorWeb`），每个 flavor 各自一份默认 query 与插入位置。**默认仍是 mcode**，现有的调用方一行不用改。

网页这套是拿一个 `URLSearchParams` 填的：公共块先填，调用方自己那份后填。所以
两半里都有的键，query 里会出现两次，而站点用的那几种解析器
（`url.Values.Get`、`URLSearchParams.get`、Java 的 `getParameter`）取的都是**第一
个**——网页这边算数的是公共块里那个值。CLI 那套是对象字面量，两半对重复键的
处理不一样，各是各的。

## 4. 密码的存法

### 4.1 密文

- 算法：AES-256-GCM
- 存进 `accounts.password` 一列，格式 `base64(nonce ‖ ciphertext ‖ tag)`，nonce 12 字节随机
- 密钥来自**环境变量 `MMXCODE_PASSWORD_KEY`**（base64 的 32 字节）

### 4.2 为什么密钥只走环境变量

配置文件和数据库在 Docker 里通常是同一卷（甚至同一个备份）。密钥跟密文放在一处，加密就等于没加密。环境变量可以只给这台机器，不进备份、不进 git。

**没设 `MMXCODE_PASSWORD_KEY` 时**：登录成功即丢密码，不入库（行为退回到「用完即弃」）。导入时要在终端里说清楚这件事，不能默默不存。

### 4.3 谁能读

- 库文件本身是明文的 SQLite——能拿到库文件的人就能拿到密文，但拿不到密钥
- 密钥不进 WebUI、不进 `/api/usage`、不进日志
- 日志里**永远不出现密码、密文、`_sid`、`authToken`**

## 5. 批量导入任务

### 5.1 输入

每行一个账号：

```
# 井号开头是注释，空行跳过
someone@example.com|password
other@example.com|p@ss|w0rd
```

- 分隔符是 `|`，第一段是邮箱，其余全当成密码（密码里可以有 `|`）
- 邮箱那一段要能过基本格式检查；不像邮箱的行直接判失败并报行号
- 重复的邮箱：跳过并报「已有这个账号」
- 文件编码 UTF-8；读的时候按行拆，`\r\n` 也认

### 5.2 节奏

- **默认串行**，一个账号跑完再下一个
- 每个账号之间默认等 3 秒（可配）
- 每个账号一个**新的 uuid + 新注册的 device_id**——批量 100 个号共用一个 device_id，在风控上就是「一个人有 100 个号」；分开注册看着像不同的人
- 每个账号一个**独立的 cookie jar**
- 登录请求走**账号自己的出口**（`--group` 指定的出口组，或全局默认出口）

### 5.3 失败怎么处理

| 情形 | 做法 |
|---|---|
| 网络错误 / 5xx / 超时 | 指数退避重试 2 次（1s、4s），还不行算失败 |
| 429、站点风控 | 退避更久（30s）重试 1 次，还不行算失败 |
| 密码错、账号要设密码、账号要绑手机、子账号要改密 | **不重试**，直接算失败 |
| 设备码过期（5 分钟内没批准完） | 不重试，算失败（下一个账号重新开一个设备码） |

失败的账号**不入库**——一个坏号进了轮转池就是给自己埋雷。只进结果汇总。

### 5.4 结果留痕

新表 `import_runs`：

| 列 | 含义 |
|---|---|
| `id` | 本次任务的 id |
| `source` | `cli` / `webui` |
| `started_at` / `finished_at` | 起止时间 |
| `total` / `ok` / `failed` | 总数、成功数、失败数 |
| `results` | JSON 数组，每条：`{email, ok, stage, code, message}` |

`stage` 是失败发生的环节：`read`（文件解析）/ `probe`（邮箱探测）/ `password`（密码登录）/ `approve`（批准设备码）/ `token`（换 token）/ `store`（入库）。

CLI 打印一张表；WebUI 显示同一张表。

## 6. 自动重登

### 6.1 放在哪

**放在调度器里，不动续期器的热路径。**

续期器的行为不变：refresh 链断了（`invalid_grant` + HTTP 400）就把账号标成 `signed_out`、写上原因。这一步是现有的行为，不改。

新加一个调度任务 `relogin`：

1. 找 `status = 'signed_out'` 且 `password IS NOT NULL` 的账号
2. 重新跑一遍 §2.1 的七步（等价于再导入一次这个号）
3. 成功 → 状态回 `active`，token 换新
4. 失败 → 状态还是 `signed_out`，`last_error` 写清原因，**冷却**一段时间再试（默认 1 小时）

### 6.2 开关与退让

- 默认**关闭**，配置里显式打开（`batch.relogin.enabled: true`）
- 同一个账号两次重登之间至少隔 `batch.relogin.cooldown`（默认 1h）
- 重登失败不会拖垮调度器：单次任务的账号数有上限（默认 20），一个账号失败不中断其余

### 6.3 为什么不做成「续期器失败就地重登」

续期器是整个项目里最 delicate 的地方：MiniMax 的 refresh token **只能用一次**，旧的重用会吊销整条链。在那条热路径上再加一个「拿密码重新登录」的分支，等于把一个可见的状态变化变成自动发生的，而且出错了不容易查。

放在调度器里，重登是**一次独立、可观察、可关掉**的动作。

## 7. 命令行

```
mmxcode import --file accounts.txt [选项]
  --group NAME       每个账号走哪个出口组（默认用全局默认出口）
  --interval 3s      每个账号之间的间隔
  --no-store-password  登录成功即丢密码，不入库（覆盖环境变量里的默认行为）
  --timeout 10m      整个任务的时限

mmxcode relogin [选项]
  --all              重登所有签出且有密码的账号
  --account ID       只重登这一个
  --group NAME       走哪个出口组
```

`import` 的输出：

```
读入 42 行

  ✔  someone@example.com        入库成功
  ✔  other@example.com          入库成功
  ✘  third@example.com          password：密码不对（第 17 行）
  ✘  fourth@example.com         approve：设备码过期（第 18 行）

成功 2，失败 2。密码已加密入库（MMXCODE_PASSWORD_KEY）。
```

失败行给出**环节**和**站点的错误码**，不给人话。

## 8. WebUI

### 8.1 新页面

- `/accounts/import`：上传文件 → 开始 → 看进度和结果
- `/accounts/relogin`：列出签出且有密码的账号，一键重登

服务端渲染 + 原生 JS（跟现在一致），进度靠轮询一个 JSON 接口。

### 8.2 接口

| 方法 | 路径 | 作用 |
|---|---|---|
| `POST` | `/api/import` | multipart 上传文件，开一个任务，返回 `{"id":"…"}` |
| `GET` | `/api/import/{id}` | 任务状态与结果 |
| `POST` | `/api/relogin` | 开一个重登任务 |

### 8.3 上传的闸：登录过的人就能传（2026-11-16 改）

WebUI 上传意味着**明文密码要经过我们的 HTTP 端口**。

原来的规矩是「配了 api_keys 就关上传」：那时候那把 key 的意思是「我想让别的机器用这个网关」。现在不是了 —— 那把 key 是**后台登录密钥**，是「只有拿着它的人能进后台」的意思。所以：

- 上传入口**一直开着**：页面开着，`POST /api/import` 收着，导航里也有那个链接
- 能不能传，看的是**有没有登录**：整个后台（页面和 `/api/*`）本来就在那把 key 后面，没有它连页面都看不到
- 明文密码走不走内网，是登录的人自己的选择。不想让它出本机，就在服务器上跑 `mmxcode import --file accounts.txt`，格式和上传的一样

为什么反过来了：后台登录密钥自己也是明文走同一个 HTTP 口过去的。它过去了，明文密码就不算「不该过」的东西 —— 关上传而放密钥过去，是拿一个已经过去的票去挡另一个。

重登页面一直没有这个闸（它不接触密码明文）。

### 8.4 上传的文件落在哪

上传的内容先进内存（一个账号文件很小），不落盘。任务跑完即弃。

## 9. 存储变更

`schemaVersion` 从 3 升到 4：

**`accounts` 加一列**

```
password TEXT NULL          -- base64(nonce‖ciphertext‖tag)，NULL = 没存
```

**新表 `import_runs`**

```
CREATE TABLE import_runs (
  id          TEXT PRIMARY KEY,
  source      TEXT NOT NULL,
  started_at  INTEGER NOT NULL,
  finished_at INTEGER,
  total       INTEGER NOT NULL DEFAULT 0,
  ok          INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  results     TEXT
);
```

迁移走现有的版本化迁移机制（`store.go` 的步骤表），旧库升上来时 `password` 是 NULL，其它列照旧。

## 10. 配置变更

```yaml
batch:
  # 批量导入时每个账号之间的间隔
  interval: 3s
  # 批量导入默认走哪个出口组（空 = 全局默认出口）
  group: ""
  relogin:
    # 自动重登，默认关闭
    enabled: false
    # 同一个账号两次重登之间的冷却
    cooldown: 1h
    # 一次任务最多处理多少个账号
    max_accounts: 20
```

密码加密的密钥**不在配置里**（见 §4.2），只从环境变量 `MMXCODE_PASSWORD_KEY` 取。

## 11. 错误语义

### 11.1 站点错误码 → 人话

| `statusInfo.code` | 常量 | 人话 |
|---|---|---|
| `0` | Success | 成功 |
| `11` | `AUTH_FORBIDDEN` | 这个账号没有权限登录 |
| `1200007` | `UNAUTHENTICATED` | 登录状态无效，重试一次 |
| `1200044` | `EMAIL_PHONE_NOT_BOUND` | 这个账号还没绑手机，去网站绑了再来 |
| `1200046` | `OLD_PASSWORD_WRONG` | 密码不对 |
| `1200056` | `NEED_SET_PASSWORD_NEW` | 这个账号还没设密码，去网站设了再来 |
| `1200057` | `NEED_SET_PASSWORD_OLD` | 这个账号的密码太旧了，去网站改过再来 |
| `1200062` | `SUB_ACCOUNT_FIRST_LOGIN_NEED_RESET_PWD` | 子账号第一次登录要改密码 |
| `1000048` | sign-in refused | 站点拒绝了这个登录（mcode 侧的口径） |

未列出的码：原样报出数字，附上站点给的消息。

### 11.2 失败发生在哪一步

`read` / `probe` / `password` / `approve` / `token` / `store`——见 §5.4。

## 12. Docker 部署

```yaml
services:
  mmxcode:
    image: mmxcode/gateway:…
    environment:
      MMXCODE_PASSWORD_KEY: ${MMXCODE_PASSWORD_KEY}   # 不设 = 不存密码
    volumes:
      - ./config.yaml:/etc/mmxcode/config.yaml
      - mmxcode-data:/data
```

容器里也能导：

```bash
docker compose exec mmxcode mmxcode import --file /data/accounts.txt
```

（把账号清单挂进 `/data` 即可。）

## 13. 测试

**全部离线。** 测试绝不打 `api.minimax.io` / `account.minimax.io`——那会真的用掉一个 refresh token 或真的登一次号。

- `internal/sign`：网页 flavor 的 query 键序与签名。先测「签的就是发的」——把生成的 URL 拿来重算一遍 `yy` 和 `x-signature`，两个要对得上；再测键序是网页那份顺序（调用方的 query 在最前）。真抓包向量等 §14 里拿到原始报文后再对拍
- `internal/webauth`：用 `httptest` 起一台假的服务端，按真站点的报文形状回话——探测、密码登录、401 login_required、批准、错误码映射，逐个测
- `internal/provision`：文件解析（注释、空行、密码里有 `|`、重复邮箱、坏邮箱）、节奏、失败分类、重试退避、结果汇总
- `internal/store`：`password` 列的迁移（从 v3 升到 v4，旧行是 NULL）、密文的写入与读回
- `internal/manager`：签出 → 调度器重登 → 回到 active；冷却期内不再试；失败不动状态
- `cmd/mmxcode`：`import` / `relogin` 两个命令的参数与输出

## 14. 待实测项

1. **拿真账号跑通整条链**（只有人能做）
2. `statusInfo.code` 在「密码错」这件事上的**确切**码值（笔记里只列了常量名，没有实测对照表）
3. `group_id` 的默认组规则：`is_default` 的那组在真实账号上是否唯一
4. 大陆出口下 `account.minimax.io` 是否也被 307 到 `.cn`（笔记里只记了 `agent.minimax.io`）
5. Akamai 的 `_abck` / `bm_sz` 在批量登录时会不会真的拦（笔记里的实测是单账号直连通过）
6. 批量到多少个号会触发风控
7. **抓一个真的网页请求存成金标准**：整条 URL 加 `yy` 加 `x-signature`。命令行那边的六个向量是照着参考实现重算出来的，网页这一边目前只是照着笔记里的散文推的——两边只有一边有金标准，这个不对称才是最大的风险。抓包笔记里已经有 `x-signature` 的那半个向量（`abd7da0095474425f28458b7b56a6626` 能用 `md5(秒 + 密钥 + body)` 复现），`yy` 那一半缺的是完整 URL
8. `op_ticket` 到底在不在：抓包笔记 §4 的公共 query 里没有它，是设计文档从 js/204 里读出来的。它在 query 里就改了签名的字节
9. `uuid` 出现两次的时候，站点读的是哪一个。网页那边公共的那一份在前、自己那份在后，常见的解析器取第一个（`url.Values.Get`、`URLSearchParams.get`、Java 的 `getParameter`）——但这是猜的
10. 同一个账号导两次，站点那边认不认。我们这边是一行：第二次导入找的是令牌里
    那个用户 id（地址对不上也找得到），把这一行的令牌换掉（设计文档 §16 记了为什
    么）。但站点那边——第二次登录换来的令牌会不会把第一次那把 refresh token 作废
    ——只有真账号能试出来
11. **令牌里那个 `sub` 是不是每一次登录都是同一个。** 我们整套去重都压在这个前提
    上：导入按它找那一行，签出以后还留着它，下一次导入才不会把这个账号当成新账
    号再插一行。它要是不变（一个站点账号一个），这套就是对的；它要是会变（每一
    次登录一个），签出以后再导入就会建出第二行——那两行都是活的
12. **站点那边报出来的地址能不能拿来登录。** 我们把 `email` 那一栏写成站点自己认
    的地址（跟网页登录那一路一个规矩），掉线以后重登用的就是它。清单上那个地址
    （登录时用的）留在这次导入的结果里，库里没有那一份。MiniMax 的账号应该只有
    一个邮箱（本站账号是邮箱，Google 登录另算），但只有真账号能试出来

## 15. 与外部资料核对过的部分

- **参考插件**（`@magpie-community/opencode-minimax-auth` 的 `packages/minimax/index.mjs`）：设备授权那三步的端点、参数、轮询语义、错误语义
- **MiniMax 网页版前端**（`account.minimax.io` 的 `prod-web-va-0.1.45` bundle）：
  - `js/204` 模块 4099：签名拦截器（`yy` / `x-timestamp` / `x-signature`）与公共 query
  - `js/204` 模块 7160：RSA 公钥
  - `js/204` 模块 4099 的 `P`：`POST /oauth2/login` 的包装（`login_redirect` 从页面 URL 取）
  - `js/992`：`unified-login` 主组件（密码登录的字段、`countryCode`、各分支）
  - `js/809` 模块 375：`loginType` 枚举
  - `app/oauth-authorize/page-*.js`：`GET` / `POST /oauth2/device/authorize` 的体与 `login_required` 的处理
- **用户自己的抓包笔记**（`C:/Users/lishihua/Downloads/README.md`，2026-11-05~06）：登录时序、cookie 清单、错误码表、以及「同一个产品并行维护两套 OAuth 客户端」的结论

## 16. 做完了什么样

整个功能做完了：清单能导、密码加密存库、账号掉了能自己登回去；命令行和页面上
两套前端共用同一批任务。下面记的是实现时跟上面这份设计不一样的地方。

**网页那半边的 query 顺序反过来了。** §3.2 原先写的是「调用方的 query 在最前」
（那是照网页前端 `{...参数, ...公共参数}` 的写法推的）。真抓包里那条设备注册
的请求是**公共块在前、调用方自己那份在后**，`uuid` 因此出现两次。代码按抓包改
了，`internal/sign` 里两套 flavor 各自的注释也跟着改了。签名用就是这个顺序，
所以 `yy` 的字节跟着变了。

**`browser_language` 是 `en-US`。** 站点自己的值就是 `en-US`；换成 `zh-CN`
签名就对不上。

**上传的那份清单不许落盘。** §8.4 说清单读进内存、不写盘。但 Go 的
`ParseMultipartForm` 给多少内存就往内存里放多少，放不下的会写临时文件去——
明文密码就落盘了。所以实现改成：用 `http.MaxBytesReader` 把请求体卡在 8 MB，
给解析器 16 MB 内存，超出的那部分先被卡住，临时文件这条路走不到。

**重登页只列登得回去的账号。** 签出而且还存着密码的才列。没有密码的账号登不
回去，列出来配一个按不动的按钮比不列更糟。

**重登成功了就把旧的那条链扔掉。** 走一遍 `Renewer.Forget(accountID)`：旧的
refresh token 到此为止，不会被花掉。

**设计 §7 里有两样，原先的计划漏了，一并做了。** `import --timeout`（整趟导入
的时限）和 `mmxcode relogin` 命令。

**`.env.example` 里添了一行。** 这个文件本来就有（compose 读的那几个变量都在
里面），这一步加的是 `MMXCODE_PASSWORD_KEY`，compose 的 `environment` 里也加了
对应的一行。

**同一个账号导两次，库里只有一行。** 这条原设计没有写。实现改成：导入之前先
找一找，找着就把**那一行**更新（令牌、状态、名字、地址、用户 id、出口组、密码），
不再插第二行——插第二行的话，那个站点账号在库里就是两个活着的账号，轮转会挑到两
次，重登一次会把另一个的令牌作废。找不到才新建。写令牌时要是写失败，这一行原样
不动（绝不删）。

**找的是令牌里的那个用户 id，不是邮箱。** 邮箱靠不住：清单上写的地址可能是别名，
站点那边报的地址又可能一时问不出来（站点 503 了）。两种情况下都靠邮箱去重，就
会给同一个站点账号建出第二行——正是上面那条要防的事。令牌里带的那个 `sub` 是站点
账号自己的 id，一个站点账号一个，重登多少次都是它，所以先按它找，邮箱是兜底。
它顺着令牌那一行找，没有加索引也没有改表结构。

**找到那一行以后，地址和用户 id 要跟站点说的对齐。** 站点那边报出来的真邮箱知道了
就写上去：键停在旧的地方，下一次导入就会把这个账号当成新账号。清单上的地址是登
录用的，站点自己认的地址才写进 `email` 那一栏（跟网页登录那一路一个规矩）。

**导入的「找」和「写」在一个事务里。** 原设计没有说并发怎么办。查了再写，中间
没有锁：网页上传两趟、或者两个进程各跑一趟导入，两个都查不到、都插，于是同一个
站点账号在库里有两个活着的行，轮转会挑到两次，其中一个的令牌迟早被另一个的重登
作废。现在查找和写入（连同令牌那一次写）在一个事务里，事务一开始就把写的锁拿
住，所以后到的那一个只能等。库本来就把事务配成 `BEGIN IMMEDIATE`（`_txlock=immediate`），
所以这件事不用改表结构、也不用加索引。事务不跨网络调用：登录、换令牌这些都在
事务外面，签到那几秒里不占着写的锁。

**整趟失败的原因要留下来。** 以前网页那条路把整趟的失败原因丢了：只记了个
「失败 1」，原因既不入库也不进日志，页面上什么也看不出来——密钥配错了这件事，
人无从知道。现在原因会写进日志，也会作为一条合成的记录放进这次导入的结果里，
页面上看得见。

**整趟失败不算账号失败。** 那一行不是账号：一次账号都没处理成的这一趟，失败的
账号数是零（以前是 1，说的是「有一个失败了」，而清单上其实一个都没轮到）。失败
的账号数是账号的事，整趟为什么没成由那一行自己说，页面上两句都在。

**待实测的项从 6 条长到 10 条。** 多出来的四条在计划文件的末尾：抓一个真的网页
请求存成金标准、`op_ticket` 到底在不在 query 里、`uuid` 出现两次时站点读的是哪
一个、同一个账号导两次时站点那边认不认（我们这边已经是一行了，剩下的是站点
那边会不会把第一次那把 refresh token 作废）。

**重登的排队只在一个进程里算数。** 同一个账号不会被两个重登同时做：那把锁在
`internal/manager` 里，是进程内的。所以一个正在跑的 `mmxcode serve` 和一个
`mmxcode relogin` 命令对着同一个库跑的时候，两边可能同时登同一个账号——站点那
边看见两次登录，令牌也是后写的那一份赢。没有为此改表结构来做跨进程的排队（那
要一张锁表，还要处理锁自己没了、人也回不来的情况）。这一条写在这里是要人知道：
不要一边开着 serve 一边跑 relogin。

**输的那一次登录失败，不会把赢家刚换好的令牌作废。** 排队只在一个进程里算数，
所以两个进程可能同时登同一个账号：一个成了，另一个没成。没成的那一个原来会把
状态写回 `signed_out`，把赢家刚写好的 `active` 盖掉——账号显示成签出的状态，手
里却握着一份能用的凭据：轮转池不用它，下一轮又要登一次，白白多登一次。现在写
失败原因是有条件的（只在那一行现在还是签出的时候才写），赢家写好的状态不会被
盖掉。

**导入带的出口组压过账号自己的地址。** 账号上有个手设的出口地址时，再导入一次
（带 `--group`），那个地址会被清掉：出口是按「先看地址、再看组」找的，地址留着
就还是从原来那个地方出去，组等于白写。导入是人明确做的一件事，它带的组说了算。
没带组的导入不动原来那一行的组和地址——那是人自己设的。

**网页上传用的也是配置里那个常驻的组。** `batch.group` 配了以后，网页上传和命令
行（不带 `--group`）都走这个组，所以手填的地址照样会被清掉。配置文件的注释和
README 里都写着这件事：那个组是常驻的，不是这一趟临时说的。

**被清掉的是哪个地址，要写在结果里。** 那是一个人自己设的地址，没了就得能照着
填回去，所以那一行里写的是完整的地址，不只是一句「清掉了」。

**重登只认签出状态，别的状态一概不登。** `Relogin` 现在只对 `signed_out` 的账号
动手：`active` 当然不用登，`error`（上次读账号出过错）也不登——以前
`mmxcode relogin --account <出过错的那个号>` 是会替它登一次的，现在它会明说「这
个账号不是签出状态，不用重登」。这是有意的：出过错的那个号得人看一眼（是密码
错了，还是站点那边出事了），机器不该悄悄把它盖过去。重登失败也不再把令牌抹
掉：原来那条链还有用，抹掉了就是毁掉一个还能用的账号。

**导入的汇总说的是「已处理」。** 以前那行写的是「共 N 个」，其实 N 是清单里的
个数，不是处理了的个数：一趟导入被中途截断的时候，那个数比真正走过的大。现
在写「已处理 N 个，成功 X，失败 Y」，另外单独一行说清单里还有几个没轮到、这趟
是被什么截断的。

**签出的时候，去重用的那个键留着。** 签出是要收走令牌的：站点不认这个登录了，
这份凭据没有用。可令牌行里的 `subject` 得留着——那是站点那边账号自己的 id，是
「这个站点账号在库里是哪一行」的键。签出的账号恰恰是最要紧的那一个：操作的人
看见它掉线了，第一件事就是重新导入它。键要是被抹掉，那一次导入就会当成新账号
再插一行，而那一行是活的：同一个站点账号两行都是活的，轮转会挑到两次。

**签出是有条件的：手里那份令牌还是库里那一份，才写。** 站点拒了手里这份令牌的
续期，可是这期间另一个进程把这账号登回来了（库里换成了新令牌）——这时候把签出
写下去，就是把别人刚登好的账号作废掉，还把那份新令牌收走：页面说这个账号登出
了，轮转池也不用它，而它手里握着一份能用的凭据。现在签出和写错误状态都是这个
条件：这次失败说的是手里这份令牌，库里换成了别的，说的就不是这个账号了。状态
和收走令牌是一个事务里一起写的——中间插进来一个写，账号就会停在「状态是签出、
令牌还在」的样子。

**网页登录那条路也去重。** 以前 `mmxcode login` 和页面上那个登录都是直接建一
行：登录一个批量导入进来的账号，库里就多一个活着的行，同一个站点账号两行都是
活的。现在登录也走库里那个「SignIn」：按令牌里的 `sub` 找那一行，找着就改它
（令牌、状态、地址、用户 id），找不到才新建。

**地址不跟着别人走。** 站点那边报出来的地址要是已经有别的账号在用，就不换过
去：两个账号共用一个地址，掉线以后重登分不清该拿哪个密码去登——密码是对着其中
一个的，登错了就把另一个也登坏了。这种时候地址留在原来那一行上，结果里说清楚
是哪一个账号占着它。

**重登的理由要带回来。** 以前 `ReloginAll` 把失败的原因丢了，只留下「成功几个、
失败几个」：连账号都没列出来（这一趟根本没跑）和「都登回去了」在只看着两个数
字的时候是一样的。现在它给回失败的原因（哪个账号、为什么），连同「这一趟没能
开始」也当错说；命令行、页面和调度器的日志里都摆着。

**整趟失败在历史里也要看得见。** 网页上「最近的导入」那几行，成功和失败都是零
的时候看不出这一趟出了什么事——像是什么都没干。现在那一栏写着整趟失败的原因，
刷新一次页面也还在。

**地址写进去之前要去空格。** 地址是去重的键之一，同一个地址写成两种样子（一边
带空格一边不带），就会看成两个账号。
