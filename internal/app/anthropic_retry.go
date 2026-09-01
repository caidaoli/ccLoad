package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/protocol"
	cliproxycommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	if !isMutableJSONObject(body) {
		return nil, false
	}
	changed := false
	updated := body
	for _, key := range []string{"thinking", "context_management", "output_config.effort"} {
		if !gjson.GetBytes(updated, key).Exists() {
			continue
		}
		var err error
		updated, err = sjson.DeleteBytes(updated, key)
		if err != nil {
			return nil, false
		}
		changed = true
	}
	if outputConfig := gjson.GetBytes(updated, "output_config"); outputConfig.IsObject() {
		empty := true
		outputConfig.ForEach(func(_, _ gjson.Result) bool {
			empty = false
			return false
		})
		if empty {
			var err error
			updated, err = sjson.DeleteBytes(updated, "output_config")
			if err != nil {
				return nil, false
			}
		}
	}
	messages := gjson.GetBytes(updated, "messages")
	if !messages.IsArray() {
		if !changed {
			return nil, false
		}
		return updated, true
	}
	for messageIndex := len(messages.Array()) - 1; messageIndex >= 0; messageIndex-- {
		message := gjson.GetBytes(updated, fmt.Sprintf("messages.%d", messageIndex))
		content := message.Get("content")
		if !message.IsObject() || !content.IsArray() {
			continue
		}
		rendered := make([][]byte, 0, len(content.Array()))
		messageChanged := false
		for _, block := range content.Array() {
			if !block.IsObject() {
				rendered = append(rendered, []byte(block.Raw))
				continue
			}
			switch block.Get("type").String() {
			case "thinking":
				messageChanged = true
				textValue := block.Get("thinking")
				if textValue.Type == gjson.String && strings.TrimSpace(textValue.String()) != "" {
					text := strings.TrimSpace(textValue.String())
					replacement, err := marshalAnthropicTextBlock(text, block.Get("cache_control"))
					if err != nil {
						return nil, false
					}
					rendered = append(rendered, replacement)
				}
			case "redacted_thinking":
				messageChanged = true
			default:
				rendered = append(rendered, []byte(block.Raw))
			}
		}
		if !messageChanged {
			continue
		}
		changed = true
		var err error
		if len(rendered) == 0 {
			updated, err = sjson.DeleteBytes(updated, fmt.Sprintf("messages.%d", messageIndex))
		} else {
			updated, err = sjson.SetRawBytes(updated, fmt.Sprintf("messages.%d.content", messageIndex), cliproxycommon.JoinRawArray(rendered))
		}
		if err != nil {
			return nil, false
		}
	}
	if !changed {
		return nil, false
	}
	return updated, true
}

func downgradeAnthropicToolBlocks(body []byte) ([]byte, bool) {
	if !isMutableJSONObject(body) {
		return nil, false
	}
	changed := false
	updated := body
	for _, key := range []string{"tools", "tool_choice"} {
		if !gjson.GetBytes(updated, key).Exists() {
			continue
		}
		var err error
		updated, err = sjson.DeleteBytes(updated, key)
		if err != nil {
			return nil, false
		}
		changed = true
	}
	messages := gjson.GetBytes(updated, "messages")
	if !messages.IsArray() {
		if !changed {
			return nil, false
		}
		return updated, true
	}
	for messageIndex, message := range messages.Array() {
		content := message.Get("content")
		if !message.IsObject() || !content.IsArray() {
			continue
		}
		rendered := make([][]byte, 0, len(content.Array()))
		messageChanged := false
		for _, block := range content.Array() {
			if !block.IsObject() {
				rendered = append(rendered, []byte(block.Raw))
				continue
			}
			var replacementText string
			switch block.Get("type").String() {
			case "tool_use":
				replacementText = anthropicToolUseText(block)
			case "tool_result":
				replacementText = anthropicToolResultText(block)
			default:
				rendered = append(rendered, []byte(block.Raw))
				continue
			}
			replacement, err := marshalAnthropicTextBlock(replacementText, gjson.Result{})
			if err != nil {
				return nil, false
			}
			rendered = append(rendered, replacement)
			messageChanged = true
		}
		if !messageChanged {
			continue
		}
		var err error
		updated, err = sjson.SetRawBytes(updated, fmt.Sprintf("messages.%d.content", messageIndex), cliproxycommon.JoinRawArray(rendered))
		if err != nil {
			return nil, false
		}
		changed = true
	}
	if !changed {
		return nil, false
	}
	return updated, true
}

func marshalAnthropicTextBlock(text string, cacheControl gjson.Result) ([]byte, error) {
	block := struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control,omitempty"`
	}{Type: "text", Text: text}
	if cacheControl.Exists() {
		block.CacheControl = json.RawMessage(cacheControl.Raw)
	}
	return json.Marshal(block)
}

func anthropicToolUseText(block gjson.Result) string {
	payload := strings.TrimSpace(block.Get("input").Raw)
	if payload == "" {
		payload = "null"
	}
	return "[Tool call: " + block.Get("name").String() + "]\n" + payload
}

func anthropicToolResultText(block gjson.Result) string {
	prefix := "[Tool result: " + block.Get("tool_use_id").String() + "]\n"
	content := block.Get("content")
	switch content.Type {
	case gjson.String:
		return prefix + content.String()
	case gjson.JSON:
		if content.IsArray() {
			parts := make([]string, 0, len(content.Array()))
			for _, raw := range content.Array() {
				if raw.IsObject() && raw.Get("type").String() == "text" {
					parts = append(parts, raw.Get("text").String())
					continue
				}
				parts = append(parts, raw.Raw)
			}
			return prefix + strings.Join(parts, "\n")
		}
		if raw := strings.TrimSpace(content.Raw); raw != "" {
			return prefix + raw
		}
	}
	if raw := strings.TrimSpace(content.Raw); raw != "" {
		return prefix + raw
	}
	return prefix + "null"
}

func rectifyAnthropicThinkingBudget(body []byte) ([]byte, bool) {
	if !isMutableJSONObject(body) {
		return nil, false
	}
	thinking := gjson.GetBytes(body, "thinking")
	if !thinking.IsObject() || strings.EqualFold(thinking.Get("type").String(), "adaptive") {
		return nil, false
	}
	const repairedBudget int64 = 32000
	changed := !strings.EqualFold(thinking.Get("type").String(), "enabled")
	budget := thinking.Get("budget_tokens")
	budgetValue, budgetOK := anthropicRetryInteger(budget)
	if !budgetOK || budgetValue != repairedBudget {
		changed = true
	}
	maxTokens := gjson.GetBytes(body, "max_tokens")
	maxTokensValue, maxTokensOK := anthropicRetryInteger(maxTokens)
	if !maxTokensOK || maxTokensValue <= repairedBudget {
		changed = true
	}
	if !changed {
		return nil, false
	}
	updated, err := sjson.SetBytes(body, "thinking.type", "enabled")
	if err != nil {
		return nil, false
	}
	if !budgetOK || budgetValue != repairedBudget {
		updated, err = sjson.SetBytes(updated, "thinking.budget_tokens", repairedBudget)
		if err != nil {
			return nil, false
		}
	}
	if !maxTokensOK || maxTokensValue <= repairedBudget {
		updated, err = sjson.SetBytes(updated, "max_tokens", int64(64000))
		if err != nil {
			return nil, false
		}
	}
	return updated, true
}

func anthropicRetryInteger(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	number, err := strconv.ParseInt(strings.TrimSpace(value.Raw), 10, 64)
	return number, err == nil
}
