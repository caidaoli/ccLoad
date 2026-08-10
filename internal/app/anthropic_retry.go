package app

import (
	"encoding/json"
	"net/http"
	"strings"

	"ccLoad/internal/protocol"
)

func anthropicRetryBodyFor400(
	upstreamProtocol protocol.Protocol,
	plan protocol.TransformPlan,
	res *fwResult,
) ([]byte, string, bool) {
	if upstreamProtocol != protocol.Anthropic || res == nil || res.ResponseCommitted || res.Status != http.StatusBadRequest {
		return nil, "", false
	}
	if !isAnthropicRepairableValidationError(res.Body) {
		return nil, "", false
	}
	errorText := anthropicErrorText(res.Body)
	if isAnthropicThinkingBudgetError(errorText) {
		if body, ok := rectifyAnthropicThinkingBudget(plan.TranslatedBody); ok {
			return body, "rectify_anthropic_thinking_budget", true
		}
	}
	if isAnthropicThinkingBlockError(errorText) {
		if body, ok := downgradeAnthropicThinkingBlocks(plan.TranslatedBody); ok {
			return body, "downgrade_anthropic_thinking", true
		}
	}
	if isAnthropicToolBlockError(errorText) || isAnthropicThinkingBlockError(errorText) {
		if body, ok := downgradeAnthropicToolBlocks(plan.TranslatedBody); ok {
			return body, "downgrade_anthropic_tools", true
		}
	}
	return nil, "", false
}

func isAnthropicRepairableValidationError(body []byte) bool {
	var payload struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return true
	}
	typ := strings.ToLower(strings.TrimSpace(payload.Error.Type))
	code := strings.ToLower(strings.TrimSpace(payload.Error.Code))
	if typ == "" && code == "" {
		return true
	}
	return typ == "invalid_request_error" || code == "invalid_request_error" ||
		code == "invalid_parameter" || code == "invalid_argument"
}

func anthropicErrorText(body []byte) string {
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return strings.ToLower(string(body))
	}
	result := strings.ToLower(strings.Join([]string{
		payload.Error.Type, payload.Error.Code, payload.Error.Param, payload.Error.Message,
	}, " "))
	if strings.TrimSpace(result) == "" {
		return strings.ToLower(string(body))
	}
	return result
}

func isAnthropicThinkingBudgetError(errorText string) bool {
	mentionsBudget := strings.Contains(errorText, "budget_tokens") || strings.Contains(errorText, "thinking budget")
	if !mentionsBudget {
		return false
	}
	return strings.Contains(errorText, "max_tokens") || strings.Contains(errorText, "minimum") ||
		strings.Contains(errorText, "at least") || strings.Contains(errorText, "greater") ||
		strings.Contains(errorText, "less than") || strings.Contains(errorText, "must be") ||
		strings.Contains(errorText, ">=") || strings.Contains(errorText, "<=") || strings.Contains(errorText, "invalid")
}

func isAnthropicThinkingBlockError(errorText string) bool {
	return strings.Contains(errorText, "thinking") || strings.Contains(errorText, "redacted_thinking")
}

func isAnthropicToolBlockError(errorText string) bool {
	return strings.Contains(errorText, "tool_use") || strings.Contains(errorText, "tool_result") ||
		strings.Contains(errorText, "tool choice") || strings.Contains(errorText, "tool_choice")
}

func downgradeAnthropicThinkingBlocks(body []byte) ([]byte, bool) {
	request, err := decodeAnthropicRequest(body)
	if err != nil {
		return nil, false
	}
	changed := false
	if _, exists := request["thinking"]; exists {
		delete(request, "thinking")
		deleteAnthropicOutputEffort(request)
		changed = true
	}
	if _, exists := request["context_management"]; exists {
		delete(request, "context_management")
		changed = true
	}
	messages, _ := request["messages"].([]any)
	filteredMessages := make([]any, 0, len(messages))
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			filteredMessages = append(filteredMessages, rawMessage)
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			filteredMessages = append(filteredMessages, message)
			continue
		}
		filteredContent := make([]any, 0, len(content))
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				filteredContent = append(filteredContent, rawBlock)
				continue
			}
			switch stringValue(block["type"]) {
			case "thinking":
				changed = true
				if text := strings.TrimSpace(stringValue(block["thinking"])); text != "" {
					replacement := map[string]any{"type": "text", "text": text}
					if cache, exists := block["cache_control"]; exists {
						replacement["cache_control"] = cache
					}
					filteredContent = append(filteredContent, replacement)
				}
			case "redacted_thinking":
				changed = true
			default:
				filteredContent = append(filteredContent, block)
			}
		}
		if len(filteredContent) == 0 {
			changed = true
			continue
		}
		message["content"] = filteredContent
		filteredMessages = append(filteredMessages, message)
	}
	request["messages"] = filteredMessages
	if !changed {
		return nil, false
	}
	encoded, err := json.Marshal(request)
	return encoded, err == nil
}

func downgradeAnthropicToolBlocks(body []byte) ([]byte, bool) {
	request, err := decodeAnthropicRequest(body)
	if err != nil {
		return nil, false
	}
	changed := false
	if _, exists := request["tools"]; exists {
		delete(request, "tools")
		changed = true
	}
	if _, exists := request["tool_choice"]; exists {
		delete(request, "tool_choice")
		changed = true
	}
	messages, _ := request["messages"].([]any)
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for index, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(block["type"]) {
			case "tool_use":
				content[index] = map[string]any{"type": "text", "text": anthropicToolUseText(block)}
				changed = true
			case "tool_result":
				content[index] = map[string]any{"type": "text", "text": anthropicToolResultText(block)}
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	encoded, err := json.Marshal(request)
	return encoded, err == nil
}

func anthropicToolUseText(block map[string]any) string {
	payload, _ := json.Marshal(block["input"])
	return "[Tool call: " + stringValue(block["name"]) + "]\n" + string(payload)
}

func anthropicToolResultText(block map[string]any) string {
	prefix := "[Tool result: " + stringValue(block["tool_use_id"]) + "]\n"
	switch content := block["content"].(type) {
	case string:
		return prefix + content
	case []any:
		parts := make([]string, 0, len(content))
		for _, raw := range content {
			if nested, ok := raw.(map[string]any); ok && stringValue(nested["type"]) == "text" {
				parts = append(parts, stringValue(nested["text"]))
				continue
			}
			encoded, _ := json.Marshal(raw)
			parts = append(parts, string(encoded))
		}
		return prefix + strings.Join(parts, "\n")
	default:
		encoded, _ := json.Marshal(content)
		return prefix + string(encoded)
	}
}

func rectifyAnthropicThinkingBudget(body []byte) ([]byte, bool) {
	request, err := decodeAnthropicRequest(body)
	if err != nil {
		return nil, false
	}
	thinking, ok := request["thinking"].(map[string]any)
	if !ok || strings.EqualFold(stringValue(thinking["type"]), "adaptive") {
		return nil, false
	}
	const repairedBudget int64 = 32000
	changed := !strings.EqualFold(stringValue(thinking["type"]), "enabled")
	thinking["type"] = "enabled"
	if budget, ok := anthropicInteger(thinking["budget_tokens"]); !ok || budget != repairedBudget {
		thinking["budget_tokens"] = repairedBudget
		changed = true
	}
	if maxTokens, ok := anthropicInteger(request["max_tokens"]); !ok || maxTokens <= repairedBudget {
		request["max_tokens"] = int64(64000)
		changed = true
	}
	if !changed {
		return nil, false
	}
	encoded, err := json.Marshal(request)
	return encoded, err == nil
}
