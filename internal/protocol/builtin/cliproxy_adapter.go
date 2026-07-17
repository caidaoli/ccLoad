package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"ccLoad/internal/protocol/cliproxy/claude/gemini"
	oaiclaude "ccLoad/internal/protocol/cliproxy/claude/openai/chat-completions"
	oairespclaude "ccLoad/internal/protocol/cliproxy/claude/openai/responses"
	claudecodex "ccLoad/internal/protocol/cliproxy/codex/claude"
	geminicodex "ccLoad/internal/protocol/cliproxy/codex/gemini"
	oaicodex "ccLoad/internal/protocol/cliproxy/codex/openai/chat-completions"
	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	claudegemini "ccLoad/internal/protocol/cliproxy/gemini/claude"
	oaigemini "ccLoad/internal/protocol/cliproxy/gemini/openai/chat-completions"
	openairespgemini "ccLoad/internal/protocol/cliproxy/gemini/openai/responses"
	"ccLoad/internal/protocol/cliproxy/openai/claude"
	openaigemini "ccLoad/internal/protocol/cliproxy/openai/gemini"
	openairesponses "ccLoad/internal/protocol/cliproxy/openai/openai/responses"
)

func cliproxyOpenAIRequestToGemini(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateOpenAIRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("openai request to gemini", raw, oaigemini.ConvertOpenAIRequestToGemini(model, raw, stream))
}

func cliproxyGeminiResponseToOpenAIStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamOpenAI, oaigemini.ConvertGeminiResponseToOpenAI)
}

func cliproxyGeminiResponseToOpenAINonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	return cliproxyJSONResponse("gemini response to openai", raw, oaigemini.ConvertGeminiResponseToOpenAINonStream(ctx, model, original, translated, raw, nil))
}

func cliproxyGeminiRequestToOpenAI(model string, raw []byte, stream bool) ([]byte, error) {
	return cliproxyJSONRequest("gemini request to openai", raw, openaigemini.ConvertGeminiRequestToOpenAI(model, raw, stream))
}

func cliproxyOpenAIResponseToGeminiStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateOpenAIToGeminiStream(ctx, model, original, translated, raw, param)
}

func cliproxyOpenAIResponseToGeminiNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	return cliproxyJSONResponse("openai response to gemini", raw, openaigemini.ConvertOpenAIResponseToGeminiNonStream(ctx, model, original, translated, raw, nil))
}

func cliproxyOpenAIRequestToAnthropic(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateOpenAIRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("openai request to anthropic", raw, oaiclaude.ConvertOpenAIRequestToClaude(model, raw, stream))
}

func cliproxyAnthropicResponseToOpenAIStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamOpenAI, oaiclaude.ConvertClaudeResponseToOpenAI)
}

func cliproxyAnthropicResponseToOpenAINonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	normalized, err := translatorcommon.NormalizeAnthropicResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize anthropic response to openai: %w", err)
	}
	return cliproxyJSONResponse("anthropic response to openai", normalized, oaiclaude.ConvertClaudeResponseToOpenAINonStream(ctx, model, original, translated, normalized, nil))
}

func cliproxyAnthropicRequestToOpenAI(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateAnthropicRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("anthropic request to openai", raw, claude.ConvertClaudeRequestToOpenAI(model, raw, stream))
}

func cliproxyOpenAIResponseToAnthropicStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateOpenAIToAnthropicStream(ctx, model, original, translated, raw, param)
}

func cliproxyOpenAIResponseToAnthropicNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	return cliproxyJSONResponse("openai response to anthropic", raw, claude.ConvertOpenAIResponseToClaudeNonStream(ctx, model, original, translated, raw, nil))
}

func cliproxyOpenAIRequestToCodex(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateOpenAIRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("openai request to codex", raw, oaicodex.ConvertOpenAIRequestToCodex(model, raw, stream))
}

func cliproxyCodexResponseToOpenAIStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamOpenAI, oaicodex.ConvertCodexResponseToOpenAI)
}

func cliproxyCodexResponseToOpenAINonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	normalized, err := cliproxyCodexCompletedResponse(raw)
	if err != nil {
		return nil, err
	}
	return cliproxyJSONResponse("codex response to openai", normalized, oaicodex.ConvertCodexResponseToOpenAINonStream(ctx, model, original, translated, normalized, nil))
}

