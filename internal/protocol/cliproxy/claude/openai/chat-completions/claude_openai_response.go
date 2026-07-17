// Package openai provides response translation functionality for Claude Code to OpenAI API compatibility.
// This package handles the conversion of Claude Code API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// ConvertAnthropicResponseToOpenAIParams holds parameters for response conversion
type ConvertAnthropicResponseToOpenAIParams struct {
	CreatedAt    int64
	ResponseID   string
	FinishReason string
	Usage        claudeUsageTokens
	// Tool calls accumulator for streaming
	ToolCallsAccumulator map[int]*ToolCallAccumulator
	ReasoningAccumulator map[int]*ReasoningAccumulator
	Done                 bool
}

type claudeUsageTokens struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	ReasoningTokens          int64
	HasUsage                 bool
}

// ToolCallAccumulator holds the state for accumulating tool call data
type ToolCallAccumulator struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// ReasoningAccumulator collects one streamed Anthropic reasoning block.
type ReasoningAccumulator struct {
	Type      string
	Text      strings.Builder
	Signature strings.Builder
	Data      string
}

func (u *claudeUsageTokens) Merge(usage gjson.Result) {
	if !usage.Exists() {
		return
	}
	u.HasUsage = true
	if inputTokens := usage.Get("input_tokens"); inputTokens.Exists() {
		u.InputTokens = inputTokens.Int()
	}
	if outputTokens := usage.Get("output_tokens"); outputTokens.Exists() {
		u.OutputTokens = outputTokens.Int()
	}
	if cacheCreationInputTokens := usage.Get("cache_creation_input_tokens"); cacheCreationInputTokens.Exists() {
		u.CacheCreationInputTokens = cacheCreationInputTokens.Int()
	}
	if cacheReadInputTokens := usage.Get("cache_read_input_tokens"); cacheReadInputTokens.Exists() {
		u.CacheReadInputTokens = cacheReadInputTokens.Int()
	}
	if reasoningTokens := usage.Get("reasoning_tokens"); reasoningTokens.Exists() {
		u.ReasoningTokens = reasoningTokens.Int()
	}
}

func (u claudeUsageTokens) OpenAIUsage() (promptTokens, completionTokens, totalTokens, cachedTokens int64) {
	cachedTokens = u.CacheReadInputTokens
	promptTokens = u.InputTokens + u.CacheCreationInputTokens + cachedTokens
	completionTokens = u.OutputTokens
	totalTokens = promptTokens + completionTokens
	return promptTokens, completionTokens, totalTokens, cachedTokens
}

