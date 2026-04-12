package builtin

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"
)

type textPrompt struct {
	System   string
	Messages []textPromptMessage
}

type textPromptMessage struct {
	Role string
	Text string
}

func normalizeOpenAITextPrompt(req openAIChatRequest) (textPrompt, error) {
	if hasJSONValue(req.Tools) {
		return textPrompt{}, fmt.Errorf("%w: unsupported openai tools", protocol.ErrUnsupportedRequestShape)
	}
	if hasJSONValue(req.ToolChoice) {
		return textPrompt{}, fmt.Errorf("%w: unsupported openai tool_choice", protocol.ErrUnsupportedRequestShape)
	}

	prompt := textPrompt{Messages: make([]textPromptMessage, 0, len(req.Messages))}
	for i, msg := range req.Messages {
		if hasJSONValue(msg.ToolCalls) {
			return textPrompt{}, fmt.Errorf("%w: unsupported openai tool_calls in message %d", protocol.ErrUnsupportedRequestShape, i)
		}
		text, err := extractOpenAITextContent(msg.Content)
		if err != nil {
			return textPrompt{}, fmt.Errorf("openai message %d: %w", i, err)
		}
		switch msg.Role {
		case "system", "developer":
			prompt.System = appendPromptText(prompt.System, text)
		case "user", "assistant":
			if text == "" {
				continue
			}
			prompt.Messages = append(prompt.Messages, textPromptMessage{Role: msg.Role, Text: text})
		default:
			return textPrompt{}, fmt.Errorf("%w: unsupported openai message role %q", protocol.ErrUnsupportedRequestShape, msg.Role)
		}
	}
	if prompt.System == "" && len(prompt.Messages) == 0 {
		return textPrompt{}, fmt.Errorf("%w: no convertible openai messages", protocol.ErrUnsupportedRequestShape)
	}
	return prompt, nil
}

func normalizeAnthropicTextPrompt(req anthropicMessagesRequest) (textPrompt, error) {
	if hasJSONValue(req.Tools) {
		return textPrompt{}, fmt.Errorf("%w: unsupported anthropic tools", protocol.ErrUnsupportedRequestShape)
	}
	if hasJSONValue(req.ToolChoice) {
		return textPrompt{}, fmt.Errorf("%w: unsupported anthropic tool_choice", protocol.ErrUnsupportedRequestShape)
	}

	prompt := textPrompt{Messages: make([]textPromptMessage, 0, len(req.Messages))}
	if req.System != nil {
		text, err := extractAnthropicTextContent(req.System)
		if err != nil {
			return textPrompt{}, fmt.Errorf("anthropic system: %w", err)
		}
		prompt.System = appendPromptText(prompt.System, text)
	}
	for i, msg := range req.Messages {
		text, err := extractAnthropicTextContent(msg.Content)
		if err != nil {
			return textPrompt{}, fmt.Errorf("anthropic message %d: %w", i, err)
		}
		switch msg.Role {
		case "user", "assistant":
			if text == "" {
				continue
			}
			prompt.Messages = append(prompt.Messages, textPromptMessage{Role: msg.Role, Text: text})
		default:
			return textPrompt{}, fmt.Errorf("%w: unsupported anthropic message role %q", protocol.ErrUnsupportedRequestShape, msg.Role)
		}
	}
	if prompt.System == "" && len(prompt.Messages) == 0 {
		return textPrompt{}, fmt.Errorf("%w: no convertible anthropic messages", protocol.ErrUnsupportedRequestShape)
	}
	return prompt, nil
}