func cliproxyAnthropicRequestToGemini(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateAnthropicRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("anthropic request to gemini", raw, claudegemini.ConvertClaudeRequestToGemini(model, raw, stream))
}

func cliproxyGeminiResponseToAnthropicStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	data, err := cliproxySSEDataEvent(raw)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if param == nil {
		var local any
		param = &local
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data:")))
	if bytes.Equal(payload, []byte("[DONE]")) {
		data = payload
	}
	return cliproxyFrameStreamChunks(cliproxyStreamAnthropic, claudegemini.ConvertGeminiResponseToClaude(ctx, model, original, translated, data, param))
}

func cliproxyGeminiResponseToAnthropicNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	return cliproxyJSONResponse("gemini response to anthropic", raw, claudegemini.ConvertGeminiResponseToClaudeNonStream(ctx, model, original, translated, raw, nil))
}

func cliproxyGeminiRequestToAnthropic(model string, raw []byte, stream bool) ([]byte, error) {
	return cliproxyJSONRequest("gemini request to anthropic", raw, gemini.ConvertGeminiRequestToClaude(model, raw, stream))
}

func cliproxyAnthropicResponseToGeminiStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamGemini, gemini.ConvertClaudeResponseToGemini)
}

func cliproxyAnthropicResponseToGeminiNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	normalized, err := translatorcommon.NormalizeAnthropicResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize anthropic response to gemini: %w", err)
	}
	return cliproxyJSONResponse("anthropic response to gemini", normalized, gemini.ConvertClaudeResponseToGeminiNonStream(ctx, model, original, translated, normalized, nil))
}

func cliproxyCodexRequestToGemini(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateCodexRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("codex request to gemini", raw, openairespgemini.ConvertOpenAIResponsesRequestToGemini(model, raw, stream))
}

func cliproxyGeminiResponseToCodexStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamCodex, openairespgemini.ConvertGeminiResponseToOpenAIResponses)
}

func cliproxyGeminiResponseToCodexNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	return cliproxyJSONResponse("gemini response to codex", raw, openairespgemini.ConvertGeminiResponseToOpenAIResponsesNonStream(ctx, model, original, translated, raw, nil))
}

func cliproxyGeminiRequestToCodex(model string, raw []byte, stream bool) ([]byte, error) {
	return cliproxyJSONRequest("gemini request to codex", raw, geminicodex.ConvertGeminiRequestToCodex(model, raw, stream))
}

func cliproxyCodexResponseToGeminiStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamGemini, geminicodex.ConvertCodexResponseToGemini)
}

func cliproxyCodexResponseToGeminiNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	normalized, err := cliproxyCodexCompletedResponse(raw)
	if err != nil {
		return nil, err
	}
	return cliproxyJSONResponse("codex response to gemini", normalized, geminicodex.ConvertCodexResponseToGeminiNonStream(ctx, model, original, translated, normalized, nil))
}

func cliproxyCodexRequestToAnthropic(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateCodexRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("codex request to anthropic", raw, oairespclaude.ConvertOpenAIResponsesRequestToClaude(model, raw, stream))
}

func cliproxyAnthropicResponseToCodexStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamCodex, oairespclaude.ConvertClaudeResponseToOpenAIResponses)
}

func cliproxyAnthropicResponseToCodexNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	normalized, err := translatorcommon.NormalizeAnthropicResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize anthropic response to codex: %w", err)
	}
	return cliproxyJSONResponse("anthropic response to codex", normalized, oairespclaude.ConvertClaudeResponseToOpenAIResponsesNonStream(ctx, model, original, translated, normalized, nil))
}

func cliproxyAnthropicRequestToCodex(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateAnthropicRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("anthropic request to codex", raw, claudecodex.ConvertClaudeRequestToCodex(model, raw, stream))
}

func cliproxyCodexResponseToAnthropicStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateStream(ctx, model, original, translated, raw, param, cliproxyStreamAnthropic, claudecodex.ConvertCodexResponseToClaude)
}

func cliproxyCodexResponseToAnthropicNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	normalized, err := cliproxyCodexCompletedResponse(raw)
	if err != nil {
		return nil, err
	}
	return cliproxyJSONResponse("codex response to anthropic", normalized, claudecodex.ConvertCodexResponseToClaudeNonStream(ctx, model, original, translated, normalized, nil))
}

