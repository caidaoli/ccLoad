package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
)

type pendingToolCall struct {
	id        string
	name      string
	arguments string
}

type openAIToCodexStreamState struct {
	model string
	usage struct {
		promptTokens             int64
		completionTokens         int64
		totalTokens              int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
	reasoningText      string
	reasoningEncrypted string
	toolCalls          map[int]*pendingToolCall
}

type codexToOpenAIStreamState struct {
	model string
	usage struct {
		inputTokens              int64
		outputTokens             int64
		totalTokens              int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
}

func convertOpenAIRequestToCodex(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req openAIChatRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeCodexRequest(model, conv, stream)
}

func convertCodexRequestToOpenAI(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req codexRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeCodexConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeOpenAIRequest(model, conv, stream)
}

func convertOpenAIResponseToCodexNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	output, err := codexOutputItemsFromOpenAIResponse(resp)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id":     "resp-proxy",
		"object": "response",
		"status": "completed",
		"model":  coalesceModel(model, resp["model"]),
		"output": output,
	}
	if usage := openAIUsageFromMap(resp["usage"]); usage != nil {
		out["usage"] = codexUsagePayload(&codexUsage{
			inputTokens:              usage.promptTokens,
			outputTokens:             usage.completionTokens,
			totalTokens:              usage.totalTokens,
			cachedTokens:             usage.cachedTokens,
			cacheCreationInputTokens: usage.cacheCreationInputTokens,
			reasoningTokens:          usage.reasoningTokens,
		})
	}
	return sonic.Marshal(out)
}

func convertCodexResponseToOpenAINonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	message, err := openAIMessageFromCodexOutput(resp["output"])
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id":      "chatcmpl-proxy",
		"object":  "chat.completion",
		"created": 0,
		"model":   coalesceModel(model, resp["model"]),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
	}
	if usage := codexUsageFromMap(resp["usage"]); usage != nil {
		out["usage"] = openAIUsagePayload(&openAIUsage{
			promptTokens:             usage.inputTokens,
			completionTokens:         usage.outputTokens,
			totalTokens:              usage.totalTokens,
			cachedTokens:             usage.cachedTokens,
			cacheCreationInputTokens: usage.cacheCreationInputTokens,
			reasoningTokens:          usage.reasoningTokens,
		})
	}
	return sonic.Marshal(out)
}

func convertOpenAIResponseToCodexStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if *param == nil {
		*param = &openAIToCodexStreamState{model: model}
	}
	st := (*param).(*openAIToCodexStreamState)
	if st.model == "" {
		st.model = model
	}

	line := strings.TrimSpace(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if after, ok := strings.CutPrefix(line, "data:"); ok {
		line = strings.TrimSpace(after)
	}
	if line == "[DONE]" {
		chunks := make([][]byte, 0, 4)
		if st.reasoningText != "" || st.reasoningEncrypted != "" {
			item := map[string]any{
				"type": "response.output_item.done",
				"item": codexReasoningItem(st.reasoningText, st.reasoningEncrypted),
			}
			body, err := sonic.Marshal(item)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, append([]byte("event: response.output_item.done\ndata: "), append(body, []byte("\n\n")...)...))
		}
		// 按 index 顺序发出所有累积的 function_call 事件
		for idx := 0; ; idx++ {
			tc, ok := st.toolCalls[idx]
			if !ok {
				break
			}
			item := map[string]any{
				"type": "response.output_item.done",
				"item": map[string]any{
					"type":      "function_call",
					"call_id":   tc.id,
					"name":      tc.name,
					"arguments": tc.arguments,
				},
			}
			body, err := sonic.Marshal(item)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, append([]byte("event: response.output_item.done\ndata: "), append(body, []byte("\n\n")...)...))
		}
		response := map[string]any{
			"id":     "resp-proxy",
			"object": "response",
			"status": "completed",
			"model":  st.model,
		}
		if st.usage.seen {
			response["usage"] = codexUsagePayload(&codexUsage{
				inputTokens:              st.usage.promptTokens,
				outputTokens:             st.usage.completionTokens,
				totalTokens:              st.usage.totalTokens,
				cachedTokens:             st.usage.cachedTokens,
				cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
				reasoningTokens:          st.usage.reasoningTokens,
			})
		}
		done := map[string]any{"type": "response.completed", "response": response}
		body, err := sonic.Marshal(done)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, append([]byte("event: response.completed\ndata: "), append(body, []byte("\n\n")...)...))
		return chunks, nil
	}

	var chunk map[string]any
	if err := sonic.Unmarshal([]byte(line), &chunk); err != nil {
		return nil, err
	}
	if chunkModel := stringValue(chunk["model"]); chunkModel != "" {
		st.model = chunkModel
	}
	if usage := openAIUsageFromMap(chunk["usage"]); usage != nil {
		st.usage.promptTokens = usage.promptTokens
		st.usage.completionTokens = usage.completionTokens
		st.usage.totalTokens = usage.totalTokens
		st.usage.cachedTokens = usage.cachedTokens
		st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
		st.usage.reasoningTokens = usage.reasoningTokens
		st.usage.seen = true
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	content := stringValue(delta["content"])
	if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
		st.reasoningText += reasoning
		return nil, nil
	}
	if reasoning, _ := delta["reasoning"].(map[string]any); reasoning != nil {
		if encrypted := stringValue(reasoning["encrypted_content"]); encrypted != "" {
			st.reasoningEncrypted = encrypted
		}
		return nil, nil
	}
	// 累积增量 tool_calls（按 index 合并 id/name/arguments）
	if rawCalls, ok := delta["tool_calls"].([]any); ok && len(rawCalls) > 0 {
		if st.toolCalls == nil {
			st.toolCalls = make(map[int]*pendingToolCall)
		}
		for _, raw := range rawCalls {
			tc, _ := raw.(map[string]any)
			if tc == nil {
				continue
			}
			idx := int(int64Value(tc["index"]))
			if _, exists := st.toolCalls[idx]; !exists {
				st.toolCalls[idx] = &pendingToolCall{}
			}
			p := st.toolCalls[idx]
			if id := stringValue(tc["id"]); id != "" {
				p.id = id
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if name := stringValue(fn["name"]); name != "" {
					p.name = name
				}
				if args := stringValue(fn["arguments"]); args != "" {
					p.arguments += args
				}
			}
		}
		return nil, nil
	}
	if content == "" {
		return nil, nil
	}
	payload := map[string]any{"type": "response.output_text.delta", "delta": content}
	body, err := sonic.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return [][]byte{append([]byte("event: response.output_text.delta\ndata: "), append(body, []byte("\n\n")...)...)}, nil
}

func convertCodexResponseToOpenAIStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if *param == nil {
		*param = &codexToOpenAIStreamState{model: model}
	}
	st := (*param).(*codexToOpenAIStreamState)
	if st.model == "" {
		st.model = model
	}

	raw := strings.TrimSpace(string(rawJSON))
	if raw == "" {
		return nil, nil
	}
	eventType, line := parseSSEEventBlock(raw)
	if line == "" {
		return nil, nil
	}

	var payload map[string]any
	if err := sonic.Unmarshal([]byte(line), &payload); err != nil {
		return nil, err
	}
	if response, ok := payload["response"].(map[string]any); ok {
		if responseModel := stringValue(response["model"]); responseModel != "" {
			st.model = responseModel
		}
		if usage := codexUsageFromMap(response["usage"]); usage != nil {
			st.usage.inputTokens = usage.inputTokens
			st.usage.outputTokens = usage.outputTokens
			st.usage.totalTokens = usage.totalTokens
			st.usage.cachedTokens = usage.cachedTokens
			st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
			st.usage.reasoningTokens = usage.reasoningTokens
			st.usage.seen = true
		}
	}
	if eventType == "response.completed" || stringValue(payload["type"]) == "response.completed" {
		chunk := map[string]any{
			"id":      "chatcmpl-proxy",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   st.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		}
		if st.usage.seen {
			chunk["usage"] = openAIUsagePayload(&openAIUsage{
				promptTokens:             st.usage.inputTokens,
				completionTokens:         st.usage.outputTokens,
				totalTokens:              st.usage.totalTokens,
				cachedTokens:             st.usage.cachedTokens,
				cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
				reasoningTokens:          st.usage.reasoningTokens,
			})
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		return [][]byte{
			append([]byte("data: "), append(body, []byte("\n\n")...)...),
			[]byte("data: [DONE]\n\n"),
		}, nil
	}
	if eventType == "response.output_text.delta" || stringValue(payload["type"]) == "response.output_text.delta" {
		delta := stringValue(payload["delta"])
		if delta == "" {
			return nil, nil
		}
		chunk := map[string]any{
			"id":      "chatcmpl-proxy",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   st.model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"content": delta},
			}},
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
	}
	if eventType == "response.output_item.done" || stringValue(payload["type"]) == "response.output_item.done" {
		item, _ := payload["item"].(map[string]any)
		itemType := stringValue(item["type"])
		switch {
		case itemType == "function_call":
			// Codex function_call -> OpenAI tool_calls chunk
			callID := stringValue(item["call_id"])
			name := stringValue(item["name"])
			// arguments 可能是 string 或 object，统一序列化为字符串
			var argsStr string
			switch v := item["arguments"].(type) {
			case string:
				argsStr = v
			default:
				if b, err := sonic.Marshal(v); err == nil {
					argsStr = string(b)
				}
			}
			chunk := map[string]any{
				"id":      "chatcmpl-proxy",
				"object":  "chat.completion.chunk",
				"created": 0,
				"model":   st.model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
							"id":    callID,
							"type":  "function",
							"function": map[string]any{
								"name":      name,
								"arguments": argsStr,
							},
						}},
					},
					"finish_reason": nil,
				}},
			}
			body, err := sonic.Marshal(chunk)
			if err != nil {
				return nil, err
			}
			return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
		case normalizeRole(itemType) == "reasoning":
			text := extractCodexReasoningText(item)
			if text == "" {
				return nil, nil
			}
			chunk := map[string]any{
				"id":      "chatcmpl-proxy",
				"object":  "chat.completion.chunk",
				"created": 0,
				"model":   st.model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{"reasoning_content": text},
				}},
			}
			body, err := sonic.Marshal(chunk)
			if err != nil {
				return nil, err
			}
			return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
		default:
			return nil, nil
		}
	}
	return nil, nil
}

