package cursorauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	toolCallOpen  = "<cc_tool_call>"
	toolCallClose = "</cc_tool_call>"
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

// Request is the Cursor SDK prompt plus any client tools to map.
type Request struct {
	Model      string
	Prompt     string
	Tools      []Tool
	ToolChoice string
}

// AllowsTools reports whether this turn should parse and emit client tool calls.
func (r Request) AllowsTools() bool {
	return r.ToolChoice != "none" && len(r.Tools) > 0
}

// ParseRequest reads an Anthropic Messages or OpenAI chat body into a
// Cursor SDK prompt. Client tools stay on the client: they are described in
// the prompt and round-tripped as <cc_tool_call> blocks, never executed on the
// ccLoad host.
func ParseRequest(body []byte) Request {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Request{}
	}
	request := Request{
		Model:      strings.TrimSpace(asString(raw["model"])),
		Tools:      parseClientTools(raw),
		ToolChoice: parseToolChoice(raw["tool_choice"], raw["function_call"]),
	}
	var parts []string
	if system := stringifyContent(raw["system"]); system != "" {
		parts = append(parts, system)
	}
	if request.AllowsTools() {
		parts = append(parts, formatToolCatalog(request.Tools, request.ToolChoice))
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

func formatToolCatalog(tools []Tool, choice string) string {
	var b strings.Builder
	b.WriteString("You can call client tools. They run on the user's machine, not on this host. ")
	b.WriteString("The client is a coding CLI (Grok Build, OpenCode, Codex CLI, or Claude Code). ")
	b.WriteString("Use the EXACT tool names listed below. Do not invent Cursor-native names such as Shell, ReadFile, or edit_file.\n")
	switch {
	case choice == "none":
		b.WriteString("Do not call tools on this turn.\n")
	case choice == "required":
		b.WriteString("You MUST call at least one tool before answering.\n")
	case choice != "" && choice != "auto":
		b.WriteString("You MUST call the tool named " + choice + ".\n")
	default:
		b.WriteString("Call a tool only when it is needed; otherwise answer in plain text.\n")
	}
	b.WriteString("To call a tool, output one or more blocks in this exact shape and nothing after the last block:\n")
	b.WriteString(toolCallOpen + "\n")
	b.WriteString(`{"name":"TOOL_NAME","arguments":{}}` + "\n")
	b.WriteString(toolCallClose + "\n")
	b.WriteString("Available tools:\n")
	for _, tool := range tools {
		b.WriteString("- ")
		b.WriteString(tool.Name)
		if hint := clientToolHint(tool.Name); hint != "" {
			b.WriteString(" (")
			b.WriteString(hint)
			b.WriteString(")")
		}
		if tool.Description != "" {
			b.WriteString(": ")
			b.WriteString(tool.Description)
		}
		if len(bytesTrimSpace(tool.Parameters)) > 0 {
			b.WriteString("\n  parameters: ")
			b.Write(tool.Parameters)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
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

// SplitToolOutput pulls <cc_tool_call> blocks out of model text. plain is the
// remaining assistant text. incomplete is true when a start tag has no close.
func SplitToolOutput(text string) (plain string, calls []ToolCall, incomplete bool) {
	remaining := text
	var builder strings.Builder
	for {
		start := strings.Index(remaining, toolCallOpen)
		if start < 0 {
			builder.WriteString(remaining)
			break
		}
		builder.WriteString(remaining[:start])
		rest := remaining[start+len(toolCallOpen):]
		end := strings.Index(rest, toolCallClose)
		if end < 0 {
			incomplete = true
			break
		}
		payload := strings.TrimSpace(stripFence(rest[:end]))
		remaining = rest[end+len(toolCallClose):]
		if call, ok := parseToolCallPayload(payload); ok {
			calls = append(calls, call)
		} else {
			builder.WriteString(strings.TrimSpace(payload))
		}
	}
	return strings.TrimSpace(builder.String()), calls, incomplete
}

func parseToolCallPayload(payload string) (ToolCall, bool) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return ToolCall{}, false
	}
	var object map[string]any
	if json.Unmarshal([]byte(payload), &object) != nil {
		return ToolCall{}, false
	}
	name := strings.TrimSpace(asString(object["name"]))
	if name == "" {
		if function, _ := object["function"].(map[string]any); function != nil {
			name = strings.TrimSpace(asString(function["name"]))
			if args := toolArguments(function["arguments"], function["parameters"], function["input"]); len(args) > 0 {
				object["arguments"] = json.RawMessage(args)
			}
		}
	}
	if name == "" {
		return ToolCall{}, false
	}
	args := toolArguments(object["arguments"], object["parameters"], object["input"])
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return ToolCall{ID: newToolCallID(), Name: name, Arguments: args}, true
}

func toolArguments(values ...any) json.RawMessage {
	for _, value := range values {
		switch typed := value.(type) {
		case json.RawMessage:
			if len(bytesTrimSpace(typed)) > 0 {
				return normalizeArguments(typed)
			}
		case string:
			if strings.TrimSpace(typed) != "" {
				return normalizeArguments([]byte(typed))
			}
		case map[string]any:
			raw, err := json.Marshal(typed)
			if err == nil {
				return raw
			}
		}
	}
	return nil
}

func normalizeArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`)
	}
	if trimmed[0] == '"' {
		var inner string
		if json.Unmarshal(trimmed, &inner) == nil {
			inner = strings.TrimSpace(inner)
			if inner == "" {
				return json.RawMessage(`{}`)
			}
			trimmed = []byte(inner)
		}
	}
	if json.Valid(trimmed) && trimmed[0] == '{' {
		return json.RawMessage(trimmed)
	}
	encoded, err := json.Marshal(map[string]any{"value": string(trimmed)})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func newToolCallID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("toolu_%d", len(raw))
	}
	return "toolu_" + hex.EncodeToString(raw)
}

func stripFence(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "```") {
		return value
	}
	value = strings.TrimPrefix(value, "```")
	if newline := strings.IndexByte(value, '\n'); newline >= 0 {
		lang := strings.TrimSpace(value[:newline])
		if lang == "json" || lang == "jsonc" || lang == "" {
			value = value[newline+1:]
		}
	}
	value = strings.TrimSuffix(value, "```")
	return strings.TrimSpace(value)
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
