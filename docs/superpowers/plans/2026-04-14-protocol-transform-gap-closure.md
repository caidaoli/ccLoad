# Protocol Transform Gap Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补齐协议审查里剩余的请求侧与 Codex 流式转换缺口，确保 thinking、富 `tool_result`、长工具名、`response.output_item.done(message)` fallback、reasoning summary/signature 都不会再丢语义或重复输出。

**Architecture:** 把工作拆成三条主线。请求侧主线只处理共享 IR、OpenAI rich tool result、Anthropic thinking，并把 Codex 缩名逻辑封装成独立 helper；`codex -> gemini` 主线只动 `codex_gemini.go` 与专属回归测试；`codex -> anthropic` 主线只动 `codex_anthropic.go` 与专属回归测试。所有新增回归都放进新的聚焦测试文件，避免继续把 2k 行文件改成垃圾堆。

**Tech Stack:** Go, `testing`, `sonic`, 现有 `internal/protocol` registry/builtin translator。

**Parallelism:** Task 1、Task 2、Task 3 可以在独立子代理里并行做，因为写集分别落在 `request_prompt*`、`codex_gemini.go`、`codex_anthropic.go`。Task 4 必须等 Task 2 和 Task 3 合入后再做，因为它要把 Codex 缩名的 reverse mapping 接回所有 Codex 响应 translator。Task 5 只做最终验证。

**File Map:**
- Create: `internal/protocol/builtin/request_reasoning.go` - 请求侧 reasoning IR、Anthropic thinking decode、OpenAI/Codex/Gemini 请求编码辅助。
- Create: `internal/protocol/builtin/request_reasoning_test.go` - Anthropic thinking / redacted_thinking 单元回归。
- Create: `internal/protocol/builtin/request_openai_tool_results_test.go` - OpenAI rich `tool_result` 编码单元回归。
- Create: `internal/protocol/builtin/request_codex_tool_names.go` - Codex 工具名缩短与 request/response 反向映射 helper。
- Create: `internal/protocol/builtin/request_codex_tool_names_test.go` - 缩名规则与映射一致性单元回归。
- Create: `internal/protocol/registry_request_semantics_test.go` - 请求翻译端到端回归，覆盖 anthropic/openai/gemini/codex。
- Create: `internal/protocol/registry_codex_gemini_stream_test.go` - `codex -> gemini` message fallback 与 completion metadata 回归。
- Create: `internal/protocol/registry_codex_anthropic_stream_test.go` - `codex -> anthropic` text fallback、reasoning summary、signature 生命周期回归。
- Create: `internal/protocol/registry_codex_tool_names_test.go` - Codex 缩名 round-trip 回归。
- Modify: `internal/protocol/builtin/request_prompt.go` - 共享 conversation IR 与请求编码入口。
- Modify: `internal/protocol/builtin/codex_gemini.go` - `codex -> gemini` stream fallback。
- Modify: `internal/protocol/builtin/codex_anthropic.go` - `codex -> anthropic` text/thinking 状态机。
- Modify: `internal/protocol/builtin/openai_codex.go` - Codex -> OpenAI reverse mapping。

---

### Task 1: 请求侧 reasoning 与 rich tool result

**Files:**
- Create: `internal/protocol/builtin/request_reasoning.go`
- Create: `internal/protocol/builtin/request_reasoning_test.go`
- Create: `internal/protocol/builtin/request_openai_tool_results_test.go`
- Create: `internal/protocol/registry_request_semantics_test.go`
- Modify: `internal/protocol/builtin/request_prompt.go`

- [x] **Step 1: 写失败的 reasoning 单元测试**

