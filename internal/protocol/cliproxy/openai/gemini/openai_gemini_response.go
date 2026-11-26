// Package gemini provides response translation functionality for OpenAI to Gemini API.
// This package handles the conversion of OpenAI Chat Completions API responses into Gemini API-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Gemini API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, and usage metadata appropriately.
package gemini

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/MoeForge/moego/ojson"
)

// ConvertOpenAIResponseToGeminiParams holds state shared by one Chat response stream.
type ConvertOpenAIResponseToGeminiParams struct {
	choices   map[int]*openAIChoiceState
	model     string
	done      bool
	lastUsage []byte
}

type openAIChoiceState struct {
	tools map[int]*ToolCallAccumulator
	done  bool
}

// ToolCallAccumulator holds the state for accumulating tool call data.
type ToolCallAccumulator struct {
	ID               string
	Name             string
	Arguments        strings.Builder
	ThoughtSignature string
}

// ConvertOpenAIResponseToGemini converts one Chat Completions chunk to Gemini stream chunks.
func ConvertOpenAIResponseToGemini(_ context.Context, modelName string, _, _ []byte, rawJSON []byte, param *any) [][]byte {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &ConvertOpenAIResponseToGeminiParams{
			choices: make(map[int]*openAIChoiceState),
			model:   modelName,
		}
	}
	state := (*param).(*ConvertOpenAIResponseToGeminiParams)

	rawJSON = bytes.TrimSpace(rawJSON)
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}
	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		if state.done {
			return nil
		}
		state.done = true
		if len(state.choices) == 0 {
			return [][]byte{geminiStreamChunk(state.model, 0, nil, "STOP")}
		}

		var unfinished []int
		for index, choice := range state.choices {
			if !choice.done {
				unfinished = append(unfinished, index)
			}
		}
		sort.Ints(unfinished)
		var results [][]byte
		for _, index := range unfinished {
			parts := appendAccumulatedToolCalls(nil, state.choices[index].tools)
			results = append(results, geminiStreamChunk(state.model, index, parts, "STOP"))
		}
		return results
	}

	root := ojson.ParseBytes(rawJSON)
	if model := root.Get("model").String(); model != "" {
		state.model = model
	}
	choices := root.Get("choices")
	if !choices.IsArray() {
		return nil
	}

	var results [][]byte
	if !state.done {
		choices.ForEach(func(position, choice ojson.Result) bool {
			index := openAIChoiceIndex(position, choice)
			candidate := state.choices[index]
			if candidate == nil {
				candidate = &openAIChoiceState{}
				state.choices[index] = candidate
			}
			if candidate.done {
				return true
			}

			delta := choice.Get("delta")
			parts := openAIMessageGeminiParts(delta)
			candidate.accumulateTools(delta.Get("tool_calls"))

			finishReason := mapOpenAIFinishReasonToGemini(choice.Get("finish_reason").Str)
			if finishReason != "" {
				parts = appendAccumulatedToolCalls(parts, candidate.tools)
				candidate.tools = nil
				candidate.done = true
			}

			if len(parts) > 0 || finishReason != "" {
				results = append(results, geminiStreamChunk(state.model, index, parts, finishReason))
			}
			return true
		})
	}

	usage := root.Get("usage")
	if !usage.IsObject() {
		return results
	}
	metadata := translatorcommon.GeminiUsageMetadataFromOpenAI(usage, nil)
	if metadata == nil {
		if len(results) > 0 || state.lastUsage != nil {
			return results
		}
		metadata = []byte(`{}`)
	}
	if len(results) == 0 {
		if bytes.Equal(metadata, state.lastUsage) {
			return results
		}
		chunk := []byte(`{"candidates":[]}`)
		if state.model != "" {
			chunk, _ = ojson.SetBytes(chunk, "model", state.model)
		}
		results = append(results, chunk)
	}
	results[0], _ = ojson.SetRawBytes(results[0], "usageMetadata", metadata)
	state.lastUsage = metadata
	return results
}

