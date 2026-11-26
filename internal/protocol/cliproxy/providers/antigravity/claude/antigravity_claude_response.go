// Package claude provides response translation functionality for Claude Code API compatibility.
// This package handles the conversion of backend client responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	geminicommon "ccLoad/internal/protocol/cliproxy/gemini/common"
	sigcompat "ccLoad/internal/protocol/cliproxy/signature"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// decodeSignature decodes R... (2-layer Base64) to E... (1-layer Base64, Anthropic format).
// Returns empty string if decoding fails (skip invalid signatures).
func decodeSignature(signature string) string {
	if signature == "" {
		return signature
	}
	if strings.HasPrefix(signature, "R") {
		decoded, err := base64.StdEncoding.DecodeString(signature)
		if err != nil {
			return ""
		}
		return string(decoded)
	}
	return signature
}

func formatGeminiClaudeCarrierValue(modelName, signature, direction, targetKind string) string {
	if sigcompat.SignatureProviderFromModelName(modelName) == sigcompat.SignatureProviderGemini {
		return encodeGeminiClaudeCarrierSignature(signature, direction, targetKind)
	}
	return formatClaudeSignatureValue(modelName, signature)
}

func formatClaudeSignatureValue(modelName, signature string) string {
	// Gemini signatures are provider-native replay state. Keep them raw so an
	// empty detached thinking block or tool_use block can round-trip through
	// Claude Code and be recognized by the Gemini request translator.
	if antigravityModelGroup(modelName) == "gemini" {
		return signature
	}
	if antigravityModelGroup(modelName) == "claude" {
		return decodeSignature(signature)
	}
	return signature
}

// Params holds parameters for response conversion and maintains state across streaming chunks.
// This structure tracks the current state of the response translation process to ensure
// proper sequencing of SSE events and transitions between different content types.
type Params struct {
	ModelName               string // Resolved once from the translated request
	WebSearchStreamMode     bool   // Resolved once from both request formats
	RequestMetadataResolved bool   // Also caches empty model names and disabled web search
	HasFirstResponse        bool   // Indicates if the initial message_start event has been sent
	ResponseType            int    // Current response type: 0=none, 1=content, 2=thinking, 3=function
	ResponseIndex           int    // Index counter for content blocks in the streaming response
	FinishReason            string // The finish reason string returned by the provider
	Usage                   translatorcommon.GeminiUsageSnapshot
	HasUsageMetadata        bool // Standard usage has been reported, independently of CPA usage.
	HasSentFinalEvents      bool // Indicates if final content/message events have been sent
	HasToolUse              bool // Indicates if tool use was observed in the stream
	HasVisibleOutput        bool // Tracks non-whitespace text and tool use separately from thinking and signatures
	HasContent              bool // Tracks whether any content (text, thinking, or tool use) has been output
	HasSemanticContent      bool
	LastSemanticKind        string
	HasWebSearchTool        bool
	WebSearchRequests       int64
	WebSearchTextBuffer     strings.Builder

	// Signature caching support
	CurrentThinkingText   strings.Builder // Accumulates thinking text for signature caching
	CurrentThinkingSigned bool            // Tracks whether the active thinking block already has its terminal signature

	// Reverse map: sanitized Gemini function name → original Claude tool name.
	// Populated lazily on the first response chunk from the original request JSON.
	ToolNameMap map[string]string

	CurrentToolName              string // Client-facing name of the currently open tool_use block
	CurrentToolIsParameterless   bool   // Whether the open tool_use block was declared parameterless
	CurrentToolInput             translatorcommon.ClaudeToolInput
	PendingToolThoughtSignatures []string
}

// toolUseIDCounter provides a process-wide unique counter for tool use identifiers.
var toolUseIDCounter uint64

const antigravityMalformedFunctionCallErrorPayload = `{"type":"error","error":{"type":"api_error","message":"Gemini response finished with MALFORMED_FUNCTION_CALL"}}`

