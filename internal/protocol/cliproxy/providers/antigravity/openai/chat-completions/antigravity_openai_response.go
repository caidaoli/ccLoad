// Package openai provides response translation functionality for Antigravity to OpenAI API compatibility.
// This package handles the conversion of Antigravity API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	corechat "ccLoad/internal/protocol/cliproxy/gemini/openai/chat-completions"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertCliResponseToOpenAIChatParams holds parameters for response conversion.
type convertCliResponseToOpenAIChatParams struct {
	translatorcommon.ApplyPatchErrorState
	UnixTimestamp        int64
	FunctionIndex        int
	SawResponse          bool   // Tracks whether any upstream response chunk was seen
	SawToolCall          bool   // Tracks if any tool call was seen in the entire stream
	SawFinishReason      bool   // Tracks whether finish_reason has been emitted
	UpstreamFinishReason string // Caches the upstream finish reason for final chunk
	ModelVersion         string
	ResponseID           string
	PendingUsageMetadata []byte
	LastUsageMetadata    string
	SanitizedNameMap     map[string]string
	ActiveToolCalls      map[string]*antigravityChatToolCall
}

type antigravityChatToolCall struct {
	RawName    string
	Index      int
	Assembler  *translatorcommon.VertexPartialArgsAssembler
	ObjectOpen bool
}

// functionCallIDCounter provides a process-wide unique counter for function call identifiers.
var functionCallIDCounter uint64

