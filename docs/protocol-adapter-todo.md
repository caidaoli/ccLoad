# 协议转换审计 TODO（对标 CLIProxyAPI）

日期：2026-04-13  
范围：
- `openai <-> gemini`
- `openai <-> anthropic`
- `openai <-> codex`
- `codex <-> gemini`
- `codex <-> anthropic`
- `anthropic <-> gemini`

## 结论

- `internal/protocol/builtin/register.go` 已注册上述 6 组**双向** `request / non-stream response / stream response` 转换，矩阵“存在”。
- 但它们还不是 **CLIProxyAPI 水平的完整协议适配**。当前真正稳定的主要是 **文本 happy path**；结构化输出、内置工具、thinking/reasoning、缓存 token 细节、以及一半方向的端到端测试还没补齐。
- 根因很直接：`internal/protocol/builtin/request_prompt.go` 已经把**请求侧**抽象成通用 conversation 归一化层，但 **response/stream 侧**仍然是按 pair 手写，很多实现只处理 `text` / `delta.content`，没有把结构化块做完。

## 当前矩阵判定

| 配对 | 注册情况 | 当前判定 | 最大缺口 |
| --- | --- | --- | --- |
| `openai <-> gemini` | 已注册双向 request/non-stream/stream | 部分完成 | 双向 response/stream 基本只处理文本，`tool_calls` / `functionCall` 没打通 |
| `openai <-> anthropic` | 已注册双向 request/non-stream/stream | 部分完成 | `anthropic -> openai` response/stream 丢 `tool_use` / thinking / cached usage |
| `openai <-> codex` | 已注册双向 request/non-stream/stream | 部分完成 | stream 只处理文本，不处理 `function_call` / reasoning 事件 |
| `codex <-> gemini` | 已注册双向 request/non-stream/stream | 部分完成 | `gemini -> codex` response/stream 只处理文本，不处理 `functionCall` |
| `codex <-> anthropic` | 已注册双向 request/non-stream/stream | 部分完成 | `anthropic -> codex` 只保留文本；`codex -> anthropic` stream 丢 reasoning/thinking |
| `anthropic <-> gemini` | 已注册双向 request/non-stream/stream | 部分完成 | `gemini -> anthropic` response/stream 只处理文本，不处理 `functionCall` |

## P0：实现缺口

### 1. 补齐 Gemini 出站 structured output 到 OpenAI / Anthropic / Codex

现状：
- `internal/protocol/builtin/openai_gemini.go` 的 `geminiResponse` 只声明了 `parts[].text`
- `internal/protocol/builtin/anthropic_gemini.go` 的 `convertGeminiResponseToAnthropicNonStream/Stream` 只拼文本
- `internal/protocol/builtin/codex_gemini.go` 的 `convertGeminiResponseToCodexNonStream/Stream` 只拼文本

TODO：
- 支持 Gemini `functionCall` -> OpenAI `tool_calls`
- 支持 Gemini `functionCall` -> Anthropic `tool_use`
- 支持 Gemini `functionCall` -> Codex `function_call`
- 保留多 part 输出，而不是把所有内容压成一个字符串
- finish reason / usage metadata 继续保留，不能因为结构化输出丢失

### 2. 补齐 Anthropic 出站 structured output 到 OpenAI / Codex

现状：
- `internal/protocol/builtin/openai_anthropic.go` 的 `convertAnthropicResponseToOpenAINonStream/Stream` 只消费文本块
- `internal/protocol/builtin/codex_anthropic.go` 的 `convertAnthropicResponseToCodexNonStream/Stream` 只消费文本块
- 当前 `anthropicMessagesResponse` 结构只声明 `[]anthropicTextBlock`，天然吃不下 `tool_use` / `thinking` / `redacted_thinking`

TODO：
- 改为 `map[string]any` 或完整 block union 解析，不要再用 text-only 结构偷懒
- 支持 `tool_use` -> OpenAI `tool_calls`
- 支持 `tool_use` -> Codex `function_call`
- 支持 thinking / redacted_thinking 的保留或显式降级策略
- stream 模式补齐 `content_block_start/content_block_delta/content_block_stop` 的结构化块映射

### 3. 补齐 OpenAI / Codex stream 里的结构化事件

现状：
- `convertOpenAIResponseToGeminiStream`
- `convertOpenAIResponseToAnthropicStream`
- `convertOpenAIResponseToCodexStream`
- `convertCodexResponseToOpenAIStream`
- `convertCodexResponseToAnthropicStream`

上面这些大多只处理文本 delta 和 completed/done。

TODO：
- OpenAI stream 处理 `tool_calls` 增量，而不是只认 `delta.content`
- Codex stream 处理 `response.output_item.done(function_call)`
- Codex stream 处理 `response.reasoning_summary_*` / reasoning item
- Anthropic stream 处理 `tool_use` / thinking block，不要只出 text block

