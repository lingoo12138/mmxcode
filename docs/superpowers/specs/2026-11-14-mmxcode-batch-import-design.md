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

网页的 axios 拦截器把公共 query 拼在**调用方 query 之后**（JS 的 `{...params, ...common}`：调用方的键在前，后来的只改值不改位置），然后最后加 `client`：

```
[调用方的 query]                              ← 设备注册时是 uuid
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
browser_language=zh-CN
browser_platform=Win32
user_id=<realUserID>                          ← 登录前是 0
op_ticket=<空>                                ← 网页在登录前发的是字面量 "undefined"
screen_width=1280
screen_height=800
client=web
```

设备注册：

```
POST https://account.minimax.io/v1/api/user/device/register?uuid=<uuid4>
body: {"uuid":"<uuid4>"}
-> 200 {"data":{"deviceIDStr":"574786429807779842"}}
```

`device_id` 建议做（它参与后续请求的签名参数）；埋点和风控信标可以不做。

### 3.3 我们要怎么改 `internal/sign`

现在 `internal/sign.Sign` 把 mcode 那套默认值写死了（`device_platform=mcode`、`browser_name=mcode`、`client=mcode`，调用方的 query 插在中间）。网页这套不一样：

- 调用方的 query 在**最前**（mcode 是在中间）
- `device_platform` / `browser_name` / `client` 的值不同
- 多出 `uuid` / `device_id` / `screen_width` / `screen_height` / `device_memory` / `cpu_core_num` / `browser_language` / `browser_platform` / `op_ticket`

改法：`sign.Options` 加一个 `Flavor` 字段（`sign.FlavorMCode` / `sign.FlavorWeb`），每个 flavor 各自一份默认 query 与插入位置。**默认仍是 mcode**，现有的调用方一行不用改。

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

### 8.3 安全闸：上传只在没有 api_keys 时开放

WebUI 上传意味着**明文密码要经过我们的 HTTP 端口**。

- `server.api_keys` 为空时，服务端本来就强制只听 `127.0.0.1`，明文密码不出本机 → **开放上传**
- `server.api_keys` 非空时，服务端可能被别的机器访问 → **关闭上传入口**，页面显示「已配置 API key，出于安全考虑不开放上传；请用 `mmxcode import --file` 在本机执行」

重登页面不受这个闸限制（它不接触密码明文）。

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