// ConvertAntigravityResponseToOpenAI translates a single chunk of a streaming response from the
// Antigravity API format to the OpenAI Chat Completions streaming format.
// It processes various Antigravity event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Antigravity API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func ConvertAntigravityResponseToOpenAI(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	params, ok := (*param).(*convertCliResponseToOpenAIChatParams)
	if !ok {
		params = &convertCliResponseToOpenAIChatParams{
			UnixTimestamp: 0,
			FunctionIndex: 0,
		}
		*param = params
	}
	if params.SanitizedNameMap == nil {
		params.SanitizedNameMap = util.DisambiguatedToolNameMap(originalRequestRawJSON)
	}
	if params.ToolInputError() != nil {
		return nil
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		params.FinalizeToolInput()
		if params.ToolInputError() != nil {
			return nil
		}
		// Never finalize a stream that produced no response at all: an empty
		// upstream body must stay detectable as a failure rather than be reported
		// as a successful empty completion.
		if params.SawResponse && !params.SawFinishReason {
			template := params.finishChunk(params.newChunk())
			if len(params.PendingUsageMetadata) > 0 {
				template = setOpenAIUsageMetadata(template, gjson.ParseBytes(params.PendingUsageMetadata))
			}
			return [][]byte{template}
		}
		return [][]byte{}
	}

	if !params.SawResponse {
		params.SawResponse = hasAntigravityResponsePayload(rawJSON)
	}

	// Carry the response metadata forward when sparse frames omit it.
	if modelVersionResult := gjson.GetBytes(rawJSON, "response.modelVersion"); modelVersionResult.Exists() {
		params.ModelVersion = modelVersionResult.String()
	}
	if createTimeResult := gjson.GetBytes(rawJSON, "response.createTime"); createTimeResult.Exists() {
		t, err := time.Parse(time.RFC3339Nano, createTimeResult.String())
		if err == nil {
			params.UnixTimestamp = t.Unix()
		}
	}
	if responseIDResult := gjson.GetBytes(rawJSON, "response.responseId"); responseIDResult.Exists() {
		params.ResponseID = responseIDResult.String()
	}
	template := params.newChunk()

	usageResult := gjson.GetBytes(rawJSON, "response.usageMetadata")
	if params.SawFinishReason {
		if !usageResult.Exists() || usageResult.Raw == params.LastUsageMetadata {
			return nil
		}
		params.LastUsageMetadata = strings.Clone(usageResult.Raw)
		template, _ = sjson.SetRawBytes(template, "choices", []byte(`[]`))
		return [][]byte{setOpenAIUsageMetadata(template, usageResult)}
	}

	// Sparse tool continuations can carry an empty finish reason after a terminal notice.
	if finishReason := gjson.GetBytes(rawJSON, "response.candidates.0.finishReason"); finishReason.Type == gjson.String {
		if reason := strings.TrimSpace(finishReason.String()); reason != "" {
			params.UpstreamFinishReason = strings.ToUpper(reason)
		}
	}

	// FilterSSEUsageMetadata renames
	// non-terminal usage to cpaUsageMetadata, so retain the latest copy for [DONE]
	// instead of treating an intermediate chunk as terminal.
	if !usageResult.Exists() {
		if pendingUsage := gjson.GetBytes(rawJSON, "response.cpaUsageMetadata"); pendingUsage.Exists() {
			params.PendingUsageMetadata = append(params.PendingUsageMetadata[:0], pendingUsage.Raw...)
		}
	}

	// Process the main content part of the response.
	hasDeltaContent := false
	hasNonEmptyDelta := false
	partsResult := gjson.GetBytes(rawJSON, "response.candidates.0.content.parts")
	if partsResult.IsArray() {
		partResults := partsResult.Array()
		var textContent strings.Builder
		var reasoningContent strings.Builder
		hasTextContent := false
		hasReasoningContent := false
		for i := 0; i < len(partResults); i++ {
			partResult := partResults[i]
			partTextResult := partResult.Get("text")
			functionCallResult := translatorcommon.GeminiPartFunctionCall(partResult)
			thoughtSignatureResult := partResult.Get("thoughtSignature")
			if !thoughtSignatureResult.Exists() {
				thoughtSignatureResult = partResult.Get("thought_signature")
			}
			inlineDataResult := partResult.Get("inlineData")
			if !inlineDataResult.Exists() {
				inlineDataResult = partResult.Get("inline_data")
			}

			hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
			hasTextPayload := partTextResult.Exists() && partTextResult.String() != ""
			hasContentPayload := hasTextPayload || functionCallResult.Exists() || inlineDataResult.Exists()

			// Ignore encrypted thoughtSignature but keep any actual content in the same part.
			if hasThoughtSignature && !hasContentPayload {
				continue
			}

			if partTextResult.Exists() {
				text := partTextResult.String()
				if text != "" && len(params.ActiveToolCalls) > 0 {
					params.SetToolInputError(errors.New("upstream Antigravity content interrupted incomplete tool arguments"))
					return nil
				}

				// Handle text content, distinguishing between regular content and reasoning/thoughts.
				if partResult.Get("thought").Bool() {
					hasReasoningContent = true
					reasoningContent.WriteString(text)
				} else {
					hasTextContent = true
					textContent.WriteString(text)
				}
				template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
			} else if functionCallResult.Exists() {
				call, err := params.convertToolCall(functionCallResult)
				if err != nil {
					params.SetToolInputError(err)
					return nil
				}
				if len(call) == 0 {
					continue
				}
				hasDeltaContent = true
				if !gjson.GetBytes(template, "choices.0.delta.tool_calls").IsArray() {
					template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
				}
				template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
				template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", call)
			} else if inlineDataResult.Exists() {
				if partResult.Get("thought").Bool() {
					continue
				}
				data := inlineDataResult.Get("data").String()
				if data == "" {
					continue
				}
				mimeType := inlineDataResult.Get("mimeType").String()
				if mimeType == "" {
					mimeType = inlineDataResult.Get("mime_type").String()
				}
				if mimeType == "" {
					mimeType = "image/png"
				}
				imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
				imagesResult := gjson.GetBytes(template, "choices.0.delta.images")
				if !imagesResult.Exists() || !imagesResult.IsArray() {
					template, _ = sjson.SetRawBytes(template, "choices.0.delta.images", []byte(`[]`))
				}
				imageIndex := len(gjson.GetBytes(template, "choices.0.delta.images").Array())
				imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
				imagePayload, _ = sjson.SetBytes(imagePayload, "index", imageIndex)
				imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
				hasDeltaContent = true
				template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
				template, _ = sjson.SetRawBytes(template, "choices.0.delta.images.-1", imagePayload)
			}
		}
		if hasTextContent {
			template, _ = sjson.SetBytes(template, "choices.0.delta.content", textContent.String())
		}
		if hasReasoningContent {
			template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", reasoningContent.String())
		}
		hasNonEmptyDelta = hasDeltaContent || textContent.Len() > 0 || reasoningContent.Len() > 0
		hasDeltaContent = hasDeltaContent || hasTextContent || hasReasoningContent
	}

	// Determine finish_reason only on the final chunk, which upstream marks with
	// both finishReason and usage metadata. A usage-carrying chunk without
	// finishReason must not be treated as terminal: upstream can split the two
	// across chunks and keep streaming afterwards, and finalizing early would make
	// strict clients drop the remaining content. When upstream never sends
	// finishReason at all, the [DONE] branch synthesizes the terminal chunk.
	if params.UpstreamFinishReason != "" && usageResult.Exists() && len(params.ActiveToolCalls) > 0 {
		// A tool explicitly marked as incomplete can have continuations after this frame.
		params.PendingUsageMetadata = append(params.PendingUsageMetadata[:0], usageResult.Raw...)
		usageResult = gjson.Result{}
	}
	if params.UpstreamFinishReason != "" && usageResult.Exists() {
		params.LastUsageMetadata = strings.Clone(usageResult.Raw)
		terminal := setOpenAIUsageMetadata(params.finishChunk(template), usageResult)
		if hasNonEmptyDelta {
			return [][]byte{template, terminal}
		}
		return [][]byte{terminal}
	}

	if !hasDeltaContent && !usageResult.Exists() {
		return [][]byte{}
	}
	if usageResult.Exists() {
		template = setOpenAIUsageMetadata(template, usageResult)
	}

	return [][]byte{template}
}

