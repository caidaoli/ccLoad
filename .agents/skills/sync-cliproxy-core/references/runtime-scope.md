# 运行时审查范围

上游修复多数落在 executor、auth conductor 和 handlers，而不是转换快照。这些代码不搬进 ccLoad，但其中的行为修复要逐 commit 判断是否移植到 ccLoad 的对应入口。

## 审查路径

用排除式路径列出 base..target 的 commit：只排除快照专属根和下文「不适用」的上游设施，上游新增的包默认进入审查。不要改回目录白名单——白名单跟不上上游新包，曾漏掉 `internal/misc` 的 Hub 版本更新。

```bash
git -C "$upstream" log --no-merges --format='%h %s' "$base..$target" -- . \
  ':!docs' ':!assets' ':!.github' ':!examples' ':!*.md' ':!go.mod' ':!go.sum' ':!.gitignore' \
  ':!internal/translator' ':!internal/signature' ':!internal/client/codex/apply-patch' \
  ':!internal/pluginhost' ':!internal/pluginstore' ':!sdk/pluginabi' ':!sdk/pluginapi' ':!sdk/pluginhost' ':!sdk/pluginstore' \
  ':!internal/home' ':!internal/homeplugins' ':!internal/redisqueue' ':!internal/kvstore' ':!internal/store' ':!internal/watcher' \
  ':!internal/tui' ':!internal/wstunnel' ':!internal/wsrelay' ':!internal/discovery' ':!internal/managementasset' ':!internal/logging' \
  ':!internal/cmd' ':!cmd/server' ':!internal/buildinfo' ':!internal/safemode' ':!internal/browser' ':!internal/githubauth'
```

pathspec 排除只剔除全部文件都在排除目录内的 commit；同时改了排除目录和运行时代码的 commit 仍会列出，需要审其运行时部分。只改快照文件（`core-snapshot.manifest` 的 root 映射，如 `internal/util`、`internal/thinking` 的非 exclude 文件）的 commit 由快照步骤处理，不重复归类。

## 映射

| 上游 | ccLoad 入口 | 专题 |
| --- | --- | --- |
| `executor/claude_*`、`auth/claude`、`client/claude`、`internal/misc/claude_code_instructions*` | `anthropic_wire.go`、`anthropic_credentials.go`、`anthropic_advisor.go`、`internal/anthropicauth/` | upstream.md#anthropic--claude-code |
| `executor/codex_*`（含 websockets、images）、`auth/codex`、`client/codex`、`cmd/fetch_codex_models` | `codex_wire.go`、`codex_credentials.go`、`codex_images.go`、`proxy_responses_websocket.go`、`internal/codexauth/`、`proxy_codex_models.go` | upstream.md#codex、proxy.md |
| `executor/antigravity_*`、`auth/antigravity`、`internal/antigravitycatalog`、`internal/misc/antigravity_version.go`、`cmd/fetch_antigravity_models` | `antigravity_wire.go`、`antigravity_credentials.go`、`internal/antigravityauth/`（Hub 版本与 changelist 表在 `service.go`） | upstream.md#antigravity |
| `executor/gemini_executor.go`、`gemini_vertex_executor.go`、`aistudio_executor.go`、`auth/vertex` | `vertex_wire.go`、`vertex_credentials.go`、`internal/vertexauth/`；Gemini API Key 走通用转发 | upstream.md#vertex |
| `executor/xai_*`、`auth/xai` | `xai_wire.go`、`xai_credentials.go`、`internal/xaiauth/` | upstream.md#xai |
| `executor/openai_compat_executor.go` | `openai_wire.go` | proxy.md |
| `executor/helps/`、`internal/misc/header_utils.go` | 按调用方归到上面各行；跨提供商的流终态与错误处理归代理 | proxy.md |
| `internal/httpwire`、`internal/httpfetch`、`sdk/httptransport` | 各 `*_wire.go` 的 header 顺序与传输、`upstream_connection_age.go` | upstream.md、runtime-storage.md |
| `sdk/cliproxy/auth`（选择、冷却、额度、会话亲和）、`internal/credentialweight` | `selector*.go`、`key_selector.go`、`internal/cooldown/`、`oauth_quota_cost.go` | routing.md、proxy.md#故障切换 |
| `sdk/api/handlers`、`internal/api`（handlers、请求体限制、压缩） | `proxy_handler.go`、`proxy_forward.go`、`proxy_stream.go` | proxy.md |
| `internal/clienterror` | `internal/util/classifier.go`、`internal/cooldown/detection.go` | proxy.md#故障切换 |
| `internal/thinking` 运行时部分（apply、validate、`provider/`） | `reasoning_*.go`、各 `*_wire.go` 的思考最终化 | routing.md、proxy.md#动态思考等级 |
| `internal/registry`、`internal/client/*/models`、`internal/modelconfig` | `model_catalog_sync.go`、`proxy_codex_models.go`、`internal/util/models_fetcher.go` | routing.md |
| `sdk/cliproxy/usage` | `internal/util/cost_calculator.go`、`internal/oauthcost/` | billing.md |
| `internal/cache`、`sdk/cliproxy/session` | 签名回放和会话粘性：ccLoad 用纯 wire 载体和自己的会话缓存，先对照 protocol.md 再判断 | protocol.md |
| `internal/config`、`sdk/config`、`config.example.yaml` | 只看默认值和行为开关的变化，对照 `config_service.go` 与 `.env.example`；纯配置加载、保存、迁移归不适用 | runtime-storage.md |
| 根目录 `test/` | 跨包行为测试，按被测行为归到上面各行 | — |

## 不适用

以下上游机制在 ccLoad 没有对应功能，按理由组计数，不逐条展开：

- ccLoad 没有的渠道：copilot、devin、kimi、meta、grokbuild、musecode、GitHub 认证；
- 上游自身的运行时设施：Home 云账号、插件、配置热加载与持久化、管理 API、Redis 队列、KV 存储、TUI、隧道、服务发现、日志落盘；
- Interactions 线协议（ccLoad 未支持）。

这些 commit 若修正的是 ccLoad 也有的通用语义（例如流截断判定、取消处理），仍要对照 ccLoad 等价位置归类，不能只看目录就判为不适用。上游新增的包若不属于以上任何一行，先在本表登记映射或不适用理由，再归类其 commit。
