# mmxcode-auth

把 magpie 接到自己的 **mmxcode 网关**上：聊天走网关（轮转、多账号、故障转移都在网关里），余额卡片问网关要（一次请求答完）。

## 它是什么

magpie 的 provider 插件，provider id 是 `mmxcode`，在 magpie 里叫 **MMXCode 网关**。

插件里没有 MiniMax 的账号、令牌和签名 —— 那些全在网关里。插件只认得两样东西：

- **网关的地址**（默认 `http://127.0.0.1:8080`，也可以用环境变量 `MMXCODE_BASE` 指）；
- **一把聊天用的 key**（网关后台 `/keys` 申请的；网关一把 key 都没有的时候可以留空）。

## 怎么装

```sh
magpie plugin add ./packages/mmxcode
```

然后在 magpie 里签入 `MMXCode 网关`：填网关地址和聊天 key。

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
magpie plugin add ./packages/mmxcode
```

Signing in asks for three things: the gateway's address, the chat key, and the **account id** — the name magpie shows for that account (leave it empty and the address becomes the name).

The gateway speaks Anthropic, Chat Completions and Responses. magpie talks Anthropic to the plugin's provider (a plugin provider has one protocol) and translates for clients that speak something else; clients that point at the gateway directly can use any of the three.

It has no check-in button (checking in is the gateway's job) and no tools. It runs beside magpie's own `minimax-code-global`, under the provider id `mmxcode`.
