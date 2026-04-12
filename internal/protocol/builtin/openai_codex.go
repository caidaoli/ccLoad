package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
)

type openAIToCodexStreamState struct {
	model string
	usage struct {
		promptTokens     int64
		completionTokens int64
		totalTokens      int64
		seen             bool
	}
}

type codexToOpenAIStreamState struct {
	model string
	usage struct {
		inputTokens  int64
		outputTokens int64
		totalTokens  int64
		seen         bool
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
		out["usage"] = map[string]any{
			"input_tokens":  usage.promptTokens,
			"output_tokens": usage.completionTokens,
			"total_tokens":  usage.totalTokens,
		}
	}
	return sonic.Marshal(out)
}

func convertCodexResponseToOpenAINonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	message, toolCalls, err := openAIMessageFromCodexOutput(resp["output"])
	if err != nil {
		return nil, err
	}
	choiceMessage := map[string]any{
		"role":    "assistant",
		"content": message,
	}
	if len(toolCalls) > 0 {
		choiceMessage["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      "chatcmpl-proxy",
		"object":  "chat.completion",
		"created": 0,
		"model":   coalesceModel(model, resp["model"]),
		"choices": []map[string]any{{
			"index":         0,
			"message":       choiceMessage,
			"finish_reason": "stop",
		}},
	}
	if usage := codexUsageFromMap(resp["usage"]); usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     usage.inputTokens,
			"completion_tokens": usage.outputTokens,
			"total_tokens":      usage.totalTokens,
		}
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
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		response := map[string]any{
			"id":     "resp-proxy",
			"object": "response",
			"status": "completed",
			"model":  st.model,
		}
		if st.usage.seen {
			response["usage"] = map[string]any{
				"input_tokens":  st.usage.promptTokens,
				"output_tokens": st.usage.completionTokens,
				"total_tokens":  st.usage.totalTokens,
			}
		}
		done := map[string]any{"type": "response.completed", "response": response}
		body, err := sonic.Marshal(done)
		if err != nil {
			return nil, err
		}
		return [][]byte{append([]byte("event: response.completed\ndata: "), append(body, []byte("\n\n")...)...)}, nil
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
		st.usage.seen = true
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	content := stringValue(delta["content"])
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
			chunk["usage"] = map[string]any{
				"prompt_tokens":     st.usage.inputTokens,
				"completion_tokens": st.usage.outputTokens,
				"total_tokens":      st.usage.totalTokens,
			}
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
	return nil, nil
}

type openAIUsage struct {
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
}

type codexUsage struct {
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
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
	return items, nil
}

func openAIMessageFromCodexOutput(output any) (any, []map[string]any, error) {
	items, _ := output.([]any)
	contentParts := make([]map[string]any, 0)
	toolCalls := make([]map[string]any, 0)
	for i, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("unsupported codex output item at index %d", i)
		}
		typ := normalizeRole(stringValue(itemMap["type"]))
		switch typ {
		case "message":
			parts, err := extractCodexContentParts(itemMap["content"])
			if err != nil {
				return nil, nil, err
			}
			for _, part := range parts {
				encoded, err := encodeOpenAIContentPart(part)
				if err != nil {
					return nil, nil, err
				}
				contentParts = append(contentParts, encoded)
			}
		case "function_call":
			call, err := decodeCodexToolCall(itemMap)
			if err != nil {
				return nil, nil, err
			}
			encoded, err := encodeOpenAIToolCall(&call)
			if err != nil {
				return nil, nil, err
			}
			toolCalls = append(toolCalls, encoded)
		}
	}
	return encodeOpenAIContentValue(contentParts), toolCalls, nil
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

func openAIUsageFromMap(value any) *openAIUsage {
	usageMap, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	usage := &openAIUsage{
		promptTokens:     int64Value(usageMap["prompt_tokens"]),
		completionTokens: int64Value(usageMap["completion_tokens"]),
		totalTokens:      int64Value(usageMap["total_tokens"]),
	}
	if usage.promptTokens == 0 && usage.completionTokens == 0 && usage.totalTokens == 0 {
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
		inputTokens:  int64Value(usageMap["input_tokens"]),
		outputTokens: int64Value(usageMap["output_tokens"]),
		totalTokens:  int64Value(usageMap["total_tokens"]),
	}
	if usage.inputTokens == 0 && usage.outputTokens == 0 && usage.totalTokens == 0 {
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