func isAntigravityMalformedFunctionCall(finishReason string, hasVisibleOutput bool) bool {
	return strings.EqualFold(strings.TrimSpace(finishReason), "MALFORMED_FUNCTION_CALL") && !hasVisibleOutput
}

func isAntigravityNonEmptyFunctionCall(fc gjson.Result) bool {
	if !fc.IsObject() {
		return false
	}
	if strings.TrimSpace(fc.Get("name").String()) != "" || strings.TrimSpace(fc.Get("id").String()) != "" {
		return true
	}
	if args := fc.Get("args"); args.IsObject() && len(args.Map()) > 0 {
		return true
	}
	return false
}

func antigravityClaudeToolUseID(modelName string, functionCall gjson.Result, fallback string) string {
	if sigcompat.SignatureProviderFromModelName(modelName) == sigcompat.SignatureProviderGemini {
		if stableID := util.GeminiClaudeToolUseID(functionCall.Get("id").String(), functionCall.Get("name").String(), functionCall.Get("args").Raw); stableID != "" {
			return stableID
		}
	}
	return util.SanitizeClaudeToolID(fallback)
}

// ConvertAntigravityResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates backend client responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Antigravity API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of bytes, each containing a Claude Code-compatible SSE payload.
func ConvertAntigravityResponseToClaude(ctx context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &Params{
			HasFirstResponse: false,
			ResponseType:     0,
			ResponseIndex:    0,
			ToolNameMap:      util.DisambiguatedToolNameMap(originalRequestRawJSON),
		}
	}
	params := (*param).(*Params)
	// Request metadata is resolved once per stream; callers pass the same
	// request bytes for the stream's lifetime.
	if !params.RequestMetadataResolved {
		params.ModelName = gjson.GetBytes(requestRawJSON, "model").String()
		params.WebSearchStreamMode = shouldTranslateWebSearchGrounding(originalRequestRawJSON, requestRawJSON)
		params.RequestMetadataResolved = true
	}
	modelName := params.ModelName
	if params.HasSentFinalEvents {
		return [][]byte{}
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		output := make([]byte, 0, 256)
		if params.WebSearchStreamMode && !params.HasWebSearchTool && params.WebSearchTextBuffer.Len() > 0 {
			appendBufferedWebSearchTextBlock(params, func(event, payload string) {
				output = translatorcommon.AppendSSEEventString(output, event, payload, 3)
			})
		}
		if params.HasFirstResponse && !params.HasContent {
			output = translatorcommon.AppendSSEEventString(output, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, params.ResponseIndex), 3)
			params.ResponseType = 1
			params.HasContent = true
		}
		if params.HasContent {
			appendFinalEvents(params, &output)
			if len(output) == 0 {
				return [][]byte{}
			}
			return [][]byte{output}
		}
		return [][]byte{}
	}

	output := make([]byte, 0, 1024)
	appendEvent := func(event, payload string) {
		output = translatorcommon.AppendSSEEventString(output, event, payload, 3)
	}
	webSearchStreamMode := params.WebSearchStreamMode
	appendThinkingSignature := func(signature, direction, targetKind string) {
		if signature == "" || params.ResponseType != 2 {
			return
		}
		params.CurrentThinkingText.Reset()
		sigValue := formatGeminiClaudeCarrierValue(modelName, signature, direction, targetKind)
		data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`, params.ResponseIndex)), "delta.signature", sigValue)
		appendEvent("content_block_delta", string(data))
		params.CurrentThinkingSigned = true
		params.HasContent = true
	}
	closeCurrentBlock := func() {
		params.closeCurrentBlock(appendEvent)
	}
	startEmptyThinkingBlock := func() {
		appendEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, params.ResponseIndex))
		params.ResponseType = 2
		params.CurrentThinkingSigned = false
		params.HasContent = true
	}
	appendCarrierSignature := func(signature, direction, targetKind string) {
		if signature == "" || params.ResponseType != 2 {
			return
		}
		sigValue := formatGeminiClaudeCarrierValue(modelName, signature, direction, targetKind)
		data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`, params.ResponseIndex)), "delta.signature", sigValue)
		appendEvent("content_block_delta", string(data))
		params.CurrentThinkingSigned = true
		params.HasContent = true
	}
	appendPartSignature := func(signature, direction, targetKind string) bool {
		if signature == "" {
			return false
		}
		if params.ResponseType == 2 && !params.CurrentThinkingSigned {
			appendThinkingSignature(signature, direction, targetKind)
			return false
		}
		closeCurrentBlock()
		startEmptyThinkingBlock()
		appendCarrierSignature(signature, direction, targetKind)
		return true
	}

	// Initialize the streaming session with a message_start event
	// This is only sent for the very first response chunk to establish the streaming session
	if !params.HasFirstResponse {
		// Create the initial message structure with default values according to Claude Code API specification
		// This follows the Claude Code API specification for streaming message initialization
		messageStartTemplate := []byte(`{"type": "message_start", "message": {"id": "", "type": "message", "role": "assistant", "content": [], "model": "claude-3-5-sonnet-20241022", "stop_reason": null, "stop_sequence": null, "usage": {"input_tokens": 0, "output_tokens": 0}}}`)

		// Use cpaUsageMetadata within the message_start event for Claude.
		if promptTokenCount := gjson.GetBytes(rawJSON, "response.cpaUsageMetadata.promptTokenCount"); promptTokenCount.Type == gjson.Number {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.usage.input_tokens", promptTokenCount.Int())
		}
		if candidatesTokenCount := gjson.GetBytes(rawJSON, "response.cpaUsageMetadata.candidatesTokenCount"); candidatesTokenCount.Type == gjson.Number && !webSearchStreamMode {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.usage.output_tokens", candidatesTokenCount.Int())
		}

		// Override default values with actual response metadata if available from the Antigravity response
		if modelVersionResult := gjson.GetBytes(rawJSON, "response.modelVersion"); modelVersionResult.Exists() {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.model", modelVersionResult.String())
		}
		messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.id",
			translatorcommon.GeminiClaudeMessageID(gjson.GetBytes(rawJSON, "response.responseId")))
		appendEvent("message_start", string(messageStartTemplate))

		params.HasFirstResponse = true
	}

	handledWebSearchGrounding := false
	if webSearchStreamMode && !params.HasWebSearchTool {
		root := gjson.ParseBytes(rawJSON)
		if groundingMetadata := antigravityGroundingMetadata(root); groundingMetadata.Exists() {
			toolUseID := newClaudeWebSearchToolUseID()
			textContent := params.WebSearchTextBuffer.String() + antigravityTextContent(root)
			params.WebSearchTextBuffer.Reset()
			params.ResponseIndex = appendClaudeWebSearchStreamBlocks(appendEvent, params.ResponseIndex, toolUseID, textContent, groundingMetadata)
			params.HasWebSearchTool = true
			params.WebSearchRequests = 1
			params.HasContent = true
			params.HasVisibleOutput = true
			params.ResponseType = 0
			handledWebSearchGrounding = true
		}
	}

	// Process the response parts array from the backend client
	// Each part can contain text content, thinking content, or function calls
	partsResult := gjson.GetBytes(rawJSON, "response.candidates.0.content.parts")
	if partsResult.IsArray() && webSearchStreamMode && !params.HasWebSearchTool && !handledWebSearchGrounding {
		appendWebSearchBufferedText(partsResult, &params.WebSearchTextBuffer)
	} else if partsResult.IsArray() && !handledWebSearchGrounding {
		partResults := partsResult.Array()
		for i := 0; i < len(partResults); i++ {
			partResult := partResults[i]

			// Extract the different types of content from each part
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")
			signature := geminicommon.ThoughtSignature(partResult)
			hasThoughtSignature := signature != "" && !functionCallResult.Exists()

			if hasThoughtSignature && (!partTextResult.Exists() || partTextResult.String() == "") {
				if params.ResponseType == 3 {
					params.PendingToolThoughtSignatures = append(params.PendingToolThoughtSignatures, signature)
					continue
				}
				direction := geminiClaudeCarrierNext
				targetKind := geminiClaudeCarrierAny
				if params.HasSemanticContent {
					direction = geminiClaudeCarrierPrevious
					targetKind = params.LastSemanticKind
				}
				appendPartSignature(signature, direction, targetKind)
				continue
			}

			// Handle text content (both regular content and thinking)
			if partTextResult.Exists() {
				partText := partTextResult.String()
				if partResult.Get("thought").Bool() {
					if partText != "" {
						params.HasSemanticContent = true
						params.LastSemanticKind = geminiClaudeCarrierText
						if params.ResponseType == 2 && params.CurrentThinkingSigned {
							closeCurrentBlock()
						}
						if params.ResponseType == 2 {
							params.CurrentThinkingText.WriteString(partText)
							data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, params.ResponseIndex)), "delta.thinking", partText)
							appendEvent("content_block_delta", string(data))
							params.HasContent = true
						} else {
							closeCurrentBlock()
							appendEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, params.ResponseIndex))
							params.CurrentThinkingSigned = false
							data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, params.ResponseIndex)), "delta.thinking", partText)
							appendEvent("content_block_delta", string(data))
							params.ResponseType = 2
							params.HasContent = true
							params.CurrentThinkingText.Reset()
							params.CurrentThinkingText.WriteString(partText)
						}
					}
					if hasThoughtSignature {
						appendThinkingSignature(signature, geminiClaudeCarrierStandalone, geminiClaudeCarrierText)
					}
				} else {
					signatureTargetsVisibleText := false
					if hasThoughtSignature {
						signatureTargetsVisibleText = appendPartSignature(signature, geminiClaudeCarrierNext, geminiClaudeCarrierText)
					}
					if params.ResponseType == 1 {
						data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, params.ResponseIndex)), "delta.text", partText)
						appendEvent("content_block_delta", string(data))
						params.HasContent = true
					} else if partText != "" {
						closeCurrentBlock()
						appendEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, params.ResponseIndex))
						data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, params.ResponseIndex)), "delta.text", partText)
						appendEvent("content_block_delta", string(data))
						params.ResponseType = 1
						params.HasContent = true
					}
					if partText != "" {
						params.HasSemanticContent = true
						params.LastSemanticKind = geminiClaudeCarrierText
						if strings.TrimSpace(partText) != "" {
							params.HasVisibleOutput = true
						}
						if signatureTargetsVisibleText {
							closeCurrentBlock()
						}
					}
				}
			} else if functionCallResult.Exists() {
				toolSignature := signature
				fcName := util.RestoreSanitizedToolName(params.ToolNameMap, functionCallResult.Get("name").String())
				isContinuation := params.ResponseType == 3 && fcName == "" && params.CurrentToolName != ""
				if !isContinuation && !isAntigravityNonEmptyFunctionCall(functionCallResult) {
					if toolSignature != "" {
						appendPartSignature(toolSignature, geminiClaudeCarrierPrevious, params.LastSemanticKind)
					}
					continue
				}
				if !isContinuation && antigravityModelGroup(modelName) != "claude" {
					appendPartSignature(toolSignature, geminiClaudeCarrierNext, geminiClaudeCarrierFunction)
				}
				// Handle function/tool calls from the AI model
				// This processes tool usage requests and formats them for Claude Code API compatibility
				params.HasToolUse = true
				params.HasVisibleOutput = true

				if isContinuation {
					if toolSignature != "" {
						params.PendingToolThoughtSignatures = append(params.PendingToolThoughtSignatures, toolSignature)
					}
					if args := functionCallResult.Get("args"); args.Exists() {
						cleaned := util.CleanClaudeToolArgumentsWithTranslated(args.Raw, params.CurrentToolName, originalRequestRawJSON, requestRawJSON)
						params.emitToolInput(appendEvent, params.CurrentToolInput.Append(cleaned, params.CurrentToolIsParameterless))
					}
					continue
				}

				closeCurrentBlock()

				params.CurrentToolName = fcName
				params.CurrentToolIsParameterless = util.ClaudeToolIsParameterless(fcName, originalRequestRawJSON)
				// Start a new tool use content block
				// This creates the structure for a function call in Claude Code format
				// Create the tool use block with unique ID and function details
				data := []byte(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`, params.ResponseIndex))
				fallbackID := fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&toolUseIDCounter, 1))
				data, _ = sjson.SetBytes(data, "content_block.id", antigravityClaudeToolUseID(modelName, functionCallResult, fallbackID))
				data, _ = sjson.SetBytes(data, "content_block.name", fcName)
				if antigravityModelGroup(modelName) == "claude" && toolSignature != "" {
					data, _ = sjson.SetBytes(data, "content_block.signature", formatClaudeSignatureValue(modelName, toolSignature))
				}
				appendEvent("content_block_start", string(data))

				if args := functionCallResult.Get("args"); args.Exists() {
					cleaned := util.CleanClaudeToolArgumentsWithTranslated(args.Raw, fcName, originalRequestRawJSON, requestRawJSON)
					params.emitToolInput(appendEvent, params.CurrentToolInput.Append(cleaned, params.CurrentToolIsParameterless))
				}
				params.ResponseType = 3
				params.HasContent = true
				params.HasSemanticContent = true
				params.LastSemanticKind = geminiClaudeCarrierFunction
			}
		}
	}

	finishReason := strings.TrimSpace(gjson.GetBytes(rawJSON, "response.candidates.0.finishReason").Str)
	if finishReason != "" {
		params.FinishReason = finishReason
	}

	hasUsage := params.Usage.Merge(gjson.GetBytes(rawJSON, "response.usageMetadata"))
	if hasUsage {
		params.HasUsageMetadata = true
	} else {
		params.Usage.Merge(gjson.GetBytes(rawJSON, "response.cpaUsageMetadata"))
	}

	if webSearchStreamMode && !params.HasWebSearchTool && params.FinishReason != "" && params.WebSearchTextBuffer.Len() > 0 {
		appendBufferedWebSearchTextBlock(params, appendEvent)
	}

	if isAntigravityMalformedFunctionCall(params.FinishReason, params.HasVisibleOutput) && !params.HasSentFinalEvents {
		closeCurrentBlock()
		appendEvent("error", antigravityMalformedFunctionCallErrorPayload)
		params.HasSentFinalEvents = true
		return [][]byte{output}
	}

	// Split terminal metadata is enough for plain text. Tool and search streams
	// can still receive parameters, signatures or grounding after those frames.
	textFinished := params.ResponseType == 1 && !params.HasToolUse && !webSearchStreamMode &&
		params.HasUsageMetadata && params.FinishReason != ""
	if (hasUsage && finishReason != "") || textFinished {
		appendFinalEvents(params, &output)
	}

	return [][]byte{output}
}

func (params *Params) emitToolInput(appendEvent func(string, string), partialJSON string) {
	if partialJSON == "" {
		return
	}
	data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, params.ResponseIndex)), "delta.partial_json", partialJSON)
	appendEvent("content_block_delta", string(data))
}

func (params *Params) closeCurrentBlock(appendEvent func(string, string)) {
	if params.ResponseType == 0 {
		return
	}
	if params.ResponseType == 3 {
		params.emitToolInput(appendEvent, params.CurrentToolInput.Finish())
	}
	appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, params.ResponseIndex))
	params.ResponseIndex++
	params.ResponseType = 0
	params.CurrentThinkingSigned = false
	for _, signature := range params.PendingToolThoughtSignatures {
		appendEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, params.ResponseIndex))
		sigValue := formatGeminiClaudeCarrierValue(params.ModelName, signature, geminiClaudeCarrierPrevious, geminiClaudeCarrierFunction)
		data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`, params.ResponseIndex)), "delta.signature", sigValue)
		appendEvent("content_block_delta", string(data))
		appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, params.ResponseIndex))
		params.ResponseIndex++
	}
	params.PendingToolThoughtSignatures = nil
}

