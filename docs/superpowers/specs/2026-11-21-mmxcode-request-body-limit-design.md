# 请求体：闸门在读身体之前，上限 64 MiB —— 设计

日期：2026-11-21
状态：待定。
计划：`docs/superpowers/plans/2026-11-21-mmxcode-request-body-limit.md`。

上一份：`docs/superpowers/specs/2026-11-21-mmxcode-upstream-body-limit-design.md`
（那份管的是**回包**；这份管的是**请求体**。那份的 §5 有一行「改请求那边那道闸
（`maxBodyBytes = 32 MiB`）—— 但那是另一件事，这次不动它」，这份就是那件事。）

## 1. 原话

用户问：「maxBodyBytes遇到用户调用模型时差不多使用了【1m 】的上下文呢？」

我算的是：

> **1M token 的上下文是 4 MB 上下**（仓库自己的注释就这么写：
> `internal/upstream/client.go`：「它的输入上限（1048576 个 token）是 4 MB 上下」）。
> 带上 JSON 外壳是 4.5–5 MB。
>
> `maxBodyBytes = 32 MiB` 是站点自己那个输入上限（1,048,576 token）的 **8 倍**。
> 作为上限它不算小 —— 问题是**读身体和排队闸门的顺序**。

现在的代码（`internal/gateway/gateway.go` 的 `handleChat`）是：

```
读整个请求体（最多 32 MiB）→ 排队等闸门 → 记在"在飞"里 → 问上游
```

排在一百个请求后面的那每一个，**都已经把身体读进内存了** —— 那是 400 MB，跟承诺
的并发一点关系都没有。闸门管的是"在飞"，而等着的那一批占的内存根本不在闸门的
账上。

我量过了（100 个 4 MB 的请求同时打进来，闸门限 20）：

| | 峰值常驻 | 峰值堆 |
|---|---|---|
| 现在（先读后排队） | 886 MB | 821 MB |
| 闸门挪到读身体之前 | 267 MB | 199 MB |

**3.3 倍。**

然后我说的是：

> 1. 把闸门挪到读请求体**之前**：等着的时候身体还在客户端那边，网关没有替它占
>    内存。**3.3 倍。**
> 2. 请求体的上限我本来想收到 8 MiB（站点的输入上限是 4.2 MB）—— 但你说要
>    64 MB，因为用户可能传图片。

用户答：「1：可以。2:改成64MB,我之前设置 64MB是因为考虑到用户有可能传图片」

**图片是真的**：参考插件的模型名单写着 `inputs: ["text", "image", "video"]`
（`E:/tmp/magpie-plugins-ref/packages/minimax/index.mjs:165`），而且插件把身体
原样转发（`index.mjs`：「magpie the gateway writes the Anthropic body itself and
the fetch below sends it on as it is」）。图片是 base64 写在身体里的，一张图几 MB
是常态。**32 MiB 太小了，64 MiB 才对。**

## 2. 1M token 是多少 MB

| 文字 | 每个 token 几个字节 | 1M token ≈ |
|---|---|---|
| 中文（UTF-8，3 字节/字，≈1 token/字） | ~3 | ~3 MB |
| 英文散文（≈4 字符/token） | ~4 | ~4 MB |
| 代码/JSON（转义把 `\n` 变成两个字节） | ~2.5–3 | ~2.5–3 MB |

**所以：1M token 是 3–4 MB**，带 JSON 外壳 4.5–5 MB。

对照站点自己收的数（`internal/site/models.go`）：

| 量 | token | 折成字节 |
|---|---|---|
| 一次请求的上限（输入） | 1,048,576 | 3–4.2 MB |
| 一次回答的上限（输出） | 524,288 | 1.6–2.1 MB |

**64 MiB 是那个输入上限的 15–20 倍**，也是一张 base64 图片的十几倍 —— 对"用户会
传图片"这件事来说是宽裕的。

## 3. 找到的洞

### 洞一：身体在排队之前就被读进来了

`internal/gateway/gateway.go` 的 `handleChat`：