type openAIUsage struct {
	promptTokens             int64
	completionTokens         int64
	totalTokens              int64
	cachedTokens             int64
	cacheCreationInputTokens int64
	reasoningTokens          int64
}

type codexUsage struct {
	inputTokens              int64
	outputTokens             int64
	totalTokens              int64
	cachedTokens             int64
	cacheCreationInputTokens int64
	reasoningTokens          int64
}

func codexOutputItemsFromOpenAIResponse(resp map[string]any) ([]map[string]any, error) {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if len(message) == 0 {
		return nil, nil
	}
	items := make([]map[string]any, 0)
	parts, err := extractOpenAIContentParts(message["content"])
	if err != nil {
		return nil, err
	}
	textContent := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		item, err := encodeCodexOutputContentPart(part)
		if err != nil {
			return nil, err
		}
		if item != nil {
			textContent = append(textContent, item)
		}
	}
	if len(textContent) > 0 {
		items = append(items, map[string]any{"type": "message", "role": "assistant", "content": textContent})
	}
	var toolCalls []openAIChatToolCall
	if rawCalls, ok := message["tool_calls"]; ok {
		bytes, err := sonic.Marshal(rawCalls)
		if err != nil {
			return nil, err
		}
		if err := sonic.Unmarshal(bytes, &toolCalls); err != nil {
			return nil, err
		}
	}
	for _, call := range toolCalls {
		arguments := jsonStringOrObject(call.Function.Arguments)
		items = append(items, map[string]any{
			"type":      "function_call",
			"call_id":   call.ID,
			"name":      call.Function.Name,
			"arguments": arguments,
		})
	}
	items = append(items, codexReasoningItemsFromOpenAIMessage(message)...)
	return items, nil
}

func openAIMessageFromCodexOutput(output any) (map[string]any, error) {
	items, _ := output.([]any)
	contentParts := make([]map[string]any, 0)
	toolCalls := make([]map[string]any, 0)
	reasoning := make([]map[string]any, 0)
	var reasoningBuilder strings.Builder
	for i, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported codex output item at index %d", i)
		}
		typ := normalizeRole(stringValue(itemMap["type"]))
		switch typ {
		case "message":
			parts, err := extractCodexContentParts(itemMap["content"])
			if err != nil {
				return nil, err
			}
			for _, part := range parts {
				encoded, err := encodeOpenAIContentPart(part)
				if err != nil {
					return nil, err
				}
				contentParts = append(contentParts, encoded)
			}
		case "function_call":
			call, err := decodeCodexToolCall(itemMap)
			if err != nil {
				return nil, err
			}
			encoded, err := encodeOpenAIToolCall(&call)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, encoded)
		case "reasoning":
			text := extractCodexReasoningText(itemMap)
			if text != "" {
				reasoningBuilder.WriteString(text)
			}
			entry := map[string]any{"type": "reasoning"}
			if text != "" {
				entry["text"] = text
			}
			if encrypted := stringValue(itemMap["encrypted_content"]); encrypted != "" {
				entry["encrypted_content"] = encrypted
			}
			reasoning = append(reasoning, entry)
		}
	}
	message := map[string]any{
		"role":    "assistant",
		"content": encodeOpenAIContentValue(contentParts),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if reasoningBuilder.Len() > 0 {
		message["reasoning_content"] = reasoningBuilder.String()
	}
	if len(reasoning) > 0 {
		message["reasoning"] = reasoning
	}
	return message, nil
}

