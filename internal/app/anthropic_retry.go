package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
	if path, message, toolError := rejectedAnthropicToolPath(plan.TranslatedBody, res.Body); toolError {
		if body, ok := downgradeRejectedAnthropicTool(plan.TranslatedBody, path, message); ok {
			return body, "downgrade_anthropic_tools", true
		}
		// A located tool rejection must not fall through to a global thinking
		// downgrade just because an argument is named "thinking".
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
	return nil, "", false
}

var anthropicToolErrorPath = regexp.MustCompile(`^(tools\.[0-9]+|messages\.[0-9]+\.content\.[0-9]+)(?:\.[A-Za-z0-9_]+)*$`)
var anthropicErrorPathIndex = regexp.MustCompile(`\[([0-9]+)\]`)

// Only explicit validation locations can select a tool. Never guess from a
// tool name mentioned in prose or from a missing/misordered tool_result error.
func rejectedAnthropicToolPath(body, errorBody []byte) (string, string, bool) {
	if !gjson.ValidBytes(errorBody) {
		return "", "", false
	}
	message := gjson.GetBytes(errorBody, "error.message").String()
	param := strings.TrimSpace(gjson.GetBytes(errorBody, "error.param").String())
	prefix, _, _ := strings.Cut(message, ":")
	parse := func(path string) string {
		path = anthropicErrorPathIndex.ReplaceAllString(strings.TrimSpace(path), ".$1")
		if match := anthropicToolErrorPath.FindStringSubmatch(path); len(match) > 0 {
			return match[1]
		}
		return ""
	}
	isTool := func(raw, path string) bool {
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "tools.") || strings.HasPrefix(raw, "tools[") {
			return true
		}
		if (strings.HasPrefix(raw, "messages.") || strings.HasPrefix(raw, "messages[")) &&
			(path == "" || !gjson.GetBytes(body, path).Exists()) {
			return true
		}
		kind := gjson.GetBytes(body, path+".type").String()
		return path != "" && (kind == "tool_use" || kind == "tool_result")
	}
	path, paramPath := parse(prefix), parse(param)
	toolError := isTool(prefix, path) || isTool(param, paramPath)
	if param != "" {
		if paramPath == "" || (path != "" && path != paramPath) {
			return "", message, toolError
		}
		path = paramPath
	}
	return path, message, toolError
}

// statesAnthropicBlockUnsupported 判断上游是否在说「这个 block 类型本身不被支持」。
//
// 判据是语义而非整句相等：真正拒绝 tool block 的多为第三方兼容网关，措辞各不
// 相同（"blocks are not supported" / "block type is unsupported" / "does not
// support tool_use" 等），写死整句等于让降级永不生效。
//
// 但「不支持该 block」必须与「该 block 内容有问题」区分开——后者要的是修正而
// 不是文本化，降级只会把错误藏起来并丢掉工具语义。所以先按否定词排除：入参非法、
// schema 错误、配对/顺序缺失，都不构成类型级不兼容。
func statesAnthropicBlockUnsupported(message, blockType string) bool {
	reason := strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(reason, blockType) {
		return false
	}
	for _, repairable := range []string{
		"invalid", "schema", "without", "missing", "expected", "must be", "must have",
		"immediately after", "does not match", "mismatch", "duplicate", "unknown field",
	} {
		if strings.Contains(reason, repairable) {
			return false
		}
	}
	return strings.Contains(reason, "not supported") || strings.Contains(reason, "unsupported") ||
		strings.Contains(reason, "not allowed") || strings.Contains(reason, "does not support") ||
		strings.Contains(reason, "cannot be used")
}

