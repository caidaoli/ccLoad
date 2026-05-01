package builtin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

// newClaudeMetadataUserID 生成 Claude CLI 兼容的 metadata.user_id 值。
func newClaudeMetadataUserID() string {
	deviceID := make([]byte, 32)
	if _, err := rand.Read(deviceID); err != nil {
		return `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"","session_id":"00000000-0000-0000-0000-000000000000"}`
	}
	// session_id 使用统一 UUIDv4 实现（util.NewUUIDv4 在 rand 失败时返回 nil-v4 占位符）。
	return fmt.Sprintf(`{"device_id":"%s","account_uuid":"","session_id":"%s"}`,
		hex.EncodeToString(deviceID), util.NewUUIDv4())
}

func encodeAnthropicRequest(model string, conv conversation, stream bool) ([]byte, error) {
	systemParts, turns, err := splitConversationForSystem(conv)
	if err != nil {
		return nil, err
	}
	if len(turns) == 0 && len(systemParts) > 0 {
		turns = []conversationTurn{{
			Role:  "user",
			Parts: systemParts,
		}}
		systemParts = nil
	}
	out := anthropicMessagesRequest{
		Model:     model,
		Messages:  make([]anthropicMessageContent, 0, len(turns)),
		Stream:    util.FlexibleBool(stream),
		MaxTokens: 32000,
		Tools:     []byte("[]"),
		Metadata:  map[string]string{"user_id": newClaudeMetadataUserID()},
	}
	if sp := conv.Sampling; sp != nil {
		if sp.MaxTokens != nil && *sp.MaxTokens > 0 {
			out.MaxTokens = *sp.MaxTokens
		}
		out.Temperature = sp.Temperature
		out.TopP = sp.TopP
		out.TopK = sp.TopK
		if len(sp.Stop) > 0 {
			out.StopSequences = sp.Stop
		}
	}
	if conv.Thinking != nil && strings.TrimSpace(conv.Thinking.Type) != "" {
		out.Thinking = conv.Thinking
	}
	if len(systemParts) > 0 {
		blocks, err := encodeAnthropicBlocks(systemParts)
		if err != nil {
			return nil, err
		}
		out.System = blocks
	}
	for i, turn := range turns {
		role := normalizeRole(turn.Role)
		if role == "tool" {
			role = "user"
		}
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("%w: unsupported anthropic role %q", protocol.ErrUnsupportedRequestShape, turn.Role)
		}
		blocks, err := encodeAnthropicBlocks(turn.Parts)
		if err != nil {
			return nil, fmt.Errorf("anthropic turn %d: %w", i, err)
		}
		if len(blocks) == 0 {
			continue
		}
		out.Messages = append(out.Messages, anthropicMessageContent{Role: role, Content: blocks})
	}
	if len(conv.Tools) > 0 {
		tools := make([]map[string]any, 0, len(conv.Tools))
		for _, tool := range conv.Tools {
			if normalizeRole(tool.Type) != "" && normalizeRole(tool.Type) != "function" {
				// 跳过目标协议不支持的 builtin tool（如 web_search）
				continue
			}
			item := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				item["description"] = tool.Description
			}
			if anySchema, err := rawJSONToAny(tool.InputSchema); err == nil && anySchema != nil {
				item["input_schema"] = anySchema
			}
			tools = append(tools, item)
		}
		if len(tools) > 0 {
			out.Tools, err = marshalStableJSON(tools)
			if err != nil {
				return nil, err
			}
		}
	}
	var anthropicToolChoice map[string]any
	if !conv.ToolChoice.IsZero() {
		choice := map[string]any{}
		switch conv.ToolChoice.Mode {
		case "auto":
			choice["type"] = "auto"
		case "required":
			choice["type"] = "any"
		case "named":
			if conv.ToolChoice.toolType() != "function" {
				// 跳过指向 builtin tool 类型的 tool_choice
				choice = nil
			} else {
				choice["type"] = "tool"
				choice["name"] = conv.ToolChoice.Name
			}
		case "none":
			choice = nil
		default:
			return nil, fmt.Errorf("%w: unsupported anthropic tool choice %q", protocol.ErrUnsupportedRequestShape, conv.ToolChoice.Mode)
		}
		anthropicToolChoice = choice
	}
	if conv.ToolChoice.DisableParallel && len(conv.Tools) > 0 {
		if anthropicToolChoice == nil {
			anthropicToolChoice = map[string]any{"type": "auto"}
		}
		anthropicToolChoice["disable_parallel_tool_use"] = true
	}
	if anthropicToolChoice != nil {
		out.ToolChoice, err = marshalStableJSON(anthropicToolChoice)
		if err != nil {
			return nil, err
		}
	}
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("%w: no convertible anthropic messages", protocol.ErrUnsupportedRequestShape)
	}
	return marshalStableJSON(out)
}