```go
func TestRequestReasoning(t *testing.T) {
	t.Run("anthropic assistant thinking becomes reasoning part", func(t *testing.T) {
		req := anthropicMessagesRequest{
			Model: "claude-3-5-sonnet",
			Messages: []anthropicMessageContent{{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "thinking", "thinking": "step by step"},
					map[string]any{"type": "redacted_thinking", "data": "enc_1"},
					map[string]any{"type": "text", "text": "hello"},
				},
			}},
		}

		conv, err := normalizeAnthropicConversation(req)
		if err != nil {
			t.Fatalf("normalizeAnthropicConversation failed: %v", err)
		}
		if len(conv.Turns) != 1 || len(conv.Turns[0].Parts) != 3 {
			t.Fatalf("unexpected turns: %+v", conv.Turns)
		}
		if got := conv.Turns[0].Parts[0].Kind; got != partKindReasoning {
			t.Fatalf("expected first part to be reasoning, got %q", got)
		}
		if got := conv.Turns[0].Parts[1].Reasoning.Subtype; got != "redacted_thinking" {
			t.Fatalf("expected redacted_thinking subtype, got %+v", conv.Turns[0].Parts[1].Reasoning)
		}
	})

	t.Run("gemini request drops reasoning but keeps sibling text and tool content", func(t *testing.T) {
		conv := conversation{
			Turns: []conversationTurn{{
				Role: "assistant",
				Parts: []conversationPart{
					{Kind: partKindReasoning, Reasoning: &conversationReasoning{Subtype: "thinking", Text: "private"}},
					{Kind: partKindText, Text: "public"},
					{Kind: partKindToolCall, ToolCall: &conversationToolCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"go"}`)}},
				},
			}},
		}

		raw, err := encodeGeminiRequest(conv)
		if err != nil {
			t.Fatalf("encodeGeminiRequest failed: %v", err)
		}
		body := string(raw)
		if strings.Contains(body, "private") || !strings.Contains(body, "public") || !strings.Contains(body, "\"lookup\"") {
			t.Fatalf("unexpected gemini request body: %s", body)
		}
	})
}
```

- [x] **Step 2: 写失败的 rich tool result 单元测试**

```go
func TestRequestOpenAIToolResults(t *testing.T) {
	parts := []conversationPart{
		{Kind: partKindText, Text: "tool ok"},
		{Kind: partKindImage, Media: &conversationMedia{URL: "https://example.com/tool.png"}},
		{Kind: partKindFile, Media: &conversationMedia{Data: "cGRm", MIMEType: "application/pdf", Filename: "doc.pdf"}},
	}

	content, err := encodeOpenAIToolResultContent(parts)
	if err != nil {
		t.Fatalf("encodeOpenAIToolResultContent failed: %v", err)
	}
	items, ok := content.([]map[string]any)
	if !ok || len(items) != 3 {
		t.Fatalf("expected structured tool result content, got %#v", content)
	}
	if items[0]["type"] != "text" || items[1]["type"] != "image_url" || items[2]["type"] != "file" {
		t.Fatalf("unexpected content items: %#v", items)
	}
}
```

- [x] **Step 3: 写失败的 registry 请求回归**

```go
func TestRegistryRequestSemantics(t *testing.T) {
	t.Run("anthropic to openai keeps assistant thinking as reasoning_content", func(t *testing.T) {
		reg := protocol.NewRegistry()
		builtin.Register(reg)

		raw := []byte(`{"model":"gpt-4o","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"step by step"},{"type":"text","text":"hello"}]}]}`)
		out, err := reg.TranslateRequest(protocol.Anthropic, protocol.OpenAI, "gpt-4o", raw, false)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		body := string(out)
		if !strings.Contains(body, `"reasoning_content":"step by step"`) || !strings.Contains(body, `"hello"`) {
			t.Fatalf("unexpected openai request body: %s", body)
		}
	})

	t.Run("anthropic to codex preserves redacted_thinking as encrypted_content", func(t *testing.T) {
		reg := protocol.NewRegistry()
		builtin.Register(reg)

		raw := []byte(`{"model":"gpt-5-codex","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"enc_1"},{"type":"text","text":"hello"}]}]}`)
		out, err := reg.TranslateRequest(protocol.Anthropic, protocol.Codex, "gpt-5-codex", raw, false)
		if err != nil {
			t.Fatalf("TranslateRequest failed: %v", err)
		}
		body := string(out)
		if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"encrypted_content":"enc_1"`) {
			t.Fatalf("unexpected codex request body: %s", body)
		}
	})
}
```

- [x] **Step 4: 运行红灯确认缺口真实存在**

Run: `go test -tags go_json ./internal/protocol/builtin ./internal/protocol -run 'TestRequestReasoning|TestRequestOpenAIToolResults|TestRegistryRequestSemantics'`

Expected: FAIL，至少包含以下一种报错：
- `unsupported anthropic content block type "thinking"`
- `openai tool results only support text content`
- `unexpected codex request body`

- [x] **Step 5: 实现共享 reasoning IR 与 rich tool result 编码**

```go
const partKindReasoning = "reasoning"

type conversationReasoning struct {
	Subtype          string
	Text             string
	Signature        string
	EncryptedContent string
}

type conversationPart struct {
	Kind       string
	Text       string
	Media      *conversationMedia
	ToolCall   *conversationToolCall
	ToolResult *conversationToolResult
	Reasoning  *conversationReasoning
}
```

```go
func decodeAnthropicContentBlock(block map[string]any) (conversationPart, error) {
	switch normalizeRole(stringValue(block["type"])) {
	case "thinking":
		text := strings.TrimSpace(stringValue(block["thinking"]))
		if text == "" {
			return conversationPart{}, nil
		}
		return conversationPart{Kind: partKindReasoning, Reasoning: &conversationReasoning{
			Subtype:   "thinking",
			Text:      text,
			Signature: strings.TrimSpace(stringValue(block["signature"])),
		}}, nil
	case "redacted_thinking":
		data := strings.TrimSpace(stringValue(block["data"]))
		if data == "" {
			return conversationPart{}, nil
		}
		return conversationPart{Kind: partKindReasoning, Reasoning: &conversationReasoning{
			Subtype:          "redacted_thinking",
			EncryptedContent: data,
		}}, nil
	}
	// existing cases remain below
}
```

```go
func encodeOpenAIToolResultContent(parts []conversationPart) (any, error) {
	return encodeToolResultContent(parts)
}

func encodeCodexReasoningPart(reasoning *conversationReasoning) map[string]any {
	item := map[string]any{"type": "reasoning"}
	if reasoning != nil && strings.TrimSpace(reasoning.Text) != "" {
		item["content"] = []map[string]any{{
			"type": "reasoning_text",
			"text": reasoning.Text,
		}}
	}
	if reasoning != nil && reasoning.EncryptedContent != "" {
		item["encrypted_content"] = reasoning.EncryptedContent
	}
	return item
}
```

实现要求：
- `normalizeAnthropicConversation` 改成按消息 role 调用内容解析，assistant 的 `thinking`/`redacted_thinking` 保留，非 assistant 的 `thinking` 直接忽略。
- `encodeOpenAIRequest` 只把 assistant reasoning parts 聚合进 `reasoning_content` 与 `reasoning`，不要塞进普通 `content`。
- `encodeCodexRequest` 为 assistant reasoning parts 生成独立 `reasoning` item；Gemini 编码直接跳过 reasoning parts，但同 turn 其他 parts 保持原样。
- `encodeOpenAIToolResultContent` 直接复用已有 `encodeToolResultContent`，不要再写第二套 image/file 编码。

- [x] **Step 6: 重新运行请求侧定向测试并确认变绿**

Run: `go test -tags go_json ./internal/protocol/builtin ./internal/protocol -run 'TestRequestReasoning|TestRequestOpenAIToolResults|TestRegistryRequestSemantics'`

Expected: PASS

- [x] **Step 7: 提交请求侧修复**

```bash
git add internal/protocol/builtin/request_prompt.go \
        internal/protocol/builtin/request_reasoning.go \
        internal/protocol/builtin/request_reasoning_test.go \
        internal/protocol/builtin/request_openai_tool_results_test.go \
        internal/protocol/registry_request_semantics_test.go
git commit -m "feat(protocol): preserve request reasoning and structured tool results"
```

---

### Task 2: `codex -> gemini` message fallback 与 completion metadata

**Files:**
- Create: `internal/protocol/registry_codex_gemini_stream_test.go`
- Modify: `internal/protocol/builtin/codex_gemini.go`

- [x] **Step 1: 写失败的 `codex -> gemini` 流式回归**

```go
func TestRegistryCodexGeminiStream(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("message done fallback emits text when no delta arrived", func(t *testing.T) {
		var state any
		out, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Gemini,
			"gemini-2.5-pro",
			nil,
			nil,
			[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		if len(out) != 1 || !strings.Contains(string(out[0]), `"text":"ok"`) {
			t.Fatalf("unexpected gemini chunks: %#v", out)
		}
	})

	t.Run("response created seeds completion metadata", func(t *testing.T) {
		var state any
		if _, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Gemini,
			"gemini-2.5-pro",
			nil,
			nil,
			[]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\"}}\n\n"),
			&state,
		); err != nil {
			t.Fatalf("response.created failed: %v", err)
		}
		done, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Gemini,
			"gemini-2.5-pro",
			nil,
			nil,
			[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("response.completed failed: %v", err)
		}
		if len(done) != 1 || !strings.Contains(string(done[0]), `"responseId":"resp_1"`) || !strings.Contains(string(done[0]), `"finishReason":"STOP"`) {
			t.Fatalf("unexpected completion chunk: %#v", done)
		}
	})
}
```

- [x] **Step 2: 运行红灯确认当前实现会吞掉 fallback**

Run: `go test -tags go_json ./internal/protocol -run 'TestRegistryCodexGeminiStream'`

Expected: FAIL，至少有一个子测试报 `unexpected gemini chunks`。

- [x] **Step 3: 给 `codex_gemini.go` 加最小状态位并实现 fallback**

```go
type codexToGeminiStreamState struct {
	model              string
	responseID         string
	hasOutputTextDelta bool
}
```

```go
if eventType == "response.output_text.delta" || stringValue(payload["type"]) == "response.output_text.delta" {
	if content := stringValue(payload["delta"]); content != "" {
		st.hasOutputTextDelta = true
		// existing Gemini text chunk code
	}
}

if eventType == "response.output_item.done" || stringValue(payload["type"]) == "response.output_item.done" {
	item, _ := payload["item"].(map[string]any)
	switch normalizeRole(stringValue(item["type"])) {
	case "function_call":
		// keep existing behavior
	case "message":
		if st.hasOutputTextDelta {
			return nil, nil
		}
		parts := codexMessageTextParts(item["content"])
		if len(parts) == 0 {
			return nil, nil
		}
		st.hasOutputTextDelta = true
		return [][]byte{mustGeminiTextChunk(st.model, st.responseID, parts)}, nil
	}
}
```

- [x] **Step 4: 重新运行定向测试并确认 message fallback、去重、metadata 都通过**

Run: `go test -tags go_json ./internal/protocol -run 'TestRegistryCodexGeminiStream'`

Expected: PASS

- [x] **Step 5: 提交 `codex -> gemini` 修复**

```bash
git add internal/protocol/builtin/codex_gemini.go \
        internal/protocol/registry_codex_gemini_stream_test.go
git commit -m "fix(protocol): add codex to gemini message fallback"
```

---

### Task 3: `codex -> anthropic` text fallback 与 reasoning 生命周期

**Files:**
- Create: `internal/protocol/registry_codex_anthropic_stream_test.go`
- Modify: `internal/protocol/builtin/codex_anthropic.go`

- [x] **Step 1: 写失败的 text fallback 与 reasoning 生命周期回归**

```go
func TestRegistryCodexAnthropicStream(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("message done fallback emits text and dedupes after delta", func(t *testing.T) {
		var state any
		if _, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Anthropic,
			"claude-3-5-sonnet",
			nil,
			nil,
			[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"),
			&state,
		); err != nil {
			t.Fatalf("delta failed: %v", err)
		}
		out, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Anthropic,
			"claude-3-5-sonnet",
			nil,
			nil,
			[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("message done failed: %v", err)
		}
		if out != nil {
			t.Fatalf("expected duplicate fallback to be ignored, got %#v", out)
		}
	})

	t.Run("reasoning summary emits thinking block with cached signature", func(t *testing.T) {
		var state any
		chunks := []string{
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_1\"}}\n\n",
			"event: response.reasoning_summary_part.added\ndata: {\"type\":\"response.reasoning_summary_part.added\"}\n\n",
			"event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"step by step\"}\n\n",
			"event: response.reasoning_summary_part.done\ndata: {\"type\":\"response.reasoning_summary_part.done\"}\n\n",
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}\n\n",
		}
		var joined bytes.Buffer
		for _, chunk := range chunks {
			out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.Anthropic, "claude-3-5-sonnet", nil, nil, []byte(chunk), &state)
			if err != nil {
				t.Fatalf("chunk failed: %v", err)
			}
			for _, b := range out {
				joined.Write(b)
			}
		}
		body := joined.String()
		if !strings.Contains(body, `"type":"thinking"`) || !strings.Contains(body, `"thinking_delta"`) || !strings.Contains(body, `"signature_delta"`) {
			t.Fatalf("unexpected anthropic reasoning stream: %s", body)
		}
	})
}
```

- [x] **Step 2: 运行红灯确认当前状态机缺少生命周期字段**

Run: `go test -tags go_json ./internal/protocol -run 'TestRegistryCodexAnthropicStream'`

Expected: FAIL，至少有一个子测试报缺少 `thinking_delta` 或 `signature_delta`，或者重复输出文本。

- [x] **Step 3: 在 `codex_anthropic.go` 引入明确的 text/thinking 状态**

```go
type codexToAnthropicStreamState struct {
	started            bool
	openBlock          bool
	blockIndex         int
	lastBlock          string
	hasTextDelta       bool
	thinkingBlockOpen  bool
	thinkingStopPending bool
	thinkingSignature  string
}
```

```go
func codexAnthropicFinalizeThinking(st *codexToAnthropicStreamState) ([][]byte, error) {
	if st == nil || !st.thinkingStopPending || !st.thinkingBlockOpen {
		return nil, nil
	}
	out := make([][]byte, 0, 2)
	if st.thinkingSignature != "" {
		out = append(out, mustAnthropicSignatureDelta(st.blockIndex, st.thinkingSignature))
	}
	out = append(out, mustAnthropicBlockStop(st.blockIndex))
	st.thinkingBlockOpen = false
	st.thinkingStopPending = false
	st.lastBlock = "thinking"
	st.blockIndex++
	return out, nil
}
```

实现要求：
- `response.output_item.done(message)` 只有在 `hasTextDelta == false` 时才落文本 fallback。
- `response.reasoning_summary_part.added` 开 thinking block；`response.reasoning_summary_text.delta` 只写 `thinking_delta`；`response.reasoning_summary_part.done` 只标记 `thinkingStopPending`，不直接 stop。
- `response.output_item.added(reasoning)` 只缓存 `thinkingSignature`，不提前输出块。
- `response.output_item.done(reasoning)` 或 `response.completed` 负责 finalize pending thinking block；如果 signature 早到了但 done 不带 signature，仍使用缓存值发 `signature_delta`。
- multipart reasoning 进入下一段前，先 finalize 前一段，再开新 block。

- [x] **Step 4: 重新运行定向回归并确认 text fallback、summary lifecycle、signature cache 全部通过**

Run: `go test -tags go_json ./internal/protocol -run 'TestRegistryCodexAnthropicStream'`

Expected: PASS

- [x] **Step 5: 提交 `codex -> anthropic` 修复**

```bash
git add internal/protocol/builtin/codex_anthropic.go \
        internal/protocol/registry_codex_anthropic_stream_test.go
git commit -m "fix(protocol): harden codex to anthropic stream state"
```

---

### Task 4: Codex 长工具名缩短与 reverse mapping

**Files:**
- Create: `internal/protocol/builtin/request_codex_tool_names.go`
- Create: `internal/protocol/builtin/request_codex_tool_names_test.go`
- Create: `internal/protocol/registry_codex_tool_names_test.go`
- Modify: `internal/protocol/builtin/request_prompt.go`
- Modify: `internal/protocol/builtin/openai_codex.go`
- Modify: `internal/protocol/builtin/codex_gemini.go`
- Modify: `internal/protocol/builtin/codex_anthropic.go`

- [x] **Step 1: 写失败的缩名 helper 与 round-trip 回归**

```go
func TestCodexToolNameAliases(t *testing.T) {
	aliases := buildCodexToolAliases([]string{
		"mcp__weather__a_very_long_tool_name_that_exceeds_sixty_four_characters_limit_here_test",
		"mcp__weather__a_very_long_tool_name_that_exceeds_sixty_four_characters_limit_here_test_2",
	})
	if len(aliases.OriginalToShort) != 2 || len(aliases.ShortToOriginal) != 2 {
		t.Fatalf("unexpected aliases: %+v", aliases)
	}
	for original, short := range aliases.OriginalToShort {
		if len(short) > 64 {
			t.Fatalf("short name too long: %s -> %s", original, short)
		}
		if aliases.ShortToOriginal[short] != original {
			t.Fatalf("reverse mapping lost original name: %+v", aliases)
		}
	}
}
```

```go
func TestRegistryCodexToolNameRoundTrip(t *testing.T) {
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	longName := "a_very_long_tool_name_that_exceeds_sixty_four_characters_limit_here_test"
	rawReq := []byte(`{"model":"gpt-4o","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + longName + `","arguments":"{}"}}]}],"tools":[{"type":"function","function":{"name":"` + longName + `","parameters":{"type":"object"}}}]}`)
	translatedReq, err := reg.TranslateRequest(protocol.OpenAI, protocol.Codex, "gpt-5-codex", rawReq, true)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}

	resp := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"a_very_long_tool_name_that_exceeds_sixty_four_characters_limit_h\",\"arguments\":{\"q\":\"go\"}}}\n\n")
	out, err := reg.TranslateResponseStream(context.Background(), protocol.Codex, protocol.OpenAI, "gpt-4o", rawReq, translatedReq, resp, new(any))
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	if len(out) == 0 || !strings.Contains(string(out[0]), longName) {
		t.Fatalf("expected restored original tool name, got %#v", out)
	}
}
```