```go
r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
body, err := io.ReadAll(r.Body)          // ← 整个身体进内存
...
if !s.gate.waitFor(ctx, s.concurrencyLimit(ctx), ...) {   // ← 才排队
```

闸门是网关对外面**承诺的并发**：在这个数里面，请求才占着内存。请求体最大 64 MiB
—— 闸门后面等着的一百个请求，每一个都已经把身体读进内存了的话，那是 **6.4 GB**，
跟承诺的并发一点关系都没有。

量过的数（100 个 4 MB 的请求，闸门 20）：

| | 峰值常驻 | 峰值堆 |
|---|---|---|
| 先读后排队 | 886 MB | 821 MB |
| 先排队后读 | 267 MB | 199 MB |

### 洞二：32 MiB 对图片不够

图片是 base64 写在身体里的（胀三分之一），一张图几 MB 是常态，用户传几张就上三
十兆。32 MiB 那道闸会让**正经的请求**（带图片的）被 413 挡掉。

### 洞三：往上游发的时候又抄了一遍

`internal/upstream/chat.go`：

```go
strings.NewReader(string(req.Body))
```

`string(req.Body)` 把整个身体**再抄一份**。64 MiB 的身体在飞的时候，内存里躺着
两份：一份在网关的 `body` 变量里，一份在这个 `string` 里。

量过的（一个 4 MB 的身体走一遍 `Chat`）：

| | 每次的分配 | 每次的时间 |
|---|---|---|
| `strings.NewReader(string(req.Body))` | 4,202,038 B | 811 µs |
| `bytes.NewReader(req.Body)` | 4,794 B | 4.7 µs |

**每次少分配 4.2 MB —— 那就是身体本身的大小。**在飞的时候也是同样的一份内存。

## 4. 改什么

### 4.1 闸门挪到读请求体之前

```go
ctx := r.Context()
if !s.gate.waitFor(ctx, s.concurrencyLimit(ctx), s.Cfg.Concurrency.Wait.Duration()) {
    w.Header().Set("Retry-After", "5")
    writeError(w, proto, http.StatusTooManyRequests, "the gateway is busy: too many requests are in the air")
    return
}
defer s.gate.give()
r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
body, err := io.ReadAll(r.Body)
```

**排队的意思变成了它本来的意思**：在这个数里面，请求才占着网关的内存。

**代价**：排队的时候客户端在往上传身体，网关没读 —— 客户端可能因为 TCP 背压而
堵着，而网关可能先给它一个 429。这是实话：**网关在忙的时候不再替别人的身体占
内存**。429 以后 Go 会试着把身体的前 256 KB 读完（好让连接还能用），读不完就把
连接关掉。

**另一件实话**：一个慢上传（一个字节一个字节地挤）现在占着一个闸门名额，直到它
传完或者客户端自己断了。服务器的 `ReadHeaderTimeout` 是 10 秒，但**身体**没有时限
（`cmd/mmxcode/serve.go` 只给了 `ReadHeaderTimeout`）。这是"闸门数 = 同时在飞的
请求数"这个承诺的直接后果 —— 一个上传中的请求就是在飞的请求。

### 4.2 上限 64 MiB

```go
const maxBodyBytes = 64 << 20
```

理由写在代码的注释里：用户可能传图片。

**这个数怎么变成内存**：在飞的请求数 × 64 MiB 是**最坏情况**。闸门限 20 的话是
1.28 GB。平常不是那么回事 —— 1M token 的对话是 4 MB，20 个是 80 MB。**要压这个
数就压 `concurrency.total_max`**，那是承诺的并发。

### 4.3 往上游发的时候不抄那一遍

```go
bytes.NewReader(req.Body)
```

`bytes.Reader` 读的是原来那块 `[]byte`，不抄。`req.Body` 在 `Chat` 里面不会被改，
所以没有别名上的问题。

## 5. 不做什么

