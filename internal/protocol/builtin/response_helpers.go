package builtin

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
)

func marshalDataSSE(payload any) ([]byte, error) {
	body, err := sonic.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), append(body, []byte("\n\n")...)...), nil
}

func marshalEventSSE(event string, payload any) ([]byte, error) {
	body, err := sonic.Marshal(payload)
	if err != nil {
		return nil, err
	}
	prefix := []byte(fmt.Sprintf("event: %s\ndata: ", event))
	return append(prefix, append(body, []byte("\n\n")...)...), nil
}

func mapOpenAIFinishReasonToGemini(reason string) string {
	switch reason {
	case "":
		return ""
	case "length":
		return "MAX_TOKENS"
	default:
		return "STOP"
	}
}

func mapOpenAIFinishReasonToAnthropic(reason string) string {
	switch reason {
	case "", "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func mapAnthropicStopReasonToGemini(reason string) string {
	switch reason {
	case "max_tokens":
		return "MAX_TOKENS"
	case "":
		return ""
	default:
		return "STOP"
	}
}

func mapAnthropicStopReasonToOpenAI(reason string, hasToolCalls bool) string {
	if hasToolCalls || reason == "tool_use" {
		return "tool_calls"
	}
	switch reason {
	case "max_tokens":
		return "length"
	case "":
		return "stop"
	default:
		return "stop"
	}
}

func mapGeminiFinishReasonToOpenAI(reason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return "length"
	case "":
		return "stop"
	default:
		return "stop"
	}
}

func mapGeminiFinishReasonToAnthropic(reason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_use"
	}
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS":
		return "max_tokens"
	case "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func buildGeminiPayload(model, text, finishReason string, promptTokens, candidateTokens, totalTokens int64, includeUsage bool) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": make([]map[string]any, 0, 1),
		},
	}
	parts := candidate["content"].(map[string]any)["parts"].([]map[string]any)
	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	candidate["content"].(map[string]any)["parts"] = parts
	if finishReason != "" {
		candidate["finishReason"] = finishReason
	}
	payload := map[string]any{"candidates": []map[string]any{candidate}}
	if model != "" {
		payload["modelVersion"] = model
	}
	if includeUsage {
		payload["usageMetadata"] = map[string]any{
			"promptTokenCount":     promptTokens,
			"candidatesTokenCount": candidateTokens,
			"totalTokenCount":      totalTokens,
		}
	}
	return payload
}

func buildGeminiPayloadFromParts(model, responseID string, parts []geminiPart, finishReason string, promptTokens, candidateTokens, totalTokens int64, includeUsage bool) map[string]any {
	if parts == nil {
		parts = []geminiPart{}
	}
	candidate := map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": parts,
		},
	}
	if finishReason != "" {
		candidate["finishReason"] = finishReason
	}
	payload := map[string]any{
		"candidates": []map[string]any{candidate},
	}
	if model != "" {
		payload["modelVersion"] = model
	}
	if responseID != "" {
		payload["responseId"] = responseID
	}
	if includeUsage {
		payload["usageMetadata"] = map[string]any{
			"promptTokenCount":     promptTokens,
			"candidatesTokenCount": candidateTokens,
			"totalTokenCount":      totalTokens,
		}
	}
	return payload
}

func geminiPartsFromConversationParts(parts []conversationPart) ([]geminiPart, error) {
	out := make([]geminiPart, 0, len(parts))
	for _, part := range parts {
		encoded, err := encodeGeminiPart(part)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded)
	}
	return out, nil
}

func conversationPartsFromGeminiParts(parts []geminiPart) ([]conversationPart, error) {
	pendingToolCallIDs := make([]string, 0)
	nextToolCallID := 1
	return extractGeminiParts(parts, &pendingToolCallIDs, &nextToolCallID)
}

