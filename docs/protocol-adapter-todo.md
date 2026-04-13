# 协议适配源码审计 TODO

日期：2026-04-13

只按源码判断，结论来自这些文件：

- `internal/protocol/types.go`
- `internal/protocol/registry.go`
- `internal/protocol/builtin/register.go`
- `internal/protocol/builtin/request_prompt.go`
- `internal/protocol/builtin/openai_*.go`
- `internal/protocol/builtin/codex_*.go`
- `internal/protocol/builtin/anthropic_gemini.go`
- `internal/protocol/registry_test.go`
- `internal/protocol/builtin/request_prompt_test.go`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/**`

## 结论

判断标准很简单：`A <-> B` 只有在 **A 作为 client 打到 B upstream** 和 **B 作为 client 打到 A upstream** 这两个方向都存在时，才算双向。

按这个标准，当前源码里的真实矩阵是：

- `openai -> gemini`
- `openai -> anthropic`
- `openai -> codex`
- `anthropic -> gemini`
- `codex -> gemini`
- `codex -> anthropic`
- `codex -> openai`
- same-protocol no-op

所以 6 组 pair 里，只有 **`openai <-> codex` 已经严格双向**。其余 5 组都不是。

## 逐项判断

### `openai <-> gemini`

- 已实现：`openai -> gemini`
  - `internal/protocol/types.go`
    - `Gemini: { RequestFamilyChatCompletions: {OpenAI} }`
  - `internal/protocol/builtin/register.go`
    - `RegisterRequest(OpenAI, Gemini, convertOpenAIRequestToGemini)`
    - `RegisterStreamResponse(Gemini, OpenAI, convertGeminiResponseToOpenAIStream)`
    - `RegisterNonStreamResponse(Gemini, OpenAI, convertGeminiResponseToOpenAINonStream)`
- 未实现：`gemini -> openai`
  - `types.go` 里没有 `OpenAI` upstream 接收 `Gemini`
  - `request_prompt.go` 里没有 `normalizeGeminiConversation`
  - `register.go` 里没有 `RegisterRequest(Gemini, OpenAI, ...)`
  - 也没有 `OpenAI -> Gemini` 响应翻译注册

### `openai <-> anthropic`

- 已实现：`openai -> anthropic`
  - `Anthropic: { RequestFamilyChatCompletions: {OpenAI} }`
  - `RegisterRequest(OpenAI, Anthropic, convertOpenAIRequestToAnthropic)`
  - `RegisterStreamResponse(Anthropic, OpenAI, convertAnthropicResponseToOpenAIStream)`
  - `RegisterNonStreamResponse(Anthropic, OpenAI, convertAnthropicResponseToOpenAINonStream)`
- 未实现：`anthropic -> openai`
  - `types.go` 里没有 `OpenAI` upstream 接收 `Anthropic`
  - `register.go` 里没有 `RegisterRequest(Anthropic, OpenAI, ...)`
  - 也没有 `RegisterStreamResponse(OpenAI, Anthropic, ...)`
  - 也没有 `RegisterNonStreamResponse(OpenAI, Anthropic, ...)`

### `openai <-> codex`

- 已实现：`openai -> codex`
  - `Codex: { RequestFamilyChatCompletions: {OpenAI} }`
  - `RegisterRequest(OpenAI, Codex, convertOpenAIRequestToCodex)`
  - `RegisterStreamResponse(Codex, OpenAI, convertCodexResponseToOpenAIStream)`
  - `RegisterNonStreamResponse(Codex, OpenAI, convertCodexResponseToOpenAINonStream)`
- 已实现：`codex -> openai`
  - `OpenAI: { RequestFamilyResponses: {Codex} }`
  - `RegisterRequest(Codex, OpenAI, convertCodexRequestToOpenAI)`
  - `RegisterStreamResponse(OpenAI, Codex, convertOpenAIResponseToCodexStream)`
  - `RegisterNonStreamResponse(OpenAI, Codex, convertOpenAIResponseToCodexNonStream)`

### `codex <-> gemini`

- 已实现：`codex -> gemini`
  - `Gemini: { RequestFamilyResponses: {Codex} }`
  - `RegisterRequest(Codex, Gemini, convertCodexRequestToGemini)`
  - `RegisterStreamResponse(Gemini, Codex, convertGeminiResponseToCodexStream)`
  - `RegisterNonStreamResponse(Gemini, Codex, convertGeminiResponseToCodexNonStream)`
- 未实现：`gemini -> codex`
  - `types.go` 里没有 `Codex` upstream 接收 `Gemini`
  - `register.go` 里没有 `RegisterRequest(Gemini, Codex, ...)`
  - 也没有 `RegisterStreamResponse(Codex, Gemini, ...)`
  - 也没有 `RegisterNonStreamResponse(Codex, Gemini, ...)`

### `codex <-> anthropic`

- 已实现：`codex -> anthropic`
  - `Anthropic: { RequestFamilyResponses: {Codex} }`
  - `RegisterRequest(Codex, Anthropic, convertCodexRequestToAnthropic)`
  - `RegisterStreamResponse(Anthropic, Codex, convertAnthropicResponseToCodexStream)`
  - `RegisterNonStreamResponse(Anthropic, Codex, convertAnthropicResponseToCodexNonStream)`
- 未实现：`anthropic -> codex`
  - `types.go` 里没有 `Codex` upstream 接收 `Anthropic`
  - `register.go` 里没有 `RegisterRequest(Anthropic, Codex, ...)`
  - 也没有 `RegisterStreamResponse(Codex, Anthropic, ...)`
  - 也没有 `RegisterNonStreamResponse(Codex, Anthropic, ...)`

### `anthropic <-> gemini`

- 已实现：`anthropic -> gemini`
  - `Gemini: { RequestFamilyMessages: {Anthropic} }`
  - `RegisterRequest(Anthropic, Gemini, convertAnthropicRequestToGemini)`
  - `RegisterStreamResponse(Gemini, Anthropic, convertGeminiResponseToAnthropicStream)`
  - `RegisterNonStreamResponse(Gemini, Anthropic, convertGeminiResponseToAnthropicNonStream)`
- 未实现：`gemini -> anthropic`
  - `types.go` 里没有 `Anthropic` upstream 接收 `Gemini`
  - `register.go` 里没有 `RegisterRequest(Gemini, Anthropic, ...)`
  - 也没有 `RegisterStreamResponse(Anthropic, Gemini, ...)`
  - 也没有 `RegisterNonStreamResponse(Anthropic, Gemini, ...)`

## no-op / normalize

### same-protocol no-op

- 已实现。
  - `internal/protocol/types.go`
    - `BuildTransformPlan()` 在 `client == upstream` 时直接返回，不要求 transform。
  - `internal/protocol/registry.go`
    - `TranslateRequest()` 在 `from == to` 时直接返回原始请求。
    - `TranslateResponseStream()` 在 `from == to` 时直接返回原始流块。
    - `TranslateResponseNonStream()` 在 `from == to` 时直接返回原始响应。

### normalize

- 已实现：
  - `normalizeOpenAIConversation`
  - `normalizeAnthropicConversation`
  - `normalizeCodexConversation`
- 未实现：
  - `normalizeGeminiConversation`

这不是小缺口，这是整个 `gemini -> *` 都打不通的根因。

另外，`DetectRequestFamily()` 识别了：

- `chat_completions`
- `responses`
- `messages`
- `generate_content`
- `completions`
- `embeddings`
- `images`

但 `supportedTransformSourcesByUpstreamAndFamily` 只给了：

- `chat_completions`
- `responses`
- `messages`

也就是说，源码层面跨协议转换只覆盖了这三个请求族。`generate_content`、`completions`、`embeddings`、`images` 没有任何跨协议能力表项。

## 是否参考 CLIProxyAPI 源码

结论：**是，明显参考了，但不是直接依赖，也不是原样搬运。**

源码证据：

1. `ccLoad` 没有直接依赖 CLIProxyAPI
   - `go.mod` / `go.sum` 里没有 `router-for-me/CLIProxyAPI`
2. 但方向组织和转换器命名，与 CLIProxyAPI 的 translator 树是一一对位的
   - `ccLoad`
     - `convertOpenAIRequestToGemini`
     - `convertGeminiResponseToOpenAIStream`
     - `convertGeminiResponseToOpenAINonStream`
   - `CLIProxyAPI`
     - `internal/translator/gemini/openai/chat-completions/init.go`
     - `ConvertOpenAIRequestToGemini`
     - `ConvertGeminiResponseToOpenAI`
     - `ConvertGeminiResponseToOpenAINonStream`
3. 下面这些方向在两边都能一一对上
   - `openai -> gemini`
   - `openai -> anthropic`
   - `openai -> codex`
   - `anthropic -> gemini`
   - `codex -> gemini`
   - `codex -> anthropic`
4. 但 `ccLoad` 额外做了自己的结构化重写
   - `internal/protocol/builtin/request_prompt.go` 先把 OpenAI / Anthropic / Codex 归一化到共享 `conversation` IR，再分别编码。
   - CLIProxyAPI 的 translator 目录是按 pair 拆开的注册布局，不是这套共享 IR 结构。
5. `codex -> openai` 在 `ccLoad` 已经落地，而 CLIProxyAPI 源码树里没有看到与之对位的 `internal/translator/openai/codex/...` 注册入口。
   - 这个方向更像 `ccLoad` 自己补的，不是直接照着 CLIProxyAPI 抄出来的。

## TODO

### P0：先修根因

- [ ] 增加 `normalizeGeminiConversation`，否则所有 `gemini -> *` 都是空谈。
- [ ] 把“协议对是否支持”和“请求族是否支持”拆成更直白的能力表；现在 `supportedTransformSourcesByUpstreamAndFamily` 太绕，查一个 pair 还得倒着推 upstream。
- [ ] 明确 `Protocol` 与 `RequestFamily` 的边界。现在 `DetectRequestFamily()` 识别了 7 个族，但能力表只覆盖 3 个族，语义是散的。

### P1：补齐缺失方向

- [ ] `gemini -> openai`
  - 需要：request / stream-response / non-stream-response / matrix entry / tests
  - 参考源码：`~/Share/Source/go/CLIProxyAPI/internal/translator/openai/gemini/init.go`
- [ ] `anthropic -> openai`
  - 需要：request / stream-response / non-stream-response / matrix entry / tests
  - 参考源码：`~/Share/Source/go/CLIProxyAPI/internal/translator/openai/claude/init.go`
- [ ] `gemini -> anthropic`
  - 需要：request / stream-response / non-stream-response / matrix entry / tests
  - 参考源码：`~/Share/Source/go/CLIProxyAPI/internal/translator/claude/gemini/init.go`
- [ ] `gemini -> codex`
  - 需要：request / stream-response / non-stream-response / matrix entry / tests
  - 参考源码：`~/Share/Source/go/CLIProxyAPI/internal/translator/codex/gemini/init.go`
- [ ] `anthropic -> codex`
  - 需要：request / stream-response / non-stream-response / matrix entry / tests
  - 参考源码：`~/Share/Source/go/CLIProxyAPI/internal/translator/codex/claude/init.go`

### P2：补证据，不要靠脑补

- [ ] 给 same-protocol no-op 补显式测试：
  - `BuildTransformPlan(client == upstream)`
  - `TranslateRequest(from == to)`
  - `TranslateResponseStream(from == to)`
  - `TranslateResponseNonStream(from == to)`
- [ ] 给 normalize 层补覆盖完整性测试：
  - 明确当前只有 OpenAI / Anthropic / Codex 三种入站 normalize
  - 明确 Gemini 还不存在入站 normalize
- [ ] 给能力矩阵补一组“方向完整性”测试，直接断言 6 组 pair 的实际状态，别再靠人肉读 `register.go`

## 直接依据的源码位置

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
- `~/Share/Source/go/CLIProxyAPI/internal/translator/gemini/openai/chat-completions/init.go`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/claude/openai/chat-completions/init.go`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/codex/openai/chat-completions/init.go`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/openai/gemini/init.go`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/openai/claude/init.go`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/codex/gemini/init.go`
- `~/Share/Source/go/CLIProxyAPI/internal/translator/codex/claude/init.go`
