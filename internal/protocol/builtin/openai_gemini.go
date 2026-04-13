package builtin

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/bytedance/sonic"
)

type openAIChatRequest struct {
	Model      string              `json:"model"`
	Messages   []openAIChatMessage `json:"messages"`
	Stream     bool                `json:"stream"`
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
	Role       string               `json:"role"`
	Content    any                  `json:"content"`
	ToolCalls  []openAIChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	Name       string               `json:"name,omitempty"`
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

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

type openAIChatCompletionResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []openAIChatCompletionChoice `json:"choices"`
	Usage   openAIChatCompletionUsage    `json:"usage,omitempty"`
}

type openAIChatCompletionChoice struct {
	Index        int                         `json:"index"`
	Message      openAIChatCompletionMessage `json:"message"`
	FinishReason string                      `json:"finish_reason"`
}

type openAIChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatCompletionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type openAIToGeminiStreamState struct {
	model string
	usage struct {
		promptTokens     int64
		completionTokens int64
		totalTokens      int64
		seen             bool
	}
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

	content := ""
	if len(resp.Candidates) > 0 {
		for _, part := range resp.Candidates[0].Content.Parts {
			content += part.Text
		}
	}

	out := openAIChatCompletionResponse{
		ID:      "chatcmpl-proxy",
		Object:  "chat.completion",
		Created: 0,
		Model:   model,
		Choices: []openAIChatCompletionChoice{{
			Index: 0,
			Message: openAIChatCompletionMessage{
				Role:    "assistant",
				Content: content,
			},
			FinishReason: "stop",
		}},
		Usage: openAIChatCompletionUsage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		},
	}

	if out.Model == "" {
		out.Model = resp.ModelVersion
	}
	return sonic.Marshal(out)
}

func convertOpenAIResponseToGeminiNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}

	responseModel := coalesceModel(model, resp["model"])
	content := ""
	finishReason := "STOP"
	if choices, _ := resp["choices"].([]any); len(choices) > 0 {
		if choice, _ := choices[0].(map[string]any); choice != nil {
			if message, _ := choice["message"].(map[string]any); message != nil {
				content = stringValue(message["content"])
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
	return sonic.Marshal(buildGeminiPayload(responseModel, content, finishReason, promptTokens, completionTokens, totalTokens, includeUsage))
}

func convertGeminiResponseToOpenAIStream(_ context.Context, model string, _, _, rawJSON []byte, _ *any) ([][]byte, error) {
	line := strings.TrimSpace(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		return [][]byte{[]byte("data: [DONE]\n\n")}, nil
	}

	var resp geminiResponse
	if err := sonic.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}

	content := ""
	if len(resp.Candidates) > 0 {
		for _, part := range resp.Candidates[0].Content.Parts {
			content += part.Text
		}
	}
	if content == "" {
		return nil, nil
	}

	chunk := map[string]any{
		"id":      "chatcmpl-proxy",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"content": content,
				},
				"finish_reason": nil,
			},
		},
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
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		body, err := marshalDataSSE(buildGeminiPayload(st.model, "", "STOP", st.usage.promptTokens, st.usage.completionTokens, st.usage.totalTokens, st.usage.seen))
		if err != nil {
			return nil, err
		}
		return [][]byte{body}, nil
	}

	var chunk map[string]any
	if err := sonic.Unmarshal([]byte(line), &chunk); err != nil {
		return nil, err
	}
	if chunkModel := stringValue(chunk["model"]); chunkModel != "" {
		st.model = chunkModel
	}
	if usage := openAIUsageFromMap(chunk["usage"]); usage != nil {
		st.usage.promptTokens = usage.promptTokens
		st.usage.completionTokens = usage.completionTokens
		st.usage.totalTokens = usage.totalTokens
		st.usage.seen = true
	}

	choices, _ := chunk["choices"].([]any)
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
		if content := stringValue(delta["content"]); content != "" {
			body, err := marshalDataSSE(buildGeminiPayload(st.model, content, "", 0, 0, 0, false))
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, body)
		}
	}
	if finishReasonRaw, ok := choice["finish_reason"]; ok && finishReasonRaw != nil {
		finishReason := mapOpenAIFinishReasonToGemini(stringValue(finishReasonRaw))
		body, err := marshalDataSSE(buildGeminiPayload(st.model, "", finishReason, st.usage.promptTokens, st.usage.completionTokens, st.usage.totalTokens, st.usage.seen))
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, body)
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}
