package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	cliproxysignature "ccLoad/internal/protocol/cliproxy/signature"
)

// normalizeAnthropicMessagesBody is the single native Claude body boundary.
// Protocol conversion produces the shape; this function only enforces Anthropic
// wire invariants shared by API-key and OAuth attempts.
func normalizeAnthropicMessagesBody(body []byte) ([]byte, error) {
	request, err := decodeAnthropicRequest(body)
	if err != nil {
		return nil, errors.New("normalize Anthropic request: invalid JSON body")
	}
	return encodeNormalizedAnthropicRequest(request)
}

// encodeNormalizedAnthropicRequest 收尾 Anthropic Messages body 并重签 CCH。
// CCH 无条件重签：签名值嵌在 body 自己的 billing header 里，finalizeAnthropicCCH
// 对没有 billing header 的 body 是 no-op，所以「签不签」不需要第二个谓词——一旦
// 有条件跳过，就会出现 body 改了而 cch 还是旧值的静默错签。
func encodeNormalizedAnthropicRequest(request map[string]any) ([]byte, error) {
	normalizeAnthropicMessagesRequest(request)
	orderAnthropicCacheControlWireShape(request)
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("normalize Anthropic request: encode body")
	}
	encoded, _ = cliproxysignature.SanitizeClaudeMessagesForClaudeUpstream(encoded, stringValue(request["model"]))
	encoded, err = finalizeAnthropicCCH(encoded)
	if err != nil {
		return nil, errors.New("normalize Anthropic request: sign Claude CCH")
	}
	return encoded, nil
}

func decodeAnthropicRequest(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request map[string]any
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("missing JSON object")
	}
	return request, nil
}

func normalizeAnthropicMessagesRequest(request map[string]any) {
	normalizeAnthropicToolChoice(request)
	normalizeAnthropicThinking(request)
	normalizeAnthropicSampling(request)
	sanitizeAnthropicOAuthMessages(request)
	if countAnthropicCacheControls(request) == 0 {
		ensureAnthropicCacheControls(request)
	}
	normalizeAnthropicCacheControlTTL(request)
	enforceAnthropicCacheControlLimit(request, 4)
}

func validateAnthropicLegacySystemMessages(request map[string]any) error {
	if !anthropicUsesLegacySystemReminder(stringValue(request["model"])) {
		return nil
	}
	messages, ok := request["messages"].([]any)
	if !ok {
		return nil
	}
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), "system") {
			return &anthropicRequestValidationError{message: fmt.Sprintf(
				"Anthropic model %q does not support system messages in messages[%d]", stringValue(request["model"]), index,
			)}
		}
	}
	return nil
}

type anthropicRequestValidationError struct{ message string }

func (e *anthropicRequestValidationError) Error() string { return e.message }

func countAnthropicCacheControls(request map[string]any) int {
	count := 0
	visitAnthropicCacheBlocks(request, func(block map[string]any) {
		if _, exists := block["cache_control"]; exists {
			count++
		}
	})
	return count
}

func normalizeAnthropicToolChoice(request map[string]any) {
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		delete(request, "tool_choice")
	}
	choice, _ := request["tool_choice"].(map[string]any)
	choiceType := strings.ToLower(strings.TrimSpace(stringValue(choice["type"])))
	if choiceType != "any" && choiceType != "tool" {
		return
	}
	delete(request, "thinking")
	deleteAnthropicOutputEffort(request)
}

func normalizeAnthropicThinking(request map[string]any) {
	thinking, ok := request["thinking"].(map[string]any)
	if !ok {
		return
	}
	typ := strings.ToLower(strings.TrimSpace(stringValue(thinking["type"])))
	switch typ {
	case "auto":
		thinking["type"] = "adaptive"
		typ = "adaptive"
	case "disabled", "off", "none":
		delete(request, "thinking")
		deleteAnthropicOutputEffort(request)
		return
	}
	if typ != "adaptive" {
		return
	}
	if budget, ok := anthropicInteger(thinking["budget_tokens"]); ok && budget > 0 {
		setAnthropicOutputEffort(request, anthropicBudgetToEffort(int(budget)))
	}
	delete(thinking, "budget_tokens")
}

func deleteAnthropicOutputEffort(request map[string]any) {
	outputConfig, ok := request["output_config"].(map[string]any)
	if !ok {
		return
	}
	delete(outputConfig, "effort")
	if len(outputConfig) == 0 {
		delete(request, "output_config")
	}
}

