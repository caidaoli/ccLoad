package builtin

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

func encodeOpenAIRequest(model string, conv conversation, stream bool) ([]byte, error) {
	messages := make([]map[string]any, 0, len(conv.Turns)+2)
	for i, turn := range conv.Turns {
		role := normalizeRole(turn.Role)
		switch role {
		case "system", "developer", "user", "assistant":
			contentParts := make([]map[string]any, 0, len(turn.Parts))
			toolCalls := make([]map[string]any, 0)
			reasoningTexts := make([]string, 0)
			pendingToolMessages := make([]map[string]any, 0)
			for _, part := range turn.Parts {
				switch part.Kind {
				case partKindText, partKindImage, partKindFile:
					encoded, err := encodeOpenAIContentPart(part)
					if err != nil {
						return nil, fmt.Errorf("openai turn %d: %w", i, err)
					}
					contentParts = append(contentParts, encoded)
				case partKindToolCall:
					if role != "assistant" {
						return nil, fmt.Errorf("%w: openai tool calls require assistant role", protocol.ErrUnsupportedRequestShape)
					}
					encoded, err := encodeOpenAIToolCall(part.ToolCall)
					if err != nil {
						return nil, fmt.Errorf("openai turn %d: %w", i, err)
					}
					toolCalls = append(toolCalls, encoded)
				case partKindToolResult:
					if part.ToolResult == nil {
						return nil, fmt.Errorf("%w: missing openai tool result content", protocol.ErrUnsupportedRequestShape)
					}
					content, err := encodeOpenAIToolResultContent(part.ToolResult.Parts)
					if err != nil {
						return nil, fmt.Errorf("openai turn %d: %w", i, err)
					}
					toolMsg := map[string]any{
						"role":         "tool",
						"tool_call_id": part.ToolResult.CallID,
						"content":      content,
					}
					if part.ToolResult.Name != "" {
						toolMsg["name"] = part.ToolResult.Name
					}
					pendingToolMessages = append(pendingToolMessages, toolMsg)
				case partKindReasoning:
					if role != "assistant" {
						continue
					}
					if part.Reasoning != nil {
						if text := strings.TrimSpace(part.Reasoning.Text); text != "" {
							reasoningTexts = append(reasoningTexts, text)
						}
					}
				default:
					return nil, fmt.Errorf("%w: unsupported openai content kind %q", protocol.ErrUnsupportedRequestShape, part.Kind)
				}
			}
			// OpenAI: tool messages must immediately follow the previous assistant tool_calls.
			// Emit any tool_result collected in this turn first, before the current turn's main message.
			messages = append(messages, pendingToolMessages...)
			if len(contentParts) > 0 || len(toolCalls) > 0 || len(reasoningTexts) > 0 {
				message := map[string]any{"role": role, "content": encodeOpenAIContentValue(contentParts)}
				if len(toolCalls) > 0 {
					message["tool_calls"] = toolCalls
				}
				if role == "assistant" && len(reasoningTexts) > 0 {
					message["reasoning_content"] = strings.Join(reasoningTexts, "\n\n")
				}
				messages = append(messages, message)
			}
		case "tool":
			for _, part := range turn.Parts {
				if part.Kind != partKindToolResult || part.ToolResult == nil {
					return nil, fmt.Errorf("%w: openai tool role only supports tool results", protocol.ErrUnsupportedRequestShape)
				}
				content, err := encodeOpenAIToolResultContent(part.ToolResult.Parts)
				if err != nil {
					return nil, fmt.Errorf("openai turn %d: %w", i, err)
				}
				message := map[string]any{
					"role":         "tool",
					"tool_call_id": part.ToolResult.CallID,
					"content":      content,
				}
				if part.ToolResult.Name != "" {
					message["name"] = part.ToolResult.Name
				}
				messages = append(messages, message)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported openai role %q", protocol.ErrUnsupportedRequestShape, turn.Role)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("%w: no convertible openai messages", protocol.ErrUnsupportedRequestShape)
	}
	payload := openAIChatRequest{Model: model, Messages: make([]openAIChatMessage, 0, len(messages)), Stream: util.FlexibleBool(stream)}
	for _, message := range messages {
		encoded := openAIChatMessage{Role: stringValue(message["role"]), Content: message["content"]}
		if rawCalls, ok := message["tool_calls"]; ok {
			callBytes, err := marshalStableJSON(rawCalls)
			if err != nil {
				return nil, err
			}
			if err := sonic.Unmarshal(callBytes, &encoded.ToolCalls); err != nil {
				return nil, err
			}
		}
		encoded.ToolCallID = stringValue(message["tool_call_id"])
		encoded.Name = stringValue(message["name"])
		encoded.ReasoningContent = stringValue(message["reasoning_content"])
		payload.Messages = append(payload.Messages, encoded)
	}
	if len(conv.Tools) > 0 {
		tools := make([]map[string]any, 0, len(conv.Tools))
		for _, tool := range conv.Tools {
			if tool.toolType() == "function" {
				item := map[string]any{
					"type": "function",
					"function": map[string]any{
						"name": tool.Name,
					},
				}
				if tool.Description != "" {
					item["function"].(map[string]any)["description"] = tool.Description
				}
				if anySchema, err := rawJSONToAny(tool.InputSchema); err == nil && anySchema != nil {
					item["function"].(map[string]any)["parameters"] = anySchema
				}
				tools = append(tools, item)
				continue
			}
			item := map[string]any{"type": tool.toolType()}
			for key, value := range tool.Options {
				item[key] = value
			}
			tools = append(tools, item)
		}
		var err error
		payload.Tools, err = marshalStableJSON(tools)
		if err != nil {
			return nil, err
		}
	}
	if !conv.ToolChoice.IsZero() {
		choice := encodeOpenAIToolChoice(conv.ToolChoice)
		if choice != nil {
			var err error
			payload.ToolChoice, err = marshalStableJSON(choice)
			if err != nil {
				return nil, err
			}
		}
	}
	if conv.ToolChoice.DisableParallel && len(conv.Tools) > 0 {
		f := false
		payload.ParallelToolCalls = &f
	}
	if sp := conv.Sampling; sp != nil {
		payload.Temperature = sp.Temperature
		payload.TopP = sp.TopP
		payload.TopK = sp.TopK
		payload.MaxTokens = sp.MaxTokens
		payload.Seed = sp.Seed
		payload.FrequencyPenalty = sp.FrequencyPenalty
		payload.PresencePenalty = sp.PresencePenalty
		payload.User = sp.User
		payload.ReasoningEffort = sp.ReasoningEffort
		if len(sp.Stop) > 0 {
			raw, err := marshalStableJSON(sp.Stop)
			if err != nil {
				return nil, err
			}
			payload.Stop = raw
		}
	}
	return marshalStableJSON(payload)
}

// normalizeOpenAIEffort 把 OpenAI reasoning_effort 枚举收敛到 Codex 接受的档位
// （low/medium/high）。minimal 归入 low，auto 归入 medium。
func normalizeOpenAIEffort(effort string) string {
	switch effort {
	case "minimal", "low":
		return "low"
	case "high":
		return "high"
	case "auto", "medium":
		return "medium"
	default:
		return "medium"
	}
}

// mapAnthropicBudgetToOpenAIEffort 把 Anthropic budget_tokens 映射成 OpenAI reasoning.effort 档位。
// 阈值参考 Anthropic 推荐范围：1024~4k=low，4k~16k=medium，16k+=high。
func mapAnthropicBudgetToOpenAIEffort(budget int) string {
	switch {
	case budget >= 16384:
		return "high"
	case budget >= 4096:
		return "medium"
	case budget > 0:
		return "low"
	default:
		return "medium"
	}
}

func encodeOpenAIContentValue(parts []map[string]any) any {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 && parts[0]["type"] == "text" {
		return stringValue(parts[0]["text"])
	}
	return parts
}

func encodeOpenAIContentPart(part conversationPart) (map[string]any, error) {
	switch part.Kind {
	case partKindText:
		return map[string]any{"type": "text", "text": part.Text}, nil
	case partKindImage:
		return encodeOpenAIImagePart(part.Media)
	case partKindFile:
		return encodeOpenAIFilePart(part.Media)
	default:
		return nil, fmt.Errorf("%w: unsupported openai content kind %q", protocol.ErrUnsupportedRequestShape, part.Kind)
	}
}

func encodeOpenAIImagePart(media *conversationMedia) (map[string]any, error) {
	if media == nil {
		return nil, fmt.Errorf("%w: missing openai image content", protocol.ErrUnsupportedRequestShape)
	}
	if media.FileID != "" {
		part := map[string]any{"type": "input_image", "file_id": media.FileID}
		if media.Detail != "" {
			part["detail"] = media.Detail
		}
		return part, nil
	}
	url := media.URL
	if url == "" && media.Data != "" {
		url = buildDataURL(media.MIMEType, media.Data)
	}
	if url == "" {
		return nil, fmt.Errorf("%w: openai image requires url, file_id, or inline data", protocol.ErrUnsupportedRequestShape)
	}
	payload := map[string]any{"url": url}
	if media.Detail != "" {
		payload["detail"] = media.Detail
	}
	return map[string]any{"type": "image_url", "image_url": payload}, nil
}

func encodeOpenAIFilePart(media *conversationMedia) (map[string]any, error) {
	if media == nil {
		return nil, fmt.Errorf("%w: missing openai file content", protocol.ErrUnsupportedRequestShape)
	}
	file := map[string]any{}
	switch {
	case media.FileID != "":
		file["file_id"] = media.FileID
	case media.Data != "":
		file["file_data"] = media.Data
	default:
		return nil, fmt.Errorf("%w: openai file requires file_id or file_data", protocol.ErrUnsupportedRequestShape)
	}
	if media.Filename != "" {
		file["filename"] = media.Filename
	}
	return map[string]any{"type": "file", "file": file}, nil
}

func encodeOpenAIToolCall(call *conversationToolCall) (map[string]any, error) {
	if call == nil {
		return nil, fmt.Errorf("%w: missing openai tool call", protocol.ErrUnsupportedRequestShape)
	}
	arguments := strings.TrimSpace(string(call.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	return map[string]any{
		"id":   call.ID,
		"type": "function",
		"function": map[string]any{
			"name":      call.Name,
			"arguments": arguments,
		},
	}, nil
}

func encodeOpenAIToolResultContent(parts []conversationPart) (any, error) {
	return encodeToolResultContent(parts)
}

func encodeOpenAIToolChoice(choice conversationToolChoice) any {
	switch choice.Mode {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "required":
		return "required"
	case "named":
		if choice.toolType() != "function" {
			return map[string]any{"type": choice.toolType()}
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	default:
		return nil
	}
}

func extractOpenAIContentParts(content any) ([]conversationPart, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		return appendTextPart(nil, v), nil
	case []any:
		parts := make([]conversationPart, 0, len(v))
		for i, item := range v {
			partMap, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: unsupported openai content part at index %d", protocol.ErrUnsupportedRequestShape, i)
			}
			part, err := decodeOpenAIContentPart(partMap)
			if err != nil {
				return nil, err
			}
			if part.Kind != "" {
				parts = append(parts, part)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("%w: unsupported openai content type %T", protocol.ErrUnsupportedRequestShape, content)
	}
}

func decodeOpenAIContentPart(part map[string]any) (conversationPart, error) {
	typ := normalizeRole(stringValue(part["type"]))
	switch typ {
	case "text", "":
		text := stringValue(part["text"])
		if typ == "" && text == "" {
			return conversationPart{}, fmt.Errorf("%w: unsupported openai content part", protocol.ErrUnsupportedRequestShape)
		}
		if text == "" {
			return conversationPart{}, nil
		}
		return conversationPart{Kind: partKindText, Text: text}, nil
	case "image_url", "input_image", "image":
		media, err := decodeOpenAIImageMedia(part)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindImage, Media: &media}, nil
	case "file", "input_file":
		media, err := decodeOpenAIFileMedia(part)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindFile, Media: &media}, nil
	default:
		return conversationPart{}, fmt.Errorf("%w: unsupported openai content part type %q", protocol.ErrUnsupportedRequestShape, typ)
	}
}

func extractOpenAIToolCallParts(calls []openAIChatToolCall) ([]conversationPart, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	parts := make([]conversationPart, 0, len(calls))
	for i, call := range calls {
		if normalizeRole(call.Type) != "function" {
			return nil, fmt.Errorf("%w: unsupported openai tool call type %q at index %d", protocol.ErrUnsupportedRequestShape, call.Type, i)
		}
		arguments := json.RawMessage(call.Function.Arguments)
		if !hasJSONValue(arguments) {
			arguments = json.RawMessage(`{}`)
		}
		parts = append(parts, conversationPart{Kind: partKindToolCall, ToolCall: &conversationToolCall{
			ID:        strings.TrimSpace(call.ID),
			Name:      strings.TrimSpace(call.Function.Name),
			Arguments: arguments,
		}})
	}
	return parts, nil
}

func decodeOpenAIImageMedia(part map[string]any) (conversationMedia, error) {
	media := conversationMedia{Detail: firstNonEmptyString(part, "detail")}
	switch value := part["image_url"].(type) {
	case string:
		media.URL = value
	case map[string]any:
		media.URL = firstNonEmptyString(value, "url")
		if media.Detail == "" {
			media.Detail = firstNonEmptyString(value, "detail")
		}
		media.FileID = firstNonEmptyString(value, "file_id")
	}
	if media.URL == "" {
		media.URL = firstNonEmptyString(part, "url")
	}
	if media.FileID == "" {
		media.FileID = firstNonEmptyString(part, "file_id")
	}
	if media.Data == "" {
		media.Data = firstNonEmptyString(part, "data", "image_data")
	}
	if media.MIMEType == "" {
		media.MIMEType = firstNonEmptyString(part, "mime_type", "media_type")
	}
	if media.URL == "" && media.FileID == "" && media.Data == "" {
		return conversationMedia{}, fmt.Errorf("%w: openai image part missing url/file_id/data", protocol.ErrUnsupportedRequestShape)
	}
	return media, nil
}

func decodeOpenAIFileMedia(part map[string]any) (conversationMedia, error) {
	fileMap, _ := part["file"].(map[string]any)
	media := conversationMedia{
		FileID:   firstNonEmptyString(fileMap, "file_id"),
		Data:     firstNonEmptyString(fileMap, "file_data", "data"),
		Filename: firstNonEmptyString(fileMap, "filename", "name"),
		MIMEType: firstNonEmptyString(fileMap, "mime_type", "media_type"),
	}
	if media.FileID == "" {
		media.FileID = firstNonEmptyString(part, "file_id")
	}
	if media.Data == "" {
		media.Data = firstNonEmptyString(part, "file_data", "data")
	}
	if media.Filename == "" {
		media.Filename = firstNonEmptyString(part, "filename", "name")
	}
	if media.MIMEType == "" {
		media.MIMEType = firstNonEmptyString(part, "mime_type", "media_type")
	}
	if media.FileID == "" && media.Data == "" {
		return conversationMedia{}, fmt.Errorf("%w: openai file part missing file_id/file_data", protocol.ErrUnsupportedRequestShape)
	}
	return media, nil
}

func encodeToolResultContent(parts []conversationPart) (any, error) {
	textOnly := true
	var builder strings.Builder
	for _, part := range parts {
		if part.Kind != partKindText {
			textOnly = false
			break
		}
		builder.WriteString(part.Text)
	}
	if textOnly {
		return builder.String(), nil
	}
	content := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case partKindText:
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case partKindImage:
			block, err := encodeOpenAIImagePart(part.Media)
			if err != nil {
				return nil, err
			}
			content = append(content, block)
		case partKindFile:
			block, err := encodeOpenAIFilePart(part.Media)
			if err != nil {
				return nil, err
			}
			content = append(content, block)
		default:
			return nil, fmt.Errorf("%w: unsupported nested tool result part %q", protocol.ErrUnsupportedRequestShape, part.Kind)
		}
	}
	return content, nil
}
