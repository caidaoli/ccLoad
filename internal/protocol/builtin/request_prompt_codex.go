package builtin

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

func encodeCodexRequest(model string, conv conversation, stream bool) ([]byte, error) {
	systemText, turns, err := collectSystemText(conv)
	if err != nil {
		return nil, err
	}
	toolAliases := buildCodexToolAliases(collectCodexAliasNames(conv))
	out := codexRequestPayload{
		Model: model,
		Input: make([]map[string]any, 0, len(turns)),
	}
	if stream {
		out.Stream = util.FlexibleBool(true)
	}
	if systemText != "" {
		out.Instructions = systemText
	}
	if len(conv.Tools) > 0 {
		tools := make([]map[string]any, 0, len(conv.Tools))
		for _, tool := range conv.Tools {
			if tool.toolType() == "function" {
				item := map[string]any{"type": "function", "name": toolAliases.shorten(tool.Name)}
				if tool.Description != "" {
					item["description"] = tool.Description
				}
				if anySchema, err := rawJSONToAny(tool.InputSchema); err == nil && anySchema != nil {
					item["parameters"] = anySchema
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
		out.Tools = tools
	}
	if !conv.ToolChoice.IsZero() {
		switch conv.ToolChoice.Mode {
		case "auto", "none", "required":
			out.ToolChoice = conv.ToolChoice.Mode
		case "named":
			if conv.ToolChoice.toolType() == "function" {
				out.ToolChoice = map[string]any{
					"type": "function",
					"name": toolAliases.shorten(conv.ToolChoice.Name),
				}
			} else {
				out.ToolChoice = map[string]any{"type": conv.ToolChoice.toolType()}
			}
		default:
			return nil, fmt.Errorf("%w: unsupported codex tool choice %q", protocol.ErrUnsupportedRequestShape, conv.ToolChoice.Mode)
		}
	}

	input := out.Input
	for i, turn := range turns {
		role := normalizeRole(turn.Role)
		switch role {
		case "user", "assistant", "developer", "system":
			messageParts := make([]map[string]any, 0, len(turn.Parts))
			for _, part := range turn.Parts {
				switch part.Kind {
				case partKindText, partKindImage, partKindFile:
					var (
						encoded map[string]any
						err     error
					)
					if role == "assistant" {
						encoded, err = encodeCodexOutputContentPart(part)
					} else {
						encoded, err = encodeCodexContentPart(part)
					}
					if err != nil {
						return nil, fmt.Errorf("codex turn %d: %w", i, err)
					}
					messageParts = append(messageParts, encoded)
				case partKindToolCall:
					if len(messageParts) > 0 {
						input = append(input, map[string]any{"type": "message", "role": role, "content": messageParts})
						messageParts = nil
					}
					encoded, err := encodeCodexToolCallWithAliases(part.ToolCall, toolAliases)
					if err != nil {
						return nil, fmt.Errorf("codex turn %d: %w", i, err)
					}
					input = append(input, encoded)
				case partKindToolResult:
					if len(messageParts) > 0 {
						input = append(input, map[string]any{"type": "message", "role": role, "content": messageParts})
						messageParts = nil
					}
					encoded, err := encodeCodexToolResultWithAliases(part.ToolResult, toolAliases)
					if err != nil {
						return nil, fmt.Errorf("codex turn %d: %w", i, err)
					}
					input = append(input, encoded)
				case partKindReasoning:
					if role != "assistant" {
						continue
					}
					if len(messageParts) > 0 {
						input = append(input, map[string]any{"type": "message", "role": role, "content": messageParts})
						messageParts = nil
					}
					encoded := encodeCodexReasoningPart(part.Reasoning)
					if encoded != nil {
						input = append(input, encoded)
					}
				default:
					return nil, fmt.Errorf("%w: unsupported codex content kind %q", protocol.ErrUnsupportedRequestShape, part.Kind)
				}
			}
			if len(messageParts) > 0 {
				input = append(input, map[string]any{"type": "message", "role": role, "content": messageParts})
			}
		case "tool":
			for _, part := range turn.Parts {
				if part.Kind != partKindToolResult || part.ToolResult == nil {
					return nil, fmt.Errorf("%w: codex tool role only supports tool results", protocol.ErrUnsupportedRequestShape)
				}
				encoded, err := encodeCodexToolResultWithAliases(part.ToolResult, toolAliases)
				if err != nil {
					return nil, fmt.Errorf("codex turn %d: %w", i, err)
				}
				input = append(input, encoded)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported codex role %q", protocol.ErrUnsupportedRequestShape, turn.Role)
		}
	}
	out.Input = input
	if len(input) == 0 {
		if systemText == "" {
			return nil, fmt.Errorf("%w: no convertible codex input", protocol.ErrUnsupportedRequestShape)
		}
		// Responses-style Codex requests can rely on instructions alone. In that
		// case omit `input` entirely instead of rejecting the transform.
		out.Input = nil
	}
	if conv.ToolChoice.DisableParallel && len(conv.Tools) > 0 {
		f := false
		out.ParallelToolCalls = &f
	}
	applyCodexSampling(&out, conv.Sampling)
	if reasoning := buildCodexReasoningConfig(conv); reasoning != nil {
		out.Reasoning = reasoning
		out.Include = []string{"reasoning.encrypted_content"}
	}
	return marshalStableJSON(out)
}

// applyCodexSampling 把 Codex responses API 支持的采样参数写入 out map。
// 只透传 Codex 实际接受的字段：temperature/top_p/max_output_tokens/user；其余静默丢弃。
func applyCodexSampling(out *codexRequestPayload, sp *samplingParams) {
	if sp == nil {
		return
	}
	if sp.Temperature != nil {
		out.Temperature = sp.Temperature
	}
	if sp.TopP != nil {
		out.TopP = sp.TopP
	}
	if sp.MaxTokens != nil && *sp.MaxTokens > 0 {
		out.MaxOutputTokens = sp.MaxTokens
	}
	if sp.User != "" {
		out.User = sp.User
	}
}

// buildCodexReasoningConfig 在以下情形输出 reasoning 配置：
// 1. Anthropic 顶层 thinking.type=enabled（来自 Anthropic 客户端）
// 2. OpenAI 顶层 reasoning_effort 非空（来自 OpenAI 客户端，优先直通枚举值）
// 未触发返回 nil，避免给非 reasoning 模型硬塞导致上游 400。
func buildCodexReasoningConfig(conv conversation) map[string]any {
	if conv.Sampling != nil {
		if effort := strings.ToLower(strings.TrimSpace(conv.Sampling.ReasoningEffort)); effort != "" && effort != "none" {
			return map[string]any{
				"effort":  normalizeOpenAIEffort(effort),
				"summary": "auto",
			}
		}
	}
	thinking := conv.Thinking
	if thinking == nil || strings.ToLower(strings.TrimSpace(thinking.Type)) != "enabled" {
		return nil
	}
	return map[string]any{
		"effort":  mapAnthropicBudgetToOpenAIEffort(thinking.BudgetTokens),
		"summary": "auto",
	}
}

func encodeCodexContentPart(part conversationPart) (map[string]any, error) {
	switch part.Kind {
	case partKindText:
		return map[string]any{"type": "input_text", "text": part.Text}, nil
	case partKindImage:
		if part.Media == nil {
			return nil, fmt.Errorf("%w: missing codex image content", protocol.ErrUnsupportedRequestShape)
		}
		item := map[string]any{"type": "input_image"}
		switch {
		case part.Media.FileID != "":
			item["file_id"] = part.Media.FileID
		case part.Media.URL != "":
			item["image_url"] = part.Media.URL
		case part.Media.Data != "":
			item["data"] = part.Media.Data
		default:
			return nil, fmt.Errorf("%w: codex image requires file_id, image_url, or data", protocol.ErrUnsupportedRequestShape)
		}
		if part.Media.Detail != "" {
			item["detail"] = part.Media.Detail
		}
		if part.Media.MIMEType != "" {
			item["mime_type"] = part.Media.MIMEType
		}
		return item, nil
	case partKindFile:
		if part.Media == nil {
			return nil, fmt.Errorf("%w: missing codex file content", protocol.ErrUnsupportedRequestShape)
		}
		item := map[string]any{"type": "input_file"}
		switch {
		case part.Media.FileID != "":
			item["file_id"] = part.Media.FileID
		case part.Media.Data != "":
			item["file_data"] = part.Media.Data
		default:
			return nil, fmt.Errorf("%w: codex file requires file_id or file_data", protocol.ErrUnsupportedRequestShape)
		}
		if part.Media.Filename != "" {
			item["filename"] = part.Media.Filename
		}
		if part.Media.MIMEType != "" {
			item["mime_type"] = part.Media.MIMEType
		}
		return item, nil
	default:
		return nil, fmt.Errorf("%w: unsupported codex content kind %q", protocol.ErrUnsupportedRequestShape, part.Kind)
	}
}

func encodeCodexToolCall(call *conversationToolCall) (map[string]any, error) {
	return encodeCodexToolCallWithAliases(call, codexToolAliases{})
}

func encodeCodexToolCallWithAliases(call *conversationToolCall, aliases codexToolAliases) (map[string]any, error) {
	if call == nil {
		return nil, fmt.Errorf("%w: missing codex tool call", protocol.ErrUnsupportedRequestShape)
	}
	arguments := strings.TrimSpace(string(call.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   call.ID,
		"name":      aliases.shorten(call.Name),
		"arguments": arguments,
	}, nil
}

func encodeCodexToolResultWithAliases(result *conversationToolResult, aliases codexToolAliases) (map[string]any, error) {
	if result == nil {
		return nil, fmt.Errorf("%w: missing codex tool result", protocol.ErrUnsupportedRequestShape)
	}
	output, err := encodeCodexToolResultOutput(result.Parts)
	if err != nil {
		return nil, err
	}
	item := map[string]any{
		"type":    "function_call_output",
		"call_id": result.CallID,
		"output":  output,
	}
	if result.Name != "" {
		item["name"] = aliases.shorten(result.Name)
	}
	return item, nil
}

func encodeCodexToolResultOutput(parts []conversationPart) (any, error) {
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
	encoded := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		item, err := encodeCodexContentPart(part)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, item)
	}
	return encoded, nil
}

func extractCodexContentParts(content any) ([]conversationPart, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		return appendTextPart(nil, v), nil
	case []map[string]any:
		parts := make([]conversationPart, 0, len(v))
		for i, item := range v {
			part, err := decodeCodexContentPart(item)
			if err != nil {
				return nil, fmt.Errorf("codex content %d: %w", i, err)
			}
			if part.Kind != "" {
				parts = append(parts, part)
			}
		}
		return parts, nil
	case []any:
		parts := make([]conversationPart, 0, len(v))
		for i, item := range v {
			partMap, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: unsupported codex content part at index %d", protocol.ErrUnsupportedRequestShape, i)
			}
			part, err := decodeCodexContentPart(partMap)
			if err != nil {
				return nil, fmt.Errorf("codex content %d: %w", i, err)
			}
			if part.Kind != "" {
				parts = append(parts, part)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("%w: unsupported codex content type %T", protocol.ErrUnsupportedRequestShape, content)
	}
}

func decodeCodexContentPart(part map[string]any) (conversationPart, error) {
	typ := normalizeRole(stringValue(part["type"]))
	switch typ {
	case "input_text", "output_text", "text":
		text := stringValue(part["text"])
		if text == "" {
			if output, ok := part["output"].(string); ok {
				text = output
			}
		}
		if text == "" {
			return conversationPart{}, nil
		}
		return conversationPart{Kind: partKindText, Text: text}, nil
	case "input_image", "image":
		media, err := decodeCodexImageMedia(part)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindImage, Media: &media}, nil
	case "input_file", "file", "document":
		media, err := decodeCodexFileMedia(part)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindFile, Media: &media}, nil
	case "function_call":
		call, err := decodeCodexToolCall(part)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindToolCall, ToolCall: &call}, nil
	case "function_call_output":
		result, err := decodeCodexToolResult(part)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindToolResult, ToolResult: &result}, nil
	default:
		return conversationPart{}, fmt.Errorf("%w: unsupported codex content part type %q", protocol.ErrUnsupportedRequestShape, typ)
	}
}