func anthropicResponseBlocksFromMaps(blocks []map[string]any) ([]anthropicResponseBlock, error) {
	if len(blocks) == 0 {
		return []anthropicResponseBlock{}, nil
	}
	body, err := sonic.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	var out []anthropicResponseBlock
	if err := sonic.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func hasConversationToolCalls(parts []conversationPart) bool {
	for _, part := range parts {
		if part.Kind == partKindToolCall && part.ToolCall != nil {
			return true
		}
	}
	return false
}

func openAIChatToolCallFromConversation(call *conversationToolCall) (openAIChatToolCall, error) {
	if call == nil {
		return openAIChatToolCall{}, fmt.Errorf("missing tool call")
	}
	arguments := strings.TrimSpace(string(call.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	out := openAIChatToolCall{
		ID:   call.ID,
		Type: "function",
	}
	out.Function.Name = call.Name
	out.Function.Arguments = arguments
	return out, nil
}

func openAIMessageFromConversationParts(parts []conversationPart) (any, []openAIChatToolCall, error) {
	contentParts := make([]map[string]any, 0, len(parts))
	toolCalls := make([]openAIChatToolCall, 0)
	for _, part := range parts {
		switch part.Kind {
		case partKindText, partKindImage, partKindFile:
			encoded, err := encodeOpenAIContentPart(part)
			if err != nil {
				return nil, nil, err
			}
			contentParts = append(contentParts, encoded)
		case partKindToolCall:
			call, err := openAIChatToolCallFromConversation(part.ToolCall)
			if err != nil {
				return nil, nil, err
			}
			toolCalls = append(toolCalls, call)
		default:
			return nil, nil, fmt.Errorf("unsupported OpenAI response part kind %q", part.Kind)
		}
	}
	return encodeOpenAIContentValue(contentParts), toolCalls, nil
}

func codexOutputItemsFromConversationParts(parts []conversationPart) ([]map[string]any, error) {
	items := make([]map[string]any, 0, len(parts))
	messageParts := make([]map[string]any, 0, len(parts))
	flushMessage := func() {
		if len(messageParts) == 0 {
			return
		}
		items = append(items, map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": messageParts,
		})
		messageParts = nil
	}
	for _, part := range parts {
		switch part.Kind {
		case partKindText:
			encoded, err := encodeCodexOutputContentPart(part)
			if err != nil {
				return nil, err
			}
			if encoded != nil {
				messageParts = append(messageParts, encoded)
			}
		case partKindToolCall:
			flushMessage()
			encoded, err := encodeCodexToolCall(part.ToolCall)
			if err != nil {
				return nil, err
			}
			items = append(items, encoded)
		default:
			return nil, fmt.Errorf("unsupported Codex response part kind %q", part.Kind)
		}
	}
	flushMessage()
	return items, nil
}

func openAIMessageFromAnthropicBlocks(blocks []anthropicResponseBlock) (openAIChatCompletionMessage, error) {
	message := openAIChatCompletionMessage{Role: "assistant"}
	contentParts := make([]map[string]any, 0, len(blocks))
	toolCalls := make([]openAIChatToolCall, 0)
	reasoning := make([]map[string]any, 0)
	var reasoningBuilder strings.Builder

	for _, block := range blocks {
		switch normalizeRole(block.Type) {
		case "thinking":
			if block.Thinking != "" {
				reasoning = append(reasoning, map[string]any{
					"type":      "thinking",
					"text":      block.Thinking,
					"signature": block.Signature,
				})
				reasoningBuilder.WriteString(block.Thinking)
			}
		case "redacted_thinking":
			reasoning = append(reasoning, map[string]any{
				"type": "redacted_thinking",
				"data": block.Data,
			})
		default:
			part, err := decodeAnthropicContentBlock(mustMap(block))
			if err != nil {
				return openAIChatCompletionMessage{}, err
			}
			switch part.Kind {
			case partKindText, partKindImage, partKindFile:
				encoded, err := encodeOpenAIContentPart(part)
				if err != nil {
					return openAIChatCompletionMessage{}, err
				}
				contentParts = append(contentParts, encoded)
			case partKindToolCall:
				call, err := openAIChatToolCallFromConversation(part.ToolCall)
				if err != nil {
					return openAIChatCompletionMessage{}, err
				}
				toolCalls = append(toolCalls, call)
			case "":
			default:
				return openAIChatCompletionMessage{}, fmt.Errorf("unsupported anthropic response block type %q", block.Type)
			}
		}
	}

	message.Content = encodeOpenAIContentValue(contentParts)
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}
	if reasoningBuilder.Len() > 0 {
		message.ReasoningContent = reasoningBuilder.String()
	}
	if len(reasoning) > 0 {
		message.Reasoning = reasoning
	}
	if message.ReasoningContent != "" && message.Content == "" {
		message.Text = message.ReasoningContent
	}
	return message, nil
}

func codexOutputItemsFromAnthropicBlocks(blocks []anthropicResponseBlock) ([]map[string]any, error) {
	items := make([]map[string]any, 0, len(blocks))
	messageParts := make([]map[string]any, 0, len(blocks))
	flushMessage := func() {
		if len(messageParts) == 0 {
			return
		}
		items = append(items, map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": messageParts,
		})
		messageParts = nil
	}
	for _, block := range blocks {
		switch normalizeRole(block.Type) {
		case "thinking":
			flushMessage()
			items = append(items, codexReasoningItem(block.Thinking, block.Signature))
		case "redacted_thinking":
			flushMessage()
			items = append(items, codexReasoningItem("", block.Data))
		default:
			part, err := decodeAnthropicContentBlock(mustMap(block))
			if err != nil {
				return nil, err
			}
			switch part.Kind {
			case partKindText:
				encoded, err := encodeCodexOutputContentPart(part)
				if err != nil {
					return nil, err
				}
				if encoded != nil {
					messageParts = append(messageParts, encoded)
				}
			case partKindToolCall:
				flushMessage()
				encoded, err := encodeCodexToolCall(part.ToolCall)
				if err != nil {
					return nil, err
				}
				items = append(items, encoded)
			case "":
			default:
				return nil, fmt.Errorf("unsupported anthropic response block type %q", block.Type)
			}
		}
	}
	flushMessage()
	return items, nil
}

