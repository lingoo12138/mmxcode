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