func openAIChoiceIndex(position, choice ojson.Result) int {
	if index := choice.Get("index"); index.Type == ojson.Number {
		return int(index.Int())
	}
	return int(position.Int())
}

func geminiCandidate(index int, parts [][]byte, finishReason string) []byte {
	candidate := []byte(`{"content":{"parts":[],"role":"model"},"index":0}`)
	if len(parts) > 0 {
		candidate, _ = ojson.SetRawBytes(candidate, "content.parts", translatorcommon.JoinRawArray(parts))
	}
	candidate, _ = ojson.SetBytes(candidate, "index", index)
	if finishReason != "" {
		candidate, _ = ojson.SetBytes(candidate, "finishReason", finishReason)
	}
	return candidate
}

func geminiStreamChunk(model string, index int, parts [][]byte, finishReason string) []byte {
	chunk, _ := ojson.SetRawBytes([]byte(`{"candidates":[]}`), "candidates.0", geminiCandidate(index, parts, finishReason))
	if model != "" {
		chunk, _ = ojson.SetBytes(chunk, "model", model)
	}
	return chunk
}

func (choice *openAIChoiceState) accumulateTools(toolCalls ojson.Result) {
	if !toolCalls.IsArray() {
		return
	}
	toolCalls.ForEach(func(_, toolCall ojson.Result) bool {
		if toolType := toolCall.Get("type").String(); toolType != "" && toolType != "function" {
			return true
		}
		function := toolCall.Get("function")
		thoughtSignature := openAIToolCallThoughtSignature(toolCall)
		if !function.Exists() && thoughtSignature == "" {
			return true
		}

		index := int(toolCall.Get("index").Int())
		if choice.tools == nil {
			choice.tools = make(map[int]*ToolCallAccumulator)
		}
		tool := choice.tools[index]
		if tool == nil {
			tool = &ToolCallAccumulator{}
			choice.tools[index] = tool
		}
		if id := toolCall.Get("id").String(); id != "" {
			tool.ID = id
		}
		if name := function.Get("name").String(); name != "" {
			tool.Name = name
		}
		tool.Arguments.WriteString(function.Get("arguments").String())
		if thoughtSignature != "" {
			tool.ThoughtSignature = thoughtSignature
		}
		return true
	})
}

func appendAccumulatedToolCalls(parts [][]byte, tools map[int]*ToolCallAccumulator) [][]byte {
	indices := make([]int, 0, len(tools))
	for index := range tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		tool := tools[index]
		if tool.Name != "" {
			parts = append(parts, geminiFunctionPart(tool.ID, tool.Name, tool.Arguments.String(), tool.ThoughtSignature))
		}
	}
	return parts
}

func geminiFunctionPart(id, name, args, thoughtSignature string) []byte {
	part := []byte(`{"functionCall":{"name":"","args":{}}}`)
	part, _ = ojson.SetBytes(part, "functionCall.name", name)
	part, _ = ojson.SetRawBytes(part, "functionCall.args", []byte(parseArgsToObjectRaw(args)))
	if id != "" {
		part, _ = ojson.SetBytes(part, "functionCall.id", id)
	}
	if thoughtSignature != "" {
		part, _ = ojson.SetBytes(part, "thoughtSignature", thoughtSignature)
	}
	return part
}

func openAIToolCallThoughtSignature(toolCall ojson.Result) string {
	for _, path := range []string{
		"extra_content.google.thought_signature",
		"function.extra_content.google.thought_signature",
		"thoughtSignature",
		"thought_signature",
	} {
		if sig := toolCall.Get(path); sig.Exists() && sig.String() != "" {
			return sig.String()
		}
	}
	return ""
}

