package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
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

	out, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false)
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	root := ojson.ParseBytes(out)
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
	responseRoot := ojson.ParseBytes(responses)
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
	_, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false)
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
			_, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", []byte(tt.raw), false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestToolSearchValidationLeavesOrdinaryUnsupportedToolsUntouched(t *testing.T) {
	raw := []byte(`{"tools":[{"type":"mcp","server_label":"existing"}],"input":"hello"}`)
	if _, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", raw, false); err != nil {
		t.Fatalf("ordinary tool validation changed without tool_search: %v", err)
	}
}

func TestToolSearchChoiceDoesNotSelectOrdinaryFunctionWithoutSearch(t *testing.T) {
	raw := []byte(`{"tools":[{"type":"function","name":"tool_search","parameters":{"type":"object"}}],"tool_choice":{"type":"tool_search"},"input":"hello"}`)
	out, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("chat-model", raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if choice := ojson.GetBytes(out, "tool_choice"); choice.Get("type").String() != "tool_search" || choice.Get("function").Exists() {
		t.Fatalf("undeclared search choice selected an ordinary function: %s", out)
	}
}

func TestToolSearchResponseNonStreamRestoresSearchCall(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}]}`)
	chatResponse := []byte(`{"id":"chat_1","object":"chat.completion","created":1710000000,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"ts_1","type":"function","function":{"name":"tool_search","arguments":"{\"goal\":\"shipping\"}"}}]},"finish_reason":"tool_calls"}]}`)
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.4", request, request, chatResponse, nil)
	root := ojson.ParseBytes(out)
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
	item := ojson.ParseBytes(out).Get("output.0")
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
	var events []ojson.Result
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

func TestToolSearchAndApplyPatchResponsePreserveBothContracts(t *testing.T) {
	request := []byte(`{"model":"gpt-5.4","tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}},{"type":"custom","name":"apply_patch"}]}`)
	chatResponse := []byte(`{"id":"chat_mixed","object":"chat.completion","created":1710000000,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"ts_mixed","type":"function","function":{"name":"tool_search","arguments":"{\"goal\":\"shipping\"}"}},{"id":"patch_mixed","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}]},"finish_reason":"tool_calls"}]}`)
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var state any
			var response ojson.Result
			if !stream {
				raw, err := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStreamWithError(context.Background(), "gpt-5.4", request, nil, chatResponse, &state)
				if err != nil {
					t.Fatal(err)
				}
				response = ojson.ParseBytes(raw)
			} else {
				chunks := [][]byte{
					[]byte(`data: {"id":"chat_mixed","object":"chat.completion.chunk","created":1710000000,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"ts_mixed","type":"function","function":{"name":"tool_search","arguments":"{\"goal\":\"shipping\"}"}},{"index":1,"id":"patch_mixed","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}]},"finish_reason":"tool_calls"}]}`),
					[]byte(`data: [DONE]`),
				}
				counts := map[string]int{}
				for _, chunk := range chunks {
					frames, err := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesWithError(context.Background(), "gpt-5.4", request, nil, chunk, &state)
					if err != nil {
						t.Fatal(err)
					}
					for _, frame := range frames {
						for _, line := range strings.Split(string(frame), "\n") {
							if !strings.HasPrefix(line, "data:") {
								continue
							}
							payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
							if !ojson.Valid(payload) {
								t.Fatalf("invalid event: %s", frame)
							}
							event := ojson.Parse(payload)
							counts[event.Get("type").String()]++
							if event.Get("type").String() == "response.completed" {
								response = event.Get("response")
							}
						}
					}
				}
				if counts["response.completed"] != 1 || counts["response.custom_tool_call_input.delta"] != 1 || counts["response.custom_tool_call_input.done"] != 1 || counts["response.failed"] != 0 {
					t.Fatalf("mixed stream did not complete once: %v", counts)
				}
			}
			if response.Get("status").String() != "completed" || response.Get("output.#").Int() != 2 || response.Get("output.0.type").String() != "tool_search_call" || response.Get("output.0.arguments.goal").String() != "shipping" || response.Get("output.0.call_id").String() != "ts_mixed" || response.Get("output.1.type").String() != "custom_tool_call" || response.Get("output.1.input").String() != "patch" || response.Get("output.1.call_id").String() != "patch_mixed" {
				t.Fatalf("mixed response lost tool identity or input: %s", response.Raw)
			}
		})
	}
}

func TestChatToolSearch_StreamResponseRestoresSearchCall(t *testing.T) {
	lines := []string{
		`data: {"id":"r1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s","type":"function","function":{"name":"tool_search","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"id":"r1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"cal\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	var param any
	var added, done, completed ojson.Result
	for _, line := range lines {
		for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "m", []byte(chatToolSearchTestRequest), nil, []byte(line), &param) {
			event, data := parseOpenAIResponsesSSEEvent(t, chunk)
			if strings.HasPrefix(event, "response.function_call_arguments") {
				t.Fatalf("search call must not stream function argument events: %s", event)
			}
			switch {
			case event == "response.output_item.added" && data.Get("item.type").String() == "tool_search_call":
				added = data
			case event == "response.output_item.done" && data.Get("item.type").String() == "tool_search_call":
				done = data
			case event == "response.completed":
				completed = data
			}
		}
	}
	if !added.Exists() || added.Get("item.call_id").String() != "call_s" {
		t.Fatalf("missing tool_search_call added event")
	}
	if done.Get("item.arguments.query").String() != "cal" || done.Get("item.execution").String() != "client" {
		t.Fatalf("bad tool_search_call done event: %s", done.Raw)
	}
	if completed.Get("response.output.0.type").String() != "tool_search_call" {
		t.Fatalf("completed output = %s", completed.Get("response.output").Raw)
	}
}

func TestChatToolSearch_NonStreamResponseRestoresSearchAndLoadedToolCalls(t *testing.T) {
	resp := `{"id":"r2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[
	  {"id":"call_a","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"cal\"}"}},
	  {"id":"call_b","type":"function","function":{"name":"mcp__demo__cal___list","arguments":"{}"}}
	]}}]}`
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "m", []byte(chatToolSearchTestRequest), nil, []byte(resp), nil)
	search := ojson.GetBytes(out, `output.#(call_id=="call_a")`)
	if search.Get("type").String() != "tool_search_call" || search.Get("arguments.query").String() != "cal" {
		t.Fatalf("want tool_search_call, got %s", search.Raw)
	}
	loaded := ojson.GetBytes(out, `output.#(call_id=="call_b")`)
	if loaded.Get("type").String() != "function_call" || loaded.Get("name").String() != "_list" || loaded.Get("namespace").String() != "mcp__demo__cal" {
		t.Fatalf("loaded tool call must restore namespace identity: %s", loaded.Raw)
	}
}

// Synthetic Codex request: one eager tool, the client tool_search declaration,
// and one completed search round that returned a namespaced tool.
const chatToolSearchTestRequest = `{
  "model": "chat-test",
  "stream": true,
  "tools": [
    {"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
    {"type":"tool_search","execution":"client","description":"Search deferred tools","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}}
  ],
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"list my calendars"}]},
    {"type":"tool_search_call","call_id":"call_ts1","status":"completed","execution":"client","arguments":{"query":"calendar"}},
    {"type":"tool_search_output","call_id":"call_ts1","status":"completed","execution":"client","tools":[
      {"type":"namespace","name":"mcp__demo__cal","description":"Calendar","tools":[
        {"type":"function","name":"_list","description":"List calendars","defer_loading":true,"parameters":{"type":"object","properties":{}}}
      ]}
    ]}
  ]
}`
