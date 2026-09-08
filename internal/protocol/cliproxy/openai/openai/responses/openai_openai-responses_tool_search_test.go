package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestToolSearchRequestMapsCallOutputAndLoadedNamespaceTools(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"tools":[{"type":"tool_search","execution":"client","description":"Load a tool","parameters":{"type":"object","properties":{"goal":{"type":"string"}}}}],
		"input":[
			{"type":"tool_search_call","execution":"client","call_id":"ts_1","status":"completed","arguments":{"goal":"shipping"}},
			{"type":"tool_search_output","execution":"client","call_id":"ts_1","status":"completed","tools":[{"type":"namespace","name":"shipping","tools":[{"type":"function","name":"get_eta","description":"Get ETA","parameters":{"type":"object"}}]}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)

	out, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletionsWithError("gpt-5.4", raw, false)
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	root := gjson.ParseBytes(out)
	if got := root.Get("tools.#").Int(); got != 2 {
		t.Fatalf("expected tool_search plus loaded tool, got %d: %s", got, out)
	}
	if got := root.Get("tools.0.function.name").String(); got != responsesChatToolSearchFunctionName {
		t.Fatalf("unexpected search function name %q", got)
	}
	if got := root.Get("tools.1.function.name").String(); got != "shipping__get_eta" {
		t.Fatalf("loaded namespace was not flattened: %q", got)
	}
	if got := root.Get("messages.0.tool_calls.0.id").String(); got != "ts_1" {
		t.Fatalf("call_id was not preserved in assistant tool call: %q", got)
	}
	if got := root.Get("messages.0.tool_calls.0.function.name").String(); got != responsesChatToolSearchFunctionName {
		t.Fatalf("unexpected assistant search name %q", got)
	}
	if got := root.Get("messages.0.tool_calls.0.function.arguments").String(); got != `{"goal":"shipping"}` {
		t.Fatalf("unexpected search arguments %q", got)
	}
	if got := root.Get("messages.1.tool_call_id").String(); got != "ts_1" {
		t.Fatalf("tool output was not paired: %q", got)
	}
	if !strings.Contains(root.Get("messages.1.content").String(), "get_eta") {
		t.Fatalf("loaded tools were dropped from tool output: %q", root.Get("messages.1.content").String())
	}

	// The loaded declaration is also the source of truth for reverse namespace
	// restoration when the provider calls it on the next turn.
	chatResponse := []byte(`{"id":"chat_1","object":"chat.completion","created":1710000000,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"shipping__get_eta","arguments":"{\"order\":\"A\"}"}}]},"finish_reason":"tool_calls"}]}`)
	responses := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.4", raw, out, chatResponse, nil)
	responseRoot := gjson.ParseBytes(responses)
	if got := responseRoot.Get("output.0.type").String(); got != "function_call" {
		t.Fatalf("unexpected reverse item type %q", got)
	}
	if got := responseRoot.Get("output.0.name").String(); got != "get_eta" {
		t.Fatalf("namespace child was not restored: %q", got)
	}
	if got := responseRoot.Get("output.0.namespace").String(); got != "shipping" {
		t.Fatalf("namespace was not restored: %q", got)
	}
}

func TestToolSearchRequestRejectsMissingHistoryEvenWithPreviousResponse(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"previous_response_id":"resp_1",
		"input":[{"type":"tool_search_output","execution":"client","call_id":"ts_1","status":"completed","tools":[{"type":"function","name":"get_eta","parameters":{"type":"object"}}]}]
	}`)
	_, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletionsWithError("gpt-5.4", raw, false)
	if err == nil || !strings.Contains(err.Error(), "matching tool_search_call") {
		t.Fatalf("expected missing history error, got %v", err)
	}
}

func TestToolSearchRequestRejectsUnsupportedHistoryAndModes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "server declaration",
			raw:  `{"tools":[{"type":"tool_search","execution":"server"}]}`,
			want: "execution must be",
		},
		{
			name: "call without output",
			raw:  `{"input":[{"type":"tool_search_call","execution":"client","call_id":"ts_1","arguments":{"goal":"x"}}]}`,
			want: "matching tool_search_output",
		},
		{
			name: "output without call",
			raw:  `{"input":[{"type":"tool_search_output","execution":"client","call_id":"ts_1","tools":[]}]}`,
			want: "matching tool_search_call",
		},
		{
			name: "invalid arguments",
			raw:  `{"input":[{"type":"tool_search_call","execution":"client","call_id":"ts_1","arguments":["bad"]},{"type":"tool_search_output","execution":"client","call_id":"ts_1","tools":[]}]}`,
			want: "arguments must be an object",
		},
		{
			name: "reserved name collision from history",
			raw:  `{"tools":[{"type":"function","name":"tool_search"}],"input":[{"type":"tool_search_call","execution":"client","call_id":"ts_1","arguments":{}},{"type":"tool_search_output","execution":"client","call_id":"ts_1","tools":[]}]}`,
			want: "reserved Chat function name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletionsWithError("gpt-5.4", []byte(tt.raw), false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestToolSearchValidationLeavesOrdinaryUnsupportedToolsUntouched(t *testing.T) {
	raw := []byte(`{"tools":[{"type":"mcp","server_label":"existing"}],"input":"hello"}`)
	if _, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletionsWithError("gpt-5.4", raw, false); err != nil {
		t.Fatalf("ordinary tool validation changed without tool_search: %v", err)
	}
}

func TestToolSearchResponseNonStreamRestoresSearchCall(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`)
	chatResponse := []byte(`{"id":"chat_1","object":"chat.completion","created":1710000000,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"ts_1","type":"function","function":{"name":"tool_search","arguments":"{\"goal\":\"shipping\"}"}}]},"finish_reason":"tool_calls"}]}`)
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.4", request, request, chatResponse, nil)
	root := gjson.ParseBytes(out)
	item := root.Get("output.0")
	if got := item.Get("type").String(); got != "tool_search_call" {
		t.Fatalf("unexpected item type %q: %s", got, out)
	}
	if got := item.Get("execution").String(); got != "client" {
		t.Fatalf("unexpected execution %q", got)
	}
	if got := item.Get("call_id").String(); got != "ts_1" {
		t.Fatalf("unexpected call_id %q", got)
	}
	if got := item.Get("arguments.goal").String(); got != "shipping" {
		t.Fatalf("unexpected arguments %s", item.Get("arguments").Raw)
	}
}

