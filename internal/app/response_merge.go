package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
)

type mergedResponseParts struct {
	Reasoning string `json:"reasoning"`
	Content   string `json:"content"`
	Tools     string `json:"tools,omitempty"`
}

type mergedResponseBuilder struct {
	reasoning          strings.Builder
	content            strings.Builder
	toolCalls          []mergedToolCall
	toolCallIndexes    map[string]int
	toolDelta          strings.Builder
	toolDeltaName      string
	toolDeltaKey       string
	toolNamesByIndex   map[string]string
	openAIToolKeys     map[string]string
	streamState        chatFrontendStreamState
	lastContentItemKey string
}

type mergedToolCall struct {
	key   string
	name  string
	value string
}

const codexMessageItemSeparator = "\n\n---\n\n"

func mergeResponseBody(raw string) mergedResponseParts {
	body := stripHTTPResponseEnvelope(strings.ReplaceAll(raw, "\r\n", "\n"))
	if strings.TrimSpace(body) == "" {
		return mergedResponseParts{}
	}

	builder := &mergedResponseBuilder{}
	payloads := parseSSEJSONPayloads(body)
	if len(payloads) > 0 {
		for _, payload := range payloads {
			builder.collectPayload(payload)
		}
		if parts := builder.parts(); hasMergedParts(parts) {
			return parts
		}
		return mergedResponseParts{Content: formatJSONForMergedContent(body)}
	}

	var obj map[string]any
	if err := sonic.Unmarshal([]byte(strings.TrimSpace(body)), &obj); err == nil {
		builder.collectPayload(obj)
		if parts := builder.parts(); hasMergedParts(parts) {
			return parts
		}
	}

	return mergedResponseParts{Content: formatJSONForMergedContent(body)}
}

func stripHTTPResponseEnvelope(raw string) string {
	headerBreak := strings.Index(raw, "\n\n")
	if headerBreak < 0 {
		return strings.TrimSpace(raw)
	}
	firstLine := strings.TrimSpace(strings.Split(raw, "\n")[0])
	if !strings.HasPrefix(strings.ToUpper(firstLine), "HTTP ") {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSpace(raw[headerBreak+2:])
}

func parseSSEJSONPayloads(body string) []map[string]any {
	payloads := make([]map[string]any, 0)
	dataLines := make([]string, 0, 1)

	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		raw := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if raw == "" || raw == "[DONE]" {
			return
		}
		var obj map[string]any
		if err := sonic.Unmarshal([]byte(raw), &obj); err == nil {
			payloads = append(payloads, obj)
		}
	}

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			dataLines = append(dataLines, value)
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
		}
	}
	flush()
	return payloads
}

func (b *mergedResponseBuilder) collectPayload(obj map[string]any) {
	if obj == nil {
		return
	}
	if _, ok := obj["candidates"].([]any); ok {
		b.collectGeminiContent(obj)
		return
	}
	if thinking := extractSSEThinkingDelta(obj); thinking != "" {
		b.appendReasoning(thinking)
	}
	if b.appendCodexTextDelta(obj) {
		// Codex text deltas carry item identity; keep item boundaries visible.
	} else if delta := extractSSEDeltaText(obj); delta != "" {
		b.appendTextDelta(delta)
	}
	b.collectOpenAIMessage(obj)
	b.collectGeminiContent(obj)
	b.collectCodexPayload(obj)
	b.collectAnthropicPayload(obj)
}

func (b *mergedResponseBuilder) collectOpenAIMessage(obj map[string]any) {
	choices, ok := obj["choices"].([]any)
	if !ok {
		return
	}
	for _, choiceValue := range choices {
		choice, ok := choiceValue.(map[string]any)
		if !ok {
			continue
		}
		choiceIndex := indexKeyFromAny(choice["index"])
		if delta, ok := choice["delta"].(map[string]any); ok {
			b.collectToolCalls(delta["tool_calls"], true, choiceIndex)
		}
		if message, ok := choice["message"].(map[string]any); ok {
			b.appendReasoningString(message["reasoning_content"])
			b.appendReasoningString(message["reasoning"])
			b.appendContentValue(message["content"])
			b.collectToolCalls(message["tool_calls"], false, choiceIndex)
		}
	}
}