func cliproxyCodexRequestToOpenAI(model string, raw []byte, stream bool) ([]byte, error) {
	if err := cliproxyValidateCodexRequest(raw); err != nil {
		return nil, err
	}
	return cliproxyJSONRequest("codex request to openai", raw, openairesponses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions(model, raw, stream))
}

func cliproxyOpenAIResponseToCodexStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	return cliproxyTranslateOpenAIToCodexStream(ctx, model, original, translated, raw, param)
}

func cliproxyOpenAIResponseToCodexNonStream(ctx context.Context, model string, original, translated, raw []byte) ([]byte, error) {
	return cliproxyJSONResponse("openai response to codex", raw, openairesponses.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(ctx, model, original, translated, raw, nil))
}

func cliproxyJSONRequest(label string, input []byte, output []byte) ([]byte, error) {
	if !json.Valid(input) {
		return nil, fmt.Errorf("invalid %s JSON", label)
	}
	if len(output) == 0 || !json.Valid(output) {
		return nil, fmt.Errorf("translate %s", label)
	}
	return output, nil
}

func cliproxyJSONResponse(label string, input []byte, output []byte) ([]byte, error) {
	if !json.Valid(input) {
		return nil, fmt.Errorf("invalid %s JSON", label)
	}
	if len(output) == 0 || !json.Valid(output) {
		return nil, fmt.Errorf("translate %s", label)
	}
	return output, nil
}

func cliproxyValidateOpenAIRequest(raw []byte) error {
	var request struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("decode openai request: %w", err)
	}
	for _, message := range request.Messages {
		var blocks []any
		switch content := message.Content.(type) {
		case nil, string:
			continue
		case []any:
			blocks = content
		case map[string]any:
			blocks = []any{content}
		default:
			return fmt.Errorf("unsupported OpenAI message content %T", message.Content)
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				return fmt.Errorf("unsupported OpenAI content block %T", rawBlock)
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text", "image_url", "input_audio", "video_url", "file", "refusal":
			default:
				return fmt.Errorf("unsupported OpenAI content block type %q", blockType)
			}
		}
	}
	return nil
}

func cliproxyValidateAnthropicRequest(raw []byte) error {
	var request struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("decode anthropic request: %w", err)
	}
	for _, message := range request.Messages {
		blocks, ok := message.Content.([]any)
		if !ok {
			if _, stringContent := message.Content.(string); stringContent || message.Content == nil {
				continue
			}
			return fmt.Errorf("unsupported Anthropic message content %T", message.Content)
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				return fmt.Errorf("unsupported Anthropic content block %T", rawBlock)
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text", "image", "document", "tool_use", "tool_result", "thinking", "redacted_thinking", "server_tool_use", "web_search_tool_result":
			default:
				return fmt.Errorf("unsupported Anthropic content block type %q", blockType)
			}
		}
	}
	return nil
}

func cliproxyValidateCodexRequest(raw []byte) error {
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return fmt.Errorf("decode Codex request: %w", err)
	}
	for _, item := range request.Input {
		itemType, _ := item["type"].(string)
		if itemType == "" {
			if _, hasRole := item["role"]; hasRole {
				itemType = "message"
			}
		}
		switch itemType {
		case "message":
			content, ok := item["content"].([]any)
			if !ok {
				if _, stringContent := item["content"].(string); stringContent || item["content"] == nil {
					continue
				}
				return fmt.Errorf("unsupported Codex message content %T", item["content"])
			}
			for _, rawBlock := range content {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					return fmt.Errorf("unsupported Codex content block %T", rawBlock)
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "input_text", "output_text", "input_image", "input_file", "refusal":
				default:
					return fmt.Errorf("unsupported Codex content block type %q", blockType)
				}
			}
		case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "reasoning", "web_search_call", "computer_call", "computer_call_output", "image_generation_call", "local_shell_call", "local_shell_call_output", "shell_call", "shell_call_output", "apply_patch_call", "apply_patch_call_output", "additional_tools":
		default:
			return fmt.Errorf("unsupported Codex input item type %q", itemType)
		}
	}
	return nil
}

func cliproxyCodexCompletedResponse(raw []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("decode codex response: %w", err)
	}
	if _, wrapped := root["response"]; wrapped {
		return raw, nil
	}
	wrapped, err := json.Marshal(map[string]json.RawMessage{
		"type":     json.RawMessage(`"response.completed"`),
		"response": raw,
	})
	if err != nil {
		return nil, fmt.Errorf("wrap codex response: %w", err)
	}
	return wrapped, nil
}

