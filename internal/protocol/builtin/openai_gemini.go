package builtin

import (
	"context"
	"encoding/json"
	"strings"

	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

type openAIChatRequest struct {
	Model      string              `json:"model"`
	Messages   []openAIChatMessage `json:"messages"`
	Stream     util.FlexibleBool   `json:"stream"`
	Tools      json.RawMessage     `json:"tools,omitempty"`
	ToolChoice json.RawMessage     `json:"tool_choice,omitempty"`
}

type openAIChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIChatMessage struct {
	Role             string               `json:"role"`
	Content          any                  `json:"content"`
	ToolCalls        []openAIChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
	Name             string               `json:"name,omitempty"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	Reasoning        any                  `json:"reasoning,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
	ResponseID   string `json:"responseId,omitempty"`
}

type openAIChatCompletionResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []openAIChatCompletionChoice `json:"choices"`
	Usage   openAIChatCompletionUsage    `json:"usage"`
}

type openAIChatCompletionChoice struct {
	Index        int                         `json:"index"`
	Message      openAIChatCompletionMessage `json:"message"`
	FinishReason string                      `json:"finish_reason"`
}

type openAIChatCompletionMessage struct {
	Role             string               `json:"role"`
	Content          any                  `json:"content,omitempty"`
	ToolCalls        []openAIChatToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	Reasoning        any                  `json:"reasoning,omitempty"`
	Text             string               `json:"text,omitempty"`
}