func (b *mergedResponseBuilder) collectGeminiContent(obj map[string]any) {
	candidates, ok := obj["candidates"].([]any)
	if !ok {
		return
	}
	for _, candidateValue := range candidates {
		candidate, ok := candidateValue.(map[string]any)
		if !ok {
			continue
		}
		content, ok := candidate["content"].(map[string]any)
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}
		for _, partValue := range parts {
			part, ok := partValue.(map[string]any)
			if !ok {
				continue
			}
			targetThinking, _ := part["thought"].(bool)
			if targetThinking {
				b.appendReasoningString(part["text"])
			} else {
				b.appendContentValue(part["text"])
			}
		}
	}
}

func (b *mergedResponseBuilder) collectCodexPayload(obj map[string]any) {
	typ, _ := obj["type"].(string)
	switch typ {
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		index := indexKeyFromAny(obj["output_index"])
		b.appendToolDelta(toolKeyFromPayload(obj), b.toolNameForIndex(index, "tool_call"), obj["delta"])
	case "response.function_call_arguments.done":
		index := indexKeyFromAny(obj["output_index"])
		b.appendToolCall(toolKeyFromPayload(obj), b.toolNameForIndex(index, "tool_call"), obj["arguments"])
	case "response.output_item.added":
		b.rememberCodexToolName(obj)
	case "response.output_item.done":
		if item, ok := obj["item"].(map[string]any); ok && b.collectCodexOutputItem(item, obj) {
			return
		}
	}

	output, ok := obj["output"].([]any)
	if !ok {
		return
	}
	for _, itemValue := range output {
		item, ok := itemValue.(map[string]any)
		if !ok {
			continue
		}
		if b.collectCodexOutputItem(item, nil) {
			continue
		}
		itemKey := stringFromAny(item["id"])
		if itemKey == "" {
			itemKey = stringFromAny(item["item_id"])
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, partValue := range content {
			part, ok := partValue.(map[string]any)
			if !ok {
				continue
			}
			b.beginContentItem(itemKey)
			b.appendContentValue(part["text"])
		}
	}
}

func (b *mergedResponseBuilder) collectAnthropicPayload(obj map[string]any) {
	if delta, ok := obj["delta"].(map[string]any); ok {
		key := toolKeyFromIndex(indexKeyFromAny(obj["index"]))
		b.appendToolDelta(key, "tool_call", delta["partial_json"])
	}
	content, ok := obj["content"].([]any)
	if !ok {
		return
	}
	for _, blockValue := range content {
		block, ok := blockValue.(map[string]any)
		if !ok {
			continue
		}
		b.appendContentValue(block["text"])
	}
}

func (b *mergedResponseBuilder) collectToolCalls(value any, streaming bool, choiceIndex string) {
	calls, ok := value.([]any)
	if !ok {
		return
	}
	for _, callValue := range calls {
		call, ok := callValue.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := call["function"].(map[string]any); ok {
			key := toolKeyFromOpenAIToolCall(call)
			if streaming {
				key = b.openAIStreamingToolKey(choiceIndex, call)
			}
			if streaming && key != "" {
				b.appendToolDelta(key, stringFromAny(fn["name"]), fn["arguments"])
			} else {
				b.appendToolCall(key, fn["name"], fn["arguments"])
			}
		}
	}
}

func (b *mergedResponseBuilder) appendTextDelta(delta string) {
	for _, part := range splitChatTextDeltaParts(delta, &b.streamState) {
		if part.text == "" {
			continue
		}
		if part.kind == "thinking" {
			b.appendReasoning(part.text)
		} else {
			b.appendContent(part.text)
		}
	}
}

func (b *mergedResponseBuilder) appendCodexTextDelta(obj map[string]any) bool {
	typ, _ := obj["type"].(string)
	if typ != "response.output_text.delta" && typ != "response.refusal.delta" {
		return false
	}
	delta := stringFromAny(obj["delta"])
	if delta == "" {
		return true
	}
	itemKey := stringFromAny(obj["item_id"])
	if itemKey == "" {
		itemKey = stringFromAny(obj["output_index"])
	}
	b.beginContentItem(itemKey)
	b.appendTextDelta(delta)
	return true
}

func (b *mergedResponseBuilder) beginContentItem(itemKey string) {
	if itemKey == "" {
		return
	}
	if b.lastContentItemKey != "" && b.lastContentItemKey != itemKey && b.content.Len() > 0 {
		b.content.WriteString(codexMessageItemSeparator)
	}
	b.lastContentItemKey = itemKey
}

func (b *mergedResponseBuilder) appendContentValue(value any) {
	switch v := value.(type) {
	case string:
		b.appendContent(v)
	case []any:
		for _, item := range v {
			b.appendContentValue(item)
		}
	case map[string]any:
		if text := stringFromAny(v["text"]); text != "" {
			b.appendContent(text)
		} else if content := stringFromAny(v["content"]); content != "" {
			b.appendContent(content)
		}
	}
}

func (b *mergedResponseBuilder) appendReasoningString(value any) {
	if text := stringFromAny(value); text != "" {
		b.appendReasoning(text)
	}
}

func (b *mergedResponseBuilder) appendToolCall(key string, name any, value any) {
	text := stringFromAny(value)
	if text == "" {
		return
	}
	if b.toolDelta.Len() > 0 {
		if key != "" && b.toolDeltaKey == key {
			b.clearToolDelta()
		} else {
			b.flushToolDelta()
		}
	}
	b.storeToolCall(key, stringFromAny(name), text)
}

func (b *mergedResponseBuilder) appendReasoning(text string) {
	b.reasoning.WriteString(text)
}

func (b *mergedResponseBuilder) appendContent(text string) {
	b.content.WriteString(text)
}

func (b *mergedResponseBuilder) parts() mergedResponseParts {
	b.flushToolDelta()
	return mergedResponseParts{
		Reasoning: strings.TrimSpace(b.reasoning.String()),
		Content:   formatJSONForMergedContent(strings.TrimSpace(b.content.String())),
		Tools:     formatMergedToolDiagnostics(strings.TrimSpace(b.toolCallsMarkdown())),
	}
}

func (b *mergedResponseBuilder) collectCodexOutputItem(item map[string]any, event map[string]any) bool {
	itemType := stringFromAny(item["type"])
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return false
	}
	value := item["arguments"]
	if itemType == "custom_tool_call" {
		value = item["input"]
	}
	b.appendToolCall(toolKeyFromCodexItem(item, event), item["name"], value)
	return true
}