func cliproxySSEDataEvent(rawEvent []byte) ([]byte, error) {
	var dataLines [][]byte
	var eventType string
	for _, line := range bytes.Split(rawEvent, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.HasPrefix(line, []byte("event:")) {
			eventType = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		dataLines = append(dataLines, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
	}
	if len(dataLines) == 0 {
		return nil, nil
	}
	payload := bytes.Join(dataLines, []byte{'\n'})
	if string(payload) != "[DONE]" && !json.Valid(payload) {
		return nil, fmt.Errorf("invalid codex SSE data: %s", strings.TrimSpace(string(payload)))
	}
	payload = cliproxyNormalizeSSEPayload(eventType, payload)
	out := make([]byte, 0, len(payload)+6)
	out = append(out, "data: "...)
	out = append(out, payload...)
	return out, nil
}

func cliproxyNormalizeSSEPayload(eventType string, payload []byte) []byte {
	if eventType == "" || !json.Valid(payload) {
		return payload
	}
	root := gjson.ParseBytes(payload)
	payloadType := root.Get("type").String()
	if payloadType == "" {
		payload, _ = sjson.SetBytes(payload, "type", eventType)
		payloadType = eventType
	}
	if payloadType != "content_block_delta" || root.Get("delta.type").Exists() {
		return payload
	}
	var deltaType string
	switch {
	case root.Get("delta.text").Exists():
		deltaType = "text_delta"
	case root.Get("delta.thinking").Exists():
		deltaType = "thinking_delta"
	case root.Get("delta.signature").Exists():
		deltaType = "signature_delta"
	case root.Get("delta.partial_json").Exists():
		deltaType = "input_json_delta"
	}
	if deltaType != "" {
		payload, _ = sjson.SetBytes(payload, "delta.type", deltaType)
	}
	return payload
}

type cliproxyStreamFunc func(context.Context, string, []byte, []byte, []byte, *any) [][]byte

type cliproxyStreamProtocol uint8

const (
	cliproxyStreamOpenAI cliproxyStreamProtocol = iota
	cliproxyStreamGemini
	cliproxyStreamAnthropic
	cliproxyStreamCodex
)

type cliproxyOpenAIToGeminiStreamState struct {
	chat      any
	responses any
	response  bool
}

type cliproxyOpenAIToAnthropicStreamState struct {
	chat      any
	responses any
	response  bool
}

type cliproxyOpenAIToCodexStreamState struct {
	chat     any
	response bool
}

func cliproxyTranslateOpenAIToCodexStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	data, err := cliproxySSEDataEvent(raw)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &cliproxyOpenAIToCodexStreamState{}
	}
	state, ok := (*param).(*cliproxyOpenAIToCodexStreamState)
	if !ok {
		return nil, fmt.Errorf("invalid OpenAI to Codex stream state %T", *param)
	}

	payload := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data:")))
	if !bytes.Equal(payload, []byte("[DONE]")) {
		root := gjson.ParseBytes(payload)
		if strings.HasPrefix(root.Get("type").String(), "response.") {
			state.response = true
			return cliproxyFrameStreamChunks(cliproxyStreamCodex, [][]byte{payload})
		}
		if !state.response && root.Get("choices.0.delta.content").String() == "\u200b" {
			return nil, nil
		}
	}
	if state.response {
		return nil, nil
	}
	return cliproxyFrameStreamChunks(cliproxyStreamCodex, openairesponses.ConvertOpenAIChatCompletionsResponseToOpenAIResponses(ctx, model, original, translated, data, &state.chat))
}

func cliproxyTranslateOpenAIToAnthropicStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	data, err := cliproxySSEDataEvent(raw)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &cliproxyOpenAIToAnthropicStreamState{}
	}
	state, ok := (*param).(*cliproxyOpenAIToAnthropicStreamState)
	if !ok {
		return nil, fmt.Errorf("invalid OpenAI to Anthropic stream state %T", *param)
	}

	payload := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data:")))
	if !bytes.Equal(payload, []byte("[DONE]")) {
		root := gjson.ParseBytes(payload)
		if strings.HasPrefix(root.Get("type").String(), "response.") {
			state.response = true
		} else if !state.response && root.Get("choices.0.delta.content").String() == "\u200b" {
			return nil, nil
		}
	}

	var chunks [][]byte
	if state.response {
		chunks = claudecodex.ConvertCodexResponseToClaude(ctx, model, original, translated, data, &state.responses)
	} else {
		chunks = claude.ConvertOpenAIResponseToClaude(ctx, model, original, translated, data, &state.chat)
	}
	return cliproxyFrameStreamChunks(cliproxyStreamAnthropic, chunks)
}