func codexReasoningItem(text, encrypted string) map[string]any {
	item := map[string]any{"type": "reasoning"}
	if text != "" {
		item["content"] = []map[string]any{{
			"type": "reasoning_text",
			"text": text,
		}}
	}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}

func mustMap(value any) map[string]any {
	// 热路径：上游解出的 JSON object 已是 map[string]any，直接断言无需序列化往返。
	if m, ok := value.(map[string]any); ok {
		return m
	}
	// 冷路径：value 是结构体或其他类型，回退到 marshal/unmarshal。
	body, err := sonic.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := sonic.Unmarshal(body, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func openAIUsagePayload(usage *openAIUsage) map[string]any {
	if usage == nil {
		return nil
	}
	payload := map[string]any{
		"prompt_tokens":     usage.promptTokens,
		"completion_tokens": usage.completionTokens,
		"total_tokens":      usage.totalTokens,
	}
	if usage.cachedTokens > 0 {
		payload["prompt_tokens_details"] = map[string]any{
			"cached_tokens": usage.cachedTokens,
		}
	}
	if usage.reasoningTokens > 0 {
		payload["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": usage.reasoningTokens,
		}
	}
	if usage.cacheCreationInputTokens > 0 {
		payload["cache_creation_input_tokens"] = usage.cacheCreationInputTokens
	}
	return payload
}

func codexUsagePayload(usage *codexUsage) map[string]any {
	if usage == nil {
		return nil
	}
	payload := map[string]any{
		"input_tokens":  usage.inputTokens,
		"output_tokens": usage.outputTokens,
		"total_tokens":  usage.totalTokens,
	}
	if usage.cachedTokens > 0 {
		payload["input_tokens_details"] = map[string]any{
			"cached_tokens": usage.cachedTokens,
		}
	}
	if usage.reasoningTokens > 0 {
		payload["output_tokens_details"] = map[string]any{
			"reasoning_tokens": usage.reasoningTokens,
		}
	}
	if usage.cacheCreationInputTokens > 0 {
		payload["cache_creation_input_tokens"] = usage.cacheCreationInputTokens
	}
	return payload
}

func openAIUsageFromAnthropicUsage(usage anthropicMessagesUsage) openAIChatCompletionUsage {
	promptTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	totalTokens := promptTokens + usage.OutputTokens
	out := openAIChatCompletionUsage{
		PromptTokens:             promptTokens,
		CompletionTokens:         usage.OutputTokens,
		TotalTokens:              totalTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
	}
	if usage.CacheReadInputTokens > 0 {
		out.PromptTokensDetails = &openAITokenDetails{CachedTokens: usage.CacheReadInputTokens}
	}
	if usage.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &openAITokenDetails{ReasoningTokens: usage.ReasoningTokens}
	}
	return out
}

func codexUsageFromAnthropicUsage(usage anthropicMessagesUsage) *codexUsage {
	inputTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	totalTokens := inputTokens + usage.OutputTokens
	return &codexUsage{
		inputTokens:              inputTokens,
		outputTokens:             usage.OutputTokens,
		totalTokens:              totalTokens,
		cachedTokens:             usage.CacheReadInputTokens,
		cacheCreationInputTokens: usage.CacheCreationInputTokens,
		reasoningTokens:          usage.ReasoningTokens,
	}
}

func decodeObjectSlice(value any) ([]map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	// 热路径 1：[]map[string]any 直接返回。
	if items, ok := value.([]map[string]any); ok {
		return items, nil
	}
	// 热路径 2：[]any 中每项已是 map[string]any（sonic 解析出的 JSON 数组的常见形态）。
	if arr, ok := value.([]any); ok {
		items := make([]map[string]any, 0, len(arr))
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				// 数组里混入非对象元素，回退到完整 marshal/unmarshal 走序列化语义。
				return decodeObjectSliceFallback(value)
			}
			items = append(items, m)
		}
		return items, nil
	}
	// 冷路径：结构体或其他类型，回退完整序列化往返。
	return decodeObjectSliceFallback(value)
}