func TestToolSearchResponseRejectsInvalidArgumentsWithoutCompletedCall(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`)
	chatResponse := []byte(`{"id":"chat_1","object":"chat.completion","created":1710000000,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"ts_1","type":"function","function":{"name":"tool_search","arguments":"{bad"}}]},"finish_reason":"tool_calls"}]}`)
	if _, err := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStreamWithError(context.Background(), "gpt-5.4", request, request, chatResponse, nil); err == nil || !strings.Contains(err.Error(), "tool_search_call") {
		t.Fatalf("expected invalid search arguments error, got %v", err)
	}
	if out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.4", request, request, chatResponse, nil); len(out) != 0 {
		t.Fatalf("legacy converter exposed an invalid completed response: %s", out)
	}
}

func TestToolSearchResponseStreamRejectsInvalidArgumentsWithoutTerminalEvents(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`)
	first := []byte(`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"ts_1","type":"function","function":{"name":"tool_search","arguments":"{bad"}}]},"finish_reason":"tool_calls"}]}`)
	done := []byte(`data: [DONE]`)
	var state any
	chunks, err := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesWithError(context.Background(), "gpt-5.4", request, request, first, &state)
	if err == nil || !strings.Contains(err.Error(), "tool_search_call") {
		t.Fatalf("expected invalid streamed search arguments error, got chunks=%q err=%v", chunks, err)
	}
	if len(chunks) != 0 {
		t.Fatalf("invalid streamed search call emitted terminal chunks: %q", chunks)
	}
	if _, err = ConvertOpenAIChatCompletionsResponseToOpenAIResponsesWithError(context.Background(), "gpt-5.4", request, request, done, &state); err == nil {
		t.Fatal("expected the failed stream state to reject subsequent [DONE]")
	}
}

func TestToolSearchResponsePreservesOrdinaryFunctionCallArguments(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	chatResponse := []byte(`{"id":"chat_1","object":"chat.completion","created":1710000000,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{bad"}}]},"finish_reason":"tool_calls"}]}`)
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.4", request, request, chatResponse, nil)
	item := gjson.ParseBytes(out).Get("output.0")
	if item.Get("type").String() != "function_call" || item.Get("arguments").String() != "{bad" {
		t.Fatalf("ordinary function call behavior changed: %s", out)
	}
}

func TestToolSearchResponseStreamRestoresSearchCall(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`)
	chunks := [][]byte{
		[]byte(`{"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"ts_1","type":"function","function":{"arguments":"{\"goal\":\"shipping\"}"}}]},"finish_reason":null}]}`),
		[]byte(`{"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"tool_search"}}]},"finish_reason":"tool_calls"}]}`),
		[]byte(`[DONE]`),
	}
	var state any
	var events []gjson.Result
	for _, chunk := range chunks {
		for _, event := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-5.4", request, request, chunk, &state) {
			_, data := parseOpenAIResponsesSSEEvent(t, event)
			events = append(events, data)
		}
	}
	var added, done, completed bool
	for _, event := range events {
		switch event.Get("type").String() {
		case "response.output_item.added":
			if event.Get("item.type").String() == "tool_search_call" {
				added = true
			}
		case "response.output_item.done":
			if event.Get("item.type").String() == "tool_search_call" {
				done = true
				if got := event.Get("item.arguments.goal").String(); got != "shipping" {
					t.Fatalf("unexpected streamed arguments %s", event.Get("item.arguments").Raw)
				}
			}
		case "response.completed":
			completed = true
			if got := event.Get("response.output.0.type").String(); got != "tool_search_call" {
				t.Fatalf("unexpected completed item %q", got)
			}
		}
	}
	if !added || !done || !completed {
		t.Fatalf("missing search stream lifecycle events: added=%v done=%v completed=%v", added, done, completed)
	}
}
