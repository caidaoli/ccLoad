package builtin

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

func normalizeOpenAIConversation(req openAIChatRequest) (conversation, error) {
	conv := conversation{Turns: make([]conversationTurn, 0, len(req.Messages))}
	var err error
	conv.Tools, err = parseFunctionTools(req.Tools, "openai")
	if err != nil {
		return conversation{}, err
	}
	conv.ToolChoice, err = parseToolChoice(req.ToolChoice, "openai")
	if err != nil {
		return conversation{}, err
	}
	// 顶层 parallel_tool_calls=false 等价 Anthropic tool_choice.disable_parallel_tool_use。
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		conv.ToolChoice.DisableParallel = true
	}
	conv.Sampling = buildOpenAISampling(req)
	if thinking := openAIReasoningEffortToThinking(req.ReasoningEffort); thinking != nil {
		conv.Thinking = thinking
	}

	for i, msg := range req.Messages {
		role := normalizeRole(msg.Role)
		parts, err := extractOpenAIContentParts(msg.Content)
		if err != nil {
			return conversation{}, fmt.Errorf("openai message %d: %w", i, err)
		}
		toolParts, err := extractOpenAIToolCallParts(msg.ToolCalls)
		if err != nil {
			return conversation{}, fmt.Errorf("openai message %d: %w", i, err)
		}
		parts = append(parts, toolParts...)

		switch role {
		case "system", "developer", "user", "assistant":
			if len(parts) == 0 {
				continue
			}
			conv.Turns = append(conv.Turns, conversationTurn{Role: role, Parts: parts})
		case "tool":
			if strings.TrimSpace(msg.ToolCallID) == "" {
				return conversation{}, fmt.Errorf("%w: openai tool message missing tool_call_id", protocol.ErrUnsupportedRequestShape)
			}
			conv.Turns = append(conv.Turns, conversationTurn{Role: role, Parts: []conversationPart{{
				Kind: partKindToolResult,
				ToolResult: &conversationToolResult{
					CallID: strings.TrimSpace(msg.ToolCallID),
					Name:   strings.TrimSpace(msg.Name),
					Parts:  parts,
				},
			}}})
		default:
			return conversation{}, fmt.Errorf("%w: unsupported openai message role %q", protocol.ErrUnsupportedRequestShape, msg.Role)
		}
	}

	resolveToolResultNames(&conv)
	if len(conv.Turns) == 0 && len(conv.Tools) == 0 {
		return conversation{}, fmt.Errorf("%w: no convertible openai messages", protocol.ErrUnsupportedRequestShape)
	}
	return conv, nil
}

func normalizeAnthropicConversation(req anthropicMessagesRequest) (conversation, error) {
	conv := conversation{Turns: make([]conversationTurn, 0, len(req.Messages)+1)}
	var err error
	conv.Tools, err = parseFunctionTools(req.Tools, "anthropic")
	if err != nil {
		return conversation{}, err
	}
	conv.ToolChoice, err = parseToolChoice(req.ToolChoice, "anthropic")
	if err != nil {
		return conversation{}, err
	}
	if disable, ok := extractAnthropicDisableParallel(req.ToolChoice); ok {
		conv.ToolChoice.DisableParallel = disable
	}
	if req.Thinking != nil && strings.TrimSpace(req.Thinking.Type) != "" {
		conv.Thinking = req.Thinking
	}

	if req.System != nil {
		systemParts, err := extractAnthropicContentParts(req.System)
		if err != nil {
			return conversation{}, fmt.Errorf("anthropic system: %w", err)
		}
		systemParts = dropReasoningParts(systemParts)
		if len(systemParts) > 0 {
			conv.Turns = append(conv.Turns, conversationTurn{Role: "system", Parts: systemParts})
		}
	}
	for i, msg := range req.Messages {
		role := normalizeRole(msg.Role)
		parts, err := extractAnthropicContentParts(msg.Content)
		if err != nil {
			return conversation{}, fmt.Errorf("anthropic message %d: %w", i, err)
		}
		if role != "assistant" {
			parts = dropReasoningParts(parts)
		}
		switch role {
		case "user", "assistant":
			if len(parts) == 0 {
				continue
			}
			conv.Turns = append(conv.Turns, conversationTurn{Role: role, Parts: parts})
		default:
			return conversation{}, fmt.Errorf("%w: unsupported anthropic message role %q", protocol.ErrUnsupportedRequestShape, msg.Role)
		}
	}

	resolveToolResultNames(&conv)
	if len(conv.Turns) == 0 && len(conv.Tools) == 0 {
		return conversation{}, fmt.Errorf("%w: no convertible anthropic messages", protocol.ErrUnsupportedRequestShape)
	}
	return conv, nil
}