// ConvertClaudeResponseToOpenAI converts Claude Code streaming response format to OpenAI Chat Completions format.
// This function processes various Claude Code event types and transforms them into OpenAI-compatible JSON responses.
// It handles text content, tool calls, reasoning content, and usage metadata, outputting responses that match
// the OpenAI API format. The function supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Claude Code API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func ConvertClaudeResponseToOpenAI(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &ConvertAnthropicResponseToOpenAIParams{
			CreatedAt:    0,
			ResponseID:   "",
			FinishReason: "",
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	root := gjson.ParseBytes(rawJSON)
	eventType := root.Get("type").String()

	// Base OpenAI streaming response template
	template := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":null}]}`)

	// Set model
	if modelName != "" {
		template, _ = sjson.SetBytes(template, "model", modelName)
	}

	// Set response ID and creation time
	if (*param).(*ConvertAnthropicResponseToOpenAIParams).ResponseID != "" {
		template, _ = sjson.SetBytes(template, "id", (*param).(*ConvertAnthropicResponseToOpenAIParams).ResponseID)
	}
	if (*param).(*ConvertAnthropicResponseToOpenAIParams).CreatedAt > 0 {
		template, _ = sjson.SetBytes(template, "created", (*param).(*ConvertAnthropicResponseToOpenAIParams).CreatedAt)
	}

	switch eventType {
	case "message_start":
		// Initialize response with message metadata when a new message begins
		if message := root.Get("message"); message.Exists() {
			(*param).(*ConvertAnthropicResponseToOpenAIParams).ResponseID = message.Get("id").String()
			(*param).(*ConvertAnthropicResponseToOpenAIParams).CreatedAt = time.Now().Unix()

			template, _ = sjson.SetBytes(template, "id", (*param).(*ConvertAnthropicResponseToOpenAIParams).ResponseID)
			template, _ = sjson.SetBytes(template, "model", modelName)
			template, _ = sjson.SetBytes(template, "created", (*param).(*ConvertAnthropicResponseToOpenAIParams).CreatedAt)

			// Set initial role to assistant for the response
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")

			// Initialize tool calls accumulator for tracking tool call progress
			if (*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator == nil {
				(*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator = make(map[int]*ToolCallAccumulator)
			}
			(*param).(*ConvertAnthropicResponseToOpenAIParams).Usage.Merge(message.Get("usage"))
		}
		return [][]byte{template}

	case "content_block_start":
		// Start of a content block (text, tool use, or reasoning)
		if contentBlock := root.Get("content_block"); contentBlock.Exists() {
			blockType := contentBlock.Get("type").String()

			switch blockType {
			case "tool_use":
				// Start of tool call - initialize accumulator to track arguments
				toolCallID := contentBlock.Get("id").String()
				toolName := contentBlock.Get("name").String()
				index := int(root.Get("index").Int())

				if (*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator == nil {
					(*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator = make(map[int]*ToolCallAccumulator)
				}

				(*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator[index] = &ToolCallAccumulator{
					ID:   toolCallID,
					Name: toolName,
				}

				// Don't output anything yet - wait for complete tool call
				return [][]byte{}
			case "thinking", "redacted_thinking":
				if (*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator == nil {
					(*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator = make(map[int]*ReasoningAccumulator)
				}
				index := int(root.Get("index").Int())
				accumulator := &ReasoningAccumulator{Type: blockType, Data: contentBlock.Get("data").String()}
				accumulator.Text.WriteString(contentBlock.Get("thinking").String())
				accumulator.Signature.WriteString(contentBlock.Get("signature").String())
				(*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator[index] = accumulator
				return [][]byte{}
			}
		}
		return [][]byte{}

	case "content_block_delta":
		// Handle content delta (text, tool use arguments, or reasoning content)
		hasContent := false
		if delta := root.Get("delta"); delta.Exists() {
			deltaType := delta.Get("type").String()

			switch deltaType {
			case "text_delta":
				// Text content delta - send incremental text updates
				if text := delta.Get("text"); text.Exists() {
					template, _ = sjson.SetBytes(template, "choices.0.delta.content", text.String())
					hasContent = true
				}
			case "thinking_delta":
				// Accumulate reasoning/thinking content
				if thinking := delta.Get("thinking"); thinking.Exists() {
					index := int(root.Get("index").Int())
					if accumulator := (*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator[index]; accumulator != nil {
						accumulator.Text.WriteString(thinking.String())
					}
					template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", thinking.String())
					hasContent = true
				}
			case "signature_delta":
				if signature := delta.Get("signature"); signature.Exists() {
					index := int(root.Get("index").Int())
					if accumulator := (*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator[index]; accumulator != nil {
						accumulator.Signature.WriteString(signature.String())
					}
				}
				return [][]byte{}
			case "input_json_delta":
				// Tool use input delta - accumulate arguments for tool calls
				if partialJSON := delta.Get("partial_json"); partialJSON.Exists() {
					index := int(root.Get("index").Int())
					if (*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator != nil {
						if accumulator, exists := (*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator[index]; exists {
							accumulator.Arguments.WriteString(partialJSON.String())
						}
					}
				}
				// Don't output anything yet - wait for complete tool call
				return [][]byte{}
			}
		}
		if hasContent {
			return [][]byte{template}
		} else {
			return [][]byte{}
		}

	case "content_block_stop":
		// End of content block - output complete tool call if it's a tool_use block
		index := int(root.Get("index").Int())
		if (*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator != nil {
			if accumulator, exists := (*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator[index]; exists {
				var item []byte
				if accumulator.Type == "redacted_thinking" {
					item = []byte(`{"type":"redacted_thinking","data":""}`)
					item, _ = sjson.SetBytes(item, "data", accumulator.Data)
				} else {
					item = []byte(`{"type":"thinking","text":""}`)
					item, _ = sjson.SetBytes(item, "text", accumulator.Text.String())
					if accumulator.Signature.Len() > 0 {
						item, _ = sjson.SetBytes(item, "signature", accumulator.Signature.String())
					}
				}
				template, _ = sjson.SetRawBytes(template, "choices.0.delta.reasoning", []byte(`[]`))
				template, _ = sjson.SetRawBytes(template, "choices.0.delta.reasoning.-1", item)
				delete((*param).(*ConvertAnthropicResponseToOpenAIParams).ReasoningAccumulator, index)
				return [][]byte{template}
			}
		}
		if (*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator != nil {
			if accumulator, exists := (*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator[index]; exists {
				// Build complete tool call with accumulated arguments
				arguments := accumulator.Arguments.String()
				if arguments == "" {
					arguments = "{}"
				}
				template, _ = sjson.SetBytes(template, "choices.0.delta.tool_calls.0.index", index)
				template, _ = sjson.SetBytes(template, "choices.0.delta.tool_calls.0.id", accumulator.ID)
				template, _ = sjson.SetBytes(template, "choices.0.delta.tool_calls.0.type", "function")
				template, _ = sjson.SetBytes(template, "choices.0.delta.tool_calls.0.function.name", accumulator.Name)
				template, _ = sjson.SetBytes(template, "choices.0.delta.tool_calls.0.function.arguments", arguments)

				// Clean up the accumulator for this index
				delete((*param).(*ConvertAnthropicResponseToOpenAIParams).ToolCallsAccumulator, index)

				return [][]byte{template}
			}
		}
		return [][]byte{}

	case "message_delta":
		// Handle message-level changes including stop reason and usage
		if delta := root.Get("delta"); delta.Exists() {
			if stopReason := delta.Get("stop_reason"); stopReason.Exists() {
				(*param).(*ConvertAnthropicResponseToOpenAIParams).FinishReason = mapAnthropicStopReasonToOpenAI(stopReason.String())
				template, _ = sjson.SetBytes(template, "choices.0.finish_reason", (*param).(*ConvertAnthropicResponseToOpenAIParams).FinishReason)
			}
		}

		// Handle usage information for token counts
		if usage := root.Get("usage"); usage.Exists() {
			(*param).(*ConvertAnthropicResponseToOpenAIParams).Usage.Merge(usage)
			promptTokens, completionTokens, totalTokens, cachedTokens := (*param).(*ConvertAnthropicResponseToOpenAIParams).Usage.OpenAIUsage()
			template, _ = sjson.SetBytes(template, "usage.prompt_tokens", promptTokens)
			template, _ = sjson.SetBytes(template, "usage.completion_tokens", completionTokens)
			template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokens)
			template, _ = sjson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", cachedTokens)
			if cacheCreationTokens := (*param).(*ConvertAnthropicResponseToOpenAIParams).Usage.CacheCreationInputTokens; cacheCreationTokens > 0 {
				template, _ = sjson.SetBytes(template, "usage.cache_creation_input_tokens", cacheCreationTokens)
			}
			if reasoningTokens := (*param).(*ConvertAnthropicResponseToOpenAIParams).Usage.ReasoningTokens; reasoningTokens > 0 {
				template, _ = sjson.SetBytes(template, "usage.completion_tokens_details.reasoning_tokens", reasoningTokens)
			}
		}
		return [][]byte{template}

	case "message_stop":
		if (*param).(*ConvertAnthropicResponseToOpenAIParams).Done {
			return [][]byte{}
		}
		(*param).(*ConvertAnthropicResponseToOpenAIParams).Done = true
		return [][]byte{[]byte("[DONE]")}

	case "ping":
		// Ping events for keeping connection alive - no output needed
		return [][]byte{}

	case "error":
		// Error event - format and return error response
		if errorData := root.Get("error"); errorData.Exists() {
			errorJSON := []byte(`{"error":{"message":"","type":""}}`)
			errorJSON, _ = sjson.SetBytes(errorJSON, "error.message", errorData.Get("message").String())
			errorJSON, _ = sjson.SetBytes(errorJSON, "error.type", errorData.Get("type").String())
			return [][]byte{errorJSON}
		}
		return [][]byte{}

	default:
		// Unknown event type - ignore
		return [][]byte{}
	}
}

// mapAnthropicStopReasonToOpenAI maps Anthropic stop reasons to OpenAI stop reasons
func mapAnthropicStopReasonToOpenAI(anthropicReason string) string {
	switch anthropicReason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

// ConvertClaudeResponseToOpenAINonStream converts a non-streaming Claude Code response to a non-streaming OpenAI response.
// This function processes the complete Claude Code response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Claude Code API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertClaudeResponseToOpenAINonStream(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	_ = originalRequestRawJSON
	_ = requestRawJSON

	root := gjson.ParseBytes(rawJSON)
	if root.Get("type").String() != "message" || !root.Get("content").IsArray() {
		return nil
	}

	out := []byte(`{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", root.Get("id").String())
	out, _ = sjson.SetBytes(out, "created", time.Now().Unix())
	responseModel := root.Get("model").String()
	if responseModel == "" {
		responseModel = modelName
	}
	out, _ = sjson.SetBytes(out, "model", responseModel)

	var content strings.Builder
	var reasoningContent strings.Builder
	reasoning := []byte(`{"items":[]}`)
	toolCallCount := 0

	for _, block := range root.Get("content").Array() {
		switch block.Get("type").String() {
		case "text":
			content.WriteString(block.Get("text").String())
		case "thinking":
			thinking := block.Get("thinking").String()
			reasoningContent.WriteString(thinking)
			item := []byte(`{"type":"thinking","text":""}`)
			item, _ = sjson.SetBytes(item, "text", thinking)
			if signature := block.Get("signature"); signature.Exists() && signature.String() != "" {
				item, _ = sjson.SetBytes(item, "signature", signature.String())
			}
			reasoning, _ = sjson.SetRawBytes(reasoning, "items.-1", item)
		case "redacted_thinking":
			item := []byte(`{"type":"redacted_thinking","data":""}`)
			item, _ = sjson.SetBytes(item, "data", block.Get("data").String())
			reasoning, _ = sjson.SetRawBytes(reasoning, "items.-1", item)
		case "tool_use":
			arguments := block.Get("input").Raw
			if !gjson.Valid(arguments) || !block.Get("input").IsObject() {
				arguments = "{}"
			}
			base := fmt.Sprintf("choices.0.message.tool_calls.%d", toolCallCount)
			out, _ = sjson.SetBytes(out, base+".id", block.Get("id").String())
			out, _ = sjson.SetBytes(out, base+".type", "function")
			out, _ = sjson.SetBytes(out, base+".function.name", block.Get("name").String())
			out, _ = sjson.SetBytes(out, base+".function.arguments", arguments)
			toolCallCount++
		default:
			return nil
		}
	}

	out, _ = sjson.SetBytes(out, "choices.0.message.content", content.String())
	if reasoningContent.Len() > 0 {
		out, _ = sjson.SetBytes(out, "choices.0.message.reasoning_content", reasoningContent.String())
	}
	if items := gjson.GetBytes(reasoning, "items"); items.IsArray() && len(items.Array()) > 0 {
		out, _ = sjson.SetRawBytes(out, "choices.0.message.reasoning", []byte(items.Raw))
	}

	stopReason := mapAnthropicStopReasonToOpenAI(root.Get("stop_reason").String())
	if toolCallCount > 0 {
		stopReason = "tool_calls"
	}
	out, _ = sjson.SetBytes(out, "choices.0.finish_reason", stopReason)

	usage := claudeUsageTokens{}
	usage.Merge(root.Get("usage"))
	if usage.HasUsage {
		promptTokens, completionTokens, totalTokens, cachedTokens := usage.OpenAIUsage()
		out, _ = sjson.SetBytes(out, "usage.prompt_tokens", promptTokens)
		out, _ = sjson.SetBytes(out, "usage.completion_tokens", completionTokens)
		out, _ = sjson.SetBytes(out, "usage.total_tokens", totalTokens)
		if cachedTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.prompt_tokens_details.cached_tokens", cachedTokens)
		}
		if usage.CacheCreationInputTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.cache_creation_input_tokens", usage.CacheCreationInputTokens)
		}
		if usage.ReasoningTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.completion_tokens_details.reasoning_tokens", usage.ReasoningTokens)
		}
	}

	return out
}
