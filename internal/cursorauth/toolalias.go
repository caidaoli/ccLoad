package cursorauth

import (
	"encoding/json"
	"strings"
)

// Client CLI tool families observed in Grok Build, OpenCode, Codex CLI,
// Claude Code, and Cursor-agent. The model often emits a sibling name
// (Shell vs bash vs run_terminal_command); we remap onto whatever the
// connected client actually advertised this turn.
var toolFamilies = [][]string{
	// shell
	{
		"bash", "Bash", "shell", "Shell",
		"run_terminal_command", "run_terminal_cmd",
		"exec_command", "shell_command", "local_shell", "command",
	},
	// read
	{"read", "Read", "read_file", "ReadFile", "readFile"},
	// write
	{"write", "Write", "write_file", "WriteFile", "writeFile"},
	// edit / strreplace
	{"edit", "Edit", "StrReplace", "str_replace", "search_replace", "searchReplace", "edit_file", "EditFile"},
	// grep
	{"grep", "Grep"},
	// glob / listdir
	{"glob", "Glob", "glob_file_search", "list_dir", "ListDir", "LS", "ls"},
	// apply_patch (OpenCode + Codex CLI)
	{"apply_patch", "ApplyPatch", "applyPatch"},
	// web fetch
	{"webfetch", "WebFetch", "web_fetch", "open_page", "WebFetchTool"},
	// web search
	{"websearch", "WebSearch", "web_search"},
	// todos
	{"todowrite", "TodoWrite", "todo_write"},
	// plan (Codex)
	{"update_plan", "UpdatePlan", "todo_write_items"},
	// skill
	{"skill", "Skill"},
	// question
	{"question", "AskUserQuestion", "ask_user_question"},
}

// Argument keys that mean the same thing across those CLIs.
var argumentAliases = [][]string{
	{"command", "cmd", "command_line", "shell_command"},
	{"path", "file_path", "filePath", "file", "target_file", "filename"},
	{"contents", "content", "text", "new_string", "newString"},
	{"old_string", "oldString", "old_str", "oldText"},
	{"new_string", "newString", "new_str", "newText"},
	{"pattern", "regex", "query", "glob_pattern", "glob"},
	{"working_directory", "workdir", "cwd", "working_dir", "target_directory"},
	{"timeout", "timeout_ms", "timeoutMs"},
	{"patch", "patchText", "patch_text", "input", "diff"},
	{"url", "href", "uri"},
	{"offset", "offset_lines", "start_line"},
	{"limit", "limit_lines", "count"},
	{"description", "desc"},
}

// ResolveClientToolCalls maps model-emitted names and argument keys onto the
// tools the client advertised (Grok Build, OpenCode, Codex CLI, Claude Code).
func ResolveClientToolCalls(calls []ToolCall, tools []Tool) []ToolCall {
	if len(calls) == 0 {
		return calls
	}
	resolved := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		if adapted, ok := resolveOneToolCall(call, tools); ok {
			resolved = append(resolved, adapted)
		}
	}
	return resolved
}

// FilterToolCalls keeps calls the client can execute, remapping sibling names.
func FilterToolCalls(calls []ToolCall, tools []Tool) []ToolCall {
	return ResolveClientToolCalls(calls, tools)
}

func resolveOneToolCall(call ToolCall, tools []Tool) (ToolCall, bool) {
	if len(tools) == 0 {
		return call, true
	}
	if tool, ok := findTool(tools, call.Name); ok {
		call.Arguments = remapArguments(call.Arguments, tool)
		return call, true
	}
	family := toolFamily(call.Name)
	if family == nil {
		return ToolCall{}, false
	}
	for _, tool := range tools {
		if sameToolFamily(tool.Name, call.Name) {
			call.Name = tool.Name
			call.Arguments = remapArguments(call.Arguments, tool)
			return call, true
		}
	}
	return ToolCall{}, false
}

func findTool(tools []Tool, name string) (Tool, bool) {
	for _, tool := range tools {
		if tool.Name == name {
			return tool, true
		}
	}
	for _, tool := range tools {
		if strings.EqualFold(tool.Name, name) {
			return tool, true
		}
	}
	return Tool{}, false
}

func toolFamily(name string) []string {
	folded := strings.ToLower(strings.TrimSpace(name))
	for _, family := range toolFamilies {
		for _, member := range family {
			if strings.ToLower(member) == folded {
				return family
			}
		}
	}
	return nil
}

func sameToolFamily(left, right string) bool {
	family := toolFamily(left)
	if family == nil {
		return strings.EqualFold(left, right)
	}
	folded := strings.ToLower(strings.TrimSpace(right))
	for _, member := range family {
		if strings.ToLower(member) == folded {
			return true
		}
	}
	return false
}

func remapArguments(raw json.RawMessage, tool Tool) json.RawMessage {
	var args map[string]any
	if json.Unmarshal(bytesTrimSpace(raw), &args) != nil || args == nil {
		return raw
	}
	wanted := schemaPropertyNames(tool.Parameters)
	if len(wanted) == 0 {
		wanted = argumentKeys(args)
	}
	for _, group := range argumentAliases {
		target := ""
		for _, key := range group {
			if _, ok := wanted[key]; ok {
				target = key
				break
			}
		}
		if target == "" {
			continue
		}
		if _, exists := args[target]; exists {
			continue
		}
		for _, key := range group {
			if key == target {
				continue
			}
			if value, ok := args[key]; ok {
				args[target] = value
				delete(args, key)
				break
			}
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return encoded
}

func schemaPropertyNames(schema json.RawMessage) map[string]struct{} {
	var payload struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(bytesTrimSpace(schema), &payload) != nil || len(payload.Properties) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(payload.Properties))
	for name := range payload.Properties {
		names[name] = struct{}{}
	}
	return names
}

func argumentKeys(args map[string]any) map[string]struct{} {
	names := make(map[string]struct{}, len(args))
	for name := range args {
		names[name] = struct{}{}
	}
	return names
}

func clientToolHint(name string) string {
	family := toolFamily(name)
	if family == nil {
		return ""
	}
	var others []string
	folded := strings.ToLower(name)
	for _, member := range family {
		if strings.ToLower(member) == folded {
			continue
		}
		others = append(others, member)
	}
	if len(others) == 0 {
		return ""
	}
	if len(others) > 6 {
		others = others[:6]
	}
	return "also called " + strings.Join(others, "/") + " — still call it " + name
}