func (p *convertCliResponseToOpenAIChatParams) convertToolCall(fc gjson.Result) ([]byte, error) {
	rawName := strings.TrimSpace(fc.Get("name").String())
	id := strings.TrimSpace(translatorcommon.VertexFunctionCallID(fc))
	args := fc.Get("args")
	streaming := translatorcommon.IsVertexStreamingFunctionCall(fc)
	marked := streaming || fc.Get("willContinue").Type == gjson.False || fc.Get("will_continue").Type == gjson.False
	if rawName == "" && id == "" && !marked && (!args.Exists() || args.IsObject() && strings.TrimSpace(args.Raw[1:len(args.Raw)-1]) == "") {
		return nil, nil // An empty placeholder carries no continuation information.
	}
	activeID, active, err := p.findActiveToolCall(id, rawName, marked)
	if err != nil {
		return nil, err
	}
	if active != nil {
		delta, done, err := active.pushArguments(fc)
		if err != nil {
			return nil, err
		}
		if done {
			delete(p.ActiveToolCalls, activeID)
		}
		if delta == "" {
			return nil, nil
		}
		call, _ := sjson.SetBytes([]byte(`{"index":0,"function":{"arguments":""}}`), "index", active.Index)
		return translatorcommon.SetStringWithoutHTMLEscape(call, "function.arguments", delta)
	}
	if rawName == "" {
		if !streaming && !args.Exists() {
			return nil, nil // Empty placeholder, unrelated to any in-progress tool call.
		}
		return nil, errors.New("upstream Antigravity function arguments have no matching tool call")
	}

	name := util.RestoreSanitizedToolName(p.SanitizedNameMap, rawName)
	if id == "" {
		id = fmt.Sprintf("%s-%d-%d", name, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1))
	}
	call := []byte(`{"id":"","index":0,"type":"function","function":{"name":"","arguments":"{}"}}`)
	call, _ = sjson.SetBytes(call, "id", id)
	call, _ = sjson.SetBytes(call, "index", p.FunctionIndex)
	call, _ = sjson.SetBytes(call, "function.name", name)
	if streaming {
		active := &antigravityChatToolCall{RawName: rawName, Index: p.FunctionIndex}
		delta, done, err := active.pushArguments(fc)
		if err != nil {
			return nil, err
		}
		if !done {
			if p.ActiveToolCalls == nil {
				p.ActiveToolCalls = make(map[string]*antigravityChatToolCall)
			}
			p.ActiveToolCalls[id] = active
		}
		call, _ = translatorcommon.SetStringWithoutHTMLEscape(call, "function.arguments", delta)
	} else if args.Exists() {
		call, _ = sjson.SetBytes(call, "function.arguments", args.Raw)
	}
	p.FunctionIndex++
	p.SawToolCall = true
	return call, nil
}

func (p *convertCliResponseToOpenAIChatParams) findActiveToolCall(id, rawName string, marked bool) (string, *antigravityChatToolCall, error) {
	if id != "" {
		call := p.ActiveToolCalls[id]
		if call != nil && rawName != "" && call.RawName != rawName {
			return "", nil, errors.New("upstream Antigravity changed the name of an in-progress tool call")
		}
		return id, call, nil
	}
	if rawName != "" && !marked {
		return "", nil, nil
	}
	var matchID string
	var match *antigravityChatToolCall
	for id, call := range p.ActiveToolCalls {
		if rawName != "" && rawName != call.RawName {
			continue
		}
		if match != nil {
			return "", nil, errors.New("upstream Antigravity tool arguments without a call ID match multiple active calls")
		}
		matchID, match = id, call
	}
	return matchID, match, nil
}

