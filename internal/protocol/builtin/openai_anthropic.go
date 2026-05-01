package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
)

type openAIAnthropicPendingTool struct {
	id        string
	name      string
	arguments string
}

type openAIToAnthropicStreamState struct {
	started          bool
	done             bool
	messageStartSent bool
	textBlockStarted bool
	model            string
	responseID       string
	blockIndex       int
	reasoningStarted bool
	reasoningText    string
	pendingToolCalls map[int]*openAIAnthropicPendingTool
	usage            struct {
		promptTokens             int64
		completionTokens         int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
}

type anthropicToOpenAIStreamState struct {
	model string
	usage struct {
		inputTokens              int64
		outputTokens             int64
		totalTokens              int64
		cacheReadInputTokens     int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
	toolCallIndex      int
	toolID             string
	toolName           string
	toolInput          any
	toolJSON           string
	toolActive         bool
	reasoningActive    bool
	reasoningText      string
	reasoningSignature string
	reasoningData      string
}

func convertOpenAIRequestToAnthropic(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req openAIChatRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeAnthropicRequest(model, conv, stream)
}

func convertAnthropicRequestToOpenAI(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req anthropicMessagesRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeOpenAIRequest(model, conv, stream)
}

func convertAnthropicResponseToOpenAINonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp anthropicMessagesResponse
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	message, err := openAIMessageFromAnthropicBlocks(resp.Content)
	if err != nil {
		return nil, err
	}
	out := openAIChatCompletionResponse{
		ID:      "chatcmpl-proxy",
		Object:  "chat.completion",
		Created: 0,
		Model:   coalesceModel(model, resp.Model),
		Choices: []openAIChatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: mapAnthropicStopReasonToOpenAI(resp.StopReason, len(message.ToolCalls) > 0),
		}},
		Usage: openAIUsageFromAnthropicUsage(resp.Usage),
	}
	return sonic.Marshal(out)
}

func convertOpenAIResponseToAnthropicNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	content := []map[string]any{}
	stopReason := "end_turn"
	if choices, _ := resp["choices"].([]any); len(choices) > 0 {
		if choice, _ := choices[0].(map[string]any); choice != nil {
			if message, _ := choice["message"].(map[string]any); message != nil {
				blocks, err := anthropicBlocksFromOpenAIMessage(message)
				if err != nil {
					return nil, err
				}
				content = blocks
			}
			stopReason = mapOpenAIFinishReasonToAnthropic(stringValue(choice["finish_reason"]))
		}
	}
	out := anthropicMessagesResponse{
		ID:         "msg-proxy",
		Type:       "message",
		Role:       "assistant",
		Content:    []anthropicResponseBlock{},
		Model:      coalesceModel(model, resp["model"]),
		StopReason: stopReason,
	}
	if len(content) > 0 {
		blocks, err := anthropicResponseBlocksFromMaps(content)
		if err != nil {
			return nil, err
		}
		out.Content = blocks
	}
	if usage := openAIUsageFromMap(resp["usage"]); usage != nil {
		inputTokens := max(usage.promptTokens-usage.cachedTokens, 0)

		out.Usage.InputTokens = inputTokens
		out.Usage.OutputTokens = usage.completionTokens
		out.Usage.CacheReadInputTokens = usage.cachedTokens
		out.Usage.CacheCreationInputTokens = usage.cacheCreationInputTokens
		out.Usage.ReasoningTokens = usage.reasoningTokens
	}
	return sonic.Marshal(out)
}

func anthropicBlocksFromOpenAIMessage(message map[string]any) ([]map[string]any, error) {
	parts, err := extractOpenAIContentParts(message["content"])
	if err != nil {
		return nil, err
	}
	if rawCalls, ok := message["tool_calls"]; ok {
		var calls []openAIChatToolCall
		callBytes, err := sonic.Marshal(rawCalls)
		if err != nil {
			return nil, err
		}
		if err := sonic.Unmarshal(callBytes, &calls); err != nil {
			return nil, err
		}
		toolParts, err := extractOpenAIToolCallParts(calls)
		if err != nil {
			return nil, err
		}
		parts = append(parts, toolParts...)
	}
	blocks, err := encodeAnthropicBlocks(parts)
	if err != nil {
		return nil, err
	}
	reasoningAdded := false
	if rawReasoning, ok := message["reasoning"].([]any); ok {
		for _, item := range rawReasoning {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch normalizeRole(stringValue(entry["type"])) {
			case "redacted_thinking":
				blocks = append(blocks, map[string]any{
					"type": "redacted_thinking",
					"data": stringValue(entry["data"]),
				})
				reasoningAdded = true
			default:
				if text := stringValue(entry["text"]); text != "" {
					block := map[string]any{
						"type":     "thinking",
						"thinking": text,
					}
					if signature := stringValue(entry["signature"]); signature != "" {
						block["signature"] = signature
					}
					blocks = append(blocks, block)
					reasoningAdded = true
				}
			}
		}
	}
	if !reasoningAdded {
		if text := stringValue(message["reasoning_content"]); text != "" {
			blocks = append(blocks, map[string]any{
				"type":     "thinking",
				"thinking": text,
			})
		}
	}
	return blocks, nil
}

func convertAnthropicResponseToOpenAIStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	st := initAnthropicToOpenAIStreamState(param, model)

	raw := strings.TrimSpace(string(rawJSON))
	if raw == "" {
		return nil, nil
	}
	eventType, line := parseSSEEventBlock(raw)
	if line == "" {
		return nil, nil
	}

	var payload map[string]any
	if err := sonic.Unmarshal([]byte(line), &payload); err != nil {
		return nil, err
	}
	switch {
	case eventType == "message_start":
		applyAnthropicOpenAIMessageStart(st, payload)
		return nil, nil
	case anthropicOpenAIIsMessageStop(eventType, payload):
		return [][]byte{[]byte("data: [DONE]\n\n")}, nil
	case stringValue(payload["type"]) == "content_block_start":
		handleAnthropicOpenAIContentBlockStart(st, payload)
		return nil, nil
	case stringValue(payload["type"]) == "content_block_delta":
		return handleAnthropicOpenAIContentBlockDelta(st, payload)
	case stringValue(payload["type"]) == "content_block_stop":
		return handleAnthropicOpenAIContentBlockStop(st)
	case stringValue(payload["type"]) == "message_delta":
		return handleAnthropicOpenAIMessageDelta(st, payload)
	}
	return nil, nil
}

func convertOpenAIResponseToAnthropicStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	st := initOpenAIToAnthropicStreamState(param, model)

	line := strings.TrimSpace(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if after, ok := strings.CutPrefix(line, "data:"); ok {
		line = strings.TrimSpace(after)
	}
	if line == "[DONE]" {
		if st.done {
			return nil, nil
		}
		return openAIAnthropicStopChunks(st, "end_turn")
	}

	var chunk map[string]any
	if err := sonic.Unmarshal([]byte(line), &chunk); err != nil {
		return nil, err
	}
	applyOpenAIAnthropicChunk(st, chunk)

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	if st.done {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil, nil
	}

	outputs := make([][]byte, 0, 8)
	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		deltaOutputs, err := handleOpenAIAnthropicChoiceDelta(st, delta)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, deltaOutputs...)
	}

	if finishReasonRaw, ok := choice["finish_reason"]; ok && finishReasonRaw != nil {
		finishOutputs, err := handleOpenAIAnthropicFinishReason(st, finishReasonRaw)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, finishOutputs...)
	}

	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}

func initAnthropicToOpenAIStreamState(param *any, model string) *anthropicToOpenAIStreamState {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &anthropicToOpenAIStreamState{model: model}
	}
	st := (*param).(*anthropicToOpenAIStreamState)
	if st.model == "" {
		st.model = model
	}
	return st
}

func applyAnthropicOpenAIMessageStart(st *anthropicToOpenAIStreamState, payload map[string]any) {
	message, _ := payload["message"].(map[string]any)
	if message == nil {
		return
	}
	if messageModel := stringValue(message["model"]); messageModel != "" {
		st.model = messageModel
	}
	if usage, _ := message["usage"].(map[string]any); usage != nil {
		st.usage.inputTokens = int64Value(usage["input_tokens"])
		st.usage.outputTokens = int64Value(usage["output_tokens"])
		st.usage.cacheReadInputTokens = int64Value(usage["cache_read_input_tokens"])
		st.usage.cacheCreationInputTokens = int64Value(usage["cache_creation_input_tokens"])
		st.usage.reasoningTokens = int64Value(usage["reasoning_tokens"])
		st.usage.totalTokens = st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens + st.usage.outputTokens
		st.usage.seen = st.usage.inputTokens != 0 || st.usage.outputTokens != 0 || st.usage.cacheReadInputTokens != 0 || st.usage.cacheCreationInputTokens != 0 || st.usage.reasoningTokens != 0
	}
}

