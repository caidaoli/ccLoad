package cursorauth

import (
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
		!strings.Contains(request.Prompt, "[tool_use id=toolu_1 name=bash]") ||
		!strings.Contains(request.Prompt, "[tool_result tool_use_id=toolu_1] a.txt") {
		t.Fatalf("prompt = %q", request.Prompt)
	}
	if len(request.ToolResults) != 1 || request.ToolResults[0].CallID != "toolu_1" ||
		request.ToolResults[0].Output != "a.txt" {
		t.Fatalf("tool results = %+v", request.ToolResults)
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
	if len(request.ToolResults) != 1 || request.ToolResults[0].CallID != "call_1" ||
		request.ToolResults[0].Output != "ok" {
		t.Fatalf("tool results = %+v", request.ToolResults)
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
}

func TestParseRequestOnlyResumesTrailingToolResults(t *testing.T) {
	t.Parallel()
	request := ParseRequest([]byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_old","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_old","content":"old"},
			{"role":"assistant","content":"done"},
			{"role":"user","content":"new question"}
		]
	}`))
	if len(request.ToolResults) != 0 {
		t.Fatalf("historical tool results were treated as a resume turn: %+v", request.ToolResults)
	}

	request = ParseRequest([]byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[]},
			{"role":"tool","tool_call_id":"call_a","content":"a"},
			{"role":"tool","tool_call_id":"call_b","content":"b"}
		]
	}`))
	if len(request.ToolResults) != 2 || request.ToolResults[0].CallID != "call_a" ||
		request.ToolResults[1].CallID != "call_b" {
		t.Fatalf("trailing tool results = %+v", request.ToolResults)
	}

	request = ParseRequest([]byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_system","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_system","content":"ok"}]},
			{"role":"system","content":"updated context"}
		]
	}`))
	if len(request.ToolResults) != 1 || request.ToolResults[0].CallID != "call_system" {
		t.Fatalf("tool result followed by system context = %+v", request.ToolResults)
	}
}