func (c *antigravityChatToolCall) pushArguments(fc gjson.Result) (string, bool, error) {
	args := fc.Get("args")
	var members string
	if args.IsObject() {
		members = strings.TrimSpace(args.Raw[1 : len(args.Raw)-1])
	}
	partial := fc.Get("partialArgs")
	if !partial.Exists() {
		partial = fc.Get("partial_args")
	}
	hasPartialArgs := partial.Exists() && (!partial.IsArray() || len(partial.Array()) > 0)
	if c.Assembler != nil || hasPartialArgs {
		if c.ObjectOpen || c.Assembler != nil && !hasPartialArgs && args.Exists() && (!args.IsObject() || members != "") {
			return "", false, errors.New("upstream Antigravity switched tool argument formats mid-call")
		}
		if c.Assembler == nil {
			c.Assembler = translatorcommon.NewVertexPartialArgsAssembler()
		}
		delta, done := c.Assembler.PushFunctionCall(fc)
		return delta, done, c.Assembler.Err()
	}

	// Ordinary args fragments are objects. Join their members into a single
	// object so Chat clients can safely concatenate the argument deltas.
	done := !translatorcommon.VertexFunctionCallWillContinue(fc)
	delta := ""
	if args.Exists() && !args.IsObject() {
		return "", false, errors.New("upstream Antigravity tool argument fragment must be an object")
	}
	if members != "" {
		prefix := "{"
		if c.ObjectOpen {
			prefix = ","
		}
		delta = prefix + members
		c.ObjectOpen = true
	}
	if done {
		if !c.ObjectOpen {
			return "{}", true, nil
		}
		delta += "}"
	}
	return delta, done, nil
}

// FinalizeToolInput reports a truncated tool call before emitting a success terminal.
func (p *convertCliResponseToOpenAIChatParams) FinalizeToolInput() [][]byte {
	if p.ToolInputError() == nil && len(p.ActiveToolCalls) > 0 {
		p.SetToolInputError(errors.New("upstream Antigravity tool call ended before complete arguments"))
	}
	return nil
}