func (b *mergedResponseBuilder) rememberCodexToolName(obj map[string]any) {
	item, ok := obj["item"].(map[string]any)
	if !ok {
		return
	}
	itemType := stringFromAny(item["type"])
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return
	}
	index := indexKeyFromAny(obj["output_index"])
	if index == "" {
		index = indexKeyFromAny(item["output_index"])
	}
	name := stringFromAny(item["name"])
	if index == "" || name == "" {
		return
	}
	if b.toolNamesByIndex == nil {
		b.toolNamesByIndex = make(map[string]string)
	}
	b.toolNamesByIndex[index] = name
}

func (b *mergedResponseBuilder) toolNameForIndex(index string, fallback string) string {
	if index != "" && b.toolNamesByIndex != nil {
		if name := b.toolNamesByIndex[index]; name != "" {
			return name
		}
	}
	return fallback
}

func (b *mergedResponseBuilder) appendToolDelta(key string, name string, value any) {
	text := stringFromAny(value)
	if text == "" {
		return
	}
	if b.toolDelta.Len() > 0 && b.toolDeltaKey != "" && key != "" && b.toolDeltaKey != key {
		b.flushToolDelta()
	}
	if b.toolDeltaName == "" {
		b.toolDeltaName = name
	}
	if key != "" {
		b.toolDeltaKey = key
	}
	b.toolDelta.WriteString(text)
}

func (b *mergedResponseBuilder) flushToolDelta() {
	text := strings.TrimSpace(b.toolDelta.String())
	if text == "" {
		b.clearToolDelta()
		return
	}
	b.storeToolCall(b.toolDeltaKey, b.toolDeltaName, text)
	b.clearToolDelta()
}

func (b *mergedResponseBuilder) clearToolDelta() {
	b.toolDelta.Reset()
	b.toolDeltaName = ""
	b.toolDeltaKey = ""
}

func (b *mergedResponseBuilder) storeToolCall(key string, name string, value string) {
	if key != "" {
		if b.toolCallIndexes == nil {
			b.toolCallIndexes = make(map[string]int)
		}
		if idx, ok := b.toolCallIndexes[key]; ok {
			if name != "" {
				b.toolCalls[idx].name = name
			}
			b.toolCalls[idx].value = value
			return
		}
		b.toolCallIndexes[key] = len(b.toolCalls)
	}
	b.toolCalls = append(b.toolCalls, mergedToolCall{
		key:   key,
		name:  name,
		value: value,
	})
}

func (b *mergedResponseBuilder) toolCallsMarkdown() string {
	if len(b.toolCalls) == 0 {
		return ""
	}
	sections := make([]string, 0, len(b.toolCalls))
	for _, call := range b.toolCalls {
		sections = append(sections, formatToolCallMarkdown(call.name, call.value))
	}
	return strings.Join(sections, "\n\n")
}