func cliproxyTranslateOpenAIToGeminiStream(ctx context.Context, model string, original, translated, raw []byte, param *any) ([][]byte, error) {
	data, err := cliproxySSEDataEvent(raw)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &cliproxyOpenAIToGeminiStreamState{}
	}
	state, ok := (*param).(*cliproxyOpenAIToGeminiStreamState)
	if !ok {
		return nil, fmt.Errorf("invalid OpenAI to Gemini stream state %T", *param)
	}

	payload := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data:")))
	if !bytes.Equal(payload, []byte("[DONE]")) {
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, fmt.Errorf("decode OpenAI stream event: %w", err)
		}
		if strings.HasPrefix(event.Type, "response.") {
			state.response = true
		}
	}

	var chunks [][]byte
	if state.response {
		chunks = geminicodex.ConvertCodexResponseToGemini(ctx, model, original, translated, data, &state.responses)
	} else {
		chunks = openaigemini.ConvertOpenAIResponseToGemini(ctx, model, original, translated, data, &state.chat)
	}
	return cliproxyFrameStreamChunks(cliproxyStreamGemini, chunks)
}

func cliproxyTranslateStream(ctx context.Context, model string, original, translated, raw []byte, param *any, target cliproxyStreamProtocol, translate cliproxyStreamFunc) ([][]byte, error) {
	data, err := cliproxySSEDataEvent(raw)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if param == nil {
		var local any
		param = &local
	}
	return cliproxyFrameStreamChunks(target, translate(ctx, model, original, translated, data, param))
}

func cliproxyFrameStreamChunks(target cliproxyStreamProtocol, chunks [][]byte) ([][]byte, error) {
	framed := make([][]byte, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = bytes.TrimSpace(chunk)
		if len(chunk) == 0 {
			continue
		}
		if bytes.HasPrefix(chunk, []byte("data:")) || bytes.HasPrefix(chunk, []byte("event:")) {
			framed = append(framed, cliproxySplitSSEEvents(chunk)...)
			continue
		}
		if bytes.Equal(chunk, []byte("[DONE]")) {
			framed = append(framed, []byte("data: [DONE]\n\n"))
			continue
		}
		if !json.Valid(chunk) {
			return nil, fmt.Errorf("translator emitted invalid stream JSON: %s", chunk)
		}
		switch target {
		case cliproxyStreamOpenAI, cliproxyStreamGemini:
			out := make([]byte, 0, len(chunk)+8)
			out = append(out, "data: "...)
			out = append(out, chunk...)
			out = append(out, '\n', '\n')
			framed = append(framed, out)
		case cliproxyStreamAnthropic, cliproxyStreamCodex:
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(chunk, &event); err != nil {
				return nil, fmt.Errorf("decode translated stream event: %w", err)
			}
			if event.Type == "" {
				return nil, fmt.Errorf("translated stream event has no type: %s", chunk)
			}
			out := make([]byte, 0, len(chunk)+len(event.Type)+16)
			out = append(out, "event: "...)
			out = append(out, event.Type...)
			out = append(out, '\n')
			out = append(out, "data: "...)
			out = append(out, chunk...)
			out = append(out, '\n', '\n')
			framed = append(framed, out)
		}
	}
	if len(framed) == 0 {
		return nil, nil
	}
	return framed, nil
}

func cliproxySplitSSEEvents(chunk []byte) [][]byte {
	normalized := bytes.ReplaceAll(chunk, []byte("\r\n"), []byte("\n"))
	blocks := bytes.Split(normalized, []byte("\n\n"))
	out := make([][]byte, 0, len(blocks))
	for _, block := range blocks {
		block = bytes.TrimSpace(block)
		if len(block) == 0 {
			continue
		}
		framed := append([]byte(nil), block...)
		framed = append(framed, '\n', '\n')
		out = append(out, framed)
	}
	return out
}
