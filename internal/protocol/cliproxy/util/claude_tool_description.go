package util

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EnsureClaudeToolDescription puts a `description` field on an Anthropic tool
// object when the source omitted it. Official Anthropic is lenient; Grok's CLI
// proxy serde-requires the key. Prefer the tool name so the model still has a label.
func EnsureClaudeToolDescription(tool []byte) []byte {
	if !gjson.ValidBytes(tool) || !gjson.ParseBytes(tool).IsObject() {
		return tool
	}
	if gjson.GetBytes(tool, "description").Exists() {
		return tool
	}
	desc := strings.TrimSpace(gjson.GetBytes(tool, "name").String())
	if desc == "" {
		desc = strings.TrimSpace(gjson.GetBytes(tool, "type").String())
	}
	updated, err := sjson.SetBytes(tool, "description", desc)
	if err != nil {
		return tool
	}
	return updated
}

// EnsureClaudeToolsDescriptions fills missing tools[].description on a Claude
// Messages body. Reports whether any field was added.
func EnsureClaudeToolsDescriptions(body []byte) ([]byte, bool) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, false
	}
	changed := false
	for i, tool := range tools.Array() {
		if !tool.IsObject() || tool.Get("description").Exists() {
			continue
		}
		desc := strings.TrimSpace(tool.Get("name").String())
		if desc == "" {
			desc = strings.TrimSpace(tool.Get("type").String())
		}
		updated, err := sjson.SetBytes(body, fmt.Sprintf("tools.%d.description", i), desc)
		if err != nil {
			continue
		}
		body = updated
		changed = true
	}
	return body, changed
}
