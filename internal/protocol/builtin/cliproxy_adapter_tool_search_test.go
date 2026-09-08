package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexToolSearchIsOnlyEnabledForOpenAIResponsesBridge(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}],
		"input":[
			{"type":"tool_search_call","execution":"client","call_id":"ts_1","arguments":{"goal":"find"}},
			{"type":"tool_search_output","execution":"client","call_id":"ts_1","tools":[]}
		]
	}`)

	translated, err := cliproxyCodexRequestToOpenAI("gpt-5.4", raw, false)
	if err != nil {
		t.Fatalf("OpenAI bridge rejected client tool_search: %v", err)
	}
	if got := gjson.GetBytes(translated, "tools.0.function.name").String(); got != "tool_search" {
		t.Fatalf("unexpected translated search function %q: %s", got, translated)
	}

	if _, err := cliproxyCodexRequestToGemini("gpt-5.4", raw, false); err == nil || !strings.Contains(err.Error(), "tool_search") {
		t.Fatalf("Gemini bridge should reject unimplemented tool_search, got %v", err)
	}
	if _, err := cliproxyCodexRequestToAnthropic("gpt-5.4", raw, false); err == nil || !strings.Contains(err.Error(), "tool_search") {
		t.Fatalf("Anthropic bridge should reject unimplemented tool_search, got %v", err)
	}
}

func TestOpenAIToCodexToolSearchResponseRejectsMalformedArguments(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`)
	nonStream := []byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"ts_1","type":"function","function":{"name":"tool_search","arguments":"{bad"}}]},"finish_reason":"tool_calls"}]}`)
	if _, err := cliproxyOpenAIResponseToCodexNonStream(context.Background(), "gpt-5.4", request, request, nonStream); err == nil || !strings.Contains(err.Error(), "tool_search_call") {
		t.Fatalf("expected non-stream translation error, got %v", err)
	}

	stream := []byte("data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"ts_1\",\"type\":\"function\",\"function\":{\"name\":\"tool_search\",\"arguments\":\"{bad\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
	var state any
	if _, err := cliproxyOpenAIResponseToCodexStream(context.Background(), "gpt-5.4", request, request, stream, &state); err == nil || !strings.Contains(err.Error(), "tool_search_call") {
		t.Fatalf("expected stream translation error, got %v", err)
	}
}