### 4. 补齐 builtin tools / non-function tools

现状：
- `internal/protocol/builtin/request_prompt.go` 的 `parseFunctionTools()` 只接受 `"function"` 或空 type
- 参考实现已有 `test/builtin_tools_translation_test.go`

TODO：
- 支持 OpenAI/Codex builtin tool 类型（至少先对齐参考测试里出现的 `web_search`）
- `tool_choice` 也要能映射 builtin tool，而不是只支持 function/tool/required/auto
- 明确哪些 builtin tool 是透传、哪些需要降级、哪些直接拒绝

### 5. 补齐 thinking / reasoning 语义

现状：
- 当前协议转换层基本没有 thinking/reasoning 语义映射
- 仓库内也没有对等测试
- 参考实现已有：
  - `internal/translator/openai/claude/openai_claude_request_test.go`
  - `internal/translator/codex/claude/codex_claude_response_test.go`
  - `test/thinking_conversion_test.go`

TODO：
- Anthropic `thinking` / `redacted_thinking` <-> OpenAI `reasoning_content`
- Codex `response.reasoning_summary_*` / `encrypted_content` <-> Anthropic thinking stream
- Gemini / OpenAI / Codex 间的 reasoning effort / thinking level 透传与降级策略
- 明确“不支持时返回什么错误”，不要静默吞字段

### 6. 补齐 usage 细节字段映射

现状：
- 当前转换普遍只保留 `prompt/completion/total` 或 `input/output/total`
- 会丢：
  - `prompt_tokens_details.cached_tokens`
  - `input_tokens_details.cached_tokens`
  - `cache_read_input_tokens`
  - `cache_creation_input_tokens`
  - `reasoning_tokens`

TODO：
- OpenAI / Codex / Anthropic / Gemini 之间补 usage detail 映射
- 至少保证 cached token 语义不丢
- 明确 total token 是输入+输出，还是包含缓存/思维 token 的归一化值

### 7. 决定 OpenAI 扩展 family 是要实现还是要删掉死枚举

现状：
- `internal/protocol/types.go` 定义了：
  - `RequestFamilyCompletions`
  - `RequestFamilyEmbeddings`
  - `RequestFamilyImages`
- 但 `supportedTransformFamiliesByClientAndUpstream` 没把这些 family 挂到任何跨协议 pair 上
- 当前测试里已有 `TestBuildTransformPlan_RejectsUnsupportedFamilyForSupportedPair`

TODO：
- 要么真的实现这些 family 的跨协议转换
- 要么删除死枚举/在文档里明确“当前只支持 chat/messages/responses/generateContent 主路径”

## P0：测试缺口

### 1. 缺少直接对标 CLIProxyAPI 的协议测试

TODO：
- 增加 `builtin_tools_translation_test` 等价测试
- 增加 `thinking_conversion_test` 的最小可维护子集
- 增加 cached usage 细节测试，对标：
  - `internal/translator/claude/openai/chat-completions/claude_openai_response_test.go`
  - `internal/translator/codex/openai/chat-completions/codex_openai_response_test.go`
  - `internal/translator/gemini/openai/responses/gemini_openai-responses_response_test.go`

### 2. 缺少 response/stream 结构化输出测试

TODO：
- `gemini -> openai`：non-stream / stream 的 `functionCall`
- `gemini -> anthropic`：non-stream / stream 的 `functionCall`
- `gemini -> codex`：non-stream / stream 的 `functionCall`
- `anthropic -> openai`：non-stream / stream 的 `tool_use` / thinking / usage detail
- `anthropic -> codex`：non-stream / stream 的 `tool_use` / thinking / usage detail
- `openai -> gemini`：non-stream / stream 的 `tool_calls`
- `openai -> codex`：stream 的 `tool_calls` / reasoning
- `codex -> openai`：stream 的 `function_call` / reasoning
- `codex -> anthropic`：stream 的 `function_call` / reasoning signature

### 3. 缺少端到端集成测试方向

当前 `internal/app` 已覆盖：
- `openai -> gemini`
- `anthropic -> gemini`
- `codex -> gemini`
- `openai -> anthropic`
- `codex -> anthropic`
- `openai -> codex`
- `codex -> openai`
- `gemini -> openai`

TODO：
- 补 `gemini -> anthropic`
- 补 `gemini -> codex`
- 补 `anthropic -> openai`
- 补 `anthropic -> codex`

## 建议的收敛顺序

1. 先修 **response/stream 的 structured output 对称性**。这是当前最大的架构洞。
2. 然后补 **builtin tools / thinking / cached usage** 三类语义字段。
3. 最后再决定要不要扩展到 `completions / embeddings / images` family。

不要反过来。现在的问题不是“矩阵没注册”，而是“注册了，但很多 pair 还停留在文本演示版”。