func downgradeRejectedAnthropicTool(body []byte, path, message string) ([]byte, bool) {
	if path == "" || !isMutableJSONObject(body) {
		return nil, false
	}
	target := gjson.GetBytes(body, path)
	if !target.IsObject() {
		return nil, false
	}
	toolName := ""
	ids := make(map[string]bool)
	if strings.HasPrefix(path, "tools.") {
		toolName = target.Get("name").String()
		if toolName == "" {
			return nil, false
		}
		matches := 0
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			if tool.Get("name").String() == toolName {
				matches++
			}
		}
		if matches != 1 {
			return nil, false
		}
	} else {
		// Historical blocks are lossy to downgrade. Accept explicit block
		// incompatibility only; invalid inputs, pairing and ordering need repair.
		blockType := target.Get("type").String()
		if blockType != "tool_use" && blockType != "tool_result" {
			return nil, false
		}
		if !statesAnthropicBlockUnsupported(message, blockType) {
			return nil, false
		}
		idField := "id"
		if blockType == "tool_result" {
			idField = "tool_use_id"
		}
		ids[target.Get(idField).String()] = true
	}

	messages := gjson.GetBytes(body, "messages").Array()
	for _, msg := range messages {
		for _, block := range msg.Get("content").Array() {
			if toolName != "" && block.Get("type").String() == "tool_use" && block.Get("name").String() == toolName {
				ids[block.Get("id").String()] = true
			}
		}
	}
	// A selected ID must identify exactly one complete adjacent call/result pair.
	// Duplicate IDs and incomplete history cannot be safely downgraded.
	if ids[""] {
		return nil, false
	}
	type pair struct{ uses, results, useIndex, resultIndex int }
	pairs := make(map[string]pair, len(ids))
	for i, msg := range messages {
		for _, block := range msg.Get("content").Array() {
			id := block.Get("id").String()
			if block.Get("type").String() == "tool_use" && ids[id] {
				if msg.Get("role").String() != "assistant" {
					return nil, false
				}
				p := pairs[id]
				p.uses++
				p.useIndex = i
				pairs[id] = p
			}
			id = block.Get("tool_use_id").String()
			if block.Get("type").String() == "tool_result" && ids[id] {
				if msg.Get("role").String() != "user" {
					return nil, false
				}
				p := pairs[id]
				p.results++
				p.resultIndex = i
				pairs[id] = p
			}
		}
	}
	for id := range ids {
		p := pairs[id]
		if p.uses != 1 || p.results != 1 || p.resultIndex != p.useIndex+1 {
			return nil, false
		}
	}
	updated := body
	for i, msg := range messages {
		var results, other [][]byte
		changed := false
		for _, block := range msg.Get("content").Array() {
			kind := block.Get("type").String()
			if (kind == "tool_use" && ids[block.Get("id").String()]) || (kind == "tool_result" && ids[block.Get("tool_use_id").String()]) {
				parts, ok := anthropicToolHistoryText(block)
				if !ok {
					return nil, false
				}
				other = append(other, parts...)
				changed = true
			} else if kind == "tool_result" {
				results = append(results, []byte(block.Raw))
			} else {
				other = append(other, []byte(block.Raw))
			}
		}
		if changed {
			var err error
			updated, err = sjson.SetRawBytes(updated, fmt.Sprintf("messages.%d.content", i), cliproxycommon.JoinRawArray(append(results, other...)))
			if err != nil {
				return nil, false
			}
		}
	}
	if toolName != "" {
		var err error
		updated, err = sjson.DeleteBytes(updated, path)
		if err != nil {
			return nil, false
		}
		if gjson.GetBytes(updated, "tools.#").Int() == 0 {
			updated, err = sjson.DeleteBytes(updated, "tools")
			if err != nil {
				return nil, false
			}
		}
		if !gjson.GetBytes(updated, "tools").Exists() || gjson.GetBytes(updated, "tool_choice.name").String() == toolName {
			updated, err = sjson.DeleteBytes(updated, "tool_choice")
			if err != nil {
				return nil, false
			}
		}
	}
	// Moving remaining results ahead of converted text must not put a 1h
	// breakpoint after a 5m breakpoint. Refuse instead of silently lowering TTL.
	seenShort, invalidCacheOrder := false, false
	forEachAnthropicCacheBlock(updated, func(_ string, block gjson.Result) bool {
		cache := block.Get("cache_control")
		if cache.Exists() {
			if cache.Get("ttl").String() == "1h" {
				invalidCacheOrder = seenShort
			} else {
				seenShort = true
			}
		}
		return !invalidCacheOrder
	})
	if invalidCacheOrder {
		return nil, false
	}
	return updated, true
}

func anthropicToolHistoryText(block gjson.Result) ([][]byte, bool) {
	// 工具名和入参是模型续写所需的上下文；id 和 cache_control 是线协议字段，
	// 放进正文只会污染 prompt——cache_control 还会随后被写回块字段，重复两次。
	input := strings.TrimSpace(block.Get("input").Raw)
	if input == "" {
		input = "null"
	}
	text := "[Tool call: " + block.Get("name").String() + "]\n" + input
	var content []gjson.Result
	if block.Get("type").String() == "tool_result" {
		text = "[Tool result: " + block.Get("tool_use_id").String() + "]"
		if block.Get("is_error").Bool() {
			text += " (error)"
		}
		value := block.Get("content")
		switch {
		case value.Type == gjson.String:
			text += "\n" + value.String()
		case value.IsArray():
			content = value.Array()
		case value.Exists() && value.Type != gjson.Null:
			return nil, false
		}
	}
	encoded, err := marshalAnthropicTextBlock(text, gjson.Result{})
	if err != nil {
		return nil, false
	}
	parts := [][]byte{encoded}
	for _, item := range content {
		switch item.Get("type").String() {
		case "text", "image", "document":
			parts = append(parts, []byte(item.Raw))
		default:
			return nil, false
		}
	}
	if cache := block.Get("cache_control"); cache.Exists() {
		// The original breakpoint followed the entire result, including images.
		// Preserve that boundary when expanding a result into ordinary content.
		last := len(parts) - 1
		if existing := gjson.GetBytes(parts[last], "cache_control"); existing.Exists() && existing.Raw != cache.Raw {
			return nil, false
		}
		parts[last], err = sjson.SetRawBytes(parts[last], "cache_control", []byte(cache.Raw))
		if err != nil {
			return nil, false
		}
	}
	return parts, true
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
