package builtin

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

func encodeGeminiRequest(conv conversation) ([]byte, error) {
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
	payload := geminiRequestPayload{Contents: make([]geminiContent, 0, len(turns))}
	if len(systemParts) > 0 {
		payload.SystemInstruction = &geminiSystemInstruction{Parts: make([]geminiPart, 0, len(systemParts))}
		for _, part := range systemParts {
			if part.Kind == partKindReasoning {
				continue
			}
			geminiPart, err := encodeGeminiPart(part)
			if err != nil {
				return nil, err
			}
			payload.SystemInstruction.Parts = append(payload.SystemInstruction.Parts, geminiPart)
		}
	}
	for i, turn := range turns {
		role, err := encodeGeminiRole(turn.Role)
		if err != nil {
			return nil, fmt.Errorf("gemini turn %d: %w", i, err)
		}
		parts := make([]geminiPart, 0, len(turn.Parts))
		for _, part := range turn.Parts {
			if part.Kind == partKindReasoning {
				continue
			}
			geminiPart, err := encodeGeminiPart(part)
			if err != nil {
				return nil, fmt.Errorf("gemini turn %d: %w", i, err)
			}
			parts = append(parts, geminiPart)
		}
		if len(parts) == 0 {
			continue
		}
		payload.Contents = append(payload.Contents, geminiContent{Role: role, Parts: parts})
	}
	if len(conv.Tools) > 0 {
		decls := make([]geminiFunctionDeclaration, 0, len(conv.Tools))
		for _, tool := range conv.Tools {
			if normalizeRole(tool.Type) != "" && normalizeRole(tool.Type) != "function" {
				// 跳过目标协议不支持的 builtin tool（如 web_search）
				continue
			}
			decl := geminiFunctionDeclaration{Name: tool.Name, Description: tool.Description}
			if anySchema, err := rawJSONToAny(tool.InputSchema); err == nil && anySchema != nil {
				decl.Parameters = cleanGeminiSchema(anySchema)
			}
			decls = append(decls, decl)
		}
		if len(decls) > 0 {
			payload.Tools = []geminiTool{{FunctionDeclarations: decls}}
		}
	}
	if !conv.ToolChoice.IsZero() && conv.ToolChoice.toolType() == "function" {
		payload.ToolConfig, err = encodeGeminiToolConfig(conv.ToolChoice)
		if err != nil {
			return nil, err
		}
	}
	payload.GenerationConfig = buildGeminiGenerationConfig(conv)
	if len(payload.Contents) == 0 {
		return nil, fmt.Errorf("%w: no convertible gemini contents", protocol.ErrUnsupportedRequestShape)
	}
	return marshalStableJSON(payload)
}

// buildGeminiGenerationConfig 聚合采样/上限参数与思考配置，未命中任何字段时返回 nil。
func buildGeminiGenerationConfig(conv conversation) *geminiGenerationConfig {
	cfg := &geminiGenerationConfig{}
	if sp := conv.Sampling; sp != nil {
		cfg.Temperature = sp.Temperature
		cfg.TopP = sp.TopP
		cfg.TopK = sp.TopK
		if sp.MaxTokens != nil && *sp.MaxTokens > 0 {
			cfg.MaxOutputTokens = sp.MaxTokens
		}
		if len(sp.Stop) > 0 {
			cfg.StopSequences = sp.Stop
		}
		cfg.Seed = sp.Seed
	}
	cfg.ThinkingConfig = buildGeminiThinkingConfig(conv.Thinking)
	if cfg.ThinkingConfig == nil && cfg.Temperature == nil && cfg.TopP == nil && cfg.TopK == nil &&
		cfg.MaxOutputTokens == nil && len(cfg.StopSequences) == 0 && cfg.Seed == nil {
		return nil
	}
	return cfg
}

