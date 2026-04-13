# 协议适配源码审计（已完成）

日期：2026-04-13
状态：`P0 / P1 / P2 已落实到源码与测试`

直接依据这些文件：

- `internal/protocol/types.go`
- `internal/protocol/registry.go`
- `internal/protocol/builtin/register.go`
- `internal/protocol/builtin/request_prompt.go`
- `internal/protocol/builtin/openai_gemini.go`
- `internal/protocol/builtin/openai_anthropic.go`
- `internal/protocol/builtin/openai_codex.go`
- `internal/protocol/builtin/anthropic_gemini.go`
- `internal/protocol/builtin/codex_gemini.go`
- `internal/protocol/builtin/codex_anthropic.go`
- `internal/protocol/registry_test.go`
- `internal/protocol/builtin/request_prompt_test.go`

## 当前结论

现在跨协议能力不再是“只通一半”的矩阵。

在源码层面，这 6 组 pair 都已经严格双向：

- `openai <-> gemini`
- `openai <-> anthropic`
- `openai <-> codex`
- `anthropic <-> gemini`
- `anthropic <-> codex`
- `codex <-> gemini`

same-protocol no-op 仍然保留。

## 已完成的根因修复

### 1. Gemini 入站 normalize 已补齐

`request_prompt.go` 现在包含：

- `normalizeGeminiConversation`
- Gemini tool declaration / tool choice 解析
- Gemini function call / function response 配对
- Gemini -> 共享 conversation IR 的媒体、工具、系统指令处理

这解决了此前 `gemini -> *` 全部打不通的根因。

### 2. 能力表已改成直白的 pair -> family 映射

`types.go` 不再用“upstream + family + source 白名单”倒推能力。
现在直接维护：

- `supportedTransformFamiliesByClientAndUpstream`

语义变成：

- client 是谁
- upstream 是谁
- 这个 pair 支持哪个 request family

这样 `SupportsTransform` / `SupportsTransformFamily` / `SupportedClientProtocolsForUpstream` 都是正向判断，少一层脑内反推。

### 3. Protocol / RequestFamily 边界已更清楚

当前跨协议能力覆盖 4 个 request family：

- `chat_completions`
- `messages`
- `responses`
- `generate_content`

仍未覆盖的 family：

- `completions`
- `embeddings`
- `images`

这三个仍然不是“协议转换能力”，只是 `DetectRequestFamily()` 可识别的路由族。

## 已补齐的方向

新增并注册了这些缺失方向：

- `gemini -> openai`
- `anthropic -> openai`
- `gemini -> anthropic`
- `gemini -> codex`
- `anthropic -> codex`

每个方向都补了：

- request transform
- stream response transform
- non-stream response transform
- matrix entry
- tests

## 已补齐的验证

测试现在显式覆盖：

- 5 个新增反向方向的 request / stream / non-stream 行为
- `normalizeGeminiConversation` 的结构化内容解析
- `SupportedClientProtocolsForUpstream()` 的双向矩阵断言
- same-protocol no-op：
  - `TranslateRequest(from == to)`
  - `TranslateResponseStream(from == to)`
  - `TranslateResponseNonStream(from == to)`
  - `BuildTransformPlan(client == upstream)`

## 仍然刻意没做的事

这次没有扩 scope 去做这些：

- `completions / embeddings / images` 的跨协议转换
- 超出当前实现层级的 provider 特有高级字段对齐
- 为所有 provider tool-call streaming 细节做完全语义等价复制

原因很简单：先把双向矩阵打通，再谈更细的 provider 特性兼容；不要把 TODO 扩成无底洞。