func normalizeCodexTextPrompt(req codexRequest) (textPrompt, error) {
	if hasJSONValue(req.Tools) {
		return textPrompt{}, fmt.Errorf("%w: unsupported codex tools", protocol.ErrUnsupportedRequestShape)
	}
	if hasJSONValue(req.ToolChoice) {
		return textPrompt{}, fmt.Errorf("%w: unsupported codex tool_choice", protocol.ErrUnsupportedRequestShape)
	}

	prompt := textPrompt{Messages: make([]textPromptMessage, 0, len(req.Input))}
	prompt.System = appendPromptText(prompt.System, req.Instructions)
	for i, item := range req.Input {
		if item.Type != "message" {
			return textPrompt{}, fmt.Errorf("%w: unsupported codex input item type %q", protocol.ErrUnsupportedRequestShape, item.Type)
		}
		text, err := extractCodexTextContent(item.Content)
		if err != nil {
			return textPrompt{}, fmt.Errorf("codex input %d: %w", i, err)
		}
		switch item.Role {
		case "system", "developer":
			prompt.System = appendPromptText(prompt.System, text)
		case "user", "assistant":
			if text == "" {
				continue
			}
			prompt.Messages = append(prompt.Messages, textPromptMessage{Role: item.Role, Text: text})
		default:
			return textPrompt{}, fmt.Errorf("%w: unsupported codex message role %q", protocol.ErrUnsupportedRequestShape, item.Role)
		}
	}
	if prompt.System == "" && len(prompt.Messages) == 0 {
		return textPrompt{}, fmt.Errorf("%w: no convertible codex input messages", protocol.ErrUnsupportedRequestShape)
	}
	return prompt, nil
}

func appendPromptText(existing, next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	return existing + "\n\n" + next
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func extractOpenAITextContent(content any) (string, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		var out strings.Builder
		for i, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("%w: unsupported openai content part at index %d", protocol.ErrUnsupportedRequestShape, i)
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text":
				text, ok := part["text"].(string)
				if !ok {
					return "", fmt.Errorf("invalid openai text content at index %d", i)
				}
				out.WriteString(text)
			case "image_url", "input_image", "image":
				return "", fmt.Errorf("%w: unsupported openai content part type %q", protocol.ErrUnsupportedRequestShape, typ)
			default:
				return "", fmt.Errorf("%w: unsupported openai content part type %q", protocol.ErrUnsupportedRequestShape, typ)
			}
		}
		return out.String(), nil
	default:
		return "", fmt.Errorf("%w: unsupported openai content type %T", protocol.ErrUnsupportedRequestShape, content)
	}
}

func extractAnthropicTextContent(content any) (string, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		var out strings.Builder
		for i, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("%w: unsupported anthropic content block at index %d", protocol.ErrUnsupportedRequestShape, i)
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text":
				text, ok := part["text"].(string)
				if !ok {
					return "", fmt.Errorf("invalid anthropic text content at index %d", i)
				}
				out.WriteString(text)
			case "image", "tool_use", "tool_result", "document":
				return "", fmt.Errorf("%w: unsupported anthropic content block type %q", protocol.ErrUnsupportedRequestShape, typ)
			default:
				return "", fmt.Errorf("%w: unsupported anthropic content block type %q", protocol.ErrUnsupportedRequestShape, typ)
			}
		}
		return out.String(), nil
	default:
		return "", fmt.Errorf("%w: unsupported anthropic content type %T", protocol.ErrUnsupportedRequestShape, content)
	}
}

func extractCodexTextContent(content any) (string, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case []map[string]any:
		var out strings.Builder
		for i, part := range v {
			typ, _ := part["type"].(string)
			switch typ {
			case "input_text", "output_text":
				text, ok := part["text"].(string)
				if !ok {
					return "", fmt.Errorf("invalid codex text content at index %d", i)
				}
				out.WriteString(text)
			case "input_image", "input_file", "image", "file":
				return "", fmt.Errorf("%w: unsupported codex content part type %q", protocol.ErrUnsupportedRequestShape, typ)
			default:
				return "", fmt.Errorf("%w: unsupported codex content part type %q", protocol.ErrUnsupportedRequestShape, typ)
			}
		}
		return out.String(), nil
	case []any:
		var out strings.Builder
		for i, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("%w: unsupported codex content part at index %d", protocol.ErrUnsupportedRequestShape, i)
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "input_text", "output_text":
				text, ok := part["text"].(string)
				if !ok {
					return "", fmt.Errorf("invalid codex text content at index %d", i)
				}
				out.WriteString(text)
			case "input_image", "input_file", "image", "file":
				return "", fmt.Errorf("%w: unsupported codex content part type %q", protocol.ErrUnsupportedRequestShape, typ)
			default:
				return "", fmt.Errorf("%w: unsupported codex content part type %q", protocol.ErrUnsupportedRequestShape, typ)
			}
		}
		return out.String(), nil
	default:
		return "", fmt.Errorf("%w: unsupported codex content type %T", protocol.ErrUnsupportedRequestShape, content)
	}
}
