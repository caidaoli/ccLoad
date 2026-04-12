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

type openAIChatMessage struct {
	Role      string          `json:"role"`
	Content   any             `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text,omitempty"`
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

func convertOpenAIRequestToGemini(model string, rawJSON []byte, _ bool) ([]byte, error) {
	var req openAIChatRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}

	prompt, err := normalizeOpenAITextPrompt(req)
	if err != nil {
		return nil, err
	}
	out := geminiRequest{Contents: make([]geminiContent, 0, len(prompt.Messages)+1)}
	for _, msg := range prompt.Messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		out.Contents = append(out.Contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: msg.Text}},
		})
	}
	payload := map[string]any{"contents": out.Contents}
	if prompt.System != "" {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": prompt.System}},
		}
	}
	return sonic.Marshal(payload)
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