func encodeAnthropicBlocks(parts []conversationPart) ([]map[string]any, error) {
	blocks := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case partKindText:
			blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
		case partKindImage:
			block, err := encodeAnthropicMediaBlock("image", part.Media)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case partKindFile:
			block, err := encodeAnthropicMediaBlock("document", part.Media)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case partKindToolCall:
			if part.ToolCall == nil {
				return nil, fmt.Errorf("%w: missing anthropic tool call content", protocol.ErrUnsupportedRequestShape)
			}
			input, err := rawJSONToAny(part.ToolCall.Arguments)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    part.ToolCall.ID,
				"name":  part.ToolCall.Name,
				"input": input,
			})
		case partKindToolResult:
			if part.ToolResult == nil {
				return nil, fmt.Errorf("%w: missing anthropic tool result content", protocol.ErrUnsupportedRequestShape)
			}
			content, err := encodeAnthropicToolResultContent(part.ToolResult.Parts)
			if err != nil {
				return nil, err
			}
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": part.ToolResult.CallID,
				"content":     content,
			}
			if part.ToolResult.IsError {
				block["is_error"] = true
			}
			blocks = append(blocks, block)
		default:
			return nil, fmt.Errorf("%w: unsupported anthropic content kind %q", protocol.ErrUnsupportedRequestShape, part.Kind)
		}
	}
	return blocks, nil
}

func encodeAnthropicToolResultContent(parts []conversationPart) (any, error) {
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
	return encodeAnthropicBlocks(parts)
}

func encodeAnthropicMediaBlock(blockType string, media *conversationMedia) (map[string]any, error) {
	if media == nil {
		return nil, fmt.Errorf("%w: missing anthropic media source", protocol.ErrUnsupportedRequestShape)
	}
	source := map[string]any{}
	switch {
	case media.Data != "":
		source["type"] = "base64"
		source["data"] = media.Data
		if media.MIMEType != "" {
			source["media_type"] = media.MIMEType
		}
	case media.URL != "":
		source["type"] = "url"
		source["url"] = media.URL
	case media.FileID != "":
		source["type"] = "file"
		source["file_id"] = media.FileID
	default:
		return nil, fmt.Errorf("%w: anthropic media requires base64, url, or file_id", protocol.ErrUnsupportedRequestShape)
	}
	block := map[string]any{"type": blockType, "source": source}
	if blockType == "document" && media.Filename != "" {
		block["title"] = media.Filename
	}
	return block, nil
}

func extractAnthropicDisableParallel(raw json.RawMessage) (bool, bool) {
	if !hasJSONValue(raw) {
		return false, false
	}
	var obj map[string]any
	if err := sonic.Unmarshal(raw, &obj); err != nil {
		return false, false
	}
	v, ok := obj["disable_parallel_tool_use"].(bool)
	if !ok {
		return false, false
	}
	return v, true
}