func hasMergedParts(parts mergedResponseParts) bool {
	return parts.Reasoning != "" || parts.Content != "" || parts.Tools != ""
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func indexKeyFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func toolKeyFromIndex(index string) string {
	if index == "" {
		return ""
	}
	return "index:" + index
}

func toolKeyFromPayload(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	if id := stringFromAny(obj["item_id"]); id != "" {
		return "id:" + id
	}
	if callID := stringFromAny(obj["call_id"]); callID != "" {
		return "call:" + callID
	}
	if key := toolKeyFromIndex(indexKeyFromAny(obj["output_index"])); key != "" {
		return key
	}
	return ""
}

func toolKeyFromCodexItem(item map[string]any, event map[string]any) string {
	if id := stringFromAny(item["id"]); id != "" {
		return "id:" + id
	}
	if callID := stringFromAny(item["call_id"]); callID != "" {
		return "call:" + callID
	}
	if event != nil {
		if key := toolKeyFromIndex(indexKeyFromAny(event["output_index"])); key != "" {
			return key
		}
	}
	return toolKeyFromIndex(indexKeyFromAny(item["output_index"]))
}

func toolKeyFromOpenAIToolCall(call map[string]any) string {
	if id := stringFromAny(call["id"]); id != "" {
		return "id:" + id
	}
	return toolKeyFromIndex(indexKeyFromAny(call["index"]))
}

func (b *mergedResponseBuilder) openAIStreamingToolKey(choiceIndex string, call map[string]any) string {
	index := indexKeyFromAny(call["index"])
	if index == "" {
		return toolKeyFromOpenAIToolCall(call)
	}
	slot := choiceIndex + ":" + index
	if id := stringFromAny(call["id"]); id != "" {
		key := "id:" + id
		if b.openAIToolKeys == nil {
			b.openAIToolKeys = make(map[string]string)
		}
		b.openAIToolKeys[slot] = key
		return key
	}
	if b.openAIToolKeys != nil {
		if key := b.openAIToolKeys[slot]; key != "" {
			return key
		}
	}
	return toolKeyFromIndex(slot)
}

func formatJSONForMergedContent(text string) string {
	raw := strings.TrimSpace(text)
	if raw == "" || (!strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[")) {
		return text
	}
	var parsed any
	if err := sonic.Unmarshal([]byte(raw), &parsed); err != nil {
		return text
	}
	formatted, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return text
	}
	return codeFence("json", string(formatted))
}

func formatMergedToolDiagnostics(text string) string {
	raw := strings.TrimSpace(text)
	if raw == "" || strings.Contains(raw, "### ") {
		return raw
	}
	return formatToolCallMarkdown("tool_call", raw)
}

func formatToolCallMarkdown(name string, value string) string {
	toolName := strings.TrimSpace(name)
	if toolName == "" {
		toolName = "tool_call"
	}

	values, ok := parseToolArgumentJSONValues(value)
	if ok {
		sections := make([]string, 0, len(values))
		for _, parsed := range values {
			sections = append(sections, formatSingleToolCallMarkdown(toolName, parsed, value))
		}
		return strings.Join(sections, "\n\n")
	}

	return "### " + toolName + "\n\n" + codeFence(toolCallRawLanguage(toolName, value), value)
}

func formatSingleToolCallMarkdown(toolName string, parsed any, original string) string {
	if obj, ok := parsed.(map[string]any); ok {
		if cmd, ok := obj["cmd"].(string); ok && strings.TrimSpace(cmd) != "" {
			return "### exec_command\n\n" + codeFence("bash", cmd)
		}
		formatted, err := json.MarshalIndent(obj, "", "  ")
		if err == nil {
			return "### " + toolName + "\n\n" + codeFence("json", string(formatted))
		}
	}
	return "### " + toolName + "\n\n" + codeFence(toolCallRawLanguage(toolName, original), original)
}

func toolCallRawLanguage(toolName string, value string) string {
	name := strings.ToLower(strings.TrimSpace(toolName))
	text := strings.TrimSpace(value)
	if name == "apply_patch" || strings.HasPrefix(text, "*** Begin Patch") {
		return "diff"
	}
	return ""
}

func parseToolArgumentJSONValues(value string) ([]any, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil, false
	}

	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	values := make([]any, 0, 1)
	for {
		var parsed any
		if err := decoder.Decode(&parsed); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false
		}
		values = append(values, parsed)
	}
	return values, len(values) > 0
}

func codeFence(language, value string) string {
	fence := "```"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	return fence + language + "\n" + value + "\n" + fence
}
