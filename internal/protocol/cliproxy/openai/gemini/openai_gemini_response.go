// Package gemini provides response translation functionality for OpenAI to Gemini API.
// This package handles the conversion of OpenAI Chat Completions API responses into Gemini API-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Gemini API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, and usage metadata appropriately.
package gemini

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponseToGeminiParams holds parameters for response conversion
type ConvertOpenAIResponseToGeminiParams struct {
	ToolCallsAccumulator map[int]*ToolCallAccumulator
	Model                string
	Done                 bool
	UsageEmitted         bool
}

// ToolCallAccumulator holds the state for accumulating tool call data
type ToolCallAccumulator struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// ConvertOpenAIResponseToGemini converts OpenAI Chat Completions streaming response format to Gemini API format.
// This function processes OpenAI streaming chunks and transforms them into Gemini-compatible JSON responses.
// It handles text content, tool calls, and usage metadata, outputting responses that match the Gemini API format.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the OpenAI API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - [][]byte: A slice of Gemini-compatible JSON responses.
func ConvertOpenAIResponseToGemini(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &ConvertOpenAIResponseToGeminiParams{
			ToolCallsAccumulator: make(map[int]*ToolCallAccumulator),
			Model:                modelName,
		}
	}
	state := (*param).(*ConvertOpenAIResponseToGeminiParams)
	if state.ToolCallsAccumulator == nil {
		state.ToolCallsAccumulator = make(map[int]*ToolCallAccumulator)
	}
	if state.Model == "" {
		state.Model = modelName
	}

	rawJSON = bytes.TrimSpace(rawJSON)
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}
	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		if state.Done {
			return nil
		}
		out := geminiStreamTemplate(state.Model, 0)
		out, _ = sjson.SetBytes(out, "candidates.0.finishReason", "STOP")
		out = appendAccumulatedToolCalls(out, state)
		state.Done = true
		return [][]byte{out}
	}

	root := gjson.ParseBytes(rawJSON)
	if model := root.Get("model"); model.Exists() && model.String() != "" {
		state.Model = model.String()
	}

	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() {
		if len(choices.Array()) == 0 {
			if usage := root.Get("usage"); usage.Exists() && !state.UsageEmitted {
				template := []byte(`{"candidates":[],"usageMetadata":{}}`)
				if state.Model != "" {
					template, _ = sjson.SetBytes(template, "model", state.Model)
				}
				template = setGeminiUsageMetadataFromOpenAIUsage(template, usage)
				state.UsageEmitted = true
				return [][]byte{template}
			}
			return nil
		}
		if state.Done {
			return nil
		}

		var results [][]byte
		choices.ForEach(func(_, choice gjson.Result) bool {
			choiceIndex := int(choice.Get("index").Int())
			template := geminiStreamTemplate(state.Model, choiceIndex)
			delta := choice.Get("delta")
			partIndex := 0

			if reasoning := delta.Get("reasoning_content"); reasoning.Exists() {
				for _, reasoningText := range extractReasoningTexts(reasoning) {
					if reasoningText == "" {
						continue
					}
					template, _ = sjson.SetBytes(template, fmt.Sprintf("candidates.0.content.parts.%d.thought", partIndex), true)
					template, _ = sjson.SetBytes(template, fmt.Sprintf("candidates.0.content.parts.%d.text", partIndex), reasoningText)
					partIndex++
				}
			}

			if content := delta.Get("content"); content.Exists() && content.String() != "" {
				template, _ = sjson.SetBytes(template, fmt.Sprintf("candidates.0.content.parts.%d.text", partIndex), content.String())
				partIndex++
			}

			if toolCalls := delta.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
				toolCalls.ForEach(func(_, toolCall gjson.Result) bool {
					toolIndex := int(toolCall.Get("index").Int())
					toolID := toolCall.Get("id").String()
					toolType := toolCall.Get("type").String()
					function := toolCall.Get("function")

					// Skip non-function tool calls explicitly marked as other types.
					if toolType != "" && toolType != "function" {
						return true
					}

					// OpenAI streaming deltas may omit the type field while still carrying function data.
					if !function.Exists() {
						return true
					}

					functionName := function.Get("name").String()
					functionArgs := function.Get("arguments").String()

					// Initialize accumulator if needed so later deltas without type can append arguments.
					if _, exists := state.ToolCallsAccumulator[toolIndex]; !exists {
						state.ToolCallsAccumulator[toolIndex] = &ToolCallAccumulator{
							ID:   toolID,
							Name: functionName,
						}
					}
					acc := state.ToolCallsAccumulator[toolIndex]

					// Update ID if provided
					if toolID != "" {
						acc.ID = toolID
					}

					// Update name if provided
					if functionName != "" {
						acc.Name = functionName
					}

					// Accumulate arguments
					if functionArgs != "" {
						acc.Arguments.WriteString(functionArgs)
					}

					return true
				})
			}

			finishReason := choice.Get("finish_reason")
			finished := finishReason.Exists() && finishReason.Type == gjson.String
			if finished {
				geminiFinishReason := mapOpenAIFinishReasonToGemini(finishReason.String())
				template, _ = sjson.SetBytes(template, "candidates.0.finishReason", geminiFinishReason)
				template = appendAccumulatedToolCalls(template, state)
				state.Done = true
			}

			if usage := root.Get("usage"); usage.Exists() {
				template = setGeminiUsageMetadataFromOpenAIUsage(template, usage)
				state.UsageEmitted = true
			}
			if partIndex > 0 || finished || root.Get("usage").Exists() {
				results = append(results, template)
			}
			return true
		})
		return results
	}
	return nil
}