func decodeCodexToolCall(item map[string]any) (conversationToolCall, error) {
	arguments, err := rawJSONFromFields(item, "arguments")
	if err != nil {
		return conversationToolCall{}, err
	}
	if !hasJSONValue(arguments) {
		arguments = json.RawMessage(`{}`)
	}
	call := conversationToolCall{
		ID:        firstNonEmptyString(item, "call_id", "id"),
		Name:      strings.TrimSpace(stringValue(item["name"])),
		Arguments: arguments,
	}
	if call.Name == "" {
		return conversationToolCall{}, fmt.Errorf("%w: codex function_call missing name", protocol.ErrUnsupportedRequestShape)
	}
	if call.ID == "" {
		call.ID = call.Name
	}
	return call, nil
}

func decodeCodexToolResult(item map[string]any) (conversationToolResult, error) {
	callID := firstNonEmptyString(item, "call_id", "id")
	if callID == "" {
		return conversationToolResult{}, fmt.Errorf("%w: codex function_call_output missing call_id", protocol.ErrUnsupportedRequestShape)
	}
	parts, err := decodeToolResultParts(item["output"])
	if err != nil {
		return conversationToolResult{}, err
	}
	if len(parts) == 0 {
		parts, err = decodeToolResultParts(item["content"])
		if err != nil {
			return conversationToolResult{}, err
		}
	}
	return conversationToolResult{
		CallID:  callID,
		Name:    strings.TrimSpace(stringValue(item["name"])),
		IsError: boolValue(item["is_error"]),
		Parts:   parts,
	}, nil
}

