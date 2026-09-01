package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	cliproxycommon "ccLoad/internal/protocol/cliproxy/common"
	sigcompat "ccLoad/internal/protocol/cliproxy/signature"

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
		updated, changed := replaceAntigravityThoughtSignatures(request)
		if !changed {
			return nil, "", false
		}
		return updated, "replace_antigravity_thought_signatures", true
	}
	if updated, changed := stripAntigravityThinkingHistory(request); changed {
		return updated, "strip_antigravity_thinking", true
	}
	if updated, changed := stripAntigravityToolHistory(request); changed {
		return updated, "strip_antigravity_tools", true
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

func unwrapAntigravityRetryRequest(body []byte) ([]byte, string, bool) {
	if !sonic.Valid(body) {
		return nil, "", false
	}
	payload := gjson.ParseBytes(body)
	if !payload.IsObject() {
		return nil, "", false
	}
	modelName := payload.Get("model").String()
	if request := payload.Get("request"); request.IsObject() {
		return []byte(request.Raw), modelName, true
	}
	return body, modelName, true
}

// replaceAntigravityThoughtSignatures 把任意深度的 thoughtSignature 换成跳过校验的
// 哨兵值。thoughtSignature 可以出现在 parts 之外的嵌套位置，所以这里递归重写而不是
// 按固定路径改。
func replaceAntigravityThoughtSignatures(body []byte) ([]byte, bool) {
	if !gjson.ValidBytes(body) {
		return nil, false
	}
	sentinel := jsonEscapedString(sigcompat.GeminiSkipThoughtSignatureValidator)
	updated, changed := rewriteJSONMembers(body, func(key string, value gjson.Result) (string, bool) {
		if key != "thoughtSignature" || value.Raw == sentinel {
			return "", false
		}
		return sentinel, true
	})
	if !changed {
		return nil, false
	}
	return updated, true
}

func stripAntigravityThinkingHistory(body []byte) ([]byte, bool) {
	updated := body
	changed := false
	if gjson.GetBytes(updated, "generationConfig.thinkingConfig").Exists() {
		var err error
		updated, err = sjson.DeleteBytes(updated, "generationConfig.thinkingConfig")
		if err != nil {
			return nil, false
		}
		changed = true
	}
	contents := gjson.GetBytes(updated, "contents")
	if !contents.IsArray() {
		return updated, changed
	}
	for contentIndex := len(contents.Array()) - 1; contentIndex >= 0; contentIndex-- {
		partsPath := fmt.Sprintf("contents.%d.parts", contentIndex)
		parts := gjson.GetBytes(updated, partsPath)
		if !parts.IsArray() {
			continue
		}
		rendered := make([][]byte, 0, len(parts.Array()))
		contentChanged := false
		for _, part := range parts.Array() {
			if !part.IsObject() {
				rendered = append(rendered, []byte(part.Raw))
				continue
			}
			if part.Get("thought").Type == gjson.True {
				textValue := ""
				if rawText := part.Get("text"); rawText.Type == gjson.String {
					textValue = rawText.String()
				}
				contentChanged = true
				if textValue != "" {
					replacement, err := sjson.SetBytes([]byte(`{}`), "text", textValue)
					if err != nil {
						return nil, false
					}
					rendered = append(rendered, replacement)
				}
				continue
			}
			if part.Get("thoughtSignature").Exists() && !part.Get("functionCall").Exists() {
				updatedPart, err := sjson.DeleteBytes([]byte(part.Raw), "thoughtSignature")
				if err != nil {
					return nil, false
				}
				rendered = append(rendered, updatedPart)
				contentChanged = true
				continue
			}
			rendered = append(rendered, []byte(part.Raw))
		}
		if !contentChanged {
			continue
		}
		changed = true
		var err error
		updated, err = sjson.SetRawBytes(updated, partsPath, cliproxycommon.JoinRawArray(rendered))
		if err != nil {
			return nil, false
		}
	}
	return updated, changed
}

func stripAntigravityToolHistory(body []byte) ([]byte, bool) {
	updated := body
	changed := false
	contents := gjson.GetBytes(updated, "contents")
	if !contents.IsArray() {
		return updated, false
	}
	for contentIndex, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		rendered := make([][]byte, 0, len(parts.Array()))
		contentChanged := false
		for _, part := range parts.Array() {
			if !part.IsObject() {
				rendered = append(rendered, []byte(part.Raw))
				continue
			}
			var kind string
			var value gjson.Result
			switch {
			case part.Get("functionCall").Type != gjson.Null && part.Get("functionCall").Exists():
				kind, value = "tool_use", part.Get("functionCall")
			case part.Get("functionResponse").Type != gjson.Null && part.Get("functionResponse").Exists():
				kind, value = "tool_result", part.Get("functionResponse")
			case part.Get("thoughtSignature").Type != gjson.Null && part.Get("thoughtSignature").Exists():
				updatedPart, err := sjson.DeleteBytes([]byte(part.Raw), "thoughtSignature")
				if err != nil {
					return nil, false
				}
				rendered = append(rendered, updatedPart)
				contentChanged = true
				continue
			default:
				rendered = append(rendered, []byte(part.Raw))
				continue
			}
			replacement, err := sjson.SetBytes([]byte(`{}`), "text", antigravityToolHistoryText(kind, value.Raw))
			if err != nil {
				return nil, false
			}
			rendered = append(rendered, replacement)
			contentChanged = true
		}
		if !contentChanged {
			continue
		}
		partsPath := fmt.Sprintf("contents.%d.parts", contentIndex)
		var err error
		updated, err = sjson.SetRawBytes(updated, partsPath, cliproxycommon.JoinRawArray(rendered))
		if err != nil {
			return nil, false
		}
		changed = true
	}
	return updated, changed
}

func antigravityToolHistoryText(kind, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "(" + kind + ")"
	}
	if !json.Valid([]byte(raw)) {
		return "(" + kind + ")"
	}
	return "(" + kind + ") " + raw
}