func anthropicOpenAIIsMessageStop(eventType string, payload map[string]any) bool {
	if eventType == "message_stop" {
		return true
	}
	return stringValue(payload["type"]) == "message_stop"
}

func handleAnthropicOpenAIContentBlockStart(st *anthropicToOpenAIStreamState, payload map[string]any) {
	block, _ := payload["content_block"].(map[string]any)
	if block == nil {
		return
	}
	switch stringValue(block["type"]) {
	case "tool_use":
		st.toolID = stringValue(block["id"])
		st.toolName = stringValue(block["name"])
		st.toolInput = block["input"]
		st.toolJSON = ""
		st.toolActive = true
	case "thinking", "redacted_thinking":
		st.reasoningActive = true
		st.reasoningText = ""
		st.reasoningSignature = ""
		st.reasoningData = stringValue(block["data"])
	}
}

func handleAnthropicOpenAIContentBlockDelta(st *anthropicToOpenAIStreamState, payload map[string]any) ([][]byte, error) {
	delta, _ := payload["delta"].(map[string]any)
	if delta == nil {
		return nil, nil
	}
	switch stringValue(delta["type"]) {
	case "input_json_delta":
		if st.toolActive {
			st.toolJSON += stringValue(delta["partial_json"])
		}
		return nil, nil
	case "thinking_delta":
		return handleAnthropicOpenAIThinkingDelta(st, delta)
	case "signature_delta":
		if st.reasoningActive {
			st.reasoningSignature += stringValue(delta["signature"])
		}
		return nil, nil
	}
	if text := stringValue(delta["text"]); text != "" {
		return marshalOpenAIAnthropicDataChunk(st.model, map[string]any{"content": text})
	}
	return nil, nil
}

func handleAnthropicOpenAIThinkingDelta(st *anthropicToOpenAIStreamState, delta map[string]any) ([][]byte, error) {
	if !st.reasoningActive {
		return nil, nil
	}
	text := stringValue(delta["thinking"])
	if text == "" {
		return nil, nil
	}
	st.reasoningText += text
	return marshalOpenAIAnthropicDataChunk(st.model, map[string]any{"reasoning_content": text})
}

func handleAnthropicOpenAIContentBlockStop(st *anthropicToOpenAIStreamState) ([][]byte, error) {
	if st.reasoningActive {
		return finalizeAnthropicOpenAIReasoning(st)
	}
	if st.toolActive {
		return finalizeAnthropicOpenAITool(st)
	}
	return nil, nil
}

func finalizeAnthropicOpenAIReasoning(st *anthropicToOpenAIStreamState) ([][]byte, error) {
	if st.reasoningSignature == "" && st.reasoningData == "" {
		st.reasoningActive = false
		st.reasoningText = ""
		return nil, nil
	}
	reasoning := map[string]any{"type": "thinking"}
	if st.reasoningSignature != "" {
		reasoning["signature"] = st.reasoningSignature
	}
	if st.reasoningData != "" {
		reasoning["type"] = "redacted_thinking"
		reasoning["data"] = st.reasoningData
	}
	outputs, err := marshalOpenAIAnthropicDataChunk(st.model, map[string]any{"reasoning": reasoning})
	if err != nil {
		return nil, err
	}
	st.reasoningActive = false
	st.reasoningText = ""
	st.reasoningSignature = ""
	st.reasoningData = ""
	return outputs, nil
}

func finalizeAnthropicOpenAITool(st *anthropicToOpenAIStreamState) ([][]byte, error) {
	arguments := strings.TrimSpace(st.toolJSON)
	if arguments == "" {
		if raw, err := sonic.Marshal(st.toolInput); err == nil && len(raw) > 0 {
			arguments = string(raw)
		}
	}
	if arguments == "" {
		arguments = "{}"
	}
	outputs, err := marshalOpenAIAnthropicDataChunk(st.model, map[string]any{
		"tool_calls": []map[string]any{{
			"index": st.toolCallIndex,
			"id":    st.toolID,
			"type":  "function",
			"function": map[string]any{
				"name":      st.toolName,
				"arguments": arguments,
			},
		}},
	})
	if err != nil {
		return nil, err
	}
	st.toolCallIndex++
	st.toolID = ""
	st.toolName = ""
	st.toolInput = nil
	st.toolJSON = ""
	st.toolActive = false
	return outputs, nil
}

