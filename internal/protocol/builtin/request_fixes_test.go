package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// 覆盖 P0 #1：user turn 中 [text, tool_result] 顺序，OpenAI 期望 tool 紧跟前一条 assistant tool_calls，
// 因此 tool 消息必须先于当前 user 消息 emit。
func TestOpenAIRequest_ToolResultPrecedesUserMessage(t *testing.T) {
	conv := conversation{
		Turns: []conversationTurn{
			{Role: "assistant", Parts: []conversationPart{
				{Kind: partKindToolCall, ToolCall: &conversationToolCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}},
			}},
			{Role: "user", Parts: []conversationPart{
				{Kind: partKindText, Text: "thanks"},
				{Kind: partKindToolResult, ToolResult: &conversationToolResult{CallID: "call_1", Parts: []conversationPart{{Kind: partKindText, Text: "result"}}}},
			}},
		},
	}
	raw, err := encodeOpenAIRequest("gpt-x", conv, false)
	if err != nil {
		t.Fatalf("encodeOpenAIRequest failed: %v", err)
	}
	body := string(raw)
	toolIdx := strings.Index(body, `"tool_call_id":"call_1"`)
	userIdx := strings.Index(body, `"thanks"`)
	if toolIdx < 0 || userIdx < 0 {
		t.Fatalf("expected both tool and user fragments, got: %s", body)
	}
	if toolIdx > userIdx {
		t.Fatalf("tool message must precede user message, got tool=%d user=%d body=%s", toolIdx, userIdx, body)
	}
}

// 覆盖 P1 #4：Anthropic disable_parallel_tool_use → OpenAI parallel_tool_calls=false。
func TestOpenAIRequest_ParallelToolCallsDisabled(t *testing.T) {
	conv := conversation{
		Turns:      []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
		Tools:      []conversationTool{{Type: "function", Name: "lookup"}},
		ToolChoice: conversationToolChoice{Mode: "auto", DisableParallel: true},
	}
	raw, err := encodeOpenAIRequest("gpt-x", conv, false)
	if err != nil {
		t.Fatalf("encodeOpenAIRequest failed: %v", err)
	}
	if !strings.Contains(string(raw), `"parallel_tool_calls":false`) {
		t.Fatalf("expected parallel_tool_calls=false in body: %s", string(raw))
	}
}

// 覆盖 P1 #4：Codex 同样透传 parallel_tool_calls=false。
func TestCodexRequest_ParallelToolCallsDisabled(t *testing.T) {
	conv := conversation{
		Turns:      []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
		Tools:      []conversationTool{{Type: "function", Name: "lookup"}},
		ToolChoice: conversationToolChoice{Mode: "auto", DisableParallel: true},
	}
	raw, err := encodeCodexRequest("gpt-x", conv, false)
	if err != nil {
		t.Fatalf("encodeCodexRequest failed: %v", err)
	}
	if !strings.Contains(string(raw), `"parallel_tool_calls":false`) {
		t.Fatalf("expected parallel_tool_calls=false in body: %s", string(raw))
	}
}

// 覆盖 P2 #5：Anthropic thinking.enabled → Codex 写 reasoning.effort + include；
// 没有 thinking 时不写 reasoning，避免给非 reasoning 模型硬塞导致 400。
func TestCodexRequest_ReasoningGatedByThinking(t *testing.T) {
	base := conversation{
		Turns: []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
	}

	t.Run("no thinking emits no reasoning config", func(t *testing.T) {
		raw, err := encodeCodexRequest("gpt-x", base, false)
		if err != nil {
			t.Fatalf("encodeCodexRequest failed: %v", err)
		}
		body := string(raw)
		if strings.Contains(body, `"reasoning"`) || strings.Contains(body, `"include"`) {
			t.Fatalf("expected no reasoning/include without thinking, got: %s", body)
		}
	})

	t.Run("thinking enabled writes reasoning effort and include", func(t *testing.T) {
		conv := base
		conv.Thinking = &anthropicThinkingConfig{Type: "enabled", BudgetTokens: 8000}
		raw, err := encodeCodexRequest("gpt-x", conv, false)
		if err != nil {
			t.Fatalf("encodeCodexRequest failed: %v", err)
		}
		body := string(raw)
		if !strings.Contains(body, `"reasoning":{`) || !strings.Contains(body, `"effort":"medium"`) || !strings.Contains(body, `"summary":"auto"`) {
			t.Fatalf("expected reasoning effort=medium summary=auto, got: %s", body)
		}
		if !strings.Contains(body, `"include":["reasoning.encrypted_content"]`) {
			t.Fatalf("expected include=reasoning.encrypted_content, got: %s", body)
		}
	})
}