func decodeToolResultParts(value any) ([]conversationPart, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return appendTextPart(nil, v), nil
	case []any, []map[string]any:
		return extractCodexContentParts(v)
	default:
		jsonText, err := sonic.Marshal(v)
		if err != nil {
			return nil, err
		}
		return []conversationPart{{Kind: partKindText, Text: string(jsonText)}}, nil
	}
}

func decodeCodexImageMedia(part map[string]any) (conversationMedia, error) {
	media := conversationMedia{
		URL:      firstNonEmptyString(part, "image_url", "url"),
		FileID:   firstNonEmptyString(part, "file_id"),
		MIMEType: firstNonEmptyString(part, "mime_type", "media_type"),
		Data:     firstNonEmptyString(part, "data", "image_data"),
		Detail:   firstNonEmptyString(part, "detail"),
	}
	if media.URL == "" && media.FileID == "" && media.Data == "" {
		return conversationMedia{}, fmt.Errorf("%w: codex image part missing image_url/file_id/data", protocol.ErrUnsupportedRequestShape)
	}
	return media, nil
}

func decodeCodexFileMedia(part map[string]any) (conversationMedia, error) {
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
		return conversationMedia{}, fmt.Errorf("%w: codex file part missing file_id/file_data", protocol.ErrUnsupportedRequestShape)
	}
	return media, nil
}