func extractAnthropicContentParts(content any) ([]conversationPart, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		return appendTextPart(nil, v), nil
	case []any:
		parts := make([]conversationPart, 0, len(v))
		for i, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: unsupported anthropic content block at index %d", protocol.ErrUnsupportedRequestShape, i)
			}
			part, err := decodeAnthropicContentBlock(block)
			if err != nil {
				return nil, err
			}
			if part.Kind != "" {
				parts = append(parts, part)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("%w: unsupported anthropic content type %T", protocol.ErrUnsupportedRequestShape, content)
	}
}

func decodeAnthropicContentBlock(block map[string]any) (conversationPart, error) {
	typ := normalizeRole(stringValue(block["type"]))
	switch typ {
	case "text":
		text := stringValue(block["text"])
		if text == "" {
			return conversationPart{}, nil
		}
		return conversationPart{Kind: partKindText, Text: text}, nil
	case "image":
		media, err := decodeAnthropicMedia(block)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindImage, Media: &media}, nil
	case "document":
		media, err := decodeAnthropicMedia(block)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindFile, Media: &media}, nil
	case "tool_use":
		input, err := rawJSONFromFields(block, "input")
		if err != nil {
			return conversationPart{}, err
		}
		if !hasJSONValue(input) {
			input = json.RawMessage(`{}`)
		}
		return conversationPart{Kind: partKindToolCall, ToolCall: &conversationToolCall{
			ID:        strings.TrimSpace(stringValue(block["id"])),
			Name:      strings.TrimSpace(stringValue(block["name"])),
			Arguments: input,
		}}, nil
	case "tool_result":
		parts, err := extractAnthropicToolResultParts(block["content"])
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindToolResult, ToolResult: &conversationToolResult{
			CallID:  strings.TrimSpace(stringValue(block["tool_use_id"])),
			IsError: boolValue(block["is_error"]),
			Parts:   parts,
		}}, nil
	case "thinking":
		return newReasoningPart("thinking", stringValue(block["thinking"]), stringValue(block["signature"]), ""), nil
	case "redacted_thinking":
		return newReasoningPart("redacted_thinking", "", "", stringValue(block["data"])), nil
	default:
		return conversationPart{}, fmt.Errorf("%w: unsupported anthropic content block type %q", protocol.ErrUnsupportedRequestShape, typ)
	}
}

func extractAnthropicToolResultParts(content any) ([]conversationPart, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		return appendTextPart(nil, v), nil
	case []any:
		parts := make([]conversationPart, 0, len(v))
		for i, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: unsupported anthropic tool_result block at index %d", protocol.ErrUnsupportedRequestShape, i)
			}
			part, err := decodeAnthropicContentBlock(block)
			if err != nil {
				return nil, err
			}
			if part.Kind == partKindToolCall || part.Kind == partKindToolResult || part.Kind == partKindReasoning {
				return nil, fmt.Errorf("%w: nested anthropic tool blocks are unsupported", protocol.ErrUnsupportedRequestShape)
			}
			if part.Kind != "" {
				parts = append(parts, part)
			}
		}
		return parts, nil
	default:
		jsonText, err := sonic.Marshal(v)
		if err != nil {
			return nil, err
		}
		return []conversationPart{{Kind: partKindText, Text: string(jsonText)}}, nil
	}
}

func decodeAnthropicMedia(block map[string]any) (conversationMedia, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return conversationMedia{}, fmt.Errorf("%w: anthropic media block missing source", protocol.ErrUnsupportedRequestShape)
	}
	media := conversationMedia{
		URL:      firstNonEmptyString(source, "url"),
		FileID:   firstNonEmptyString(source, "file_id"),
		MIMEType: firstNonEmptyString(source, "media_type", "mime_type"),
		Data:     firstNonEmptyString(source, "data"),
		Filename: firstNonEmptyString(block, "title", "filename"),
	}
	if media.URL == "" && media.FileID == "" && media.Data == "" {
		return conversationMedia{}, fmt.Errorf("%w: anthropic media block missing source payload", protocol.ErrUnsupportedRequestShape)
	}
	return media, nil
}
