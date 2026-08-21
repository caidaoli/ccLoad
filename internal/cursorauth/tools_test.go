package cursorauth

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRequestMapsAnthropicToolsAndHistory(t *testing.T) {
	t.Parallel()
	request := ParseRequest([]byte(`{
		"model":"claude-sonnet-5",
		"system":"sys",
		"tools":[{"name":"bash","description":"run a command","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"list files"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"cmd":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt"}]}
		]
	}`))
	if request.Model != "claude-sonnet-5" || len(request.Tools) != 1 || request.Tools[0].Name != "bash" {
		t.Fatalf("request = %+v", request)
	}
	if !request.AllowsTools() {
		t.Fatal("tools should be enabled")
	}
	if !strings.Contains(request.Prompt, "sys") ||
		!strings.Contains(request.Prompt, "<cc_tool_call>") ||
		!strings.Contains(request.Prompt, "[tool_use id=toolu_1 name=bash]") ||
		!strings.Contains(request.Prompt, "[tool_result tool_use_id=toolu_1] a.txt") {
		t.Fatalf("prompt = %q", request.Prompt)
	}
}

func TestParseRequestMapsOpenAIToolCalls(t *testing.T) {
	t.Parallel()
	request := ParseRequest([]byte(`{
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"go\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		]
	}`))
	if len(request.Tools) != 1 || request.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v", request.Tools)
	}
	if !strings.Contains(request.Prompt, "[tool_use id=call_1 name=lookup]") ||
		!strings.Contains(request.Prompt, "[tool_result tool_use_id=call_1] ok") {
		t.Fatalf("prompt = %q", request.Prompt)
	}
}

func TestParseRequestHonorsToolChoiceNone(t *testing.T) {
	t.Parallel()
	request := ParseRequest([]byte(`{
		"tools":[{"name":"bash","input_schema":{"type":"object"}}],
		"tool_choice":"none",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	if request.AllowsTools() {
		t.Fatal("tool_choice=none must disable mapped calls")
	}
	if strings.Contains(request.Prompt, "<cc_tool_call>") {
		t.Fatalf("prompt = %q", request.Prompt)
	}
}

func TestSplitToolOutputParsesBlocksAndLeavesText(t *testing.T) {
	t.Parallel()
	plain, calls, incomplete := SplitToolOutput("hello\n<cc_tool_call>\n{\"name\":\"bash\",\"arguments\":{\"cmd\":\"ls\"}}\n</cc_tool_call>\n")
	if incomplete || plain != "hello" || len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("plain=%q calls=%+v incomplete=%v", plain, calls, incomplete)
	}
	if string(calls[0].Arguments) != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %s", calls[0].Arguments)
	}
	_, _, incomplete = SplitToolOutput("hello <cc_tool_call>{\"name\":\"bash\"")
	if !incomplete {
		t.Fatal("unclosed block must be incomplete")
	}
}

func TestSplitToolOutputAcceptsStringArguments(t *testing.T) {
	t.Parallel()
	_, calls, _ := SplitToolOutput(`<cc_tool_call>{"name":"lookup","arguments":"{\"q\":\"go\"}"}</cc_tool_call>`)
	if len(calls) != 1 || string(calls[0].Arguments) != `{"q":"go"}` {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestFilterToolCallsDropsUnknownNames(t *testing.T) {
	t.Parallel()
	calls := FilterToolCalls([]ToolCall{{Name: "bash"}, {Name: "rm"}}, []Tool{{Name: "bash"}})
	if len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestResolveClientToolCallsMapsGrokOpenCodeCodexAliases(t *testing.T) {
	t.Parallel()
	grok := []Tool{{
		Name:       "run_terminal_command",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"working_directory":{"type":"string"}}}`),
	}}
	calls := ResolveClientToolCalls([]ToolCall{{
		Name: "Shell", Arguments: json.RawMessage(`{"cmd":"ls","cwd":"/tmp"}`),
	}}, grok)
	if len(calls) != 1 || calls[0].Name != "run_terminal_command" {
		t.Fatalf("grok = %+v", calls)
	}
	if string(calls[0].Arguments) != `{"command":"ls","working_directory":"/tmp"}` &&
		string(calls[0].Arguments) != `{"working_directory":"/tmp","command":"ls"}` {
		t.Fatalf("grok args = %s", calls[0].Arguments)
	}

	opencode := []Tool{{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object","properties":{"filePath":{"type":"string"}}}`),
	}}
	calls = ResolveClientToolCalls([]ToolCall{{
		Name: "ReadFile", Arguments: json.RawMessage(`{"path":"main.go"}`),
	}}, opencode)
	if len(calls) != 1 || calls[0].Name != "read" || string(calls[0].Arguments) != `{"filePath":"main.go"}` {
		t.Fatalf("opencode = %+v args=%s", calls, calls[0].Arguments)
	}

	codex := []Tool{{
		Name:       "apply_patch",
		Parameters: json.RawMessage(`{"type":"object","properties":{"patch":{"type":"string"}}}`),
	}}
	calls = ResolveClientToolCalls([]ToolCall{{
		Name: "ApplyPatch", Arguments: json.RawMessage(`{"patchText":"*** Begin Patch"}`),
	}}, codex)
	if len(calls) != 1 || calls[0].Name != "apply_patch" || string(calls[0].Arguments) != `{"patch":"*** Begin Patch"}` {
		t.Fatalf("codex = %+v args=%s", calls, calls[0].Arguments)
	}
}