func handleAnthropicOpenAIMessageDelta(st *anthropicToOpenAIStreamState, payload map[string]any) ([][]byte, error) {
	if usage, _ := payload["usage"].(map[string]any); usage != nil {
		if val := int64Value(usage["input_tokens"]); val != 0 {
			st.usage.inputTokens = val
		}
		if val := int64Value(usage["output_tokens"]); val != 0 {
			st.usage.outputTokens = val
		}
		if val := int64Value(usage["cache_read_input_tokens"]); val != 0 {
			st.usage.cacheReadInputTokens = val
		}
		if val := int64Value(usage["cache_creation_input_tokens"]); val != 0 {
			st.usage.cacheCreationInputTokens = val
		}
		if val := int64Value(usage["reasoning_tokens"]); val != 0 {
			st.usage.reasoningTokens = val
		}
		st.usage.totalTokens = st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens + st.usage.outputTokens
		st.usage.seen = true
	}
	finishReason := any(nil)
	if delta, _ := payload["delta"].(map[string]any); delta != nil {
		if reason := stringValue(delta["stop_reason"]); reason != "" {
			finishReason = mapAnthropicStopReasonToOpenAI(reason, reason == "tool_use")
		}
	}
	if finishReason == nil && !st.usage.seen {
		return nil, nil
	}
	var usage *openAIUsage
	if st.usage.seen {
		usage = &openAIUsage{
			promptTokens:             st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens,
			completionTokens:         st.usage.outputTokens,
			totalTokens:              st.usage.totalTokens,
			cachedTokens:             st.usage.cacheReadInputTokens,
			cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
			reasoningTokens:          st.usage.reasoningTokens,
		}
	}
	return marshalOpenAIAnthropicChunk(st.model, map[string]any{}, finishReason, usage)
}

func marshalOpenAIAnthropicDataChunk(model string, delta map[string]any) ([][]byte, error) {
	return marshalOpenAIAnthropicChunk(model, delta, nil, nil)
}

func marshalOpenAIAnthropicChunk(model string, delta map[string]any, finishReason any, usage *openAIUsage) ([][]byte, error) {
	chunk := map[string]any{
		"id":      "chatcmpl-proxy",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		chunk["usage"] = openAIUsagePayload(usage)
	}
	body, err := sonic.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
}

func initOpenAIToAnthropicStreamState(param *any, model string) *openAIToAnthropicStreamState {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &openAIToAnthropicStreamState{model: model}
	}
	st := (*param).(*openAIToAnthropicStreamState)
	if st.model == "" {
		st.model = model
	}
	return st
}

func applyOpenAIAnthropicChunk(st *openAIToAnthropicStreamState, chunk map[string]any) {
	if chunkModel := stringValue(chunk["model"]); chunkModel != "" {
		st.model = chunkModel
	}
	if id := stringValue(chunk["id"]); id != "" && st.responseID == "" {
		st.responseID = id
	}
	if usage := openAIUsageFromMap(chunk["usage"]); usage != nil {
		st.usage.promptTokens = usage.promptTokens
		st.usage.completionTokens = usage.completionTokens
		st.usage.cachedTokens = usage.cachedTokens
		st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
		st.usage.reasoningTokens = usage.reasoningTokens
		st.usage.seen = true
	}
}

func handleOpenAIAnthropicChoiceDelta(st *openAIToAnthropicStreamState, delta map[string]any) ([][]byte, error) {
	outputs := make([][]byte, 0, 6)
	if rc := stringValue(delta["reasoning_content"]); rc != "" {
		reasoningOutputs, err := handleOpenAIAnthropicReasoningDelta(st, rc)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, reasoningOutputs...)
	}
	accumulateOpenAIAnthropicToolCalls(st, delta["tool_calls"])
	if content := stringValue(delta["content"]); content != "" {
		textOutputs, err := handleOpenAIAnthropicTextDelta(st, content)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, textOutputs...)
	}
	return outputs, nil
}

func handleOpenAIAnthropicReasoningDelta(st *openAIToAnthropicStreamState, text string) ([][]byte, error) {
	outputs := make([][]byte, 0, 2)
	if !st.reasoningStarted {
		msgStart, err := openAIAnthropicEnsureMessageStart(st)
		if err != nil {
			return nil, err
		}
		if len(msgStart) > 0 {
			outputs = append(outputs, msgStart)
		}
		thinkStart, err := marshalEventSSE("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type":     "thinking",
				"thinking": "",
			},
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, thinkStart)
		st.reasoningStarted = true
	}
	thinkDelta, err := marshalEventSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": text,
		},
	})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, thinkDelta)
	st.reasoningText += text
	return outputs, nil
}