func encodeCodexOutputContentPart(part conversationPart) (map[string]any, error) {
	switch part.Kind {
	case partKindText:
		return map[string]any{"type": "output_text", "text": part.Text}, nil
	case partKindImage, partKindFile:
		return nil, fmt.Errorf("unsupported non-text OpenAI response content for Codex output")
	default:
		return nil, nil
	}
}

func codexReasoningItemsFromOpenAIMessage(message map[string]any) []map[string]any {
	items := make([]map[string]any, 0)
	if rawReasoning, ok := message["reasoning"].([]any); ok {
		for _, item := range rawReasoning {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text := stringValue(entry["text"])
			encrypted := stringValue(entry["encrypted_content"])
			items = append(items, codexReasoningItem(text, encrypted))
		}
	}
	if len(items) == 0 {
		if text := stringValue(message["reasoning_content"]); text != "" {
			items = append(items, codexReasoningItem(text, ""))
		}
	}
	return items
}

func extractCodexReasoningText(item map[string]any) string {
	for _, key := range []string{"content", "summary"} {
		parts, _ := item[key].([]any)
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch normalizeRole(stringValue(part["type"])) {
			case "reasoning_text", "summary_text":
				if text := stringValue(part["text"]); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func openAIUsageFromMap(value any) *openAIUsage {
	usageMap, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	usage := &openAIUsage{
		promptTokens:             int64Value(usageMap["prompt_tokens"]),
		completionTokens:         int64Value(usageMap["completion_tokens"]),
		totalTokens:              int64Value(usageMap["total_tokens"]),
		cacheCreationInputTokens: int64Value(usageMap["cache_creation_input_tokens"]),
	}
	if details, ok := usageMap["prompt_tokens_details"].(map[string]any); ok {
		usage.cachedTokens = int64Value(details["cached_tokens"])
	}
	if details, ok := usageMap["completion_tokens_details"].(map[string]any); ok {
		usage.reasoningTokens = int64Value(details["reasoning_tokens"])
	}
	if usage.totalTokens == 0 {
		usage.totalTokens = usage.promptTokens + usage.completionTokens
	}
	if usage.promptTokens == 0 && usage.completionTokens == 0 && usage.totalTokens == 0 && usage.cachedTokens == 0 && usage.cacheCreationInputTokens == 0 && usage.reasoningTokens == 0 {
		return nil
	}
	return usage
}

func codexUsageFromMap(value any) *codexUsage {
	usageMap, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	usage := &codexUsage{
		inputTokens:              int64Value(usageMap["input_tokens"]),
		outputTokens:             int64Value(usageMap["output_tokens"]),
		totalTokens:              int64Value(usageMap["total_tokens"]),
		cacheCreationInputTokens: int64Value(usageMap["cache_creation_input_tokens"]),
	}
	if details, ok := usageMap["input_tokens_details"].(map[string]any); ok {
		usage.cachedTokens = int64Value(details["cached_tokens"])
	} else {
		usage.cachedTokens = int64Value(usageMap["cache_read_input_tokens"])
	}
	if details, ok := usageMap["output_tokens_details"].(map[string]any); ok {
		usage.reasoningTokens = int64Value(details["reasoning_tokens"])
	}
	if usage.totalTokens == 0 {
		usage.totalTokens = usage.inputTokens + usage.outputTokens
	}
	if usage.inputTokens == 0 && usage.outputTokens == 0 && usage.totalTokens == 0 && usage.cachedTokens == 0 && usage.cacheCreationInputTokens == 0 && usage.reasoningTokens == 0 {
		return nil
	}
	return usage
}

func int64Value(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	default:
		return 0
	}
}

func coalesceModel(model string, fallback any) string {
	if strings.TrimSpace(model) != "" {
		return model
	}
	return stringValue(fallback)
}

func jsonStringOrObject(value string) any {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return map[string]any{}
	}
	var decoded any
	if err := sonic.Unmarshal([]byte(trimmed), &decoded); err == nil {
		return decoded
	}
	return trimmed
}
