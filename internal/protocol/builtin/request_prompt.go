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

const (
	partKindText       = "text"
	partKindImage      = "image"
	partKindFile       = "file"
	partKindToolCall   = "tool_call"
	partKindToolResult = "tool_result"
	partKindReasoning  = "reasoning"
)

type conversation struct {
	Turns      []conversationTurn
	Tools      []conversationTool
	ToolChoice conversationToolChoice
	Thinking   *anthropicThinkingConfig
	Sampling   *samplingParams
}

// samplingParams 承载客户端指定的采样/上限参数，供各目标编码器按需透传。
// 字段为 nil 表示客户端未显式指定，目标侧走默认行为。
type samplingParams struct {
	Temperature      *float64
	TopP             *float64
	TopK             *int
	MaxTokens        *int
	Stop             []string
	ReasoningEffort  string
	Seed             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	User             string
}

type conversationTurn struct {
	Role  string
	Parts []conversationPart
}

type conversationPart struct {
	Kind       string
	Text       string
	Media      *conversationMedia
	ToolCall   *conversationToolCall
	ToolResult *conversationToolResult
	Reasoning  *conversationReasoning
}

type conversationMedia struct {
	URL      string
	FileID   string
	MIMEType string
	Data     string
	Filename string
	Detail   string
}

type conversationTool struct {
	Type        string
	Name        string
	Description string
	InputSchema json.RawMessage
	Options     map[string]any
}

type conversationToolChoice struct {
	Mode            string
	Name            string
	ToolType        string
	DisableParallel bool
}

type conversationToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type conversationToolResult struct {
	CallID  string
	Name    string
	IsError bool
	Parts   []conversationPart
}

type geminiRequestPayload struct {
	Contents          []geminiContent          `json:"contents"`
	SystemInstruction *geminiSystemInstruction `json:"systemInstruction,omitempty"`
	Tools             []geminiTool             `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig        `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenerationConfig  `json:"generationConfig,omitempty"`
}

type geminiGenerationConfig struct {
	ThinkingConfig  *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"topP,omitempty"`
	TopK            *int                  `json:"topK,omitempty"`
	MaxOutputTokens *int                  `json:"maxOutputTokens,omitempty"`
	StopSequences   []string              `json:"stopSequences,omitempty"`
	Seed            *int64                `json:"seed,omitempty"`
}

type geminiThinkingConfig struct {
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int `json:"thinkingBudget,omitempty"`
}

type geminiSystemInstruction struct {
	Parts []geminiPart `json:"parts"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type geminiFileData struct {
	MIMEType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri,omitempty"`
}

type geminiFunctionCall struct {
	Name string `json:"name"`
	Args any    `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response,omitempty"`
}

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
	return sonic.Marshal(payload)
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

// newClaudeMetadataUserID 生成 Claude CLI 兼容的 metadata.user_id 值。
func newClaudeMetadataUserID() string {
	deviceID := make([]byte, 32)
	if _, err := rand.Read(deviceID); err != nil {
		return `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"","session_id":"00000000-0000-0000-0000-000000000000"}`
	}
	sid := make([]byte, 16)
	if _, err := rand.Read(sid); err != nil {
		return fmt.Sprintf(`{"device_id":"%s","account_uuid":"","session_id":"00000000-0000-0000-0000-000000000000"}`, hex.EncodeToString(deviceID))
	}
	sid[6] = (sid[6] & 0x0f) | 0x40 // UUID v4
	sid[8] = (sid[8] & 0x3f) | 0x80
	return fmt.Sprintf(`{"device_id":"%s","account_uuid":"","session_id":"%x-%x-%x-%x-%x"}`,
		hex.EncodeToString(deviceID), sid[0:4], sid[4:6], sid[6:8], sid[8:10], sid[10:16])
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
			out.Tools, err = sonic.Marshal(tools)
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
		out.ToolChoice, err = sonic.Marshal(anthropicToolChoice)
		if err != nil {
			return nil, err
		}
	}
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("%w: no convertible anthropic messages", protocol.ErrUnsupportedRequestShape)
	}
	return sonic.Marshal(out)
}

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
			callBytes, err := sonic.Marshal(rawCalls)
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
		payload.Tools, err = sonic.Marshal(tools)
		if err != nil {
			return nil, err
		}
	}
	if !conv.ToolChoice.IsZero() {
		choice := encodeOpenAIToolChoice(conv.ToolChoice)
		if choice != nil {
			var err error
			payload.ToolChoice, err = sonic.Marshal(choice)
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
			raw, err := sonic.Marshal(sp.Stop)
			if err != nil {
				return nil, err
			}
			payload.Stop = raw
		}
	}
	return sonic.Marshal(payload)
}