// buildGeminiThinkingConfig 把 Anthropic 顶层 thinking 映射成 Gemini thinkingConfig；
// disabled/未设置 → 显式 thinkingBudget=0 关闭，enabled+budget_tokens → 透传预算并请求返回思考摘要。
func buildGeminiThinkingConfig(thinking *anthropicThinkingConfig) *geminiThinkingConfig {
	if thinking == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "enabled":
		cfg := &geminiThinkingConfig{IncludeThoughts: true}
		if thinking.BudgetTokens > 0 {
			b := thinking.BudgetTokens
			cfg.ThinkingBudget = &b
		}
		return cfg
	case "disabled":
		zero := 0
		return &geminiThinkingConfig{ThinkingBudget: &zero}
	default:
		return nil
	}
}

func encodeGeminiRole(role string) (string, error) {
	switch normalizeRole(role) {
	case "user", "tool":
		return "user", nil
	case "assistant":
		return "model", nil
	default:
		return "", fmt.Errorf("%w: unsupported gemini role %q", protocol.ErrUnsupportedRequestShape, role)
	}
}

func encodeGeminiPart(part conversationPart) (geminiPart, error) {
	switch part.Kind {
	case partKindText:
		return geminiPart{Text: part.Text}, nil
	case partKindImage, partKindFile:
		if part.Media == nil {
			return geminiPart{}, fmt.Errorf("%w: missing media content", protocol.ErrUnsupportedRequestShape)
		}
		if part.Media.Data != "" {
			return geminiPart{InlineData: &geminiInlineData{MIMEType: part.Media.MIMEType, Data: part.Media.Data}}, nil
		}
		fileURI := part.Media.URL
		if fileURI == "" {
			fileURI = part.Media.FileID
		}
		if fileURI == "" {
			return geminiPart{}, fmt.Errorf("%w: gemini media requires file uri or inline data", protocol.ErrUnsupportedRequestShape)
		}
		return geminiPart{FileData: &geminiFileData{MIMEType: part.Media.MIMEType, FileURI: fileURI}}, nil
	case partKindToolCall:
		if part.ToolCall == nil {
			return geminiPart{}, fmt.Errorf("%w: missing tool call content", protocol.ErrUnsupportedRequestShape)
		}
		args, err := rawJSONToAny(part.ToolCall.Arguments)
		if err != nil {
			return geminiPart{}, err
		}
		return geminiPart{FunctionCall: &geminiFunctionCall{Name: part.ToolCall.Name, Args: args}}, nil
	case partKindToolResult:
		if part.ToolResult == nil {
			return geminiPart{}, fmt.Errorf("%w: missing tool result content", protocol.ErrUnsupportedRequestShape)
		}
		// Gemini functionResponse.response 期望承载工具的"返回值"本身，
		// 而非 Anthropic envelope（call_id/is_error 等字段对 Gemini 无意义）。
		// 用 {output: ...} 包一层，以便上游模型识别为函数输出。
		content, err := encodeToolResultContent(part.ToolResult.Parts)
		if err != nil {
			return geminiPart{}, err
		}
		response := map[string]any{"output": content}
		name := part.ToolResult.Name
		if name == "" {
			name = part.ToolResult.CallID
		}
		return geminiPart{FunctionResponse: &geminiFunctionResponse{Name: name, Response: response}}, nil
	default:
		return geminiPart{}, fmt.Errorf("%w: unsupported gemini content kind %q", protocol.ErrUnsupportedRequestShape, part.Kind)
	}
}

func encodeGeminiToolConfig(choice conversationToolChoice) (*geminiToolConfig, error) {
	if choice.Mode == "named" && choice.toolType() != "function" {
		return nil, fmt.Errorf("%w: gemini does not support builtin tool_choice type %q", protocol.ErrUnsupportedRequestShape, choice.toolType())
	}
	cfg := &geminiToolConfig{}
	switch choice.Mode {
	case "auto":
		cfg.FunctionCallingConfig.Mode = "AUTO"
	case "none":
		cfg.FunctionCallingConfig.Mode = "NONE"
	case "required":
		cfg.FunctionCallingConfig.Mode = "ANY"
	case "named":
		cfg.FunctionCallingConfig.Mode = "ANY"
		cfg.FunctionCallingConfig.AllowedFunctionNames = []string{choice.Name}
	default:
		return nil, fmt.Errorf("%w: unsupported gemini tool choice %q", protocol.ErrUnsupportedRequestShape, choice.Mode)
	}
	return cfg, nil
}