// 覆盖 P0 #2：Gemini functionDeclarations.parameters 必须剥除 Gemini 不识别的 schema 关键字。
func TestGeminiRequest_SchemaCleaning(t *testing.T) {
	conv := conversation{
		Turns: []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
		Tools: []conversationTool{{
			Type: "function",
			Name: "lookup",
			InputSchema: json.RawMessage(`{
				"$schema": "http://json-schema.org/draft-07/schema#",
				"type": "object",
				"additionalProperties": false,
				"x-google-ext": "drop-me",
				"properties": {
					"q": {"type": "string", "format": "email", "minLength": 1, "title": "Query"},
					"format": {"type": "string"}
				},
				"required": ["q"]
			}`),
		}},
	}
	raw, err := encodeGeminiRequest(conv)
	if err != nil {
		t.Fatalf("encodeGeminiRequest failed: %v", err)
	}
	body := string(raw)
	for _, banned := range []string{`"$schema"`, `"additionalProperties"`, `"x-google-ext"`, `"format":"email"`, `"minLength"`, `"title":"Query"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("expected %s removed, body: %s", banned, body)
		}
	}
	// 用户字段名 "format" 是 properties 子键，必须保留
	if !strings.Contains(body, `"format":{"type":"string"}`) {
		t.Fatalf("user-defined property named 'format' must be preserved, body: %s", body)
	}
	if !strings.Contains(body, `"required":["q"]`) {
		t.Fatalf("required must be preserved, body: %s", body)
	}
}

// 覆盖 P1 #3：Gemini functionResponse.response 只承载 {output: ...}，不含 call_id/is_error。
func TestGeminiRequest_FunctionResponseEnvelope(t *testing.T) {
	conv := conversation{
		Turns: []conversationTurn{
			{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "go"}}},
			{Role: "user", Parts: []conversationPart{{Kind: partKindToolResult, ToolResult: &conversationToolResult{
				CallID:  "call_42",
				Name:    "lookup",
				IsError: true,
				Parts:   []conversationPart{{Kind: partKindText, Text: "boom"}},
			}}}},
		},
	}
	raw, err := encodeGeminiRequest(conv)
	if err != nil {
		t.Fatalf("encodeGeminiRequest failed: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"functionResponse":{"name":"lookup","response":{"output":"boom"}}`) {
		t.Fatalf("unexpected functionResponse shape: %s", body)
	}
	if strings.Contains(body, `"call_id"`) || strings.Contains(body, `"is_error"`) {
		t.Fatalf("envelope leaked anthropic-only fields: %s", body)
	}
}

// 覆盖 P2 #6：Anthropic 顶层 thinking → Gemini generationConfig.thinkingConfig。
func TestGeminiRequest_ThinkingConfig(t *testing.T) {
	base := conversation{
		Turns: []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
	}

	t.Run("enabled with budget", func(t *testing.T) {
		conv := base
		conv.Thinking = &anthropicThinkingConfig{Type: "enabled", BudgetTokens: 1024}
		raw, err := encodeGeminiRequest(conv)
		if err != nil {
			t.Fatalf("encodeGeminiRequest failed: %v", err)
		}
		body := string(raw)
		if !strings.Contains(body, `"thinkingConfig":{"includeThoughts":true,"thinkingBudget":1024}`) {
			t.Fatalf("expected enabled thinkingConfig, got: %s", body)
		}
	})

	t.Run("disabled forces thinkingBudget=0", func(t *testing.T) {
		conv := base
		conv.Thinking = &anthropicThinkingConfig{Type: "disabled"}
		raw, err := encodeGeminiRequest(conv)
		if err != nil {
			t.Fatalf("encodeGeminiRequest failed: %v", err)
		}
		if !strings.Contains(string(raw), `"thinkingConfig":{"thinkingBudget":0}`) {
			t.Fatalf("expected disabled thinkingConfig, got: %s", string(raw))
		}
	})

	t.Run("nil emits no generationConfig", func(t *testing.T) {
		raw, err := encodeGeminiRequest(base)
		if err != nil {
			t.Fatalf("encodeGeminiRequest failed: %v", err)
		}
		if strings.Contains(string(raw), `"generationConfig"`) {
			t.Fatalf("expected no generationConfig without thinking, got: %s", string(raw))
		}
	})
}

