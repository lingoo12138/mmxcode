# mmxcode

MiniMax Code 国际站的多账号网关。

## 做什么

- **登录**：MiniMax Code 自己的 device flow（PKCE，client `mcode-public`）。不读写 MiniMax Code 自己的 `~/.minimax` 登录信息。
- **多账号**：每个账号一份凭据，各自独立。
- **签到**：每个账号每天签到一次，签到得到的积分进各自的账本。
- **轮转**：会话粘在一个账号上，配额耗尽时自动换号。
- **代理**：按账号、按出口组或全局指定出口，Cloudflare WARP 可作额外出口。

## 命令

```
mmxcode login                    # 登录一个账号
mmxcode accounts list            # 列出账号
mmxcode accounts proxy <id> --group warp
mmxcode checkin                  # 给所有账号签到
mmxcode usage                    # 积分汇总
mmxcode models                   # 模型列表
```

## 配置

见 `config.example.yaml`。`MMXCODE_CONFIG` 环境变量指向配置文件，`--db` 覆盖数据库路径。

## 协议对齐

上游行为对齐社区插件 [`@magpie-community/opencode-minimax-auth`](https://github.com/magpie-community/plugins/tree/main/packages/minimax) 的 `packages/minimax/index.mjs`：端点、请求签名、刷新语义、签到语义、配额拒绝的判定，都以它为准。

## 计划

- 计划 A（本仓库已实现）：核心库与 CLI
- 计划 B：HTTP 网关（Anthropic / OpenAI / Codex 三协议）、WebUI、Docker 部署