func parseGeminiTools(tools []geminiTool) ([]conversationTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]conversationTool, 0)
	for toolIndex, tool := range tools {
		for declIndex, decl := range tool.FunctionDeclarations {
			name := strings.TrimSpace(decl.Name)
			if name == "" {
				return nil, fmt.Errorf("%w: gemini tool declaration %d.%d missing name", protocol.ErrUnsupportedRequestShape, toolIndex, declIndex)
			}
			var schema json.RawMessage
			if decl.Parameters != nil {
				raw, err := sonic.Marshal(decl.Parameters)
				if err != nil {
					return nil, err
				}
				schema = raw
			}
			out = append(out, conversationTool{
				Type:        "function",
				Name:        name,
				Description: strings.TrimSpace(decl.Description),
				InputSchema: schema,
			})
		}
	}
	return out, nil
}

func parseGeminiToolChoice(cfg *geminiToolConfig) (conversationToolChoice, error) {
	if cfg == nil {
		return conversationToolChoice{}, nil
	}
	mode := strings.ToUpper(strings.TrimSpace(cfg.FunctionCallingConfig.Mode))
	switch mode {
	case "", "AUTO":
		return conversationToolChoice{Mode: "auto"}, nil
	case "NONE":
		return conversationToolChoice{Mode: "none"}, nil
	case "ANY":
		if len(cfg.FunctionCallingConfig.AllowedFunctionNames) == 1 {
			name := strings.TrimSpace(cfg.FunctionCallingConfig.AllowedFunctionNames[0])
			if name == "" {
				return conversationToolChoice{}, fmt.Errorf("%w: gemini named tool choice missing name", protocol.ErrUnsupportedRequestShape)
			}
			return conversationToolChoice{Mode: "named", Name: name, ToolType: "function"}, nil
		}
		return conversationToolChoice{Mode: "required"}, nil
	default:
		return conversationToolChoice{}, fmt.Errorf("%w: unsupported gemini tool choice mode %q", protocol.ErrUnsupportedRequestShape, mode)
	}
}

func normalizeGeminiRole(role string) (string, error) {
	switch normalizeRole(role) {
	case "", "user":
		return "user", nil
	case "model", "assistant":
		return "assistant", nil
	case "tool", "function":
		return "tool", nil
	default:
		return "", fmt.Errorf("%w: unsupported gemini content role %q", protocol.ErrUnsupportedRequestShape, role)
	}
}

func extractGeminiParts(parts []geminiPart, pendingToolCallIDs *[]string, nextToolCallID *int) ([]conversationPart, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]conversationPart, 0, len(parts))
	for i, part := range parts {
		decoded, err := decodeGeminiPart(part, pendingToolCallIDs, nextToolCallID)
		if err != nil {
			return nil, fmt.Errorf("gemini part %d: %w", i, err)
		}
		if decoded.Kind != "" {
			out = append(out, decoded)
		}
	}
	return out, nil
}