func appendWebSearchBufferedText(partsResult gjson.Result, buffer *strings.Builder) {
	for _, partResult := range partsResult.Array() {
		if partResult.Get("thought").Bool() || partResult.Get("functionCall").Exists() {
			continue
		}
		if partTextResult := partResult.Get("text"); partTextResult.Exists() {
			buffer.WriteString(partTextResult.String())
		}
	}
}

func appendBufferedWebSearchTextBlock(params *Params, appendEvent func(string, string)) {
	text := params.WebSearchTextBuffer.String()
	params.WebSearchTextBuffer.Reset()
	if text == "" {
		return
	}
	appendEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, params.ResponseIndex))
	data, _ := sjson.SetBytes([]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, params.ResponseIndex)), "delta.text", text)
	appendEvent("content_block_delta", string(data))
	params.ResponseType = 1
	params.HasContent = true
	if strings.TrimSpace(text) != "" {
		params.HasVisibleOutput = true
	}
}

func appendFinalEvents(params *Params, output *[]byte) {
	if params.HasSentFinalEvents {
		return
	}

	// Only send final events if we have actually output content
	if !params.HasContent {
		return
	}

	params.closeCurrentBlock(func(event, payload string) {
		*output = translatorcommon.AppendSSEEventString(*output, event, payload, 3)
	})

	stopReason := resolveStopReason(params.FinishReason, params.HasToolUse)
	usageOutputTokens := params.Usage.ClaudeOutputTokens()
	if params.Usage.Candidates == 0 {
		usageOutputTokens = max(usageOutputTokens, params.Usage.Total-params.Usage.Prompt)
	}

	delta := []byte(fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`, stopReason, params.Usage.ClaudeInputTokens(), usageOutputTokens))
	if params.WebSearchRequests > 0 {
		delta, _ = sjson.SetBytes(delta, "usage.server_tool_use.web_search_requests", params.WebSearchRequests)
	}
	// Add cache_read_input_tokens if cached tokens are present (indicates prompt caching is working)
	if params.Usage.Cached > 0 {
		delta, _ = sjson.SetBytes(delta, "usage.cache_read_input_tokens", params.Usage.Cached)
	}
	*output = translatorcommon.AppendSSEEventString(*output, "message_delta", string(delta), 3)
	*output = translatorcommon.AppendSSEEventString(*output, "message_stop", `{"type":"message_stop"}`, 3)

	params.HasSentFinalEvents = true
}

func resolveStopReason(finishReason string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}

	switch strings.ToUpper(strings.TrimSpace(finishReason)) {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "IMAGE_SAFETY", "MALFORMED_RESPONSE":
		return "refusal"
	}

	return "end_turn"
}

// ConvertAntigravityResponseToClaudeNonStream converts a non-streaming Antigravity response to a non-streaming Claude response.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the Antigravity API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - []byte: A Claude-compatible JSON response.
func ConvertAntigravityResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	toolNameMap := util.DisambiguatedToolNameMap(originalRequestRawJSON)
	modelName := gjson.GetBytes(requestRawJSON, "model").String()

	root := gjson.ParseBytes(rawJSON)
	var usage translatorcommon.GeminiUsageSnapshot
	hasUsage := usage.Merge(root.Get("response.usageMetadata")) || usage.Merge(root.Get("response.cpaUsageMetadata"))

	responseJSON := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	responseJSON, _ = sjson.SetBytes(responseJSON, "id", translatorcommon.GeminiClaudeMessageID(root.Get("response.responseId")))
	responseJSON, _ = sjson.SetBytes(responseJSON, "model", root.Get("response.modelVersion").String())
	if hasUsage {
		responseJSON, _ = sjson.SetBytes(responseJSON, "usage.input_tokens", usage.ClaudeInputTokens())
		responseJSON, _ = sjson.SetBytes(responseJSON, "usage.output_tokens", usage.ClaudeOutputTokens())
		if usage.Cached > 0 {
			responseJSON, _ = sjson.SetBytes(responseJSON, "usage.cache_read_input_tokens", usage.Cached)
		}
	}

	if shouldTranslateWebSearchGrounding(originalRequestRawJSON, requestRawJSON) {
		if groundingMetadata := antigravityGroundingMetadata(root); groundingMetadata.Exists() {
			toolUseID := newClaudeWebSearchToolUseID()
			responseJSON, _ = sjson.SetRawBytes(responseJSON, "content", buildClaudeWebSearchContent(toolUseID, antigravityTextContent(root), groundingMetadata))
			responseJSON, _ = sjson.SetBytes(responseJSON, "stop_reason", resolveStopReason(root.Get("response.candidates.0.finishReason").String(), false))
			responseJSON, _ = sjson.SetBytes(responseJSON, "usage.server_tool_use.web_search_requests", 1)
			return responseJSON
		}
	}

	var blocks [][]byte

	parts := root.Get("response.candidates.0.content.parts")
	textBuilder := strings.Builder{}
	thinkingBuilder := strings.Builder{}
	thinkingSignature := ""
	thinkingSignatureDirection := geminiClaudeCarrierStandalone
	thinkingSignatureTargetKind := geminiClaudeCarrierText
	toolIDCounter := 0
	hasToolCall := false
	hasVisibleOutput := false
	hasSemanticContent := false
	lastSemanticKind := geminiClaudeCarrierAny

	flushText := func() {
		if textBuilder.Len() == 0 {
			return
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", textBuilder.String())
		blocks = append(blocks, block)
		textBuilder.Reset()
	}

	flushThinking := func() {
		if thinkingBuilder.Len() == 0 && thinkingSignature == "" {
			return
		}
		block := []byte(`{"type":"thinking","thinking":""}`)
		block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
		if thinkingSignature != "" {
			sigValue := formatGeminiClaudeCarrierValue(modelName, thinkingSignature, thinkingSignatureDirection, thinkingSignatureTargetKind)
			block, _ = sjson.SetBytes(block, "signature", sigValue)
		}
		blocks = append(blocks, block)
		thinkingBuilder.Reset()
		thinkingSignature = ""
		thinkingSignatureDirection = geminiClaudeCarrierStandalone
		thinkingSignatureTargetKind = geminiClaudeCarrierText
	}

	appendSignatureCarrier := func(signature, direction, targetKind string) {
		if signature == "" {
			return
		}
		carrier := []byte(`{"type":"thinking","thinking":"","signature":""}`)
		carrier, _ = sjson.SetBytes(carrier, "signature", formatGeminiClaudeCarrierValue(modelName, signature, direction, targetKind))
		blocks = append(blocks, carrier)
	}

	if parts.IsArray() {
		for _, part := range parts.Array() {
			signature := geminicommon.ThoughtSignature(part)

			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				if !isAntigravityNonEmptyFunctionCall(functionCall) {
					if signature != "" {
						appendSignatureCarrier(signature, geminiClaudeCarrierPrevious, lastSemanticKind)
					}
					continue
				}
				signatureAttachedToThought := false
				isClaudeTarget := antigravityModelGroup(modelName) == "claude"
				if !isClaudeTarget && signature != "" && thinkingBuilder.Len() > 0 && thinkingSignature == "" {
					thinkingSignature = signature
					thinkingSignatureDirection = geminiClaudeCarrierNext
					thinkingSignatureTargetKind = geminiClaudeCarrierFunction
					signatureAttachedToThought = true
				}
				flushThinking()
				flushText()
				hasToolCall = true
				hasVisibleOutput = true

				name := util.RestoreSanitizedToolName(toolNameMap, functionCall.Get("name").String())
				toolIDCounter++
				if !isClaudeTarget && signature != "" && !signatureAttachedToThought {
					appendSignatureCarrier(signature, geminiClaudeCarrierNext, geminiClaudeCarrierFunction)
				}
				toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolBlock, _ = sjson.SetBytes(toolBlock, "id", antigravityClaudeToolUseID(modelName, functionCall, fmt.Sprintf("tool_%d", toolIDCounter)))
				toolBlock, _ = sjson.SetBytes(toolBlock, "name", name)
				if isClaudeTarget && signature != "" {
					toolBlock, _ = sjson.SetBytes(toolBlock, "signature", formatClaudeSignatureValue(modelName, signature))
				}

				if args := functionCall.Get("args"); args.Exists() && args.Raw != "" && gjson.Valid(args.Raw) && args.IsObject() {
					cleanArgs := util.CleanClaudeToolArgumentsWithTranslated(args.Raw, name, originalRequestRawJSON, requestRawJSON)
					toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(cleanArgs))
				}

				blocks = append(blocks, toolBlock)
				hasSemanticContent = true
				lastSemanticKind = geminiClaudeCarrierFunction
				continue
			}

			text := part.Get("text")
			isThought := part.Get("thought").Bool()
			if isThought {
				flushText()
				if thinkingSignature != "" {
					flushThinking()
				}
				if text.Exists() && text.String() != "" {
					thinkingBuilder.WriteString(text.String())
					hasSemanticContent = true
					lastSemanticKind = geminiClaudeCarrierText
				}
				if signature != "" {
					if thinkingBuilder.Len() > 0 {
						thinkingSignature = signature
						thinkingSignatureDirection = geminiClaudeCarrierStandalone
						thinkingSignatureTargetKind = geminiClaudeCarrierText
						flushThinking()
					} else if hasSemanticContent {
						appendSignatureCarrier(signature, geminiClaudeCarrierPrevious, lastSemanticKind)
					} else {
						appendSignatureCarrier(signature, geminiClaudeCarrierNext, geminiClaudeCarrierAny)
					}
				}
				continue
			}

			visibleSignatureCarrier := false
			if signature != "" {
				if thinkingBuilder.Len() > 0 && thinkingSignature == "" {
					thinkingSignature = signature
					thinkingSignatureDirection = geminiClaudeCarrierNext
					thinkingSignatureTargetKind = geminiClaudeCarrierText
					flushThinking()
				} else {
					flushThinking()
					flushText()
					if text.Exists() && text.String() != "" {
						appendSignatureCarrier(signature, geminiClaudeCarrierNext, geminiClaudeCarrierText)
						visibleSignatureCarrier = true
					} else if hasSemanticContent {
						appendSignatureCarrier(signature, geminiClaudeCarrierPrevious, lastSemanticKind)
					} else {
						appendSignatureCarrier(signature, geminiClaudeCarrierNext, geminiClaudeCarrierAny)
					}
				}
			}
			if text.Exists() && text.String() != "" {
				flushThinking()
				textBuilder.WriteString(text.String())
				if strings.TrimSpace(text.String()) != "" {
					hasVisibleOutput = true
				}
				hasSemanticContent = true
				lastSemanticKind = geminiClaudeCarrierText
				if visibleSignatureCarrier {
					flushText()
				}
			}
		}
	}

	flushThinking()
	flushText()

	if len(blocks) > 0 {
		responseJSON, _ = sjson.SetRawBytes(responseJSON, "content", translatorcommon.JoinRawArray(blocks))
	}

	finishReason := root.Get("response.candidates.0.finishReason").Str
	if isAntigravityMalformedFunctionCall(finishReason, hasVisibleOutput) {
		return []byte(antigravityMalformedFunctionCallErrorPayload)
	}
	stopReason := resolveStopReason(finishReason, hasToolCall)
	responseJSON, _ = sjson.SetBytes(responseJSON, "stop_reason", stopReason)

	if !hasUsage {
		responseJSON, _ = sjson.DeleteBytes(responseJSON, "usage")
	}

	return responseJSON
}

// ClaudeTokenCount builds an Anthropic token-count response.
func ClaudeTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}
