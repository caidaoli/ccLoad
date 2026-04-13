# Protocol Transform Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复协议转换中已确认的三个响应语义错误，确保 OpenAI/Gemini/Codex/Anthropic 在 tool call 流与非流场景下保持一致语义。

**Architecture:** 保持现有 `conversation` 请求抽象不扩散，直接在最小范围内修正响应翻译实现与测试覆盖。优先修复具体根因：缺失 tool call 映射、错误的 `call_id` 字段读取、错误的 OpenAI stream tool call index 维护。

**Tech Stack:** Go, `go test`, sonic JSON, 内置 protocol registry 测试

---

### Task 1: 锁定 OpenAI 到 Gemini 的工具调用回归

**Files:**
- Modify: `internal/protocol/registry_structured_response_test.go`
- Modify: `internal/protocol/builtin/openai_gemini.go`

- [ ] **Step 1: 写失败测试**

补充一个 OpenAI non-stream response 含 `tool_calls` 的用例，断言 Gemini 响应里出现 `functionCall`。

- [ ] **Step 2: 跑单测确认失败**

Run: `go test -tags go_json -count=1 ./internal/protocol/... -run 'OpenAI.*Gemini|Structured'`

Expected: 新增用例失败，输出里没有 `functionCall`

- [ ] **Step 3: 写最小实现**

让 `convertOpenAIResponseToGeminiNonStream` 和 `convertOpenAIResponseToGeminiStream` 复用已有的 OpenAI message 解析逻辑，把 text/tool_calls 统一转成 Gemini parts，而不是只读字符串 `content`。

- [ ] **Step 4: 重新跑单测确认通过**

Run: `go test -tags go_json -count=1 ./internal/protocol/... -run 'OpenAI.*Gemini|Structured'`

Expected: PASS

### Task 2: 修正 Codex 到 Anthropic 的 `call_id` 丢失

**Files:**
- Modify: `internal/protocol/registry_stream_toolcalls_test.go`
- Modify: `internal/protocol/builtin/codex_anthropic.go`

- [ ] **Step 1: 写失败测试**

新增只携带 `call_id`、不携带 `id` 的 Codex `function_call` SSE 用例，断言生成的 Anthropic `tool_use.id` 等于原始 `call_id`。

- [ ] **Step 2: 跑单测确认失败**

Run: `go test -tags go_json -count=1 ./internal/protocol/... -run 'CodexToAnthropic.*FunctionCall'`

Expected: 新增用例失败，输出中的 `tool_use.id` 为空

- [ ] **Step 3: 写最小实现**

在 `convertCodexResponseToAnthropicStream` 里读取 `call_id`，并兼容老输入里的 `id` 回退。

- [ ] **Step 4: 重新跑单测确认通过**

Run: `go test -tags go_json -count=1 ./internal/protocol/... -run 'CodexToAnthropic.*FunctionCall'`

Expected: PASS

### Task 3: 修正 Codex 到 OpenAI 的流式 tool call 索引

**Files:**
- Modify: `internal/protocol/registry_stream_toolcalls_test.go`
- Modify: `internal/protocol/builtin/openai_codex.go`

- [ ] **Step 1: 写失败测试**

新增连续两个 Codex `function_call` 事件的流式用例，断言第二个 OpenAI `tool_calls[0].index` 为 `1`。

- [ ] **Step 2: 跑单测确认失败**

Run: `go test -tags go_json -count=1 ./internal/protocol/... -run 'CodexToOpenAI.*FunctionCall|Stream'`

Expected: 新增用例失败，第二个 index 仍为 `0`

- [ ] **Step 3: 写最小实现**

给 `codexToOpenAIStreamState` 增加独立的 tool call 计数器，在每次输出 `function_call` chunk 后递增。

- [ ] **Step 4: 重新跑单测确认通过**

Run: `go test -tags go_json -count=1 ./internal/protocol/... -run 'CodexToOpenAI.*FunctionCall|Stream'`

Expected: PASS

### Task 4: 跑协议回归验证

**Files:**
- Verify only

- [ ] **Step 1: 跑 fresh 协议测试**

Run: `go test -tags go_json -count=1 ./internal/protocol/...`

Expected: PASS

- [ ] **Step 2: 跑全量 internal 验证**

Run: `go test -tags go_json -count=1 ./internal/...`

Expected: PASS