| 不做 | 为什么 |
|---|---|
| 把请求体的上限做成配置项 | 回包那边已经有 `upstream.max_body_mb` 了。请求这边 64 MiB 是个"够用"的上限，不是要人调的旋钮；真嫌小就改代码 |
| 给在飞的身体加一道"总字节数"的闸 | 那是另一件事（要算在飞的字节、要超时）。闸门限 20 已经把在飞的身体数限住了 |
| 把身体读四次的改成读一次 | 身体确实被 `json.Unmarshal` 了四遍（`modelOf`、`isStream`、`systemOf`、`firstUserOf`）：量出来 84.7 ms/op、4.9 MB/op，换成一次是 22.9 ms/op、304 B/op。改它要动 `gateway.go` 里的解析（三种协议的形状不一样），是另一份计划的事 |
| 给 64 MiB 的身体做流式转发 | 身体是客户端的 JSON，网关不重写它 —— 转发就是转发整个，读进来是最简单的写法 |
| 改回包那边那道闸 | 那是上一份规格的事，已经做完了（`upstream.max_body_mb`，默认 8 MiB） |

## 6. 行为变了的地方

1. **排队的时候网关不读身体**：排在闸门后面的请求，它的 body 还在客户端那边。
2. **超限的身体答 413** —— 从前也是 413，那个没变；但**超限的请求现在会占一个
   闸门名额**（从前它读完就拒了，不占名额）。
3. **上限从 32 MiB 变成 64 MiB**。
4. **429 的路径不再读身体**：连接多半会被关掉（Go 读完前 256 KB 才复用）。
5. **往上游发的时候身体不再被抄一遍**（少一份 64 MiB 的复制）。
6. **说了大小的超限请求连读都不用读**：客户端在请求头里说了 `Content-Length`
   的话，超限的那个当场就拒了 —— 一个字节都没读进来（从前是读完 64 MiB 才发现
   超了）。分块传的客户端没说，那种还是读，`MaxBytesReader` 照旧挡着。
7. **413 的消息里带着那个数**：那个上限是多少（字节和 MiB 都报）、客户端自己
   说了多大。回包那边那道闸早就是这么说的。

## 7. 怎么量

- **等着的时候身体没被读**：把闸门占满，发一个请求 → 客户端拿到 429，而那个读
  身体的东西**一个字节都没读到**。
- **超限被拒**：64 MiB + 1 的身体 → 413，上游一次都没被问。
- **超限的请求一个字节都没读**：请求头里说 `Content-Length: 67108865` → 413，
  而那个读身体的东西**一个字节都没读到**。
- **413 说的是那个数**：消息里有那个上限（67108864、64 MiB），也有客户端自己说的
  那个大小。
- **64 MiB 收得下**：一个 8 MiB 的身体（远超 1M token 的 4 MB）→ 200，上游收到的
  身体跟发过去的一模一样。
- **往上游发的身体没被抄**：`bytes.NewReader` 不复制 —— 一次 4 MB 的身体走一遍
  `Chat`，分配从 4,202,038 B 变成 4,794 B。

## 8. 风险

| 风险 | 怎么办 |
|---|---|
| 64 MiB × 20 并发 = 1.28 GB（最坏情况） | `concurrency.total_max` 是那个旋钮。平常 1M token 的对话是 4 MB，20 个是 80 MB |
| 慢上传占着闸门名额 | 身体的读没有时限（`ReadHeaderTimeout` 只管头）。真被占满了，把 `concurrency.total_max` 调小，或者给服务器加一个身体的读时限 |
| 429 的连接多半被关掉 | Go 只在读完前 256 KB 时才复用连接。客户端重连就是了 —— 429 本来就该带 `Retry-After` |
| 超限的请求现在占名额 | 它占的是"正在读一个 64 MiB 身体"的名额 —— 那本来就是它占的 |
| `Content-Length` 是客户端说的，它可以说谎 | 说了谎也逃不掉：`MaxBytesReader` 照旧挡着，而 Go 自己按 `Content-Length` 读（说 10 字节就只读 10 字节）。早退只是省了那一次读，不是唯一的闸 |
| 早退在闸门之前 | 超限的请求拿到的是 413，不是 429 —— 它本来也不会被服务，先说清楚比先排队强 |
