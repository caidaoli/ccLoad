package cursorauth

import (
	"encoding/json"
	"strings"
)

// Tool is one client-advertised function (Anthropic tools[] or OpenAI tools[]).
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is one model-requested client tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is one client-executed result for a suspended native Cursor
// custom-tool callback.
type ToolResult struct {
	CallID  string
	Output  string
	IsError bool
}

// Request is one client turn normalized for the Cursor SDK runner.
type Request struct {
	Model       string
	Prompt      string
	Tools       []Tool
	ToolChoice  string
	ToolResults []ToolResult
}

// AllowsTools reports whether this turn exposes native custom tools.
func (r Request) AllowsTools() bool {
	return r.ToolChoice != "none" && len(r.Tools) > 0
}

// ParseRequest reads an Anthropic Messages or OpenAI chat body into one native
// Cursor SDK turn. Tool definitions and results remain structured.
func ParseRequest(body []byte) Request {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Request{}
	}
	request := Request{
		Model:       strings.TrimSpace(asString(raw["model"])),
		Tools:       parseClientTools(raw),
		ToolChoice:  parseToolChoice(raw["tool_choice"], raw["function_call"]),
		ToolResults: parseToolResults(raw),
	}
	var parts []string
	if system := stringifyContent(raw["system"]); system != "" {
		parts = append(parts, system)
	}
	for _, rawMessage := range requestMessages(raw) {
		message, _ := rawMessage.(map[string]any)
		if message == nil {
			continue
		}
		if text := formatMessage(message); text != "" {
			parts = append(parts, text)
		}
	}
	request.Prompt = strings.TrimSpace(strings.Join(parts, "\n\n"))
	return request
}

// ExtractPrompt is the text the Cursor SDK Agent receives, including tool history.
func ExtractPrompt(body []byte) string {
	return ParseRequest(body).Prompt
}

func requestMessages(raw map[string]any) []any {
	if messages, _ := raw["messages"].([]any); len(messages) > 0 {
		return messages
	}
	if input, _ := raw["input"].([]any); len(input) > 0 {
		return input
	}
	return nil
}

func parseClientTools(raw map[string]any) []Tool {
	seen := make(map[string]struct{})
	var tools []Tool
	add := func(tool Tool) {
		tool.Name = strings.TrimSpace(tool.Name)
		if tool.Name == "" {
			return
		}
		if _, exists := seen[tool.Name]; exists {
			return
		}
		seen[tool.Name] = struct{}{}
		if len(bytesTrimSpace(tool.Parameters)) == 0 {
			tool.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, tool)
	}
	if entries, _ := raw["tools"].([]any); len(entries) > 0 {
		for _, entry := range entries {
			object, _ := entry.(map[string]any)
			if object == nil {
				continue
			}
			if strings.EqualFold(asString(object["type"]), "function") {
				function, _ := object["function"].(map[string]any)
				if function == nil {
					continue
				}
				add(Tool{
					Name:        asString(function["name"]),
					Description: asString(function["description"]),
					Parameters:  rawJSON(function["parameters"]),
				})
				continue
			}
			schema := rawJSON(object["input_schema"])
			if len(schema) == 0 {
				schema = rawJSON(object["parameters"])
			}
			add(Tool{
				Name:        asString(object["name"]),
				Description: asString(object["description"]),
				Parameters:  schema,
			})
		}
	}
	if entries, _ := raw["functions"].([]any); len(entries) > 0 {
		for _, entry := range entries {
			object, _ := entry.(map[string]any)
			if object == nil {
				continue
			}
			add(Tool{
				Name:        asString(object["name"]),
				Description: asString(object["description"]),
				Parameters:  rawJSON(object["parameters"]),
			})
		}
	}
	return tools
}

func parseToolChoice(choice any, legacy any) string {
	switch typed := choice.(type) {
	case string:
		value := strings.ToLower(strings.TrimSpace(typed))
		switch value {
		case "none", "auto", "required", "any":
			if value == "any" {
				return "required"
			}
			return value
		default:
			if value != "" {
				return value
			}
		}
	case map[string]any:
		if name := strings.TrimSpace(asString(typed["name"])); name != "" {
			return name
		}
		if function, _ := typed["function"].(map[string]any); function != nil {
			if name := strings.TrimSpace(asString(function["name"])); name != "" {
				return name
			}
		}
		switch strings.ToLower(asString(typed["type"])) {
		case "none":
			return "none"
		case "auto":
			return "auto"
		case "any", "required":
			return "required"
		}
	}
	if legacy != nil {
		switch typed := legacy.(type) {
		case string:
			value := strings.ToLower(strings.TrimSpace(typed))
			if value == "none" {
				return "none"
			}
		case map[string]any:
			if name := strings.TrimSpace(asString(typed["name"])); name != "" {
				return name
			}
		}
	}
	return "auto"
}

