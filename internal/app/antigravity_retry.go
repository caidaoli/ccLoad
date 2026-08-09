package app

import (
	"encoding/json"
	"net/http"
	"strings"

	sigcompat "ccLoad/internal/protocol/cliproxy/signature"
)

func antigravitySignatureRetryBody(planBody []byte, responseBody []byte, statusCode int) ([]byte, string, bool) {
	if statusCode != http.StatusBadRequest || !isAntigravitySignatureError(responseBody) {
		return nil, "", false
	}
	request, modelName, ok := unwrapAntigravityRetryRequest(planBody)
	if !ok {
		return nil, "", false
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelName)), "gemini-") {
		if !replaceAntigravityThoughtSignatures(request) {
			return nil, "", false
		}
		return marshalAntigravityRetryRequest(request, "replace_antigravity_thought_signatures")
	}
	if stripAntigravityThinkingHistory(request) {
		return marshalAntigravityRetryRequest(request, "strip_antigravity_thinking")
	}
	if stripAntigravityToolHistory(request) {
		return marshalAntigravityRetryRequest(request, "strip_antigravity_tools")
	}
	return nil, "", false
}

func isAntigravitySignatureError(body []byte) bool {
	message := strings.ToLower(string(body))
	if strings.Contains(message, "thought_signature") || strings.Contains(message, "thoughtsignature") ||
		strings.Contains(message, "signature") {
		return true
	}
	return strings.Contains(message, "expected") &&
		(strings.Contains(message, "thinking") || strings.Contains(message, "redacted_thinking"))
}

func unwrapAntigravityRetryRequest(body []byte) (map[string]any, string, bool) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, "", false
	}
	modelName, _ := payload["model"].(string)
	if request, ok := payload["request"].(map[string]any); ok {
		return request, modelName, true
	}
	return payload, modelName, true
}

func marshalAntigravityRetryRequest(request map[string]any, strategy string) ([]byte, string, bool) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, "", false
	}
	return raw, strategy, true
}

func replaceAntigravityThoughtSignatures(value any) bool {
	changed := false
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if key == "thoughtSignature" {
				if signature, _ := child.(string); signature != sigcompat.GeminiSkipThoughtSignatureValidator {
					item[key] = sigcompat.GeminiSkipThoughtSignatureValidator
					changed = true
				}
				continue
			}
			changed = replaceAntigravityThoughtSignatures(child) || changed
		}
	case []any:
		for _, child := range item {
			changed = replaceAntigravityThoughtSignatures(child) || changed
		}
	}
	return changed
}

func stripAntigravityThinkingHistory(request map[string]any) bool {
	changed := false
	if generationConfig, _ := request["generationConfig"].(map[string]any); generationConfig != nil {
		if _, exists := generationConfig["thinkingConfig"]; exists {
			delete(generationConfig, "thinkingConfig")
			changed = true
		}
	}
	contents, _ := request["contents"].([]any)
	for _, rawContent := range contents {
		content, _ := rawContent.(map[string]any)
		parts, _ := content["parts"].([]any)
		filtered := make([]any, 0, len(parts))
		partsChanged := false
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part == nil {
				filtered = append(filtered, rawPart)
				continue
			}
			thought, _ := part["thought"].(bool)
			_, hasSignature := part["thoughtSignature"]
			_, hasFunctionCall := part["functionCall"]
			if thought {
				if text, _ := part["text"].(string); text != "" {
					filtered = append(filtered, map[string]any{"text": text})
				}
				partsChanged = true
				continue
			}
			if hasSignature && !hasFunctionCall {
				delete(part, "thoughtSignature")
				partsChanged = true
			}
			filtered = append(filtered, part)
		}
		if partsChanged {
			content["parts"] = filtered
			changed = true
		}
	}
	return changed
}

func stripAntigravityToolHistory(request map[string]any) bool {
	changed := false
	contents, _ := request["contents"].([]any)
	for _, rawContent := range contents {
		content, _ := rawContent.(map[string]any)
		parts, _ := content["parts"].([]any)
		for index, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part == nil {
				continue
			}
			switch {
			case part["functionCall"] != nil:
				parts[index] = map[string]any{"text": antigravityToolHistoryText("tool_use", part["functionCall"])}
				changed = true
			case part["functionResponse"] != nil:
				parts[index] = map[string]any{"text": antigravityToolHistoryText("tool_result", part["functionResponse"])}
				changed = true
			case part["thoughtSignature"] != nil:
				delete(part, "thoughtSignature")
				changed = true
			}
		}
	}
	return changed
}

func antigravityToolHistoryText(kind string, value any) string {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 {
		return "(" + kind + ")"
	}
	return "(" + kind + ") " + string(raw)
}