func decodeGeminiPart(part geminiPart, pendingToolCallIDs *[]string, nextToolCallID *int) (conversationPart, error) {
	switch {
	case strings.TrimSpace(part.Text) != "":
		return conversationPart{Kind: partKindText, Text: part.Text}, nil
	case part.InlineData != nil:
		media := conversationMedia{
			MIMEType: strings.TrimSpace(part.InlineData.MIMEType),
			Data:     strings.TrimSpace(part.InlineData.Data),
		}
		if media.Data == "" {
			return conversationPart{}, fmt.Errorf("%w: gemini inlineData missing data", protocol.ErrUnsupportedRequestShape)
		}
		return conversationPart{Kind: geminiMediaPartKind(media.MIMEType), Media: &media}, nil
	case part.FileData != nil:
		media := conversationMedia{
			MIMEType: strings.TrimSpace(part.FileData.MIMEType),
			URL:      strings.TrimSpace(part.FileData.FileURI),
		}
		if media.URL == "" {
			return conversationPart{}, fmt.Errorf("%w: gemini fileData missing fileUri", protocol.ErrUnsupportedRequestShape)
		}
		return conversationPart{Kind: geminiMediaPartKind(media.MIMEType), Media: &media}, nil
	case part.FunctionCall != nil:
		if strings.TrimSpace(part.FunctionCall.Name) == "" {
			return conversationPart{}, fmt.Errorf("%w: gemini functionCall missing name", protocol.ErrUnsupportedRequestShape)
		}
		arguments, err := sonic.Marshal(part.FunctionCall.Args)
		if err != nil {
			return conversationPart{}, err
		}
		if !hasJSONValue(arguments) {
			arguments = json.RawMessage(`{}`)
		}
		callID := nextGeminiToolCallID(pendingToolCallIDs, nextToolCallID)
		return conversationPart{Kind: partKindToolCall, ToolCall: &conversationToolCall{
			ID:        callID,
			Name:      strings.TrimSpace(part.FunctionCall.Name),
			Arguments: arguments,
		}}, nil
	case part.FunctionResponse != nil:
		result, err := decodeGeminiToolResult(part.FunctionResponse, pendingToolCallIDs, nextToolCallID)
		if err != nil {
			return conversationPart{}, err
		}
		return conversationPart{Kind: partKindToolResult, ToolResult: &result}, nil
	default:
		return conversationPart{}, nil
	}
}

func decodeGeminiToolResult(resp *geminiFunctionResponse, pendingToolCallIDs *[]string, nextToolCallID *int) (conversationToolResult, error) {
	if resp == nil {
		return conversationToolResult{}, fmt.Errorf("%w: missing gemini functionResponse", protocol.ErrUnsupportedRequestShape)
	}
	result := conversationToolResult{Name: strings.TrimSpace(resp.Name)}
	var parts []conversationPart
	switch response := resp.Response.(type) {
	case map[string]any:
		if callID := strings.TrimSpace(stringValue(response["call_id"])); callID != "" {
			result.CallID = callID
		}
		if result.Name == "" {
			result.Name = strings.TrimSpace(stringValue(response["name"]))
		}
		result.IsError = boolValue(response["is_error"])
		switch {
		case response["content"] != nil || response["call_id"] != nil || response["is_error"] != nil:
			var err error
			parts, err = decodeToolResultParts(response["content"])
			if err != nil {
				return conversationToolResult{}, err
			}
		case response["result"] != nil:
			var err error
			parts, err = decodeToolResultParts(response["result"])
			if err != nil {
				return conversationToolResult{}, err
			}
		default:
			var err error
			parts, err = decodeToolResultParts(response)
			if err != nil {
				return conversationToolResult{}, err
			}
		}
	default:
		var err error
		parts, err = decodeToolResultParts(resp.Response)
		if err != nil {
			return conversationToolResult{}, err
		}
	}
	result.Parts = parts
	if result.CallID == "" {
		result.CallID = consumeGeminiToolCallID(pendingToolCallIDs)
	}
	if result.CallID == "" {
		result.CallID = nextGeminiToolCallID(nil, nextToolCallID)
	}
	return result, nil
}

func geminiMediaPartKind(mimeType string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
		return partKindImage
	}
	return partKindFile
}

func nextGeminiToolCallID(pendingToolCallIDs *[]string, nextToolCallID *int) string {
	callID := "call_1"
	if nextToolCallID != nil {
		callID = fmt.Sprintf("call_%d", *nextToolCallID)
		*nextToolCallID++
	}
	if pendingToolCallIDs != nil {
		*pendingToolCallIDs = append(*pendingToolCallIDs, callID)
	}
	return callID
}

func consumeGeminiToolCallID(pendingToolCallIDs *[]string) string {
	if pendingToolCallIDs == nil || len(*pendingToolCallIDs) == 0 {
		return ""
	}
	callID := (*pendingToolCallIDs)[0]
	*pendingToolCallIDs = (*pendingToolCallIDs)[1:]
	return callID
}