func normalizeCodexConversation(req codexRequest) (conversation, error) {
	conv := conversation{Turns: make([]conversationTurn, 0, len(req.Input)+1)}
	var err error
	conv.Tools, err = parseFunctionTools(req.Tools, "codex")
	if err != nil {
		return conversation{}, err
	}
	conv.ToolChoice, err = parseToolChoice(req.ToolChoice, "codex")
	if err != nil {
		return conversation{}, err
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		conv.ToolChoice.DisableParallel = true
	}
	conv.Sampling = buildCodexSampling(req)
	if conv.Sampling != nil {
		if thinking := openAIReasoningEffortToThinking(conv.Sampling.ReasoningEffort); thinking != nil {
			conv.Thinking = thinking
		}
	}
	if strings.TrimSpace(req.Instructions) != "" {
		conv.Turns = append(conv.Turns, conversationTurn{Role: "system", Parts: []conversationPart{{Kind: partKindText, Text: req.Instructions}}})
	}

	for i, rawItem := range req.Input {
		var item map[string]any
		if err := sonic.Unmarshal(rawItem, &item); err != nil {
			return conversation{}, fmt.Errorf("codex input %d: %w", i, err)
		}
		typ := normalizeRole(stringValue(item["type"]))
		if typ == "" {
			role := normalizeRole(stringValue(item["role"]))
			if role != "" {
				if _, hasContent := item["content"]; hasContent {
					typ = "message"
				}
			}
		}
		switch typ {
		case "message":
			role := normalizeRole(stringValue(item["role"]))
			parts, err := extractCodexContentParts(item["content"])
			if err != nil {
				return conversation{}, fmt.Errorf("codex input %d: %w", i, err)
			}
			switch role {
			case "system", "developer", "user", "assistant":
				if len(parts) == 0 {
					continue
				}
				conv.Turns = append(conv.Turns, conversationTurn{Role: role, Parts: parts})
			default:
				return conversation{}, fmt.Errorf("%w: unsupported codex message role %q", protocol.ErrUnsupportedRequestShape, role)
			}
		case "function_call":
			call, err := decodeCodexToolCall(item)
			if err != nil {
				return conversation{}, fmt.Errorf("codex input %d: %w", i, err)
			}
			conv.Turns = append(conv.Turns, conversationTurn{Role: "assistant", Parts: []conversationPart{{Kind: partKindToolCall, ToolCall: &call}}})
		case "function_call_output":
			result, err := decodeCodexToolResult(item)
			if err != nil {
				return conversation{}, fmt.Errorf("codex input %d: %w", i, err)
			}
			conv.Turns = append(conv.Turns, conversationTurn{Role: "tool", Parts: []conversationPart{{Kind: partKindToolResult, ToolResult: &result}}})
		case "input_text", "output_text", "text", "input_image", "image", "input_file", "file":
			part, err := decodeCodexContentPart(item)
			if err != nil {
				return conversation{}, fmt.Errorf("codex input %d: %w", i, err)
			}
			conv.Turns = append(conv.Turns, conversationTurn{Role: "user", Parts: []conversationPart{part}})
		default:
			return conversation{}, fmt.Errorf("%w: unsupported codex input item type %q", protocol.ErrUnsupportedRequestShape, typ)
		}
	}

	resolveToolResultNames(&conv)
	if len(conv.Turns) == 0 && len(conv.Tools) == 0 {
		return conversation{}, fmt.Errorf("%w: no convertible codex input messages", protocol.ErrUnsupportedRequestShape)
	}
	return conv, nil
}

func normalizeGeminiConversation(req geminiRequestPayload) (conversation, error) {
	conv := conversation{Turns: make([]conversationTurn, 0, len(req.Contents)+1)}
	var err error
	conv.Tools, err = parseGeminiTools(req.Tools)
	if err != nil {
		return conversation{}, err
	}
	conv.ToolChoice, err = parseGeminiToolChoice(req.ToolConfig)
	if err != nil {
		return conversation{}, err
	}

	if req.SystemInstruction != nil {
		systemParts, err := extractGeminiParts(req.SystemInstruction.Parts, nil, nil)
		if err != nil {
			return conversation{}, fmt.Errorf("gemini system_instruction: %w", err)
		}
		if len(systemParts) > 0 {
			conv.Turns = append(conv.Turns, conversationTurn{Role: "system", Parts: systemParts})
		}
	}

	pendingToolCallIDs := make([]string, 0)
	nextToolCallID := 1
	for i, content := range req.Contents {
		role, err := normalizeGeminiRole(content.Role)
		if err != nil {
			return conversation{}, fmt.Errorf("gemini content %d: %w", i, err)
		}
		parts, err := extractGeminiParts(content.Parts, &pendingToolCallIDs, &nextToolCallID)
		if err != nil {
			return conversation{}, fmt.Errorf("gemini content %d: %w", i, err)
		}
		if len(parts) == 0 {
			continue
		}
		conv.Turns = append(conv.Turns, conversationTurn{Role: role, Parts: parts})
	}

	resolveToolResultNames(&conv)
	if len(conv.Turns) == 0 && len(conv.Tools) == 0 {
		return conversation{}, fmt.Errorf("%w: no convertible gemini contents", protocol.ErrUnsupportedRequestShape)
	}
	return conv, nil
}