func decodeObjectSliceFallback(value any) ([]map[string]any, error) {
	body, err := sonic.Marshal(value)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	if err := sonic.Unmarshal(body, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func decodeOpenAIToolCalls(value any) ([]openAIChatToolCall, error) {
	if value == nil {
		return nil, nil
	}
	body, err := sonic.Marshal(value)
	if err != nil {
		return nil, err
	}
	var calls []openAIChatToolCall
	if err := sonic.Unmarshal(body, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

func geminiPartsFromOpenAIMessage(content any, rawToolCalls any) ([]geminiPart, error) {
	parts, err := extractOpenAIContentParts(content)
	if err != nil {
		return nil, err
	}
	toolCalls, err := decodeOpenAIToolCalls(rawToolCalls)
	if err != nil {
		return nil, err
	}
	toolParts, err := extractOpenAIToolCallParts(toolCalls)
	if err != nil {
		return nil, err
	}
	parts = append(parts, toolParts...)
	return geminiPartsFromConversationParts(parts)
}

func geminiPartsFromAnthropicContent(value any) ([]geminiPart, error) {
	blocks, err := decodeObjectSlice(value)
	if err != nil {
		return nil, err
	}
	parts := make([]conversationPart, 0, len(blocks))
	for _, block := range blocks {
		part, err := decodeAnthropicContentBlock(block)
		if err != nil {
			return nil, err
		}
		if part.Kind != "" {
			parts = append(parts, part)
		}
	}
	return geminiPartsFromConversationParts(parts)
}

func geminiPartsFromCodexOutput(value any, restore func(string) string) ([]geminiPart, error) {
	if restore == nil {
		restore = func(name string) string { return name }
	}
	items, err := decodeObjectSlice(value)
	if err != nil {
		return nil, err
	}
	parts := make([]conversationPart, 0, len(items))
	for _, item := range items {
		switch normalizeRole(stringValue(item["type"])) {
		case "message":
			contentParts, err := extractCodexContentParts(item["content"])
			if err != nil {
				return nil, err
			}
			parts = append(parts, contentParts...)
		case "function_call":
			call, err := decodeCodexToolCall(item)
			if err != nil {
				return nil, err
			}
			call.Name = restore(call.Name)
			parts = append(parts, conversationPart{Kind: partKindToolCall, ToolCall: &call})
		case "function_call_output":
			result, err := decodeCodexToolResult(item)
			if err != nil {
				return nil, err
			}
			parts = append(parts, conversationPart{Kind: partKindToolResult, ToolResult: &result})
		}
	}
	return geminiPartsFromConversationParts(parts)
}