func formatMessage(message map[string]any) string {
	role := strings.ToLower(strings.TrimSpace(asString(message["role"])))
	var chunks []string
	if text := stringifyContent(message["content"]); text != "" {
		chunks = append(chunks, text)
	}
	if text := stringifyContent(message["text"]); text != "" && len(chunks) == 0 {
		chunks = append(chunks, text)
	}
	if calls, _ := message["tool_calls"].([]any); len(calls) > 0 {
		for _, call := range calls {
			object, _ := call.(map[string]any)
			if object == nil {
				continue
			}
			function, _ := object["function"].(map[string]any)
			name := asString(object["name"])
			id := asString(object["id"])
			args := rawJSON(object["arguments"])
			if function != nil {
				if name == "" {
					name = asString(function["name"])
				}
				if len(args) == 0 {
					args = rawJSON(function["arguments"])
				}
			}
			chunks = append(chunks, formatToolUseLine(id, name, args))
		}
	}
	if role == "tool" || role == "function" {
		id := firstNonEmpty(asString(message["tool_call_id"]), asString(message["id"]))
		name := asString(message["name"])
		chunks = append([]string{formatToolResultLine(id, name, stringifyContent(message["content"]))}, chunks...)
	}
	text := strings.TrimSpace(strings.Join(chunks, "\n"))
	if text == "" {
		return ""
	}
	if role == "" {
		return text
	}
	return role + ": " + text
}

func parseToolResults(raw map[string]any) []ToolResult {
	messages := requestMessages(raw)
	var trailing []ToolResult
	for index := len(messages) - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		batch := toolResultsFromMessage(message)
		if len(batch) == 0 {
			break
		}
		trailing = append(batch, trailing...)
	}

	seen := make(map[string]struct{})
	var results []ToolResult
	for _, result := range trailing {
		result.CallID = strings.TrimSpace(result.CallID)
		if result.CallID == "" {
			continue
		}
		if _, exists := seen[result.CallID]; exists {
			continue
		}
		seen[result.CallID] = struct{}{}
		results = append(results, result)
	}
	return results
}

func toolResultsFromMessage(message map[string]any) []ToolResult {
	if message == nil {
		return nil
	}
	var results []ToolResult
	role := strings.ToLower(strings.TrimSpace(asString(message["role"])))
	typeName := strings.ToLower(strings.TrimSpace(asString(message["type"])))
	if role == "tool" || role == "function" || typeName == "function_call_output" {
		results = append(results, ToolResult{
			CallID:  firstNonEmpty(asString(message["tool_call_id"]), asString(message["call_id"]), asString(message["id"])),
			Output:  stringifyToolOutput(firstNonNil(message["content"], message["output"])),
			IsError: asBool(message["is_error"]),
		})
	}
	content, _ := message["content"].([]any)
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		if block == nil || !strings.EqualFold(asString(block["type"]), "tool_result") {
			continue
		}
		results = append(results, ToolResult{
			CallID:  firstNonEmpty(asString(block["tool_use_id"]), asString(block["call_id"])),
			Output:  stringifyToolOutput(block["content"]),
			IsError: asBool(block["is_error"]),
		})
	}
	return results
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func asBool(value any) bool {
	result, _ := value.(bool)
	return result
}

func stringifyToolOutput(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return stringifyContent(value)
	}
	return string(encoded)
}

func stringifyContent(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []any:
		chunks := make([]string, 0, len(typed))
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				if text := strings.TrimSpace(block); text != "" {
					chunks = append(chunks, text)
				}
			case map[string]any:
				if line := formatContentBlock(block); line != "" {
					chunks = append(chunks, line)
				}
			}
		}
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	default:
		return strings.TrimSpace(asString(value))
	}
}

func formatContentBlock(block map[string]any) string {
	switch strings.ToLower(asString(block["type"])) {
	case "", "text":
		return strings.TrimSpace(asString(block["text"]))
	case "tool_use":
		return formatToolUseLine(asString(block["id"]), asString(block["name"]), rawJSON(block["input"]))
	case "tool_result":
		return formatToolResultLine(asString(block["tool_use_id"]), "", stringifyContent(block["content"]))
	case "function_call":
		return formatToolUseLine(asString(block["id"]), asString(block["name"]), rawJSON(block["arguments"]))
	default:
		return strings.TrimSpace(asString(block["text"]))
	}
}

func formatToolUseLine(id, name string, args json.RawMessage) string {
	line := "[tool_use"
	if id != "" {
		line += " id=" + id
	}
	if name != "" {
		line += " name=" + name
	}
	line += "]"
	if len(bytesTrimSpace(args)) > 0 {
		line += " " + string(args)
	}
	return line
}

func formatToolResultLine(id, name, content string) string {
	line := "[tool_result"
	if id != "" {
		line += " tool_use_id=" + id
	}
	if name != "" {
		line += " name=" + name
	}
	line += "]"
	if content != "" {
		line += " " + content
	}
	return line
}

func rawJSON(value any) json.RawMessage {
	switch typed := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return bytesTrimSpace(typed)
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []byte(typed)
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return nil
		}
		return raw
	}
}

func bytesTrimSpace(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