func splitConversationForSystem(conv conversation) ([]conversationPart, []conversationTurn, error) {
	systemParts := make([]conversationPart, 0)
	turns := make([]conversationTurn, 0, len(conv.Turns))
	for _, turn := range conv.Turns {
		role := normalizeRole(turn.Role)
		if role == "system" || role == "developer" {
			systemParts = append(systemParts, turn.Parts...)
			continue
		}
		turns = append(turns, turn)
	}
	return systemParts, turns, nil
}

func collectSystemText(conv conversation) (string, []conversationTurn, error) {
	systemParts, turns, err := splitConversationForSystem(conv)
	if err != nil {
		return "", nil, err
	}
	var builder strings.Builder
	for _, part := range systemParts {
		if part.Kind != partKindText {
			return "", nil, fmt.Errorf("%w: codex instructions only support text system content", protocol.ErrUnsupportedRequestShape)
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(part.Text)
	}
	return builder.String(), turns, nil
}

func parseFunctionTools(raw json.RawMessage, source string) ([]conversationTool, error) {
	if !hasJSONValue(raw) {
		return nil, nil
	}
	var items []map[string]any
	if err := sonic.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s tools: %w", source, err)
	}
	tools := make([]conversationTool, 0, len(items))
	for i, item := range items {
		typ, err := normalizeConversationToolType(stringValue(item["type"]))
		if err != nil {
			return nil, fmt.Errorf("%w: unsupported %s tool type %q at index %d", protocol.ErrUnsupportedRequestShape, source, normalizeRole(stringValue(item["type"])), i)
		}
		if typ != "function" {
			tool := conversationTool{
				Type:    typ,
				Options: cloneMapWithoutKeys(item, "type"),
			}
			tools = append(tools, tool)
			continue
		}
		fn := item
		if normalizeRole(stringValue(item["type"])) == "function" {
			nested, ok := item["function"].(map[string]any)
			if ok && len(nested) > 0 {
				fn = nested
			}
		}
		name := strings.TrimSpace(stringValue(fn["name"]))
		if name == "" {
			return nil, fmt.Errorf("%w: %s tool %d missing name", protocol.ErrUnsupportedRequestShape, source, i)
		}
		schema, err := rawJSONFromFields(fn, "parameters", "input_schema")
		if err != nil {
			return nil, err
		}
		tools = append(tools, conversationTool{
			Type:        "function",
			Name:        name,
			Description: stringValue(fn["description"]),
			InputSchema: schema,
		})
	}
	return tools, nil
}

func parseToolChoice(raw json.RawMessage, source string) (conversationToolChoice, error) {
	if !hasJSONValue(raw) {
		return conversationToolChoice{}, nil
	}
	var text string
	if err := sonic.Unmarshal(raw, &text); err == nil {
		switch normalizeRole(text) {
		case "", "auto":
			return conversationToolChoice{Mode: "auto"}, nil
		case "none":
			return conversationToolChoice{Mode: "none"}, nil
		case "required", "any":
			return conversationToolChoice{Mode: "required"}, nil
		default:
			return conversationToolChoice{}, fmt.Errorf("%w: unsupported %s tool_choice %q", protocol.ErrUnsupportedRequestShape, source, text)
		}
	}
	var obj map[string]any
	if err := sonic.Unmarshal(raw, &obj); err != nil {
		return conversationToolChoice{}, fmt.Errorf("%s tool_choice: %w", source, err)
	}
	typ := normalizeRole(stringValue(obj["type"]))
	switch typ {
	case "auto", "":
		if name := nestedNameField(obj, "function", "name"); name != "" {
			return conversationToolChoice{Mode: "named", Name: name, ToolType: "function"}, nil
		}
		return conversationToolChoice{Mode: "auto"}, nil
	case "none":
		return conversationToolChoice{Mode: "none"}, nil
	case "required", "any":
		return conversationToolChoice{Mode: "required"}, nil
	case "function":
		name := nestedNameField(obj, "function", "name")
		if name == "" {
			name = strings.TrimSpace(stringValue(obj["name"]))
		}
		if name == "" {
			return conversationToolChoice{}, fmt.Errorf("%w: named %s tool_choice missing name", protocol.ErrUnsupportedRequestShape, source)
		}
		return conversationToolChoice{Mode: "named", Name: name, ToolType: "function"}, nil
	case "tool":
		name := strings.TrimSpace(stringValue(obj["name"]))
		if name == "" {
			return conversationToolChoice{}, fmt.Errorf("%w: named %s tool_choice missing name", protocol.ErrUnsupportedRequestShape, source)
		}
		return conversationToolChoice{Mode: "named", Name: name, ToolType: "function"}, nil
	default:
		if isBuiltinConversationToolType(typ) {
			return conversationToolChoice{Mode: "named", ToolType: typ}, nil
		}
		return conversationToolChoice{}, fmt.Errorf("%w: unsupported %s tool_choice type %q", protocol.ErrUnsupportedRequestShape, source, typ)
	}
}