// 覆盖 OpenAI 入站顶层 parallel_tool_calls=false → DisableParallel 透传。
func TestNormalizeOpenAI_TopLevelParallelToolCallsDisabled(t *testing.T) {
	f := false
	req := openAIChatRequest{
		Model:             "gpt-x",
		Messages:          []openAIChatMessage{{Role: "user", Content: "hi"}},
		ParallelToolCalls: &f,
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		t.Fatalf("normalizeOpenAIConversation failed: %v", err)
	}
	if !conv.ToolChoice.DisableParallel {
		t.Fatalf("expected DisableParallel=true, got %+v", conv.ToolChoice)
	}
}

// 覆盖 OpenAI 入站采样参数 → Anthropic 请求透传。
func TestOpenAIToAnthropic_SamplingPropagation(t *testing.T) {
	temp := 0.3
	topP := 0.9
	topK := 40
	mct := 2048
	req := openAIChatRequest{
		Model:               "gpt-x",
		Messages:            []openAIChatMessage{{Role: "user", Content: "hi"}},
		Temperature:         &temp,
		TopP:                &topP,
		TopK:                &topK,
		MaxCompletionTokens: &mct,
		Stop:                json.RawMessage(`["stop1","stop2"]`),
		ReasoningEffort:     "high",
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		t.Fatalf("normalizeOpenAIConversation failed: %v", err)
	}
	raw, err := encodeAnthropicRequest("claude-x", conv, false)
	if err != nil {
		t.Fatalf("encodeAnthropicRequest failed: %v", err)
	}
	body := string(raw)
	for _, frag := range []string{
		`"max_tokens":2048`, `"temperature":0.3`, `"top_p":0.9`, `"top_k":40`,
		`"stop_sequences":["stop1","stop2"]`, `"thinking":{"type":"enabled","budget_tokens":16384}`,
	} {
		if !strings.Contains(body, frag) {
			t.Fatalf("expected fragment %s in body: %s", frag, body)
		}
	}
}

// 覆盖 OpenAI 入站采样参数 → Codex 请求透传（含 reasoning_effort 直通）。
func TestOpenAIToCodex_SamplingPropagation(t *testing.T) {
	temp := 0.5
	topP := 0.95
	maxT := 4096
	req := openAIChatRequest{
		Model:           "gpt-x",
		Messages:        []openAIChatMessage{{Role: "user", Content: "hi"}},
		Temperature:     &temp,
		TopP:            &topP,
		MaxTokens:       &maxT,
		ReasoningEffort: "low",
		User:            "user-42",
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		t.Fatalf("normalizeOpenAIConversation failed: %v", err)
	}
	raw, err := encodeCodexRequest("codex-x", conv, false)
	if err != nil {
		t.Fatalf("encodeCodexRequest failed: %v", err)
	}
	body := string(raw)
	for _, frag := range []string{
		`"temperature":0.5`, `"top_p":0.95`, `"max_output_tokens":4096`,
		`"user":"user-42"`, `"effort":"low"`, `"summary":"auto"`,
		`"include":["reasoning.encrypted_content"]`,
	} {
		if !strings.Contains(body, frag) {
			t.Fatalf("expected fragment %s in body: %s", frag, body)
		}
	}
}

// 覆盖 OpenAI 入站采样参数 → Gemini 请求透传（含 thinkingConfig）。
func TestOpenAIToGemini_SamplingPropagation(t *testing.T) {
	temp := 0.2
	topP := 0.8
	topK := 50
	maxT := 1024
	seed := int64(12345)
	req := openAIChatRequest{
		Model:           "gpt-x",
		Messages:        []openAIChatMessage{{Role: "user", Content: "hi"}},
		Temperature:     &temp,
		TopP:            &topP,
		TopK:            &topK,
		MaxTokens:       &maxT,
		Stop:            json.RawMessage(`"END"`),
		ReasoningEffort: "medium",
		Seed:            &seed,
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		t.Fatalf("normalizeOpenAIConversation failed: %v", err)
	}
	raw, err := encodeGeminiRequest(conv)
	if err != nil {
		t.Fatalf("encodeGeminiRequest failed: %v", err)
	}
	body := string(raw)
	for _, frag := range []string{
		`"temperature":0.2`, `"topP":0.8`, `"topK":50`, `"maxOutputTokens":1024`,
		`"stopSequences":["END"]`, `"seed":12345`,
		`"thinkingConfig":{"includeThoughts":true,"thinkingBudget":4096}`,
	} {
		if !strings.Contains(body, frag) {
			t.Fatalf("expected fragment %s in body: %s", frag, body)
		}
	}
}