func normalizeAnthropicSampling(request map[string]any) {
	// Claude Code does not forward caller sampling knobs. Keeping both temperature
	// and top_p is invalid, and thinking requests reject top_k as well.
	delete(request, "temperature")
	delete(request, "top_p")
	thinking, _ := request["thinking"].(map[string]any)
	switch strings.ToLower(strings.TrimSpace(stringValue(thinking["type"]))) {
	case "enabled", "adaptive", "auto":
		delete(request, "top_k")
	}
}

func ensureAnthropicCacheControls(request map[string]any) {
	ensureAnthropicToolCacheControl(request)
	ensureAnthropicSystemCacheControl(request)
	ensureAnthropicMessageCacheControl(request)
}

func ensureAnthropicToolCacheControl(request map[string]any) {
	tools, ok := request["tools"].([]any)
	if !ok {
		return
	}
	var lastEligible map[string]any
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := tool["cache_control"]; exists {
			return
		}
		deferred, _ := tool["defer_loading"].(bool)
		if !deferred {
			lastEligible = tool
		}
	}
	if lastEligible != nil {
		lastEligible["cache_control"] = anthropicEphemeralCacheControl()
	}
}

func ensureAnthropicSystemCacheControl(request map[string]any) {
	system, exists := request["system"]
	if !exists {
		return
	}
	switch value := system.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			request["system"] = []any{map[string]any{
				"type": "text", "text": value, "cache_control": anthropicEphemeralCacheControl(),
			}}
		}
	case []any:
		var last map[string]any
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := block["cache_control"]; exists {
				return
			}
			last = block
		}
		if last != nil {
			last["cache_control"] = anthropicEphemeralCacheControl()
		}
	}
}

func ensureAnthropicMessageCacheControl(request map[string]any) {
	messages, ok := request["messages"].([]any)
	if !ok {
		return
	}
	userIndexes := make([]int, 0, 2)
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := message["content"].([]any); ok {
			for _, rawBlock := range content {
				if block, ok := rawBlock.(map[string]any); ok {
					if _, exists := block["cache_control"]; exists {
						return
					}
				}
			}
		}
		if stringValue(message["role"]) == "user" {
			userIndexes = append(userIndexes, index)
		}
	}
	if len(userIndexes) < 2 {
		return
	}
	message, _ := messages[userIndexes[len(userIndexes)-2]].(map[string]any)
	switch content := message["content"].(type) {
	case string:
		message["content"] = []any{map[string]any{
			"type": "text", "text": content, "cache_control": anthropicEphemeralCacheControl(),
		}}
	case []any:
		for index := len(content) - 1; index >= 0; index-- {
			if block, ok := content[index].(map[string]any); ok {
				block["cache_control"] = anthropicEphemeralCacheControl()
				return
			}
		}
	}
}

func anthropicEphemeralCacheControl() map[string]any {
	return map[string]any{"type": "ephemeral"}
}

func normalizeAnthropicCacheControlTTL(request map[string]any) {
	seenFiveMinutes := false
	process := func(block map[string]any) {
		cache, ok := block["cache_control"].(map[string]any)
		if !ok {
			if _, exists := block["cache_control"]; exists {
				seenFiveMinutes = true
			}
			return
		}
		if stringValue(cache["ttl"]) != "1h" {
			seenFiveMinutes = true
			return
		}
		if seenFiveMinutes {
			delete(cache, "ttl")
		}
	}
	visitAnthropicCacheBlocks(request, process)
}

// visitAnthropicCacheBlocks follows Anthropic's evaluation order.
func visitAnthropicCacheBlocks(request map[string]any, visit func(map[string]any)) {
	if tools, ok := request["tools"].([]any); ok {
		for _, raw := range tools {
			if block, ok := raw.(map[string]any); ok {
				visit(block)
			}
		}
	}
	if system, ok := request["system"].([]any); ok {
		for _, raw := range system {
			if block, ok := raw.(map[string]any); ok {
				visit(block)
			}
		}
	}
	if messages, ok := request["messages"].([]any); ok {
		for _, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue
			}
			if content, ok := message["content"].([]any); ok {
				for _, raw := range content {
					if block, ok := raw.(map[string]any); ok {
						visit(block)
					}
				}
			}
		}
	}
}

func anthropicInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		result, err := number.Int64()
		return result, err == nil
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		return int64(number), number == float64(int64(number))
	default:
		return 0, false
	}
}
