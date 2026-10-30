package app

import (
	"errors"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

// prepareOpenCodeRequestBody applies the Zen/Go inference wire requirements.
// The caller owns endpoint and API-key gating. Keep untouched JSON subtrees
// intact: provider extensions, tool schemas and reasoning payloads are opaque.
func prepareOpenCodeRequestBody(body []byte, upstream protocol.Protocol, path string) ([]byte, error) {
	switch protocol.DetectRequestFamily(path) {
	case protocol.RequestFamilyChatCompletions, protocol.RequestFamilyResponses, protocol.RequestFamilyMessages:
	default:
		return body, nil
	}
	if upstream != protocol.OpenAI && upstream != protocol.Codex && upstream != protocol.Anthropic {
		return body, nil
	}
	root, err := parseOrderedJSON(body)
	if err != nil || root.kind != '{' {
		return nil, errors.New("invalid OpenCode inference request body")
	}
	if upstream == protocol.Codex {
		if _, budget := root.objectMember("max_output_tokens"); budget != nil {
			value := gjson.Parse(budget.raw)
			if value.Type == gjson.Number && value.Num > 0 && value.Num < 16 {
				root.overridePath([]string{"max_output_tokens"}, &orderedJSONNode{raw: "16"})
			}
		}
		return root.render(), nil
	}
	modelName := openCodeBodyString(root, "model")
	vendor := isOpenCodeVendorReasoningModel(modelName)
	if !vendor && (upstream == protocol.Anthropic || !openCodeBodyEnablesReasoning(root)) {
		return root.render(), nil
	}
	_, messages := root.objectMember("messages")
	if messages == nil || messages.kind != '[' {
		return root.render(), nil
	}
	deepSeekChat := upstream == protocol.OpenAI && strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelName)), "deepseek")
	for _, message := range messages.array {
		if message.kind != '{' {
			continue
		}
		role := openCodeBodyString(message, "role")
		// The selected OpenCode endpoint routes DeepSeek Chat to a provider
		// that accepts system instructions but rejects the developer role.
		if deepSeekChat && role == "developer" {
			message.overridePath([]string{"role"}, &orderedJSONNode{raw: `"system"`})
		}
		if role != "assistant" {
			continue
		}
		if upstream == protocol.OpenAI {
			_, calls := message.objectMember("tool_calls")
			if calls == nil || calls.kind != '[' || len(calls.array) == 0 || strings.TrimSpace(openCodeBodyString(message, "reasoning_content")) != "" {
				continue
			}
			reasoning := openCodeBodyString(message, "reasoning")
			if strings.TrimSpace(reasoning) == "" {
				reasoning = "tool call"
			}
			message.overridePath([]string{"reasoning_content"}, &orderedJSONNode{raw: jsonEscapedString(reasoning)})
			continue
		}
		prepareOpenCodeThinkingHistory(message)
	}
	return root.render(), nil
}

// Vendor reasoning models need their thinking history replayed on every
// tool-call turn, signed or not.
func isOpenCodeVendorReasoningModel(name string) bool {
	name = strings.ToLower(name)
	return strings.Contains(name, "deepseek") || strings.Contains(name, "kimi") ||
		strings.Contains(name, "moonshot") || strings.Contains(name, "mimo")
}

// normalizeOpenCodeAnthropicMessagesBody applies the ordinary API-key Messages
// normalization. For vendor reasoning models prepareOpenCodeThinkingHistory owns
// thinking replay, so Claude signature sanitization, which would drop their
// unsigned or foreign-signed history, is skipped.
func normalizeOpenCodeAnthropicMessagesBody(body []byte) ([]byte, error) {
	if !isOpenCodeVendorReasoningModel(gjson.GetBytes(body, "model").String()) {
		return normalizeAnthropicMessagesBody(body)
	}
	if !isAnthropicJSONObject(body) {
		return nil, errors.New("normalize Anthropic request: invalid JSON body")
	}
	return orderAnthropicCacheControlWireShape(normalizeAnthropicMessagesRequest(body)), nil
}

func openCodeBodyString(node *orderedJSONNode, field string) string {
	_, value := node.objectMember(field)
	if value == nil {
		return ""
	}
	parsed := gjson.Parse(value.raw)
	if parsed.Type != gjson.String {
		return ""
	}
	return parsed.Str
}

func openCodeBodyEnablesReasoning(root *orderedJSONNode) bool {
	for _, field := range []string{"reasoning_effort", "reasoning", "thinking", "effort"} {
		_, node := root.objectMember(field)
		if node == nil {
			continue
		}
		value := gjson.Parse(node.raw)
		switch value.Type {
		case gjson.Null, gjson.False:
			continue
		case gjson.String:
			mode := strings.ToLower(strings.TrimSpace(value.Str))
			if mode != "" && mode != "none" && mode != "disabled" {
				return true
			}
		case gjson.JSON:
			if value.IsObject() {
				mode := strings.TrimSpace(openCodeBodyString(node, "type"))
				if mode == "" {
					mode = strings.TrimSpace(openCodeBodyString(node, "effort"))
				}
				mode = strings.ToLower(mode)
				if mode == "none" || mode == "disabled" {
					continue
				}
			}
			return true
		default:
			return true
		}
	}
	return false
}

func prepareOpenCodeThinkingHistory(message *orderedJSONNode) {
	_, content := message.objectMember("content")
	if content == nil || content.kind != '[' {
		return
	}
	hasTool := false
	for _, block := range content.array {
		if openCodeBodyString(block, "type") == "tool_use" {
			hasTool = true
			break
		}
	}
	if !hasTool {
		return
	}
	hasThinking := false
	for index, block := range content.array {
		switch openCodeBodyString(block, "type") {
		case "thinking":
			hasThinking = true
			block.removePath([]string{"signature"})
			if strings.TrimSpace(openCodeBodyString(block, "thinking")) == "" {
				block.overridePath([]string{"thinking"}, &orderedJSONNode{raw: `"tool call"`})
			}
		case "redacted_thinking":
			hasThinking = true
			content.array[index] = &orderedJSONNode{raw: `{"type":"thinking","thinking":"[redacted thinking]"}`, dirty: true}
		}
	}
	if !hasThinking {
		content.array = append([]*orderedJSONNode{{raw: `{"type":"thinking","thinking":"tool call"}`}}, content.array...)
		content.dirty = true
	}
}
