# mmxcode-auth

把 magpie 接到自己的 **mmxcode 网关**上：聊天走网关（轮转、多账号、故障转移都在网关里），余额卡片问网关要（一次请求答完）。

## 它是什么

magpie 的 provider 插件，provider id 是 `mmxcode`，在 magpie 里叫 **MMXCode 网关**。

插件里没有 MiniMax 的账号、令牌和签名 —— 那些全在网关里。插件只认得两样东西：

- **网关的地址**（默认 `http://127.0.0.1:8080`，也可以用环境变量 `MMXCODE_BASE` 指）；
- **一把聊天用的 key**（网关后台 `/keys` 申请的；网关一把 key 都没有的时候可以留空）。

## 怎么装

一条命令：

```sh
magpie plugin add github:lingoo12138/mmxcode
```

magpie 把整个仓库当成一个 npm 包装下来，照着根目录 `package.json` 的 `main`
找到 `packages/mmxcode/index.mjs`。

自己改插件的话，从源码装（改完重启 magpie 就生效）：

```sh
git clone https://github.com/lingoo12138/mmxcode.git
magpie plugin add ./mmxcode/packages/mmxcode
```

装完在 magpie 里签入 `MMXCode 网关`：填网关地址和聊天 key。

不想点表单就喂给它 —— 地址、key、账号 id，一行一个：

```sh
printf 'http://127.0.0.1:8080\n\n\n' | magpie plugin login mmxcode
```

签入的时候它会真打一次网关：网关没起来就直接说「网关连不上」，不会给你
留一个半签入的账号。

## 签入的时候问三样

| 问的 | 是什么 | 留空的话 |
|---|---|---|
| 网关地址 | 网关在哪儿，比如 `http://127.0.0.1:8080` | `http://127.0.0.1:8080` |
| 聊天用的 key | 后台 `/keys` 申请的那把 | 网关一把 key 都没有的时候可以留空 |
| **账号 id** | magpie 里显示这个账号的名字 | **用网关地址当名字** |

账号 id 留空的话，magpie 的账号列表里显示的就是那个地址 —— 一眼看得见是哪个网关。

想加另一个网关（或者同一把网关、另一把 key），再签入一次就是一个新的账号；不要了在 magpie 里删掉那个账号。

## 协议

网关对外讲三种协议：

| 协议 | 路径 |
|---|---|
| Anthropic | `/v1/messages` |
| Chat Completions | `/v1/chat/completions` |
| Responses | `/v1/responses` |

- **magpie 跟网关讲的是 Anthropic**（插件的 provider 只能有一种协议，宿主按插件声明的 `npm` 定）。Claude Code 走的就是它，不用翻译。
- **别的客户端 magpie 自己翻**：`chat/completions` 的请求，magpie 会翻成 Anthropic 再问网关（实测 magpie 的网关答了 200）。所以一个 provider 就够用，不必再加 `mmxcode-chat`。
- **直接指网关的客户端**（不经过 magpie 的 Claude Code、Codex、脚本）按自己的协议说话：三种都收。

## 卡片

卡片是网关给的，一个请求答完整个池子。上面是：

- **Credits**：全池的积分（`used` 是用掉的份额）；
- **账号**：几个账号、几个有读数、几个没读到；
- **有话说的账号**：用过积分的和用完的，按用得最多的排前，最多十个；
- **协议**：网关讲的三种。

没读到的账号不写成「0」—— 是「没读到」。

## 在 magpie 里改地址、密钥、协议

插件的 provider（**MMXCode 网关**）在 magpie 的供应商页面上**没有**「接口地址 / 接口密钥」那两个框 —— 那是宿主给**自建 provider** 的。插件的 provider 是 magpie 内部的一个 provider（宿主把它的地址换成 `plugin://`，所以编辑器的「端点」那一栏也不出现）：它的地址和密钥在签入的时候问，换地址就再签入一次。

要一个能改地址、密钥、每把 key 选协议的页面，把网关加成**自建 provider**：

```sh
magpie provider add mmxcode-direct id=mmxcode-direct "name=MMXCode 网关（直连）" \
  anthropic=http://127.0.0.1:8080 \
  chat=http://127.0.0.1:8080/v1 \
  responses=http://127.0.0.1:8080/v1 \
  key=sk-… \
  models.url=http://127.0.0.1:8080/v1/models \
  balance="http://127.0.0.1:8080/v1/usage?refresh=1" balance.path=balance
```

那一页上就有了：

- **接口地址**：分协议的几张标签页（Anthropic 兼容 / OpenAI 兼容 / OpenAI Responses / Gemini 兼容）—— 网关三种都收；
- **接口密钥**：密钥那一行，可以改地址、换 key、再加一把；
- **协议**：每把 key 旁边一个下拉（任意协议 / Anthropic / Chat Completions / Responses）—— 填了三个协议地址它就有得选；
- **余额**：从网关的 `/v1/usage` 读。

两个供应商的分工：

| | MMXCode 网关（插件） | MMXCode 网关（直连，自建） |
|---|---|---|
| 模型 | 有 | 有 |
| 一段对话黏在一个账号上 | 有（插件送 `X-Session-Id`） | 没有（宿主不转发会话 id） |
| 用量卡片 | 有（积分、账号、协议行） | 只有余额 |
| 地址 / 密钥 / 协议能改 | 不能（在签入表单里） | 能 |

两个都在模型列表里，agent 各挑一个用。

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

The plugin holds no MiniMax account, token or signature — the gateway holds those. It knows the gateway's address (default `http://127.0.0.1:8080`, or the `MMXCODE_BASE` environment variable) and a chat key (one you apply for in the gateway's console; leave it empty when the gateway has no keys at all).

```sh
magpie plugin add github:lingoo12138/mmxcode
```

magpie installs the repository as one npm package and loads
`packages/mmxcode/index.mjs` through the root `package.json`. Working on the
plugin itself? Install it from a clone instead:
`magpie plugin add ./mmxcode/packages/mmxcode`.

Signing in asks for three things: the gateway's address, the chat key, and the **account id** — the name magpie shows for that account (leave it empty and the address becomes the name).

The gateway speaks Anthropic, Chat Completions and Responses. magpie talks Anthropic to the plugin's provider (a plugin provider has one protocol) and translates for clients that speak something else; clients that point at the gateway directly can use any of the three.

The plugin's provider page has no address or key field of its own — that is a host feature for providers you added yourself (a custom provider). Add the gateway as a custom provider to get one:

```sh
magpie provider add mmxcode-direct id=mmxcode-direct "name=MMXCode 网关（直连）" \
  anthropic=http://127.0.0.1:8080 chat=http://127.0.0.1:8080/v1 responses=http://127.0.0.1:8080/v1 \
  key=sk-… models.url=http://127.0.0.1:8080/v1/models \
  balance="http://127.0.0.1:8080/v1/usage?refresh=1" balance.path=balance
```

That page has the endpoint (one tab per protocol), the key, a protocol per key, and the balance read from the gateway.

It has no check-in button (checking in is the gateway's job) and no tools. It runs beside magpie's own `minimax-code-global`, under the provider id `mmxcode`.