- [x] **Step 2: 运行红灯确认当前实现不会恢复原名**

Run: `go test -tags go_json ./internal/protocol/builtin ./internal/protocol -run 'TestCodexToolNameAliases|TestRegistryCodexToolNameRoundTrip'`

Expected: FAIL，至少有一个子测试报 `expected restored original tool name`。

- [x] **Step 3: 实现单请求级别的缩名与反向恢复**

```go
type codexToolAliases struct {
	OriginalToShort map[string]string
	ShortToOriginal map[string]string
}

func buildCodexToolAliases(names []string) codexToolAliases {
	// deterministic <=64 chars aliases, unique within one request
}

func codexToolAliasesFromRequests(rawReq, translatedReq []byte) codexToolAliases {
	// parse original tool names from rawReq and shortened names from translatedReq
}
```

```go
type codexToOpenAIStreamState struct {
	model       string
	toolNameMap map[string]string
	// existing fields remain
}

func (st *codexToOpenAIStreamState) restoreToolName(rawReq, translatedReq []byte, name string) string {
	if st.toolNameMap == nil {
		st.toolNameMap = codexToolAliasesFromRequests(rawReq, translatedReq).ShortToOriginal
	}
	if original := st.toolNameMap[name]; original != "" {
		return original
	}
	return name
}
```