// mapOpenAIFinishReasonToGemini maps OpenAI finish reasons to Gemini finish reasons
func mapOpenAIFinishReasonToGemini(openAIReason string) string {
	switch strings.TrimSpace(openAIReason) {
	case "":
		return ""
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls":
		return "STOP" // Gemini doesn't have a specific tool_calls finish reason
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}

// parseArgsToObjectRaw safely parses a JSON string of function arguments into an object JSON string.
// It returns "{}" if the input is empty or cannot be parsed as a JSON object.
func parseArgsToObjectRaw(argsStr string) string {
	trimmed := strings.TrimSpace(argsStr)
	if trimmed == "" || trimmed == "{}" {
		return "{}"
	}

	// First try strict JSON
	if ojson.Valid(trimmed) {
		strict := ojson.Parse(trimmed)
		if strict.IsObject() {
			return strict.Raw
		}
	}

	// Tolerant parse: handle streams where values are barewords (e.g., 北京, celsius)
	tolerant := tolerantParseJSONObjectRaw(trimmed)
	if tolerant != "{}" {
		return tolerant
	}

	// Fallback: return empty object when parsing fails
	return "{}"
}

func escapeSjsonPathKey(key string) string {
	key = strings.ReplaceAll(key, `\`, `\\`)
	key = strings.ReplaceAll(key, `.`, `\.`)
	return key
}

// tolerantParseJSONObjectRaw attempts to parse a JSON-like object string into a JSON object string, tolerating
// bareword values (unquoted strings) commonly seen during streamed tool calls.
// Example input: {"location": 北京, "unit": celsius}
func tolerantParseJSONObjectRaw(s string) string {
	// Ensure we operate within the outermost braces if present
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || start >= end {
		return "{}"
	}
	content := s[start+1 : end]

	runes := []rune(content)
	n := len(runes)
	i := 0
	result := []byte(`{}`)

	for i < n {
		// Skip whitespace and commas
		for i < n && (runes[i] == ' ' || runes[i] == '\n' || runes[i] == '\r' || runes[i] == '\t' || runes[i] == ',') {
			i++
		}
		if i >= n {
			break
		}

		// Expect quoted key
		if runes[i] != '"' {
			// Unable to parse this segment reliably; skip to next comma
			for i < n && runes[i] != ',' {
				i++
			}
			continue
		}

		// Parse JSON string for key
		keyToken, nextIdx := parseJSONStringRunes(runes, i)
		if nextIdx == -1 {
			break
		}
		keyName := jsonStringTokenToRawString(keyToken)
		sjsonKey := escapeSjsonPathKey(keyName)
		i = nextIdx

		// Skip whitespace
		for i < n && (runes[i] == ' ' || runes[i] == '\n' || runes[i] == '\r' || runes[i] == '\t') {
			i++
		}
		if i >= n || runes[i] != ':' {
			break
		}
		i++ // skip ':'
		// Skip whitespace
		for i < n && (runes[i] == ' ' || runes[i] == '\n' || runes[i] == '\r' || runes[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}

		// Parse value (string, number, object/array, bareword)
		switch runes[i] {
		case '"':
			// JSON string
			valToken, ni := parseJSONStringRunes(runes, i)
			if ni == -1 {
				// Malformed; treat as empty string
				result, _ = ojson.SetBytes(result, sjsonKey, "")
				i = n
			} else {
				result, _ = ojson.SetBytes(result, sjsonKey, jsonStringTokenToRawString(valToken))
				i = ni
			}
		case '{', '[':
			// Bracketed value: attempt to capture balanced structure
			seg, ni := captureBracketed(runes, i)
			if ni == -1 {
				i = n
			} else {
				if ojson.Valid(seg) {
					result, _ = ojson.SetRawBytes(result, sjsonKey, []byte(seg))
				} else {
					result, _ = ojson.SetBytes(result, sjsonKey, seg)
				}
				i = ni
			}
		default:
			// Bare token until next comma or end
			j := i
			for j < n && runes[j] != ',' {
				j++
			}
			token := strings.TrimSpace(string(runes[i:j]))
			// Interpret common JSON atoms and numbers; otherwise treat as string
			if token == "true" {
				result, _ = ojson.SetBytes(result, sjsonKey, true)
			} else if token == "false" {
				result, _ = ojson.SetBytes(result, sjsonKey, false)
			} else if token == "null" {
				result, _ = ojson.SetBytes(result, sjsonKey, nil)
			} else if numVal, ok := tryParseNumber(token); ok {
				result, _ = ojson.SetBytes(result, sjsonKey, numVal)
			} else {
				result, _ = ojson.SetBytes(result, sjsonKey, token)
			}
			i = j
		}

		// Skip trailing whitespace and optional comma before next pair
		for i < n && (runes[i] == ' ' || runes[i] == '\n' || runes[i] == '\r' || runes[i] == '\t') {
			i++
		}
		if i < n && runes[i] == ',' {
			i++
		}
	}

	return string(result)
}

// parseJSONStringRunes returns the JSON string token (including quotes) and the index just after it.
func parseJSONStringRunes(runes []rune, start int) (string, int) {
	if start >= len(runes) || runes[start] != '"' {
		return "", -1
	}
	i := start + 1
	escaped := false
	for i < len(runes) {
		r := runes[i]
		if r == '\\' && !escaped {
			escaped = true
			i++
			continue
		}
		if r == '"' && !escaped {
			return string(runes[start : i+1]), i + 1
		}
		escaped = false
		i++
	}
	return string(runes[start:]), -1
}

// jsonStringTokenToRawString converts a JSON string token (including quotes) to a raw Go string value.
func jsonStringTokenToRawString(token string) string {
	r := ojson.Parse(token)
	if r.Type == ojson.String {
		return r.String()
	}
	// Fallback: strip surrounding quotes if present
	if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
		return token[1 : len(token)-1]
	}
	return token
}

// captureBracketed captures a balanced JSON object/array starting at index i.
// Returns the segment string and the index just after it; -1 if malformed.
func captureBracketed(runes []rune, i int) (string, int) {
	if i >= len(runes) {
		return "", -1
	}
	startRune := runes[i]
	var endRune rune
	switch startRune {
	case '{':
		endRune = '}'
	case '[':
		endRune = ']'
	default:
		return "", -1
	}
	depth := 0
	j := i
	inStr := false
	escaped := false
	for j < len(runes) {
		r := runes[j]
		if inStr {
			if r == '\\' && !escaped {
				escaped = true
				j++
				continue
			}
			if r == '"' && !escaped {
				inStr = false
			} else {
				escaped = false
			}
			j++
			continue
		}
		if r == '"' {
			inStr = true
			j++
			continue
		}
		switch r {
		case startRune:
			depth++
		case endRune:
			depth--
			if depth == 0 {
				return string(runes[i : j+1]), j + 1
			}
		}
		j++
	}
	return string(runes[i:]), -1
}

// tryParseNumber attempts to parse a string as an int or float.
func tryParseNumber(s string) (interface{}, bool) {
	if s == "" {
		return nil, false
	}
	// Try integer
	if i64, errParseInt := strconv.ParseInt(s, 10, 64); errParseInt == nil {
		return i64, true
	}
	if u64, errParseUInt := strconv.ParseUint(s, 10, 64); errParseUInt == nil {
		return u64, true
	}
	if f64, errParseFloat := strconv.ParseFloat(s, 64); errParseFloat == nil {
		return f64, true
	}
	return nil, false
}

// ConvertOpenAIResponseToGeminiNonStream converts a non-streaming OpenAI response to a non-streaming Gemini response.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the OpenAI API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - []byte: A Gemini-compatible JSON response.
func ConvertOpenAIResponseToGeminiNonStream(_ context.Context, _ string, _, _ []byte, rawJSON []byte, _ *any) []byte {
	root := ojson.ParseBytes(rawJSON)
	out := []byte(`{"candidates":[{"content":{"parts":[],"role":"model"},"index":0}]}`)
	if model := root.Get("model"); model.Exists() {
		out, _ = ojson.SetBytes(out, "model", model.String())
	}

	if choices := root.Get("choices"); choices.IsArray() {
		var candidates [][]byte
		choices.ForEach(func(position, choice ojson.Result) bool {
			message := choice.Get("message")
			parts := openAIMessageGeminiParts(message)
			if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() {
				for _, toolCall := range toolCalls.Array() {
					if toolCall.Get("type").String() != "function" {
						continue
					}
					function := toolCall.Get("function")
					parts = append(parts, geminiFunctionPart(toolCall.Get("id").String(), function.Get("name").String(), function.Get("arguments").String(), openAIToolCallThoughtSignature(toolCall)))
				}
			}

			finishReason := mapOpenAIFinishReasonToGemini(choice.Get("finish_reason").Str)
			index := openAIChoiceIndex(position, choice)
			candidates = append(candidates, geminiCandidate(index, parts, finishReason))
			return true
		})
		if len(candidates) > 0 {
			out, _ = ojson.SetRawBytes(out, "candidates", translatorcommon.JoinRawArray(candidates))
		}
	}
	if usage := root.Get("usage"); usage.IsObject() {
		if metadata := translatorcommon.GeminiUsageMetadataFromOpenAI(usage, nil); metadata != nil {
			out, _ = ojson.SetRawBytes(out, "usageMetadata", metadata)
		}
	}
	return out
}

// GeminiTokenCount builds a Gemini token count response.
func GeminiTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}

func openAIMessageGeminiParts(message ojson.Result) [][]byte {
	var parts [][]byte
	appendText := func(text string, thought bool) {
		if text == "" {
			return
		}
		part, _ := ojson.SetBytes([]byte(`{}`), "text", text)
		if thought {
			part, _ = ojson.SetBytes(part, "thought", true)
		}
		parts = append(parts, part)
	}
	var reasoning []string
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
		reasoning = extractReasoningTexts(message.Get(field))
		if len(reasoning) > 0 {
			for _, text := range reasoning {
				appendText(text, true)
			}
			break
		}
	}

	content := message.Get("content")
	if content.Type == ojson.String {
		appendText(content.String(), false)
	} else if content.IsArray() {
		content.ForEach(func(_, part ojson.Result) bool {
			if part.Type == ojson.String {
				appendText(part.String(), false)
				return true
			}
			switch part.Get("type").String() {
			case "text", "output_text":
				if text := part.Get("text"); text.Type == ojson.String {
					appendText(text.String(), false)
				}
			case "refusal":
				text := part.Get("refusal")
				if text.Type != ojson.String || text.String() == "" {
					text = part.Get("text")
				}
				if text.Type == ojson.String {
					appendText(text.String(), false)
				}
			case "thinking", "thought", "reasoning", "reasoning.text", "reasoning_text", "reasoning.summary":
				if len(reasoning) == 0 {
					for _, field := range []string{"thinking", "reasoning", "reasoning_content", "text", "summary"} {
						if texts := extractReasoningTexts(part.Get(field)); len(texts) > 0 {
							appendText(strings.Join(texts, ""), true)
							break
						}
					}
				}
			}
			return true
		})
	}
	return parts
}

func extractReasoningTexts(node ojson.Result) []string {
	if node.IsArray() {
		var texts []string
		node.ForEach(func(_, value ojson.Result) bool {
			texts = append(texts, extractReasoningTexts(value)...)
			return true
		})
		return texts
	}

	switch node.Type {
	case ojson.String:
		if text := node.String(); text != "" {
			return []string{text}
		}
	case ojson.JSON:
		if text := node.Get("text"); text.Type == ojson.String && text.String() != "" {
			return []string{text.String()}
		}
		if node.Get("type").String() == "reasoning.summary" {
			if summary := node.Get("summary"); summary.Type == ojson.String && summary.String() != "" {
				return []string{summary.String()}
			}
		}
		var texts []string
		for _, part := range node.Get("content").Array() {
			if part.Get("type").String() == "reasoning_text" {
				texts = append(texts, extractReasoningTexts(part)...)
			}
		}
		return texts
	}
	return nil
}