// 覆盖 OpenAI 入站采样参数 → OpenAI 请求直通（保留完整语义）。
func TestOpenAIToOpenAI_SamplingPropagation(t *testing.T) {
	temp := 0.4
	topP := 0.85
	maxT := 512
	seed := int64(7)
	fp := 0.1
	pp := -0.1
	req := openAIChatRequest{
		Model:            "gpt-x",
		Messages:         []openAIChatMessage{{Role: "user", Content: "hi"}},
		Temperature:      &temp,
		TopP:             &topP,
		MaxTokens:        &maxT,
		Stop:             json.RawMessage(`["a","b"]`),
		ReasoningEffort:  "high",
		Seed:             &seed,
		FrequencyPenalty: &fp,
		PresencePenalty:  &pp,
		User:             "alice",
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		t.Fatalf("normalizeOpenAIConversation failed: %v", err)
	}
	raw, err := encodeOpenAIRequest("gpt-x", conv, false)
	if err != nil {
		t.Fatalf("encodeOpenAIRequest failed: %v", err)
	}
	body := string(raw)
	for _, frag := range []string{
		`"temperature":0.4`, `"top_p":0.85`, `"max_tokens":512`,
		`"stop":["a","b"]`, `"reasoning_effort":"high"`, `"seed":7`,
		`"frequency_penalty":0.1`, `"presence_penalty":-0.1`, `"user":"alice"`,
	} {
		if !strings.Contains(body, frag) {
			t.Fatalf("expected fragment %s in body: %s", frag, body)
		}
	}
}

// 覆盖 P0 #1+P1 #4：normalizeAnthropicConversation 解析顶层 thinking + tool_choice.disable_parallel_tool_use。
func TestNormalizeAnthropic_ThinkingAndDisableParallel(t *testing.T) {
	req := anthropicMessagesRequest{
		Model:      "claude-x",
		Messages:   []anthropicMessageContent{{Role: "user", Content: "hi"}},
		Tools:      json.RawMessage(`[{"name":"lookup","description":"d","input_schema":{"type":"object"}}]`),
		ToolChoice: json.RawMessage(`{"type":"auto","disable_parallel_tool_use":true}`),
		Thinking:   &anthropicThinkingConfig{Type: "enabled", BudgetTokens: 4096},
	}
	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicConversation failed: %v", err)
	}
	if !conv.ToolChoice.DisableParallel {
		t.Fatalf("expected ToolChoice.DisableParallel=true, got %+v", conv.ToolChoice)
	}
	if conv.Thinking == nil || conv.Thinking.Type != "enabled" || conv.Thinking.BudgetTokens != 4096 {
		t.Fatalf("expected Thinking{enabled,4096}, got %+v", conv.Thinking)
	}
}

// 覆盖 Codex 入站顶层 parallel_tool_calls=false / 采样 / reasoning 字段透传。
func TestNormalizeCodex_SamplingReasoningAndParallel(t *testing.T) {
	temp := 0.7
	topP := 0.9
	maxTok := 2048
	f := false
	req := codexRequest{
		Model:             "gpt-5",
		Input:             []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)},
		Reasoning:         &codexReasoning{Effort: "high", Summary: "auto"},
		ParallelToolCalls: &f,
		Temperature:       &temp,
		TopP:              &topP,
		MaxOutputTokens:   &maxTok,
		Stop:              json.RawMessage(`["END"]`),
		User:              "tester",
	}
	conv, err := normalizeCodexConversation(req)
	if err != nil {
		t.Fatalf("normalizeCodexConversation failed: %v", err)
	}
	if !conv.ToolChoice.DisableParallel {
		t.Fatalf("expected DisableParallel=true, got %+v", conv.ToolChoice)
	}
	if conv.Sampling == nil || conv.Sampling.Temperature == nil || *conv.Sampling.Temperature != 0.7 ||
		conv.Sampling.TopP == nil || *conv.Sampling.TopP != 0.9 ||
		conv.Sampling.MaxTokens == nil || *conv.Sampling.MaxTokens != 2048 ||
		conv.Sampling.ReasoningEffort != "high" || conv.Sampling.User != "tester" ||
		len(conv.Sampling.Stop) != 1 || conv.Sampling.Stop[0] != "END" {
		t.Fatalf("sampling mismatch: %+v", conv.Sampling)
	}
	if conv.Thinking == nil || conv.Thinking.Type != "enabled" || conv.Thinking.BudgetTokens != 16384 {
		t.Fatalf("expected Thinking{enabled,16384} from reasoning.effort=high, got %+v", conv.Thinking)
	}
}