// newChunk creates a stream chunk using the most recent response metadata.
func (p *convertCliResponseToOpenAIChatParams) newChunk() []byte {
	chunk := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null},"finish_reason":null,"native_finish_reason":null}]}`)
	chunk, _ = sjson.SetBytes(chunk, "created", p.UnixTimestamp)
	if p.ModelVersion != "" {
		chunk, _ = sjson.SetBytes(chunk, "model", p.ModelVersion)
	}
	if p.ResponseID != "" {
		chunk, _ = sjson.SetBytes(chunk, "id", p.ResponseID)
	}
	return chunk
}

// finishChunk emits the terminal reason with an empty delta.
func (p *convertCliResponseToOpenAIChatParams) finishChunk(chunk []byte) []byte {
	p.SawFinishReason = true
	finishReason, nativeFinishReason := resolveOpenAIFinishReason(p)
	chunk, _ = sjson.SetRawBytes(chunk, "choices.0.delta", []byte(`{}`))
	chunk, _ = sjson.SetBytes(chunk, "choices.0.finish_reason", finishReason)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.native_finish_reason", nativeFinishReason)
	return chunk
}

// hasAntigravityResponsePayload reports whether a chunk carries actual generated
// content or non-zero output token accounting. Presence alone is not enough: an
// envelope such as `{}`, `{"response":{}}`, `{"response":{"candidates":[]}}`, or
// a signature-only empty-text part with zero output tokens must not count as a
// started stream, otherwise a keepalive or zero-output event would be enough to
// finalize a stream that produced nothing.
func hasAntigravityResponsePayload(rawJSON []byte) bool {
	for _, path := range []string{"response.candidates", "candidates"} {
		if candidates := gjson.GetBytes(rawJSON, path); candidates.IsArray() {
			for _, cand := range candidates.Array() {
				if strings.TrimSpace(cand.Get("finishReason").String()) != "" {
					return true
				}
				if chunks := cand.Get("groundingMetadata.groundingChunks"); chunks.IsArray() && len(chunks.Array()) > 0 {
					return true
				}
				for _, part := range cand.Get("content.parts").Array() {
					hasVisibleInlineData := !part.Get("thought").Bool() &&
						(part.Get("inlineData.data").String() != "" || part.Get("inline_data.data").String() != "")
					if part.Get("text").String() != "" ||
						part.Get("functionCall").Exists() || part.Get("function_call").Exists() ||
						hasVisibleInlineData ||
						part.Get("fileData").Exists() || part.Get("file_data").Exists() ||
						part.Get("executableCode").Exists() || part.Get("executable_code").Exists() ||
						part.Get("codeExecutionResult").Exists() || part.Get("code_execution_result").Exists() {
						return true
					}
				}
			}
		}
	}
	// FilterSSEUsageMetadata renames non-terminal usage to cpaUsageMetadata before
	// the translator sees it, so both spellings must be accepted.
	for _, path := range []string{
		"response.usageMetadata", "response.cpaUsageMetadata",
		"usageMetadata", "cpaUsageMetadata",
	} {
		if usage := gjson.GetBytes(rawJSON, path); usage.IsObject() {
			if usage.Get("candidatesTokenCount").Int() > 0 || usage.Get("thoughtsTokenCount").Int() > 0 {
				return true
			}
		}
	}
	return false
}

// resolveOpenAIFinishReason maps the cached upstream state to the OpenAI
// finish_reason and native_finish_reason pair. Both the terminal upstream chunk
// and the synthesized [DONE] chunk must agree on this mapping.
func resolveOpenAIFinishReason(params *convertCliResponseToOpenAIChatParams) (finishReason, nativeFinishReason string) {
	switch {
	case params.SawToolCall:
		finishReason = "tool_calls"
	case params.UpstreamFinishReason == "MAX_TOKENS":
		finishReason = "length"
	case isAntigravityContentFilterFinishReason(params.UpstreamFinishReason):
		finishReason = "content_filter"
	default:
		finishReason = "stop"
	}
	nativeFinishReason = "stop"
	if params.UpstreamFinishReason != "" {
		nativeFinishReason = strings.ToLower(params.UpstreamFinishReason)
	}
	return finishReason, nativeFinishReason
}

func isAntigravityContentFilterFinishReason(reason string) bool {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "MALFORMED_RESPONSE", "MALFORMED_FUNCTION_CALL":
		return true
	default:
		return false
	}
}

func setOpenAIUsageMetadata(template []byte, usageResult gjson.Result) []byte {
	cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
	template, _ = sjson.SetBytes(template, "usage.completion_tokens", usageResult.Get("candidatesTokenCount").Int()+usageResult.Get("thoughtsTokenCount").Int())
	if totalTokenCountResult := usageResult.Get("totalTokenCount"); totalTokenCountResult.Exists() {
		template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokenCountResult.Int())
	}
	promptTokenCount := usageResult.Get("promptTokenCount").Int()
	thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
	template, _ = sjson.SetBytes(template, "usage.prompt_tokens", promptTokenCount)
	if thoughtsTokenCount > 0 {
		template, _ = sjson.SetBytes(template, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
	}
	if cachedTokenCount > 0 {
		template, _ = sjson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
	}
	return template
}

// ConvertAntigravityResponseToOpenAINonStream converts a non-streaming Antigravity response to a non-streaming OpenAI response.
// This function processes the complete Antigravity response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Antigravity API
//   - param: A pointer to a parameter object for the conversion
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertAntigravityResponseToOpenAINonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	responseResult := gjson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		responseJSON := restoreAntigravityOpenAIFunctionNames([]byte(responseResult.Raw), originalRequestRawJSON)
		// Antigravity restores its own aliases before invoking the Gemini converter.
		return corechat.ConvertGeminiResponseToOpenAINonStream(ctx, modelName, nil, requestRawJSON, responseJSON, param)
	}
	return []byte{}
}

func restoreAntigravityOpenAIFunctionNames(rawJSON, originalRequestRawJSON []byte) []byte {
	nameMap := util.DisambiguatedToolNameMap(originalRequestRawJSON)
	if len(nameMap) == 0 {
		return rawJSON
	}
	candidates := gjson.GetBytes(rawJSON, "candidates")
	for candidateIndex, candidate := range candidates.Array() {
		for partIndex, part := range candidate.Get("content.parts").Array() {
			for _, field := range []string{"functionCall", "functionResponse"} {
				nameResult := part.Get(field + ".name")
				name := nameResult.String()
				if name == "" {
					continue
				}
				restoredName := util.RestoreSanitizedToolName(nameMap, name)
				if nameResult.Type == gjson.String && restoredName == name {
					continue
				}
				path := fmt.Sprintf("candidates.%d.content.parts.%d.%s.name", candidateIndex, partIndex, field)
				rawJSON, _ = sjson.SetBytes(rawJSON, path, restoredName)
			}
		}
	}
	return rawJSON
}
