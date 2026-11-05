package protocol

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AnthropicEffortDirective returns the latest system-message effort. Claude
// clients can retain a cached top-level effort while changing it mid-conversation.
func AnthropicEffortDirective(body []byte) string {
	root := gjson.ParseBytes(body)
	switch strings.ToLower(strings.TrimSpace(root.Get("thinking.type").String())) {
	case "disabled", "between_tools":
		return ""
	}
	var effort string
	messages := root.Get("messages")
	if !messages.IsArray() {
		return ""
	}
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
			if value := message.Get("output_config.effort"); value.Type == gjson.String {
				if value := strings.ToLower(strings.TrimSpace(value.String())); value != "" {
					effort = value
				}
			}
		}
		return true
	})
	return effort
}

// NormalizeAnthropicEffortForTranslation projects the current effort in a validated
// request onto fields understood by cross-protocol converters. Native Anthropic
// requests keep their original cache prefix and per-turn directives.
func NormalizeAnthropicEffortForTranslation(body []byte) ([]byte, error) {
	effort := AnthropicEffortDirective(body)
	if effort == "" {
		return body, nil
	}
	thinkingType := "adaptive"
	if effort == "none" {
		// Every converter recognizes a zero budget, including Gemini providers
		// whose default thinking would remain enabled if the config were omitted.
		thinkingType = "enabled"
	}
	out, err := sjson.SetBytes(body, "thinking.type", thinkingType)
	if err != nil {
		return nil, err
	}
	if effort == "none" {
		out, err = sjson.SetBytes(out, "thinking.budget_tokens", 0)
	} else {
		out, err = sjson.DeleteBytes(out, "thinking.budget_tokens")
	}
	if err != nil {
		return nil, err
	}
	if effort == "none" || effort == "auto" {
		return sjson.DeleteBytes(out, "output_config.effort")
	}
	return sjson.SetBytes(out, "output_config.effort", effort)
}