实现要求：
- `encodeCodexRequest` 只构建一次 alias map，并统一作用到 `tools`、`tool_choice`、`function_call`、`function_call_output.name`。
- `convertCodexResponseToOpenAINonStream` / `convertCodexResponseToOpenAIStream`、`convertCodexResponseToGeminiNonStream` / `convertCodexResponseToGeminiStream`、`convertCodexResponseToAnthropicNonStream` / `convertCodexResponseToAnthropicStream` 在落下游 payload 前都调用 reverse mapping。
- stream state 只缓存 `ShortToOriginal`，不要引入跨请求全局缓存。

- [x] **Step 4: 重新运行缩名定向测试并确认 OpenAI/Gemini/Anthropic 三条回程都能恢复原名**

Run: `go test -tags go_json ./internal/protocol/builtin ./internal/protocol -run 'TestCodexToolNameAliases|TestRegistryCodexToolNameRoundTrip'`

Expected: PASS

- [x] **Step 5: 提交 Codex 缩名兼容**

```bash
git add internal/protocol/builtin/request_codex_tool_names.go \
        internal/protocol/builtin/request_codex_tool_names_test.go \
        internal/protocol/builtin/request_prompt.go \
        internal/protocol/builtin/openai_codex.go \
        internal/protocol/builtin/codex_gemini.go \
        internal/protocol/builtin/codex_anthropic.go \
        internal/protocol/registry_codex_tool_names_test.go
git commit -m "fix(protocol): round-trip shortened codex tool names"
```

