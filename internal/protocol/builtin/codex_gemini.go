package builtin

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/bytedance/sonic"
)

type codexRequest struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions,omitempty"`
	Stream       bool              `json:"stream,omitempty"`
	Tools        json.RawMessage   `json:"tools,omitempty"`
	ToolChoice   json.RawMessage   `json:"tool_choice,omitempty"`
	Input        []json.RawMessage `json:"input"`
}

type codexResponse struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Status string `json:"status"`
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

func convertCodexRequestToGemini(_ string, rawJSON []byte, _ bool) ([]byte, error) {
	var req codexRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}

	conv, err := normalizeCodexConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeGeminiRequest(conv)
}

func convertGeminiResponseToCodexNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
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

	out := codexResponse{
		ID:     "resp-proxy",
		Object: "response",
		Status: "completed",
		Model:  model,
		Output: []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{
			{
				Type: "message",
				Role: "assistant",
				Content: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{
					{Type: "output_text", Text: content},
				},
			},
		},
	}
	out.Usage.InputTokens = resp.UsageMetadata.PromptTokenCount
	out.Usage.OutputTokens = resp.UsageMetadata.CandidatesTokenCount
	out.Usage.TotalTokens = resp.UsageMetadata.TotalTokenCount
	if out.Model == "" {
		out.Model = resp.ModelVersion
	}
	return sonic.Marshal(out)
}

type codexStreamState struct {
	responseID string
	model      string
	usage      struct {
		inputTokens  int64
		outputTokens int64
		totalTokens  int64
		seen         bool
	}
}

func convertGeminiResponseToCodexStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if *param == nil {
		*param = &codexStreamState{responseID: "resp-proxy", model: model}
	}
	st := (*param).(*codexStreamState)
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
		response := map[string]any{
			"id":     st.responseID,
			"object": "response",
			"status": "completed",
		}
		if st.model != "" {
			response["model"] = st.model
		}
		if st.usage.seen {
			response["usage"] = map[string]any{
				"input_tokens":  st.usage.inputTokens,
				"output_tokens": st.usage.outputTokens,
				"total_tokens":  st.usage.totalTokens,
			}
		}
		done := map[string]any{
			"type":     "response.completed",
			"response": response,
		}
		body, err := sonic.Marshal(done)
		if err != nil {
			return nil, err
		}
		return [][]byte{append([]byte("event: response.completed\ndata: "), append(body, []byte("\n\n")...)...)}, nil
	}

	var resp geminiResponse
	if err := sonic.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	if st.model == "" {
		st.model = resp.ModelVersion
	}
	if resp.UsageMetadata.PromptTokenCount != 0 || resp.UsageMetadata.CandidatesTokenCount != 0 || resp.UsageMetadata.TotalTokenCount != 0 {
		st.usage.inputTokens = resp.UsageMetadata.PromptTokenCount
		st.usage.outputTokens = resp.UsageMetadata.CandidatesTokenCount
		st.usage.totalTokens = resp.UsageMetadata.TotalTokenCount
		st.usage.seen = true
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
		"type":  "response.output_text.delta",
		"delta": content,
	}
	body, err := sonic.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return [][]byte{append([]byte("event: response.output_text.delta\ndata: "), append(body, []byte("\n\n")...)...)}, nil
}