func accumulateOpenAIAnthropicToolCalls(st *openAIToAnthropicStreamState, raw any) {
	toolCallsRaw, ok := raw.([]any)
	if !ok {
		return
	}
	if st.pendingToolCalls == nil {
		st.pendingToolCalls = make(map[int]*openAIAnthropicPendingTool)
	}
	for _, tcRaw := range toolCallsRaw {
		tc, _ := tcRaw.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if idxRaw, ok := tc["index"].(float64); ok {
			idx = int(idxRaw)
		}
		pt, exists := st.pendingToolCalls[idx]
		if !exists {
			pt = &openAIAnthropicPendingTool{}
			st.pendingToolCalls[idx] = pt
		}
		if id := stringValue(tc["id"]); id != "" {
			pt.id = id
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if name := stringValue(fn["name"]); name != "" {
				pt.name = name
			}
			if args := stringValue(fn["arguments"]); args != "" {
				pt.arguments += args
			}
		}
	}
}

func handleOpenAIAnthropicTextDelta(st *openAIToAnthropicStreamState, content string) ([][]byte, error) {
	outputs, err := openAIAnthropicEnsureTextBlockOpen(st)
	if err != nil {
		return nil, err
	}
	deltaChunk, err := marshalEventSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]any{
			"type": "text_delta",
			"text": content,
		},
	})
	if err != nil {
		return nil, err
	}
	return append(outputs, deltaChunk), nil
}

func handleOpenAIAnthropicFinishReason(st *openAIToAnthropicStreamState, finishReasonRaw any) ([][]byte, error) {
	outputs := make([][]byte, 0, 6)
	if st.reasoningStarted {
		thinkStop, err := marshalEventSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": st.blockIndex,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, thinkStop)
		st.blockIndex++
		st.reasoningStarted = false
		st.reasoningText = ""
	}
	toolOutputs, err := flushOpenAIAnthropicPendingToolCalls(st)
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, toolOutputs...)
	done, err := openAIAnthropicStopChunks(st, mapOpenAIFinishReasonToAnthropic(stringValue(finishReasonRaw)))
	if err != nil {
		return nil, err
	}
	return append(outputs, done...), nil
}

func flushOpenAIAnthropicPendingToolCalls(st *openAIToAnthropicStreamState) ([][]byte, error) {
	if len(st.pendingToolCalls) == 0 {
		return nil, nil
	}
	outputs := make([][]byte, 0, len(st.pendingToolCalls)*3+2)
	msgStart, err := openAIAnthropicEnsureMessageStart(st)
	if err != nil {
		return nil, err
	}
	if len(msgStart) > 0 {
		outputs = append(outputs, msgStart)
	}
	textStop, err := openAIAnthropicCloseTextBlock(st)
	if err != nil {
		return nil, err
	}
	if len(textStop) > 0 {
		outputs = append(outputs, textStop)
	}
	for _, tcIdx := range sortedOpenAIAnthropicToolCallIndices(st.pendingToolCalls) {
		pt := st.pendingToolCalls[tcIdx]
		toolID := pt.id
		if toolID == "" {
			toolID = fmt.Sprintf("toolu_%d", tcIdx)
		}
		toolStart, err := marshalEventSSE("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    toolID,
				"name":  pt.name,
				"input": map[string]any{},
			},
		})
		if err != nil {
			return nil, err
		}
		toolDelta, err := marshalEventSSE("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": st.blockIndex,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": pt.arguments,
			},
		})
		if err != nil {
			return nil, err
		}
		toolStop, err := marshalEventSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": st.blockIndex,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, toolStart, toolDelta, toolStop)
		st.blockIndex++
	}
	st.pendingToolCalls = nil
	st.started = true
	return outputs, nil
}

func sortedOpenAIAnthropicToolCallIndices(pending map[int]*openAIAnthropicPendingTool) []int {
	indices := make([]int, 0, len(pending))
	for i := range pending {
		indices = append(indices, i)
	}
	for i := 1; i < len(indices); i++ {
		for j := i; j > 0 && indices[j] < indices[j-1]; j-- {
			indices[j], indices[j-1] = indices[j-1], indices[j]
		}
	}
	return indices
}

func openAIAnthropicEnsureMessageStart(st *openAIToAnthropicStreamState) ([]byte, error) {
	if st == nil || st.messageStartSent {
		return nil, nil
	}
	msgStart, err := openAIAnthropicMessageStart(st)
	if err != nil {
		return nil, err
	}
	st.messageStartSent = true
	st.started = true
	return msgStart, nil
}

