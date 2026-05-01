package builtin

import (
	"encoding/json"
	"fmt"
	"strings"

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

type codexRequestPayload struct {
	Model             string            `json:"model"`
	Instructions      string            `json:"instructions,omitempty"`
	Input             []map[string]any  `json:"input,omitempty"`
	Tools             []map[string]any  `json:"tools,omitempty"`
	ToolChoice        any               `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Stream            util.FlexibleBool `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	User              string            `json:"user,omitempty"`
	Reasoning         map[string]any    `json:"reasoning,omitempty"`
	Include           []string          `json:"include,omitempty"`
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

// stableSonicCfg 配置 sonic 与 encoding/json 行为一致的 JSON 序列化器：
// - SortMapKeys=true：map 按 key 字母序输出，保证 byte-level 稳定（prompt cache prefix 命中要求）
// - EscapeHTML=false：保留 <、>、& 等字符原样，避免污染 prompt
// 性能比 encoding/json 提升约 2-3x，且无需 bytes.Buffer + TrimSuffix 的额外分配。
var stableSonicCfg = sonic.Config{
	SortMapKeys: true,
	EscapeHTML:  false,
}.Froze()

// marshalStableJSON 使用 sonic 序列化任意值为字段顺序稳定的 JSON。
// "stable" 含义：相同输入两次序列化字节完全一致，是 prompt cache 命中前提。
func marshalStableJSON(v any) ([]byte, error) {
	return stableSonicCfg.Marshal(v)
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