type openAITokenDetails struct {
	CachedTokens    int64 `json:"cached_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

type openAIChatCompletionUsage struct {
	PromptTokens             int64               `json:"prompt_tokens"`
	CompletionTokens         int64               `json:"completion_tokens"`
	TotalTokens              int64               `json:"total_tokens"`
	PromptTokensDetails      *openAITokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails  *openAITokenDetails `json:"completion_tokens_details,omitempty"`
	CacheCreationInputTokens int64               `json:"cache_creation_input_tokens,omitempty"`
}

type openAIToGeminiStreamState struct {
	model            string
	done             bool
	doneUsageEmitted bool
	pendingToolCalls map[int]*pendingToolCall
	usage            struct {
		promptTokens     int64
		completionTokens int64
		totalTokens      int64
		seen             bool
	}
}

type geminiToOpenAIStreamState struct {
	model              string
	pendingToolCallIDs []string
	nextToolCallID     int
	toolCallIndex      int
}

func convertOpenAIRequestToGemini(model string, rawJSON []byte, _ bool) ([]byte, error) {
	var req openAIChatRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}

	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeGeminiRequest(conv)
}

func convertGeminiRequestToOpenAI(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req geminiRequestPayload
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeGeminiConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeOpenAIRequest(model, conv, stream)
}

func convertGeminiResponseToOpenAINonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp geminiResponse
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}

	message := openAIChatCompletionMessage{
		Role:    "assistant",
		Content: "",
	}
	finishReason := "stop"
	if len(resp.Candidates) > 0 {
		parts, err := conversationPartsFromGeminiParts(resp.Candidates[0].Content.Parts)
		if err != nil {
			return nil, err
		}
		content, toolCalls, err := openAIMessageFromConversationParts(parts)
		if err != nil {
			return nil, err
		}
		message.Content = content
		message.ToolCalls = toolCalls
		finishReason = mapGeminiFinishReasonToOpenAI(resp.Candidates[0].FinishReason, len(toolCalls) > 0)
	}

	out := openAIChatCompletionResponse{
		ID:      "chatcmpl-proxy",
		Object:  "chat.completion",
		Created: 0,
		Model:   coalesceModel(model, resp.ModelVersion),
		Choices: []openAIChatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReason,
		}},
		Usage: openAIChatCompletionUsage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		},
	}
	return sonic.Marshal(out)
}

func convertOpenAIResponseToGeminiNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}

	responseModel := coalesceModel(model, resp["model"])
	finishReason := "STOP"
	parts := []geminiPart{}
	if choices, _ := resp["choices"].([]any); len(choices) > 0 {
		if choice, _ := choices[0].(map[string]any); choice != nil {
			if message, _ := choice["message"].(map[string]any); message != nil {
				var err error
				parts, err = geminiPartsFromOpenAIMessage(message["content"], message["tool_calls"])
				if err != nil {
					return nil, err
				}
			}
			finishReason = mapOpenAIFinishReasonToGemini(stringValue(choice["finish_reason"]))
			if finishReason == "" {
				finishReason = "STOP"
			}
		}
	}
	usage := openAIUsageFromMap(resp["usage"])
	includeUsage := usage != nil
	var promptTokens, completionTokens, totalTokens int64
	if usage != nil {
		promptTokens = usage.promptTokens
		completionTokens = usage.completionTokens
		totalTokens = usage.totalTokens
	}
	return sonic.Marshal(buildGeminiPayloadFromParts(responseModel, "", parts, finishReason, promptTokens, completionTokens, totalTokens, includeUsage))
}

func convertGeminiResponseToOpenAIStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &geminiToOpenAIStreamState{model: model, nextToolCallID: 1}
	}
	st := (*param).(*geminiToOpenAIStreamState)
	if st.model == "" {
		st.model = model
	}
	if st.nextToolCallID == 0 {
		st.nextToolCallID = 1
	}

	line := strings.TrimSpace(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if after, ok := strings.CutPrefix(line, "data:"); ok {
		line = strings.TrimSpace(after)
	}
	if line == "[DONE]" {
		return [][]byte{[]byte("data: [DONE]\n\n")}, nil
	}

	var resp geminiResponse
	if err := sonic.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	if resp.ModelVersion != "" {
		st.model = resp.ModelVersion
	}
	if len(resp.Candidates) == 0 {
		return nil, nil
	}
	parts, err := extractGeminiParts(resp.Candidates[0].Content.Parts, &st.pendingToolCallIDs, &st.nextToolCallID)
	if err != nil {
		return nil, err
	}
	content, toolCalls, err := openAIMessageFromConversationParts(parts)
	if err != nil {
		return nil, err
	}

	delta := map[string]any{}
	switch v := content.(type) {
	case string:
		if v != "" {
			delta["content"] = v
		}
	case nil:
	default:
		delta["content"] = v
	}
	if len(toolCalls) > 0 {
		chunkToolCalls := make([]map[string]any, 0, len(toolCalls))
		for i, call := range toolCalls {
			chunkToolCalls = append(chunkToolCalls, map[string]any{
				"index": st.toolCallIndex + i,
				"id":    call.ID,
				"type":  call.Type,
				"function": map[string]any{
					"name":      call.Function.Name,
					"arguments": call.Function.Arguments,
				},
			})
		}
		delta["tool_calls"] = chunkToolCalls
		st.toolCallIndex += len(toolCalls)
	}
	finishReason := any(nil)
	if resp.Candidates[0].FinishReason != "" || len(toolCalls) > 0 {
		finishReason = mapGeminiFinishReasonToOpenAI(resp.Candidates[0].FinishReason, len(toolCalls) > 0)
	}
	if len(delta) == 0 && finishReason == nil {
		return nil, nil
	}

	chunk := map[string]any{
		"id":      "chatcmpl-proxy",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   st.model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	if resp.UsageMetadata.PromptTokenCount != 0 || resp.UsageMetadata.CandidatesTokenCount != 0 || resp.UsageMetadata.TotalTokenCount != 0 {
		chunk["usage"] = map[string]any{
			"prompt_tokens":     resp.UsageMetadata.PromptTokenCount,
			"completion_tokens": resp.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      resp.UsageMetadata.TotalTokenCount,
		}
	}
	body, err := sonic.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
}

func convertOpenAIResponseToGeminiStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &openAIToGeminiStreamState{model: model}
	}
	st := (*param).(*openAIToGeminiStreamState)
	if st.model == "" {
		st.model = model
	}

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
		parts, err := st.flushPendingToolCalls()
		if err != nil {
			return nil, err
		}
		body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, "", parts, "STOP", st.usage.promptTokens, st.usage.completionTokens, st.usage.totalTokens, st.usage.seen))
		if err != nil {
			return nil, err
		}
		st.done = true
		st.doneUsageEmitted = st.usage.seen
		return [][]byte{body}, nil
	}

	var chunk map[string]any
	if err := sonic.Unmarshal([]byte(line), &chunk); err != nil {
		return nil, err
	}
	if chunkModel := stringValue(chunk["model"]); chunkModel != "" && st.model == "" {
		st.model = chunkModel
	}
	chunkHasUsage := false
	if usage := openAIUsageFromMap(chunk["usage"]); usage != nil {
		st.usage.promptTokens = usage.promptTokens
		st.usage.completionTokens = usage.completionTokens
		st.usage.totalTokens = usage.totalTokens
		st.usage.seen = true
		chunkHasUsage = true
	}
	choices, _ := chunk["choices"].([]any)
	if st.done {
		if !st.doneUsageEmitted && chunkHasUsage && len(choices) == 0 {
			body, err := marshalDataSSE(buildGeminiPayload(st.model, "", "", st.usage.promptTokens, st.usage.completionTokens, st.usage.totalTokens, true))
			if err != nil {
				return nil, err
			}
			st.doneUsageEmitted = true
			return [][]byte{body}, nil
		}
		return nil, nil
	}
	if len(choices) == 0 {
		if !st.usage.seen {
			return nil, nil
		}
		body, err := marshalDataSSE(buildGeminiPayload(st.model, "", "", st.usage.promptTokens, st.usage.completionTokens, st.usage.totalTokens, true))
		if err != nil {
			return nil, err
		}
		return [][]byte{body}, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil, nil
	}

	outputs := make([][]byte, 0, 2)
	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		// reasoning_content has no Gemini semantic; emit as a plain text part.
		if rc := stringValue(delta["reasoning_content"]); rc != "" {
			body, err := marshalDataSSE(buildGeminiPayload(st.model, rc, "", 0, 0, 0, false))
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, body)
		}
		contentParts, err := geminiPartsFromOpenAIMessage(delta["content"], nil)
		if err != nil {
			return nil, err
		}
		if len(contentParts) > 0 {
			body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, "", contentParts, "", 0, 0, 0, false))
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, body)
		}
		if err := st.accumulateToolCalls(delta["tool_calls"]); err != nil {
			return nil, err
		}
	}
	if finishReasonRaw, ok := choice["finish_reason"]; ok && finishReasonRaw != nil {
		finishReason := mapOpenAIFinishReasonToGemini(stringValue(finishReasonRaw))
		parts, err := st.flushPendingToolCalls()
		if err != nil {
			return nil, err
		}
		body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, "", parts, finishReason, st.usage.promptTokens, st.usage.completionTokens, st.usage.totalTokens, st.usage.seen))
		if err != nil {
			return nil, err
		}
		st.done = true
		st.doneUsageEmitted = chunkHasUsage
		outputs = append(outputs, body)
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}

func (st *openAIToGeminiStreamState) accumulateToolCalls(rawToolCalls any) error {
	toolCalls, err := decodeObjectSlice(rawToolCalls)
	if err != nil {
		return err
	}
	if len(toolCalls) == 0 {
		return nil
	}
	if st.pendingToolCalls == nil {
		st.pendingToolCalls = make(map[int]*pendingToolCall)
	}
	for _, tc := range toolCalls {
		idx := int(int64Value(tc["index"]))
		pending := st.pendingToolCalls[idx]
		if pending == nil {
			pending = &pendingToolCall{}
			st.pendingToolCalls[idx] = pending
		}
		if id := stringValue(tc["id"]); id != "" {
			pending.id = id
		}
		if fn, _ := tc["function"].(map[string]any); fn != nil {
			if name := stringValue(fn["name"]); name != "" {
				pending.name = name
			}
			if args, ok := fn["arguments"]; ok {
				pending.arguments += stringValue(args)
			}
		}
	}
	return nil
}

func (st *openAIToGeminiStreamState) flushPendingToolCalls() ([]geminiPart, error) {
	if len(st.pendingToolCalls) == 0 {
		return nil, nil
	}
	indices := make([]int, 0, len(st.pendingToolCalls))
	for idx := range st.pendingToolCalls {
		indices = append(indices, idx)
	}
	for i := 1; i < len(indices); i++ {
		for j := i; j > 0 && indices[j] < indices[j-1]; j-- {
			indices[j], indices[j-1] = indices[j-1], indices[j]
		}
	}
	parts := make([]geminiPart, 0, len(indices))
	for _, idx := range indices {
		pending := st.pendingToolCalls[idx]
		if pending == nil {
			continue
		}
		args, err := rawJSONToAny(json.RawMessage(pending.arguments))
		if err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]any{}
		}
		parts = append(parts, geminiPart{
			FunctionCall: &geminiFunctionCall{
				Name: pending.name,
				Args: args,
			},
		})
	}
	st.pendingToolCalls = nil
	return parts, nil
}