func geminiStreamTemplate(model string, index int) []byte {
	template := []byte(`{"candidates":[{"content":{"parts":[],"role":"model"},"index":0}]}`)
	template, _ = sjson.SetBytes(template, "candidates.0.index", index)
	if model != "" {
		template, _ = sjson.SetBytes(template, "model", model)
	}
	return template
}

func appendAccumulatedToolCalls(template []byte, state *ConvertOpenAIResponseToGeminiParams) []byte {
	indices := make([]int, 0, len(state.ToolCallsAccumulator))
	for index := range state.ToolCallsAccumulator {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	partIndex := len(gjson.GetBytes(template, "candidates.0.content.parts").Array())
	for _, index := range indices {
		accumulator := state.ToolCallsAccumulator[index]
		if accumulator == nil || accumulator.Name == "" {
			continue
		}
		basePath := fmt.Sprintf("candidates.0.content.parts.%d.functionCall", partIndex)
		if accumulator.ID != "" {
			template, _ = sjson.SetBytes(template, basePath+".id", accumulator.ID)
		}
		template, _ = sjson.SetBytes(template, basePath+".name", accumulator.Name)
		template, _ = sjson.SetRawBytes(template, basePath+".args", []byte(parseArgsToObjectRaw(accumulator.Arguments.String())))
		partIndex++
	}
	state.ToolCallsAccumulator = make(map[int]*ToolCallAccumulator)
	return template
}

// mapOpenAIFinishReasonToGemini maps OpenAI finish reasons to Gemini finish reasons
func mapOpenAIFinishReasonToGemini(openAIReason string) string {
	switch openAIReason {
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
	if gjson.Valid(trimmed) {
		strict := gjson.Parse(trimmed)
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
				result, _ = sjson.SetBytes(result, sjsonKey, "")
				i = n
			} else {
				result, _ = sjson.SetBytes(result, sjsonKey, jsonStringTokenToRawString(valToken))
				i = ni
			}
		case '{', '[':
			// Bracketed value: attempt to capture balanced structure
			seg, ni := captureBracketed(runes, i)
			if ni == -1 {
				i = n
			} else {
				if gjson.Valid(seg) {
					result, _ = sjson.SetRawBytes(result, sjsonKey, []byte(seg))
				} else {
					result, _ = sjson.SetBytes(result, sjsonKey, seg)
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
				result, _ = sjson.SetBytes(result, sjsonKey, true)
			} else if token == "false" {
				result, _ = sjson.SetBytes(result, sjsonKey, false)
			} else if token == "null" {
				result, _ = sjson.SetBytes(result, sjsonKey, nil)
			} else if numVal, ok := tryParseNumber(token); ok {
				result, _ = sjson.SetBytes(result, sjsonKey, numVal)
			} else {
				result, _ = sjson.SetBytes(result, sjsonKey, token)
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
	r := gjson.Parse(token)
	if r.Type == gjson.String {
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
func ConvertOpenAIResponseToGeminiNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	root := gjson.ParseBytes(rawJSON)

	// Base Gemini response template without finishReason; set when known
	out := []byte(`{"candidates":[{"content":{"parts":[],"role":"model"},"index":0}]}`)

	// Set model if available
	if model := root.Get("model"); model.Exists() {
		out, _ = sjson.SetBytes(out, "model", model.String())
	}

	// Process choices
	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() {
		choices.ForEach(func(choiceIndex, choice gjson.Result) bool {
			choiceIdx := int(choice.Get("index").Int())
			message := choice.Get("message")

			// Set role
			if role := message.Get("role"); role.Exists() {
				if role.String() == "assistant" {
					out, _ = sjson.SetBytes(out, "candidates.0.content.role", "model")
				}
			}

			partIndex := 0

			// Handle reasoning content before visible text
			if reasoning := message.Get("reasoning_content"); reasoning.Exists() {
				for _, reasoningText := range extractReasoningTexts(reasoning) {
					if reasoningText == "" {
						continue
					}
					out, _ = sjson.SetBytes(out, fmt.Sprintf("candidates.0.content.parts.%d.thought", partIndex), true)
					out, _ = sjson.SetBytes(out, fmt.Sprintf("candidates.0.content.parts.%d.text", partIndex), reasoningText)
					partIndex++
				}
			}

			// Handle content first
			if content := message.Get("content"); content.Exists() && content.String() != "" {
				out, _ = sjson.SetBytes(out, fmt.Sprintf("candidates.0.content.parts.%d.text", partIndex), content.String())
				partIndex++
			}

			// Handle tool calls
			if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
				toolCalls.ForEach(func(_, toolCall gjson.Result) bool {
					if toolCall.Get("type").String() == "function" {
						function := toolCall.Get("function")
						functionName := function.Get("name").String()
						functionArgs := function.Get("arguments").String()
						functionID := toolCall.Get("id").String()

						idPath := fmt.Sprintf("candidates.0.content.parts.%d.functionCall.id", partIndex)
						namePath := fmt.Sprintf("candidates.0.content.parts.%d.functionCall.name", partIndex)
						argsPath := fmt.Sprintf("candidates.0.content.parts.%d.functionCall.args", partIndex)
						if functionID != "" {
							out, _ = sjson.SetBytes(out, idPath, functionID)
						}
						out, _ = sjson.SetBytes(out, namePath, functionName)
						out, _ = sjson.SetRawBytes(out, argsPath, []byte(parseArgsToObjectRaw(functionArgs)))
						partIndex++
					}
					return true
				})
			}

			// Handle finish reason
			if finishReason := choice.Get("finish_reason"); finishReason.Exists() {
				geminiFinishReason := mapOpenAIFinishReasonToGemini(finishReason.String())
				out, _ = sjson.SetBytes(out, "candidates.0.finishReason", geminiFinishReason)
			}

			// Set index
			out, _ = sjson.SetBytes(out, "candidates.0.index", choiceIdx)

			return true
		})
	}

	// Handle usage information
	if usage := root.Get("usage"); usage.Exists() {
		out = setGeminiUsageMetadataFromOpenAIUsage(out, usage)
	}

	return out
}

// GeminiTokenCount builds a Gemini token count response.
func GeminiTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}

func reasoningTokensFromUsage(usage gjson.Result) int64 {
	if usage.Exists() {
		if v := usage.Get("completion_tokens_details.reasoning_tokens"); v.Exists() {
			return v.Int()
		}
		if v := usage.Get("output_tokens_details.reasoning_tokens"); v.Exists() {
			return v.Int()
		}
	}
	return 0
}

func setGeminiUsageMetadataFromOpenAIUsage(out []byte, usage gjson.Result) []byte {
	promptTokens, hasPromptTokens := tokenCountFromUsage(usage, "prompt_tokens", "input_tokens")
	completionTokens, hasCompletionTokens := tokenCountFromUsage(usage, "completion_tokens", "output_tokens")
	totalTokens, hasTotalTokens := tokenCountFromUsage(usage, "total_tokens")
	if hasPromptTokens {
		out, _ = sjson.SetBytes(out, "usageMetadata.promptTokenCount", promptTokens)
	}
	if hasCompletionTokens {
		out, _ = sjson.SetBytes(out, "usageMetadata.candidatesTokenCount", completionTokens)
	}
	if hasTotalTokens {
		out, _ = sjson.SetBytes(out, "usageMetadata.totalTokenCount", totalTokens)
	} else if hasPromptTokens || hasCompletionTokens {
		out, _ = sjson.SetBytes(out, "usageMetadata.totalTokenCount", promptTokens+completionTokens)
	}
	if reasoningTokens := reasoningTokensFromUsage(usage); reasoningTokens > 0 {
		out, _ = sjson.SetBytes(out, "usageMetadata.thoughtsTokenCount", reasoningTokens)
	}
	if cachedTokens := cachedTokensFromUsage(usage); cachedTokens > 0 {
		out, _ = sjson.SetBytes(out, "usageMetadata.cachedContentTokenCount", cachedTokens)
	}
	return out
}

func tokenCountFromUsage(usage gjson.Result, paths ...string) (int64, bool) {
	for _, path := range paths {
		if v := usage.Get(path); v.Exists() {
			return v.Int(), true
		}
	}
	return 0, false
}

func cachedTokensFromUsage(usage gjson.Result) int64 {
	if usage.Exists() {
		if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			return v.Int()
		}
		if v := usage.Get("input_tokens_details.cached_tokens"); v.Exists() {
			return v.Int()
		}
	}
	return 0
}

func extractReasoningTexts(node gjson.Result) []string {
	var texts []string
	if !node.Exists() {
		return texts
	}

	if node.IsArray() {
		node.ForEach(func(_, value gjson.Result) bool {
			texts = append(texts, extractReasoningTexts(value)...)
			return true
		})
		return texts
	}

	switch node.Type {
	case gjson.String:
		texts = append(texts, node.String())
	case gjson.JSON:
		if text := node.Get("text"); text.Exists() {
			texts = append(texts, text.String())
		} else if raw := strings.TrimSpace(node.Raw); raw != "" && !strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[") {
			texts = append(texts, raw)
		}
	}

	return texts
}
