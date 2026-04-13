package builtin

import (
	"context"
	"encoding/json"
	"fmt"
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

type codexToGeminiStreamState struct {
	model      string
	responseID string
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

func convertGeminiRequestToCodex(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req geminiRequestPayload
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeGeminiConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeCodexRequest(model, conv, stream)
}

func convertGeminiResponseToCodexNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp geminiResponse
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}

	output := make([]map[string]any, 0)
	if len(resp.Candidates) > 0 {
		parts, err := conversationPartsFromGeminiParts(resp.Candidates[0].Content.Parts)
		if err != nil {
			return nil, err
		}
		output, err = codexOutputItemsFromConversationParts(parts)
		if err != nil {
			return nil, err
		}
	}

	out := map[string]any{
		"id":     "resp-proxy",
		"object": "response",
		"status": "completed",
		"model":  coalesceModel(model, resp.ModelVersion),
		"output": output,
	}
	if resp.UsageMetadata.PromptTokenCount != 0 || resp.UsageMetadata.CandidatesTokenCount != 0 || resp.UsageMetadata.TotalTokenCount != 0 {
		out["usage"] = map[string]any{
			"input_tokens":  resp.UsageMetadata.PromptTokenCount,
			"output_tokens": resp.UsageMetadata.CandidatesTokenCount,
			"total_tokens":  resp.UsageMetadata.TotalTokenCount,
		}
	}
	return sonic.Marshal(out)
}

func convertCodexResponseToGeminiNonStream(_ context.Context, model string, _, _, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	parts, err := geminiPartsFromCodexOutput(resp["output"])
	if err != nil {
		return nil, err
	}
	var promptTokens, candidateTokens, totalTokens int64
	includeUsage := false
	if usage := codexUsageFromMap(resp["usage"]); usage != nil {
		promptTokens = usage.inputTokens
		candidateTokens = usage.outputTokens
		totalTokens = usage.totalTokens
		includeUsage = true
	}
	return sonic.Marshal(buildGeminiPayloadFromParts(coalesceModel(model, resp["model"]), stringValue(resp["id"]), parts, "STOP", promptTokens, candidateTokens, totalTokens, includeUsage))
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
	if len(resp.Candidates) == 0 {
		return nil, nil
	}
	parts, err := conversationPartsFromGeminiParts(resp.Candidates[0].Content.Parts)
	if err != nil {
		return nil, err
	}

	outputs := make([][]byte, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case partKindText:
			if part.Text == "" {
				continue
			}
			chunk := map[string]any{
				"type":  "response.output_text.delta",
				"delta": part.Text,
			}
			body, err := sonic.Marshal(chunk)
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, append([]byte("event: response.output_text.delta\ndata: "), append(body, []byte("\n\n")...)...))
		case partKindToolCall:
			encoded, err := encodeCodexToolCall(part.ToolCall)
			if err != nil {
				return nil, err
			}
			chunk := map[string]any{
				"type": "response.output_item.done",
				"item": encoded,
			}
			body, err := sonic.Marshal(chunk)
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, append([]byte("event: response.output_item.done\ndata: "), append(body, []byte("\n\n")...)...))
		default:
			return nil, fmt.Errorf("unsupported gemini response part kind %q", part.Kind)
		}
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}

func convertCodexResponseToGeminiStream(_ context.Context, model string, _, _, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &codexToGeminiStreamState{model: model}
	}
	st := (*param).(*codexToGeminiStreamState)
	if st.model == "" {
		st.model = model
	}

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
	if response, _ := payload["response"].(map[string]any); response != nil {
		if responseID := stringValue(response["id"]); responseID != "" {
			st.responseID = responseID
		}
		if responseModel := stringValue(response["model"]); responseModel != "" {
			st.model = responseModel
		}
		if usage := codexUsageFromMap(response["usage"]); usage != nil && (eventType == "response.completed" || stringValue(payload["type"]) == "response.completed") {
			body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, st.responseID, nil, "STOP", usage.inputTokens, usage.outputTokens, usage.totalTokens, true))
			if err != nil {
				return nil, err
			}
			return [][]byte{body}, nil
		}
	}
	if eventType == "response.output_text.delta" || stringValue(payload["type"]) == "response.output_text.delta" {
		if content := stringValue(payload["delta"]); content != "" {
			body, err := marshalDataSSE(buildGeminiPayload(st.model, content, "", 0, 0, 0, false))
			if err != nil {
				return nil, err
			}
			return [][]byte{body}, nil
		}
	}
	if eventType == "response.output_item.done" || stringValue(payload["type"]) == "response.output_item.done" {
		if item, _ := payload["item"].(map[string]any); item != nil && normalizeRole(stringValue(item["type"])) == "function_call" {
			call, err := decodeCodexToolCall(item)
			if err != nil {
				return nil, err
			}
			args, err := rawJSONToAny(call.Arguments)
			if err != nil {
				return nil, err
			}
			body, err := marshalDataSSE(buildGeminiPayloadFromParts(st.model, "", []geminiPart{{
				FunctionCall: &geminiFunctionCall{Name: call.Name, Args: args},
			}}, "", 0, 0, 0, false))
			if err != nil {
				return nil, err
			}
			return [][]byte{body}, nil
		}
	}
	return nil, nil
}
