# 运行时审查范围

上游修复多数落在 executor、auth conductor 和 handlers，而不是转换快照。这些代码不搬进 ccLoad，但其中的行为修复要逐 commit 判断是否移植到 ccLoad 的对应入口。

## 审查路径

```bash
git -C "$upstream" log --no-merges --format='%h %s' "$base..$target" -- \
  internal/runtime internal/auth internal/client internal/clienterror internal/cache \
  internal/registry internal/thinking internal/api sdk/cliproxy sdk/api sdk/auth sdk/proxyutil
```

只改快照文件（`core-snapshot.manifest` 的 root 映射）的 commit 由快照步骤处理，不重复归类。

## 映射

| 上游 | ccLoad 入口 | 专题 |
| --- | --- | --- |
| `executor/claude_*`、`auth/claude`、`client/claude` | `anthropic_wire.go`、`anthropic_credentials.go`、`anthropic_advisor.go`、`internal/anthropicauth/` | upstream.md#anthropic--claude-code |
| `executor/codex_*`（含 websockets、images）、`auth/codex`、`client/codex` | `codex_wire.go`、`codex_credentials.go`、`codex_images.go`、`proxy_responses_websocket.go`、`internal/codexauth/` | upstream.md#codex、proxy.md |
| `executor/antigravity_*`、`auth/antigravity`、`internal/antigravitycatalog` | `antigravity_wire.go`、`antigravity_credentials.go`、`internal/antigravityauth/` | upstream.md#antigravity |
| `executor/gemini_executor.go`、`gemini_vertex_executor.go`、`aistudio_executor.go`、`auth/vertex` | `vertex_wire.go`、`vertex_credentials.go`、`internal/vertexauth/`；Gemini API Key 走通用转发 | upstream.md#vertex |
| `executor/xai_*`、`auth/xai` | `xai_wire.go`、`xai_credentials.go`、`internal/xaiauth/` | upstream.md#xai |
| `executor/openai_compat_executor.go` | `openai_wire.go` | proxy.md |
| `executor/helps/` | 按调用方归到上面各行；跨提供商的流终态与错误处理归代理 | proxy.md |
| `sdk/cliproxy/auth`（选择、冷却、额度、会话亲和） | `selector*.go`、`key_selector.go`、`internal/cooldown/`、`oauth_quota_cost.go` | routing.md、proxy.md#故障切换 |
| `sdk/api/handlers`、`internal/api`（handlers、请求体限制、压缩） | `proxy_handler.go`、`proxy_forward.go`、`proxy_stream.go` | proxy.md |
| `internal/clienterror` | `internal/util/classifier.go`、`internal/cooldown/detection.go` | proxy.md#故障切换 |
| `internal/thinking` 运行时部分（apply、validate、`provider/`） | `reasoning_*.go`、各 `*_wire.go` 的思考最终化 | routing.md、proxy.md#动态思考等级 |
| `internal/registry`、`internal/client/*/models` | `model_catalog_sync.go`、`proxy_codex_models.go`、`internal/util/models_fetcher.go` | routing.md |
| `sdk/cliproxy/usage` | `internal/util/cost_calculator.go`、`internal/oauthcost/` | billing.md |
| `internal/cache`、`sdk/cliproxy/session` | 签名回放和会话粘性：ccLoad 用纯 wire 载体和自己的会话缓存，先对照 protocol.md 再判断 | protocol.md |

## 不适用

以下上游机制在 ccLoad 没有对应功能，按理由组计数，不逐条展开：

- ccLoad 没有的渠道：copilot、devin、kimi、meta、grokbuild、musecode、GitHub 认证；
- 上游自身的运行时设施：Home 云账号、插件、配置热加载、管理 API、Redis 队列、TUI、隧道；
- Interactions 线协议（ccLoad 未支持）。

这些 commit 若修正的是 ccLoad 也有的通用语义（例如流截断判定、取消处理），仍要对照 ccLoad 等价位置归类，不能只看目录就判为不适用。