func openAIAnthropicCloseTextBlock(st *openAIToAnthropicStreamState) ([]byte, error) {
	if st == nil || !st.textBlockStarted {
		return nil, nil
	}
	blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.blockIndex,
	})
	if err != nil {
		return nil, err
	}
	st.textBlockStarted = false
	st.blockIndex++
	return blockStop, nil
}

func openAIAnthropicEnsureTextBlockOpen(st *openAIToAnthropicStreamState) ([][]byte, error) {
	outputs := make([][]byte, 0, 3)
	if st == nil {
		return outputs, nil
	}
	msgStart, err := openAIAnthropicEnsureMessageStart(st)
	if err != nil {
		return nil, err
	}
	if len(msgStart) > 0 {
		outputs = append(outputs, msgStart)
	}
	if st.reasoningStarted {
		thinkStop, err := marshalEventSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": st.blockIndex,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, thinkStop)
		st.blockIndex++
		st.reasoningStarted = false
		st.reasoningText = ""
	}
	if st.textBlockStarted {
		return outputs, nil
	}
	blockStart, err := openAIAnthropicTextBlockStart(st.blockIndex)
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, blockStart)
	st.textBlockStarted = true
	return outputs, nil
}

func openAIAnthropicMessageStart(st *openAIToAnthropicStreamState) ([]byte, error) {
	inputTokens := int64(0)
	cacheReadTokens := int64(0)
	cacheCreationTokens := int64(0)
	if st != nil && st.usage.seen {
		inputTokens = max(st.usage.promptTokens-st.usage.cachedTokens, 0)
		cacheReadTokens = st.usage.cachedTokens
		cacheCreationTokens = st.usage.cacheCreationInputTokens
	}
	usage := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": 0,
	}
	if cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = cacheReadTokens
	}
	if cacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = cacheCreationTokens
	}
	msgID := st.responseID
	if msgID == "" {
		msgID = "msg-proxy"
	}
	return marshalEventSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         st.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usage,
		},
	})
}

func openAIAnthropicTextBlockStart(index int) ([]byte, error) {
	return marshalEventSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
}

func openAIAnthropicStartChunks(st *openAIToAnthropicStreamState) ([][]byte, error) {
	msgStart, err := openAIAnthropicMessageStart(st)
	if err != nil {
		return nil, err
	}
	blockStart, err := openAIAnthropicTextBlockStart(0)
	if err != nil {
		return nil, err
	}
	return [][]byte{msgStart, blockStart}, nil
}

func openAIAnthropicStopChunks(st *openAIToAnthropicStreamState, stopReason string) ([][]byte, error) {
	if stopReason == "" {
		stopReason = "end_turn"
	}
	outputs := make([][]byte, 0, 5)
	if st != nil && !st.started {
		// Nothing has been streamed yet: emit message_start + text block_start then stop it.
		start, err := openAIAnthropicStartChunks(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, start...)
		st.textBlockStarted = true
	}
	// Only close the text block if it was actually opened.
	if st == nil || st.textBlockStarted {
		textBlockIndex := 0
		if st != nil {
			textBlockIndex = st.blockIndex
		}
		blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStop)
	}
	outputTokens := int64(0)
	cacheReadTokens := int64(0)
	cacheCreationTokens := int64(0)
	reasoningTokens := int64(0)
	if st != nil && st.usage.seen {
		outputTokens = st.usage.completionTokens
		cacheReadTokens = st.usage.cachedTokens
		cacheCreationTokens = st.usage.cacheCreationInputTokens
		reasoningTokens = st.usage.reasoningTokens
	}
	usage := map[string]any{
		"output_tokens": outputTokens,
	}
	if cacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = cacheReadTokens
	}
	if cacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = cacheCreationTokens
	}
	if reasoningTokens > 0 {
		usage["reasoning_tokens"] = reasoningTokens
	}
	inputTokens := int64(0)
	if st != nil && st.usage.seen {
		inputTokens = max(st.usage.promptTokens-st.usage.cachedTokens, 0)
	}
	usage["input_tokens"] = inputTokens
	messageDelta, err := marshalEventSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usage,
	})
	if err != nil {
		return nil, err
	}
	messageStop, err := marshalEventSSE("message_stop", map[string]any{"type": "message_stop"})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, messageDelta, messageStop)
	if st != nil {
		st.started = false
		st.done = true
	}
	return outputs, nil
}