func encodeCodexRequest(model string, conv conversation, stream bool) ([]byte, error) {
	systemText, turns, err := collectSystemText(conv)
	if err != nil {
		return nil, err
	}
	toolAliases := buildCodexToolAliases(collectCodexAliasNames(conv))
	out := map[string]any{
		"model": model,
		"input": make([]map[string]any, 0, len(turns)),
	}
	if stream {
		out["stream"] = true
	}
	if systemText != "" {
		out["instructions"] = systemText
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
		out["tools"] = tools
	}
	if !conv.ToolChoice.IsZero() {
		switch conv.ToolChoice.Mode {
		case "auto", "none", "required":
			out["tool_choice"] = conv.ToolChoice.Mode
		case "named":
			if conv.ToolChoice.toolType() == "function" {
				out["tool_choice"] = map[string]any{
					"type": "function",
					"name": toolAliases.shorten(conv.ToolChoice.Name),
				}
			} else {
				out["tool_choice"] = map[string]any{"type": conv.ToolChoice.toolType()}
			}
		default:
			return nil, fmt.Errorf("%w: unsupported codex tool choice %q", protocol.ErrUnsupportedRequestShape, conv.ToolChoice.Mode)
		}
	}

	input := out["input"].([]map[string]any)
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
	out["input"] = input
	if len(input) == 0 {
		if systemText == "" {
			return nil, fmt.Errorf("%w: no convertible codex input", protocol.ErrUnsupportedRequestShape)
		}
		// Responses-style Codex requests can rely on instructions alone. In that
		// case omit `input` entirely instead of rejecting the transform.
		delete(out, "input")
	}
	if conv.ToolChoice.DisableParallel && len(conv.Tools) > 0 {
		out["parallel_tool_calls"] = false
	}
	applyCodexSampling(out, conv.Sampling)
	if reasoning := buildCodexReasoningConfig(conv); reasoning != nil {
		out["reasoning"] = reasoning
		out["include"] = []string{"reasoning.encrypted_content"}
	}
	return sonic.Marshal(out)
}

// applyCodexSampling 把 Codex responses API 支持的采样参数写入 out map。
// 只透传 Codex 实际接受的字段：temperature/top_p/max_output_tokens/user；其余静默丢弃。
func applyCodexSampling(out map[string]any, sp *samplingParams) {
	if sp == nil {
		return
	}
	if sp.Temperature != nil {
		out["temperature"] = *sp.Temperature
	}
	if sp.TopP != nil {
		out["top_p"] = *sp.TopP
	}
	if sp.MaxTokens != nil && *sp.MaxTokens > 0 {
		out["max_output_tokens"] = *sp.MaxTokens
	}
	if sp.User != "" {
		out["user"] = sp.User
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

func appendTextPart(parts []conversationPart, text string) []conversationPart {
	if text == "" {
		return parts
	}
	return append(parts, conversationPart{Kind: partKindText, Text: text})
}

func dropReasoningParts(parts []conversationPart) []conversationPart {
	if len(parts) == 0 {
		return nil
	}
	filtered := parts[:0]
	for _, part := range parts {
		if part.Kind == partKindReasoning {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

func resolveToolResultNames(conv *conversation) {
	if conv == nil {
		return
	}
	callNames := make(map[string]string)
	for _, turn := range conv.Turns {
		for _, part := range turn.Parts {
			if part.Kind == partKindToolCall && part.ToolCall != nil && part.ToolCall.ID != "" && part.ToolCall.Name != "" {
				callNames[part.ToolCall.ID] = part.ToolCall.Name
			}
		}
	}
	for ti := range conv.Turns {
		for pi := range conv.Turns[ti].Parts {
			part := &conv.Turns[ti].Parts[pi]
			if part.Kind != partKindToolResult || part.ToolResult == nil || part.ToolResult.Name != "" {
				continue
			}
			if name := callNames[part.ToolResult.CallID]; name != "" {
				part.ToolResult.Name = name
			}
		}
	}
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func (c conversationToolChoice) IsZero() bool {
	return c.Mode == "" && c.Name == "" && c.ToolType == ""
}

func (t conversationTool) toolType() string {
	if typ := normalizeRole(t.Type); typ != "" {
		return typ
	}
	return "function"
}

func (c conversationToolChoice) toolType() string {
	if typ := normalizeRole(c.ToolType); typ != "" {
		return typ
	}
	return "function"
}

func normalizeConversationToolType(value string) (string, error) {
	switch typ := normalizeRole(value); typ {
	case "", "function":
		return "function", nil
	case "web_search", "web_search_preview":
		return typ, nil
	default:
		return "", fmt.Errorf("unsupported conversation tool type %q", typ)
	}
}

func isBuiltinConversationToolType(value string) bool {
	switch normalizeRole(value) {
	case "web_search", "web_search_preview":
		return true
	default:
		return false
	}
}

func cloneMapWithoutKeys(src map[string]any, keys ...string) map[string]any {
	if len(src) == 0 {
		return nil
	}
	skip := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		skip[key] = struct{}{}
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		if _, ok := skip[key]; ok {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeRole(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolValue(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func firstNonEmptyString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(m[key])); value != "" {
			return value
		}
	}
	return ""
}

func rawJSONFromFields(m map[string]any, keys ...string) (json.RawMessage, error) {
	for _, key := range keys {
		value, ok := m[key]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			trimmed := strings.TrimSpace(text)
			if trimmed == "" {
				return nil, nil
			}
			var decoded any
			if err := sonic.UnmarshalString(trimmed, &decoded); err == nil {
				return json.RawMessage(trimmed), nil
			}
		}
		raw, err := sonic.Marshal(value)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
	return nil, nil
}

func rawJSONToAny(raw json.RawMessage) (any, error) {
	if !hasJSONValue(raw) {
		return nil, nil
	}
	var decoded any
	if err := sonic.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func nestedNameField(m map[string]any, nestedKey, nameKey string) string {
	if nested, ok := m[nestedKey].(map[string]any); ok {
		if name := strings.TrimSpace(stringValue(nested[nameKey])); name != "" {
			return name
		}
	}
	return ""
}

func buildDataURL(mimeType, encoded string) string {
	if encoded == "" {
		return ""
	}
	if strings.HasPrefix(encoded, "data:") {
		return encoded
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, encoded)
}