// 覆盖 Codex→OpenAI：透传 temperature/top_p/max_tokens/stop/reasoning_effort/user/parallel_tool_calls=false。
func TestConvertCodexRequestToOpenAI_FieldsPreserved(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"reasoning":{"effort":"medium","summary":"auto"},
		"parallel_tool_calls":false,
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"temperature":0.5,
		"top_p":0.8,
		"max_output_tokens":512,
		"stop":["DONE"],
		"user":"u1"
	}`)
	out, err := convertCodexRequestToOpenAI("gpt-5", raw, false)
	if err != nil {
		t.Fatalf("convertCodexRequestToOpenAI failed: %v", err)
	}
	body := string(out)
	for _, want := range []string{
		`"temperature":0.5`, `"top_p":0.8`, `"max_tokens":512`,
		`"stop":["DONE"]`, `"user":"u1"`,
		`"reasoning_effort":"medium"`, `"parallel_tool_calls":false`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in output: %s", want, body)
		}
	}
}

// 覆盖 Codex→Anthropic：temperature/top_p/max_tokens/stop_sequences/thinking 全部透传，
// 并在 DisableParallel 时给 tool_choice 注入 disable_parallel_tool_use=true。
func TestConvertCodexRequestToAnthropic_FieldsPreserved(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"reasoning":{"effort":"high"},
		"parallel_tool_calls":false,
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"temperature":0.3,
		"top_p":0.95,
		"max_output_tokens":4096,
		"stop":["BYE"]
	}`)
	out, err := convertCodexRequestToAnthropic("claude-sonnet", raw, false)
	if err != nil {
		t.Fatalf("convertCodexRequestToAnthropic failed: %v", err)
	}
	body := string(out)
	for _, want := range []string{
		`"temperature":0.3`, `"top_p":0.95`, `"max_tokens":4096`,
		`"stop_sequences":["BYE"]`,
		`"thinking":{"type":"enabled","budget_tokens":16384}`,
		`"disable_parallel_tool_use":true`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in output: %s", want, body)
		}
	}
}

// 覆盖 Codex→Gemini：采样 + thinkingBudget 映射 + stopSequences 写入 generationConfig。
func TestConvertCodexRequestToGemini_FieldsPreserved(t *testing.T) {
	raw := []byte(`{
		"model":"gemini-2",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"reasoning":{"effort":"medium"},
		"temperature":0.4,
		"top_p":0.85,
		"max_output_tokens":1024,
		"stop":["STOP"]
	}`)
	out, err := convertCodexRequestToGemini("gemini-2", raw, false)
	if err != nil {
		t.Fatalf("convertCodexRequestToGemini failed: %v", err)
	}
	body := string(out)
	for _, want := range []string{
		`"temperature":0.4`, `"topP":0.85`, `"maxOutputTokens":1024`,
		`"stopSequences":["STOP"]`,
		`"thinkingBudget":4096`, `"includeThoughts":true`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in output: %s", want, body)
		}
	}
}

// 覆盖 Anthropic 编码器：任意入站在 DisableParallel + 有工具 时注入 disable_parallel_tool_use，
// 即便原 Mode 未设置也要初始化为 {"type":"auto"} 再挂字段。
func TestEncodeAnthropicRequest_DisableParallelInjected(t *testing.T) {
	conv := conversation{
		Turns:      []conversationTurn{{Role: "user", Parts: []conversationPart{{Kind: partKindText, Text: "hi"}}}},
		Tools:      []conversationTool{{Type: "function", Name: "lookup"}},
		ToolChoice: conversationToolChoice{DisableParallel: true}, // Mode=""，确保初始化分支生效
	}
	raw, err := encodeAnthropicRequest("claude-x", conv, false)
	if err != nil {
		t.Fatalf("encodeAnthropicRequest failed: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"tool_choice":{`) ||
		!strings.Contains(body, `"type":"auto"`) ||
		!strings.Contains(body, `"disable_parallel_tool_use":true`) {
		t.Fatalf("expected tool_choice with disable_parallel_tool_use=true, got: %s", body)
	}
}
