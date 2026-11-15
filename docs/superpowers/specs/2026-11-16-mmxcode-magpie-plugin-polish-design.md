# magpie 插件打磨 —— 地址、id、协议说清楚；额度卡片瘦身

> 日期：2026-11-16
> 相关：[[2026-11-15-mmxcode-magpie-plugin-design]]（插件本身的设计）、[[2026-11-15-mmxcode-magpie-card]]（卡片的形状）

## 1. 这次要解决什么

用户的三句话（原话）：

- 「我们的magpie扩展要可以加接口地址和id」
- 「我们的账号也要像[Image #1]一样可以用的协议等都要清楚一些」
- 「还有我们的用量卡片，太多信息堆积了。删减一些」

拆成三件事：

1. **地址和 id**：在 magpie 里加我们的网关时，地址能填、id 能定，并且看得见。
2. **协议说清楚**：我们的账号（网关后面的 98 个号）能被哪些协议问到，这件事实要摆在明处。
3. **卡片瘦身**：现在的卡片是 98 个窗口（每个账号一行「xxx@hotmail.com · Credits 0 / 400」），堆得没法看。

## 2. 宿主的事实（实测出来的，不是猜的）

这一节是这次改动的基础。每一条都在本机的 magpie 上跑过，方法写在括号里。

### 2.1 插件的 provider 只有一个地址、一种协议

- 插件的 `config` 钩子能声明的 provider 字段，宿主读的就是这几个：`name`、`env`、`options`、`npm`、`api`、`decide`、`searches`、`models`（插件运行时的 `info()` 里：`npm: cfg?.npm ?? md?.npm`、`api: cfg?.api ?? md?.api`）。
- `api` 只能是**一个字符串**。给它一个 `{anthropic, chat, responses}` 的对象，宿主当字符串读，多出来的地址没有（实测：改完 `magpie model api mmxcode/MiniMax-M3` 仍然说「只有一个 API」）。
- **协议由 `npm` 定**。同一个插件、同一个账号，只把 `npm` 从 `@ai-sdk/anthropic` 改成 `@ai-sdk/openai-compatible`，宿主跟网关说的就换成 `POST /v1/chat/completions`（实测：网关前面架一个记路径的代理，看得见路径）。
- **每个模型自己的 `npm` 不算数**。模型行里的 `npm` 改成 openai-compatible，宿主还是用它自己的 Anthropic（实测：宿主仍报「✓ anthropic」）。
- 宿主对插件 provider 明确说「只有一个 API，没有可选的」；`magpie model api mmxcode/MiniMax-M3 chat` 被拒。
- Image #1 里「每一把 key 旁边那个协议下拉」（任意协议 / Anthropic / Chat Completions / Responses）是宿主给**自建 provider**（`providers.json`）的功能：下拉里的选项是 provider 自己有哪些地址（`have`），插件 provider 只有一个地址，所以没有下拉。

### 2.2 客户端的协议不用我们管

- **magpie 自己会翻译**：拿 `chat/completions` 的请求去问 `mmxcode` 的模型（`/v1/models` 里它的 `native_endpoints` 只有 `["/v1/messages"]`），magpie 把它翻成 Anthropic 再问网关，答得出来（实测：magpie 的网关 `127.0.0.1:3425` 答了 200）。
- 所以：**一个 provider 就够**。magpie 跟网关讲 Anthropic（Claude Code 的原生协议，不用翻译），别的客户端 magpie 自己翻。
- 我们网关本身三种都收（`/v1/messages`、`/v1/chat/completions`、`/v1/responses`，见 `internal/gateway/gateway.go`）——那是给**不经过 magpie 的客户端**（直接指网关的 Claude Code、Codex、脚本）用的。

### 2.3 地址和 id 在哪儿能改

- 插件 provider 的 **id 是插件定的**（`mmxcode`），宿主不改，也没有改名的地方。
- 插件 provider 的编辑器里**没有 Base URL、没有 More endpoints**（那是自建 provider 才有的）。
- 插件 provider 的「添加」就是**签入表单**：我们自己的 prompts（地址、key、名字）。
- 一个 provider 可以有**多个账号**：再签入一次就多一个账号（`magpie plugin login mmxcode`），旧的能删。
- 账号的名字由我们给（`metadata.email`）。不给的话，magpie 显示的是「API key」。

## 3. 做什么

### 3.1 地址和 id（插件侧）

- 签入表单三个提示，说清楚各自是什么：
  - `base`：网关地址（默认 `http://127.0.0.1:8080`，环境变量 `MMXCODE_BASE` 兜底）。
  - `key`：聊天用的 key（后台 `/keys` 申请的；网关一把 key 都没有时可以留空，环境变量 `MMXCODE_KEY` 兜底）。
  - `label`：**账号的 id**（magpie 里显示的名字）。留空就用网关地址。
- 账号名 = label，没有 label 就用地址 → magpie 的账号列表里一眼看得见是哪个网关。
- 地址和 id 都能改：再签入一次（`magpie plugin login mmxcode`）就是一个新的地址 + id；不要了就在 magpie 里删掉那个账号。
- 不做：插件 provider 的 id 可配（宿主定死，一个插件一个 id）。

### 3.2 协议说清楚

- **卡片上一行**：网关对外讲的协议写在卡片上（Anthropic / Chat Completions / Responses），每个账号后面都是这三种（网关自己翻）。
- **provider 名**：`MMXCode` → `MMXCode 网关`（`magpie providers` 那一列一眼认得）。
- **README**：写清三件事 —— magpie 跟网关讲的是 Anthropic；别的客户端 magpie 自己翻；网关本身三种都收，直接指网关的客户端按自己的协议说话。
- 不做：第二个 provider id（`mmxcode-chat` 之类）。理由：magpie 自己会翻译（§2.2），多一个 provider 只是多一份模型名单和一张卡，是噪声不是能力。真要 magpie 直接跟网关说 chat/completions，再议。

### 3.3 卡片瘦身

现在（实测，`GET /v1/usage?refresh=1`）：`windows` 有 **98** 个，每个账号一个 `Credits` 窗口（名字带账号邮箱、`used` 是百分比、`display` 是「31.54 / 400」、外加 `amount`/`limit`/`unit`/`runsOutAt`/`runsOut`），另有一个 `mmxcode` 块（`read_at`、`accounts`、`readings`、`unread`、`unreadable`）。

之后：

| 位置 | 内容 |
| --- | --- |
| `plan` / `until` | 不变 |
| `balance` | 不变：池里剩下的积分加起来 |
| `windows[0]` | **Credits（全池）**：`used` 是全池用掉的份额，`display` 是「282.44 / 39200」 |
| `windows[1]` | **账号**：`display` 是「98 个 · 96 有读数 · 2 没读到」，`aside`（没有 `used`：不是 0，是不适用） |
| `windows[2..]` | **有话说的账号**：用过积分的（`used > 0`）和用完的，按用得最多的排前，**最多 10 个** |
| 之后 | 账号的**计划窗口**（照旧，只有有计划的账号才有） |
| 最后一行 | **协议**：`display` 是「Anthropic · Chat Completions · Responses」，`aside` |

只有账号的时候不摆全池那一行（账号自己的窗口就是全池的窗口），也不摆「账号」那一行（它自己说了）。

去掉的：

- 每个账号**一行**（98 行「xxx@hotmail.com · Credits 0 / 400」）—— 换成上面那个最多十行的名单。这是卡片没法看的真正原因，不是每行的字段多。
- `mmxcode` 块里 `readings` / `unread` / `unreadable` —— 搬到了卡片上看得见的地方（「账号」那一行的 `display`），留着 `read_at` 和 `accounts`。

留着的：

- 每个窗口上的 `amount` / `limit` / `unit` —— `display` 说了同样的数，但客户端拿 `amount`/`limit` 算余量比读字符串靠得住。
- 每个窗口上的 `runsOut` / `runsOutAt` —— 积分什么时候过期、还剩多少，是客户端要显示的东西。
- 计划窗口的 `resetsAt`（重置时间）。

## 4. 三条不变量（跟原来一样）

1. **形状是 magpie 的**：`plan`、`until`、`balance`、`windows`。我们自己多知道的收在 `mmxcode` 那一个名字下面。
2. **没读到的不是零**：读不到的账号不算进 `balance`，也不写 `used`（指针空的就不写这个字段）。
3. **卡片是整个网关的**：`balance` 是所有账号剩下的积分加起来；计划取账号里最重的那个。

## 5. 怎么验

- 单元测试（`internal/card`）：98 个账号的读数 → 卡片上是 2 个汇总窗口 + 有话说的那几行；`used` 为空的账号不写成 0；`balance` 不变；计划窗口还在。
- 单元测试（插件，`packages/mmxcode`）：签入时留空 label → 账号名是网关地址；填了 label → 账号名是 label；`baseOf` 的兜底顺序（签入的值 → 环境变量 → 默认）。
- 本机实测：重启网关 → `magpie provider test mmxcode` 通过 → 卡片在 magpie 里只有几行。

## 6. 待实测

1. magpie 卡片上「aside 的窗口」（没有 `used` 的）画出来是什么样 —— 参考实现里 Unlimited 的窗口就是这个样子，但我们的「账号」那一行是中文的、数量是活的。
2. 最多 10 个账号这个数合不合手：98 个号的时候，卡片上是 12 行上下（2 汇总 + 最多 10 个账号）。用户要是觉得还是多，改 `card` 里的那个数就行。
3. provider 名改成「MMXCode 网关」之后，magpie 的 provider 列表和插件页显示的样子。