---

### Task 5: 全量验证与收口

**Files:**
- Modify: `docs/superpowers/plans/2026-04-14-protocol-transform-gap-closure.md`

- [x] **Step 1: 跑协议包回归**

Run: `go test -tags go_json ./internal/protocol/...`

Expected: PASS

- [x] **Step 2: 跑 internal 全量回归**

Run: `go test -tags go_json ./internal/...`

Expected: PASS

- [x] **Step 3: 跑竞态回归**

Run: `go test -tags go_json -race ./internal/...`

Expected: PASS

- [x] **Step 4: 跑静态检查**

Run: `golangci-lint run ./...`

Expected: `0 issues.`

- [x] **Step 5: 跑实际构建**

Run: `CGO_ENABLED=0 go build -tags go_json -trimpath -ldflags="-s -w -X ccLoad/internal/version.Version=$(git describe --tags --always) -X ccLoad/internal/version.Commit=$(git rev-parse --short HEAD) -X 'ccLoad/internal/version.BuildTime=$(date '+%Y-%m-%d %H:%M:%S %z')' -X ccLoad/internal/version.BuiltBy=$(whoami)" -o /tmp/ccload-verify .`

Expected: exit 0

- [x] **Step 6: 核对工作区与计划勾选状态**

Run: `git status --short`

Expected: 空输出，或者只剩你明确还没提交的计划文件更新。

- [x] **Step 7: 把计划里的 checkbox 勾成真实状态并提交最后的验证痕迹**

```bash
git add docs/superpowers/plans/2026-04-14-protocol-transform-gap-closure.md
git commit -m "docs: record protocol transform execution status"
```
