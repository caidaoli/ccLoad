// Package gemini provides response translation functionality for Claude Code to Gemini API compatibility.
// This package handles the conversion of Claude Code API responses into Gemini-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Gemini API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, and usage metadata appropriately.
package gemini

import (
	"bytes"
	"context"
	"strings"
	"time"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// ConvertAnthropicResponseToGeminiParams holds parameters for response conversion
// It also carries minimal streaming state across calls to assemble tool_use input_json_delta.
// This structure maintains state information needed for proper conversion of streaming responses
// from Claude Code format to Gemini format, particularly for handling tool calls that span
// multiple streaming events.
type ConvertAnthropicResponseToGeminiParams struct {
	Model             string
	CreatedAt         int64
	ResponseID        string
	LastStorageOutput []byte
	IsStreaming       bool

	// Streaming state for tool_use assembly
	// Keyed by content_block index from Claude SSE events
	ToolUseNames             map[int]string           // function/tool name per block index
	ToolUseArgs              map[int]*strings.Builder // accumulates partial_json across deltas
	ToolUseIDs               map[int]string           // tool use ID per block index
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	ReasoningTokens          int64
}

func mergeClaudeGeminiUsage(state *ConvertAnthropicResponseToGeminiParams, usage gjson.Result) {
	if !usage.Exists() {
		return
	}
	if value := usage.Get("input_tokens"); value.Exists() {
		state.InputTokens = value.Int()
	}
	if value := usage.Get("output_tokens"); value.Exists() {
		state.OutputTokens = value.Int()
	}
	if value := usage.Get("cache_creation_input_tokens"); value.Exists() {
		state.CacheCreationInputTokens = value.Int()
	}
	if value := usage.Get("cache_read_input_tokens"); value.Exists() {
		state.CacheReadInputTokens = value.Int()
	}
	if value := usage.Get("reasoning_tokens"); value.Exists() {
		state.ReasoningTokens = value.Int()
	} else if value = usage.Get("thinking_tokens"); value.Exists() {
		state.ReasoningTokens = value.Int()
	}
}

// ConvertClaudeResponseToGemini converts Claude Code streaming response format to Gemini format.
// This function processes various Claude Code event types and transforms them into Gemini-compatible JSON responses.
// It handles text content, tool calls, reasoning content, and usage metadata, outputting responses that match
// the Gemini API format. The function supports incremental updates for streaming responses and maintains
// state information to properly assemble multi-part tool calls.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Claude Code API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Gemini-compatible JSON responses
func ConvertClaudeResponseToGemini(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &ConvertAnthropicResponseToGeminiParams{
			Model:      modelName,
			CreatedAt:  0,
			ResponseID: "",
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	root := gjson.ParseBytes(rawJSON)
	eventType := root.Get("type").String()

	// Base Gemini response template with default values
	template := []byte(`{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"","createTime":"","responseId":""}`)

	// Set model version
	if (*param).(*ConvertAnthropicResponseToGeminiParams).Model != "" {
		// Map Claude model names back to Gemini model names
		template, _ = sjson.SetBytes(template, "modelVersion", (*param).(*ConvertAnthropicResponseToGeminiParams).Model)
	}

	// Set response ID and creation time
	if (*param).(*ConvertAnthropicResponseToGeminiParams).ResponseID != "" {
		template, _ = sjson.SetBytes(template, "responseId", (*param).(*ConvertAnthropicResponseToGeminiParams).ResponseID)
	}

	// Set creation time to current time if not provided
	if (*param).(*ConvertAnthropicResponseToGeminiParams).CreatedAt == 0 {
		(*param).(*ConvertAnthropicResponseToGeminiParams).CreatedAt = time.Now().Unix()
	}
	template, _ = sjson.SetBytes(template, "createTime", time.Unix((*param).(*ConvertAnthropicResponseToGeminiParams).CreatedAt, 0).Format(time.RFC3339Nano))

	switch eventType {
	case "message_start":
		// Initialize response with message metadata when a new message begins
		if message := root.Get("message"); message.Exists() {
			(*param).(*ConvertAnthropicResponseToGeminiParams).ResponseID = message.Get("id").String()
			(*param).(*ConvertAnthropicResponseToGeminiParams).Model = message.Get("model").String()
			mergeClaudeGeminiUsage((*param).(*ConvertAnthropicResponseToGeminiParams), message.Get("usage"))
		}
		return [][]byte{}

	case "content_block_start":
		// Start of a content block - record tool_use name by index for functionCall assembly
		if cb := root.Get("content_block"); cb.Exists() {
			if cb.Get("type").String() == "tool_use" {
				idx := int(root.Get("index").Int())
				if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseNames == nil {
					(*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseNames = map[int]string{}
				}
				if name := cb.Get("name"); name.Exists() {
					(*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseNames[idx] = name.String()
				}
				if toolID := cb.Get("id").String(); toolID != "" {
					if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseIDs == nil {
						(*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseIDs = map[int]string{}
					}
					(*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseIDs[idx] = toolID
				}
			} else if cb.Get("type").String() == "redacted_thinking" {
				if data := cb.Get("data"); data.Exists() && data.String() != "" {
					part := []byte(`{"thought":true,"thoughtSignature":""}`)
					part, _ = sjson.SetBytes(part, "thoughtSignature", data.String())
					template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
					return [][]byte{template}
				}
			}
		}
		return [][]byte{}

	case "content_block_delta":
		// Handle content delta (text, thinking, or tool use arguments)
		if delta := root.Get("delta"); delta.Exists() {
			deltaType := delta.Get("type").String()

			switch deltaType {
			case "text_delta":
				// Regular text content delta for normal response text
				if text := delta.Get("text"); text.Exists() && text.String() != "" {
					textPart := []byte(`{"text":""}`)
					textPart, _ = sjson.SetBytes(textPart, "text", text.String())
					template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", textPart)
				}
			case "thinking_delta":
				// Thinking/reasoning content delta for models with reasoning capabilities
				if text := delta.Get("thinking"); text.Exists() && text.String() != "" {
					thinkingPart := []byte(`{"thought":true,"text":""}`)
					thinkingPart, _ = sjson.SetBytes(thinkingPart, "text", text.String())
					template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", thinkingPart)
				}
			case "signature_delta":
				if signature := delta.Get("signature"); signature.Exists() && signature.String() != "" {
					part := []byte(`{"thought":true,"thoughtSignature":""}`)
					part, _ = sjson.SetBytes(part, "thoughtSignature", signature.String())
					template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
				}
			case "input_json_delta":
				// Tool use input delta - accumulate partial_json by index for later assembly at content_block_stop
				idx := int(root.Get("index").Int())
				if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs == nil {
					(*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs = map[int]*strings.Builder{}
				}
				b, ok := (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs[idx]
				if !ok || b == nil {
					bb := &strings.Builder{}
					(*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs[idx] = bb
					b = bb
				}
				if pj := delta.Get("partial_json"); pj.Exists() {
					b.WriteString(pj.String())
				}
				return [][]byte{}
			}
		}
		return [][]byte{template}

	case "content_block_stop":
		// End of content block - finalize tool calls if any
		idx := int(root.Get("index").Int())
		// Claude's content_block_stop often doesn't include content_block payload (see docs/response-claude.txt)
		// So we finalize using accumulated state captured during content_block_start and input_json_delta.
		name := ""
		if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseNames != nil {
			name = (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseNames[idx]
		}
		var argsTrim string
		if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs != nil {
			if b := (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs[idx]; b != nil {
				argsTrim = strings.TrimSpace(b.String())
			}
		}
		toolID := ""
		if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseIDs != nil {
			toolID = (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseIDs[idx]
		}
		if name != "" || argsTrim != "" {
			functionCall := []byte(`{"functionCall":{"name":"","args":{}}}`)
			if name != "" {
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.name", name)
			}
			if argsTrim != "" {
				functionCall, _ = sjson.SetRawBytes(functionCall, "functionCall.args", []byte(argsTrim))
			}
			if toolID != "" {
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.id", toolID)
			}
			template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", functionCall)
			template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
			(*param).(*ConvertAnthropicResponseToGeminiParams).LastStorageOutput = append([]byte(nil), template...)
			// cleanup used state for this index
			if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs != nil {
				delete((*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseArgs, idx)
			}
			if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseNames != nil {
				delete((*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseNames, idx)
			}
			if (*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseIDs != nil {
				delete((*param).(*ConvertAnthropicResponseToGeminiParams).ToolUseIDs, idx)
			}
			return [][]byte{template}
		}
		return [][]byte{}

	case "message_delta":
		// Handle message-level changes (like stop reason and usage information)
		if delta := root.Get("delta"); delta.Exists() {
			if stopReason := delta.Get("stop_reason"); stopReason.Exists() {
				switch stopReason.String() {
				case "end_turn":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				case "tool_use":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				case "max_tokens":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "MAX_TOKENS")
				case "stop_sequence":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				default:
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				}
			}
		}

		if usage := root.Get("usage"); usage.Exists() {
			state := (*param).(*ConvertAnthropicResponseToGeminiParams)
			mergeClaudeGeminiUsage(state, usage)
			cachedTokens := state.CacheCreationInputTokens + state.CacheReadInputTokens
			inputTokens := state.InputTokens + cachedTokens
			outputTokens := state.OutputTokens

			// Set basic usage metadata according to Gemini API specification
			template, _ = sjson.SetBytes(template, "usageMetadata.promptTokenCount", inputTokens)
			template, _ = sjson.SetBytes(template, "usageMetadata.candidatesTokenCount", outputTokens)
			template, _ = sjson.SetBytes(template, "usageMetadata.totalTokenCount", inputTokens+outputTokens)

			if cachedTokens > 0 {
				template, _ = sjson.SetBytes(template, "usageMetadata.cachedContentTokenCount", cachedTokens)
			}

			// Add thinking tokens if present (for models with reasoning capabilities)
			if state.ReasoningTokens > 0 {
				template, _ = sjson.SetBytes(template, "usageMetadata.thoughtsTokenCount", state.ReasoningTokens)
			}

			// Set traffic type (required by Gemini API)
			template, _ = sjson.SetBytes(template, "usageMetadata.trafficType", "PROVISIONED_THROUGHPUT")
		}
		template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")

		return [][]byte{template}
	case "message_stop":
		// Final message with usage information - no additional output needed
		return [][]byte{}
	case "error":
		// Handle error responses and convert to Gemini error format
		errorMsg := root.Get("error.message").String()
		if errorMsg == "" {
			errorMsg = "Unknown error occurred"
		}

		// Create error response in Gemini format
		errorResponse := []byte(`{"error":{"code":400,"message":"","status":"INVALID_ARGUMENT"}}`)
		errorResponse, _ = sjson.SetBytes(errorResponse, "error.message", errorMsg)
		return [][]byte{errorResponse}

	default:
		// Unknown event type, return empty response
		return [][]byte{}
	}
}

// ConvertClaudeResponseToGeminiNonStream converts a non-streaming Claude Code response to a non-streaming Gemini response.
// This function processes the complete Claude Code response and transforms it into a single Gemini-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Gemini API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Claude Code API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: A Gemini-compatible JSON response containing all message content and metadata
func ConvertClaudeResponseToGeminiNonStream(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	_ = originalRequestRawJSON
	_ = requestRawJSON

	root := gjson.ParseBytes(rawJSON)
	if root.Get("type").String() != "message" || !root.Get("content").IsArray() {
		return nil
	}

	out := []byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"modelVersion":"","responseId":""}`)
	out, _ = sjson.SetBytes(out, "modelVersion", modelName)
	out, _ = sjson.SetBytes(out, "responseId", root.Get("id").String())

	for _, block := range root.Get("content").Array() {
		var part []byte
		switch block.Get("type").String() {
		case "text":
			part = []byte(`{"text":""}`)
			part, _ = sjson.SetBytes(part, "text", block.Get("text").String())
		case "thinking":
			part = []byte(`{"thought":true,"text":""}`)
			part, _ = sjson.SetBytes(part, "text", block.Get("thinking").String())
			if signature := block.Get("signature"); signature.Exists() && signature.String() != "" {
				part, _ = sjson.SetBytes(part, "thoughtSignature", signature.String())
			}
		case "redacted_thinking":
			part = []byte(`{"thought":true,"thoughtSignature":""}`)
			part, _ = sjson.SetBytes(part, "thoughtSignature", block.Get("data").String())
		case "tool_use":
			part = []byte(`{"functionCall":{"id":"","name":"","args":{}}}`)
			part, _ = sjson.SetBytes(part, "functionCall.id", block.Get("id").String())
			part, _ = sjson.SetBytes(part, "functionCall.name", block.Get("name").String())
			if input := block.Get("input"); input.Exists() && input.IsObject() {
				part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(input.Raw))
			}
		default:
			return nil
		}
		out, _ = sjson.SetRawBytes(out, "candidates.0.content.parts.-1", part)
	}

	finishReason := "STOP"
	if root.Get("stop_reason").String() == "max_tokens" {
		finishReason = "MAX_TOKENS"
	}
	out, _ = sjson.SetBytes(out, "candidates.0.finishReason", finishReason)

	if usage := root.Get("usage"); usage.Exists() {
		cachedTokens := usage.Get("cache_read_input_tokens").Int() + usage.Get("cache_creation_input_tokens").Int()
		inputTokens := usage.Get("input_tokens").Int() + cachedTokens
		outputTokens := usage.Get("output_tokens").Int()
		out, _ = sjson.SetBytes(out, "usageMetadata.promptTokenCount", inputTokens)
		out, _ = sjson.SetBytes(out, "usageMetadata.candidatesTokenCount", outputTokens)
		out, _ = sjson.SetBytes(out, "usageMetadata.totalTokenCount", inputTokens+outputTokens)
		if cachedTokens > 0 {
			out, _ = sjson.SetBytes(out, "usageMetadata.cachedContentTokenCount", cachedTokens)
		}
		if reasoningTokens := usage.Get("reasoning_tokens").Int(); reasoningTokens > 0 {
			out, _ = sjson.SetBytes(out, "usageMetadata.thoughtsTokenCount", reasoningTokens)
		}
	}

	return out
}

// GeminiTokenCount builds a Gemini token count response.
func GeminiTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}
